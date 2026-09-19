package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func stateFixture(status db.RequestStatus) *db.ProjectWatchState {
	return &db.ProjectWatchState{ActiveSessions: 3, Requests: []db.WatchRequestState{{
		ID: "request-one", ProjectPath: "/project", Command: "password=secret-value", Requestor: "agent",
		CommandHash: "hash", Status: status, RiskTier: db.RiskTierDangerous, MinApprovals: 1,
		CreatedAt: "2026-01-01T00:00:00Z",
	}}}
}

func stateReadEvent(t *testing.T, reader *bufio.Reader) Event {
	t.Helper()
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var frame struct {
		Event Event `json:"event"`
	}
	if err := json.Unmarshal(line, &frame); err != nil {
		t.Fatal(err)
	}
	return frame.Event
}

func TestRequestStateLateSubscribeAndUpdates(t *testing.T) {
	server, addr := deliveryServer(t)
	server.publishRequestState(stateFixture(db.StatusPending))
	conn := deliveryDial(t, addr)
	reader := deliverySubscribe(t, conn)
	event := stateReadEvent(t, reader)
	payload := event.Payload.(map[string]any)
	if event.Type != "request_pending" || payload["request_id"] != "request-one" || strings.Contains(payload["command"].(string), "secret-value") {
		t.Fatalf("missing/unsafe bootstrap: %+v", event)
	}
	server.publishRequestState(stateFixture(db.StatusPending)) // No duplicate.
	approved := stateFixture(db.StatusApproved)
	approved.Requests[0].Approvals = 1
	server.publishRequestState(approved)
	if event := stateReadEvent(t, reader); event.Type != "request_approved" {
		t.Fatalf("unchanged snapshot emitted duplicate or lost approval: %+v", event)
	}
	status := server.handleStatus(RPCRequest{}).Result.(map[string]any)
	if status["pending_count"] != int32(0) || status["active_sessions"] != 3 || status["state_ready"] != true {
		t.Fatalf("status not wired to persisted state: %+v", status)
	}
}

func TestRequestStateSnapshotHandoffHasNoGap(t *testing.T) {
	server, addr := deliveryServer(t)
	for i := 0; i < 30; i++ {
		server.publishRequestState(stateFixture(db.StatusPending))
		conn := deliveryDial(t, addr)
		done := make(chan struct{})
		go func() { server.publishRequestState(stateFixture(db.StatusApproved)); close(done) }()
		reader := deliverySubscribe(t, conn)
		// Either a new bootstrap or old bootstrap followed by the update.
		first := stateReadEvent(t, reader)
		if first.Type != "request_approved" {
			if first.Type != "request_pending" || stateReadEvent(t, reader).Type != "request_approved" {
				t.Fatal("snapshot-registration race lost/reordered approval")
			}
		}
		<-done
		_ = conn.Close()
	}
}

func TestRequestStateLargeBootstrapDoesNotOverflowLiveQueue(t *testing.T) {
	server, addr := deliveryServer(t)
	state := &db.ProjectWatchState{}
	for i := 0; i < 150; i++ {
		state.Requests = append(state.Requests, db.WatchRequestState{ID: fmt.Sprint(i), Status: db.StatusPending})
	}
	server.publishRequestState(state)
	conn := deliveryDial(t, addr)
	reader := deliverySubscribe(t, conn)
	ids := make(map[string]bool)
	for i := 0; i < 150; i++ {
		event := stateReadEvent(t, reader)
		ids[event.Payload.(map[string]any)["request_id"].(string)] = true
	}
	if len(ids) != 150 {
		t.Fatal("bootstrap dropped requests")
	}
}

func TestRequestStateFailureDisconnectsAndRecovers(t *testing.T) {
	server, addr := deliveryServer(t)
	server.publishRequestState(stateFixture(db.StatusPending))
	conn := deliveryDial(t, addr)
	reader := deliverySubscribe(t, conn)
	stateReadEvent(t, reader)
	server.invalidateRequestState(errors.New("database unavailable"))
	if _, err := reader.ReadBytes('\n'); !errors.Is(err, io.EOF) {
		t.Fatalf("stale stream still looks healthy: %v", err)
	}
	status := server.handleStatus(RPCRequest{}).Result.(map[string]any)
	if status["state_ready"] != false || status["state_error"] != "database unavailable" {
		t.Fatalf("stale status looks healthy: %+v", status)
	}
	conn2 := deliveryDial(t, addr)
	if _, err := io.WriteString(conn2, "{\"id\":2,\"method\":\"subscribe\"}\n"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn2).ReadBytes('\n')
	if err != nil || !strings.Contains(string(line), "request state unavailable") {
		t.Fatalf("failed state accepted new stream: %s %v", line, err)
	}
	server.publishRequestState(stateFixture(db.StatusApproved))
	conn3 := deliveryDial(t, addr)
	if event := stateReadEvent(t, deliverySubscribe(t, conn3)); event.Type != "request_approved" {
		t.Fatalf("recovery did not refresh snapshot: %+v", event)
	}
}

func TestRequestStatePartialReviewsAndSessions(t *testing.T) {
	server, addr := deliveryServer(t)
	server.publishRequestState(stateFixture(db.StatusPending))
	conn := deliveryDial(t, addr)
	reader := deliverySubscribe(t, conn)
	stateReadEvent(t, reader)
	changed := stateFixture(db.StatusPending)
	changed.Requests[0].Approvals = 1
	changed.ActiveSessions = 4
	server.publishRequestState(changed)
	if event := stateReadEvent(t, reader); event.Type != "request_pending" || event.Payload.(map[string]any)["approvals"] != float64(1) {
		t.Fatalf("partial review disappeared: %+v", event)
	}
	if event := stateReadEvent(t, reader); event.Type != "sessions_changed" {
		t.Fatalf("session update disappeared: %+v", event)
	}
}

func TestRequestStateMonitorCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runRequestStateMonitor(ctx, nil, "/project", nil, nil, nil)
}

func TestRequestStateEventOutcomes(t *testing.T) {
	for _, status := range []db.RequestStatus{db.StatusExecuting, db.StatusExecuted, db.StatusExecutionFailed, db.StatusTimedOut, db.StatusEscalated, db.StatusRejected, db.StatusCancelled} {
		state := stateFixture(status)
		code := 7
		state.Requests[0].ExitCode = &code
		events, pending := requestStateEvents(state)
		if pending != 0 || len(events) != 1 || events[0].Type != "request_"+string(status) || events[0].Payload.(map[string]any)["exit_code"] != 7 {
			t.Fatalf("incorrect outcome event for %s: %+v", status, events)
		}
	}
}
