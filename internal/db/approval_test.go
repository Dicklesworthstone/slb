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

func approvedEvidenceFixture(t *testing.T) (*DB, *Request, []*Session, *Execution) {
	t.Helper()
	database, request, sessions := reviewDecisionFixture(t, 2)
	for _, s := range sessions[1:3] {
		outcome, err := applyFixtureReview(database, request, s, DecisionApprove, ReviewPolicy{})
		if err != nil {
			t.Fatal(err)
		}
		request = outcome.Request
	}
	now := time.Now().UTC()
	execution := &Execution{ExecutedAt: &now, ExecutedBySessionID: sessions[0].ID,
		ExecutedByAgent: sessions[0].AgentName, ExecutedByModel: sessions[0].Model,
		LogPath: filepath.Join(request.ProjectPath, "claim.log")}
	return database, request, sessions, execution
}

func assertNoExecutionClaim(t *testing.T, database *DB, requestID string) {
	t.Helper()
	var status string
	var claims int
	if err := database.QueryRow(`SELECT status, (execution_log_path IS NOT NULL) + (execution_executed_at IS NOT NULL) FROM requests WHERE id = ?`, requestID).Scan(&status, &claims); err != nil {
		t.Fatal(err)
	}
	if status != "approved" || claims != 0 {
		t.Fatalf("denied claim changed state: status=%s claims=%d", status, claims)
	}
}

func TestExecutionRejectsMissingOrInvalidApprovalEvidence(t *testing.T) {
	for _, tc := range []struct{ name, query string }{
		{"deleted review", `DELETE FROM reviews WHERE reviewer_session_id = 'review-session-1'`},
		{"forged signature", `UPDATE reviews SET signature = 'forged' WHERE reviewer_session_id = 'review-session-1'`},
		{"malformed timestamp", `UPDATE reviews SET signature_timestamp = 'not-a-time'`},
		{"changed reviewer name", `UPDATE reviews SET reviewer_agent = 'NotTheSigner'`},
		{"changed reviewer model", `UPDATE reviews SET reviewer_model = 'NotTheModel'`},
		{"changed stored identity", `UPDATE sessions SET agent_name = 'Renamed' WHERE id = 'review-session-1'`},
		{"rotated key", `UPDATE sessions SET session_key = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' WHERE id = 'review-session-1'`},
		{"invalid key encoding", `UPDATE sessions SET session_key = 'not-hex' WHERE id = 'review-session-1'`},
		{"missing expiry", `UPDATE requests SET approval_expires_at = NULL`},
		{"expired", `UPDATE requests SET approval_expires_at = '2000-01-01T00:00:00Z'`},
		{"malformed expiry", `UPDATE requests SET approval_expires_at = 'not-a-time'`},
		{"larger quorum", `UPDATE requests SET min_approvals = 3`},
		{"negative quorum", `UPDATE requests SET min_approvals = -1`},
		{"zero dangerous quorum", `UPDATE requests SET min_approvals = 0`},
		{"unknown tier", `UPDATE requests SET risk_tier = 'unknown'`},
		{"invalid decision", `UPDATE reviews SET decision = 'maybe'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, request, _, execution := approvedEvidenceFixture(t)
			// The initial advisory check is deliberately before evidence changes.
			if _, err := database.VerifyRequestApproval(request.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := database.Exec(tc.query); err != nil {
				t.Fatal(err)
			}
			if _, err := database.VerifyRequestApproval(request.ID); !errors.Is(err, ErrInvalidApprovalProof) {
				t.Fatalf("advisory check accepted changed evidence: %v", err)
			}
			if err := database.ClaimRequestExecution(request, execution); !errors.Is(err, ErrInvalidApprovalProof) {
				t.Fatalf("execution accepted changed evidence: %v", err)
			}
			assertNoExecutionClaim(t, database, request.ID)
		})
	}
}

func TestExecutionCannotTrustApprovedStatusWithoutVotes(t *testing.T) {
	database, request, sessions := reviewDecisionFixture(t, 2)
	expiry := time.Now().Add(time.Hour)
	if _, err := database.Exec(`UPDATE requests SET status = 'approved', approval_expires_at = ?`, expiry.UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	request.Status = StatusApproved
	now := time.Now().UTC()
	execution := &Execution{ExecutedAt: &now, ExecutedBySessionID: sessions[0].ID,
		ExecutedByAgent: sessions[0].AgentName, ExecutedByModel: sessions[0].Model, LogPath: "no-proof.log"}
	if err := database.ClaimRequestExecution(request, execution); !errors.Is(err, ErrInvalidApprovalProof) || !strings.Contains(err.Error(), "insufficient authentic approvals") {
		t.Fatalf("status-only approval authorized execution: %v", err)
	}
	assertNoExecutionClaim(t, database, request.ID)
}

func TestExecutionCountsDistinctReviewerAgents(t *testing.T) {
	database, request, sessions, execution := approvedEvidenceFixture(t)
	// Simulate duplicate historical votes from one agent across two sessions.
	if _, err := database.Exec(`UPDATE sessions SET ended_at = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339), sessions[1].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE sessions SET agent_name = ? WHERE id = ?`, sessions[1].AgentName, sessions[2].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE reviews SET reviewer_agent = ? WHERE reviewer_session_id = ?`, sessions[1].AgentName, sessions[2].ID); err != nil {
		t.Fatal(err)
	}
	if err := database.ClaimRequestExecution(request, execution); !errors.Is(err, ErrInvalidApprovalProof) || !strings.Contains(err.Error(), "duplicate reviewer") {
		t.Fatalf("duplicate agent counted twice: %v", err)
	}
	assertNoExecutionClaim(t, database, request.ID)
}

