package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func deliveryServer(t *testing.T) (*IPCServer, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := newIPCServer(listener, listener.Addr().String(), nil, nil, nil)
	done := make(chan error, 1)
	go func() { done <- server.Start(context.Background()) }()
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Error(err)
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("accept loop leaked")
		}
	})
	return server, listener.Addr().String()
}

func deliveryWait(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("connection/subscription did not terminate")
		}
		time.Sleep(time.Millisecond)
	}
}

func deliveryDial(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return conn
}

func deliverySubscribe(t *testing.T, conn net.Conn) *bufio.Reader {
	t.Helper()
	if _, err := io.WriteString(conn, "{\"id\":1,\"method\":\"subscribe\"}\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var reply RPCResponse
	if err := json.Unmarshal(line, &reply); err != nil || reply.Error != nil {
		t.Fatalf("subscribe failed: %s, %v", line, err)
	}
	return reader
}

func TestIPCDisconnectRemovesIdleSubscription(t *testing.T) {
	server, addr := deliveryServer(t)
	conn := deliveryDial(t, addr)
	deliverySubscribe(t, conn)
	_ = conn.Close()
	deliveryWait(t, func() bool {
		server.subscribersMu.RLock()
		defer server.subscribersMu.RUnlock()
		return len(server.subscribers) == 0 && server.activeConns.Load() == 0
	})
}

func TestIPCOverflowDisconnectsInsteadOfDroppingDecisions(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := newIPCServer(listener, "test", nil, nil, nil)
	defer server.Stop()
	slow, slowPeer := net.Pipe()
	defer slowPeer.Close()
	fast, fastPeer := net.Pipe()
	defer fastPeer.Close()
	slowSub := &subscriber{id: 1, conn: slow, events: make(chan Event, 1), done: make(chan struct{})}
	fastSub := &subscriber{id: 2, conn: fast, events: make(chan Event, 2), done: make(chan struct{})}
	server.subscribers[1], server.subscribers[2] = slowSub, fastSub
	slowSub.events <- Event{Type: "old"}
	server.BroadcastEvent("request_approved", map[string]string{"request_id": "r"})
	select {
	case <-slowSub.done:
	default:
		t.Fatal("overflow was silently dropped")
	}
	if _, err := slowPeer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("slow peer did not receive EOF: %v", err)
	}
	if event := <-fastSub.events; event.Type != "request_approved" {
		t.Fatalf("healthy subscriber lost decision: %+v", event)
	}
	if len(server.subscribers) != 1 {
		t.Fatal("slow subscription remained registered")
	}
	// Repeated removal/shutdown must not close the same channel twice.
	server.removeSubscriber(1)
	server.removeSubscriber(1)
}

func TestIPCStopClosesIdleReadersAndJoinsStreams(t *testing.T) {
	server, addr := deliveryServer(t)
	conn := deliveryDial(t, addr)
	deliverySubscribe(t, conn)
	idle := deliveryDial(t, addr)
	if _, err := io.WriteString(idle, "{\"method\":"); err != nil {
		t.Fatal(err)
	}
	deliveryWait(t, func() bool { return server.activeConns.Load() == 2 })
	started := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = server.Stop() }()
	}
	wg.Wait()
	if time.Since(started) > time.Second || server.activeConns.Load() != 0 {
		t.Fatalf("shutdown left blocked readers: elapsed=%v connections=%d", time.Since(started), server.activeConns.Load())
	}
}

func TestIPCResponsesAndEventsKeepCompleteFrames(t *testing.T) {
	server, addr := deliveryServer(t)
	conn := deliveryDial(t, addr)
	reader := deliverySubscribe(t, conn)
	const count = 30
	writerDone := make(chan error, 1)
	go func() {
		for i := 0; i < count; i++ {
			if err := json.NewEncoder(conn).Encode(RPCRequest{ID: int64(i + 10), Method: "ping"}); err != nil {
				writerDone <- err
				return
			}
		}
		writerDone <- nil
	}()
	for i := 0; i < count; i++ {
		server.BroadcastEvent("request_pending", map[string]int{"index": i})
	}
	replies, events := 0, 0
	for i := 0; i < 2*count; i++ {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var frame map[string]json.RawMessage
		if err := json.Unmarshal(line, &frame); err != nil {
			t.Fatalf("interleaved frame: %q: %v", line, err)
		}
		if frame["event"] != nil {
			events++
		} else if frame["result"] != nil {
			replies++
		} else {
			t.Fatalf("unexpected frame: %q", line)
		}
	}
	if err := <-writerDone; err != nil || replies != count || events != count {
		t.Fatalf("replies=%d events=%d write=%v", replies, events, err)
	}
}

func TestIPCDuplicateSubscribeRejected(t *testing.T) {
	server, addr := deliveryServer(t)
	conn := deliveryDial(t, addr)
	reader := deliverySubscribe(t, conn)
	if _, err := io.WriteString(conn, "{\"id\":2,\"method\":\"subscribe\"}\n"); err != nil {
		t.Fatal(err)
	}
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var resp RPCResponse
	if err := json.Unmarshal(line, &resp); err != nil || resp.Error == nil || resp.Error.Code != ErrCodeInvalidReq {
		t.Fatalf("duplicate subscription accepted: %s %v", line, err)
	}
	server.subscribersMu.RLock()
	defer server.subscribersMu.RUnlock()
	if len(server.subscribers) != 1 {
		t.Fatal("duplicate created another stream")
	}
}

func TestIPCStopBeforeStartAndRepeatedCleanup(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var cleanups atomic.Int32
	server := newIPCServer(listener, "test", nil, func() error { cleanups.Add(1); return nil }, nil)
	if err := server.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := server.Start(context.Background()); err == nil {
		t.Fatal("stopped server restarted")
	}
	if err := server.Stop(); err != nil || cleanups.Load() != 1 {
		t.Fatalf("cleanup repeated: count=%d err=%v", cleanups.Load(), err)
	}
}

func TestIPCUnencodableEventDisconnects(t *testing.T) {
	server, addr := deliveryServer(t)
	conn := deliveryDial(t, addr)
	reader := deliverySubscribe(t, conn)
	server.BroadcastEvent("request_pending", make(chan int))
	if _, err := reader.ReadBytes('\n'); !errors.Is(err, io.EOF) {
		t.Fatalf("invalid event silently skipped: %v", err)
	}
}
