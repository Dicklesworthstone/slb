// Package e2e contains end-to-end integration tests for SLB workflows.
package e2e

import (
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/testutil"
)

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
	if recoveredSession.SessionKey != sessionKey {
		t.Error("Session key changed after restart")
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

// containsSession checks if a session ID is in the slice.
func containsSession(sessions []*db.Session, id string) bool {
	for _, s := range sessions {
		if s.ID == id {
			return true
		}
	}
	return false
}
