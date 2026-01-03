// Package daemon provides IPC server for fast agent communication.
package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/log"
)

// JSON-RPC request/response types.
type (
	// RPCRequest is a JSON-RPC style request.
	RPCRequest struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params,omitempty"`
		ID     int64           `json:"id"`
	}

	// RPCResponse is a JSON-RPC style response.
	RPCResponse struct {
		Result any    `json:"result,omitempty"`
		Error  *Error `json:"error,omitempty"`
		ID     int64  `json:"id"`
	}

	// Error represents a JSON-RPC error.
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
)

// Standard JSON-RPC error codes.
const (
	ErrCodeParse          = -32700
	ErrCodeInvalidReq     = -32600
	ErrCodeMethodNotFound = -32601
	ErrCodeInvalidParams  = -32602
	ErrCodeInternal       = -32603
)

type lockedConn struct {
	net.Conn
	mu sync.Mutex
}

func (c *lockedConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Conn.Write(p)
}

func newIPCServer(listener net.Listener, addr string, logger *log.Logger, cleanup func() error, connGuard func(net.Conn, *bufio.Scanner) error) *IPCServer {
	if logger == nil {
		logger = log.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	startDone := make(chan struct{})
	close(startDone)
	return &IPCServer{
		socketPath:  addr,
		listener:    listener,
		logger:      logger,
		startTime:   time.Now(),
		subscribers: make(map[int64]*subscriber),
		startDone:   startDone,
		ctx:         ctx,
		cancel:      cancel,
		cleanup:     cleanup,
		connGuard:   connGuard,
	}
}

// IPCServer handles Unix socket IPC for the daemon.
type IPCServer struct {
	socketPath string
	listener   net.Listener
	logger     *log.Logger
	cleanup    func() error
	connGuard  func(conn net.Conn, scanner *bufio.Scanner) error

	// State tracking.
	startTime    time.Time
	activeConns  atomic.Int32
	pendingCount atomic.Int32

	// Subscriber management.
	subscribers   map[int64]*subscriber
	subscribersMu sync.RWMutex
	nextSubID     atomic.Int64

	// Shutdown coordination.
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	startMu   sync.Mutex
	startDone chan struct{}

	// Optional verifier for execution gate checks.
	verifier *Verifier
}

// subscriber tracks an event subscription.
type subscriber struct {
	id     int64
	conn   net.Conn
	events chan Event
	done   chan struct{}
}

// Event represents a daemon event sent to subscribers.
type Event struct {
	Type    string `json:"type"`
	Payload any    `json:"payload"`
	Time    int64  `json:"time"`
}

// NewIPCServer creates a new IPC server listening on the given Unix socket.
func NewIPCServer(socketPath string, logger *log.Logger) (*IPCServer, error) {
	if socketPath == "" {
		return nil, fmt.Errorf("socket path is required")
	}

	// Check if path exists and verify it's a socket before removing.
	// Refuse to delete non-socket files for safety.
	if fi, err := os.Lstat(socketPath); err == nil {
		if fi.Mode().Type()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("path exists but is not a socket: %s", socketPath)
		}
		// It's a socket, safe to remove.
		if err := os.Remove(socketPath); err != nil {
			return nil, fmt.Errorf("removing stale socket: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("checking socket path: %w", err)
	}

	// Create the listener.
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("creating unix socket: %w", err)
	}

	// Set socket permissions to 0600 (owner only).
	if err := os.Chmod(socketPath, 0600); err != nil {
		_ = ln.Close()
		_ = os.Remove(socketPath)
		return nil, fmt.Errorf("setting socket permissions: %w", err)
	}

	cleanup := func() error {
		if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return newIPCServer(ln, socketPath, logger, cleanup, nil), nil
}

// Start begins accepting connections. Blocks until context is cancelled.
func (s *IPCServer) Start(ctx context.Context) error {
	s.startMu.Lock()
	startDone := make(chan struct{})
	s.startDone = startDone
	s.startMu.Unlock()
	defer func() {
		s.startMu.Lock()
		close(startDone)
		stopped := make(chan struct{})
		close(stopped)
		s.startDone = stopped
		s.startMu.Unlock()
	}()

	s.logger.Info("ipc server started", "socket", s.socketPath)

	// Merge with our internal context.
	go func() {
		<-ctx.Done()
		s.cancel()
		_ = s.listener.Close()
	}()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			select {
			case <-s.ctx.Done():
				return nil
			default:
				s.logger.Error("accept failed", "error", err)
				continue
			}
		}

		s.wg.Add(1)
		go s.handleConnection(conn)
	}
}

