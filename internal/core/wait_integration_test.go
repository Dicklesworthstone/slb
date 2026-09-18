package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestWaitForDecisionWithoutDaemon(t *testing.T) {
	database, request := pendingFixture(t)
	got, err := WaitForDecision(context.Background(), database, request.ID, WaitOptions{Timeout: time.Second})
	if err != nil || got.Status != db.StatusApproved || got.ApprovalExpiresAt == nil {
		t.Fatalf("daemonless CAUTION did not resolve: %+v %v", got, err)
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil || stored.Status != db.StatusApproved || stored.ResolvedAt == nil || stored.ApprovalExpiresAt == nil {
		t.Fatalf("decision not persisted: %+v %v", stored, err)
	}
	var reviews int
	if err := database.QueryRow(`SELECT COUNT(*) FROM reviews WHERE request_id = ?`, request.ID).Scan(&reviews); err != nil || reviews != 0 {
		t.Fatalf("wait manufactured reviews: %d %v", reviews, err)
	}
	again, err := WaitForDecision(context.Background(), database, request.ID, WaitOptions{Timeout: time.Second})
	if err != nil || !again.ApprovalExpiresAt.Equal(*stored.ApprovalExpiresAt) {
		t.Fatalf("waiting again renewed approval: %+v %v", again, err)
	}
}

func TestWaitForDecisionHonorsExplicitPolicyAndLocalTimeout(t *testing.T) {
	database, request := pendingFixture(t)
	configPath := filepath.Join(t.TempDir(), "explicit.toml")
	if err := os.WriteFile(configPath, []byte("[patterns.caution]\nauto_approve_delay_seconds = 3600\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := WaitForDecision(context.Background(), database, request.ID, WaitOptions{
		ConfigPath: configPath, Timeout: 50 * time.Millisecond, PollInterval: time.Hour,
	})
	if !errors.Is(err, context.DeadlineExceeded) || got == nil || got.Status != db.StatusPending {
		t.Fatalf("explicit policy or wait deadline ignored: %+v %v", got, err)
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil || stored.Status != db.StatusPending || stored.ResolvedAt != nil || stored.ApprovalExpiresAt != nil {
		t.Fatalf("local deadline changed shared request: %+v %v", stored, err)
	}
}

func TestWaitForDecisionReturnsExpiryAndCompetingDecisions(t *testing.T) {
	for _, status := range []db.RequestStatus{db.StatusApproved, db.StatusTimeout, db.StatusEscalated, db.StatusExecuting, db.StatusRejected} {
		t.Run(string(status), func(t *testing.T) {
			database, request := pendingFixture(t)
			pendingSQL(t, database, `UPDATE requests SET status = ? WHERE id = ?`, string(status), request.ID)
			got, err := WaitForDecision(context.Background(), database, request.ID, WaitOptions{Timeout: time.Second})
			if err != nil || got.Status != status {
				t.Fatalf("existing decision not returned: %+v %v", got, err)
			}
		})
	}
	database, request := pendingFixture(t)
	pendingSQL(t, database, `UPDATE requests SET expires_at = ?, min_approvals = 1 WHERE id = ?`,
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), request.ID)
	pendingConfig(t, request, "[general]\ntimeout_action = 'escalate'\n")
	got, err := WaitForDecision(context.Background(), database, request.ID, WaitOptions{Timeout: time.Second})
	if err != nil || got.Status != db.StatusEscalated {
		t.Fatalf("daemonless expiry did not escalate: %+v %v", got, err)
	}
}

func TestWaitForDecisionFailsClosedOnBrokenPolicy(t *testing.T) {
	database, request := pendingFixture(t)
	pendingConfig(t, request, "invalid TOML")
	if _, err := WaitForDecision(context.Background(), database, request.ID, WaitOptions{Timeout: time.Second}); err == nil {
		t.Fatal("broken policy allowed a decision")
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil || stored.Status != db.StatusPending {
		t.Fatalf("failed policy load changed request: %+v %v", stored, err)
	}
}
