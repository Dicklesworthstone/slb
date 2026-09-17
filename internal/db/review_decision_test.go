package db

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func reviewDecisionFixture(t *testing.T, quorum int) (*DB, *Request, []*Session) {
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
	if err := database.CreateRequest(r); err != nil {
		t.Fatal(err)
	}
	return database, r, sessions
}

func applyFixtureReview(database *DB, request *Request, session *Session, decision Decision, policy ReviewPolicy) (*ReviewOutcome, error) {
	return database.ApplyReview(&Review{RequestID: request.ID, ReviewerSessionID: session.ID, Decision: decision}, session.SessionKey, policy)
}

func TestApplyReviewPersistsApprovalDeadline(t *testing.T) {
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
	for _, tc := range []struct {
		name, query string
		want        error
	}{
		{"expired", `UPDATE requests SET expires_at = '2000-01-01T00:00:00Z'`, ErrReviewExpired},
		{"malformed expiry", `UPDATE requests SET expires_at = 'invalid'`, ErrReviewExpired},
		{"cancelled", `UPDATE requests SET status = 'cancelled'`, ErrReviewNotPending},
		{"executing", `UPDATE requests SET status = 'executing'`, ErrReviewNotPending},
		{"ended reviewer", `UPDATE sessions SET ended_at = '2000-01-01T00:00:00Z' WHERE id = 'review-session-1'`, ErrReviewSessionInactive},
		{"rotated key", `UPDATE sessions SET session_key = 'different' WHERE id = 'review-session-1'`, ErrReviewSessionKeyMismatch},
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

func TestApplyReviewConcurrentQuorumAcrossConnections(t *testing.T) {
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
