package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func setupTestDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func createTestSession(t *testing.T, database *db.DB, id string) *db.Session {
	t.Helper()
	key := sha256.Sum256([]byte("notary-test-key-" + id))
	session := &db.Session{
		ID: id, AgentName: "Agent-" + id, Program: "test-cli", Model: "test-model",
		ProjectPath: "/test/project", SessionKey: hex.EncodeToString(key[:]),
	}
	if err := database.CreateSession(session); err != nil {
		t.Fatal(err)
	}
	return session
}

func createTestRequest(t *testing.T, database *db.DB, id, sessionID string, status db.RequestStatus, minApprovals int) *db.Request {
	t.Helper()
	session, err := database.GetSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(5 * time.Minute)
	request := &db.Request{
		ID: id, ProjectPath: session.ProjectPath,
		Command:  db.CommandSpec{Raw: "rm -rf /tmp/test", Argv: []string{"rm", "-rf", "/tmp/test"}, Cwd: "/tmp"},
		RiskTier: db.RiskTierDangerous, RequestorSessionID: sessionID,
		RequestorAgent: session.AgentName, RequestorModel: session.Model,
		Justification: db.Justification{Reason: "Test notary; no command is executed"},
		Status:        status, MinApprovals: minApprovals, ApprovalExpiresAt: &expires,
	}
	request.Command.Hash = db.ComputeCommandHash(request.Command)
	if err := database.CreateRequest(request); err != nil {
		t.Fatal(err)
	}
	return request
}

func createTestReview(t *testing.T, database *db.DB, requestID, sessionID string, decision db.Decision) *db.Review {
	t.Helper()
	session, err := database.GetSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	review := &db.Review{
		RequestID: requestID, ReviewerSessionID: sessionID,
		ReviewerAgent: session.AgentName, ReviewerModel: session.Model, Decision: decision,
		SignatureTimestamp: now, Signature: db.ComputeReviewSignature(session.SessionKey, requestID, decision, now),
	}
	if err := database.CreateReview(review); err != nil {
		t.Fatal(err)
	}
	return review
}

func notaryFixture(t *testing.T) (*db.DB, *Verifier, VerifyExecuteParams) {
	t.Helper()
	database := setupTestDB(t)
	requestor := createTestSession(t, database, "requestor")
	executor := createTestSession(t, database, "executor")
	reviewer := createTestSession(t, database, "reviewer")
	request := createTestRequest(t, database, "request", requestor.ID, db.StatusApproved, 1)
	createTestReview(t, database, request.ID, reviewer.ID, db.DecisionApprove)
	return database, NewVerifier(database), VerifyExecuteParams{
		RequestID: request.ID, SessionID: executor.ID, SessionKey: executor.SessionKey, CommandHash: request.Command.Hash,
	}
}

