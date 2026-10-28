package core

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/integrations"
)

func sameHostReviewSettingsText(resolution string, normal, critical, quorum int) string {
	return fmt.Sprintf("[general]\napproval_policy = 'same_host'\nconflict_resolution = %q\napproval_ttl_minutes = %d\napproval_ttl_critical_minutes = %d\nrequire_different_model = true\n[patterns.dangerous]\npatterns = ['^printf same-host-policy$']\nmin_approvals = %d\ndynamic_quorum = false\n[integrations]\nagent_mail_enabled = false\n", resolution, normal, critical, quorum)
}

func TestReviewServiceReloadsApprovalDeadlines(t *testing.T) {
	for _, tier := range []db.RiskTier{db.RiskTierDangerous, db.RiskTierCritical} {
		for _, explicit := range []bool{false, true} {
			for _, current := range []struct{ normal, critical int }{{3, 1}, {60, 20}} {
				t.Run(fmt.Sprintf("%s/explicit=%t/ttl=%d", tier, explicit, current.normal), func(t *testing.T) {
					database, request, _, peers := sameHostPolicyFixture(t, 1)
					if _, err := database.Exec(`UPDATE requests SET risk_tier = ? WHERE id = ?`, tier, request.ID); err != nil {
						t.Fatal(err)
					}
					path := writeExecutionPolicy(t, request.ProjectPath, sameHostReviewSettingsText("first_wins", 30, 10, 1))
					options := DefaultReviewConfig()
					options.ApprovalPolicy = db.ApprovalPolicySameHost
					if explicit {
						path = filepath.Join(t.TempDir(), "selected.toml")
						options.ConfigPath = path
						if err := os.WriteFile(path, []byte(sameHostReviewSettingsText("first_wins", 30, 10, 1)), 0600); err != nil {
							t.Fatal(err)
						}
					}
					service := NewReviewService(database, options)
					if err := os.WriteFile(path, []byte(sameHostReviewSettingsText("first_wins", current.normal, current.critical, 1)), 0600); err != nil {
						t.Fatal(err)
					}
					result, err := service.SubmitReview(ReviewOptions{RequestID: request.ID, SessionID: peers[0].ID, SessionKey: peers[0].SessionKey, Decision: db.DecisionApprove})
					if err != nil || result == nil || result.RequestStatus != db.StatusApproved || !result.RequestStatusChanged {
						t.Fatalf("fresh policy approval failed: %+v %v", result, err)
					}
					stored, err := database.GetRequest(request.ID)
					if err != nil || stored.ResolvedAt == nil || stored.ApprovalExpiresAt == nil {
						t.Fatalf("missing committed deadline: %+v %v", stored, err)
					}
					minutes := current.normal
					if tier == db.RiskTierCritical {
						minutes = current.critical
					}
					wantExpiry := stored.ResolvedAt.Add(time.Duration(minutes) * time.Minute).Truncate(time.Second)
					if !stored.ApprovalExpiresAt.Equal(wantExpiry) {
						t.Fatalf("used cached TTL: got expiry %s, want %s (%d minutes)", stored.ApprovalExpiresAt, wantExpiry, minutes)
					}
					if !db.VerifyReviewEvidenceSignature(result.Review, peers[0].SessionKey) || result.Review.Assurance != db.AssuranceSameHost {
						t.Fatal("fresh decision controls changed signed same-host assurance")
					}
				})
			}
		}
	}
}

