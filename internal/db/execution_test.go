package db

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func executionClaimFixture(t *testing.T) (*DB, *Request, *Execution) {
	t.Helper()
	project := t.TempDir()
	database, err := Open(filepath.Join(project, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	session := &Session{ID: "executor", AgentName: "Executor", Model: "model", Program: "test", ProjectPath: project}
	if err := database.CreateSession(session); err != nil {
		t.Fatal(err)
	}
	req := &Request{
		ProjectPath: project, RequestorSessionID: session.ID,
		RequestorAgent: session.AgentName, RequestorModel: session.Model,
		Command: CommandSpec{Raw: "echo approved", Argv: []string{"echo", "approved"}, Cwd: project},
		Status:  StatusApproved, RiskTier: RiskTierCaution,
		Justification: Justification{Reason: "Execution claim test"},
	}
	if err := database.CreateRequest(req); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	execution := &Execution{
		ExecutedAt: &now, ExecutedBySessionID: session.ID,
		ExecutedByAgent: session.AgentName, ExecutedByModel: session.Model,
		LogPath: filepath.Join(project, "unique-execution.log"),
	}
	return database, req, execution
}

func TestClaimRequestExecutionRejectsChangedEligibility(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
	}{
		{"cancelled", "UPDATE requests SET status = 'cancelled'"},
		{"raw command changed", "UPDATE requests SET command_raw = 'echo changed'"},
		{"cwd changed", "UPDATE requests SET command_cwd = '/elsewhere'"},
		{"argv changed", `UPDATE requests SET command_argv_json = '["echo","changed"]'`},
		{"shell changed", "UPDATE requests SET command_shell = 1"},
		{"hash changed", "UPDATE requests SET command_hash = 'changed'"},
		{"tier changed", "UPDATE requests SET risk_tier = 'critical'"},
		{"quorum changed", "UPDATE requests SET min_approvals = 3"},
		{"model requirement changed", "UPDATE requests SET require_different_model = 1"},
		{"expired", "UPDATE requests SET approval_expires_at = '2000-01-01T00:00:00Z'"},
		{"invalid expiry", "UPDATE requests SET approval_expires_at = 'not-a-timestamp'"},
		{"ended session", "UPDATE sessions SET ended_at = '2000-01-01T00:00:00Z'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, req, execution := executionClaimFixture(t)
			if _, err := database.Exec(tc.query); err != nil {
				t.Fatal(err)
			}
			if err := database.ClaimRequestExecution(req, execution); !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("expected rejected claim, got %v", err)
			}
			stored, err := database.GetRequest(req.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Status == StatusExecuting || stored.Execution != nil {
				t.Fatalf("rejected claim partially wrote execution: %+v", stored)
			}
		})
	}
}

