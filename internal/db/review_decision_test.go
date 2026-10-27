package db

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func reviewDecisionFixture(t *testing.T, quorum int, policies ...ApprovalPolicy) (*DB, *Request, []*Session) {
	t.Helper()
	project := t.TempDir()
	database, err := Open(filepath.Join(project, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	sessions := make([]*Session, 0, 18)
	for i := 0; i < 18; i++ {
		s := &Session{ID: fmt.Sprintf("review-session-%d", i), AgentName: fmt.Sprintf("Agent%d", i),
			Model: fmt.Sprintf("model-%d", i), Program: "test", ProjectPath: project}
		if err := database.CreateSession(s); err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, s)
	}
	r := &Request{ID: "review-decision", ProjectPath: project, RequestorSessionID: sessions[0].ID,
		RequestorAgent: sessions[0].AgentName, RequestorModel: sessions[0].Model,
		Command:  CommandSpec{Raw: "echo reviewed", Argv: []string{"echo", "reviewed"}, Cwd: project},
		RiskTier: RiskTierDangerous, Status: StatusPending, MinApprovals: quorum,
		Justification: Justification{Reason: "Review transaction test"}}
	if len(policies) > 0 {
		r.ApprovalPolicy = policies[0]
	}
	if err := database.CreateRequest(r); err != nil {
		t.Fatal(err)
	}
	return database, r, sessions
}

func applyFixtureReview(database *DB, request *Request, session *Session, decision Decision, policy ReviewPolicy) (*ReviewOutcome, error) {
	return database.ApplyReview(&Review{RequestID: request.ID, ReviewerSessionID: session.ID, Decision: decision}, session.SessionKey, policy)
}

// Even possession of every local HMAC key must not establish independent
// identity. Exercise the public DB boundary, not a mock identity checker.
func TestIndependentIdentityFailsClosed(t *testing.T) {
	for _, attack := range []struct {
		name, agent, model                     string
		trustedSelf, delegated, differentModel bool
	}{
		{name: "forged reviewer", agent: "OtherReviewer", model: "other-model"},
		{name: "self approval", agent: "Requester", model: "request-model"},
		{name: "trusted self approval", agent: "Requester", model: "request-model", trustedSelf: true},
		{name: "fabricated different model", agent: "OtherReviewer", model: "other-model", differentModel: true},
		{name: "cross project identity", agent: "OtherReviewer", model: "other-model", delegated: true},
	} {
		t.Run(attack.name, func(t *testing.T) {
			database, err := Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			requester := &Session{AgentName: "Requester", Model: "request-model", ProjectPath: "/project"}
			if err := database.CreateSession(requester); err != nil {
				t.Fatal(err)
			}
			reviewer := requester
			if attack.agent != requester.AgentName {
				reviewer = &Session{AgentName: attack.agent, Model: attack.model, ProjectPath: "/project"}
				if err := database.CreateSession(reviewer); err != nil {
					t.Fatal(err)
				}
			}
			request := &Request{ProjectPath: "/project", RequestorSessionID: requester.ID,
				RequestorAgent: requester.AgentName, RequestorModel: requester.Model,
				Command:  CommandSpec{Raw: "echo review", Argv: []string{"echo", "review"}, Cwd: "/project"},
				RiskTier: RiskTierDangerous, Status: StatusPending, MinApprovals: 1,
				RequireDifferentModel: attack.differentModel, Justification: Justification{Reason: "identity regression"}}
			if err := database.CreateRequest(request); err != nil {
				t.Fatal(err)
			}
			policy := ReviewPolicy{}
			if attack.trustedSelf {
				policy.TrustedSelfApprove = []string{requester.AgentName}
			}
			if attack.delegated {
				policy.DelegatedReviewer = &DelegatedReviewerIdentity{SourceProjectPath: "/other", SourceSessionID: reviewer.ID, AgentName: attack.agent, Model: attack.model}
			}
			review := &Review{RequestID: request.ID, ReviewerSessionID: reviewer.ID,
				ReviewerAgent: attack.agent, ReviewerModel: attack.model, Decision: DecisionApprove}
			if result, err := database.ApplyReview(review, reviewer.SessionKey, policy); result != nil || !errors.Is(err, ErrIndependentIdentityUnavailable) {
				t.Fatalf("unverified identity authorized approval: %v", err)
			}
			stored, err := database.GetRequest(request.ID)
			if err != nil || stored.Status != StatusPending {
				t.Fatalf("denial mutated request: %v", err)
			}
			approvals, _, err := database.CountReviewsByDecision(request.ID)
			if err != nil || approvals != 0 {
				t.Fatalf("denial inserted vote: %d %v", approvals, err)
			}

			// Simulate a historical approval or an attacker who can read the
			// SQLite keys and write a perfectly valid local HMAC and status.
			review.SignatureTimestamp = time.Now().UTC().Truncate(time.Second)
			review.Signature = ComputeReviewSignature(reviewer.SessionKey, request.ID, DecisionApprove, review.SignatureTimestamp)
			if err := database.CreateReview(review); err != nil {
				t.Fatal(err)
			}
			expiry := time.Now().UTC().Add(time.Hour)
			if _, err := database.Exec(`UPDATE requests SET status = 'approved', approval_expires_at = ? WHERE id = ?`, expiry.Format(time.RFC3339), request.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := database.VerifyRequestApproval(request.ID); !errors.Is(err, ErrIndependentIdentityUnavailable) {
				t.Fatalf("historical proof accepted: %v", err)
			}
			if approved, _, err := database.CheckRequestApprovalStatus(request.ID); approved || !errors.Is(err, ErrIndependentIdentityUnavailable) {
				t.Fatalf("status helper accepted local identity: %v", err)
			}
			request.Status, request.ApprovalExpiresAt = StatusApproved, &expiry
			now := time.Now().UTC()
			execution := &Execution{ExecutedAt: &now, ExecutedBySessionID: requester.ID, ExecutedByAgent: requester.AgentName, ExecutedByModel: requester.Model, LogPath: "identity-test.log"}
			if err := database.ClaimRequestExecutionAuthenticated(request, execution, requester.SessionKey); !errors.Is(err, ErrIndependentIdentityUnavailable) {
				t.Fatalf("authenticated execution accepted local identity: %v", err)
			}
			if err := database.ClaimRequestExecution(request, execution); !errors.Is(err, ErrIndependentIdentityUnavailable) {
				t.Fatalf("local execution accepted local identity: %v", err)
			}
			assertNoExecutionClaim(t, database, request.ID)
		})
	}
}

func TestIndependentIdentityStillAllowsRejection(t *testing.T) {
	database, request, sessions := reviewDecisionFixture(t, 1)
	outcome, err := applyFixtureReview(database, request, sessions[1], DecisionReject, ReviewPolicy{})
	if err != nil || outcome.Request.Status != StatusRejected || outcome.Approvals != 0 {
		t.Fatalf("rejection was blocked or approved request: %+v %v", outcome, err)
	}
	approved, rejected, err := database.CheckRequestApprovalStatus(request.ID)
	if err != nil || approved || !rejected {
		t.Fatalf("rejection status was lost: approved=%v rejected=%v err=%v", approved, rejected, err)
	}
}

func TestApplyReviewPersistsApprovalDeadline(t *testing.T) {
	SimulateIndependentIdentityForTest(t)
	for _, tier := range []RiskTier{RiskTierDangerous, RiskTierCritical} {
		t.Run(string(tier), func(t *testing.T) {
			database, request, sessions := reviewDecisionFixture(t, 2)
			if _, err := database.Exec(`UPDATE requests SET risk_tier = ? WHERE id = ?`, string(tier), request.ID); err != nil {
				t.Fatal(err)
			}
			policy := ReviewPolicy{ApprovalTTL: 12 * time.Minute, CriticalApprovalTTL: 4 * time.Minute}
			one, err := applyFixtureReview(database, request, sessions[1], DecisionApprove, policy)
			if err != nil || one.StatusChanged || one.Request.ApprovalExpiresAt != nil {
				t.Fatalf("premature approval: %+v %v", one, err)
			}
			before := time.Now().UTC()
			two, err := applyFixtureReview(database, request, sessions[2], DecisionApprove, policy)
			if err != nil {
				t.Fatal(err)
			}
			stored, err := database.GetRequest(request.ID)
			if err != nil {
				t.Fatal(err)
			}
			ttl := policy.ApprovalTTL
			if tier == RiskTierCritical {
				ttl = policy.CriticalApprovalTTL
			}
			if !two.StatusChanged || two.Approvals != 2 || stored.Status != StatusApproved || stored.ResolvedAt == nil || stored.ApprovalExpiresAt == nil {
				t.Fatalf("missing atomic approval/deadline: %+v %+v", two, stored)
			}
			if stored.ApprovalExpiresAt.Before(before.Add(ttl-time.Second)) || stored.ApprovalExpiresAt.After(time.Now().Add(ttl+time.Second)) {
				t.Fatalf("wrong approval TTL: %v", stored.ApprovalExpiresAt)
			}
			if !VerifyReviewSignature(sessions[2].SessionKey, two.Review.RequestID, two.Review.Decision, two.Review.SignatureTimestamp, two.Review.Signature) {
				t.Fatal("committed review signature does not verify")
			}
		})
	}
}

func TestApplyReviewRejectsChangedEligibility(t *testing.T) {
	SimulateIndependentIdentityForTest(t)
	for _, tc := range []struct {
		name, query string
		want        error
	}{
		{"expired", `UPDATE requests SET expires_at = '2000-01-01T00:00:00Z'`, ErrReviewExpired},
		{"malformed expiry", `UPDATE requests SET expires_at = 'invalid'`, ErrReviewExpired},
		{"cancelled", `UPDATE requests SET status = 'cancelled'`, ErrReviewNotPending},
		{"executing", `UPDATE requests SET status = 'executing'`, ErrReviewNotPending},
		{"ended reviewer", `UPDATE sessions SET ended_at = '2000-01-01T00:00:00Z' WHERE id = 'review-session-1'`, ErrReviewSessionInactive},
		{"rotated key", `UPDATE sessions SET session_key_digest = 'different' WHERE id = 'review-session-1'`, ErrReviewSessionKeyMismatch},
		{"changed command", `UPDATE requests SET command_raw = 'echo changed'`, ErrInvalidSignature},
		{"changed model rule", `UPDATE requests SET require_different_model = 1, requestor_model = 'model-1'`, ErrReviewDifferentModel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, request, sessions := reviewDecisionFixture(t, 1)
			if _, err := database.Exec(tc.query); err != nil {
				t.Fatal(err)
			}
			if result, err := applyFixtureReview(database, request, sessions[1], DecisionApprove, ReviewPolicy{}); result != nil || !errors.Is(err, tc.want) {
				t.Fatalf("accepted invalid eligibility: %+v, %v; want %v", result, err, tc.want)
			}
			approvals, rejections, err := database.CountReviewsByDecision(request.ID)
			if err != nil || approvals+rejections != 0 {
				t.Fatalf("rejected review persisted: %d %d %v", approvals, rejections, err)
			}
		})
	}
}

func TestApplyReviewOutcomeFailureRollsBackVote(t *testing.T) {
	database, request, sessions := reviewDecisionFixture(t, 1)
	_, err := database.Exec(`CREATE TRIGGER refuse_approval BEFORE UPDATE OF approval_expires_at ON requests BEGIN SELECT RAISE(ABORT, 'injected expiry write failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	review := &Review{RequestID: request.ID, ReviewerSessionID: sessions[1].ID, Decision: DecisionApprove}
	if outcome, err := database.ApplyReview(review, sessions[1].SessionKey, ReviewPolicy{}); err == nil || outcome != nil {
		t.Fatalf("unexpected success: %+v, %v", outcome, err)
	}
	if review.ID != "" || review.Signature != "" {
		t.Fatal("rollback changed caller's review")
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, r, err := database.CountReviewsByDecision(request.ID)
	if err != nil || a+r != 0 || stored.Status != StatusPending || stored.ApprovalExpiresAt != nil {
		t.Fatalf("partial approval was persisted: %+v, %d/%d, %v", stored, a, r, err)
	}
}

func TestApplyReviewWaitsForWriterThenRevalidatesSession(t *testing.T) {
	SimulateIndependentIdentityForTest(t)
	database, request, sessions := reviewDecisionFixture(t, 1)
	other, err := Open(filepath.Join(request.ProjectPath, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE sessions SET ended_at = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339), sessions[1].ID); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		close(started)
		_, err := applyFixtureReview(other, request, sessions[1], DecisionApprove, ReviewPolicy{})
		result <- err
	}()
	<-started
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, ErrReviewSessionInactive) {
			t.Fatalf("review ignored committed session revocation: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("review did not finish after writer committed")
	}
}

func TestSameHostReviewBindsDisplayedPolicy(t *testing.T) {
	for _, decision := range []Decision{DecisionApprove, DecisionReject} {
		t.Run(string(decision), func(t *testing.T) {
			database, request, sessions := reviewDecisionFixture(t, 1, ApprovalPolicySameHost)
			out, err := applyFixtureReview(database, request, sessions[1], decision, ReviewPolicy{
				ApprovalPolicy: ApprovalPolicySameHost, ExpectedApprovalPolicy: ApprovalPolicyIndependent,
			})
			if out != nil || !errors.Is(err, ErrReviewTargetChanged) {
				t.Fatalf("review accepted a different policy from the displayed request: %+v %v", out, err)
			}
			approvals, rejections, err := database.CountReviewsByDecision(request.ID)
			if err != nil || approvals+rejections != 0 {
				t.Fatalf("stale view persisted a vote: %d/%d %v", approvals, rejections, err)
			}
		})
	}
}

func TestSameHostReviewRechecksPolicyAfterWriterReservation(t *testing.T) {
	database, request, sessions := reviewDecisionFixture(t, 1, ApprovalPolicySameHost)
	other, err := Open(filepath.Join(request.ProjectPath, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE requests SET id = id WHERE id = ?`, request.ID); err != nil {
		t.Fatal(err)
	}
	var revoked atomic.Bool
	checked := make(chan struct{})
	result := make(chan error, 1)
	review := &Review{RequestID: request.ID, ReviewerSessionID: sessions[1].ID, Decision: DecisionApprove}
	go func() {
		_, err := other.ApplyReview(review, sessions[1].SessionKey, ReviewPolicy{
			ApprovalPolicy: ApprovalPolicySameHost,
			PolicyCheck: func(current *Request) error {
				close(checked)
				if current.ID != request.ID || current.ApprovalPolicy != ApprovalPolicySameHost {
					return fmt.Errorf("policy guard received a different request")
				}
				if revoked.Load() {
					return ErrReviewPolicyMismatch
				}
				return nil
			},
		})
		result <- err
	}()
	// The fresh-policy callback cannot run while another connection owns the
	// writer reservation, even if ApplyReview was called with an older opt-in.
	select {
	case <-checked:
		t.Error("policy check ran before acquiring the writer reservation")
	case <-time.After(100 * time.Millisecond):
	}
	revoked.Store(true)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, ErrReviewPolicyMismatch) {
			t.Fatalf("review ignored current policy revocation: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("review did not finish after writer committed")
	}
	if review.ID != "" || review.Signature != "" {
		t.Fatal("policy denial changed the caller's review")
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil || stored.Status != StatusPending || stored.ApprovalExpiresAt != nil {
		t.Fatalf("policy denial changed request state: %+v %v", stored, err)
	}
	approvals, rejections, err := database.CountReviewsByDecision(request.ID)
	if err != nil || approvals+rejections != 0 {
		t.Fatalf("policy denial persisted a vote: %d/%d %v", approvals, rejections, err)
	}
}

func TestSameHostReviewAllowsRejectionAfterOptOut(t *testing.T) {
	database, request, sessions := reviewDecisionFixture(t, 1, ApprovalPolicySameHost)
	checked := false
	out, err := applyFixtureReview(database, request, sessions[1], DecisionReject, ReviewPolicy{
		ApprovalPolicy: ApprovalPolicyIndependent, ExpectedApprovalPolicy: ApprovalPolicySameHost,
		PolicyCheck: func(*Request) error {
			checked = true
			return ErrReviewPolicyMismatch
		},
	})
	if err != nil || out == nil || out.Request.Status != StatusRejected {
		t.Fatalf("opt-out prevented rejection of a same-host request: %+v %v", out, err)
	}
	if checked {
		t.Fatal("rejection unexpectedly required current approval authorization")
	}
	if out.Review.ApprovalPolicy != ApprovalPolicySameHost || out.Review.Assurance != AssuranceSameHost {
		t.Fatalf("rejection lost its original same-host policy and assurance: %+v", out.Review)
	}
}

func TestApplyReviewConcurrentQuorumAcrossConnections(t *testing.T) {
	SimulateIndependentIdentityForTest(t)
	database, request, sessions := reviewDecisionFixture(t, 16)
	other, err := Open(filepath.Join(request.ProjectPath, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	connections := []*DB{database, other}
	start := make(chan struct{})
	results := make(chan *ReviewOutcome, 16)
	errs := make(chan error, 16)
	var wg sync.WaitGroup
	for i := 1; i <= 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			out, err := applyFixtureReview(connections[i%2], request, sessions[i], DecisionApprove, ReviewPolicy{})
			results <- out
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent review failed: %v", err)
		}
	}
	decisions := 0
	for out := range results {
		if out != nil && out.StatusChanged {
			decisions++
		}
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, _, err := database.CountReviewsByDecision(request.ID)
	if err != nil || a != 16 || decisions != 1 || stored.Status != StatusApproved || stored.ApprovalExpiresAt == nil {
		t.Fatalf("lost reviews/quorum: approvals=%d decisions=%d status=%s error=%v", a, decisions, stored.Status, err)
	}
}

func TestApplyReviewRestartDoesNotCreateAnotherVoter(t *testing.T) {
	SimulateIndependentIdentityForTest(t)
	database, request, sessions := reviewDecisionFixture(t, 2)
	if _, err := applyFixtureReview(database, request, sessions[1], DecisionApprove, ReviewPolicy{}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE sessions SET ended_at = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339), sessions[1].ID); err != nil {
		t.Fatal(err)
	}
	restarted := &Session{ID: "restarted", AgentName: sessions[1].AgentName, Model: "changed-model", Program: "test", ProjectPath: request.ProjectPath}
	if err := database.CreateSession(restarted); err != nil {
		t.Fatal(err)
	}
	if _, err := applyFixtureReview(database, request, restarted, DecisionApprove, ReviewPolicy{}); !errors.Is(err, ErrReviewExists) {
		t.Fatalf("restart gained another vote: %v", err)
	}
	if _, err := database.Exec(`UPDATE sessions SET ended_at = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339), sessions[0].ID); err != nil {
		t.Fatal(err)
	}
	author := &Session{ID: "author-restarted", AgentName: sessions[0].AgentName, Model: "other", Program: "test", ProjectPath: request.ProjectPath}
	if err := database.CreateSession(author); err != nil {
		t.Fatal(err)
	}
	if _, err := applyFixtureReview(database, request, author, DecisionApprove, ReviewPolicy{}); !errors.Is(err, ErrSelfReview) {
		t.Fatalf("requestor restart allowed self review: %v", err)
	}
}

func TestApplyReviewConflictPoliciesAndQuorum(t *testing.T) {
	SimulateIndependentIdentityForTest(t)
	for _, mode := range []string{"any_rejection_blocks", "first_wins", "human_breaks_tie"} {
		t.Run(mode, func(t *testing.T) {
			database, request, sessions := reviewDecisionFixture(t, 2)
			p := ReviewPolicy{ConflictResolution: mode}
			one, err := applyFixtureReview(database, request, sessions[1], DecisionApprove, p)
			if err != nil || one.StatusChanged {
				t.Fatalf("first vote bypassed quorum: %+v %v", one, err)
			}
			two, err := applyFixtureReview(database, request, sessions[2], DecisionReject, p)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "any_rejection_blocks":
				if two.Request.Status != StatusRejected {
					t.Fatal(two.Request.Status)
				}
			case "human_breaks_tie":
				if two.Request.Status != StatusEscalated {
					t.Fatal(two.Request.Status)
				}
			case "first_wins":
				if two.StatusChanged {
					t.Fatal("later rejection changed first-wins outcome")
				}
				three, err := applyFixtureReview(database, request, sessions[3], DecisionApprove, p)
				if err != nil || three.Request.Status != StatusApproved {
					t.Fatalf("quorum did not resolve: %+v %v", three, err)
				}
			}
		})
	}
}

func TestApplyReviewAuthenticatesSuppliedSignature(t *testing.T) {
	SimulateIndependentIdentityForTest(t)
	for _, mode := range []string{"forged", "stale", "future", "valid"} {
		t.Run(mode, func(t *testing.T) {
			database, request, sessions := reviewDecisionFixture(t, 1)
			r := &Review{RequestID: request.ID, ReviewerSessionID: sessions[1].ID, Decision: DecisionApprove, ReviewerAgent: "forged-name", ReviewerModel: "forged-model", SignatureTimestamp: time.Now().UTC()}
			if mode == "stale" {
				r.SignatureTimestamp = r.SignatureTimestamp.Add(-6 * time.Minute)
			}
			if mode == "future" {
				r.SignatureTimestamp = r.SignatureTimestamp.Add(2 * time.Minute)
			}
			r.Signature = ComputeReviewSignature(sessions[1].SessionKey, r.RequestID, r.Decision, r.SignatureTimestamp)
			if mode == "forged" {
				r.Signature = strings.Repeat("0", 64)
			}
			out, err := database.ApplyReview(r, sessions[1].SessionKey, ReviewPolicy{})
			if mode == "valid" {
				if err != nil || out.Review.ReviewerAgent != sessions[1].AgentName || out.Review.ReviewerModel != sessions[1].Model {
					t.Fatalf("persisted untrusted identity: %+v %v", out, err)
				}
			} else if !errors.Is(err, ErrInvalidSignature) {
				t.Fatalf("invalid signature admitted: %v", err)
			}
		})
	}
}

func TestSignedReviewEntryPointAuthenticatesPersistedSession(t *testing.T) {
	SimulateIndependentIdentityForTest(t)
	database, request, sessions := reviewDecisionFixture(t, 1)
	review := &Review{RequestID: request.ID, ReviewerSessionID: sessions[1].ID,
		Decision: DecisionApprove, SignatureTimestamp: time.Now().UTC()}
	attackerKey := strings.Repeat("ab", 32)
	review.Signature = ComputeReviewSignature(attackerKey, review.RequestID, review.Decision, review.SignatureTimestamp)
	if err := database.CreateReviewWithValidation(review, attackerKey); !errors.Is(err, ErrReviewSessionKeyMismatch) {
		t.Fatalf("external review authenticated using an arbitrary supplied key: %v", err)
	}
	review.Signature = ComputeReviewSignature(sessions[1].SessionKey, review.RequestID, review.Decision, review.SignatureTimestamp)
	if err := database.CreateReviewWithValidation(review, sessions[1].SessionKey); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusApproved || stored.ApprovalExpiresAt == nil {
		t.Fatal("signed review entry point omitted deadline")
	}
}

// No simulated authority: this is the actual low-assurance production path.
func TestSameHostReviewIntegrationOptIn(t *testing.T) {
	database, request, sessions := reviewDecisionFixture(t, 2, ApprovalPolicySameHost)
	policy := ReviewPolicy{ApprovalPolicy: ApprovalPolicySameHost, ApprovalTTL: 4 * time.Minute}
	for i := 1; i <= 2; i++ {
		outcome, err := applyFixtureReview(database, request, sessions[i], DecisionApprove, policy)
		if err != nil {
			t.Fatal(err)
		}
		if outcome.Approvals != i || outcome.StatusChanged != (i == 2) {
			t.Fatalf("wrong quorum transition: %+v", outcome)
		}
		vote, err := database.GetReview(outcome.Review.ID)
		if err != nil {
			t.Fatal(err)
		}
		if vote.ApprovalPolicy != ApprovalPolicySameHost || vote.Assurance != AssuranceSameHost ||
			vote.CommandHash != request.Command.Hash || vote.SignatureVersion != 2 || vote.RequestBinding == "" {
			t.Fatalf("missing same-host evidence: %+v", vote)
		}
		if !VerifyReviewEvidenceSignature(vote, sessions[i].SessionKey) ||
			VerifyReviewSignature(sessions[i].SessionKey, vote.RequestID, vote.Decision, vote.SignatureTimestamp, vote.Signature) {
			t.Fatal("versioned signature failed verification or crossed into the historical signature domain")
		}
		request = outcome.Request
	}
	if request.ApprovalExpiresAt == nil || request.ResolvedAt == nil || request.ApprovalExpiresAt.Sub(*request.ResolvedAt) != 4*time.Minute {
		t.Fatalf("missing finite approval deadline: %+v", request)
	}
	// Ending a reviewer's session does not invalidate a completed review.
	if err := database.EndSession(sessions[1].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.VerifyRequestApproval(request.ID); err != nil {
		t.Fatal(err)
	}
	if approved, rejected, err := database.CheckRequestApprovalStatus(request.ID); err != nil || !approved || rejected {
		t.Fatalf("advisory status lost same-host review: %v %v %v", approved, rejected, err)
	}
	now := time.Now().UTC()
	execution := &Execution{ExecutedAt: &now, ExecutedBySessionID: sessions[0].ID,
		ExecutedByAgent: sessions[0].AgentName, ExecutedByModel: sessions[0].Model, LogPath: "same-host.log",
		ApprovalPolicy: ApprovalPolicyIndependent, Assurance: AssuranceIndependent}
	if err := database.ClaimRequestExecutionAuthenticated(request, execution, sessions[0].SessionKey); err != nil {
		t.Fatal(err)
	}
	if execution.ApprovalPolicy != ApprovalPolicySameHost || execution.Assurance != AssuranceSameHost {
		t.Fatal("caller-supplied stronger execution assurance was trusted")
	}
	code := 0
	execution.ExitCode = &code
	execution.ApprovalPolicy, execution.Assurance = ApprovalPolicyIndependent, AssuranceIndependent
	if err := database.CompleteRequestExecutionAuthenticated(request.ID, StatusExecuted, execution, sessions[0].SessionKey); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil || stored.Execution == nil || stored.Execution.ApprovalPolicy != ApprovalPolicySameHost || stored.Execution.Assurance != AssuranceSameHost {
		t.Fatalf("completion relabeled verified execution: %+v %v", stored, err)
	}
}

func TestSameHostReviewIntegrationRequiresBothOptIns(t *testing.T) {
	for _, tc := range []struct {
		name          string
		requestPolicy ApprovalPolicy
		reviewPolicy  ApprovalPolicy
		want          error
	}{
		{"secure default", "", "", ErrIndependentIdentityUnavailable},
		{"historical request after opt in", "", ApprovalPolicySameHost, ErrReviewPolicyMismatch},
		{"opt in revoked", ApprovalPolicySameHost, "", ErrIndependentIdentityUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, request, sessions := reviewDecisionFixture(t, 1, tc.requestPolicy)
			out, err := applyFixtureReview(database, request, sessions[1], DecisionApprove, ReviewPolicy{ApprovalPolicy: tc.reviewPolicy})
			if out != nil || !errors.Is(err, tc.want) {
				t.Fatalf("unintended policy accepted: %+v %v", out, err)
			}
			a, r, err := database.CountReviewsByDecision(request.ID)
			if err != nil || a+r != 0 {
				t.Fatalf("denied opt-in persisted a review: %d/%d %v", a, r, err)
			}
		})
	}
}

func TestSameHostReviewIntegrationSelfAndModelRestrictions(t *testing.T) {
	for _, mode := range []string{"same session", "trusted self", "restarted requester", "same model"} {
		t.Run(mode, func(t *testing.T) {
			database, request, sessions := reviewDecisionFixture(t, 1, ApprovalPolicySameHost)
			policy := ReviewPolicy{ApprovalPolicy: ApprovalPolicySameHost}
			reviewer, want := sessions[0], ErrSelfReview
			switch mode {
			case "trusted self":
				policy.TrustedSelfApprove = []string{reviewer.AgentName}
			case "restarted requester":
				if err := database.EndSession(reviewer.ID); err != nil {
					t.Fatal(err)
				}
				reviewer = &Session{AgentName: reviewer.AgentName, ProjectPath: request.ProjectPath, Model: "other"}
				if err := database.CreateSession(reviewer); err != nil {
					t.Fatal(err)
				}
			case "same model":
				reviewer, want = sessions[1], ErrReviewDifferentModel
				journalExec(t, database, `UPDATE requests SET require_different_model = 1, requestor_model = ? WHERE id = ?`, reviewer.Model, request.ID)
			}
			if out, err := applyFixtureReview(database, request, reviewer, DecisionApprove, policy); out != nil || !errors.Is(err, want) {
				t.Fatalf("restriction bypassed: %+v %v, want %v", out, err, want)
			}
		})
	}
}

func TestSameHostReviewIntegrationSignedSubmission(t *testing.T) {
	for _, mode := range []string{"valid", "wrong key", "invalid signature", "stale", "future", "legacy signature"} {
		t.Run(mode, func(t *testing.T) {
			database, request, sessions := reviewDecisionFixture(t, 1, ApprovalPolicySameHost)
			reviewer := sessions[1]
			at := time.Now().UTC().Truncate(time.Second)
			if mode == "stale" {
				at = at.Add(-6 * time.Minute)
			} else if mode == "future" {
				at = at.Add(2 * time.Minute)
			}
			vote := &Review{RequestID: request.ID, ReviewerSessionID: reviewer.ID,
				ReviewerAgent: reviewer.AgentName, ReviewerModel: reviewer.Model, Decision: DecisionApprove,
				ApprovalPolicy: ApprovalPolicySameHost, Assurance: AssuranceSameHost, CommandHash: request.Command.Hash,
				RequestBinding: reviewRequestBinding(request), SignatureVersion: 2, SignatureTimestamp: at}
			digest, _ := SessionKeyDigest(reviewer.SessionKey)
			vote.Signature = signReviewEvidenceWithDigest(digest, vote)
			key, want := reviewer.SessionKey, ErrInvalidSignature
			switch mode {
			case "wrong key":
				key, want = strings.Repeat("ab", 32), ErrReviewSessionKeyMismatch
			case "invalid signature":
				vote.Signature = strings.Repeat("0", 64)
			case "legacy signature":
				vote.Signature = ComputeReviewSignature(key, request.ID, vote.Decision, at)
			}
			out, err := database.ApplyReview(vote, key, ReviewPolicy{ApprovalPolicy: ApprovalPolicySameHost})
			if mode == "valid" {
				if err != nil || out.Request.Status != StatusApproved {
					t.Fatalf("valid same-host signature rejected: %+v %v", out, err)
				}
			} else if out != nil || !errors.Is(err, want) {
				t.Fatalf("invalid submission accepted: %+v %v, want %v", out, err, want)
			}
		})
	}
}

func TestSameHostExecutionIntegrationRechecksEvidence(t *testing.T) {
	for _, tc := range []struct{ name, query string }{
		{"invalid signature", `UPDATE reviews SET signature = 'invalid'`},
		{"expired", `UPDATE requests SET approval_expires_at = '2000-01-01T00:00:00Z'`},
		{"missing expiration", `UPDATE requests SET approval_expires_at = NULL`},
		{"command mutation", `UPDATE requests SET command_raw = 'echo changed'`},
		{"changed quorum", `UPDATE requests SET min_approvals = 2`},
		{"changed model policy", `UPDATE requests SET require_different_model = 1`},
		{"changed justification", `UPDATE requests SET justification_reason = 'different purpose'`},
		{"unknown policy", `UPDATE requests SET approval_policy = 'unknown'`},
		{"request promotion", `UPDATE requests SET approval_policy = 'independent'`},
		{"review promotion", `UPDATE reviews SET assurance = 'independent'`},
		{"missing assurance", `UPDATE reviews SET assurance = ''`},
		{"missing policy", `UPDATE reviews SET approval_policy = ''`},
		{"signature downgrade", `UPDATE reviews SET signature_version = 1`},
		{"missing command binding", `UPDATE reviews SET command_hash = ''`},
		{"changed requester", `UPDATE requests SET requestor_agent = 'SomeoneElse'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, request, sessions := reviewDecisionFixture(t, 1, ApprovalPolicySameHost)
			out, err := applyFixtureReview(database, request, sessions[1], DecisionApprove, ReviewPolicy{ApprovalPolicy: ApprovalPolicySameHost})
			if err != nil {
				t.Fatal(err)
			}
			request = out.Request
			if _, err := database.VerifyRequestApproval(request.ID); err != nil {
				t.Fatal(err)
			}
			journalExec(t, database, tc.query)
			if _, err := database.VerifyRequestApproval(request.ID); !errors.Is(err, ErrInvalidApprovalProof) {
				t.Fatalf("changed evidence passed advisory verification: %v", err)
			}
			now := time.Now().UTC()
			exec := &Execution{ExecutedAt: &now, ExecutedBySessionID: sessions[0].ID,
				ExecutedByAgent: sessions[0].AgentName, ExecutedByModel: sessions[0].Model, LogPath: "denied.log"}
			if err := database.ClaimRequestExecutionAuthenticated(request, exec, sessions[0].SessionKey); !errors.Is(err, ErrInvalidApprovalProof) {
				t.Fatalf("changed evidence authorized execution: %v", err)
			}
			assertNoExecutionClaim(t, database, request.ID)
		})
	}
}

