package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/log"
)

type observedIPCWrite struct {
	net.Conn
	entered chan struct{}
	once    sync.Once
}

func (c *observedIPCWrite) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.entered) })
	return c.Conn.Write(p)
}

func pipeIPCClient(t *testing.T) (*IPCClient, net.Conn) {
	t.Helper()
	local, peer := net.Pipe()
	client := NewIPCClient("unused")
	client.conn, client.scanner, client.connDone = local, bufio.NewScanner(local), make(chan struct{})
	t.Cleanup(func() { _ = client.Close(); _ = peer.Close() })
	return client, peer
}

func receiveIPCRequest(t *testing.T, peer net.Conn) RPCRequest {
	t.Helper()
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	var request RPCRequest
	if err := json.NewDecoder(peer).Decode(&request); err != nil {
		t.Fatal(err)
	}
	_ = peer.SetReadDeadline(time.Time{})
	return request
}

func requireIPCResult(t *testing.T, done <-chan error, want error) {
	t.Helper()
	select {
	case err := <-done:
		if err == nil || (want != nil && !errors.Is(err, want)) {
			t.Fatalf("got %v, want %v", err, want)
		}
	case <-time.After(time.Second):
		t.Fatal("IPC operation remained blocked")
	}
}

func requireIPCStreamClosed(t *testing.T, events <-chan Event) {
	t.Helper()
	timeout := time.After(time.Second)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("IPC subscription remained open")
		}
	}
}

func TestIPCClientCancellationAndCloseInterruptIO(t *testing.T) {
	for _, mode := range []string{"cancel-read", "close-read", "cancel-write", "close-write", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			client, peer := pipeIPCClient(t)
			var writing <-chan struct{}
			if strings.HasSuffix(mode, "write") {
				observed := &observedIPCWrite{Conn: client.conn, entered: make(chan struct{})}
				client.conn = observed
				writing = observed.entered
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "deadline" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 50*time.Millisecond)
				defer stop()
			}
			done := make(chan error, 1)
			go func() { _, err := client.callContext(ctx, "status", nil); done <- err }()
			if !strings.HasSuffix(mode, "write") {
				receiveIPCRequest(t, peer)
			} else {
				select {
				case <-writing:
				case <-time.After(time.Second):
					t.Fatal("RPC did not begin its write")
				}
			}
			var want error
			switch {
			case strings.HasPrefix(mode, "cancel"):
				cancel()
				want = context.Canceled
			case strings.HasPrefix(mode, "close"):
				closed := make(chan error, 1)
				go func() { closed <- client.Close() }()
				select {
				case <-closed:
				case <-time.After(time.Second):
					t.Fatal("Close waits behind blocked RPC")
				}
			case mode == "deadline":
				want = context.DeadlineExceeded
			}
			requireIPCResult(t, done, want)
			client.mu.Lock()
			connected := client.conn != nil
			client.mu.Unlock()
			if connected {
				t.Fatal("failed RPC retained a poisoned scanner/connection")
			}
		})
	}
}

func TestIPCClientGateWaitRespectsCancellation(t *testing.T) {
	client, peer := pipeIPCClient(t)
	first := make(chan error, 1)
	go func() { _, err := client.call("status", nil); first <- err }()
	receiveIPCRequest(t, peer)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	second := make(chan error, 1)
	go func() { _, err := client.callContext(ctx, "ping", nil); second <- err }()
	requireIPCResult(t, second, context.DeadlineExceeded)
	_ = client.Close()
	requireIPCResult(t, first, nil)
}

func TestIPCClientRejectsUnboundOrMalformedResponses(t *testing.T) {
	for _, response := range []string{
		`{"id":999,"result":{"pong":true}}`,
		`{"result":{"pong":true}}`,
		`{"id":1}`,
		`{"id":1,"result":true,"error":{"code":-1,"message":"both"}}`,
		`{"event":{"type":"request_approved"}}`,
		`not JSON`,
	} {
		t.Run(response, func(t *testing.T) {
			client, peer := pipeIPCClient(t)
			done := make(chan error, 1)
			go func() { _, err := client.call("ping", nil); done <- err }()
			receiveIPCRequest(t, peer)
			_, _ = fmt.Fprintln(peer, response)
			requireIPCResult(t, done, nil)
			if _, err := client.call("ping", nil); err == nil || !strings.Contains(err.Error(), "not connected") {
				t.Fatalf("malformed stream was reused: %v", err)
			}
		})
	}
}

func TestIPCClientMarshalFailureDoesNotConsumeConnection(t *testing.T) {
	client, peer := pipeIPCClient(t)
	if _, err := client.call("notify", make(chan int)); err == nil {
		t.Fatal("invalid params accepted")
	}
	done := make(chan error, 1)
	go func() { done <- client.Ping(context.Background()) }()
	req := receiveIPCRequest(t, peer)
	if req.Method != "ping" {
		t.Fatalf("invalid params were written: %+v", req)
	}
	_ = json.NewEncoder(peer).Encode(RPCResponse{ID: req.ID, Result: map[string]bool{"pong": true}})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func pipeSubscription(t *testing.T) (*IPCClient, net.Conn, <-chan Event, context.CancelFunc) {
	t.Helper()
	client, peer := pipeIPCClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	type outcome struct {
		events <-chan Event
		err    error
	}
	done := make(chan outcome, 1)
	go func() { events, err := client.Subscribe(ctx); done <- outcome{events, err} }()
	req := receiveIPCRequest(t, peer)
	_ = json.NewEncoder(peer).Encode(RPCResponse{ID: req.ID, Result: SubscriptionInfo{Subscribed: true, SubscriptionID: 1}})
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	return client, peer, result.events, cancel
}

func TestIPCClientSubscriptionHandshakeIsCancellable(t *testing.T) {
	client, peer := pipeIPCClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := client.Subscribe(ctx); done <- err }()
	receiveIPCRequest(t, peer)
	cancel()
	requireIPCResult(t, done, context.Canceled)
}

