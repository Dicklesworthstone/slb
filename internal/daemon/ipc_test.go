package daemon

import (
	"bufio"
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

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/charmbracelet/log"
)

func newTestLogger() *log.Logger {
	return log.NewWithOptions(os.Stderr, log.Options{
		Level:           log.DebugLevel,
		ReportTimestamp: false,
	})
}

func TestNewIPCServer(t *testing.T) {
	t.Parallel()

	t.Run("creates server with valid socket path", func(t *testing.T) {
		t.Parallel()
		socketPath := filepath.Join(t.TempDir(), "test.sock")
		logger := newTestLogger()

		srv, err := NewIPCServer(socketPath, logger)
		if err != nil {
			t.Fatalf("NewIPCServer failed: %v", err)
		}
		defer srv.Stop()

		// Verify socket exists with correct permissions.
		info, err := os.Stat(socketPath)
		if err != nil {
			t.Fatalf("socket not created: %v", err)
		}
		if info.Mode().Perm() != 0600 {
			t.Errorf("socket permissions = %o, want 0600", info.Mode().Perm())
		}
	})

	t.Run("fails with empty socket path", func(t *testing.T) {
		t.Parallel()
		_, err := NewIPCServer("", newTestLogger())
		if err == nil {
			t.Error("expected error for empty socket path")
		}
	})

	t.Run("refuses to delete non-socket file", func(t *testing.T) {
		t.Parallel()
		socketPath := filepath.Join(t.TempDir(), "regular.file")

		// Create a regular file at the socket path.
		if err := os.WriteFile(socketPath, []byte("not a socket"), 0644); err != nil {
			t.Fatalf("creating regular file: %v", err)
		}

		_, err := NewIPCServer(socketPath, newTestLogger())
		if err == nil {
			t.Error("expected error when path exists but is not a socket")
		}
		if err != nil && !strings.Contains(err.Error(), "not a socket") {
			t.Errorf("expected error about non-socket, got: %v", err)
		}

		// Verify the file was NOT deleted.
		if _, statErr := os.Stat(socketPath); os.IsNotExist(statErr) {
			t.Error("regular file should not have been deleted")
		}
	})

	t.Run("removes stale socket", func(t *testing.T) {
		t.Parallel()
		socketPath := filepath.Join(t.TempDir(), "stale.sock")

		// Create a real unix socket to simulate a stale one.
		// We create a listener but don't close it until after stat check.
		ln, err := net.Listen("unix", socketPath)
		if err != nil {
			t.Fatalf("creating stale socket: %v", err)
		}

		// Verify socket exists before closing.
		if _, statErr := os.Stat(socketPath); statErr != nil {
			ln.Close()
			t.Fatalf("stale socket not created: %v", statErr)
		}

		// Now close the old listener to release the socket file for reuse.
		// The socket file should remain on disk.
		ln.Close()

		// On some systems, closing the listener removes the socket.
		// If so, we'll create a new one with just os.Mknod or skip.
		fi, statErr := os.Stat(socketPath)
		if os.IsNotExist(statErr) {
			// Socket was removed on close - this is platform-specific behavior.
			// Create a socket file using net.Listen again and leave it for cleanup.
			ln2, err := net.Listen("unix", socketPath)
			if err != nil {
				t.Fatalf("recreating stale socket: %v", err)
			}
			// Don't close ln2 - it will be cleaned up with temp dir
			_ = ln2 // intentionally kept open
		} else if statErr == nil && fi.Mode().Type()&os.ModeSocket == 0 {
			t.Fatalf("expected socket but got %v", fi.Mode())
		}

		srv, err := NewIPCServer(socketPath, newTestLogger())
		if err != nil {
			t.Fatalf("NewIPCServer failed: %v", err)
		}
		defer srv.Stop()
	})

	t.Run("refuses to delete directory", func(t *testing.T) {
		t.Parallel()
		socketPath := filepath.Join(t.TempDir(), "subdir")

		// Create a directory at the socket path.
		if err := os.Mkdir(socketPath, 0755); err != nil {
			t.Fatalf("creating directory: %v", err)
		}

		_, err := NewIPCServer(socketPath, newTestLogger())
		if err == nil {
			t.Error("expected error when path exists but is a directory")
		}
	})
}