func TestExecutionRechecksDifferentModelEvidence(t *testing.T) {
	database, request, sessions := reviewDecisionFixture(t, 1)
	outcome, err := applyFixtureReview(database, request, sessions[1], DecisionApprove, ReviewPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	request = outcome.Request
	if _, err := database.Exec(`UPDATE requests SET require_different_model = 1, requestor_model = ?`, sessions[1].Model); err != nil {
		t.Fatal(err)
	}
	if _, err := database.VerifyRequestApproval(request.ID); !errors.Is(err, ErrInvalidApprovalProof) || !strings.Contains(err.Error(), "different model") {
		t.Fatalf("accepted same-model-only proof: %v", err)
	}
}

func TestExecutionPreservesValidHistoricalApprovals(t *testing.T) {
	database, request, sessions, execution := approvedEvidenceFixture(t)
	signedAt := time.Now().UTC().Add(-15 * time.Minute).Truncate(time.Second)
	signature := ComputeReviewSignature(sessions[1].SessionKey, request.ID, DecisionApprove, signedAt)
	if _, err := database.Exec(`UPDATE reviews SET signature = ?, signature_timestamp = ? WHERE reviewer_session_id = ?`, signature, signedAt.Format(time.RFC3339), sessions[1].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE sessions SET ended_at = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339), sessions[1].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.VerifyRequestApproval(request.ID); err != nil {
		t.Fatalf("submission freshness was incorrectly reapplied at execution: %v", err)
	}
	if err := database.ClaimRequestExecution(request, execution); err != nil {
		t.Fatalf("valid historical approval was rejected: %v", err)
	}
}

func TestExecutionHonorsFirstWinsResolutionWithoutWeakeningQuorum(t *testing.T) {
	database, request, sessions := reviewDecisionFixture(t, 2)
	policy := ReviewPolicy{ConflictResolution: "first_wins"}
	for i, decision := range []Decision{DecisionApprove, DecisionReject, DecisionApprove} {
		outcome, err := applyFixtureReview(database, request, sessions[i+1], decision, policy)
		if err != nil {
			t.Fatal(err)
		}
		request = outcome.Request
	}
	if _, err := database.VerifyRequestApproval(request.ID); err != nil {
		t.Fatalf("proof check reinterpreted the committed conflict policy: %v", err)
	}
}

func TestExecutionRetainsZeroReviewCautionAutoApproval(t *testing.T) {
	database, request, sessions := reviewDecisionFixture(t, 0)
	if _, err := database.Exec(`UPDATE requests SET risk_tier = 'caution', status = 'approved'`); err != nil {
		t.Fatal(err)
	}
	request.RiskTier, request.Status = RiskTierCaution, StatusApproved
	now := time.Now().UTC()
	execution := &Execution{ExecutedAt: &now, ExecutedBySessionID: sessions[0].ID,
		ExecutedByAgent: sessions[0].AgentName, ExecutedByModel: sessions[0].Model, LogPath: "auto-approval.log"}
	if err := database.ClaimRequestExecution(request, execution); err != nil {
		t.Fatalf("explicit zero-review caution path was lost: %v", err)
	}
}

func TestExecutionEvidenceCheckAndClaimHaveOneWinner(t *testing.T) {
	database, request, _, execution := approvedEvidenceFixture(t)
	other, err := Open(filepath.Join(request.ProjectPath, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	connections := []*DB{database, other}
	start := make(chan struct{})
	errs := make(chan error, 16)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			candidate := *execution
			candidate.LogPath = fmt.Sprintf("%s-%d", execution.LogPath, i)
			<-start
			errs <- connections[i%2].ClaimRequestExecution(request, &candidate)
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	winners := 0
	for err := range errs {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("unexpected claim error: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("expected one verified execution, got %d", winners)
	}
	var session, path string
	if err := database.QueryRow(`SELECT execution_executed_by_session_id, execution_log_path FROM requests WHERE id = ?`, request.ID).Scan(&session, &path); err != nil {
		t.Fatal(err)
	}
	if session != execution.ExecutedBySessionID || !strings.HasPrefix(path, execution.LogPath+"-") {
		t.Fatalf("missing winning identity: %s %s", session, path)
	}
}

func TestExecutionRechecksEvidenceAfterConcurrentWriter(t *testing.T) {
	database, request, _, execution := approvedEvidenceFixture(t)
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
	if _, err := tx.Exec(`UPDATE reviews SET signature = 'changed-after-preflight'`); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		close(start)
		result <- other.ClaimRequestExecution(request, execution)
	}()
	<-start
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, ErrInvalidApprovalProof) {
			t.Fatalf("concurrent evidence change did not block claim: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("claim failed to finish after writer commit")
	}
	assertNoExecutionClaim(t, database, request.ID)
}
