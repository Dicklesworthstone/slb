// Package daemon provides IPC client for communicating with the daemon.
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
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const defaultIPCOperationTimeout = 5 * time.Second

// ErrIPCSubscribed prevents RPC replies and subscription events from competing
// for the same scanner. Use a separate client for calls while watching events.
var ErrIPCSubscribed = errors.New("IPC client is subscribed; use a separate client for RPC calls")

// IPCClient owns one connection generation. mu protects state, never network
// I/O; Close must be able to interrupt a blocked reader or writer immediately.
// gate serializes connection setup and RPCs and is itself context-cancellable.
type IPCClient struct {
	socketPath string
	conn       net.Conn
	scanner    *bufio.Scanner
	mu         sync.Mutex
	nextID     atomic.Int64
	gateOnce   sync.Once
	gate       chan struct{}
	generation uint64
	connDone   chan struct{}
	subscribed bool
}

type ipcClientConnection struct {
	conn       net.Conn
	scanner    *bufio.Scanner
	done       <-chan struct{}
	generation uint64
}

func NewIPCClient(socketPath string) *IPCClient { return &IPCClient{socketPath: socketPath} }

func (c *IPCClient) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.gateOnce.Do(func() { c.gate = make(chan struct{}, 1) })
	select {
	case c.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			c.release()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *IPCClient) release() { <-c.gate }

// Connect establishes a bounded connection. An explicit SLB_HOST is
// authoritative: failure must not silently redirect work to another daemon.
func (c *IPCClient) Connect(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, defaultIPCOperationTimeout)
	defer cancel()
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()
	return c.connect(ctx)
}

// connect requires gate. Connection setup does not hold mu, so Close can
// invalidate an in-flight dial as well as an already established connection.
func (c *IPCClient) connect(ctx context.Context) error {
	c.mu.Lock()
	if c.conn != nil {
		c.mu.Unlock()
		return nil
	}
	generation := c.generation
	c.mu.Unlock()

	network, address := "unix", c.socketPath
	if host := strings.TrimSpace(os.Getenv("SLB_HOST")); host != "" {
		network, address = "tcp", host
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, network, address)
	if err != nil {
		return fmt.Errorf("connecting to daemon: %w", err)
	}
	// DialContext does not cancel subsequent I/O. Bound the TCP hello too.
	stop := closeIPCOnCancel(ctx, func() { _ = conn.Close() })
	if deadline, ok := ctx.Deadline(); ok {
		err = conn.SetDeadline(deadline)
	}
	if err == nil && network == "tcp" {
		var hello []byte
		hello, err = json.Marshal(map[string]string{"auth": strings.TrimSpace(os.Getenv("SLB_SESSION_KEY"))})
		if err == nil {
			err = writeIPCFrame(conn, append(hello, '\n'))
		}
	}
	stop()
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = conn.SetDeadline(time.Time{})
	}
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("establishing IPC transport: %w", ipcContextError(ctx, err))
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation != generation {
		_ = conn.Close()
		return net.ErrClosed // Close won while the dial/handshake was in flight.
	}
	c.conn = conn
	c.scanner = bufio.NewScanner(conn)
	c.scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	c.connDone = make(chan struct{})
	c.subscribed = false
	return nil
}

// Close does not wait for the RPC gate. It also invalidates in-flight setup.
// A later Connect is allowed and receives a distinct connection generation.
func (c *IPCClient) Close() error {
	c.mu.Lock()
	conn := c.conn
	c.clearConnectionLocked()
	c.mu.Unlock()
	if conn != nil {
		return conn.Close()
	}
	return nil
}

func (c *IPCClient) clearConnectionLocked() {
	c.generation++
	c.conn, c.scanner, c.subscribed = nil, nil, false
	if c.connDone != nil {
		close(c.connDone)
		c.connDone = nil
	}
}

func (c *IPCClient) connection() (ipcClientConnection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return ipcClientConnection{}, errors.New("not connected")
	}
	if c.subscribed {
		return ipcClientConnection{}, ErrIPCSubscribed
	}
	return ipcClientConnection{c.conn, c.scanner, c.connDone, c.generation}, nil
}

// A stale stream/cancellation callback closes only its own generation. It must
// never close a freshly reconnected client or swap its scanner underneath it.
func (c *IPCClient) retire(connection ipcClientConnection) {
	c.mu.Lock()
	if c.generation == connection.generation {
		c.clearConnectionLocked()
	}
	c.mu.Unlock()
	_ = connection.conn.Close()
}

// The stop function joins a callback that already started. This prevents a
// cancellation racing with cleanup from later retiring a reusable connection.
func closeIPCOnCancel(ctx context.Context, closeConn func()) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		closeConn()
	})
	return func() {
		if !stop() {
			<-done
		}
	}
}

func ipcContextError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// The socket's deadline can fire just before the context timer is
	// scheduled. Preserve errors.Is(DeadlineExceeded) in that race too.
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
			return context.DeadlineExceeded
		}
	}
	return err
}