func notarySQL(t *testing.T, database *db.DB, query string, args ...any) {
	t.Helper()
	if _, err := database.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func requireNotaryClaim(t *testing.T, v *Verifier, p VerifyExecuteParams) *VerificationResult {
	t.Helper()
	result, err := v.VerifyAndMarkExecuting(p)
	if err != nil || result == nil || !result.Allowed || !validNotaryReceipt(result.ExecutionReceipt) {
		t.Fatalf("claim failed: result=%+v error=%v", result, err)
	}
	return result
}

func TestVerifier_AdvisoryAndAtomicClaim(t *testing.T) {
	database, v, p := notaryFixture(t)
	for i := 0; i < 2; i++ {
		result, err := v.VerifyExecutionAllowed(p)
		if err != nil || !result.Allowed || result.ExecutionReceipt != "" {
			t.Fatalf("advisory check: %+v %v", result, err)
		}
		if response := result.ToIPCResponse(); response.Allowed || response.CommandSpec != nil || response.Command != "" {
			t.Fatalf("advisory result became a permit: %+v", response)
		}
	}
	result := requireNotaryClaim(t, v, p)
	stored, err := database.GetRequest(p.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != db.StatusExecuting || stored.Execution == nil ||
		stored.Execution.ExecutedBySessionID != p.SessionID || stored.Execution.LogPath != result.ExecutionReceipt {
		t.Fatalf("claim identity not persisted: %+v", stored)
	}
	response := result.ToIPCResponse()
	if !response.Allowed || response.CommandSpec == nil || response.CommandHash != p.CommandHash ||
		response.CommandSpec.Cwd != "/tmp" || response.CommandSpec.Shell || len(response.CommandSpec.Argv) != 3 {
		t.Fatalf("incomplete command binding: %+v", response)
	}
	payload, err := json.Marshal(response)
	if err != nil || strings.Contains(string(payload), p.SessionKey) {
		t.Fatalf("response leaked credentials or failed encoding: %v", err)
	}
	response.CommandSpec.Argv[0] = "changed"
	if result.Request.Command.Argv[0] != "rm" {
		t.Fatal("response aliases approved argv")
	}
	second, err := v.VerifyAndMarkExecuting(p)
	if err != nil || second.Allowed || second.ToIPCResponse().Command != "" {
		t.Fatalf("claim replay accepted: %+v %v", second, err)
	}
}

func TestVerifier_RefusesInvalidClaims(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *db.DB, *VerifyExecuteParams)
	}{
		{"unknown session", func(_ *testing.T, _ *db.DB, p *VerifyExecuteParams) { p.SessionID = "missing" }},
		{"wrong key", func(_ *testing.T, _ *db.DB, p *VerifyExecuteParams) { p.SessionKey = strings.Repeat("f", 64) }},
		{"malformed key", func(_ *testing.T, _ *db.DB, p *VerifyExecuteParams) { p.SessionKey = "not-hex" }},
		{"wrong command", func(_ *testing.T, _ *db.DB, p *VerifyExecuteParams) { p.CommandHash = "wrong" }},
		{"ended session", func(t *testing.T, d *db.DB, p *VerifyExecuteParams) {
			notarySQL(t, d, "UPDATE sessions SET ended_at = ? WHERE id = ?", time.Now().UTC().Format(time.RFC3339), p.SessionID)
		}},
		{"other project", func(t *testing.T, d *db.DB, p *VerifyExecuteParams) {
			notarySQL(t, d, "UPDATE sessions SET project_path = '/other' WHERE id = ?", p.SessionID)
		}},
		{"no evidence", func(t *testing.T, d *db.DB, p *VerifyExecuteParams) {
			notarySQL(t, d, "DELETE FROM reviews WHERE request_id = ?", p.RequestID)
		}},
		{"forged review", func(t *testing.T, d *db.DB, p *VerifyExecuteParams) {
			notarySQL(t, d, "UPDATE reviews SET signature = 'forged' WHERE request_id = ?", p.RequestID)
		}},
		{"missing ttl", func(t *testing.T, d *db.DB, p *VerifyExecuteParams) {
			notarySQL(t, d, "UPDATE requests SET approval_expires_at = NULL WHERE id = ?", p.RequestID)
		}},
		{"expired ttl", func(t *testing.T, d *db.DB, p *VerifyExecuteParams) {
			notarySQL(t, d, "UPDATE requests SET approval_expires_at = ? WHERE id = ?", time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), p.RequestID)
		}},
		{"mutated command", func(t *testing.T, d *db.DB, p *VerifyExecuteParams) {
			notarySQL(t, d, "UPDATE requests SET command_raw = 'changed' WHERE id = ?", p.RequestID)
		}},
		{"quorum unmet", func(t *testing.T, d *db.DB, p *VerifyExecuteParams) {
			notarySQL(t, d, "UPDATE requests SET min_approvals = 2 WHERE id = ?", p.RequestID)
		}},
		{"model unmet", func(t *testing.T, d *db.DB, p *VerifyExecuteParams) {
			notarySQL(t, d, "UPDATE requests SET require_different_model = 1 WHERE id = ?", p.RequestID)
		}},
		{"revoked signer key", func(t *testing.T, d *db.DB, _ *VerifyExecuteParams) {
			notarySQL(t, d, "UPDATE sessions SET session_key = ? WHERE id = 'reviewer'", strings.Repeat("f", 64))
		}},
		{"new restrictive pattern", func(t *testing.T, d *db.DB, _ *VerifyExecuteParams) {
			if _, err := d.InsertCustomPattern("critical", `^rm\s+-rf`, "new restriction", "human"); err != nil {
				t.Fatal(err)
			}
		}},
		{"corrupt policy", func(t *testing.T, d *db.DB, _ *VerifyExecuteParams) {
			if _, err := d.InsertCustomPattern("bogus", `^rm`, "invalid tier", "human"); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			database, v, p := notaryFixture(t)
			tc.mutate(t, database, &p)
			result, err := v.VerifyAndMarkExecuting(p)
			if err != nil || result.Allowed || result.Reason == "" || result.Request != nil || result.ExecutionReceipt != "" {
				t.Fatalf("unsafe claim: %+v %v", result, err)
			}
			stored, err := database.GetRequest(p.RequestID)
			if err != nil || stored.Status != db.StatusApproved || stored.Execution != nil {
				t.Fatalf("refusal consumed authorization: %+v %v", stored, err)
			}
		})
	}
}

