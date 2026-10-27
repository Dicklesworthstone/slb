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
	db.SimulateIndependentIdentityForTest(t)
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
	db.SimulateIndependentIdentityForTest(t)
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
	db.SimulateIndependentIdentityForTest(t)
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

func TestSameHostReviewRejectsForeignProjectSessionInSharedDatabase(t *testing.T) {
	for _, crossProject := range []bool{false, true} {
		t.Run(fmt.Sprintf("cross_project=%t", crossProject), func(t *testing.T) {
			database, request, author, _ := sameHostPolicyFixture(t, 1)
			foreign := &db.Session{AgentName: "foreign-project-reviewer", Model: "other-model", ProjectPath: t.TempDir()}
			if err := database.CreateSession(foreign); err != nil {
				t.Fatal(err)
			}
			writeExecutionPolicy(t, request.ProjectPath, fmt.Sprintf("[general]\napproval_policy = 'same_host'\nrequire_different_model = true\ncross_project_reviews = %t\nreview_pool = ['foreign-project-reviewer']\n[integrations]\nagent_mail_enabled = false\n", crossProject))
			options := DefaultReviewConfig()
			options.ApprovalPolicy, options.CrossProjectReviews = db.ApprovalPolicySameHost, crossProject
			options.ReviewPool = []string{foreign.AgentName}
			service := NewReviewService(database, options)
			review := ReviewOptions{RequestID: request.ID, SessionID: foreign.ID, SessionKey: foreign.SessionKey, Decision: db.DecisionApprove}
			// Even an allowed foreign reviewer must take the delegated path;
			// a shared DB does not turn its session into a local one.
			result, err := service.SubmitReview(review)
			if result != nil || !errors.Is(err, db.ErrReviewSessionProjectMismatch) {
				allowed, reason := NewExecutor(database, nil).CanExecute(request.ID)
				t.Fatalf("foreign project bypassed cross-project review policy: result=%+v err=%v executable=%t reason=%q", result, err, allowed, reason)
			}
			stored, err := database.GetRequest(request.ID)
			if err != nil {
				t.Fatal(err)
			}
			approvals, rejections, err := database.CountReviewsByDecision(request.ID)
			if err != nil || approvals+rejections != 0 || stored.Status != db.StatusPending {
				t.Fatalf("denied foreign review changed the request: status=%s approvals=%d rejections=%d err=%v", stored.Status, approvals, rejections, err)
			}
			result, err = service.SubmitCrossProjectReview(database, foreign.ProjectPath, review)
			if !crossProject {
				if result != nil || !errors.Is(err, ErrCrossProjectReviewDenied) {
					t.Fatalf("disabled delegated review was accepted: %+v %v", result, err)
				}
				return
			}
			if err != nil || result == nil || result.Review.Assurance != db.AssuranceSameHost {
				t.Fatalf("authorized same-host delegation failed: %+v %v", result, err)
			}
			provenance, err := database.GetSession(result.Review.ReviewerSessionID)
			if err != nil || provenance.ID == foreign.ID || provenance.IsActive() || provenance.ProjectPath != request.ProjectPath {
				t.Fatalf("delegation did not preserve target-scoped ended provenance: %+v %v", provenance, err)
			}
			approved, err := database.VerifyRequestApproval(request.ID)
			if err != nil {
				t.Fatal(err)
			}
			execution := policyExecutionIdentity(author)
			if err := database.ClaimRequestExecutionAuthenticated(approved, execution, author.SessionKey, ExecutionPolicyGuard("")); err != nil {
				t.Fatalf("authorized delegated evidence could not claim execution: %v", err)
			}
			if execution.Assurance != db.AssuranceSameHost {
				t.Fatalf("delegation changed execution assurance: %s", execution.Assurance)
			}
		})
	}
}