func TestIPCServer_PingMethod(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(t.TempDir(), "ping.sock")
	srv, err := NewIPCServer(socketPath, newTestLogger())
	if err != nil {
		t.Fatalf("NewIPCServer failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = srv.Start(ctx)
	}()

	// Give server time to start.
	time.Sleep(50 * time.Millisecond)

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Send ping request.
	req := RPCRequest{Method: "ping", ID: 1}
	data, _ := json.Marshal(req)
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// Read response.
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatal("no response received")
	}

	var resp RPCResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.Error != nil {
		t.Errorf("unexpected error: %v", resp.Error)
	}
	if resp.ID != 1 {
		t.Errorf("response ID = %d, want 1", resp.ID)
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result not a map: %T", resp.Result)
	}
	if pong, _ := result["pong"].(bool); !pong {
		t.Error("expected pong: true")
	}

	_ = conn.Close()
	cancel()
	_ = srv.Stop()
}

func TestIPCServer_StatusMethod(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(t.TempDir(), "status.sock")
	srv, err := NewIPCServer(socketPath, newTestLogger())
	if err != nil {
		t.Fatalf("NewIPCServer failed: %v", err)
	}

	srv.SetPendingCount(5)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = srv.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Send status request.
	req := RPCRequest{Method: "status", ID: 2}
	data, _ := json.Marshal(req)
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatal("no response received")
	}

	var resp RPCResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.Error != nil {
		t.Errorf("unexpected error: %v", resp.Error)
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result not a map: %T", resp.Result)
	}

	if pending, _ := result["pending_count"].(float64); pending != 5 {
		t.Errorf("pending_count = %v, want 5", result["pending_count"])
	}
	if _, ok := result["uptime_seconds"]; !ok {
		t.Error("expected uptime_seconds in status")
	}
	if _, ok := result["active_sessions"]; !ok {
		t.Error("expected active_sessions in status")
	}

	_ = conn.Close()
	cancel()
	_ = srv.Stop()
}

