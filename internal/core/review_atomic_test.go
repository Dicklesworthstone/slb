package core

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/integrations"
)

type committedReviewNotifier struct {
	integrations.NoopNotifier
	requests []*db.Request
}

func (n *committedReviewNotifier) NotifyRequestApproved(request *db.Request, _ *db.Review) error {
	copy := *request
	n.requests = append(n.requests, &copy)
	return nil
}

func atomicReviewFixture(t *testing.T, quorum int) (*db.DB, *db.Request, []*db.Session) {
	t.Helper()
	project := t.TempDir()
	database, err := db.Open(filepath.Join(project, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	var sessions []*db.Session
	for i := 0; i < 3; i++ {
		session := &db.Session{ID: fmt.Sprintf("atomic-review-%d", i), AgentName: fmt.Sprintf("AtomicReviewer%d", i),
			Model: fmt.Sprintf("model-%d", i), Program: "test", ProjectPath: project}
		if err := database.CreateSession(session); err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, session)
	}
	request := &db.Request{ID: "atomic-request", ProjectPath: project, RequestorSessionID: sessions[0].ID,
		RequestorAgent: sessions[0].AgentName, RequestorModel: sessions[0].Model,
		Command:  db.CommandSpec{Raw: "echo review-only", Argv: []string{"echo", "review-only"}, Cwd: project},
		RiskTier: db.RiskTierDangerous, Status: db.StatusPending, MinApprovals: quorum,
		Justification: db.Justification{Reason: "Atomic review regression"}}
	if err := database.CreateRequest(request); err != nil {
		t.Fatal(err)
	}
	return database, request, sessions
}

func TestReviewServiceNotifiesCommittedApprovalAndTTL(t *testing.T) {
	database, request, sessions := atomicReviewFixture(t, 2)
	cfg := DefaultReviewConfig()
	cfg.ApprovalTTL = 2 * time.Minute
	service := NewReviewService(database, cfg)
	notifier := &committedReviewNotifier{}
	service.SetNotifier(notifier)
	for i := 1; i <= 2; i++ {
		outcome, err := service.SubmitReview(ReviewOptions{RequestID: request.ID, SessionID: sessions[i].ID,
			SessionKey: sessions[i].SessionKey, Decision: db.DecisionApprove})
		if err != nil {
			t.Fatal(err)
		}
		if outcome.Approvals != i || outcome.RequestStatusChanged != (i == 2) {
			t.Fatalf("wrong review outcome: %+v", outcome)
		}
	}
	if len(notifier.requests) != 2 || notifier.requests[0].Status != db.StatusPending || notifier.requests[1].Status != db.StatusApproved {
		t.Fatalf("notifications reported stale request state: %+v", notifier.requests)
	}
	approved := notifier.requests[1]
	if approved.ApprovalExpiresAt == nil || approved.ResolvedAt == nil || approved.ApprovalExpiresAt.Sub(*approved.ResolvedAt) != cfg.ApprovalTTL {
		t.Fatalf("notification omitted committed deadline: %+v", approved)
	}
}

func TestReviewServiceWriteFailureDoesNotNotifyOrApprove(t *testing.T) {
	database, request, sessions := atomicReviewFixture(t, 1)
	_, err := database.Exec(`CREATE TRIGGER fail_review_resolution BEFORE UPDATE OF approval_expires_at ON requests BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	service := NewReviewService(database, DefaultReviewConfig())
	notifier := &committedReviewNotifier{}
	service.SetNotifier(notifier)
	result, err := service.SubmitReview(ReviewOptions{RequestID: request.ID, SessionID: sessions[1].ID,
		SessionKey: sessions[1].SessionKey, Decision: db.DecisionApprove})
	if err == nil || result != nil || len(notifier.requests) != 0 {
		t.Fatalf("failed transaction announced success: %+v %v %+v", result, err, notifier.requests)
	}
	a, r, err := database.CountReviewsByDecision(request.ID)
	if err != nil || a+r != 0 {
		t.Fatalf("failed review was persisted: %d/%d %v", a, r, err)
	}
}

func TestReviewServiceExpiryAndSessionErrors(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired=%v", expired), func(t *testing.T) {
			database, request, sessions := atomicReviewFixture(t, 1)
			query := `UPDATE sessions SET ended_at = '2000-01-01T00:00:00Z'`
			want := ErrSessionInactive
			if expired {
				query = `UPDATE requests SET expires_at = '2000-01-01T00:00:00Z'`
				want = db.ErrReviewExpired
			}
			if _, err := database.Exec(query); err != nil {
				t.Fatal(err)
			}
			service := NewReviewService(database, DefaultReviewConfig())
			_, err := service.SubmitReview(ReviewOptions{RequestID: request.ID, SessionID: sessions[1].ID, SessionKey: sessions[1].SessionKey, Decision: db.DecisionApprove})
			if !errors.Is(err, want) {
				t.Fatalf("wrong eligibility error: %v want %v", err, want)
			}
		})
	}
}

func TestReviewServiceFirstDecisionCannotBypassQuorum(t *testing.T) {
	database, request, sessions := atomicReviewFixture(t, 2)
	cfg := DefaultReviewConfig()
	cfg.ConflictResolution = ConflictFirstWins
	service := NewReviewService(database, cfg)
	for i := 1; i <= 2; i++ {
		result, err := service.SubmitReview(ReviewOptions{RequestID: request.ID, SessionID: sessions[i].ID, SessionKey: sessions[i].SessionKey, Decision: db.DecisionApprove})
		if err != nil {
			t.Fatal(err)
		}
		if result.RequestStatusChanged != (i == 2) {
			t.Fatalf("first_wins changed quorum: %+v", result)
		}
	}
}