// Stop gracefully shuts down the IPC server.
func (s *IPCServer) Stop() error {
	s.cancel()

	// Close listener to stop accepting new connections.
	if err := s.listener.Close(); err != nil {
		s.logger.Warn("closing listener", "error", err)
	}

	s.startMu.Lock()
	startDone := s.startDone
	s.startMu.Unlock()
	<-startDone

	// Close all subscribers.
	s.subscribersMu.Lock()
	for _, sub := range s.subscribers {
		close(sub.done)
	}
	s.subscribers = make(map[int64]*subscriber)
	s.subscribersMu.Unlock()

	// Wait for existing connections to finish.
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		s.logger.Warn("timed out waiting for connections to close")
	}

	if s.cleanup != nil {
		if err := s.cleanup(); err != nil {
			return fmt.Errorf("cleanup: %w", err)
		}
	}

	s.logger.Info("ipc server stopped")
	return nil
}

// handleConnection processes a single client connection.
func (s *IPCServer) handleConnection(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()

	// Ensure responses and subscription events cannot interleave on the same connection.
	locked := &lockedConn{Conn: conn}

	s.activeConns.Add(1)
	defer s.activeConns.Add(-1)

	scanner := bufio.NewScanner(locked)
	// Increase buffer for larger requests.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	if s.connGuard != nil {
		if err := s.connGuard(locked, scanner); err != nil {
			s.logger.Debug("connection rejected", "error", err)
			return
		}
	}

	for scanner.Scan() {
		select {
		case <-s.ctx.Done():
			return
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		resp := s.handleRequest(locked, line)
		if resp != nil {
			if err := s.writeResponse(locked, resp); err != nil {
				s.logger.Debug("write response failed", "error", err)
				return
			}
		}
	}

	if err := scanner.Err(); err != nil {
		s.logger.Debug("connection read error", "error", err)
	}
}

// handleRequest parses and dispatches a JSON-RPC request.
func (s *IPCServer) handleRequest(conn net.Conn, data []byte) *RPCResponse {
	var req RPCRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return &RPCResponse{
			Error: &Error{Code: ErrCodeParse, Message: "parse error: " + err.Error()},
			ID:    0,
		}
	}

	switch req.Method {
	case "ping":
		return s.handlePing(req)
	case "status":
		return s.handleStatus(req)
	case "notify":
		return s.handleNotify(req)
	case "subscribe":
		return s.handleSubscribe(req, conn)
	case "verify_execute":
		return s.handleVerifyExecute(req)
	case "hook_query":
		return s.handleHookQuery(req)
	case "hook_health":
		return s.handleHookHealth(req)
	default:
		return &RPCResponse{
			Error: &Error{Code: ErrCodeMethodNotFound, Message: "method not found: " + req.Method},
			ID:    req.ID,
		}
	}
}

// handlePing responds to health check.
func (s *IPCServer) handlePing(req RPCRequest) *RPCResponse {
	return &RPCResponse{
		Result: map[string]bool{"pong": true},
		ID:     req.ID,
	}
}

// handleStatus returns daemon status.
func (s *IPCServer) handleStatus(req RPCRequest) *RPCResponse {
	s.subscribersMu.RLock()
	subCount := len(s.subscribers)
	s.subscribersMu.RUnlock()

	return &RPCResponse{
		Result: map[string]any{
			"uptime_seconds":  int64(time.Since(s.startTime).Seconds()),
			"pending_count":   s.pendingCount.Load(),
			"active_sessions": s.activeConns.Load(),
			"subscribers":     subCount,
		},
		ID: req.ID,
	}
}

// NotifyParams are parameters for the notify method.
type NotifyParams struct {
	Type    string `json:"type"`
	Payload any    `json:"payload"`
}

// handleNotify broadcasts an event to all subscribers.
func (s *IPCServer) handleNotify(req RPCRequest) *RPCResponse {
	var params NotifyParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return &RPCResponse{
			Error: &Error{Code: ErrCodeInvalidParams, Message: "invalid params: " + err.Error()},
			ID:    req.ID,
		}
	}

	if params.Type == "" {
		return &RPCResponse{
			Error: &Error{Code: ErrCodeInvalidParams, Message: "type is required"},
			ID:    req.ID,
		}
	}

	event := Event{
		Type:    params.Type,
		Payload: params.Payload,
		Time:    time.Now().Unix(),
	}

	s.broadcast(event)

	return &RPCResponse{
		Result: map[string]bool{"sent": true},
		ID:     req.ID,
	}
}

