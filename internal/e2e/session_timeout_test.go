// Package e2e contains end-to-end integration tests for SLB workflows.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/daemon"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/testutil"
)

// A timeout escalates unresolved work; it does not hide it from cooperating
// reviewers or strand the requester's immutable session identity forever.
func TestBuiltCLIEscalatedRequestRecoveryIntegration(t *testing.T) {
	binary := buildIdentityBoundaryCLI(t)
	f := newSameHostCLI(t, binary, 2, true)
	f.json("config", "set", "general.request_timeout", "1")
	requester := f.session("Requester", "model-a")
	reviewer := f.session("Reviewer", "model-b")
	second := f.session("SecondReviewer", "model-c")
	escalate := func(command string) *db.Request {
		t.Helper()
		view := f.json("request", command, "--session-id", requester.ID,
			"--reason", "Test recovery of timed-out review", "--expected-effect", "Create an isolated marker",
			"--safety", "Harmless command in a temporary project", "--wait", "--timeout", "10")
		id, ok := view["request_id"].(string)
		if !ok || view["status"] != "escalated" || view["approval_policy"] != "same_host" {
			t.Fatalf("request did not escalate under its own deadline: %+v", view)
		}
		request, err := f.DB.GetRequest(id)
		if err != nil || request.Status != db.StatusEscalated || request.MinApprovals != 2 || !request.RequireDifferentModel {
			t.Fatalf("escalation weakened review requirements: %+v %v", request, err)
		}
		return request
	}
	queueContains := func(id string, want bool, args ...string) {
		t.Helper()
		stdout, stderr, err := f.run(append(args, "--json")...)
		var rows []map[string]any
		if err != nil || json.Unmarshal([]byte(stdout), &rows) != nil {
			t.Fatalf("reviewer queue failed: %v\n%s\n%s", err, stdout, stderr)
		}
		found := false
		for _, row := range rows {
			if row["request_id"] == id || row["id"] == id {
				found = true
				if row["status"] != "escalated" || row["approval_policy"] != "same_host" {
					t.Fatalf("queue obscured escalated low-assurance status: %+v", row)
				}
			}
		}
		if found != want {
			t.Fatalf("queue %v includes request %s: got %t, want %t (%s)", args, id, found, want, stdout)
		}
	}
	queues := [][]string{
		{"pending"}, {"review", "list"},
		{"pending", "--review-pool", "--session-id", second.ID},
		{"review", "list", "--review-pool", "--session-id", second.ID},
	}
	approved := escalate("touch recovered-escalation-marker")
	for _, args := range queues {
		queueContains(approved.ID, true, args...)
	}
	f.fails("cannot review your own request", "approve", approved.ID,
		"--session-id", requester.ID, "--session-key", requester.SessionKey)
	f.approve(approved, reviewer)
	partial, err := f.DB.GetRequest(approved.ID)
	if err != nil || partial.Status != db.StatusEscalated || partial.ApprovalExpiresAt != nil {
		t.Fatalf("one vote bypassed quorum on an escalated request: %+v %v", partial, err)
	}
	queueContains(approved.ID, true, "pending")
	f.approve(approved, second)
	result := f.json("execute", approved.ID, "--session-id", requester.ID)
	if result["status"] != "executed" || result["exit_code"] != float64(0) || result["assurance"] != "same_host" {
		t.Fatalf("reviewed escalated request could not execute: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(f.ProjectDir, "recovered-escalation-marker")); err != nil {
		t.Fatal(err)
	}
	queueContains(approved.ID, false, "pending")

	withdrawn := escalate("touch withdrawn-escalation-marker")
	f.approve(withdrawn, reviewer)
	before, reviews, err := f.DB.GetRequestWithReviews(withdrawn.ID)
	if err != nil || before.Status != db.StatusEscalated || len(reviews) != 1 || reviews[0].Assurance != db.AssuranceSameHost {
		t.Fatalf("partial review evidence missing before withdrawal: %+v %+v %v", before, reviews, err)
	}
	f.fails("session has outstanding requests", "session", "resume", "--agent", requester.AgentName,
		"--session-id", requester.ID, "--session-key", requester.SessionKey, "--model", "replacement-model")
	f.fails("not the requestor", "cancel", withdrawn.ID, "--session-id", second.ID)
	cancelled := f.json("cancel", withdrawn.ID, "--session-id", requester.ID)
	if cancelled["status"] != "cancelled" || cancelled["approval_policy"] != "same_host" {
		t.Fatalf("withdrawal lost status or policy: %+v", cancelled)
	}
	stored, afterReviews, err := f.DB.GetRequestWithReviews(withdrawn.ID)
	if err != nil || stored.Status != db.StatusCancelled || stored.Execution != nil || stored.ResolvedAt == nil ||
		stored.ApprovalPolicy != db.ApprovalPolicySameHost || stored.Command.Hash != before.Command.Hash ||
		!reflect.DeepEqual(reviews, afterReviews) {
		t.Fatalf("withdrawal changed immutable evidence: %+v %+v %v", stored, afterReviews, err)
	}
	page, err := f.DB.ReadRequestEvents(context.Background(), f.ProjectDir, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	var cancellationEvents int
	for _, event := range page.Events {
		if event.RequestID == withdrawn.ID && event.State.Status == "cancelled" {
			cancellationEvents++
			if event.PreviousStatus == nil || *event.PreviousStatus != "escalated" ||
				event.State.ApprovalPolicy != db.ApprovalPolicySameHost || event.State.Approvals != 1 ||
				event.State.ExecutionAssurance != db.AssuranceLegacyUnknown || event.State.ExecutorSessionID != nil ||
				event.State.ExecutedAt != nil || event.State.CommandHash != before.Command.Hash {
				t.Fatalf("withdrawal journal lost original review provenance: %+v", event)
			}
		}
	}
	if cancellationEvents != 1 {
		t.Fatalf("withdrawal should create one durable event, got %d", cancellationEvents)
	}
	for _, args := range queues {
		queueContains(withdrawn.ID, false, args...)
	}
	f.fails("cancelled", "approve", withdrawn.ID, "--session-id", second.ID, "--session-key", second.SessionKey)
	f.fails("cancelled", "execute", withdrawn.ID, "--session-id", requester.ID)
	if _, err := os.Stat(filepath.Join(f.ProjectDir, "withdrawn-escalation-marker")); !os.IsNotExist(err) {
		t.Fatalf("withdrawn command executed: %v", err)
	}
	rotated := f.json("session", "resume", "--agent", requester.AgentName,
		"--session-id", requester.ID, "--session-key", requester.SessionKey, "--model", "replacement-model")
	if rotated["session_id"] == requester.ID || rotated["model"] != "replacement-model" {
		t.Fatalf("resolved escalation still strands requester identity: %+v", rotated)
	}
}

// TestSessionHeartbeat tests that session heartbeats update last_active_at.
func TestSessionHeartbeat(t *testing.T) {
	h := testutil.NewHarness(t)

	t.Log("=== TestSessionHeartbeat ===")
	t.Logf("ENV: temp_db=%s", h.DBPath)

	// Step 1: Create session
	t.Log("STEP 1: Creating session")
	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("heartbeat-agent"),
		testutil.WithModel("test-model"),
	)
	t.Logf("  Session created: %s", sess.ID)

	// Step 2: Verify initial last_active_at
	t.Log("STEP 2: Verifying initial last_active_at")
	initialSession, err := h.DB.GetSession(sess.ID)
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}
	initialLastActive := initialSession.LastActiveAt
	t.Logf("  Initial last_active: %s", initialLastActive.Format(time.RFC3339))

	// Step 3: Wait at least 1 second (RFC3339 has second-level precision) and send heartbeat
	t.Log("STEP 3: Waiting 1.1s and sending heartbeat")
	time.Sleep(1100 * time.Millisecond)

	err = h.DB.UpdateSessionHeartbeat(sess.ID)
	if err != nil {
		t.Fatalf("UpdateSessionHeartbeat failed: %v", err)
	}
	t.Log("  Heartbeat sent")

	// Step 4: Verify last_active_at updated
	t.Log("STEP 4: Verifying last_active_at updated")
	updatedSession, err := h.DB.GetSession(sess.ID)
	if err != nil {
		t.Fatalf("GetSession after heartbeat failed: %v", err)
	}

	// Compare truncated to second precision since that's what the DB stores
	if !updatedSession.LastActiveAt.Truncate(time.Second).After(initialLastActive.Truncate(time.Second)) {
		t.Errorf("last_active_at not updated: initial=%s, after=%s",
			initialLastActive.Format(time.RFC3339),
			updatedSession.LastActiveAt.Format(time.RFC3339))
	}
	t.Logf("  last_active updated: %s", updatedSession.LastActiveAt.Format(time.RFC3339))

	// Step 5: Verify session still active
	t.Log("STEP 5: Verifying session still active")
	if updatedSession.EndedAt != nil {
		t.Error("Session should still be active (EndedAt should be nil)")
	}
	t.Log("  Session still active")

	t.Log("=== PASS: TestSessionHeartbeat ===")
}