func TestSameHostExecutionIntegrationCommandAndBindingMutation(t *testing.T) {
	for _, rewriteBinding := range []bool{false, true} {
		t.Run(fmt.Sprintf("rewrite_binding_%v", rewriteBinding), func(t *testing.T) {
			database, request, sessions := reviewDecisionFixture(t, 1, ApprovalPolicySameHost)
			out, err := applyFixtureReview(database, request, sessions[1], DecisionApprove, ReviewPolicy{ApprovalPolicy: ApprovalPolicySameHost})
			if err != nil {
				t.Fatal(err)
			}
			changed := *out.Request
			changed.Command.Raw = "echo changed"
			changed.Command.Hash = ComputeCommandHash(changed.Command)
			journalExec(t, database, `UPDATE requests SET command_raw = ?, command_hash = ? WHERE id = ?`, changed.Command.Raw, changed.Command.Hash, changed.ID)
			if rewriteBinding {
				journalExec(t, database, `UPDATE reviews SET command_hash = ?, request_binding = ? WHERE request_id = ?`, changed.Command.Hash, reviewRequestBinding(&changed), changed.ID)
			}
			if _, err := database.VerifyRequestApproval(request.ID); !errors.Is(err, ErrInvalidApprovalProof) {
				t.Fatalf("new command reused old signature: %v", err)
			}
		})
	}
}