func TestVerifier_MissingClaimFields(t *testing.T) {
	_, v, valid := notaryFixture(t)
	for _, field := range []string{"request_id", "session_id", "session_key", "command_hash"} {
		t.Run(field, func(t *testing.T) {
			p := valid
			switch field {
			case "request_id":
				p.RequestID = ""
			case "session_id":
				p.SessionID = ""
			case "session_key":
				p.SessionKey = ""
			case "command_hash":
				p.CommandHash = ""
			}
			if _, err := v.VerifyAndMarkExecuting(p); err == nil || err.Error() != field+" is required" {
				t.Fatalf("missing %s: %v", field, err)
			}
		})
	}
}

func TestVerifier_ConcurrentClaims(t *testing.T) {
	var p VerifyExecuteParams
	// Independent DB handles exercise SQLite locking, not a Go mutex.
	path := filepath.Join(t.TempDir(), "race.db")
	other, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	requestor := createTestSession(t, other, "requestor")
	executor := createTestSession(t, other, "executor")
	reviewer := createTestSession(t, other, "reviewer")
	request := createTestRequest(t, other, "race", requestor.ID, db.StatusApproved, 1)
	createTestReview(t, other, request.ID, reviewer.ID, db.DecisionApprove)
	p.RequestID, p.SessionID, p.SessionKey, p.CommandHash = request.ID, executor.ID, executor.SessionKey, request.Command.Hash
	var winners atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		conn, err := db.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		wg.Add(1)
		go func(d *db.DB) {
			defer wg.Done()
			<-start
			result, err := NewVerifier(d).VerifyAndMarkExecuting(p)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if result.Allowed {
				winners.Add(1)
			}
		}(conn)
	}
	close(start)
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("got %d winners, want exactly one", winners.Load())
	}
}

func completionFor(p VerifyExecuteParams, result *VerificationResult) CompleteExecuteParams {
	zero, duration := 0, int64(23)
	return CompleteExecuteParams{RequestID: p.RequestID, SessionID: p.SessionID, SessionKey: p.SessionKey,
		ExecutionReceipt: result.ExecutionReceipt, Status: db.StatusExecuted, ExitCode: &zero, DurationMs: &duration}
}

