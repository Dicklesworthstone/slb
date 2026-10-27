package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
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

// testSessionKey is the deterministic key createTestSession issues. The
// database keeps only its digest, so fixtures recompute it rather than read it.
func testSessionKey(id string) string {
	key := sha256.Sum256([]byte("notary-test-key-" + id))
	return hex.EncodeToString(key[:])
}

func createTestSession(t *testing.T, database *db.DB, id string) *db.Session {
	t.Helper()
	session := &db.Session{
		ID: id, AgentName: "Agent-" + id, Program: "test-cli", Model: "test-model",
		ProjectPath: "/test/project", SessionKey: testSessionKey(id),
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
		SignatureTimestamp: now, Signature: db.ComputeReviewSignature(testSessionKey(sessionID), requestID, decision, now),
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

func TestVerifierSameHostClaimLabelsAndCurrentPolicy(t *testing.T) {
	t.Setenv(db.SimulatedIdentityEnvForTest, "")
	t.Setenv("HOME", t.TempDir())
	for _, revoke := range []bool{false, true} {
		t.Run(fmt.Sprint("revoke=", revoke), func(t *testing.T) {
			project := t.TempDir()
			database, err := db.OpenAndMigrate(filepath.Join(project, ".slb", "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			configPath := filepath.Join(project, ".slb", "config.toml")
			if err := os.WriteFile(configPath, []byte("[general]\napproval_policy = \"same_host\"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			author := &db.Session{AgentName: "requester", Model: "model-a", ProjectPath: project}
			peer := &db.Session{AgentName: "reviewer", Model: "model-b", ProjectPath: project}
			for _, session := range []*db.Session{author, peer} {
				if err := database.CreateSession(session); err != nil {
					t.Fatal(err)
				}
			}
			request := &db.Request{ProjectPath: project, RequestorSessionID: author.ID,
				RequestorAgent: author.AgentName, RequestorModel: author.Model, ApprovalPolicy: db.ApprovalPolicySameHost,
				Command:  db.CommandSpec{Raw: "echo reviewed", Argv: []string{"echo", "reviewed"}, Cwd: project},
				RiskTier: db.RiskTierDangerous, MinApprovals: 1, Justification: db.Justification{Reason: "test claim only"}}
			if err := database.CreateRequest(request); err != nil {
				t.Fatal(err)
			}
			_, err = database.ApplyReview(&db.Review{RequestID: request.ID, ReviewerSessionID: peer.ID, Decision: db.DecisionApprove},
				peer.SessionKey, db.ReviewPolicy{ApprovalPolicy: db.ApprovalPolicySameHost})
			if err != nil {
				t.Fatal(err)
			}
			stagedPath := filepath.Join(project, "previous-preflight")
			stagedAt := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
			if err := database.UpdateRequestRollbackPath(request.ID, stagedPath); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRequestRolledBackAt(request.ID, stagedAt); err != nil {
				t.Fatal(err)
			}
			v := NewVerifier(database)
			params := VerifyExecuteParams{RequestID: request.ID, SessionID: author.ID,
				SessionKey: author.SessionKey, CommandHash: request.Command.Hash}
			advisory, err := v.VerifyExecutionAllowed(params)
			if err != nil || !advisory.Allowed {
				t.Fatalf("intentional workflow refused: %+v %v", advisory, err)
			}
			if revoke {
				if err := os.WriteFile(configPath, []byte("[general]\napproval_policy = \"independent\"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			claim, err := v.VerifyAndMarkExecuting(params)
			if revoke {
				if err != nil || claim == nil || claim.Allowed || !strings.Contains(claim.Reason, "approval policy") {
					t.Fatalf("notary ignored current policy: %+v %v", claim, err)
				}
				stored, err := database.GetRequest(request.ID)
				if err != nil || stored.Status != db.StatusApproved || stored.Execution != nil || stored.Rollback == nil ||
					stored.Rollback.Path != stagedPath || stored.Rollback.RolledBackAt == nil || !stored.Rollback.RolledBackAt.Equal(stagedAt) {
					t.Fatalf("refused notary claim changed recovery metadata: %+v %v", stored, err)
				}
				return
			}
			if err != nil || claim == nil || !claim.Allowed || claim.Request == nil {
				t.Fatalf("claim failed: %+v %v", claim, err)
			}
			if claim.Request.Rollback != nil {
				t.Fatalf("notary advertised a previous preflight as its own capture: %+v", claim.Request.Rollback)
			}
			claimed, err := database.GetRequest(request.ID)
			if err != nil || claimed.Status != db.StatusExecuting || claimed.Rollback != nil {
				t.Fatalf("notary retained recovery metadata without capturing state: %+v %v", claimed, err)
			}
			response := claim.ToIPCResponse()
			if response.ApprovalPolicy != db.ApprovalPolicySameHost || response.Assurance != db.AssuranceSameHost ||
				!strings.Contains(response.ReviewNotice, "not independently authenticated authorization") {
				t.Fatalf("notary promoted or concealed local evidence: %+v", response)
			}
			// A later opt-out revokes new execution claims, but completion must
			// keep the policy captured by this already committed claim.
			if err := os.WriteFile(configPath, []byte("[general]\napproval_policy = 'independent'\n"), 0600); err != nil {
				t.Fatal(err)
			}
			zero := 0
			completion, err := json.Marshal(CompleteExecuteParams{RequestID: request.ID,
				SessionID: author.ID, SessionKey: author.SessionKey, ExecutionReceipt: claim.ExecutionReceipt,
				Status: db.StatusExecuted, ExitCode: &zero})
			if err != nil {
				t.Fatal(err)
			}
			completed := (&IPCServer{verifier: v}).handleCompleteExecute(RPCRequest{Params: completion, ID: 31})
			if completed.Error != nil {
				t.Fatalf("completion after opt-out failed: %+v", completed.Error)
			}
			receipt, ok := completed.Result.(map[string]any)
			if !ok || receipt["approval_policy"] != db.ApprovalPolicySameHost || receipt["assurance"] != db.AssuranceSameHost || receipt["review_notice"] != db.ApprovalPolicySameHost.Label() {
				t.Fatalf("completion receipt reinterpreted historical execution: %+v", completed.Result)
			}
			stored, err := database.GetRequest(request.ID)
			if err != nil || stored.Status != db.StatusExecuted || stored.Execution == nil || stored.Execution.Assurance != db.AssuranceSameHost || stored.Execution.ApprovalPolicy != db.ApprovalPolicySameHost || stored.Rollback != nil {
				t.Fatalf("completed execution lost captured evidence category: %+v %v", stored, err)
			}
		})
	}
}

func TestVerifier_AdvisoryAndAtomicClaim(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
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

func TestVerifierRechecksExplicitPolicyForOwnedProject(t *testing.T) {
	database, request := pendingTimerFixture(t)
	handler := NewTimeoutHandler(database, TimeoutHandlerConfig{})
	if err := handler.advance(context.Background(), request.ID, "", false); err != nil {
		t.Fatal(err)
	}
	// The fixture's requester key is deliberately available only at creation;
	// authenticate the real issued key through a separate executor session.
	executor := &db.Session{AgentName: "executor", Model: "test", ProjectPath: request.ProjectPath}
	if err := database.CreateSession(executor); err != nil {
		t.Fatal(err)
	}
	params := VerifyExecuteParams{RequestID: request.ID, SessionID: executor.ID,
		SessionKey: executor.SessionKey, CommandHash: request.Command.Hash}
	v := NewVerifier(database)
	if advisory, err := v.VerifyExecutionAllowed(params); err != nil || !advisory.Allowed {
		t.Fatalf("baseline automatic approval refused: %+v %v", advisory, err)
	}
	configPath := filepath.Join(t.TempDir(), "execution policy.toml")
	if err := os.WriteFile(configPath, []byte("[patterns.caution]\nmin_approvals = 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	v.projectPath, v.configPath = request.ProjectPath, configPath
	if claim, err := v.VerifyAndMarkExecuting(params); err != nil || claim.Allowed || !strings.Contains(claim.Reason, "approval") {
		t.Fatalf("claim bypassed selected quorum: %+v %v", claim, err)
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil || stored.Status != db.StatusApproved || stored.Execution != nil {
		t.Fatalf("denied claim changed execution: %+v %v", stored, err)
	}
	if err := os.WriteFile(configPath, []byte("[patterns.caution]\nmin_approvals = 0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if advisory, err := v.VerifyExecutionAllowed(params); err != nil || !advisory.Allowed {
		t.Fatalf("current selected policy not reloaded: %+v %v", advisory, err)
	}
	if err := os.WriteFile(configPath, []byte("[patterns.caution]\nmin_approvals = 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if claim, err := v.VerifyAndMarkExecuting(params); err != nil || claim.Allowed {
		t.Fatalf("claim reused earlier advisory policy: %+v %v", claim, err)
	}
	// One daemon's override must never apply to requests belonging to another
	// project, even if an administrative database contains both projects.
	v.projectPath = t.TempDir()
	if advisory, err := v.VerifyExecutionAllowed(params); err != nil || !advisory.Allowed {
		t.Fatalf("daemon override leaked into another project: %+v %v", advisory, err)
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
			notarySQL(t, d, "UPDATE sessions SET session_key_digest = ? WHERE id = 'reviewer'", strings.Repeat("f", 64))
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
	db.SimulateIndependentIdentityForTest(t)
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
	db.SimulateIndependentIdentityForTest(t)
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
	db.SimulateIndependentIdentityForTest(t)
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
				notarySQL(t, database, "UPDATE sessions SET session_key_digest = ? WHERE id = ?", strings.Repeat("f", 64), p.SessionID)
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
	db.SimulateIndependentIdentityForTest(t)
	database, v, p := notaryFixture(t)
	claim := requireNotaryClaim(t, v, p)
	notarySQL(t, database, "UPDATE sessions SET ended_at = ? WHERE id = ?", time.Now().UTC().Format(time.RFC3339), p.SessionID)
	if err := v.MarkExecutionComplete(completionFor(p, claim)); err != nil {
		t.Fatal(err)
	}
}

func TestVerifier_DatabaseFailuresNeverAuthorize(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
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