func TestReviewServiceReloadsRejectionConflictPolicy(t *testing.T) {
	for _, current := range []struct {
		resolution string
		status     db.RequestStatus
	}{
		{"any_rejection_blocks", db.StatusRejected},
		{"human_breaks_tie", db.StatusEscalated},
		{"first_wins", db.StatusPending},
	} {
		t.Run(current.resolution, func(t *testing.T) {
			database, request, _, peers := sameHostPolicyFixture(t, 2)
			options := DefaultReviewConfig()
			options.ApprovalPolicy, options.ConflictResolution = db.ApprovalPolicySameHost, ConflictFirstWins
			service := NewReviewService(database, options)
			first, err := service.SubmitReview(ReviewOptions{RequestID: request.ID, SessionID: peers[0].ID, SessionKey: peers[0].SessionKey, Decision: db.DecisionApprove})
			if err != nil || first.RequestStatus != db.StatusPending || first.NewRequestStatus != "" {
				t.Fatalf("partial quorum did not report its committed state: %+v %v", first, err)
			}
			writeExecutionPolicy(t, request.ProjectPath, sameHostReviewSettingsText(current.resolution, 3, 1, 2))
			result, err := service.SubmitReview(ReviewOptions{RequestID: request.ID, SessionID: peers[1].ID, SessionKey: peers[1].SessionKey, Decision: db.DecisionReject})
			if err != nil || result == nil || result.RequestStatus != current.status || result.Approvals != 1 || result.Rejections != 1 {
				t.Fatalf("rejection used stale conflict policy: %+v %v", result, err)
			}
			changed := current.status != db.StatusPending
			if result.RequestStatusChanged != changed || (!changed && result.NewRequestStatus != "") {
				t.Fatalf("incorrect status-change report: %+v", result)
			}
		})
	}
}

func TestReviewPolicyResolverWaitsForTargetWriter(t *testing.T) {
	database, request, _, peers := sameHostPolicyFixture(t, 1)
	contender, err := db.OpenWithOptions(database.Path(), db.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = contender.Close() })
	writer, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback() }()
	if _, err := writer.Exec(`UPDATE requests SET id = id WHERE id = ?`, request.ID); err != nil {
		t.Fatal(err)
	}
	started, resolved := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	resolver := ReviewPolicyResolver("", nil)
	go func() {
		close(started)
		_, err := contender.ApplyReview(&db.Review{RequestID: request.ID, ReviewerSessionID: peers[0].ID, Decision: db.DecisionApprove}, peers[0].SessionKey, db.ReviewPolicy{
			ApprovalPolicy: db.ApprovalPolicySameHost, ApprovalTTL: 30 * time.Minute,
			ResolveSettings: func(current *db.Request, decision db.Decision) (db.ReviewSettings, error) {
				close(resolved)
				return resolver(current, decision)
			},
		})
		finished <- err
	}()
	<-started
	select {
	case <-resolved:
		t.Error("policy was read before the writer reservation")
	case <-time.After(50 * time.Millisecond):
	}
	writeExecutionPolicy(t, request.ProjectPath, sameHostReviewSettingsText("any_rejection_blocks", 1, 1, 1))
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("review did not finish after writer release")
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil || stored.ResolvedAt == nil || stored.ApprovalExpiresAt == nil || !stored.ApprovalExpiresAt.Equal(stored.ResolvedAt.Add(time.Minute).Truncate(time.Second)) {
		t.Fatalf("writer-waiting review used an old deadline: %+v %v", stored, err)
	}
}