// TestStaleSessionDetection tests that sessions without heartbeats are detected as stale.
func TestStaleSessionDetection(t *testing.T) {
	h := testutil.NewHarness(t)

	t.Log("=== TestStaleSessionDetection ===")
	t.Logf("ENV: temp_db=%s", h.DBPath)

	// Step 1: Create session
	t.Log("STEP 1: Creating session")
	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("stale-agent"),
		testutil.WithModel("test-model"),
	)
	t.Logf("  Session created: %s", sess.ID)

	// Step 2: Use a threshold that works with RFC3339 second-level precision
	// Note: FindStaleSessions uses RFC3339 string comparison (second precision)
	staleThreshold := 2 * time.Second
	t.Logf("STEP 2: Using stale threshold: %v", staleThreshold)

	// Step 3: Verify not stale initially
	t.Log("STEP 3: Verifying session not stale initially")
	staleSessions, err := h.DB.FindStaleSessions(staleThreshold)
	if err != nil {
		t.Fatalf("FindStaleSessions failed: %v", err)
	}
	if containsSession(staleSessions, sess.ID) {
		t.Error("Session should not be stale immediately after creation")
	}
	t.Log("  Session not stale initially")

	// Step 4: Wait for session to become stale (3s to cross the 2s threshold boundary)
	t.Log("STEP 4: Waiting for session to become stale")
	time.Sleep(3 * time.Second)

	// Step 5: Verify session detected as stale
	t.Log("STEP 5: Checking for stale sessions")
	staleSessions, err = h.DB.FindStaleSessions(staleThreshold)
	if err != nil {
		t.Fatalf("FindStaleSessions failed: %v", err)
	}
	if !containsSession(staleSessions, sess.ID) {
		t.Error("Session should be detected as stale")
	}
	t.Logf("  Session marked stale: %s", sess.ID)

	// Step 6: End the stale session and verify new session can be created
	t.Log("STEP 6: Ending stale session and creating new one")
	err = h.DB.EndSession(sess.ID)
	if err != nil {
		t.Fatalf("EndSession failed: %v", err)
	}

	// Verify ended_at is set
	endedSession, err := h.DB.GetSession(sess.ID)
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}
	if endedSession.EndedAt == nil {
		t.Error("ended_at should be set after EndSession")
	}
	t.Logf("  ended_at set: %s", endedSession.EndedAt.Format(time.RFC3339))

	// Create new session for same agent
	newSess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("stale-agent"), // Same agent name
		testutil.WithModel("test-model"),
	)
	t.Logf("  New session created: %s", newSess.ID)

	t.Log("=== PASS: TestStaleSessionDetection ===")
}