func TestSameHostEvidenceNeverBecomesIndependent(t *testing.T) {
	// Even if a genuine authority is added later, a low-assurance signature
	// must not be reusable as that authority's evidence.
	SimulateIndependentIdentityForTest(t)
	for _, relabelEvidence := range []bool{false, true} {
		t.Run(fmt.Sprintf("relabel_evidence_%v", relabelEvidence), func(t *testing.T) {
			database, request, sessions := reviewDecisionFixture(t, 1, ApprovalPolicySameHost)
			if _, err := applyFixtureReview(database, request, sessions[1], DecisionApprove, ReviewPolicy{ApprovalPolicy: ApprovalPolicySameHost}); err != nil {
				t.Fatal(err)
			}
			journalExec(t, database, `UPDATE requests SET approval_policy = 'independent' WHERE id = ?`, request.ID)
			if relabelEvidence {
				journalExec(t, database, `UPDATE reviews SET approval_policy = 'independent', assurance = 'legacy_local',
					command_hash = '', request_binding = '', signature_version = 1 WHERE request_id = ?`, request.ID)
			}
			if _, err := database.VerifyRequestApproval(request.ID); !errors.Is(err, ErrInvalidApprovalProof) {
				t.Fatalf("low-assurance review promoted to independent authorization: %v", err)
			}
		})
	}
}

