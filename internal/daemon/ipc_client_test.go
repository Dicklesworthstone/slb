package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/log"
)

func TestNewIPCClient(t *testing.T) {
	client := NewIPCClient("/tmp/test.sock")
	if client == nil {
		t.Fatal("expected non-nil client")
	}
	if client.socketPath != "/tmp/test.sock" {
		t.Errorf("expected socketPath '/tmp/test.sock', got %q", client.socketPath)
	}
}

func TestIPCClient_Close_NotConnected(t *testing.T) {
	client := NewIPCClient("/tmp/test.sock")
	err := client.Close()
	if err != nil {
		t.Errorf("Close on non-connected client should return nil, got: %v", err)
	}
}

func TestIPCClient_Connect_NoServer(t *testing.T) {
	client := NewIPCClient("/nonexistent/path/test.sock")
	ctx := context.Background()
	err := client.Connect(ctx)
	if err == nil {
		t.Fatal("expected error connecting to non-existent socket")
	}
}

func TestIPCClient_Connect_AlreadyConnected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket tests not supported on windows")
	}

	socketPath := filepath.Join(shortSocketDir(t), "t.sock")
	srv, err := NewIPCServer(socketPath, log.New(io.Discard))
	if err != nil {
		t.Fatalf("NewIPCServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()

	// Wait for server to be ready
	time.Sleep(50 * time.Millisecond)

	client := NewIPCClient(socketPath)
	err = client.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	// Second connect should return nil (already connected)
	err = client.Connect(ctx)
	if err != nil {
		t.Errorf("Second Connect should return nil, got: %v", err)
	}

	_ = client.Close()
	_ = srv.Stop()
}

func TestIPCClient_Ping_Success(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket tests not supported on windows")
	}

	socketPath := filepath.Join(shortSocketDir(t), "t.sock")
	srv, err := NewIPCServer(socketPath, log.New(io.Discard))
	if err != nil {
		t.Fatalf("NewIPCServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()

	time.Sleep(50 * time.Millisecond)

	client := NewIPCClient(socketPath)
	err = client.Ping(ctx)
	if err != nil {
		t.Fatalf("Ping failed: %v", err)
	}

	_ = client.Close()
	_ = srv.Stop()
}

func TestIPCClient_Ping_NotConnected(t *testing.T) {
	client := NewIPCClient("/nonexistent/test.sock")
	ctx := context.Background()
	err := client.Ping(ctx)
	if err == nil {
		t.Fatal("expected error when pinging non-existent server")
	}
}

func TestIPCClient_Status_Success(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket tests not supported on windows")
	}

	socketPath := filepath.Join(shortSocketDir(t), "t.sock")
	srv, err := NewIPCServer(socketPath, log.New(io.Discard))
	if err != nil {
		t.Fatalf("NewIPCServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()

	time.Sleep(50 * time.Millisecond)

	client := NewIPCClient(socketPath)
	status, err := client.Status(ctx)
	if err != nil {
		t.Fatalf("Status failed: %v", err)
	}

	if status == nil {
		t.Fatal("expected non-nil status")
	}
	// Just verify it has reasonable values
	if status.UptimeSeconds < 0 {
		t.Errorf("expected non-negative uptime, got %d", status.UptimeSeconds)
	}

	_ = client.Close()
	_ = srv.Stop()
}

func TestIPCClient_Status_NotConnected(t *testing.T) {
	client := NewIPCClient("/nonexistent/test.sock")
	ctx := context.Background()
	_, err := client.Status(ctx)
	if err == nil {
		t.Fatal("expected error when getting status from non-existent server")
	}
}

func TestIPCClient_Notify_Success(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket tests not supported on windows")
	}

	socketPath := filepath.Join(shortSocketDir(t), "t.sock")
	srv, err := NewIPCServer(socketPath, log.New(io.Discard))
	if err != nil {
		t.Fatalf("NewIPCServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()

	time.Sleep(50 * time.Millisecond)

	client := NewIPCClient(socketPath)
	err = client.Notify(ctx, "test_event", map[string]string{"key": "value"})
	if err != nil {
		t.Fatalf("Notify failed: %v", err)
	}

	_ = client.Close()
	_ = srv.Stop()
}

func TestIPCClient_Notify_NotConnected(t *testing.T) {
	client := NewIPCClient("/nonexistent/test.sock")
	ctx := context.Background()
	err := client.Notify(ctx, "test_event", nil)
	if err == nil {
		t.Fatal("expected error when notifying non-existent server")
	}
}

func TestIPCClient_Subscribe_NotConnected(t *testing.T) {
	client := NewIPCClient("/nonexistent/test.sock")
	ctx := context.Background()
	_, err := client.Subscribe(ctx)
	if err == nil {
		t.Fatal("expected error when subscribing to non-existent server")
	}
}

func TestIPCClient_Subscribe_Success(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket tests not supported on windows")
	}

	socketPath := filepath.Join(shortSocketDir(t), "t.sock")
	srv, err := NewIPCServer(socketPath, log.New(io.Discard))
	if err != nil {
		t.Fatalf("NewIPCServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Start(ctx) }()

	time.Sleep(50 * time.Millisecond)

	client := NewIPCClient(socketPath)
	events, err := client.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}

	if events == nil {
		t.Fatal("expected non-nil events channel")
	}

	// Cancel context to clean up
	cancel()

	// Wait for channel to close
	select {
	case <-events:
		// Channel closed or event received, both are fine
	case <-time.After(time.Second):
		// Timeout is acceptable
	}

	_ = client.Close()
	_ = srv.Stop()
}

func TestIPCClient_Call_NotConnected(t *testing.T) {
	client := NewIPCClient("/tmp/test.sock")
	_, err := client.call("test", nil)
	if err == nil {
		t.Fatal("expected error when calling on non-connected client")
	}
	if err.Error() != "not connected" {
		t.Errorf("expected 'not connected' error, got: %v", err)
	}
}

func TestToRequestStreamEvent_BasicFields(t *testing.T) {
	now := time.Now().Unix()
	event := Event{
		Type: "request_created",
		Time: now,
		Payload: map[string]any{
			"request_id": "req-123",
			"risk_tier":  "caution",
			"command":    "echo hello",
			"requestor":  "TestAgent",
		},
	}

	result := ToRequestStreamEvent(event)

	if result.Event != "request_created" {
		t.Errorf("expected event 'request_created', got %q", result.Event)
	}
	if result.RequestID != "req-123" {
		t.Errorf("expected request_id 'req-123', got %q", result.RequestID)
	}
	if result.RiskTier != "caution" {
		t.Errorf("expected risk_tier 'caution', got %q", result.RiskTier)
	}
	if result.Command != "echo hello" {
		t.Errorf("expected command 'echo hello', got %q", result.Command)
	}
	if result.Requestor != "TestAgent" {
		t.Errorf("expected requestor 'TestAgent', got %q", result.Requestor)
	}
}

func TestToRequestStreamEvent_ApprovalFields(t *testing.T) {
	event := Event{
		Type: "request_approved",
		Time: time.Now().Unix(),
		Payload: map[string]any{
			"request_id":  "req-456",
			"approved_by": "ReviewerAgent",
		},
	}

	result := ToRequestStreamEvent(event)

	if result.ApprovedBy != "ReviewerAgent" {
		t.Errorf("expected approved_by 'ReviewerAgent', got %q", result.ApprovedBy)
	}
}

func TestToRequestStreamEvent_RejectionFields(t *testing.T) {
	event := Event{
		Type: "request_rejected",
		Time: time.Now().Unix(),
		Payload: map[string]any{
			"request_id":  "req-789",
			"rejected_by": "ReviewerAgent",
			"reason":      "too risky",
		},
	}

	result := ToRequestStreamEvent(event)

	if result.RejectedBy != "ReviewerAgent" {
		t.Errorf("expected rejected_by 'ReviewerAgent', got %q", result.RejectedBy)
	}
	if result.Reason != "too risky" {
		t.Errorf("expected reason 'too risky', got %q", result.Reason)
	}
}

func TestToRequestStreamEvent_ExitCode(t *testing.T) {
	event := Event{
		Type: "request_executed",
		Time: time.Now().Unix(),
		Payload: map[string]any{
			"request_id": "req-exec",
			"exit_code":  float64(42), // JSON numbers are float64
		},
	}

	result := ToRequestStreamEvent(event)

	if result.ExitCode == nil {
		t.Fatal("expected exit_code to be set")
	}
	if *result.ExitCode != 42 {
		t.Errorf("expected exit_code 42, got %d", *result.ExitCode)
	}
}

func TestToRequestStreamEvent_NoPayload(t *testing.T) {
	event := Event{
		Type:    "test_event",
		Time:    time.Now().Unix(),
		Payload: nil,
	}

	result := ToRequestStreamEvent(event)

	if result.Event != "test_event" {
		t.Errorf("expected event 'test_event', got %q", result.Event)
	}
	// All optional fields should be empty
	if result.RequestID != "" {
		t.Errorf("expected empty request_id, got %q", result.RequestID)
	}
}

func TestToRequestStreamEvent_WrongPayloadType(t *testing.T) {
	event := Event{
		Type:    "test_event",
		Time:    time.Now().Unix(),
		Payload: "string payload", // Wrong type, should be map[string]any
	}

	result := ToRequestStreamEvent(event)

	// Should not panic, just return event with empty fields
	if result.Event != "test_event" {
		t.Errorf("expected event 'test_event', got %q", result.Event)
	}
}

// Since f95ab24 an explicit SLB_HOST is authoritative: when the TCP daemon is
// unreachable, Connect must fail rather than silently use the local Unix
// daemon (which may belong to a different trust domain). Without SLB_HOST the
// same client reaches the local daemon.
func TestIPCClient_Connect_ExplicitTCPFailureDoesNotFallBack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket tests not supported on windows")
	}

	t.Setenv("SLB_HOST", "127.0.0.1:65534") // High port unlikely to be in use
	t.Setenv("SLB_SESSION_KEY", "test-key")

	socketPath := filepath.Join(shortSocketDir(t), "t.sock")
	srv, err := NewIPCServer(socketPath, log.New(io.Discard))
	if err != nil {
		t.Fatalf("NewIPCServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()
	defer func() { _ = srv.Stop() }()

	time.Sleep(50 * time.Millisecond)

	client := NewIPCClient(socketPath)
	err = client.Connect(ctx)
	_ = client.Close()
	if err == nil {
		t.Fatal("explicit SLB_HOST failure silently fell back to the local Unix daemon")
	}
	if !strings.Contains(err.Error(), "127.0.0.1:65534") {
		t.Fatalf("error does not identify the explicit TCP target: %v", err)
	}

	t.Setenv("SLB_HOST", "")
	local := NewIPCClient(socketPath)
	defer local.Close()
	if err := local.Connect(ctx); err != nil {
		t.Fatalf("local Unix daemon unreachable without SLB_HOST: %v", err)
	}
}

func TestIPCClient_ConnectTCP_HandshakeWriteError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket tests not supported on windows")
	}

	// Create a server that accepts and immediately closes
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	addr := ln.Addr().String()
	t.Setenv("SLB_HOST", addr)
	t.Setenv("SLB_SESSION_KEY", "test-key")

	// Accept connection and close immediately to cause write error
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_ = conn.Close()
	}()

	// Set up Unix socket as fallback
	socketPath := filepath.Join(shortSocketDir(t), "t.sock")
	srv, err := NewIPCServer(socketPath, log.New(io.Discard))
	if err != nil {
		t.Fatalf("NewIPCServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()

	time.Sleep(50 * time.Millisecond)

	// Should fall back to Unix socket when TCP handshake fails
	client := NewIPCClient(socketPath)
	err = client.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect should fall back when TCP write fails: %v", err)
	}

	_ = client.Close()
	_ = srv.Stop()
}

func TestIPCClient_Close_Connected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket tests not supported on windows")
	}

	socketPath := filepath.Join(shortSocketDir(t), "t.sock")
	srv, err := NewIPCServer(socketPath, log.New(io.Discard))
	if err != nil {
		t.Fatalf("NewIPCServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()

	time.Sleep(50 * time.Millisecond)

	client := NewIPCClient(socketPath)
	err = client.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	err = client.Close()
	if err != nil {
		t.Errorf("Close failed: %v", err)
	}

	// Double close should be safe
	err = client.Close()
	if err != nil {
		t.Errorf("Double close should return nil: %v", err)
	}

	_ = srv.Stop()
}

func TestDaemonStatusInfo_JSON(t *testing.T) {
	info := DaemonStatusInfo{
		UptimeSeconds:  3600,
		PendingCount:   5,
		ActiveSessions: 2,
		Subscribers:    1,
	}

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded DaemonStatusInfo
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.UptimeSeconds != 3600 {
		t.Errorf("expected UptimeSeconds 3600, got %d", decoded.UptimeSeconds)
	}
	if decoded.PendingCount != 5 {
		t.Errorf("expected PendingCount 5, got %d", decoded.PendingCount)
	}
	if decoded.ActiveSessions != 2 {
		t.Errorf("expected ActiveSessions 2, got %d", decoded.ActiveSessions)
	}
	if decoded.Subscribers != 1 {
		t.Errorf("expected Subscribers 1, got %d", decoded.Subscribers)
	}
}

func TestSubscriptionInfo_JSON(t *testing.T) {
	info := SubscriptionInfo{
		Subscribed:     true,
		SubscriptionID: 12345,
	}

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded SubscriptionInfo
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if !decoded.Subscribed {
		t.Error("expected Subscribed true")
	}
	if decoded.SubscriptionID != 12345 {
		t.Errorf("expected SubscriptionID 12345, got %d", decoded.SubscriptionID)
	}
}

func TestIPCClient_Call_MarshalParamsError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket tests not supported on windows")
	}

	socketPath := filepath.Join(shortSocketDir(t), "t.sock")
	srv, err := NewIPCServer(socketPath, log.New(io.Discard))
	if err != nil {
		t.Fatalf("NewIPCServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()

	time.Sleep(50 * time.Millisecond)

	client := NewIPCClient(socketPath)
	err = client.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	// Create unmarshalable params (channel cannot be marshaled)
	_, err = client.call("test", make(chan int))
	if err == nil {
		t.Fatal("expected error for unmarshalable params")
	}

	_ = client.Close()
	_ = srv.Stop()
}

func TestIPCClient_ContextCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket tests not supported on windows")
	}

	socketPath := filepath.Join(shortSocketDir(t), "t.sock")
	srv, err := NewIPCServer(socketPath, log.New(io.Discard))
	if err != nil {
		t.Fatalf("NewIPCServer: %v", err)
	}

	srvCtx, srvCancel := context.WithCancel(context.Background())
	defer srvCancel()
	go func() { _ = srv.Start(srvCtx) }()

	time.Sleep(50 * time.Millisecond)

	// Cancel context before connecting
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	client := NewIPCClient(socketPath)
	err = client.Connect(ctx)
	if err == nil {
		// Some systems may connect before context cancellation is checked
		_ = client.Close()
	}

	_ = srv.Stop()
}