// TestRequestTimeout tests that expired requests are detected via IsExpired()
// and can be found via FindExpiredRequests().
// Note: In production, a daemon background process would mark expired requests
// and the review service should check IsExpired() before allowing approval.
func TestRequestTimeout(t *testing.T) {
	h := testutil.NewHarness(t)

	t.Log("=== TestRequestTimeout ===")
	t.Logf("ENV: temp_db=%s", h.DBPath)

	// Step 1: Create requestor session
	t.Log("STEP 1: Creating requestor session")
	requestorSess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("requestor"),
		testutil.WithModel("model-a"),
	)
	t.Logf("  Session created: %s", requestorSess.ID)

	// Step 2: Create request with short TTL (2 seconds)
	// Note: Must use at least 2 seconds because DB stores at second precision (RFC3339)
	// and FindExpiredRequests uses string comparison which needs a clear second boundary
	t.Log("STEP 2: Creating request with 2s TTL")
	shortExpiry := time.Now().UTC().Add(2 * time.Second)
	req := testutil.MakeRequest(t, h.DB, requestorSess,
		testutil.WithCommand("rm -rf ./test", h.ProjectDir, true),
		testutil.WithRisk(db.RiskTierDangerous),
		testutil.WithMinApprovals(1),
		testutil.WithExpiresAt(shortExpiry),
	)
	t.Logf("  Request created: %s", req.ID)
	t.Logf("  Status: %s", req.Status)
	t.Logf("  Expires at: %s", shortExpiry.Format(time.RFC3339))

	// Step 3: Verify request is pending and not expired initially
	t.Log("STEP 3: Verifying request is pending and not expired initially")
	if req.Status != db.StatusPending {
		t.Errorf("Expected status=pending, got %s", req.Status)
	}
	// Initially should not be expired
	initialExpired := req.IsExpired()
	t.Logf("  Status: %s, IsExpired: %v", req.Status, initialExpired)

	// Step 4: Wait for request to expire (3 seconds to ensure we cross the boundary)
	t.Log("STEP 4: Waiting for request to expire")
	time.Sleep(3 * time.Second)

	// Step 5: Verify request is now detected as expired
	t.Log("STEP 5: Verifying request is now expired")
	refreshedReq, err := h.DB.GetRequest(req.ID)
	if err != nil {
		t.Fatalf("GetRequest failed: %v", err)
	}

	// Verify IsExpired() returns true
	if !refreshedReq.IsExpired() {
		t.Error("Expected IsExpired() to return true after waiting past expiry")
	}
	t.Logf("  IsExpired: %v", refreshedReq.IsExpired())

	// Verify FindExpiredRequests finds it
	expiredRequests, err := h.DB.FindExpiredRequests()
	if err != nil {
		t.Fatalf("FindExpiredRequests failed: %v", err)
	}
	found := false
	for _, r := range expiredRequests {
		if r.ID == req.ID {
			found = true
			break
		}
	}
	if !found {
		t.Error("Expected request to be in FindExpiredRequests results")
	}
	t.Logf("  Found in FindExpiredRequests: %v", found)

	// Step 6: Verify that expired requests should be rejected
	// Note: The current ReviewService doesn't check IsExpired(), but in production
	// a background process would mark the status as timeout before approval attempts.
	// Here we verify the detection mechanism works.
	t.Log("STEP 6: Documenting expected behavior")
	t.Log("  In production, daemon would mark status=timeout for expired requests")
	t.Log("  ReviewService checks status=pending, so timeout requests are rejected")

	// Manually set status to timeout to verify review rejection
	err = h.DB.UpdateRequestStatus(req.ID, db.StatusTimeout)
	if err != nil {
		t.Fatalf("UpdateRequestStatus failed: %v", err)
	}

	reviewerSess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("reviewer"),
		testutil.WithModel("model-b"),
	)

	rs := core.NewReviewService(h.DB, core.DefaultReviewConfig())
	_, err = rs.SubmitReview(core.ReviewOptions{
		SessionID:  reviewerSess.ID,
		SessionKey: reviewerSess.SessionKey,
		RequestID:  req.ID,
		Decision:   db.DecisionApprove,
		Comments:   "Approving timed out request",
	})
	if err == nil {
		t.Fatal("Expected error when approving timed-out request")
	}
	t.Logf("  Approval correctly rejected for timed-out request: %v", err)

	t.Log("=== PASS: TestRequestTimeout ===")
}