func writeIPCFrame(conn net.Conn, frame []byte) error {
	n, err := conn.Write(frame)
	if err == nil && n != len(frame) {
		return io.ErrShortWrite
	}
	return err
}

func (c *IPCClient) call(method string, params any) (*RPCResponse, error) {
	return c.callContext(context.Background(), method, params)
}

func (c *IPCClient) callContext(ctx context.Context, method string, params any) (*RPCResponse, error) {
	return c.request(ctx, method, params, false)
}

// request never retries a failed exchange. A lost reply to an execution claim
// is ambiguous; reconnecting must not replay that claim or imply it failed.
func (c *IPCClient) request(ctx context.Context, method string, params any, connect bool) (*RPCResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultIPCOperationTimeout)
	defer cancel()
	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	defer c.release()
	if connect {
		if err := c.connect(ctx); err != nil {
			return nil, err
		}
	}
	connection, err := c.connection()
	if err != nil {
		return nil, err
	}
	return c.exchange(ctx, connection, method, params)
}

// exchange requires gate. Any I/O or framing error poisons this connection:
// a scanner that timed out, or a delayed reply, cannot serve a subsequent RPC.
func (c *IPCClient) exchange(ctx context.Context, connection ipcClientConnection, method string, params any) (resp *RPCResponse, err error) {
	var paramsJSON json.RawMessage
	if params != nil {
		paramsJSON, err = json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("marshal params: %w", err)
		}
	}
	id := c.nextID.Add(1)
	data, err := json.Marshal(RPCRequest{Method: method, Params: paramsJSON, ID: id})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	stop := closeIPCOnCancel(ctx, func() { c.retire(connection) })
	defer func() {
		stop()
		if err != nil {
			c.retire(connection)
		} else if clearErr := connection.conn.SetDeadline(time.Time{}); clearErr != nil {
			// A complete, validated reply still wins over cancellation. Retire
			// the socket but do not hide a confirmed commit from the caller.
			c.retire(connection)
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		if err := connection.conn.SetDeadline(deadline); err != nil {
			return nil, ipcContextError(ctx, err)
		}
	}
	if err := writeIPCFrame(connection.conn, append(data, '\n')); err != nil {
		return nil, fmt.Errorf("write request: %w", ipcContextError(ctx, err))
	}
	if !connection.scanner.Scan() {
		err := connection.scanner.Err()
		if err == nil {
			err = io.EOF
		}
		return nil, fmt.Errorf("read response: %w", ipcContextError(ctx, err))
	}
	var wire struct {
		ID     int64           `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *Error          `json:"error"`
	}
	if err := json.Unmarshal(connection.scanner.Bytes(), &wire); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	if wire.ID != id {
		return nil, fmt.Errorf("IPC response ID mismatch: got %d, want %d", wire.ID, id)
	}
	if (len(wire.Result) == 0) == (wire.Error == nil) {
		return nil, errors.New("IPC response must contain exactly one of result or error")
	}
	resp = &RPCResponse{ID: wire.ID, Error: wire.Error}
	if len(wire.Result) > 0 {
		if err := json.Unmarshal(wire.Result, &resp.Result); err != nil {
			return nil, fmt.Errorf("unmarshal result: %w", err)
		}
	}
	return resp, nil
}

func (c *IPCClient) Ping(ctx context.Context) error {
	resp, err := c.request(ctx, "ping", nil, true)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("ping error: %s", resp.Error.Message)
	}
	result, ok := resp.Result.(map[string]any)
	if !ok || result["pong"] != true {
		return errors.New("invalid ping response")
	}
	return nil
}

type DaemonStatusInfo struct {
	UptimeSeconds  int64 `json:"uptime_seconds"`
	PendingCount   int32 `json:"pending_count"`
	ActiveSessions int32 `json:"active_sessions"`
	Subscribers    int   `json:"subscribers"`
}

func (c *IPCClient) Status(ctx context.Context) (*DaemonStatusInfo, error) {
	resp, err := c.request(ctx, "status", nil, true)
	if err != nil {
		return nil, err
	}
	var result DaemonStatusInfo
	if err := decodeIPCResult(resp, &result); err != nil {
		return nil, fmt.Errorf("status: %w", err)
	}
	return &result, nil
}

func (c *IPCClient) HookHealth(ctx context.Context, cwd string) (*HookHealthResult, error) {
	resp, err := c.request(ctx, "hook_health", HookHealthParams{CWD: cwd}, true)
	if err != nil {
		return nil, err
	}
	var result HookHealthResult
	if err := decodeIPCResult(resp, &result); err != nil {
		return nil, fmt.Errorf("hook health: %w", err)
	}
	return &result, nil
}

func (c *IPCClient) HookQuery(ctx context.Context, params HookQueryParams) (*HookQueryResult, error) {
	resp, err := c.request(ctx, "hook_query", params, true)
	if err != nil {
		return nil, err
	}
	var result HookQueryResult
	if err := decodeIPCResult(resp, &result); err != nil {
		return nil, fmt.Errorf("hook query: %w", err)
	}
	return &result, nil
}

func decodeIPCResult(resp *RPCResponse, target any) error {
	if resp.Error != nil {
		return fmt.Errorf("daemon error (%d): %s", resp.Error.Code, resp.Error.Message)
	}
	if resp.Result == nil {
		return errors.New("missing IPC result")
	}
	data, err := json.Marshal(resp.Result)
	if err != nil {
		return fmt.Errorf("marshal result: %w", err)
	}
	return json.Unmarshal(data, target)
}

func (c *IPCClient) Notify(ctx context.Context, eventType string, payload any) error {
	resp, err := c.request(ctx, "notify", NotifyParams{Type: eventType, Payload: payload}, true)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("notify error: %s", resp.Error.Message)
	}
	result, ok := resp.Result.(map[string]any)
	if !ok || result["sent"] != true {
		return errors.New("invalid notify response")
	}
	return nil
}

type SubscriptionInfo struct {
	Subscribed     bool  `json:"subscribed"`
	SubscriptionID int64 `json:"subscription_id"`
}

// Subscribe dedicates this connection to a single event stream. Its handshake
// is bounded; the stream lives until ctx is cancelled, Close is called, or the
// transport/protocol fails. Channel closure signals loss and requires a fresh
// snapshot, never silent skipping of malformed events.
func (c *IPCClient) Subscribe(ctx context.Context) (<-chan Event, error) {
	handshakeCtx, cancel := context.WithTimeout(ctx, defaultIPCOperationTimeout)
	defer cancel()
	if err := c.acquire(handshakeCtx); err != nil {
		return nil, err
	}
	defer c.release()
	if err := c.connect(handshakeCtx); err != nil {
		return nil, err
	}
	connection, err := c.connection()
	if err != nil {
		return nil, err
	}
	resp, err := c.exchange(handshakeCtx, connection, "subscribe", nil)
	if err != nil {
		return nil, err
	}
	var info SubscriptionInfo
	if err := decodeIPCResult(resp, &info); err != nil {
		c.retire(connection)
		return nil, fmt.Errorf("subscribe: %w", err)
	}
	if !info.Subscribed || info.SubscriptionID <= 0 {
		c.retire(connection)
		return nil, errors.New("invalid subscription confirmation")
	}
	c.mu.Lock()
	if c.generation != connection.generation || c.conn == nil {
		c.mu.Unlock()
		return nil, ipcContextError(ctx, net.ErrClosed)
	}
	c.subscribed = true
	c.mu.Unlock()

	events := make(chan Event, 100)
	go c.readEvents(ctx, connection, events)
	return events, nil
}

func (c *IPCClient) readEvents(ctx context.Context, connection ipcClientConnection, events chan<- Event) {
	defer close(events)
	defer c.retire(connection)
	stop := closeIPCOnCancel(ctx, func() { c.retire(connection) })
	defer stop()
	for connection.scanner.Scan() {
		if len(connection.scanner.Bytes()) == 0 {
			continue
		}
		var message struct {
			Event *Event `json:"event"`
		}
		if err := json.Unmarshal(connection.scanner.Bytes(), &message); err != nil || message.Event == nil || message.Event.Type == "" {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-connection.done:
			return
		case events <- *message.Event:
		}
	}
}

// RequestStreamEvent is a structured event for the watch command output.
type RequestStreamEvent struct {
	Event      string `json:"event"`
	RequestID  string `json:"request_id,omitempty"`
	RiskTier   string `json:"risk_tier,omitempty"`
	Command    string `json:"command,omitempty"`
	Requestor  string `json:"requestor,omitempty"`
	ApprovedBy string `json:"approved_by,omitempty"`
	RejectedBy string `json:"rejected_by,omitempty"`
	Reason     string `json:"reason,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	CreatedAt  string `json:"created_at,omitempty"`
	ExecutedAt string `json:"executed_at,omitempty"`
}

// ToRequestStreamEvent converts a daemon Event to a RequestStreamEvent.
func ToRequestStreamEvent(e Event) *RequestStreamEvent {
	we := &RequestStreamEvent{Event: e.Type, CreatedAt: time.Unix(e.Time, 0).Format(time.RFC3339)}
	if payload, ok := e.Payload.(map[string]any); ok {
		if v, ok := payload["request_id"].(string); ok {
			we.RequestID = v
		}
		if v, ok := payload["risk_tier"].(string); ok {
			we.RiskTier = v
		}
		if v, ok := payload["command"].(string); ok {
			we.Command = v
		}
		if v, ok := payload["requestor"].(string); ok {
			we.Requestor = v
		}
		if v, ok := payload["approved_by"].(string); ok {
			we.ApprovedBy = v
		}
		if v, ok := payload["rejected_by"].(string); ok {
			we.RejectedBy = v
		}
		if v, ok := payload["reason"].(string); ok {
			we.Reason = v
		}
		if v, ok := payload["exit_code"].(float64); ok {
			code := int(v)
			we.ExitCode = &code
		}
	}
	return we
}