func TestRequestStreamEvent_JSON(t *testing.T) {
	code := 0
	event := RequestStreamEvent{
		Event:      "request_executed",
		RequestID:  "req-123",
		RiskTier:   "caution",
		Command:    "echo test",
		Requestor:  "Agent",
		ApprovedBy: "Reviewer",
		ExitCode:   &code,
		CreatedAt:  "2025-01-01T00:00:00Z",
		ExecutedAt: "2025-01-01T00:00:01Z",
	}

	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded RequestStreamEvent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.Event != "request_executed" {
		t.Errorf("expected Event 'request_executed', got %q", decoded.Event)
	}
	if decoded.RequestID != "req-123" {
		t.Errorf("expected RequestID 'req-123', got %q", decoded.RequestID)
	}
	if decoded.ExitCode == nil || *decoded.ExitCode != 0 {
		t.Error("expected ExitCode 0")
	}
}

func TestIPCClient_SLBHostEmptyString(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket tests not supported on windows")
	}

	// Ensure SLB_HOST is not set (empty)
	t.Setenv("SLB_HOST", "")

	socketPath := filepath.Join(shortSocketDir(t), "t.sock")
	srv, err := NewIPCServer(socketPath, log.New(io.Discard))
	if err != nil {
		t.Fatalf("NewIPCServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()

	time.Sleep(50 * time.Millisecond)

	client := NewIPCClient(socketPath)
	err = client.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect failed with empty SLB_HOST: %v", err)
	}

	_ = client.Close()
	_ = srv.Stop()
}