// TestConcurrentSessionsSameAgent tests that only one active session per agent is allowed.
func TestConcurrentSessionsSameAgent(t *testing.T) {
	h := testutil.NewHarness(t)

	t.Log("=== TestConcurrentSessionsSameAgent ===")
	t.Logf("ENV: temp_db=%s", h.DBPath)

	// Step 1: Create first session
	t.Log("STEP 1: Creating first session for agent-1")
	sess1 := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("agent-1"),
		testutil.WithModel("model-a"),
	)
	t.Logf("  First session created: %s", sess1.ID)

	// Step 2: Attempt to create second session for same agent
	t.Log("STEP 2: Attempting second session for agent-1")
	sess2 := &db.Session{
		AgentName:   "agent-1", // Same agent
		Program:     "test",
		Model:       "model-a",
		ProjectPath: h.ProjectDir,
	}
	err := h.DB.CreateSession(sess2)

	// Step 3: Verify error for duplicate active session
	t.Log("STEP 3: Verifying error for duplicate session")
	if err == nil {
		t.Fatal("Expected error when creating duplicate active session")
	}
	if err != db.ErrActiveSessionExists {
		t.Errorf("Expected ErrActiveSessionExists, got: %v", err)
	}
	t.Logf("  Correctly rejected: %v", err)

	// Step 4: End first session
	t.Log("STEP 4: Ending first session")
	err = h.DB.EndSession(sess1.ID)
	if err != nil {
		t.Fatalf("EndSession failed: %v", err)
	}
	t.Log("  First session ended")

	// Step 5: Create new session for same agent succeeds
	t.Log("STEP 5: Creating new session for agent-1 after ending first")
	newSess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("agent-1"),
		testutil.WithModel("model-a"),
	)
	t.Logf("  New session created: %s", newSess.ID)

	// Verify old session is ended and new session is active
	oldSession, _ := h.DB.GetSession(sess1.ID)
	if oldSession.EndedAt == nil {
		t.Error("Old session should have ended_at set")
	}
	newSession, _ := h.DB.GetSession(newSess.ID)
	if newSession.EndedAt != nil {
		t.Error("New session should be active")
	}

	t.Log("=== PASS: TestConcurrentSessionsSameAgent ===")
}