func TestSameHostReviewIntegrationCannotCountChangedPriorVote(t *testing.T) {
	database, request, sessions := reviewDecisionFixture(t, 2, ApprovalPolicySameHost)
	policy := ReviewPolicy{ApprovalPolicy: ApprovalPolicySameHost}
	if _, err := applyFixtureReview(database, request, sessions[1], DecisionApprove, policy); err != nil {
		t.Fatal(err)
	}
	request.Command.Raw = "echo different-command"
	request.Command.Hash = ComputeCommandHash(request.Command)
	journalExec(t, database, `UPDATE requests SET command_raw = ?, command_hash = ? WHERE id = ?`, request.Command.Raw, request.Command.Hash, request.ID)
	if out, err := applyFixtureReview(database, request, sessions[2], DecisionApprove, policy); out != nil || !errors.Is(err, ErrInvalidApprovalProof) {
		t.Fatalf("new vote laundered a previous command's approval: %+v %v", out, err)
	}
	stored, err := database.GetRequest(request.ID)
	a, r, countErr := database.CountReviewsByDecision(request.ID)
	if err != nil || countErr != nil || stored.Status != StatusPending || a != 1 || r != 0 {
		t.Fatalf("failed review did not roll back: %+v %d/%d %v %v", stored, a, r, err, countErr)
	}
}