func TestDelegatedReviewRefreshesPolicyAfterSourceWait(t *testing.T) {
	for _, decision := range []db.Decision{db.DecisionApprove, db.DecisionReject} {
		t.Run(string(decision), func(t *testing.T) {
			sourceProject, targetProject := t.TempDir(), t.TempDir()
			source, err := db.Open(filepath.Join(sourceProject, "source.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = source.Close() })
			target, err := db.Open(filepath.Join(targetProject, "target.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = target.Close() })
			reviewer, request, service := delegatedReviewFixture(t, source, target, sourceProject, targetProject)
			policy := func(conflict string, minutes int) string {
				return fmt.Sprintf("[general]\napproval_policy = 'same_host'\nconflict_resolution = %q\napproval_ttl_minutes = %d\ncross_project_reviews = true\nreview_pool = ['Reviewer']\n[integrations]\nagent_mail_enabled = false\n", conflict, minutes)
			}
			writeExecutionPolicy(t, targetProject, policy("first_wins", 30))
			service.config.ConflictResolution = ConflictFirstWins
			if _, err := target.Exec(`UPDATE requests SET min_approvals = 2 WHERE id = ?`, request.ID); err != nil {
				t.Fatal(err)
			}
			peer := &db.Session{AgentName: "EarlierReviewer", Model: "earlier-model", ProjectPath: targetProject}
			if err := target.CreateSession(peer); err != nil {
				t.Fatal(err)
			}
			if _, err := service.SubmitReview(ReviewOptions{RequestID: request.ID, SessionID: peer.ID, SessionKey: peer.SessionKey, Decision: db.DecisionApprove}); err != nil {
				t.Fatal(err)
			}
			reading, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			finished := make(chan error, 1)
			go func() {
				_, err := service.submitCrossProjectReview(sourceProject, ReviewOptions{RequestID: request.ID, SessionID: reviewer.ID, SessionKey: reviewer.SessionKey, Decision: decision}, func(tx *sql.Tx) (*db.Session, error) {
					session, err := source.AuthenticateSession(reviewer.ID, reviewer.SessionKey)
					if tx != nil {
						close(reading)
						<-release
					}
					return session, err
				})
				finished <- err
			}()
			select {
			case <-reading:
			case <-time.After(5 * time.Second):
				t.Fatal("review did not reach its current source check")
			}
			writeExecutionPolicy(t, targetProject, policy("any_rejection_blocks", 1))
			close(release)
			select {
			case err := <-finished:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("review did not finish after source read release")
			}
			stored, err := target.GetRequest(request.ID)
			if err != nil {
				t.Fatal(err)
			}
			if decision == db.DecisionReject {
				if stored.Status != db.StatusRejected || stored.ApprovalExpiresAt != nil {
					t.Fatalf("source-waiting rejection used stale conflict policy: %+v", stored)
				}
			} else if stored.Status != db.StatusApproved || stored.ResolvedAt == nil || stored.ApprovalExpiresAt == nil || !stored.ApprovalExpiresAt.Equal(stored.ResolvedAt.Add(time.Minute).Truncate(time.Second)) {
				t.Fatalf("source-waiting approval used stale TTL: %+v", stored)
			}
		})
	}
}

func TestReviewServiceCurrentPolicyErrorsLeaveNoVote(t *testing.T) {
	for _, problem := range []string{"invalid TOML", "unknown policy", "overflow TTL", "missing explicit file", "directory"} {
		for _, decision := range []db.Decision{db.DecisionApprove, db.DecisionReject} {
			t.Run(problem+"/"+string(decision), func(t *testing.T) {
				database, request, _, peers := sameHostPolicyFixture(t, 1)
				options := DefaultReviewConfig()
				options.ApprovalPolicy = db.ApprovalPolicySameHost
				options.ConfigPath = filepath.Join(t.TempDir(), "selected.toml")
				text := sameHostReviewSettingsText("first_wins", 30, 10, 1)
				switch problem {
				case "invalid TOML":
					text = "[general\ninvalid"
				case "unknown policy":
					text = "[general]\napproval_policy = 'unknown'\n"
				case "overflow TTL":
					text = "[general]\napproval_policy = 'same_host'\napproval_ttl_minutes = 9223372036854775807\n"
				case "directory":
					options.ConfigPath = t.TempDir()
				}
				if problem != "missing explicit file" && problem != "directory" {
					if err := os.WriteFile(options.ConfigPath, []byte(text), 0600); err != nil {
						t.Fatal(err)
					}
				}
				service := NewReviewService(database, options)
				outcome, err := service.SubmitReview(ReviewOptions{RequestID: request.ID, SessionID: peers[0].ID, SessionKey: peers[0].SessionKey, Decision: decision})
				if err == nil || outcome != nil {
					t.Fatalf("bad current policy admitted a vote: %+v %v", outcome, err)
				}
				stored, reviews, err := database.GetRequestWithReviews(request.ID)
				if err != nil || stored.Status != db.StatusPending || stored.ResolvedAt != nil || stored.ApprovalExpiresAt != nil || len(reviews) != 0 {
					t.Fatalf("failed policy load changed evidence: %+v %+v %v", stored, reviews, err)
				}
			})
		}
	}
}

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
	writeExecutionPolicy(t, request.ProjectPath, "[general]\napproval_ttl_minutes = 2\n")
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
	writeExecutionPolicy(t, request.ProjectPath, "[general]\nconflict_resolution = 'first_wins'\n")
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

func TestSameHostDelegatedReviewSupportsMultipleTargetsInSharedDatabase(t *testing.T) {
	database, first, firstAuthor, foreign, service := sameHostDelegatedReviewFixture(t, 1)
	secondProject := t.TempDir()
	if err := os.MkdirAll(filepath.Join(secondProject, ".slb"), 0700); err != nil {
		t.Fatal(err)
	}
	policy, err := os.ReadFile(filepath.Join(first.ProjectPath, ".slb", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	writeExecutionPolicy(t, secondProject, string(policy))
	secondAuthor := &db.Session{AgentName: "second-project-author", Model: firstAuthor.Model, ProjectPath: secondProject}
	if err := database.CreateSession(secondAuthor); err != nil {
		t.Fatal(err)
	}
	second := *first
	second.ID, second.ProjectPath = "", secondProject
	second.RequestorSessionID, second.RequestorAgent, second.RequestorModel = secondAuthor.ID, secondAuthor.AgentName, secondAuthor.Model
	second.Command.Cwd, second.Command.Hash = secondProject, ""
	if err := database.CreateRequest(&second); err != nil {
		t.Fatal(err)
	}
	provenanceIDs := make(map[string]bool)
	for _, request := range []*db.Request{first, &second} {
		result, err := service.SubmitCrossProjectReview(database, foreign.ProjectPath, ReviewOptions{
			RequestID: request.ID, SessionID: foreign.ID, SessionKey: foreign.SessionKey, Decision: db.DecisionApprove,
		})
		if err != nil || result == nil || result.NewRequestStatus != db.StatusApproved || result.Review.Assurance != db.AssuranceSameHost {
			t.Fatalf("authorized target %s could not receive delegated review: %+v %v", request.ProjectPath, result, err)
		}
		provenance, err := database.GetSession(result.Review.ReviewerSessionID)
		if err != nil || provenance.IsActive() || provenance.ProjectPath != request.ProjectPath || provenanceIDs[provenance.ID] {
			t.Fatalf("targets did not receive distinct ended provenance: %+v %v", provenance, err)
		}
		provenanceIDs[provenance.ID] = true
	}
	// Reviewing the second target must leave the first target's signed proof
	// usable. Both execute under their own freshly loaded target policy.
	for i, request := range []*db.Request{first, &second} {
		approved, err := database.VerifyRequestApproval(request.ID)
		if err != nil {
			t.Fatal(err)
		}
		author := []*db.Session{firstAuthor, secondAuthor}[i]
		execution := policyExecutionIdentity(author)
		if err := database.ClaimRequestExecutionAuthenticated(approved, execution, author.SessionKey, ExecutionPolicyGuard("")); err != nil {
			t.Fatalf("target could not claim its own reviewed execution: %v", err)
		}
		if execution.Assurance != db.AssuranceSameHost {
			t.Fatalf("delegation changed execution assurance: %s", execution.Assurance)
		}
	}
	assertDelegatedSourceUnchanged(t, database, foreign)
}

func TestSameHostDelegatedReviewPreservesSnapshotsAcrossSourceChanges(t *testing.T) {
	for _, change := range []string{"model", "credential"} {
		t.Run(change, func(t *testing.T) {
			database, first, author, foreign, service := sameHostDelegatedReviewFixture(t, 2)
			oldSourceID, oldKey := foreign.ID, foreign.SessionKey
			review := ReviewOptions{RequestID: first.ID, SessionID: foreign.ID, SessionKey: oldKey, Decision: db.DecisionApprove}
			result, err := service.SubmitCrossProjectReview(database, foreign.ProjectPath, review)
			if err != nil || result.RequestStatusChanged {
				t.Fatalf("initial vote did not remain below quorum: %+v %v", result, err)
			}
			oldReview, err := database.GetReview(result.Review.ID)
			if err != nil {
				t.Fatal(err)
			}
			oldProvenance, err := database.GetSession(oldReview.ReviewerSessionID)
			if err != nil {
				t.Fatal(err)
			}
			if change == "model" {
				foreign, err = ResumeSession(database, ResumeOptions{
					SessionID: foreign.ID, SessionKey: oldKey, AgentName: foreign.AgentName,
					ProjectPath: foreign.ProjectPath, Model: "resumed-model",
				})
				if err != nil {
					t.Fatal(err)
				}
				if foreign.ID == oldSourceID || foreign.SessionKey == oldKey {
					t.Fatal("model change did not return a replacement source identity and credential")
				}
				oldSource, err := database.GetSession(oldSourceID)
				if err != nil || oldSource.IsActive() || oldSource.Model != oldProvenance.Model {
					t.Fatalf("model replacement changed or retained the active historical source: %+v %v", oldSource, err)
				}
				if _, err := service.SubmitCrossProjectReview(database, foreign.ProjectPath, review); !errors.Is(err, ErrSessionInactive) {
					t.Fatalf("ended source identity admitted a fresh delegated review: %v", err)
				}
			} else {
				// There is no CLI key-rotation command. Model a credential
				// replacement at the authoritative source, then authenticate it
				// through the same public cross-project submission path.
				foreign.SessionKey = strings.Repeat("ab", 32)
				digest, ok := db.SessionKeyDigest(foreign.SessionKey)
				if !ok {
					t.Fatal("invalid replacement test credential")
				}
				if _, err := database.Exec(`UPDATE sessions SET session_key_digest = ? WHERE id = ?`, digest, foreign.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := service.SubmitCrossProjectReview(database, foreign.ProjectPath, review); !errors.Is(err, ErrSessionKeyMismatch) {
					t.Fatalf("replaced source credential still admitted a fresh review: %v", err)
				}
			}
			review.SessionID, review.SessionKey = foreign.ID, foreign.SessionKey
			if _, err := service.SubmitCrossProjectReview(database, foreign.ProjectPath, review); !errors.Is(err, ErrAlreadyReviewed) {
				t.Fatalf("a new provenance snapshot gained a second vote from one agent: %v", err)
			}
			if approvals, rejections, err := database.CountReviewsByDecision(first.ID); err != nil || approvals != 1 || rejections != 0 {
				t.Fatalf("source change altered quorum: %d/%d %v", approvals, rejections, err)
			}
			second := *first
			second.ID, second.MinApprovals = "", 1
			if err := database.CreateRequest(&second); err != nil {
				t.Fatal(err)
			}
			review.RequestID = second.ID
			result, err = service.SubmitCrossProjectReview(database, foreign.ProjectPath, review)
			if err != nil || result == nil || result.NewRequestStatus != db.StatusApproved ||
				result.Review.ReviewerSessionID == oldProvenance.ID || result.Review.ReviewerModel != foreign.Model {
				t.Fatalf("current source identity could not create a fresh snapshot: %+v %v", result, err)
			}

			// Existing delegation already treats completed votes as immutable
			// snapshots. Model/key changes must neither rewrite their identity
			// nor retroactively revoke their signatures through row reuse.
			storedReview, err := database.GetReview(oldReview.ID)
			if err != nil || !reflect.DeepEqual(storedReview, oldReview) {
				t.Fatalf("source change rewrote the old signed vote: %+v %v", storedReview, err)
			}
			storedProvenance, err := database.GetSession(oldProvenance.ID)
			if err != nil || !reflect.DeepEqual(storedProvenance, oldProvenance) {
				t.Fatalf("source change rewrote old provenance: %+v %v", storedProvenance, err)
			}
			peer := &db.Session{AgentName: "additional-local-reviewer", Model: "third-model", ProjectPath: first.ProjectPath}
			if err := database.CreateSession(peer); err != nil {
				t.Fatal(err)
			}
			if _, err := service.SubmitReview(ReviewOptions{RequestID: first.ID, SessionID: peer.ID, SessionKey: peer.SessionKey, Decision: db.DecisionApprove}); err != nil {
				t.Fatalf("unchanged old vote could not contribute to valid quorum: %v", err)
			}
			for _, request := range []*db.Request{first, &second} {
				approved, err := database.VerifyRequestApproval(request.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := database.ClaimRequestExecutionAuthenticated(approved, policyExecutionIdentity(author), author.SessionKey, ExecutionPolicyGuard("")); err != nil {
					t.Fatalf("valid snapshot could not claim execution: %v", err)
				}
			}
			assertDelegatedSourceUnchanged(t, database, foreign)
		})
	}
}