// handleSubscribe sets up event streaming for the connection.
func (s *IPCServer) handleSubscribe(req RPCRequest, conn net.Conn) *RPCResponse {
	id := s.nextSubID.Add(1)

	sub := &subscriber{
		id:     id,
		conn:   conn,
		events: make(chan Event, 100),
		done:   make(chan struct{}),
	}

	s.subscribersMu.Lock()
	s.subscribers[id] = sub
	s.subscribersMu.Unlock()

	// Send initial response.
	resp := &RPCResponse{
		Result: map[string]any{
			"subscribed":      true,
			"subscription_id": id,
		},
		ID: req.ID,
	}
	if err := s.writeResponse(conn, resp); err != nil {
		s.removeSubscriber(id)
		return nil
	}

	// Stream events until done.
	go s.streamEvents(sub)

	return nil // Response already sent.
}

// streamEvents sends events to a subscriber until done.
func (s *IPCServer) streamEvents(sub *subscriber) {
	defer s.removeSubscriber(sub.id)

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-sub.done:
			return
		case event := <-sub.events:
			data, err := json.Marshal(map[string]any{
				"event": event,
			})
			if err != nil {
				s.logger.Debug("marshal event failed", "error", err)
				continue
			}
			data = append(data, '\n')
			if _, err := sub.conn.Write(data); err != nil {
				return
			}
		}
	}
}

// broadcast sends an event to all subscribers.
func (s *IPCServer) broadcast(event Event) {
	s.subscribersMu.RLock()
	defer s.subscribersMu.RUnlock()

	for _, sub := range s.subscribers {
		select {
		case sub.events <- event:
		default:
			// Buffer full, skip this subscriber.
		}
	}
}

// removeSubscriber removes a subscriber from the map.
func (s *IPCServer) removeSubscriber(id int64) {
	s.subscribersMu.Lock()
	delete(s.subscribers, id)
	s.subscribersMu.Unlock()
}

// writeResponse sends a JSON-RPC response.
func (s *IPCServer) writeResponse(conn net.Conn, resp *RPCResponse) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}
	data = append(data, '\n')
	_, err = conn.Write(data)
	return err
}

// SetPendingCount updates the pending request count (for status reporting).
func (s *IPCServer) SetPendingCount(count int32) {
	s.pendingCount.Store(count)
}

// BroadcastEvent sends an event to all subscribers (public API).
func (s *IPCServer) BroadcastEvent(eventType string, payload any) {
	s.broadcast(Event{
		Type:    eventType,
		Payload: payload,
		Time:    time.Now().Unix(),
	})
}

// SetVerifier configures the execution verifier for gate checks.
func (s *IPCServer) SetVerifier(v *Verifier) {
	s.verifier = v
}

// handleVerifyExecute handles the verify_execute IPC method.
func (s *IPCServer) handleVerifyExecute(req RPCRequest) *RPCResponse {
	if s.verifier == nil {
		return &RPCResponse{
			Error: &Error{Code: ErrCodeInternal, Message: "verifier not configured"},
			ID:    req.ID,
		}
	}

	var params VerifyExecuteParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return &RPCResponse{
			Error: &Error{Code: ErrCodeInvalidParams, Message: "invalid params: " + err.Error()},
			ID:    req.ID,
		}
	}

	if params.RequestID == "" {
		return &RPCResponse{
			Error: &Error{Code: ErrCodeInvalidParams, Message: "request_id is required"},
			ID:    req.ID,
		}
	}
	if params.SessionID == "" {
		return &RPCResponse{
			Error: &Error{Code: ErrCodeInvalidParams, Message: "session_id is required"},
			ID:    req.ID,
		}
	}

	result, err := s.verifier.VerifyAndMarkExecuting(params.RequestID, params.SessionID)
	if err != nil {
		return &RPCResponse{
			Error: &Error{Code: ErrCodeInternal, Message: err.Error()},
			ID:    req.ID,
		}
	}

	return &RPCResponse{
		Result: result.ToIPCResponse(),
		ID:     req.ID,
	}
}