func TestSameHostReviewIntegrationDistinctSessionsAndRejection(t *testing.T) {
	database, request, sessions := reviewDecisionFixture(t, 2, ApprovalPolicySameHost)
	policy := ReviewPolicy{ApprovalPolicy: ApprovalPolicySameHost}
	if _, err := applyFixtureReview(database, request, sessions[1], DecisionApprove, policy); err != nil {
		t.Fatal(err)
	}
	if err := database.EndSession(sessions[1].ID); err != nil {
		t.Fatal(err)
	}
	restarted := &Session{AgentName: sessions[1].AgentName, ProjectPath: request.ProjectPath, Model: "new-model"}
	if err := database.CreateSession(restarted); err != nil {
		t.Fatal(err)
	}
	if out, err := applyFixtureReview(database, request, restarted, DecisionApprove, policy); out != nil || !errors.Is(err, ErrReviewExists) {
		t.Fatalf("reviewer restart supplied another vote: %+v %v", out, err)
	}
	// Opting out stops future approvals, but must not prevent a reviewer from
	// rejecting a request. The rejection retains the request's low assurance.
	out, err := applyFixtureReview(database, request, sessions[2], DecisionReject, ReviewPolicy{})
	if err != nil || out.Request.Status != StatusRejected || out.Review.Assurance != AssuranceSameHost ||
		out.Review.ApprovalPolicy != ApprovalPolicySameHost || !VerifyReviewEvidenceSignature(out.Review, sessions[2].SessionKey) {
		t.Fatalf("rejection lost its evidence category: %+v %v", out, err)
	}
}
