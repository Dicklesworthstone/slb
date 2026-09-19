package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func pendingFixture(t *testing.T) (*db.DB, *db.Request) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	project := t.TempDir()
	database, err := db.OpenAndMigrate(filepath.Join(project, ".slb", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	session := &db.Session{AgentName: "pending-requester", Model: "test-model", Program: "test", ProjectPath: project}
	if err := database.CreateSession(session); err != nil {
		t.Fatal(err)
	}
	request := &db.Request{
		ProjectPath: project, Command: db.CommandSpec{Raw: "git branch -d obsolete", Cwd: project, Shell: true},
		RiskTier: db.RiskTierCaution, Status: db.StatusPending,
		RequestorSessionID: session.ID, RequestorAgent: session.AgentName, RequestorModel: session.Model,
		Justification: db.Justification{Reason: "test delayed policy decision"},
	}
	if err := database.CreateRequest(request); err != nil {
		t.Fatal(err)
	}
	pendingSQL(t, database, `UPDATE requests SET created_at = ? WHERE id = ?`, time.Now().UTC().Add(-2*time.Minute).Format(time.RFC3339), request.ID)
	return database, request
}

func pendingSQL(t *testing.T, database *db.DB, query string, args ...any) {
	t.Helper()
	if _, err := database.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func pendingConfig(t *testing.T, request *db.Request, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(request.ProjectPath, ".slb", "config.toml"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestPendingDelayAndApprovalDeadline(t *testing.T) {
	database, request := pendingFixture(t)
	pendingConfig(t, request, "[patterns.caution]\nauto_approve_delay_seconds = 600\n")
	before, err := AdvancePendingRequest(context.Background(), database, request.ID, PendingOptions{})
	if err != nil || before == nil || before.Changed || before.Request.Status != db.StatusPending {
		t.Fatalf("approved before the configured delay: %+v %v", before, err)
	}
	pendingConfig(t, request, "[general]\napproval_ttl_minutes = 7\n[patterns.caution]\nauto_approve_delay_seconds = 30\n")
	now := time.Now().UTC()
	result, err := AdvancePendingRequest(context.Background(), database, request.ID, PendingOptions{})
	if err != nil || result == nil || !result.Changed || result.Request.Status != db.StatusApproved {
		t.Fatalf("due CAUTION did not approve: %+v %v", result, err)
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ApprovalExpiresAt == nil || stored.ApprovalExpiresAt.Before(now.Add(7*time.Minute-time.Second)) ||
		stored.ApprovalExpiresAt.After(time.Now().Add(7*time.Minute+time.Second)) || stored.ResolvedAt == nil {
		t.Fatalf("approval deadline/decision not durable: %+v", stored)
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM reviews WHERE request_id = ?`, request.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("timer manufactured review evidence: %d %v", count, err)
	}
	again, err := AdvancePendingRequest(context.Background(), database, request.ID, PendingOptions{})
	if err != nil || again.Changed || !again.Request.ApprovalExpiresAt.Equal(*stored.ApprovalExpiresAt) {
		t.Fatalf("repeated timer renewed or rewrote approval: %+v %v", again, err)
	}
}

func TestPendingNeverWaivesReviewOrIdentityConstraints(t *testing.T) {
	for _, tc := range []struct{ name, query string }{
		{"stored-quorum", `UPDATE requests SET min_approvals = 1`},
		{"stored-model", `UPDATE requests SET require_different_model = 1`},
		{"dangerous", `UPDATE requests SET risk_tier = 'dangerous'`},
		{"critical", `UPDATE requests SET risk_tier = 'critical'`},
		{"unknown-tier", `UPDATE requests SET risk_tier = 'typo'`},
		{"tampered-command", `UPDATE requests SET command_raw = 'git branch -d other'`},
		{"future-creation", `UPDATE requests SET created_at = '2099-01-01T00:00:00Z'`},
		{"invalid-creation", `UPDATE requests SET created_at = 'broken'`},
		{"ended-session", `UPDATE sessions SET ended_at = '2000-01-01T00:00:00Z'`},
		{"changed-agent", `UPDATE sessions SET agent_name = 'replacement'`},
		{"changed-model", `UPDATE sessions SET model = 'replacement'`},
		{"wrong-project", `UPDATE sessions SET project_path = '/unrelated'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, request := pendingFixture(t)
			pendingSQL(t, database, tc.query)
			result, err := AdvancePendingRequest(context.Background(), database, request.ID, PendingOptions{})
			if err != nil || result.Changed || result.Request.Status != db.StatusPending {
				t.Fatalf("timer waived %s: %+v %v", tc.name, result, err)
			}
		})
	}
	for _, decision := range []db.Decision{db.DecisionApprove, db.DecisionReject} {
		t.Run(string(decision)+"-review", func(t *testing.T) {
			database, request := pendingFixture(t)
			// Even a malformed or unverified review forces explicit resolution;
			// a timer must not hide or override an existing reviewer's decision.
			if err := database.CreateReview(&db.Review{RequestID: request.ID, ReviewerSessionID: request.RequestorSessionID,
				ReviewerAgent: request.RequestorAgent, ReviewerModel: request.RequestorModel, Decision: decision}); err != nil {
				t.Fatal(err)
			}
			result, err := AdvancePendingRequest(context.Background(), database, request.ID, PendingOptions{})
			if err != nil || result.Changed {
				t.Fatalf("timer overrode existing review: %+v %v", result, err)
			}
		})
	}
}

func TestPendingReloadsPolicyBeforeApproval(t *testing.T) {
	for _, text := range []string{
		"[patterns.caution]\nmin_approvals = 1\n",
		"[general]\nrequire_different_model = true\n",
		"[agents]\nblocked = ['pending-requester']\n",
		"[patterns.critical]\npatterns = ['^git branch -d obsolete$']\n",
	} {
		t.Run(text, func(t *testing.T) {
			database, request := pendingFixture(t)
			pendingConfig(t, request, text)
			result, err := AdvancePendingRequest(context.Background(), database, request.ID, PendingOptions{})
			if err != nil || result.Changed {
				t.Fatalf("current policy was ignored: %+v %v", result, err)
			}
		})
	}
	for _, text := range []string{"invalid TOML", "[patterns.safe]\npatterns = ['[']\n"} {
		t.Run(text, func(t *testing.T) {
			database, request := pendingFixture(t)
			pendingConfig(t, request, text)
			if _, err := AdvancePendingRequest(context.Background(), database, request.ID, PendingOptions{}); err == nil {
				t.Fatal("invalid policy accepted")
			}
			stored, err := database.GetRequest(request.ID)
			if err != nil || stored.Status != db.StatusPending || stored.ApprovalExpiresAt != nil {
				t.Fatalf("failed policy load published approval: %+v %v", stored, err)
			}
		})
	}
}

func TestPendingExpirationActionsAreAtomic(t *testing.T) {
	for _, tc := range []struct {
		action string
		quorum int
		want   db.RequestStatus
	}{
		{"escalate", 0, db.StatusEscalated},
		{"auto_reject", 0, db.StatusTimeout},
		{"auto_approve_warn", 0, db.StatusApproved},
		{"auto_approve_warn", 1, db.StatusEscalated},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.action, tc.quorum), func(t *testing.T) {
			database, request := pendingFixture(t)
			pendingSQL(t, database, `UPDATE requests SET expires_at = ?, min_approvals = ? WHERE id = ?`,
				time.Now().UTC().Add(-time.Second).Format(time.RFC3339), tc.quorum, request.ID)
			pendingConfig(t, request, fmt.Sprintf("[general]\ntimeout_action = '%s'\n", tc.action))
			result, err := AdvancePendingRequest(context.Background(), database, request.ID, PendingOptions{})
			if err != nil || !result.Changed || result.Request.Status != tc.want || result.Request.ResolvedAt == nil {
				t.Fatalf("incorrect expiry transition: %+v %v", result, err)
			}
		})
	}
}

func TestPendingDecisionFailuresRollBack(t *testing.T) {
	database, request := pendingFixture(t)
	pendingSQL(t, database, `CREATE TRIGGER reject_timer_update BEFORE UPDATE OF status ON requests
		BEGIN SELECT RAISE(ABORT, 'injected decision write failure'); END`)
	if _, err := AdvancePendingRequest(context.Background(), database, request.ID, PendingOptions{}); err == nil {
		t.Fatal("write failure was hidden")
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil || stored.Status != db.StatusPending || stored.ApprovalExpiresAt != nil || stored.ResolvedAt != nil {
		t.Fatalf("failed decision partially committed: %+v %v", stored, err)
	}
}

func TestPendingTimersAreSingleWinner(t *testing.T) {
	database, request := pendingFixture(t)
	other, err := db.OpenAndMigrate(database.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan *db.PendingResolution, 2)
	errorsCh := make(chan error, 2)
	for _, connection := range []*db.DB{database, other} {
		wg.Add(1)
		go func(d *db.DB) {
			defer wg.Done()
			<-start
			r, err := AdvancePendingRequest(context.Background(), d, request.ID, PendingOptions{})
			results <- r
			errorsCh <- err
		}(connection)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	winners := 0
	for result := range results {
		if result.Changed {
			winners++
		}
		if result.Request.Status != db.StatusApproved {
			t.Fatalf("competing timer lost durable outcome: %+v", result)
		}
	}
	if winners != 1 {
		t.Fatalf("%d timers claimed the same decision", winners)
	}
}

func TestPendingContextAndExistingDecisions(t *testing.T) {
	database, request := pendingFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := AdvancePendingRequest(ctx, database, request.ID, PendingOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
	for _, status := range []db.RequestStatus{db.StatusRejected, db.StatusCancelled, db.StatusEscalated, db.StatusExecuting, db.StatusExecuted} {
		pendingSQL(t, database, `UPDATE requests SET status = ? WHERE id = ?`, string(status), request.ID)
		result, err := AdvancePendingRequest(context.Background(), database, request.ID, PendingOptions{})
		if err != nil || result.Changed || result.Request.Status != status {
			t.Fatalf("timer rewrote existing %s: %+v %v", status, result, err)
		}
	}
}

func TestPendingEntryPointScope(t *testing.T) {
	database, request := pendingFixture(t)
	result, err := AdvancePendingRequest(context.Background(), database, request.ID, PendingOptions{ExpiredOnly: true})
	if err != nil || result.Changed {
		t.Fatalf("expiration entry point approved a nonexpired request: %+v %v", result, err)
	}
	pendingSQL(t, database, `UPDATE requests SET expires_at = ? WHERE id = ?`, time.Now().UTC().Add(-time.Second).Format(time.RFC3339), request.ID)
	result, err = AdvancePendingRequest(context.Background(), database, request.ID, PendingOptions{OnlyAutoApprove: true})
	if err != nil || result.Changed {
		t.Fatalf("watch entry point changed expired policy: %+v %v", result, err)
	}
}

func TestPendingDatabaseBoundaryRefusesConstraintWaivers(t *testing.T) {
	for _, tc := range []struct{ name, query string }{
		{"quorum", `UPDATE requests SET min_approvals = 1`},
		{"model", `UPDATE requests SET require_different_model = 1`},
		{"tier", `UPDATE requests SET risk_tier = 'critical'`},
		{"hash", `UPDATE requests SET command_hash = 'wrong'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, request := pendingFixture(t)
			pendingSQL(t, database, tc.query)
			_, err := database.ResolvePendingRequest(context.Background(), request.ID,
				func(*sql.Tx, *db.Request, time.Time) (*db.PendingDecision, error) {
					return &db.PendingDecision{Status: db.StatusApproved, ApprovalTTL: time.Minute}, nil
				})
			if !errors.Is(err, db.ErrInvalidTransition) {
				t.Fatalf("database accepted a policy waiver: %v", err)
			}
		})
	}
}

func TestPendingDifferentModelTimeoutEscalatesAtomically(t *testing.T) {
	for _, tc := range []struct {
		name            string
		createdAgo      time.Duration
		reviewerModel   string
		reviewerAlready bool
		want            db.RequestStatus
	}{
		{name: "no-reviewer", createdAgo: 2 * time.Minute, want: db.StatusEscalated},
		{name: "same-model-does-not-satisfy", createdAgo: 2 * time.Minute, reviewerModel: "test-model", want: db.StatusEscalated},
		{name: "different-model-available", createdAgo: 2 * time.Minute, reviewerModel: "other-model", want: db.StatusPending},
		{name: "different-model-already-voted", createdAgo: 2 * time.Minute, reviewerModel: "other-model", reviewerAlready: true, want: db.StatusEscalated},
		{name: "timeout-not-due", createdAgo: 10 * time.Second, want: db.StatusPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, request := pendingFixture(t)
			pendingSQL(t, database, "UPDATE requests SET risk_tier = 'dangerous', min_approvals = 2, require_different_model = 1, created_at = ?, expires_at = ? WHERE id = ?",
				time.Now().UTC().Add(-tc.createdAgo).Format(time.RFC3339),
				time.Now().UTC().Add(time.Hour).Format(time.RFC3339), request.ID)
			pendingConfig(t, request, "[general]\ndifferent_model_timeout = 30\n")
			if tc.reviewerModel != "" {
				reviewer := &db.Session{AgentName: "independent-reviewer", Model: tc.reviewerModel, Program: "test", ProjectPath: request.ProjectPath}
				if err := database.CreateSession(reviewer); err != nil {
					t.Fatal(err)
				}
				if tc.reviewerAlready {
					if err := database.CreateReview(&db.Review{
						RequestID: request.ID, ReviewerSessionID: reviewer.ID, ReviewerAgent: reviewer.AgentName,
						ReviewerModel: reviewer.Model, Decision: db.DecisionApprove,
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			result, err := AdvancePendingRequest(context.Background(), database, request.ID, PendingOptions{})
			if err != nil || result.Request.Status != tc.want || result.Changed != (tc.want == db.StatusEscalated) {
				t.Fatalf("different-model timeout result=%+v err=%v want=%s", result, err, tc.want)
			}
		})
	}
}

func TestPendingDifferentModelTimeoutDatabaseGuard(t *testing.T) {
	database, request := pendingFixture(t)
	pendingSQL(t, database, "UPDATE requests SET risk_tier = 'dangerous', min_approvals = 1, created_at = ?, expires_at = ? WHERE id = ?",
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
		time.Now().UTC().Add(time.Hour).Format(time.RFC3339), request.ID)

	_, err := database.ResolvePendingRequest(context.Background(), request.ID,
		func(*sql.Tx, *db.Request, time.Time) (*db.PendingDecision, error) {
			return &db.PendingDecision{Status: db.StatusEscalated, DifferentModelTimeout: 30 * time.Second}, nil
		})
	if !errors.Is(err, db.ErrInvalidTransition) {
		t.Fatalf("database accepted unbound early escalation: %v", err)
	}

	pendingSQL(t, database, "UPDATE requests SET require_different_model = 1 WHERE id = ?", request.ID)
	reviewer := &db.Session{AgentName: "available-reviewer", Model: "other-model", Program: "test", ProjectPath: request.ProjectPath}
	if err := database.CreateSession(reviewer); err != nil {
		t.Fatal(err)
	}
	result, err := database.ResolvePendingRequest(context.Background(), request.ID,
		func(*sql.Tx, *db.Request, time.Time) (*db.PendingDecision, error) {
			return &db.PendingDecision{Status: db.StatusEscalated, DifferentModelTimeout: 30 * time.Second}, nil
		})
	if err != nil || result.Changed || result.Request.Status != db.StatusPending {
		t.Fatalf("database ignored available different-model reviewer: %+v %v", result, err)
	}
}

func TestPendingAutoApproveOnlyDoesNotEscalateModelTimeout(t *testing.T) {
	database, request := pendingFixture(t)
	pendingSQL(t, database, "UPDATE requests SET risk_tier = 'dangerous', min_approvals = 1, require_different_model = 1, created_at = ?, expires_at = ? WHERE id = ?",
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
		time.Now().UTC().Add(time.Hour).Format(time.RFC3339), request.ID)
	pendingConfig(t, request, "[general]\ndifferent_model_timeout = 1\n")
	result, err := AdvancePendingRequest(context.Background(), database, request.ID, PendingOptions{OnlyAutoApprove: true})
	if err != nil || result.Changed || result.Request.Status != db.StatusPending {
		t.Fatalf("auto-approval-only path escalated model timeout: %+v %v", result, err)
	}
}
