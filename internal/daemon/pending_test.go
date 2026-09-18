package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func pendingTimerFixture(t *testing.T) (*db.DB, *db.Request) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	project := t.TempDir()
	database, err := db.OpenAndMigrate(filepath.Join(project, ".slb", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	session := &db.Session{AgentName: "timer-requester", Model: "test", Program: "test", ProjectPath: project}
	if err := database.CreateSession(session); err != nil {
		t.Fatal(err)
	}
	request := &db.Request{ProjectPath: project, Command: db.CommandSpec{Raw: "git branch -d obsolete", Cwd: project, Shell: true},
		Status: db.StatusPending, RiskTier: db.RiskTierCaution, RequestorSessionID: session.ID,
		RequestorAgent: session.AgentName, RequestorModel: session.Model, Justification: db.Justification{Reason: "timer integration"}}
	if err := database.CreateRequest(request); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE requests SET created_at = ? WHERE id = ?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), request.ID); err != nil {
		t.Fatal(err)
	}
	return database, request
}

func TestPendingTimerApprovesDueCautionWithoutExpiry(t *testing.T) {
	database, request := pendingTimerFixture(t)
	handler := NewTimeoutHandler(database, TimeoutHandlerConfig{CheckInterval: time.Millisecond, Action: TimeoutActionEscalate})
	handler.checkAndHandleExpired()
	stored, err := database.GetRequest(request.ID)
	if err != nil || stored.Status != db.StatusApproved || stored.ApprovalExpiresAt == nil {
		t.Fatalf("daemon did not process unexpired delayed CAUTION: %+v %v", stored, err)
	}
	var reviews int
	if err := database.QueryRow(`SELECT COUNT(*) FROM reviews WHERE request_id = ?`, request.ID).Scan(&reviews); err != nil || reviews != 0 {
		t.Fatalf("daemon inserted fake reviews: count=%d err=%v", reviews, err)
	}
}

func TestPendingTimerIgnoresStaleExpiredSnapshots(t *testing.T) {
	for _, status := range []db.RequestStatus{db.StatusRejected, db.StatusApproved, db.StatusExecuting, db.StatusCancelled} {
		t.Run(string(status), func(t *testing.T) {
			database, request := pendingTimerFixture(t)
			if _, err := database.Exec(`UPDATE requests SET status = ? WHERE id = ?`, string(status), request.ID); err != nil {
				t.Fatal(err)
			}
			handler := NewTimeoutHandler(database, TimeoutHandlerConfig{Action: TimeoutActionEscalate})
			if err := handler.HandleExpiredRequest(request); err != nil {
				t.Fatal(err)
			}
			stored, err := database.GetRequest(request.ID)
			if err != nil || stored.Status != status {
				t.Fatalf("stale scan rewrote live state: %+v %v", stored, err)
			}
		})
	}
}

func TestPendingTimerExpiredCautionCannotWaiveQuorum(t *testing.T) {
	database, request := pendingTimerFixture(t)
	if _, err := database.Exec(`UPDATE requests SET min_approvals = 1, expires_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), request.ID); err != nil {
		t.Fatal(err)
	}
	handler := NewTimeoutHandler(database, TimeoutHandlerConfig{Action: TimeoutActionAutoApproveWarn})
	if err := handler.HandleExpiredRequest(request); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil || stored.Status != db.StatusEscalated || stored.ApprovalExpiresAt != nil {
		t.Fatalf("timeout waived review quorum: %+v %v", stored, err)
	}
}

func TestPendingTimerStopJoinsAndCanRestart(t *testing.T) {
	database, request := pendingTimerFixture(t)
	// Keep the request pending throughout restarts without invoking a shell.
	if err := os.WriteFile(filepath.Join(request.ProjectPath, ".slb", "config.toml"),
		[]byte("[patterns.caution]\nauto_approve_delay_seconds = 600\n"), 0600); err != nil {
		t.Fatal(err)
	}
	handler := NewTimeoutHandler(database, TimeoutHandlerConfig{CheckInterval: -1})
	for i := 0; i < 10; i++ {
		if err := handler.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		handler.Stop()
		if handler.IsRunning() {
			t.Fatal("Stop returned before the timer finished")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := handler.Start(ctx); err == nil || handler.IsRunning() {
		t.Fatal("cancelled context started a timer generation")
	}
}