func TestClaimRequestExecutionAuditFailureIsAtomic(t *testing.T) {
	database, req, execution := executionClaimFixture(t)
	req.Rollback = &Rollback{Path: filepath.Join(req.ProjectPath, "candidate-backup")}
	_, err := database.Exec(`CREATE TRIGGER refuse_claim BEFORE UPDATE OF execution_log_path ON requests
		BEGIN SELECT RAISE(ABORT, 'injected audit write failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.ClaimRequestExecution(req, execution); err == nil {
		t.Fatal("claim succeeded despite audit write failure")
	}
	stored, err := database.GetRequest(req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusApproved || stored.Execution != nil || stored.Rollback != nil {
		t.Fatalf("claim was not atomic: %+v", stored)
	}
}

func TestClaimRequestExecutionConcurrentSingleWinner(t *testing.T) {
	database, req, execution := executionClaimFixture(t)
	other, err := Open(filepath.Join(req.ProjectPath, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	connections := []*DB{database, other}
	const workers = 16
	start := make(chan struct{})
	results := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			candidate := *execution
			candidate.LogPath = fmt.Sprintf("%s-%d", execution.LogPath, i)
			<-start
			results <- connections[i%len(connections)].ClaimRequestExecution(req, &candidate)
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("unexpected claim error: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("got %d successful executors, want one", winners)
	}
	stored, err := database.GetRequest(req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusExecuting || stored.Execution == nil || stored.Execution.ExecutedBySessionID != execution.ExecutedBySessionID {
		t.Fatalf("winning claim lacks execution identity: %+v", stored)
	}
}

func TestClaimRequestExecutionRollbackBelongsToWinningClaim(t *testing.T) {
	database, req, execution := executionClaimFixture(t)
	other, err := Open(filepath.Join(req.ProjectPath, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	connections := []*DB{database, other}
	type claimResult struct {
		logPath, rollbackPath string
		err                   error
	}
	const workers = 12
	start := make(chan struct{})
	results := make(chan claimResult, workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			candidate := *req
			candidate.Rollback = &Rollback{Path: filepath.Join(req.ProjectPath, fmt.Sprintf("backup-%d", i))}
			attempt := *execution
			attempt.LogPath = fmt.Sprintf("%s-%d", execution.LogPath, i)
			<-start
			err := connections[i%len(connections)].ClaimRequestExecution(&candidate, &attempt)
			results <- claimResult{attempt.LogPath, candidate.Rollback.Path, err}
		}(i)
	}
	close(start)
	var winner claimResult
	winners := 0
	for i := 0; i < workers; i++ {
		result := <-results
		if result.err == nil {
			winner = result
			winners++
		} else if !errors.Is(result.err, ErrInvalidTransition) {
			t.Errorf("unexpected claim error: %v", result.err)
		}
	}
	if winners != 1 {
		t.Fatalf("got %d successful claims, want one", winners)
	}
	stored, err := database.GetRequest(req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Execution == nil || stored.Execution.LogPath != winner.logPath || stored.Rollback == nil || stored.Rollback.Path != winner.rollbackPath {
		t.Fatalf("rollback is not bound to the winning execution: execution=%+v rollback=%+v winner=%+v", stored.Execution, stored.Rollback, winner)
	}
	if err := database.UpdateRequestRollbackPath(req.ID, filepath.Join(req.ProjectPath, "late-loser-backup")); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a late capture overwrote the winning rollback: %v", err)
	}
	stored, err = database.GetRequest(req.ID)
	if err != nil || stored.Rollback == nil || stored.Rollback.Path != winner.rollbackPath {
		t.Fatalf("winning snapshot changed after a rejected update: %+v %v", stored, err)
	}
}

func TestClaimRequestExecutionClearsUncapturedRollback(t *testing.T) {
	database, req, execution := executionClaimFixture(t)
	if err := database.UpdateRequestRollbackPath(req.ID, filepath.Join(req.ProjectPath, "previous-preflight")); err != nil {
		t.Fatal(err)
	}
	// This attempt did not capture a snapshot. An old preflight must not be
	// advertised as recovery evidence for the execution that now wins.
	if err := database.ClaimRequestExecution(req, execution); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetRequest(req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Rollback != nil {
		t.Fatalf("uncaptured execution retained stale recovery evidence: %+v", stored.Rollback)
	}
}

func TestUpdateRequestRollbackPathRejectsClaimedAndTerminalRequests(t *testing.T) {
	for _, status := range []RequestStatus{StatusExecuting, StatusExecuted, StatusExecutionFailed, StatusTimedOut, StatusCancelled, StatusRejected} {
		t.Run(string(status), func(t *testing.T) {
			database, req, _ := executionClaimFixture(t)
			original := filepath.Join(req.ProjectPath, "winning-backup")
			if _, err := database.Exec(`UPDATE requests SET status = ?, rollback_path = ? WHERE id = ?`, status, original, req.ID); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRequestRollbackPath(req.ID, filepath.Join(req.ProjectPath, "late-backup")); !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("rollback update accepted after %s: %v", status, err)
			}
			stored, err := database.GetRequest(req.ID)
			if err != nil || stored.Rollback == nil || stored.Rollback.Path != original {
				t.Fatalf("execution recovery evidence was overwritten: %+v %v", stored, err)
			}
		})
	}
}

func TestCompleteRequestExecutionFencesAndPersistsOutcome(t *testing.T) {
	database, req, execution := executionClaimFixture(t)
	if err := database.ClaimRequestExecution(req, execution); err != nil {
		t.Fatal(err)
	}
	code, duration := -1, int64(1200)
	execution.ExitCode, execution.DurationMs = &code, &duration
	stale := *execution
	stale.LogPath += ".stale"
	if err := database.CompleteRequestExecution(req.ID, StatusTimedOut, &stale); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("stale executor completion accepted: %v", err)
	}
	if err := database.CompleteRequestExecution(req.ID, StatusTimedOut, execution); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetRequest(req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusTimedOut || stored.ResolvedAt == nil || stored.Execution.ExitCode == nil || *stored.Execution.ExitCode != code || stored.Execution.DurationMs == nil || *stored.Execution.DurationMs != duration {
		t.Fatalf("lost terminal outcome: %+v", stored)
	}
	if err := database.CompleteRequestExecution(req.ID, StatusExecutionFailed, execution); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("terminal outcome was overwritten: %v", err)
	}
}

func TestCompleteRequestExecutionWriteFailureIsAtomic(t *testing.T) {
	database, req, execution := executionClaimFixture(t)
	if err := database.ClaimRequestExecution(req, execution); err != nil {
		t.Fatal(err)
	}
	_, err := database.Exec(`CREATE TRIGGER refuse_outcome BEFORE UPDATE OF execution_exit_code ON requests
		BEGIN SELECT RAISE(ABORT, 'injected outcome write failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	code, duration := 0, int64(20)
	execution.ExitCode, execution.DurationMs = &code, &duration
	if err := database.CompleteRequestExecution(req.ID, StatusExecuted, execution); err == nil {
		t.Fatal("outcome write unexpectedly succeeded")
	}
	stored, err := database.GetRequest(req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusExecuting || stored.Execution.ExitCode != nil || stored.ResolvedAt != nil {
		t.Fatalf("outcome was partially persisted: %+v", stored)
	}
}