func TestVerifier_CompletionOutcomes(t *testing.T) {
	for _, status := range []db.RequestStatus{db.StatusExecuted, db.StatusExecutionFailed, db.StatusTimedOut} {
		t.Run(string(status), func(t *testing.T) {
			database, v, p := notaryFixture(t)
			claim := requireNotaryClaim(t, v, p)
			report := completionFor(p, claim)
			report.Status = status
			if status != db.StatusExecuted {
				report.ExitCode = nil
			}
			if err := v.MarkExecutionComplete(report); err != nil {
				t.Fatal(err)
			}
			stored, err := database.GetRequest(p.RequestID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Status != status || stored.Execution.LogPath != claim.ExecutionReceipt ||
				stored.Execution.ExecutedByAgent != claim.Request.Execution.ExecutedByAgent ||
				stored.Execution.DurationMs == nil || *stored.Execution.DurationMs != 23 {
				t.Fatalf("incorrect outcome: %+v", stored)
			}
			if err := v.MarkExecutionComplete(report); err == nil {
				t.Fatal("completion replay accepted")
			}
			if next, err := v.VerifyAndMarkExecuting(p); err != nil || next.Allowed {
				t.Fatalf("terminal claim reissued: %+v %v", next, err)
			}
		})
	}
}

func TestVerifier_CompletionOwnershipAndValidation(t *testing.T) {
	for _, mode := range []string{"wrong key", "wrong receipt", "local log", "other session", "rotated key", "invalid status", "missing exit", "nonzero success", "negative duration"} {
		t.Run(mode, func(t *testing.T) {
			database, v, p := notaryFixture(t)
			claim := requireNotaryClaim(t, v, p)
			report := completionFor(p, claim)
			switch mode {
			case "wrong key":
				report.SessionKey = strings.Repeat("f", 64)
			case "wrong receipt":
				report.ExecutionReceipt = "notary:" + strings.Repeat("f", 64)
			case "local log":
				report.ExecutionReceipt = "/tmp/local.log"
			case "other session":
				other := createTestSession(t, database, "other")
				report.SessionID, report.SessionKey = other.ID, other.SessionKey
			case "rotated key":
				notarySQL(t, database, "UPDATE sessions SET session_key = ? WHERE id = ?", strings.Repeat("f", 64), p.SessionID)
			case "invalid status":
				report.Status = db.StatusApproved
			case "missing exit":
				report.ExitCode = nil
			case "nonzero success":
				code := 1
				report.ExitCode = &code
			case "negative duration":
				duration := int64(-1)
				report.DurationMs = &duration
			}
			if err := v.MarkExecutionComplete(report); err == nil {
				t.Fatalf("accepted %s", mode)
			}
			stored, err := database.GetRequest(p.RequestID)
			if err != nil || stored.Status != db.StatusExecuting || stored.Execution.ExitCode != nil {
				t.Fatalf("invalid report modified execution: %+v %v", stored, err)
			}
		})
	}
}

func TestVerifier_EndedSessionCanComplete(t *testing.T) {
	database, v, p := notaryFixture(t)
	claim := requireNotaryClaim(t, v, p)
	notarySQL(t, database, "UPDATE sessions SET ended_at = ? WHERE id = ?", time.Now().UTC().Format(time.RFC3339), p.SessionID)
	if err := v.MarkExecutionComplete(completionFor(p, claim)); err != nil {
		t.Fatal(err)
	}
}

func TestVerifier_DatabaseFailuresNeverAuthorize(t *testing.T) {
	for _, boundary := range []string{"claim", "completion"} {
		t.Run(boundary, func(t *testing.T) {
			database, v, p := notaryFixture(t)
			var claim *VerificationResult
			if boundary == "completion" {
				claim = requireNotaryClaim(t, v, p)
			}
			next := "executing"
			if boundary == "completion" {
				next = "executed"
			}
			notarySQL(t, database, fmt.Sprintf(`CREATE TRIGGER fail_notary BEFORE UPDATE OF status ON requests
			WHEN NEW.status = '%s' BEGIN SELECT RAISE(ABORT, 'injected persistence failure'); END`, next))
			if boundary == "claim" {
				result, err := v.VerifyAndMarkExecuting(p)
				if err == nil || result != nil {
					t.Fatalf("failed claim granted permission: %+v %v", result, err)
				}
			} else if err := v.MarkExecutionComplete(completionFor(p, claim)); err == nil {
				t.Fatal("failed completion reported success")
			}
			stored, err := database.GetRequest(p.RequestID)
			if err != nil {
				t.Fatal(err)
			}
			if boundary == "claim" && (stored.Status != db.StatusApproved || stored.Execution != nil) {
				t.Fatal("failed claim partially persisted")
			}
			if boundary == "completion" && (stored.Status != db.StatusExecuting || stored.Execution.ExitCode != nil) {
				t.Fatal("failed completion partially persisted")
			}
		})
	}
}
