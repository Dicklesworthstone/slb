// Package daemon provides IPC server for fast agent communication.
package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/log"
)

// JSON-RPC request/response types.
type (
	RPCRequest struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params,omitempty"`
		ID     int64           `json:"id"`
	}
	RPCResponse struct {
		Result any    `json:"result,omitempty"`
		Error  *Error `json:"error,omitempty"`
		ID     int64  `json:"id"`
	}
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
)

const (
	ErrCodeParse          = -32700
	ErrCodeInvalidReq     = -32600
	ErrCodeMethodNotFound = -32601
	ErrCodeInvalidParams  = -32602
	ErrCodeInternal       = -32603
	ipcWriteTimeout       = 5 * time.Second
)

// Responses and events share one framed stream. Serialize the whole frame and
// its deadline, not individual fragments; a slow peer cannot hold it forever.
type lockedConn struct {
	net.Conn
	mu sync.Mutex
}

func (c *lockedConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.Conn.SetWriteDeadline(time.Now().Add(ipcWriteTimeout)); err != nil {
		return 0, err
	}
	defer func() { _ = c.Conn.SetWriteDeadline(time.Time{}) }()
	n, err := c.Conn.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

func newIPCServer(listener net.Listener, addr string, logger *log.Logger, cleanup func() error, connGuard func(net.Conn, *bufio.Scanner) error) *IPCServer {
	if logger == nil {
		logger = log.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	startDone := make(chan struct{})
	close(startDone)
	return &IPCServer{
		socketPath: addr, listener: listener, logger: logger, startTime: time.Now(),
		subscribers: make(map[int64]*subscriber), connections: make(map[net.Conn]struct{}),
		startDone: startDone, ctx: ctx, cancel: cancel, cleanup: cleanup, connGuard: connGuard,
	}
}

// IPCServer handles Unix socket IPC for the daemon.
type IPCServer struct {
	socketPath string
	listener   net.Listener
	logger     *log.Logger
	cleanup    func() error
	connGuard  func(conn net.Conn, scanner *bufio.Scanner) error

	startTime    time.Time
	activeConns  atomic.Int32
	pendingCount atomic.Int32

	subscribers     map[int64]*subscriber
	subscribersMu   sync.RWMutex
	nextSubID       atomic.Int64
	requestSnapshot map[string]Event
	stateManaged    bool
	stateReady      bool
	stateError      string
	sessionCount    int

	connections   map[net.Conn]struct{}
	connectionsMu sync.Mutex
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	streamWG      sync.WaitGroup
	startMu       sync.Mutex
	startDone     chan struct{}
	started       bool
	stopOnce      sync.Once
	stopErr       error

	// Configure before Start; handlers never mutate the verifier.
	verifier *Verifier
}

type subscriber struct {
	id       int64
	conn     net.Conn
	events   chan Event
	done     chan struct{}
	stopOnce sync.Once
	initial  []Event
}

func (sub *subscriber) stop() {
	sub.stopOnce.Do(func() {
		close(sub.done)
		// Unblock BOTH a streaming write and the connection's scanner read.
		_ = sub.conn.Close()
	})
}

// Event represents a daemon event sent to subscribers.
type Event struct {
	Type    string `json:"type"`
	Payload any    `json:"payload"`
	Time    int64  `json:"time"`
}

func NewIPCServer(socketPath string, logger *log.Logger) (*IPCServer, error) {
	if socketPath == "" {
		return nil, fmt.Errorf("socket path is required")
	}
	if fi, err := os.Lstat(socketPath); err == nil {
		if fi.Mode().Type()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("path exists but is not a socket: %s", socketPath)
		}
		if err := os.Remove(socketPath); err != nil {
			return nil, fmt.Errorf("removing stale socket: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("checking socket path: %w", err)
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("creating unix socket: %w", err)
	}
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

// Start accepts connections until cancellation. A stopped server cannot be
// restarted: callers create a new listener/server for a new generation.
func (s *IPCServer) Start(ctx context.Context) error {
	s.startMu.Lock()
	if s.started || s.ctx.Err() != nil || ctx.Err() != nil {
		s.startMu.Unlock()
		return errors.New("ipc server already started or stopped")
	}
	s.started = true
	startDone := make(chan struct{})
	s.startDone = startDone
	s.startMu.Unlock()
	defer close(startDone)
	s.logger.Info("ipc server started", "socket", s.socketPath)
	go func() {
		select {
		case <-ctx.Done():
		case <-s.ctx.Done():
		}
		s.cancel()
		_ = s.listener.Close()
		s.closeConnections()
	}()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || s.ctx.Err() != nil {
				return nil
			}
			s.logger.Error("accept failed", "error", err)
			continue
		}
		s.connectionsMu.Lock()
		if s.ctx.Err() != nil {
			s.connectionsMu.Unlock()
			_ = conn.Close()
			return nil
		}
		s.connections[conn] = struct{}{}
		s.wg.Add(1)
		s.connectionsMu.Unlock()
		go s.handleConnection(conn)
	}
}

func (s *IPCServer) closeConnections() {
	s.connectionsMu.Lock()
	defer s.connectionsMu.Unlock()
	for conn := range s.connections {
		_ = conn.Close()
	}
}

// Stop closes blocked readers/writers and joins connection AND event-stream
// goroutines before releasing the socket. Safe before Start and concurrently.
func (s *IPCServer) Stop() error {
	s.stopOnce.Do(func() {
		s.cancel()
		_ = s.listener.Close()
		s.startMu.Lock()
		done := s.startDone
		s.startMu.Unlock()
		<-done // No more connection WaitGroup additions after this point.
		s.closeConnections()
		s.subscribersMu.Lock()
		for id, sub := range s.subscribers {
			sub.stop()
			delete(s.subscribers, id)
		}
		s.subscribersMu.Unlock()
		s.wg.Wait() // No handler can start another stream after this point.
		s.streamWG.Wait()
		if s.cleanup != nil {
			if err := s.cleanup(); err != nil {
				s.stopErr = fmt.Errorf("cleanup: %w", err)
			}
		}
		s.logger.Info("ipc server stopped")
	})
	return s.stopErr
}

func (s *IPCServer) handleConnection(conn net.Conn) {
	defer s.wg.Done()
	locked := &lockedConn{Conn: conn}
	defer func() {
		_ = conn.Close()
		s.subscribersMu.Lock()
		for id, sub := range s.subscribers {
			if sub.conn == locked {
				sub.stop()
				delete(s.subscribers, id)
			}
		}
		s.subscribersMu.Unlock()
		s.connectionsMu.Lock()
		delete(s.connections, conn)
		s.connectionsMu.Unlock()
	}()
	s.activeConns.Add(1)
	defer s.activeConns.Add(-1)
	scanner := bufio.NewScanner(locked)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	if s.connGuard != nil {
		if err := s.connGuard(locked, scanner); err != nil {
			s.logger.Debug("connection rejected", "error", err)
			return
		}
	}
	for scanner.Scan() {
		if s.ctx.Err() != nil {
			return
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if resp := s.handleRequest(locked, line); resp != nil {
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

func (s *IPCServer) handleRequest(conn net.Conn, data []byte) *RPCResponse {
	var req RPCRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return &RPCResponse{Error: &Error{Code: ErrCodeParse, Message: "parse error: " + err.Error()}, ID: 0}
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
	case "complete_execute":
		return s.handleCompleteExecute(req)
	case "hook_query":
		return s.handleHookQuery(req)
	case "hook_health":
		return s.handleHookHealth(req)
	default:
		return &RPCResponse{Error: &Error{Code: ErrCodeMethodNotFound, Message: "method not found: " + req.Method}, ID: req.ID}
	}
}

func (s *IPCServer) handlePing(req RPCRequest) *RPCResponse {
	return &RPCResponse{Result: map[string]bool{"pong": true}, ID: req.ID}
}

func (s *IPCServer) handleStatus(req RPCRequest) *RPCResponse {
	s.subscribersMu.RLock()
	defer s.subscribersMu.RUnlock()
	subCount := len(s.subscribers)
	sessions := int(s.activeConns.Load())
	if s.stateManaged {
		sessions = s.sessionCount
	}
	return &RPCResponse{Result: map[string]any{
		"uptime_seconds": int64(time.Since(s.startTime).Seconds()),
		"pending_count":  s.pendingCount.Load(), "active_sessions": sessions,
		"active_connections": s.activeConns.Load(), "state_ready": s.stateReady,
		"state_error": s.stateError,
		"subscribers": subCount,
	}, ID: req.ID}
}

type NotifyParams struct {
	Type    string `json:"type"`
	Payload any    `json:"payload"`
}

func (s *IPCServer) handleNotify(req RPCRequest) *RPCResponse {
	var params NotifyParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return &RPCResponse{Error: &Error{Code: ErrCodeInvalidParams, Message: "invalid params: " + err.Error()}, ID: req.ID}
	}
	if params.Type == "" {
		return &RPCResponse{Error: &Error{Code: ErrCodeInvalidParams, Message: "type is required"}, ID: req.ID}
	}
	s.broadcast(Event{Type: params.Type, Payload: params.Payload, Time: time.Now().Unix()})
	return &RPCResponse{Result: map[string]bool{"sent": true}, ID: req.ID}
}

func (s *IPCServer) handleSubscribe(req RPCRequest, conn net.Conn) *RPCResponse {
	id := s.nextSubID.Add(1)
	sub := &subscriber{id: id, conn: conn, events: make(chan Event, 100), done: make(chan struct{})}
	s.subscribersMu.Lock()
	if s.ctx.Err() != nil {
		s.subscribersMu.Unlock()
		return &RPCResponse{Error: &Error{Code: ErrCodeInternal, Message: "server is stopping"}, ID: req.ID}
	}
	for _, existing := range s.subscribers {
		if existing.conn == conn {
			s.subscribersMu.Unlock()
			return &RPCResponse{Error: &Error{Code: ErrCodeInvalidReq, Message: "connection already subscribed"}, ID: req.ID}
		}
	}
	if s.stateManaged && !s.stateReady {
		s.subscribersMu.Unlock()
		return &RPCResponse{Error: &Error{Code: ErrCodeInternal, Message: "request state unavailable; use database polling"}, ID: req.ID}
	}
	sub.initial = s.snapshotEventsLocked()
	s.subscribers[id] = sub
	s.streamWG.Add(1)
	s.subscribersMu.Unlock()
	resp := &RPCResponse{Result: map[string]any{"subscribed": true, "subscription_id": id}, ID: req.ID}
	if err := s.writeResponse(conn, resp); err != nil {
		s.removeSubscriber(id)
		s.streamWG.Done()
		return nil
	}
	go func() {
		defer s.streamWG.Done()
		s.streamEvents(sub)
	}()
	return nil
}

func (s *IPCServer) streamEvents(sub *subscriber) {
	defer s.removeSubscriber(sub.id)
	// Bootstrap is separate from the live queue, so >100 existing requests do
	// not overflow a brand-new subscription before it can read its first event.
	for _, event := range sub.initial {
		if s.ctx.Err() != nil || s.writeEvent(sub, event) != nil {
			return
		}
	}
	sub.initial = nil
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-sub.done:
			return
		case event := <-sub.events:
			if err := s.writeEvent(sub, event); err != nil {
				return
			}
		}
	}
}

func (s *IPCServer) writeEvent(sub *subscriber, event Event) error {
	data, err := json.Marshal(map[string]any{"event": event})
	if err != nil {
		return err
	}
	data = append(data, '\n')
	n, err := sub.conn.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

func (s *IPCServer) broadcast(event Event) {
	s.subscribersMu.Lock()
	defer s.subscribersMu.Unlock()
	s.broadcastLocked(event)
}

// Caller holds subscribersMu, including while replacing a cached snapshot.
func (s *IPCServer) broadcastLocked(event Event) {
	for id, sub := range s.subscribers {
		select {
		case sub.events <- event:
		default:
			// Explicit EOF activates the client's resnapshot/polling fallback.
			// Silently dropping a decision would leave it indefinitely stale.
			sub.stop()
			delete(s.subscribers, id)
		}
	}
}

func (s *IPCServer) removeSubscriber(id int64) {
	s.subscribersMu.Lock()
	if sub, ok := s.subscribers[id]; ok {
		sub.stop()
		delete(s.subscribers, id)
	}
	s.subscribersMu.Unlock()
}

func (s *IPCServer) writeResponse(conn net.Conn, resp *RPCResponse) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}
	data = append(data, '\n')
	n, err := conn.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

func (s *IPCServer) SetPendingCount(count int32) { s.pendingCount.Store(count) }

func (s *IPCServer) BroadcastEvent(eventType string, payload any) {
	s.broadcast(Event{Type: eventType, Payload: payload, Time: time.Now().Unix()})
}

func (s *IPCServer) SetVerifier(v *Verifier) { s.verifier = v }

func (s *IPCServer) handleVerifyExecute(req RPCRequest) *RPCResponse {
	if s.verifier == nil {
		return &RPCResponse{Error: &Error{Code: ErrCodeInternal, Message: "verifier not configured"}, ID: req.ID}
	}
	var params VerifyExecuteParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return &RPCResponse{Error: &Error{Code: ErrCodeInvalidParams, Message: "invalid params: " + err.Error()}, ID: req.ID}
	}
	if err := params.validate(); err != nil {
		return &RPCResponse{Error: &Error{Code: ErrCodeInvalidParams, Message: err.Error()}, ID: req.ID}
	}
	result, err := s.verifier.VerifyAndMarkExecuting(params)
	if err != nil {
		return &RPCResponse{Error: &Error{Code: ErrCodeInternal, Message: err.Error()}, ID: req.ID}
	}
	return &RPCResponse{Result: result.ToIPCResponse(), ID: req.ID}
}

// handleCompleteExecute cannot execute a command or return it to APPROVED.
func (s *IPCServer) handleCompleteExecute(req RPCRequest) *RPCResponse {
	if s.verifier == nil {
		return &RPCResponse{Error: &Error{Code: ErrCodeInternal, Message: "verifier not configured"}, ID: req.ID}
	}
	var params CompleteExecuteParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return &RPCResponse{Error: &Error{Code: ErrCodeInvalidParams, Message: "invalid params: " + err.Error()}, ID: req.ID}
	}
	if err := params.validate(); err != nil {
		return &RPCResponse{Error: &Error{Code: ErrCodeInvalidParams, Message: err.Error()}, ID: req.ID}
	}
	if err := s.verifier.MarkExecutionComplete(params); err != nil {
		return &RPCResponse{Error: &Error{Code: ErrCodeInternal, Message: err.Error()}, ID: req.ID}
	}
	return &RPCResponse{Result: map[string]any{"recorded": true, "request_id": params.RequestID, "status": params.Status}, ID: req.ID}
}