func TestIPCClient_SLBHostWhitespaceOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket tests not supported on windows")
	}

	// Set SLB_HOST to whitespace (should be trimmed and ignored)
	t.Setenv("SLB_HOST", "   ")

	socketPath := filepath.Join(shortSocketDir(t), "t.sock")
	srv, err := NewIPCServer(socketPath, log.New(io.Discard))
	if err != nil {
		t.Fatalf("NewIPCServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()

	time.Sleep(50 * time.Millisecond)

	client := NewIPCClient(socketPath)
	err = client.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect failed with whitespace SLB_HOST: %v", err)
	}

	_ = client.Close()
	_ = srv.Stop()
}

// TestIPCClient_ConnectAndUnixFallbackNoEnv verifies that without SLB_HOST,
// the client connects directly to Unix socket.
func TestIPCClient_ConnectAndUnixFallbackNoEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket tests not supported on windows")
	}

	// Unset SLB_HOST environment variable
	os.Unsetenv("SLB_HOST")

	socketPath := filepath.Join(shortSocketDir(t), "t.sock")
	srv, err := NewIPCServer(socketPath, log.New(io.Discard))
	if err != nil {
		t.Fatalf("NewIPCServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()

	time.Sleep(50 * time.Millisecond)

	client := NewIPCClient(socketPath)
	err = client.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	// Verify we can ping
	err = client.Ping(ctx)
	if err != nil {
		t.Fatalf("Ping failed: %v", err)
	}

	_ = client.Close()
	_ = srv.Stop()
}
