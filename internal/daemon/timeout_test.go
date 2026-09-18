package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func expiredTimerFixture(t *testing.T) (*db.DB, *db.Request) {
	t.Helper()
	database, request := pendingTimerFixture(t)
	expiredAt := time.Now().UTC().Add(-time.Hour)
	if _, err := database.Exec(`UPDATE requests SET expires_at = ? WHERE id = ?`, expiredAt.Format(time.RFC3339), request.ID); err != nil {
		t.Fatal(err)
	}
	request.ExpiresAt = &expiredAt
	return database, request
}

func TestTimeoutHandler_HandleExpiredRequest_Escalate(t *testing.T) {
	database, request := expiredTimerFixture(t)
	handler := NewTimeoutHandler(database, TimeoutHandlerConfig{Action: TimeoutActionEscalate})
	if err := handler.HandleExpiredRequest(request); err != nil {
		t.Fatal(err)
	}
	updated, err := database.GetRequest(request.ID)
	if err != nil || updated.Status != db.StatusEscalated {
		t.Fatalf("expected ESCALATED, got %+v (%v)", updated, err)
	}
}

func TestTimeoutHandler_HandleExpiredRequest_AutoReject(t *testing.T) {
	database, request := expiredTimerFixture(t)
	handler := NewTimeoutHandler(database, TimeoutHandlerConfig{Action: TimeoutActionAutoReject})
	if err := handler.HandleExpiredRequest(request); err != nil {
		t.Fatal(err)
	}
	updated, err := database.GetRequest(request.ID)
	if err != nil || updated.Status != db.StatusTimeout {
		t.Fatalf("expected terminal TIMEOUT, got %+v (%v)", updated, err)
	}
}

func TestTimeoutHandler_HandleExpiredRequest_AutoApproveWarn_CautionTier(t *testing.T) {
	// Auto-approval must have an elapsed delay, a currently recognized CAUTION
	// command, zero required reviews and a live requester. Merely editing the
	// stored risk_tier of an opaque command must not grant permission.
	database, request := expiredTimerFixture(t)
	handler := NewTimeoutHandler(database, TimeoutHandlerConfig{Action: TimeoutActionAutoApproveWarn})
	if err := handler.HandleExpiredRequest(request); err != nil {
		t.Fatal(err)
	}
	updated, err := database.GetRequest(request.ID)
	if err != nil || updated.Status != db.StatusApproved || updated.ApprovalExpiresAt == nil {
		t.Fatalf("expected time-bounded APPROVED, got %+v (%v)", updated, err)
	}
}

func TestTimeoutHandler_HandleExpiredRequest_AutoApproveWarn_DangerousTier_Escalates(t *testing.T) {
	database, request := expiredTimerFixture(t)
	if _, err := database.Exec(`UPDATE requests SET risk_tier = 'dangerous', min_approvals = 1 WHERE id = ?`, request.ID); err != nil {
		t.Fatal(err)
	}
	// Pass the stale CAUTION snapshot deliberately. The database's current
	// risk/quorum must win over whatever a scan or caller previously observed.
	handler := NewTimeoutHandler(database, TimeoutHandlerConfig{Action: TimeoutActionAutoApproveWarn})
	if err := handler.HandleExpiredRequest(request); err != nil {
		t.Fatal(err)
	}
	updated, err := database.GetRequest(request.ID)
	if err != nil || updated.Status != db.StatusEscalated {
		t.Fatalf("high-risk request was not escalated: %+v (%v)", updated, err)
	}
}

func TestTimeoutHandler_StartStop(t *testing.T) {
	database, _ := pendingTimerFixture(t)
	handler := NewTimeoutHandler(database, TimeoutHandlerConfig{CheckInterval: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := handler.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if !handler.IsRunning() {
		t.Fatal("handler is not running")
	}
	if err := handler.Start(ctx); err == nil {
		t.Fatal("starting an already running handler succeeded")
	}
	handler.Stop()
	if handler.IsRunning() {
		t.Fatal("handler remained running after Stop")
	}
	handler.Stop() // Idempotent shutdown.
}

func TestTimeoutHandler_ChecksExpiredRequests(t *testing.T) {
	database, request := expiredTimerFixture(t)
	handler := NewTimeoutHandler(database, TimeoutHandlerConfig{CheckInterval: 10 * time.Millisecond, Action: TimeoutActionEscalate})
	if err := handler.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer handler.Stop()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		updated, err := database.GetRequest(request.ID)
		if err != nil {
			t.Fatal(err)
		}
		if updated.Status == db.StatusEscalated {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expired request was not escalated")
}

func TestCheckExpiredRequests_FindsExpired(t *testing.T) {
	database, request := expiredTimerFixture(t)
	active := &db.Request{ProjectPath: request.ProjectPath, Command: request.Command, RiskTier: db.RiskTierDangerous,
		RequestorSessionID: request.RequestorSessionID, RequestorAgent: request.RequestorAgent,
		RequestorModel: request.RequestorModel, MinApprovals: 1, Justification: db.Justification{Reason: "not expired"}}
	if err := database.CreateRequest(active); err != nil {
		t.Fatal(err)
	}
	expired, err := CheckExpiredRequests(database)
	if err != nil || len(expired) != 1 || expired[0].ID != request.ID {
		t.Fatalf("incorrect expired requests: %+v %v", expired, err)
	}
}

func TestTruncateString(t *testing.T) {
	for _, tc := range []struct {
		input string
		max   int
		want  string
	}{
		{"hello", 10, "hello"}, {"hello world", 8, "hello..."},
		{"hi", 2, "hi"}, {"hello", 3, "hel"}, {"hello", 5, "hello"}, {"", 5, ""},
	} {
		if got := truncateString(tc.input, tc.max); got != tc.want {
			t.Errorf("truncateString(%q, %d) = %q, want %q", tc.input, tc.max, got, tc.want)
		}
	}
}

func TestEscapePowerShellDoubleQuoted(t *testing.T) {
	out := escapePowerShellDoubleQuoted("a`b\"c$d\re\nf")
	if strings.ContainsAny(out, "\r\n") {
		t.Fatalf("raw newlines after escaping: %q", out)
	}
	for i := 0; i < len(out); i++ {
		if (out[i] == '"' || out[i] == '$') && (i == 0 || out[i-1] != '`') {
			t.Fatalf("unescaped character at %d: %q", i, out)
		}
	}
}

func TestTimeoutHandler_WithNotifications(t *testing.T) {
	database, request := expiredTimerFixture(t)
	handler := NewTimeoutHandler(database, TimeoutHandlerConfig{Action: TimeoutActionEscalate, DesktopNotify: true})
	if err := handler.HandleExpiredRequest(request); err != nil {
		t.Fatalf("notification failure must not undo committed decision: %v", err)
	}
	updated, err := database.GetRequest(request.ID)
	if err != nil || updated.Status != db.StatusEscalated {
		t.Fatalf("notification prevented durable escalation: %+v %v", updated, err)
	}
}
