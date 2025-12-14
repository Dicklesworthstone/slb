// Package daemon provides tests for the execution verifier.
package daemon

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func setupTestDB(t *testing.T) *db.DB {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

func createTestSession(t *testing.T, database *db.DB, id string) *db.Session {
	t.Helper()
	session := &db.Session{
		ID:          id,
		AgentName:   "Agent-" + id, // Unique agent name per session
		Program:     "test-cli",
		Model:       "test-model",
		ProjectPath: "/test/project-" + id, // Unique project path per session
		SessionKey:  "0123456789abcdef0123456789abcdef",
	}
	if err := database.CreateSession(session); err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	return session
}

func createTestRequest(t *testing.T, database *db.DB, id, sessionID string, status db.RequestStatus, minApprovals int) *db.Request {
	t.Helper()
	now := time.Now().UTC()
	expiresAt := now.Add(30 * time.Minute)
	approvalExpiresAt := now.Add(5 * time.Minute)

	request := &db.Request{
		ID:          id,
		ProjectPath: "/test/project",
		Command: db.CommandSpec{
			Raw:  "rm -rf /tmp/test",
			Cwd:  "/tmp",
			Hash: "testhash123",
		},
		RiskTier:           db.RiskTierDangerous,
		RequestorSessionID: sessionID,
		RequestorAgent:     "TestAgent",
		RequestorModel:     "test-model",
		Justification: db.Justification{
			Reason: "Testing execution",
		},
		Status:            status,
		MinApprovals:      minApprovals,
		ExpiresAt:         &expiresAt,
		ApprovalExpiresAt: &approvalExpiresAt,
	}
	if err := database.CreateRequest(request); err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	return request
}

func createTestReview(t *testing.T, database *db.DB, requestID, sessionID string, decision db.Decision) *db.Review {
	t.Helper()
	review := &db.Review{
		RequestID:         requestID,
		ReviewerSessionID: sessionID,
		ReviewerAgent:     "ReviewerAgent",
		ReviewerModel:     "reviewer-model",
		Decision:          decision,
		Comments:          "Test review",
	}
	if err := database.CreateReview(review); err != nil {
		t.Fatalf("failed to create review: %v", err)
	}
	return review
}

func TestVerifier_VerifyExecutionAllowed_MissingParams(t *testing.T) {
	database := setupTestDB(t)
	v := NewVerifier(database)

	tests := []struct {
		name      string
		requestID string
		sessionID string
		wantErr   string
	}{
		{"missing request_id", "", "sess1", "request_id is required"},
		{"missing session_id", "req1", "", "session_id is required"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := v.VerifyExecutionAllowed(tc.requestID, tc.sessionID)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if err.Error() != tc.wantErr {
				t.Errorf("expected error %q, got %q", tc.wantErr, err.Error())
			}
		})
	}
}

func TestVerifier_VerifyExecutionAllowed_RequestNotFound(t *testing.T) {
	database := setupTestDB(t)
	v := NewVerifier(database)

	_, err := v.VerifyExecutionAllowed("nonexistent-id", "sess1")
	if err == nil {
		t.Fatalf("expected error for nonexistent request")
	}
}

func TestVerifier_VerifyExecutionAllowed_NotApproved(t *testing.T) {
	database := setupTestDB(t)
	v := NewVerifier(database)

	createTestSession(t, database, "sess1")
	createTestRequest(t, database, "req1", "sess1", db.StatusPending, 1)

	result, err := v.VerifyExecutionAllowed("req1", "sess1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Error("expected Allowed=false for pending request")
	}
	if result.Reason == "" {
		t.Error("expected reason to be set")
	}
}

func TestVerifier_VerifyExecutionAllowed_ExpiredApproval(t *testing.T) {
	database := setupTestDB(t)
	v := NewVerifier(database)

	createTestSession(t, database, "sess1")

	// Create a request with expired approval
	now := time.Now().UTC()
	expiresAt := now.Add(30 * time.Minute)
	approvalExpiresAt := now.Add(-1 * time.Minute) // Already expired

	request := &db.Request{
		ID:          "req-expired",
		ProjectPath: "/test/project",
		Command: db.CommandSpec{
			Raw:  "rm -rf /tmp/test",
			Cwd:  "/tmp",
			Hash: "testhash123",
		},
		RiskTier:           db.RiskTierDangerous,
		RequestorSessionID: "sess1",
		RequestorAgent:     "TestAgent",
		RequestorModel:     "test-model",
		Justification: db.Justification{
			Reason: "Testing execution",
		},
		Status:            db.StatusApproved,
		MinApprovals:      1,
		ExpiresAt:         &expiresAt,
		ApprovalExpiresAt: &approvalExpiresAt,
	}
	if err := database.CreateRequest(request); err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	// Add approval
	createTestSession(t, database, "reviewer-sess")
	createTestReview(t, database, "req-expired", "reviewer-sess", db.DecisionApprove)

	result, err := v.VerifyExecutionAllowed("req-expired", "sess1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Error("expected Allowed=false for expired approval")
	}
	if result.Reason != "approval has expired" {
		t.Errorf("expected reason 'approval has expired', got %q", result.Reason)
	}
}