func TestIPCClientValidatesSubscriptionConfirmation(t *testing.T) {
	for _, result := range []any{nil, map[string]any{"subscribed": false, "subscription_id": 1}, map[string]any{"subscribed": true}, "ok"} {
		t.Run(fmt.Sprint(result), func(t *testing.T) {
			client, peer := pipeIPCClient(t)
			done := make(chan error, 1)
			go func() { _, err := client.Subscribe(context.Background()); done <- err }()
			req := receiveIPCRequest(t, peer)
			_ = json.NewEncoder(peer).Encode(map[string]any{"id": req.ID, "result": result})
			requireIPCResult(t, done, nil)
		})
	}
}

func TestIPCClientSubscriptionCancellationAndProtocolLoss(t *testing.T) {
	for _, mode := range []string{"cancel-idle", "close-idle", "peer-eof", "malformed", "missing-event", "empty-type", "close-full-channel"} {
		t.Run(mode, func(t *testing.T) {
			client, peer, events, cancel := pipeSubscription(t)
			switch mode {
			case "cancel-idle":
				cancel()
			case "close-idle":
				_ = client.Close()
			case "peer-eof":
				_ = peer.Close()
			case "malformed":
				_, _ = io.WriteString(peer, "bad-json\n")
			case "missing-event":
				_, _ = io.WriteString(peer, "{\"result\":true}\n")
			case "empty-type":
				_, _ = io.WriteString(peer, "{\"event\":{\"type\":\"\"}}\n")
			case "close-full-channel":
				written := make(chan struct{})
				go func() {
					defer close(written)
					for i := 0; i < 101; i++ {
						if err := json.NewEncoder(peer).Encode(map[string]any{"event": Event{Type: "request_pending", Payload: i}}); err != nil {
							return
						}
					}
				}()
				select {
				case <-written:
				case <-time.After(time.Second):
					t.Fatal("did not fill client event channel")
				}
				_ = client.Close()
			}
			requireIPCStreamClosed(t, events)
		})
	}
}

func TestIPCClientSubscriptionOwnsScanner(t *testing.T) {
	client, peer, events, cancel := pipeSubscription(t)
	defer cancel()
	if _, err := client.Subscribe(context.Background()); !errors.Is(err, ErrIPCSubscribed) {
		t.Fatalf("duplicate stream: %v", err)
	}
	if err := client.Ping(context.Background()); !errors.Is(err, ErrIPCSubscribed) {
		t.Fatalf("RPC stole subscription scanner: %v", err)
	}
	_ = json.NewEncoder(peer).Encode(map[string]any{"event": Event{Type: "request_approved", Payload: map[string]string{"request_id": "real-id"}}})
	select {
	case event, ok := <-events:
		if !ok || event.Type != "request_approved" {
			t.Fatalf("stream damaged by rejected RPC: %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("event missing")
	}
}

func TestIPCClientOldGenerationCannotCloseReconnect(t *testing.T) {
	client, _, events, _ := pipeSubscription(t)
	client.mu.Lock()
	old := ipcClientConnection{client.conn, client.scanner, client.connDone, client.generation}
	client.mu.Unlock()
	_ = client.Close()
	fresh, peer := net.Pipe()
	defer peer.Close()
	client.mu.Lock()
	client.conn, client.scanner, client.connDone = fresh, bufio.NewScanner(fresh), make(chan struct{})
	client.mu.Unlock()
	client.retire(old)
	requireIPCStreamClosed(t, events)
	done := make(chan error, 1)
	go func() { done <- client.Ping(context.Background()) }()
	req := receiveIPCRequest(t, peer)
	_ = json.NewEncoder(peer).Encode(RPCResponse{ID: req.ID, Result: map[string]bool{"pong": true}})
	if err := <-done; err != nil {
		t.Fatalf("old reader closed new connection: %v", err)
	}
}

func TestIPCClientExplicitTCPNeverFallsBackToLocal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix local fallback test")
	}
	path := filepath.Join(t.TempDir(), "local.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("SLB_HOST", "127.0.0.1:0")
	client := NewIPCClient(path)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Connect(ctx); err == nil {
		t.Fatal("explicit remote target silently fell back to local")
	}
}

func TestIPCClientRealServerSnapshotStreamAndReconnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newIPCServer(ln, ln.Addr().String(), log.New(io.Discard), nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer srv.Stop()
	go func() { _ = srv.Start(ctx) }()
	// Plain test listener has no TCP auth guard. Connect on a raw socket to
	// exercise production RPC framing without pretending to test TCP auth.
	client := NewIPCClient("unused")
	defer client.Close()
	connect := func() {
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		client.mu.Lock()
		client.conn, client.scanner, client.connDone = conn, bufio.NewScanner(conn), make(chan struct{})
		client.mu.Unlock()
	}
	connect()
	if err := client.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	events, err := client.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	srv.BroadcastEvent("request_pending", map[string]string{"request_id": "one"})
	select {
	case event := <-events:
		if event.Type != "request_pending" {
			t.Fatal(event)
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
	_ = client.Close()
	requireIPCStreamClosed(t, events)
	connect()
	if _, err := client.Status(ctx); err != nil {
		t.Fatal(err)
	}
}