func TestSameHostDelegatedReviewRechecksTargetPolicy(t *testing.T) {
	for _, change := range []string{"disabled", "removed from pool"} {
		for _, phase := range []string{"review", "execution"} {
			t.Run(change+"/"+phase, func(t *testing.T) {
				database, request, author, _ := sameHostPolicyFixture(t, 1)
				foreign := &db.Session{AgentName: "delegated-reviewer", Model: "other-model", ProjectPath: t.TempDir()}
				if err := database.CreateSession(foreign); err != nil {
					t.Fatal(err)
				}
				policyText := func(enabled bool, reviewer string) string {
					return fmt.Sprintf("[general]\napproval_policy = 'same_host'\nrequire_different_model = true\ncross_project_reviews = %t\nreview_pool = [%q]\n[integrations]\nagent_mail_enabled = false\n", enabled, reviewer)
				}
				writeExecutionPolicy(t, request.ProjectPath, policyText(true, foreign.AgentName))
				options := DefaultReviewConfig()
				options.ApprovalPolicy, options.CrossProjectReviews = db.ApprovalPolicySameHost, true
				options.ReviewPool = []string{foreign.AgentName}
				service := NewReviewService(database, options)
				review := ReviewOptions{RequestID: request.ID, SessionID: foreign.ID, SessionKey: foreign.SessionKey, Decision: db.DecisionApprove}
				if phase == "execution" {
					if _, err := service.SubmitCrossProjectReview(database, foreign.ProjectPath, review); err != nil {
						t.Fatal(err)
					}
				}
				enabled, pool := false, foreign.AgentName
				if change == "removed from pool" {
					enabled, pool = true, "another-reviewer"
				}
				writeExecutionPolicy(t, request.ProjectPath, policyText(enabled, pool))
				if phase == "review" {
					result, err := service.SubmitCrossProjectReview(database, foreign.ProjectPath, review)
					if result != nil || !errors.Is(err, ErrCrossProjectReviewDenied) {
						t.Fatalf("stale service ignored current target policy: %+v %v", result, err)
					}
					approvals, rejections, err := database.CountReviewsByDecision(request.ID)
					if err != nil || approvals+rejections != 0 {
						t.Fatalf("denied delegated vote was persisted: %d/%d %v", approvals, rejections, err)
					}
				} else {
					if allowed, reason := NewExecutor(database, nil).CanExecute(request.ID); allowed || reason == "" {
						t.Fatalf("target policy revocation retained delegated execution: allowed=%t reason=%q", allowed, reason)
					}
					approved, err := database.VerifyRequestApproval(request.ID)
					if err != nil {
						t.Fatalf("configuration change rewrote signed evidence: %v", err)
					}
					if err := database.ClaimRequestExecutionAuthenticated(approved, policyExecutionIdentity(author), author.SessionKey, ExecutionPolicyGuard("")); !errors.Is(err, ErrApprovalPolicyChanged) {
						t.Fatalf("atomic claim ignored delegated policy revocation: %v", err)
					}
					assertPolicyUnclaimed(t, database, request.ID)
				}
				source, err := database.AuthenticateSession(foreign.ID, foreign.SessionKey)
				if err != nil || !source.IsActive() || source.ProjectPath != foreign.ProjectPath || source.Model != foreign.Model {
					t.Fatalf("target revocation mutated the source session: %+v %v", source, err)
				}
			})
		}
	}
}

func sameHostDelegatedReviewFixture(t *testing.T, quorum int) (*db.DB, *db.Request, *db.Session, *db.Session, *ReviewService) {
	t.Helper()
	database, request, author, _ := sameHostPolicyFixture(t, quorum)
	foreign := &db.Session{AgentName: "delegated-reviewer", Model: "other-model", ProjectPath: t.TempDir()}
	if err := database.CreateSession(foreign); err != nil {
		t.Fatal(err)
	}
	writeExecutionPolicy(t, request.ProjectPath, "[general]\napproval_policy = 'same_host'\nrequire_different_model = true\ncross_project_reviews = true\nreview_pool = ['delegated-reviewer']\n[integrations]\nagent_mail_enabled = false\n")
	options := DefaultReviewConfig()
	options.ApprovalPolicy, options.CrossProjectReviews = db.ApprovalPolicySameHost, true
	options.ReviewPool = []string{foreign.AgentName}
	return database, request, author, foreign, NewReviewService(database, options)
}

func assertDelegatedSourceUnchanged(t *testing.T, database *db.DB, foreign *db.Session) {
	t.Helper()
	source, err := database.AuthenticateSession(foreign.ID, foreign.SessionKey)
	if err != nil || !source.IsActive() || source.ProjectPath != foreign.ProjectPath ||
		source.AgentName != foreign.AgentName || source.Model != foreign.Model {
		t.Fatalf("delegation mutated the source session: %+v %v", source, err)
	}
}