func TestVerifier_VerifyExecutionAllowed_InsufficientApprovals(t *testing.T) {
	database := setupTestDB(t)
	v := NewVerifier(database)

	createTestSession(t, database, "sess1")
	createTestRequest(t, database, "req1", "sess1", db.StatusApproved, 2)

	// Only add 1 approval when 2 are required
	createTestSession(t, database, "reviewer-sess")
	createTestReview(t, database, "req1", "reviewer-sess", db.DecisionApprove)

	result, err := v.VerifyExecutionAllowed("req1", "sess1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Error("expected Allowed=false for insufficient approvals")
	}
	if result.Reason == "" {
		t.Error("expected reason to be set")
	}
}

func TestVerifier_VerifyExecutionAllowed_Success(t *testing.T) {
	database := setupTestDB(t)
	v := NewVerifier(database)

	createTestSession(t, database, "sess1")
	createTestRequest(t, database, "req1", "sess1", db.StatusApproved, 1)

	createTestSession(t, database, "reviewer-sess")
	createTestReview(t, database, "req1", "reviewer-sess", db.DecisionApprove)

	result, err := v.VerifyExecutionAllowed("req1", "sess1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Errorf("expected Allowed=true, got false (reason: %s)", result.Reason)
	}
	if result.Request == nil {
		t.Error("expected Request to be set when allowed")
	}
	if result.ApprovalRemainingSeconds <= 0 {
		t.Error("expected positive ApprovalRemainingSeconds")
	}
}

func TestVerifier_VerifyAndMarkExecuting_Success(t *testing.T) {
	database := setupTestDB(t)
	v := NewVerifier(database)

	createTestSession(t, database, "sess1")
	createTestRequest(t, database, "req1", "sess1", db.StatusApproved, 1)

	createTestSession(t, database, "reviewer-sess")
	createTestReview(t, database, "req1", "reviewer-sess", db.DecisionApprove)

	result, err := v.VerifyAndMarkExecuting("req1", "sess1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Errorf("expected Allowed=true, got false (reason: %s)", result.Reason)
	}

	// Verify status was updated
	request, err := database.GetRequest("req1")
	if err != nil {
		t.Fatalf("failed to get request: %v", err)
	}
	if request.Status != db.StatusExecuting {
		t.Errorf("expected status %s, got %s", db.StatusExecuting, request.Status)
	}
}

func TestVerifier_VerifyAndMarkExecuting_RaceCondition(t *testing.T) {
	database := setupTestDB(t)
	v := NewVerifier(database)

	createTestSession(t, database, "sess1")
	createTestRequest(t, database, "req1", "sess1", db.StatusApproved, 1)

	createTestSession(t, database, "reviewer-sess")
	createTestReview(t, database, "req1", "reviewer-sess", db.DecisionApprove)

	// First executor wins
	result1, err := v.VerifyAndMarkExecuting("req1", "sess1")
	if err != nil {
		t.Fatalf("first VerifyAndMarkExecuting failed: %v", err)
	}
	if !result1.Allowed {
		t.Errorf("expected first executor to be allowed, got false (reason: %s)", result1.Reason)
	}

	// Second executor should fail
	result2, err := v.VerifyAndMarkExecuting("req1", "sess1")
	if err != nil {
		t.Fatalf("second VerifyAndMarkExecuting returned error: %v", err)
	}
	if result2.Allowed {
		t.Error("expected second executor to be denied")
	}
}

func TestVerifier_MarkExecutionComplete_Success(t *testing.T) {
	database := setupTestDB(t)
	v := NewVerifier(database)

	createTestSession(t, database, "sess1")
	createTestRequest(t, database, "req1", "sess1", db.StatusApproved, 1)

	createTestSession(t, database, "reviewer-sess")
	createTestReview(t, database, "req1", "reviewer-sess", db.DecisionApprove)

	// Mark as executing first
	_, err := v.VerifyAndMarkExecuting("req1", "sess1")
	if err != nil {
		t.Fatalf("VerifyAndMarkExecuting failed: %v", err)
	}

	// Mark as complete
	if err := v.MarkExecutionComplete("req1", 0, true); err != nil {
		t.Fatalf("MarkExecutionComplete failed: %v", err)
	}

	// Verify status
	request, err := database.GetRequest("req1")
	if err != nil {
		t.Fatalf("failed to get request: %v", err)
	}
	if request.Status != db.StatusExecuted {
		t.Errorf("expected status %s, got %s", db.StatusExecuted, request.Status)
	}
}