// TestSessionRecoveryAfterRestart tests that session and request state persists.
// Note: This test simulates restart by closing and reopening the database.
func TestSessionRecoveryAfterRestart(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	h := testutil.NewHarness(t)

	t.Log("=== TestSessionRecoveryAfterRestart ===")
	t.Logf("ENV: temp_db=%s", h.DBPath)

	// Step 1: Create session and request
	t.Log("STEP 1: Creating session and request")
	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("recovery-agent"),
		testutil.WithModel("test-model"),
	)
	t.Logf("  Session created: %s", sess.ID)

	// Create request with long expiry
	longExpiry := time.Now().UTC().Add(30 * time.Minute)
	req := testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("rm -rf ./build", h.ProjectDir, true),
		testutil.WithRisk(db.RiskTierDangerous),
		testutil.WithMinApprovals(1),
		testutil.WithExpiresAt(longExpiry),
	)
	t.Logf("  Request created: %s", req.ID)

	// Store IDs for later verification
	sessionID := sess.ID
	requestID := req.ID
	sessionKey := sess.SessionKey

	// Step 2: Close database (simulate daemon crash)
	t.Log("STEP 2: Closing database (simulating crash)")
	h.DB.Close()
	t.Log("  Database closed")

	// Step 3: Reopen database
	t.Log("STEP 3: Reopening database (simulating restart)")
	newDB, err := db.Open(h.DBPath)
	if err != nil {
		t.Fatalf("Failed to reopen database: %v", err)
	}
	defer newDB.Close()
	t.Log("  Database reopened")

	// Step 4: Verify session state preserved
	t.Log("STEP 4: Verifying session state preserved")
	recoveredSession, err := newDB.GetSession(sessionID)
	if err != nil {
		t.Fatalf("GetSession failed after restart: %v", err)
	}
	if recoveredSession.AgentName != "recovery-agent" {
		t.Errorf("Session agent name mismatch: got %s, want recovery-agent", recoveredSession.AgentName)
	}
	// The key is not stored, only its digest: it still authenticates after
	// the restart, but nothing read back from the database contains it.
	if recoveredSession.SessionKey != "" {
		t.Error("GetSession returned a session key")
	}
	if _, err := newDB.AuthenticateSession(sessionID, sessionKey); err != nil {
		t.Errorf("Session key no longer authenticates after restart: %v", err)
	}
	if recoveredSession.EndedAt != nil {
		t.Error("Session should still be active after restart")
	}
	t.Logf("  Session state preserved: %s", recoveredSession.ID)

	// Step 5: Verify request still pending
	t.Log("STEP 5: Verifying request still pending")
	recoveredRequest, err := newDB.GetRequest(requestID)
	if err != nil {
		t.Fatalf("GetRequest failed after restart: %v", err)
	}
	if recoveredRequest.Status != db.StatusPending {
		t.Errorf("Request status changed: got %s, want pending", recoveredRequest.Status)
	}
	t.Logf("  Request status: %s", recoveredRequest.Status)

	// Step 6: Verify session can still be used for approval
	t.Log("STEP 6: Verifying session can be used for approval")
	reviewerSess := &db.Session{
		AgentName:   "recovery-reviewer",
		Program:     "test",
		Model:       "different-model",
		ProjectPath: h.ProjectDir,
	}
	err = newDB.CreateSession(reviewerSess)
	if err != nil {
		t.Fatalf("CreateSession for reviewer failed: %v", err)
	}

	rs := core.NewReviewService(newDB, core.DefaultReviewConfig())
	result, err := rs.SubmitReview(core.ReviewOptions{
		SessionID:  reviewerSess.ID,
		SessionKey: reviewerSess.SessionKey,
		RequestID:  requestID,
		Decision:   db.DecisionApprove,
		Comments:   "Approved after restart",
	})
	if err != nil {
		t.Fatalf("Approval after restart failed: %v", err)
	}
	t.Logf("  Approval succeeded, new status: %s", result.NewRequestStatus)

	t.Log("=== PASS: TestSessionRecoveryAfterRestart ===")
}