func TestSameHostDelegatedReviewRejectsAlteredProvenance(t *testing.T) {
	for _, mutation := range []struct{ name, query string }{
		{"relabeled as local", `UPDATE sessions SET program = 'local-agent' WHERE id = ?`},
		{"active provenance", `UPDATE sessions SET ended_at = NULL WHERE id = ?`},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			database, request, author, foreign, service := sameHostDelegatedReviewFixture(t, 1)
			review := ReviewOptions{RequestID: request.ID, SessionID: foreign.ID, SessionKey: foreign.SessionKey, Decision: db.DecisionApprove}
			result, err := service.SubmitCrossProjectReview(database, foreign.ProjectPath, review)
			if err != nil {
				t.Fatal(err)
			}
			approved, err := database.VerifyRequestApproval(request.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.Exec(mutation.query, result.Review.ReviewerSessionID); err != nil {
				t.Fatal(err)
			}
			if _, err := database.VerifyRequestApproval(request.ID); !errors.Is(err, db.ErrInvalidApprovalProof) {
				t.Fatalf("altered provenance remained valid evidence: %v", err)
			}
			if err := database.ClaimRequestExecutionAuthenticated(approved, policyExecutionIdentity(author), author.SessionKey, ExecutionPolicyGuard("")); !errors.Is(err, db.ErrInvalidApprovalProof) {
				t.Fatalf("claim accepted altered delegation: %v", err)
			}
			assertPolicyUnclaimed(t, database, request.ID)

			// A colliding provenance row must also fail before a new vote can
			// reuse it, even when all identity and key fields still match.
			next := *request
			next.ID = ""
			if err := database.CreateRequest(&next); err != nil {
				t.Fatal(err)
			}
			review.RequestID = next.ID
			if result, err := service.SubmitCrossProjectReview(database, foreign.ProjectPath, review); result != nil || !errors.Is(err, db.ErrReviewDelegationMismatch) {
				t.Fatalf("colliding provenance admitted a fresh vote: %+v %v", result, err)
			}
			assertDelegatedSourceUnchanged(t, database, foreign)
		})
	}
}

func TestSameHostDelegatedRejectionSurvivesTargetOptOut(t *testing.T) {
	database, request, author, foreign, service := sameHostDelegatedReviewFixture(t, 1)
	writeExecutionPolicy(t, request.ProjectPath, "[general]\napproval_policy = 'same_host'\nrequire_different_model = true\ncross_project_reviews = false\nreview_pool = []\n[integrations]\nagent_mail_enabled = false\n")
	result, err := service.SubmitCrossProjectReview(database, foreign.ProjectPath, ReviewOptions{
		RequestID: request.ID, SessionID: foreign.ID, SessionKey: foreign.SessionKey, Decision: db.DecisionReject,
	})
	if err != nil || result == nil || result.NewRequestStatus != db.StatusRejected || result.Review.Assurance != db.AssuranceSameHost {
		t.Fatalf("target opt-out prevented rejection: %+v %v", result, err)
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if allowed, reason := NewExecutor(database, nil).CanExecute(request.ID); allowed || reason == "" {
		t.Fatalf("delegated rejection permitted execution: %t %q", allowed, reason)
	}
	if err := database.ClaimRequestExecutionAuthenticated(stored, policyExecutionIdentity(author), author.SessionKey, ExecutionPolicyGuard("")); err == nil {
		t.Fatal("claim accepted a delegated rejection")
	}
	stored, err = database.GetRequest(request.ID)
	if err != nil || stored.Status != db.StatusRejected || stored.Execution != nil {
		t.Fatalf("rejected request was claimed: %+v %v", stored, err)
	}
	assertDelegatedSourceUnchanged(t, database, foreign)
}

func TestSameHostDelegatedApprovalStillExpires(t *testing.T) {
	database, request, author, foreign, service := sameHostDelegatedReviewFixture(t, 1)
	if _, err := service.SubmitCrossProjectReview(database, foreign.ProjectPath, ReviewOptions{
		RequestID: request.ID, SessionID: foreign.ID, SessionKey: foreign.SessionKey, Decision: db.DecisionApprove,
	}); err != nil {
		t.Fatal(err)
	}
	approved, err := database.VerifyRequestApproval(request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE requests SET approval_expires_at = '2000-01-01T00:00:00Z' WHERE id = ?`, request.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.VerifyRequestApproval(request.ID); !errors.Is(err, db.ErrInvalidApprovalProof) {
		t.Fatalf("expired delegated approval remained executable: %v", err)
	}
	if err := database.ClaimRequestExecutionAuthenticated(approved, policyExecutionIdentity(author), author.SessionKey, ExecutionPolicyGuard("")); err == nil {
		t.Fatal("claim accepted expired delegated approval")
	}
	assertPolicyUnclaimed(t, database, request.ID)
	assertDelegatedSourceUnchanged(t, database, foreign)
}