func TestVerifier_MarkExecutionComplete_Failure(t *testing.T) {
	database := setupTestDB(t)
	v := NewVerifier(database)

	createTestSession(t, database, "sess1")
	createTestRequest(t, database, "req1", "sess1", db.StatusApproved, 1)

	createTestSession(t, database, "reviewer-sess")
	createTestReview(t, database, "req1", "reviewer-sess", db.DecisionApprove)

	// Mark as executing first
	_, err := v.VerifyAndMarkExecuting("req1", "sess1")
	if err != nil {
		t.Fatalf("VerifyAndMarkExecuting failed: %v", err)
	}

	// Mark as failed
	if err := v.MarkExecutionComplete("req1", 1, false); err != nil {
		t.Fatalf("MarkExecutionComplete failed: %v", err)
	}

	// Verify status
	request, err := database.GetRequest("req1")
	if err != nil {
		t.Fatalf("failed to get request: %v", err)
	}
	if request.Status != db.StatusExecutionFailed {
		t.Errorf("expected status %s, got %s", db.StatusExecutionFailed, request.Status)
	}
}

func TestVerifier_MarkExecutionComplete_WrongStatus(t *testing.T) {
	database := setupTestDB(t)
	v := NewVerifier(database)

	createTestSession(t, database, "sess1")
	createTestRequest(t, database, "req1", "sess1", db.StatusApproved, 1)

	// Try to mark complete without first marking as executing
	err := v.MarkExecutionComplete("req1", 0, true)
	if err == nil {
		t.Fatal("expected error when marking complete on non-executing request")
	}
}

func TestVerifier_MarkExecutionComplete_MissingRequestID(t *testing.T) {
	database := setupTestDB(t)
	v := NewVerifier(database)

	err := v.MarkExecutionComplete("", 0, true)
	if err == nil {
		t.Fatal("expected error for missing request_id")
	}
}

func TestVerificationResult_ToIPCResponse(t *testing.T) {
	result := &VerificationResult{
		Allowed:                  true,
		Reason:                   "",
		ApprovalRemainingSeconds: 300,
		Request: &db.Request{
			ID: "req-123",
			Command: db.CommandSpec{
				Raw:  "echo hello",
				Hash: "abc123",
			},
			RiskTier: db.RiskTierCaution,
		},
	}

	resp := result.ToIPCResponse()

	if !resp.Allowed {
		t.Error("expected Allowed=true")
	}
	if resp.RequestID != "req-123" {
		t.Errorf("expected RequestID 'req-123', got %q", resp.RequestID)
	}
	if resp.Command != "echo hello" {
		t.Errorf("expected Command 'echo hello', got %q", resp.Command)
	}
	if resp.CommandHash != "abc123" {
		t.Errorf("expected CommandHash 'abc123', got %q", resp.CommandHash)
	}
	if resp.RiskTier != "caution" {
		t.Errorf("expected RiskTier 'caution', got %q", resp.RiskTier)
	}
	if resp.ApprovalRemainingSeconds != 300 {
		t.Errorf("expected ApprovalRemainingSeconds 300, got %d", resp.ApprovalRemainingSeconds)
	}
}

func TestVerificationResult_ToIPCResponse_Denied(t *testing.T) {
	result := &VerificationResult{
		Allowed: false,
		Reason:  "request is not approved",
	}

	resp := result.ToIPCResponse()

	if resp.Allowed {
		t.Error("expected Allowed=false")
	}
	if resp.Reason != "request is not approved" {
		t.Errorf("expected Reason 'request is not approved', got %q", resp.Reason)
	}
	if resp.RequestID != "" {
		t.Errorf("expected empty RequestID for denied request, got %q", resp.RequestID)
	}
}

func TestVerifier_NilApprovalExpiresAt(t *testing.T) {
	database := setupTestDB(t)
	v := NewVerifier(database)

	createTestSession(t, database, "sess1")

	// Create request without ApprovalExpiresAt
	now := time.Now().UTC()
	expiresAt := now.Add(30 * time.Minute)

	request := &db.Request{
		ID:          "req-no-approval-expiry",
		ProjectPath: "/test/project",
		Command: db.CommandSpec{
			Raw:  "echo test",
			Cwd:  "/tmp",
			Hash: "testhash",
		},
		RiskTier:           db.RiskTierDangerous,
		RequestorSessionID: "sess1",
		RequestorAgent:     "TestAgent",
		RequestorModel:     "test-model",
		Justification: db.Justification{
			Reason: "Testing",
		},
		Status:            db.StatusApproved,
		MinApprovals:      1,
		ExpiresAt:         &expiresAt,
		ApprovalExpiresAt: nil, // No approval expiry set
	}
	if err := database.CreateRequest(request); err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	createTestSession(t, database, "reviewer-sess")
	createTestReview(t, database, "req-no-approval-expiry", "reviewer-sess", db.DecisionApprove)

	result, err := v.VerifyExecutionAllowed("req-no-approval-expiry", "sess1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Error("expected Allowed=false when ApprovalExpiresAt is nil")
	}
	if result.Reason != "approval_expires_at is not set" {
		t.Errorf("expected reason 'approval_expires_at is not set', got %q", result.Reason)
	}
}
