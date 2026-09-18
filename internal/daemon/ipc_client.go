// Package daemon provides IPC client for communicating with the daemon.
package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// IPCClient provides methods to communicate with the daemon via IPC.
type IPCClient struct {
	socketPath string
	conn       net.Conn
	scanner    *bufio.Scanner
	mu         sync.Mutex
	nextID     atomic.Int64
}

// NewIPCClient creates a new IPC client.
func NewIPCClient(socketPath string) *IPCClient {
	return &IPCClient{
		socketPath: socketPath,
	}
}

// Connect establishes a connection to the daemon IPC socket.
func (c *IPCClient) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		return nil // Already connected
	}

	var d net.Dialer
	var conn net.Conn
	var err error

	if host := strings.TrimSpace(os.Getenv("SLB_HOST")); host != "" {
		conn, err = d.DialContext(ctx, "tcp", host)
		if err == nil {
			hello, err := json.Marshal(map[string]string{
				"auth": strings.TrimSpace(os.Getenv("SLB_SESSION_KEY")),
			})
			if err != nil {
				_ = conn.Close()
				return fmt.Errorf("marshal tcp handshake: %w", err)
			}
			hello = append(hello, '\n')
			if _, err := conn.Write(hello); err != nil {
				_ = conn.Close()
				// Fall back to Unix socket when TCP is reachable but not usable.
				conn = nil
			}
		}
	}

	if conn == nil {
		conn, err = d.DialContext(ctx, "unix", c.socketPath)
		if err != nil {
			return fmt.Errorf("connecting to daemon: %w", err)
		}
	}

	c.conn = conn
	c.scanner = bufio.NewScanner(conn)
	c.scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	return nil
}

// Close closes the connection to the daemon.
func (c *IPCClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return nil
	}

	err := c.conn.Close()
	c.conn = nil
	c.scanner = nil
	return err
}

// call sends a JSON-RPC request and returns the response.
func (c *IPCClient) call(method string, params any) (*RPCResponse, error) {
	return c.callContext(context.Background(), method, params)
}

func (c *IPCClient) callContext(ctx context.Context, method string, params any) (*RPCResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.conn == nil {
		return nil, fmt.Errorf("not connected")
	}
	conn := c.conn
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, fmt.Errorf("set IPC deadline: %w", err)
		}
		defer conn.SetDeadline(time.Time{})
	}

	id := c.nextID.Add(1)
	var paramsJSON json.RawMessage
	if params != nil {
		p, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("marshal params: %w", err)
		}
		paramsJSON = p
	}

	req := RPCRequest{Method: method, Params: paramsJSON, ID: id}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	data = append(data, '\n')

	if _, err := conn.Write(data); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("write request: %w", err)
	}
	if !c.scanner.Scan() {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err := c.scanner.Err(); err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}
		return nil, fmt.Errorf("connection closed")
	}

	var resp RPCResponse
	if err := json.Unmarshal(c.scanner.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	return &resp, nil
}

// Ping sends a ping to the daemon and verifies it's responsive.
func (c *IPCClient) Ping(ctx context.Context) error {
	if err := c.Connect(ctx); err != nil {
		return err
	}

	resp, err := c.callContext(ctx, "ping", nil)
	if err != nil {
		return err
	}

	if resp.Error != nil {
		return fmt.Errorf("ping error: %s", resp.Error.Message)
	}

	return nil
}

// DaemonStatusInfo contains daemon status information.
type DaemonStatusInfo struct {
	UptimeSeconds  int64 `json:"uptime_seconds"`
	PendingCount   int32 `json:"pending_count"`
	ActiveSessions int32 `json:"active_sessions"`
	Subscribers    int   `json:"subscribers"`
}

// Status returns the daemon's status information.
func (c *IPCClient) Status(ctx context.Context) (*DaemonStatusInfo, error) {
	if err := c.Connect(ctx); err != nil {
		return nil, err
	}

	resp, err := c.callContext(ctx, "status", nil)
	if err != nil {
		return nil, err
	}

	if resp.Error != nil {
		return nil, fmt.Errorf("status error: %s", resp.Error.Message)
	}

	// Convert result to DaemonStatusInfo
	data, err := json.Marshal(resp.Result)
	if err != nil {
		return nil, fmt.Errorf("marshal result: %w", err)
	}

	var info DaemonStatusInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("unmarshal status: %w", err)
	}

	return &info, nil
}