// TestDaemonCLI_ProjectLifecycle uses the actual CLI binary, including its
// background re-exec path. One build is shared by all lifecycle assertions.
func TestDaemonCLI_ProjectLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the CLI and launches background daemons")
	}
	moduleRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	// Keep runtime paths short enough for Unix sockets on macOS too, where
	// sun_path holds 103 bytes and the per-user TMPDIR resolves to a long
	// /private/var/folders/... path once symlinks are evaluated below.
	tempRoot := os.TempDir()
	if resolved, err := filepath.EvalSymlinks(tempRoot); err == nil {
		tempRoot = resolved
	}
	longest := filepath.Join(tempRoot, "slb-e2e-4294967295", "runtime", "slb-0123456789ab.sock")
	if runtime.GOOS != "windows" && len(longest) > 103 {
		tempRoot = "/tmp"
	}
	base, err := os.MkdirTemp(tempRoot, "slb-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	binaryName := "slb"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	binary := filepath.Join(base, binaryName)
	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelBuild()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", binary, "./cmd/slb")
	build.Dir = moduleRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building CLI: %v\n%s", err, output)
	}

	runtimeDir, home := filepath.Join(base, "runtime"), filepath.Join(base, "home")
	for _, dir := range []string{runtimeDir, home} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	var environment []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(name) {
		case "HOME", "USERPROFILE", "XDG_CONFIG_HOME", "TMPDIR", "TMP", "TEMP", "PWD", "TOON_DEFAULT_FORMAT":
			continue
		}
		if strings.HasPrefix(strings.ToUpper(name), "SLB_") {
			continue
		}
		environment = append(environment, entry)
	}
	environment = append(environment,
		"HOME="+home, "USERPROFILE="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"TMPDIR="+runtimeDir, "TMP="+runtimeDir, "TEMP="+runtimeDir, "SLB_HOST=", "SLB_PROJECT=",
	)
	run := func(cwd, executable string, args ...string) ([]byte, []byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, executable, args...)
		cmd.Dir, cmd.Env = cwd, environment
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		if ctx.Err() != nil {
			err = fmt.Errorf("CLI command timed out: %w", ctx.Err())
		}
		return stdout.Bytes(), stderr.Bytes(), err
	}
	mustRun := func(t *testing.T, cwd, executable string, args ...string) []byte {
		t.Helper()
		stdout, stderr, err := run(cwd, executable, args...)
		if err != nil {
			t.Fatalf("%s %v from %s: %v\nstdout: %s\nstderr: %s", executable, args, cwd, err, stdout, stderr)
		}
		return stdout
	}
	type statusInfo struct {
		Running     bool   `json:"running"`
		Status      string `json:"status"`
		PID         int    `json:"pid"`
		PIDFile     string `json:"pid_file"`
		SocketPath  string `json:"socket_path"`
		SocketAlive bool   `json:"socket_alive"`
	}
	status := func(t *testing.T, cwd string, projectArgs ...string) statusInfo {
		t.Helper()
		args := append([]string{"daemon", "status", "-o", "json"}, projectArgs...)
		data := mustRun(t, cwd, binary, args...)
		var info statusInfo
		if err := json.Unmarshal(data, &info); err != nil {
			t.Fatalf("invalid daemon status JSON: %v\n%s", err, data)
		}
		return info
	}
	assertRunning := func(t *testing.T, info statusInfo, pid int) {
		t.Helper()
		if !info.Running || !info.SocketAlive || info.Status != "running" || info.PID != pid || pid <= 0 {
			t.Fatalf("unexpected daemon status: %+v, want running PID %d", info, pid)
		}
	}
	start := func(t *testing.T, cwd, executable string, projectArgs ...string) int {
		t.Helper()
		args := append([]string{"daemon", "start", "-o", "json"}, projectArgs...)
		data := mustRun(t, cwd, executable, args...)
		var result struct {
			PID        int    `json:"pid"`
			SocketPath string `json:"socket_path"`
			Foreground bool   `json:"foreground"`
		}
		if err := json.Unmarshal(data, &result); err != nil || result.PID <= 0 || result.SocketPath == "" || result.Foreground {
			t.Fatalf("invalid background-start result: %s (decode error %v)", data, err)
		}
		return result.PID
	}
	for _, name := range []string{"A", "B", "unused", "broken"} {
		project := filepath.Join(base, name)
		if err := os.MkdirAll(project, 0700); err != nil {
			t.Fatal(err)
		}
		mustRun(t, base, binary, "init", "-C", name, "-o", "json")
		config := "[daemon]\nuse_file_watcher = false\n[notifications]\ndesktop_enabled = false\n"
		if err := os.WriteFile(filepath.Join(project, ".slb", "config.toml"), []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		// Cleanup uses explicit resource paths, so a CLI -C regression cannot
		// prevent the test from stopping the background daemon it launched.
		cleanupOptions := daemon.ServerOptions{
			ProjectDir: project,
			SocketPath: filepath.Join(runtimeDir, filepath.Base(daemon.SocketPathForCWD(project))),
			PIDFile:    filepath.Join(runtimeDir, filepath.Base(daemon.PIDFileForCWD(project))),
		}
		t.Cleanup(func() { _ = daemon.StopDaemonWithOptions(cleanupOptions, 3*time.Second) })
	}

	t.Run("relative_executable_and_project", func(t *testing.T) {
		// Both the executable and -C path are relative to the original CWD.
		// The -- terminator prevents an appended project flag from being parsed.
		relativeBinary := "." + string(filepath.Separator) + binaryName
		pidA := start(t, base, relativeBinary, "-C", "A", "--")
		pidB := start(t, base, relativeBinary, "-C", "B", "--")
		if pidA == pidB {
			t.Fatal("two independent projects reported the same daemon PID")
		}
		a, b := status(t, base, "-C", "A"), status(t, base, "-C", "B")
		assertRunning(t, a, pidA)
		assertRunning(t, b, pidB)
		if a.PIDFile == b.PIDFile || a.SocketPath == b.SocketPath {
			t.Fatalf("projects share lifecycle resources: A=%+v, B=%+v", a, b)
		}
		for _, info := range []statusInfo{a, b} {
			if filepath.Dir(info.PIDFile) != runtimeDir || filepath.Dir(info.SocketPath) != runtimeDir ||
				strings.TrimSuffix(info.PIDFile, ".pid") != strings.TrimSuffix(info.SocketPath, ".sock") {
				t.Fatalf("PID and socket identity disagree: %+v", info)
			}
		}
		if unused := status(t, base, "-C", "unused"); unused.Running || unused.SocketAlive || unused.PID != 0 || unused.Status != "not running" {
			t.Fatalf("never-started project inherited another daemon's status: %+v", unused)
		}
		nested := filepath.Join(base, "A", "src", "deep")
		if err := os.MkdirAll(nested, 0700); err != nil {
			t.Fatal(err)
		}
		fromNested := status(t, nested)
		assertRunning(t, fromNested, pidA)
		if fromNested.PIDFile != a.PIDFile || fromNested.SocketPath != a.SocketPath {
			t.Fatalf("nested CWD selected a different daemon: %+v", fromNested)
		}
		mustRun(t, nested, binary, "daemon", "stop", "-o", "json")
		if stopped := status(t, base, "-C", "A"); stopped.Running || stopped.PID != 0 {
			t.Fatalf("project A still running after nested-CWD stop: %+v", stopped)
		}
		assertRunning(t, status(t, base, "-C", "B"), pidB)
		pidA = start(t, nested, binary)
		assertRunning(t, status(t, base, "-C", "A"), pidA)
		if _, err := os.Lstat(filepath.Join(nested, ".slb")); !os.IsNotExist(err) {
			t.Fatalf("nested start created a second project database: %v", err)
		}
		mustRun(t, base, relativeBinary, "daemon", "stop", "-C", "B", "-o", "json")
		assertRunning(t, status(t, nested), pidA)
		mustRun(t, base, relativeBinary, "daemon", "stop", "-C", filepath.Join("A", "src", "deep"), "-o", "json")
		for _, name := range []string{"A", "B"} {
			info := status(t, base, "-C", name)
			if info.Running || info.PID != 0 || info.SocketAlive {
				t.Fatalf("project %s did not stop: %+v", name, info)
			}
			for _, path := range []string{info.PIDFile, info.SocketPath} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("stopped daemon left resource %s: %v", path, err)
				}
			}
		}
	})

	t.Run("failed_startup_has_no_pid_residue", func(t *testing.T) {
		configPath := filepath.Join(base, "broken", ".slb", "config.toml")
		if err := os.WriteFile(configPath, []byte("[daemon\ninvalid TOML"), 0600); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, err := run(base, binary, "daemon", "start", "-C", "broken", "-o", "json")
		if err == nil {
			t.Fatalf("failed startup reported success: %s", stdout)
		}
		if !strings.Contains(string(stderr), "daemon exited before readiness") {
			t.Fatalf("startup did not report the child readiness failure: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
		}
		info := status(t, base, "-C", "broken")
		if info.Running || info.PID != 0 || info.SocketAlive {
			t.Fatalf("failed startup left a running daemon: %+v", info)
		}
		for _, path := range []string{info.PIDFile, info.SocketPath} {
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("failed startup left resource %s: %v", path, err)
			}
		}
	})
}

// containsSession checks if a session ID is in the slice.
func containsSession(sessions []*db.Session, id string) bool {
	for _, s := range sessions {
		if s.ID == id {
			return true
		}
	}
	return false
}