func TestIPCServer_NotifyMethod(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(t.TempDir(), "notify.sock")
	srv, err := NewIPCServer(socketPath, newTestLogger())
	if err != nil {
		t.Fatalf("NewIPCServer failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = srv.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Send notify request.
	params, _ := json.Marshal(NotifyParams{Type: "test_event", Payload: map[string]string{"key": "value"}})
	req := RPCRequest{Method: "notify", Params: params, ID: 3}
	data, _ := json.Marshal(req)
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatal("no response received")
	}

	var resp RPCResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.Error != nil {
		t.Errorf("unexpected error: %v", resp.Error)
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result not a map: %T", resp.Result)
	}
	if sent, _ := result["sent"].(bool); !sent {
		t.Error("expected sent: true")
	}

	_ = conn.Close()
	cancel()
	_ = srv.Stop()
}

func TestIPCServer_NotifyMethod_MissingType(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(t.TempDir(), "notify-missing.sock")
	srv, err := NewIPCServer(socketPath, newTestLogger())
	if err != nil {
		t.Fatalf("NewIPCServer failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = srv.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Send notify without type.
	params, _ := json.Marshal(NotifyParams{Payload: "data"})
	req := RPCRequest{Method: "notify", Params: params, ID: 4}
	data, _ := json.Marshal(req)
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatal("no response received")
	}

	var resp RPCResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.Error == nil {
		t.Error("expected error for missing type")
	}
	if resp.Error != nil && resp.Error.Code != ErrCodeInvalidParams {
		t.Errorf("error code = %d, want %d", resp.Error.Code, ErrCodeInvalidParams)
	}

	_ = conn.Close()
	cancel()
	_ = srv.Stop()
}

func TestIPCServer_MethodNotFound(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(t.TempDir(), "unknown.sock")
	srv, err := NewIPCServer(socketPath, newTestLogger())
	if err != nil {
		t.Fatalf("NewIPCServer failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = srv.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	req := RPCRequest{Method: "unknown_method", ID: 5}
	data, _ := json.Marshal(req)
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatal("no response received")
	}

	var resp RPCResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.Error == nil {
		t.Error("expected error for unknown method")
	}
	if resp.Error != nil && resp.Error.Code != ErrCodeMethodNotFound {
		t.Errorf("error code = %d, want %d", resp.Error.Code, ErrCodeMethodNotFound)
	}

	_ = conn.Close()
	cancel()
	_ = srv.Stop()
}

func TestIPCServer_ParseError(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(t.TempDir(), "parse.sock")
	srv, err := NewIPCServer(socketPath, newTestLogger())
	if err != nil {
		t.Fatalf("NewIPCServer failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = srv.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Send invalid JSON.
	if _, err := conn.Write([]byte("not valid json\n")); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatal("no response received")
	}

	var resp RPCResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.Error == nil {
		t.Error("expected parse error")
	}
	if resp.Error != nil && resp.Error.Code != ErrCodeParse {
		t.Errorf("error code = %d, want %d", resp.Error.Code, ErrCodeParse)
	}

	_ = conn.Close()
	cancel()
	_ = srv.Stop()
}

func TestIPCServer_Subscribe(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(t.TempDir(), "subscribe.sock")
	srv, err := NewIPCServer(socketPath, newTestLogger())
	if err != nil {
		t.Fatalf("NewIPCServer failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = srv.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Subscribe.
	req := RPCRequest{Method: "subscribe", ID: 6}
	data, _ := json.Marshal(req)
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatal("no response received")
	}

	var resp RPCResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.Error != nil {
		t.Errorf("unexpected error: %v", resp.Error)
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result not a map: %T", resp.Result)
	}
	if subscribed, _ := result["subscribed"].(bool); !subscribed {
		t.Error("expected subscribed: true")
	}
	if _, ok := result["subscription_id"]; !ok {
		t.Error("expected subscription_id in response")
	}

	// Broadcast an event.
	srv.BroadcastEvent("test_event", map[string]string{"msg": "hello"})

	// Give time for event delivery.
	time.Sleep(50 * time.Millisecond)

	// Set read deadline to avoid hanging.
	conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))

	if scanner.Scan() {
		var eventMsg map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &eventMsg); err != nil {
			t.Fatalf("unmarshal event: %v", err)
		}
		event, ok := eventMsg["event"].(map[string]any)
		if !ok {
			t.Fatalf("event not found in message: %v", eventMsg)
		}
		if event["type"] != "test_event" {
			t.Errorf("event type = %v, want test_event", event["type"])
		}
	}

	_ = conn.Close()
	cancel()
	_ = srv.Stop()
}

func TestIPCServer_MultipleClients(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(t.TempDir(), "multi.sock")
	srv, err := NewIPCServer(socketPath, newTestLogger())
	if err != nil {
		t.Fatalf("NewIPCServer failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = srv.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	// Connect multiple clients.
	const numClients = 5
	conns := make([]net.Conn, numClients)

	for i := range numClients {
		conn, err := net.Dial("unix", socketPath)
		if err != nil {
			t.Fatalf("dial client %d failed: %v", i, err)
		}
		conns[i] = conn
	}

	// Each client sends ping.
	for i, conn := range conns {
		req := RPCRequest{Method: "ping", ID: int64(i + 1)}
		data, _ := json.Marshal(req)
		data = append(data, '\n')
		if _, err := conn.Write(data); err != nil {
			t.Fatalf("write client %d failed: %v", i, err)
		}
	}

	// Each client reads response.
	for i, conn := range conns {
		scanner := bufio.NewScanner(conn)
		if !scanner.Scan() {
			t.Fatalf("no response for client %d", i)
		}

		var resp RPCResponse
		if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal response %d: %v", i, err)
		}
		if resp.Error != nil {
			t.Errorf("client %d error: %v", i, resp.Error)
		}
	}

	// Cleanup.
	for _, conn := range conns {
		conn.Close()
	}

	cancel()
	_ = srv.Stop()
}

func TestIPCServer_GracefulShutdown(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(t.TempDir(), "shutdown.sock")
	srv, err := NewIPCServer(socketPath, newTestLogger())
	if err != nil {
		t.Fatalf("NewIPCServer failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- srv.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	// Connect a client.
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Cancel context and stop server.
	_ = conn.Close()
	cancel()
	if err := srv.Stop(); err != nil {
		t.Errorf("Stop failed: %v", err)
	}

	// Verify socket is cleaned up.
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Error("socket file should be removed after stop")
	}

	// Server should exit cleanly.
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("server did not exit in time")
	}
}

func TestIPCClient_call_NotConnected(t *testing.T) {
	client := NewIPCClient("/tmp/does-not-matter.sock")
	if _, err := client.call("ping", nil); err == nil {
		t.Fatalf("expected error when calling without Connect")
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close should be idempotent, got: %v", err)
	}
}

func TestIPCClient_PingStatusNotify_Unix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket tests not supported on windows")
	}

	socketPath := filepath.Join(t.TempDir(), "ipc.sock")
	logger := log.New(io.Discard)
	srv, err := NewIPCServer(socketPath, logger)
	if err != nil {
		t.Fatalf("NewIPCServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = srv.Stop()
	})
	go func() { _ = srv.Start(ctx) }()

	callCtx, callCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer callCancel()

	client := NewIPCClient(socketPath)
	t.Cleanup(func() { _ = client.Close() })

	if err := client.Ping(callCtx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	info, err := client.Status(callCtx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if info == nil {
		t.Fatalf("expected status info")
	}

	if err := client.Notify(callCtx, "request_pending", map[string]any{
		"request_id": "req-1",
	}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
}

func TestIPCClient_SubscribeReceivesEvents_Unix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket tests not supported on windows")
	}

	socketPath := filepath.Join(t.TempDir(), "ipc.sock")
	logger := log.New(io.Discard)
	srv, err := NewIPCServer(socketPath, logger)
	if err != nil {
		t.Fatalf("NewIPCServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = srv.Stop()
	})
	go func() { _ = srv.Start(ctx) }()

	subCtx, subCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer subCancel()

	subscriber := NewIPCClient(socketPath)
	t.Cleanup(func() { _ = subscriber.Close() })

	events, err := subscriber.Subscribe(subCtx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	publisher := NewIPCClient(socketPath)
	t.Cleanup(func() { _ = publisher.Close() })

	callCtx, callCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer callCancel()

	if err := publisher.Notify(callCtx, "request_executed", map[string]any{
		"request_id":  "req-123",
		"risk_tier":   "critical",
		"command":     "rm -rf /tmp/x",
		"requestor":   "AgentA",
		"approved_by": "AgentB",
		"exit_code":   7,
	}); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	select {
	case ev := <-events:
		if ev.Type != "request_executed" {
			t.Fatalf("unexpected event type: %s", ev.Type)
		}

		stream := ToRequestStreamEvent(ev)
		if stream == nil {
			t.Fatalf("expected stream event")
		}
		if stream.Event != "request_executed" || stream.RequestID != "req-123" || stream.RiskTier != "critical" {
			t.Fatalf("unexpected stream mapping: %+v", stream)
		}
		if stream.ExitCode == nil || *stream.ExitCode != 7 {
			t.Fatalf("expected exit_code=7, got %+v", stream.ExitCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for event")
	}
}

func TestIPCClient_ConnectFallsBackToUnixWhenSLBHostInvalid(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket tests not supported on windows")
	}

	t.Setenv("SLB_HOST", "127.0.0.1:0")
	t.Setenv("SLB_SESSION_KEY", "ignored")

	socketPath := filepath.Join(t.TempDir(), "ipc.sock")
	logger := log.New(io.Discard)
	srv, err := NewIPCServer(socketPath, logger)
	if err != nil {
		t.Fatalf("NewIPCServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = srv.Stop()
	})
	go func() { _ = srv.Start(ctx) }()

	callCtx, callCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer callCancel()

	client := NewIPCClient(socketPath)
	t.Cleanup(func() { _ = client.Close() })

	if err := client.Ping(callCtx); err != nil {
		t.Fatalf("Ping (expected unix fallback): %v", err)
	}
}

func TestIPCClient_PingOverTCPWithSLBHost(t *testing.T) {
	logger := log.New(io.Discard)

	srv, err := NewTCPServer(TCPServerOptions{
		Addr:        "127.0.0.1:0",
		RequireAuth: true,
		AllowedIPs:  []string{"127.0.0.1"},
		ValidateAuth: func(_ context.Context, sessionKey string) (bool, error) {
			return sessionKey == "good", nil
		},
	}, logger)
	if err != nil {
		t.Fatalf("NewTCPServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = srv.Stop()
	})
	go func() { _ = srv.Start(ctx) }()

	addr := srv.listener.Addr().String()
	t.Setenv("SLB_HOST", addr)
	t.Setenv("SLB_SESSION_KEY", "good")

	callCtx, callCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer callCancel()

	client := NewIPCClient("/tmp/unused.sock")
	t.Cleanup(func() { _ = client.Close() })

	if err := client.Ping(callCtx); err != nil {
		t.Fatalf("Ping over TCP: %v", err)
	}
}

func TestIPCServer_handleVerifyExecute_VerifierNotConfigured(t *testing.T) {
	t.Parallel()

	srv := &IPCServer{}
	req := RPCRequest{
		Method: "verify_execute",
		Params: json.RawMessage(`{"request_id":"req-1","session_id":"sess-1"}`),
		ID:     1,
	}

	resp := srv.handleVerifyExecute(req)
	if resp.Error == nil {
		t.Fatalf("expected error")
	}
	if resp.Error.Code != ErrCodeInternal {
		t.Fatalf("error code=%d want %d", resp.Error.Code, ErrCodeInternal)
	}
}

func TestIPCServer_handleVerifyExecute_InvalidParams(t *testing.T) {
	t.Parallel()

	srv := &IPCServer{}
	srv.SetVerifier(NewVerifier(setupTestDB(t)))

	req := RPCRequest{
		Method: "verify_execute",
		Params: json.RawMessage(`not-json`),
		ID:     1,
	}

	resp := srv.handleVerifyExecute(req)
	if resp.Error == nil {
		t.Fatalf("expected error")
	}
	if resp.Error.Code != ErrCodeInvalidParams {
		t.Fatalf("error code=%d want %d", resp.Error.Code, ErrCodeInvalidParams)
	}
}

func TestIPCServer_handleVerifyExecute_RequiresRequestIDAndSessionID(t *testing.T) {
	t.Parallel()

	srv := &IPCServer{}
	srv.SetVerifier(NewVerifier(setupTestDB(t)))

	t.Run("missing request_id", func(t *testing.T) {
		req := RPCRequest{
			Method: "verify_execute",
			Params: json.RawMessage(`{"session_id":"sess-1"}`),
			ID:     1,
		}
		resp := srv.handleVerifyExecute(req)
		if resp.Error == nil {
			t.Fatalf("expected error")
		}
		if resp.Error.Code != ErrCodeInvalidParams {
			t.Fatalf("error code=%d want %d", resp.Error.Code, ErrCodeInvalidParams)
		}
	})

	t.Run("missing session_id", func(t *testing.T) {
		req := RPCRequest{
			Method: "verify_execute",
			Params: json.RawMessage(`{"request_id":"req-1"}`),
			ID:     1,
		}
		resp := srv.handleVerifyExecute(req)
		if resp.Error == nil {
			t.Fatalf("expected error")
		}
		if resp.Error.Code != ErrCodeInvalidParams {
			t.Fatalf("error code=%d want %d", resp.Error.Code, ErrCodeInvalidParams)
		}
	})
}

func TestIPCServer_handleVerifyExecute_RequestNotFound(t *testing.T) {
	t.Parallel()

	srv := &IPCServer{}
	srv.SetVerifier(NewVerifier(setupTestDB(t)))

	req := RPCRequest{
		Method: "verify_execute",
		Params: json.RawMessage(`{"request_id":"does-not-exist","session_id":"sess-1"}`),
		ID:     1,
	}

	resp := srv.handleVerifyExecute(req)
	if resp.Error == nil {
		t.Fatalf("expected error")
	}
	if resp.Error.Code != ErrCodeInternal {
		t.Fatalf("error code=%d want %d", resp.Error.Code, ErrCodeInternal)
	}
}

func TestIPCServer_handleVerifyExecute_AllowedMarksExecuting(t *testing.T) {
	t.Parallel()

	database := setupTestDB(t)
	requestor := createTestSession(t, database, "sess-requestor")
	createTestRequest(t, database, "req-1", requestor.ID, db.StatusApproved, 1)

	reviewer := createTestSession(t, database, "sess-reviewer")
	createTestReview(t, database, "req-1", reviewer.ID, db.DecisionApprove)

	srv := &IPCServer{}
	srv.SetVerifier(NewVerifier(database))

	req := RPCRequest{
		Method: "verify_execute",
		Params: json.RawMessage(`{"request_id":"req-1","session_id":"sess-executor"}`),
		ID:     1,
	}

	resp := srv.handleVerifyExecute(req)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	out, ok := resp.Result.(*VerifyExecuteResponse)
	if !ok {
		t.Fatalf("unexpected result type: %T", resp.Result)
	}
	if !out.Allowed {
		t.Fatalf("expected allowed, got denied: %s", out.Reason)
	}
	if out.RequestID != "req-1" {
		t.Fatalf("request_id=%q want %q", out.RequestID, "req-1")
	}

	got, err := database.GetRequest("req-1")
	if err != nil {
		t.Fatalf("GetRequest: %v", err)
	}
	if got.Status != db.StatusExecuting {
		t.Fatalf("status=%s want %s", got.Status, db.StatusExecuting)
	}
}

func TestIPCServer_handleVerifyExecute_DeniedDoesNotMarkExecuting(t *testing.T) {
	t.Parallel()

	database := setupTestDB(t)
	requestor := createTestSession(t, database, "sess-requestor")
	createTestRequest(t, database, "req-1", requestor.ID, db.StatusApproved, 2)

	reviewer := createTestSession(t, database, "sess-reviewer")
	createTestReview(t, database, "req-1", reviewer.ID, db.DecisionApprove)

	srv := &IPCServer{}
	srv.SetVerifier(NewVerifier(database))

	req := RPCRequest{
		Method: "verify_execute",
		Params: json.RawMessage(`{"request_id":"req-1","session_id":"sess-executor"}`),
		ID:     1,
	}

	resp := srv.handleVerifyExecute(req)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	out, ok := resp.Result.(*VerifyExecuteResponse)
	if !ok {
		t.Fatalf("unexpected result type: %T", resp.Result)
	}
	if out.Allowed {
		t.Fatalf("expected denied")
	}

	got, err := database.GetRequest("req-1")
	if err != nil {
		t.Fatalf("GetRequest: %v", err)
	}
	if got.Status != db.StatusApproved {
		t.Fatalf("status=%s want %s", got.Status, db.StatusApproved)
	}
}