// HookHealth returns bounded, project-aware hook diagnostics.
func (c *IPCClient) HookHealth(ctx context.Context, cwd string) (*HookHealthResult, error) {
	if err := c.Connect(ctx); err != nil {
		return nil, err
	}
	resp, err := c.callContext(ctx, "hook_health", HookHealthParams{CWD: cwd})
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("hook health error: %s", resp.Error.Message)
	}
	data, err := json.Marshal(resp.Result)
	if err != nil {
		return nil, fmt.Errorf("marshal hook health: %w", err)
	}
	var health HookHealthResult
	if err := json.Unmarshal(data, &health); err != nil {
		return nil, fmt.Errorf("unmarshal hook health: %w", err)
	}
	return &health, nil
}

// HookQuery performs the same bounded project-aware classification used by the
// generated PreToolUse hook.
func (c *IPCClient) HookQuery(ctx context.Context, params HookQueryParams) (*HookQueryResult, error) {
	if err := c.Connect(ctx); err != nil {
		return nil, err
	}
	resp, err := c.callContext(ctx, "hook_query", params)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("hook query error: %s", resp.Error.Message)
	}
	data, err := json.Marshal(resp.Result)
	if err != nil {
		return nil, fmt.Errorf("marshal hook query: %w", err)
	}
	var result HookQueryResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("unmarshal hook query: %w", err)
	}
	return &result, nil
}

// Notify sends a notification to the daemon for broadcasting.
func (c *IPCClient) Notify(ctx context.Context, eventType string, payload any) error {
	if err := c.Connect(ctx); err != nil {
		return err
	}

	resp, err := c.callContext(ctx, "notify", NotifyParams{
		Type:    eventType,
		Payload: payload,
	})
	if err != nil {
		return err
	}

	if resp.Error != nil {
		return fmt.Errorf("notify error: %s", resp.Error.Message)
	}

	return nil
}

// SubscriptionInfo contains subscription information.
type SubscriptionInfo struct {
	Subscribed     bool  `json:"subscribed"`
	SubscriptionID int64 `json:"subscription_id"`
}

// Subscribe subscribes to daemon events. Returns a channel that receives events.
// The caller should read from the channel and call Close when done.
func (c *IPCClient) Subscribe(ctx context.Context) (<-chan Event, error) {
	if err := c.Connect(ctx); err != nil {
		return nil, err
	}

	// Subscribe is designed for long-lived event streaming.
	// Avoid issuing other RPC calls on this client while subscribed.

	c.mu.Lock()
	// Send subscribe request
	id := c.nextID.Add(1)
	req := RPCRequest{
		Method: "subscribe",
		ID:     id,
	}

	data, err := json.Marshal(req)
	if err != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	data = append(data, '\n')

	if _, err := c.conn.Write(data); err != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("write request: %w", err)
	}

	// Read subscription confirmation
	if !c.scanner.Scan() {
		c.mu.Unlock()
		if err := c.scanner.Err(); err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}
		return nil, fmt.Errorf("connection closed")
	}

	var resp RPCResponse
	if err := json.Unmarshal(c.scanner.Bytes(), &resp); err != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	if resp.Error != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("subscribe error: %s", resp.Error.Message)
	}
	c.mu.Unlock()

	// Create event channel and start reading events.
	events := make(chan Event, 100)

	go func() {
		defer close(events)
		for {
			select {
			case <-ctx.Done():
				_ = c.Close()
				return
			default:
			}

			c.mu.Lock()
			scanner := c.scanner
			c.mu.Unlock()
			if scanner == nil {
				return
			}

			if !scanner.Scan() {
				_ = c.Close()
				return
			}

			line := scanner.Bytes()

			if len(line) == 0 {
				continue
			}

			// Parse event message
			var eventMsg struct {
				Event Event `json:"event"`
			}
			if err := json.Unmarshal(line, &eventMsg); err != nil {
				continue
			}

			select {
			case events <- eventMsg.Event:
			case <-ctx.Done():
				_ = c.Close()
				return
			}
		}
	}()

	return events, nil
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
	we := &RequestStreamEvent{
		Event:     e.Type,
		CreatedAt: time.Unix(e.Time, 0).Format(time.RFC3339),
	}

	// Extract common fields from payload
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
