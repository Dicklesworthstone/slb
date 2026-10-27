package core

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

// setupReviewTest creates a DB with a session and request for testing.
func setupReviewTest(t *testing.T) (*db.DB, *db.Session, *db.Request) {
	t.Helper()
	dbConn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open(:memory:) error = %v", err)
	}

	sess := &db.Session{
		AgentName:   "BlueSnow",
		Program:     "codex-cli",
		Model:       "gpt-5.2",
		ProjectPath: "/test/project",
	}
	if err := dbConn.CreateSession(sess); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	req := &db.Request{
		ProjectPath:           "/test/project",
		RequestorSessionID:    sess.ID,
		RequestorAgent:        sess.AgentName,
		RequestorModel:        sess.Model,
		RiskTier:              db.RiskTierDangerous,
		MinApprovals:          1,
		RequireDifferentModel: true,
		Command: db.CommandSpec{
			Raw: "rm -rf ./build",
			Cwd: "/test/project",
		},
		Justification: db.Justification{
			Reason: "Cleaning build output",
		},
	}
	if err := dbConn.CreateRequest(req); err != nil {
		t.Fatalf("CreateRequest() error = %v", err)
	}

	return dbConn, sess, req
}

func TestCheckDifferentModelEscalation_NoDifferentModelRequired(t *testing.T) {
	dbConn, sess, _ := setupReviewTest(t)
	defer dbConn.Close()

	// Create a request that doesn't require different model
	req := &db.Request{
		ProjectPath:           "/test/project",
		RequestorSessionID:    sess.ID,
		RequestorAgent:        sess.AgentName,
		RequestorModel:        sess.Model,
		RiskTier:              db.RiskTierCaution,
		MinApprovals:          1,
		RequireDifferentModel: false,
		Command: db.CommandSpec{
			Raw: "go build",
			Cwd: "/test/project",
		},
		Justification: db.Justification{
			Reason: "Building project",
		},
	}
	if err := dbConn.CreateRequest(req); err != nil {
		t.Fatalf("CreateRequest() error = %v", err)
	}

	rs := NewReviewService(dbConn, DefaultReviewConfig())
	status, err := rs.CheckDifferentModelEscalation(req.ID)
	if err != nil {
		t.Fatalf("CheckDifferentModelEscalation() error = %v", err)
	}

	if status.NeedsDifferentModel {
		t.Error("Expected NeedsDifferentModel to be false")
	}
	if status.ShouldEscalate {
		t.Error("Expected ShouldEscalate to be false")
	}
}

func TestCheckDifferentModelEscalation_DifferentModelAvailable(t *testing.T) {
	dbConn, _, req := setupReviewTest(t)
	defer dbConn.Close()

	// Create a session with a different model
	diffSess := &db.Session{
		AgentName:   "GreenLake",
		Program:     "claude-code",
		Model:       "opus-4.5",
		ProjectPath: "/test/project",
	}
	if err := dbConn.CreateSession(diffSess); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	rs := NewReviewService(dbConn, DefaultReviewConfig())
	status, err := rs.CheckDifferentModelEscalation(req.ID)
	if err != nil {
		t.Fatalf("CheckDifferentModelEscalation() error = %v", err)
	}

	if !status.NeedsDifferentModel {
		t.Error("Expected NeedsDifferentModel to be true")
	}
	if !status.DifferentModelAvailable {
		t.Error("Expected DifferentModelAvailable to be true")
	}
	if status.ShouldEscalate {
		t.Error("Expected ShouldEscalate to be false when different model is available")
	}
	if len(status.DifferentModelAgents) != 1 || status.DifferentModelAgents[0] != "GreenLake" {
		t.Errorf("Expected DifferentModelAgents=[GreenLake], got %v", status.DifferentModelAgents)
	}
}

func TestCheckDifferentModelEscalation_NoDifferentModel_TimeoutNotExpired(t *testing.T) {
	dbConn, _, req := setupReviewTest(t)
	defer dbConn.Close()

	// Only same-model sessions exist, request just created
	rs := NewReviewService(dbConn, DefaultReviewConfig())
	status, err := rs.CheckDifferentModelEscalation(req.ID)
	if err != nil {
		t.Fatalf("CheckDifferentModelEscalation() error = %v", err)
	}

	if !status.NeedsDifferentModel {
		t.Error("Expected NeedsDifferentModel to be true")
	}
	if status.DifferentModelAvailable {
		t.Error("Expected DifferentModelAvailable to be false")
	}
	if status.TimeoutExpired {
		t.Error("Expected TimeoutExpired to be false for fresh request")
	}
	if status.ShouldEscalate {
		t.Error("Expected ShouldEscalate to be false before timeout")
	}
	if status.TimeUntilEscalation <= 0 {
		t.Error("Expected TimeUntilEscalation to be positive")
	}
}

func TestCheckDifferentModelEscalation_NoDifferentModel_TimeoutExpired(t *testing.T) {
	dbConn, _, req := setupReviewTest(t)
	defer dbConn.Close()

	// Backdate the request to simulate timeout
	old := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	if _, err := dbConn.Exec(`UPDATE requests SET created_at = ? WHERE id = ?`, old, req.ID); err != nil {
		t.Fatalf("failed to backdate request: %v", err)
	}

	rs := NewReviewService(dbConn, DefaultReviewConfig())
	status, err := rs.CheckDifferentModelEscalation(req.ID)
	if err != nil {
		t.Fatalf("CheckDifferentModelEscalation() error = %v", err)
	}

	if !status.NeedsDifferentModel {
		t.Error("Expected NeedsDifferentModel to be true")
	}
	if status.DifferentModelAvailable {
		t.Error("Expected DifferentModelAvailable to be false")
	}
	if !status.TimeoutExpired {
		t.Error("Expected TimeoutExpired to be true")
	}
	if !status.ShouldEscalate {
		t.Error("Expected ShouldEscalate to be true after timeout")
	}
	if status.EscalationReason == "" {
		t.Error("Expected EscalationReason to be set")
	}
}

func TestEscalateDifferentModelTimeout_Success(t *testing.T) {
	dbConn, _, req := setupReviewTest(t)
	defer dbConn.Close()

	// Backdate the request
	old := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	if _, err := dbConn.Exec(`UPDATE requests SET created_at = ? WHERE id = ?`, old, req.ID); err != nil {
		t.Fatalf("failed to backdate request: %v", err)
	}

	rs := NewReviewService(dbConn, DefaultReviewConfig())
	err := rs.EscalateDifferentModelTimeout(req.ID)
	if err != nil {
		t.Fatalf("EscalateDifferentModelTimeout() error = %v", err)
	}

	// Verify status changed
	updated, err := dbConn.GetRequest(req.ID)
	if err != nil {
		t.Fatalf("GetRequest() error = %v", err)
	}
	if updated.Status != db.StatusEscalated {
		t.Errorf("Expected status=escalated, got %s", updated.Status)
	}
}

func TestEscalateDifferentModelTimeout_NotWarranted(t *testing.T) {
	dbConn, _, req := setupReviewTest(t)
	defer dbConn.Close()

	// Request is fresh, escalation not warranted
	rs := NewReviewService(dbConn, DefaultReviewConfig())
	err := rs.EscalateDifferentModelTimeout(req.ID)
	if err == nil {
		t.Fatal("Expected error when escalation not warranted")
	}

	// Verify status unchanged
	updated, err := dbConn.GetRequest(req.ID)
	if err != nil {
		t.Fatalf("GetRequest() error = %v", err)
	}
	if updated.Status != db.StatusPending {
		t.Errorf("Expected status=pending, got %s", updated.Status)
	}
}

func TestCheckAndEscalatePendingRequests(t *testing.T) {
	dbConn, sess, req := setupReviewTest(t)
	defer dbConn.Close()

	// Create another request that doesn't need escalation
	req2 := &db.Request{
		ProjectPath:           "/test/project",
		RequestorSessionID:    sess.ID,
		RequestorAgent:        sess.AgentName,
		RequestorModel:        sess.Model,
		RiskTier:              db.RiskTierCaution,
		MinApprovals:          1,
		RequireDifferentModel: false,
		Command: db.CommandSpec{
			Raw: "go test",
			Cwd: "/test/project",
		},
		Justification: db.Justification{
			Reason: "Running tests",
		},
	}
	if err := dbConn.CreateRequest(req2); err != nil {
		t.Fatalf("CreateRequest() error = %v", err)
	}

	// Backdate only the first request
	old := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	if _, err := dbConn.Exec(`UPDATE requests SET created_at = ? WHERE id = ?`, old, req.ID); err != nil {
		t.Fatalf("failed to backdate request: %v", err)
	}

	rs := NewReviewService(dbConn, DefaultReviewConfig())
	escalated, err := rs.CheckAndEscalatePendingRequests("/test/project")
	if err != nil {
		t.Fatalf("CheckAndEscalatePendingRequests() error = %v", err)
	}

	if escalated != 1 {
		t.Errorf("Expected 1 escalated, got %d", escalated)
	}

	// Verify first request escalated
	updated, _ := dbConn.GetRequest(req.ID)
	if updated.Status != db.StatusEscalated {
		t.Errorf("Expected req1 status=escalated, got %s", updated.Status)
	}

	// Verify second request unchanged
	updated2, _ := dbConn.GetRequest(req2.ID)
	if updated2.Status != db.StatusPending {
		t.Errorf("Expected req2 status=pending, got %s", updated2.Status)
	}
}

func TestSubmitReview_DifferentModelRequired_SameModelRejected(t *testing.T) {
	dbConn, _, req := setupReviewTest(t)
	defer dbConn.Close()

	// Create a reviewer session with same model as requestor
	reviewerSess := &db.Session{
		AgentName:   "RedCat",
		Program:     "codex-cli",
		Model:       "gpt-5.2", // Same model
		ProjectPath: "/test/project",
	}
	if err := dbConn.CreateSession(reviewerSess); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	rs := NewReviewService(dbConn, DefaultReviewConfig())
	_, err := rs.SubmitReview(ReviewOptions{
		SessionID:  reviewerSess.ID,
		SessionKey: reviewerSess.SessionKey,
		RequestID:  req.ID,
		Decision:   db.DecisionApprove,
	})
	if err == nil {
		t.Fatal("Expected error for same-model approval")
	}
	if err != ErrRequireDiffModel && (err == nil || err.Error() == "") {
		t.Errorf("Expected ErrRequireDiffModel, got %v", err)
	}
}

func TestSubmitReview_DifferentModelRequired_DifferentModelAccepted(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	dbConn, _, req := setupReviewTest(t)
	defer dbConn.Close()

	// Create a reviewer session with different model
	reviewerSess := &db.Session{
		AgentName:   "GreenLake",
		Program:     "claude-code",
		Model:       "opus-4.5", // Different model
		ProjectPath: "/test/project",
	}
	if err := dbConn.CreateSession(reviewerSess); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	rs := NewReviewService(dbConn, DefaultReviewConfig())
	result, err := rs.SubmitReview(ReviewOptions{
		SessionID:  reviewerSess.ID,
		SessionKey: reviewerSess.SessionKey,
		RequestID:  req.ID,
		Decision:   db.DecisionApprove,
	})
	if err != nil {
		t.Fatalf("SubmitReview() error = %v", err)
	}

	if result.Review == nil {
		t.Fatal("Expected review to be created")
	}
	if !result.RequestStatusChanged {
		t.Error("Expected request status to change")
	}
	if result.NewRequestStatus != db.StatusApproved {
		t.Errorf("Expected status=approved, got %s", result.NewRequestStatus)
	}
}

func TestSubmitReview_SessionKeyMismatch_Rejected(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	dbConn, _, req := setupReviewTest(t)
	defer dbConn.Close()

	// Create a reviewer session with different model (would normally be accepted)
	reviewerSess := &db.Session{
		AgentName:   "GreenLake",
		Program:     "claude-code",
		Model:       "opus-4.5",
		ProjectPath: "/test/project",
	}
	if err := dbConn.CreateSession(reviewerSess); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	rs := NewReviewService(dbConn, DefaultReviewConfig())

	// Try to submit review with WRONG session key
	_, err := rs.SubmitReview(ReviewOptions{
		SessionID:  reviewerSess.ID,
		SessionKey: "wrong-session-key-not-matching-stored-key",
		RequestID:  req.ID,
		Decision:   db.DecisionApprove,
	})
	if err == nil {
		t.Fatal("Expected error for mismatched session key")
	}
	if err != ErrSessionKeyMismatch {
		t.Errorf("Expected ErrSessionKeyMismatch, got %v", err)
	}

	// Verify no review was created
	reviews, err := dbConn.ListReviewsForRequest(req.ID)
	if err != nil {
		t.Fatalf("ListReviewsForRequest() error = %v", err)
	}
	if len(reviews) != 0 {
		t.Errorf("Expected no reviews to be created, got %d", len(reviews))
	}
}

func TestSubmitReview_MissingSessionKey_Rejected(t *testing.T) {
	dbConn, _, req := setupReviewTest(t)
	defer dbConn.Close()

	// Create a reviewer session
	reviewerSess := &db.Session{
		AgentName:   "GreenLake",
		Program:     "claude-code",
		Model:       "opus-4.5",
		ProjectPath: "/test/project",
	}
	if err := dbConn.CreateSession(reviewerSess); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	rs := NewReviewService(dbConn, DefaultReviewConfig())

	// Try to submit review with EMPTY session key
	_, err := rs.SubmitReview(ReviewOptions{
		SessionID:  reviewerSess.ID,
		SessionKey: "", // Empty!
		RequestID:  req.ID,
		Decision:   db.DecisionApprove,
	})
	if err == nil {
		t.Fatal("Expected error for missing session key")
	}
	if err != ErrMissingSessionKey {
		t.Errorf("Expected ErrMissingSessionKey, got %v", err)
	}
}

func TestGetDifferentModelStatus(t *testing.T) {
	dbConn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open(:memory:) error = %v", err)
	}
	defer dbConn.Close()

	// Create sessions with different models
	sessions := []*db.Session{
		{AgentName: "BlueSnow", Program: "codex-cli", Model: "gpt-5.2", ProjectPath: "/test/project"},
		{AgentName: "GreenLake", Program: "claude-code", Model: "opus-4.5", ProjectPath: "/test/project"},
		{AgentName: "RedCat", Program: "codex-cli", Model: "gpt-5.2", ProjectPath: "/test/project"},
	}
	for _, s := range sessions {
		if err := dbConn.CreateSession(s); err != nil {
			t.Fatalf("CreateSession() error = %v", err)
		}
	}

	status, err := dbConn.GetDifferentModelStatus("/test/project", "gpt-5.2")
	if err != nil {
		t.Fatalf("GetDifferentModelStatus() error = %v", err)
	}

	if !status.HasDifferentModel {
		t.Error("Expected HasDifferentModel to be true")
	}
	if len(status.AvailableModels) != 2 {
		t.Errorf("Expected 2 available models, got %d", len(status.AvailableModels))
	}
	if len(status.SameModelSessions) != 2 {
		t.Errorf("Expected 2 same-model sessions, got %d", len(status.SameModelSessions))
	}
	if len(status.DifferentModelSessions) != 1 {
		t.Errorf("Expected 1 different-model session, got %d", len(status.DifferentModelSessions))
	}
}

func TestSetNotifier(t *testing.T) {
	dbConn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open(:memory:) error = %v", err)
	}
	defer dbConn.Close()

	rs := NewReviewService(dbConn, DefaultReviewConfig())

	// SetNotifier with nil should not panic and keep default
	rs.SetNotifier(nil)
	// If we got here without panic, test passes

	// SetNotifier with a valid notifier should update
	mockNotifier := &mockRequestNotifier{}
	rs.SetNotifier(mockNotifier)
	// Verify it was set (indirectly through usage in SubmitReview)
}

// mockRequestNotifier implements integrations.RequestNotifier for testing.
type mockRequestNotifier struct {
	newRequestCalled bool
	approvedCalled   bool
	rejectedCalled   bool
	executedCalled   bool
}

func (m *mockRequestNotifier) NotifyNewRequest(req *db.Request) error {
	m.newRequestCalled = true
	return nil
}

func (m *mockRequestNotifier) NotifyRequestApproved(req *db.Request, review *db.Review) error {
	m.approvedCalled = true
	return nil
}

func (m *mockRequestNotifier) NotifyRequestRejected(req *db.Request, review *db.Review) error {
	m.rejectedCalled = true
	return nil
}

func (m *mockRequestNotifier) NotifyRequestExecuted(req *db.Request, exec *db.Execution, exitCode int) error {
	m.executedCalled = true
	return nil
}

func TestIsTrustedSelfApprove(t *testing.T) {
	dbConn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open(:memory:) error = %v", err)
	}
	defer dbConn.Close()

	tests := []struct {
		name       string
		config     ReviewConfig
		agentName  string
		wantResult bool
	}{
		{
			name: "agent in trusted list",
			config: ReviewConfig{
				TrustedSelfApprove: []string{"TrustedAgent", "AnotherTrusted"},
			},
			agentName:  "TrustedAgent",
			wantResult: true,
		},
		{
			name: "agent not in trusted list",
			config: ReviewConfig{
				TrustedSelfApprove: []string{"TrustedAgent", "AnotherTrusted"},
			},
			agentName:  "UntrustedAgent",
			wantResult: false,
		},
		{
			name: "empty trusted list",
			config: ReviewConfig{
				TrustedSelfApprove: nil,
			},
			agentName:  "AnyAgent",
			wantResult: false,
		},
		{
			name: "case sensitive match",
			config: ReviewConfig{
				TrustedSelfApprove: []string{"TrustedAgent"},
			},
			agentName:  "trustedagent",
			wantResult: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rs := NewReviewService(dbConn, tc.config)
			got := rs.isTrustedSelfApprove(tc.agentName)
			if got != tc.wantResult {
				t.Errorf("isTrustedSelfApprove(%q) = %v, want %v", tc.agentName, got, tc.wantResult)
			}
		})
	}
}

func TestDetermineNewStatus(t *testing.T) {
	dbConn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open(:memory:) error = %v", err)
	}
	defer dbConn.Close()

	tests := []struct {
		name       string
		resolution ConflictResolution
		request    *db.Request
		decision   db.Decision
		approvals  int
		rejections int
		wantStatus db.RequestStatus
	}{
		// ConflictAnyRejectionBlocks tests
		{
			name:       "any_rejection_blocks: rejection immediately blocks",
			resolution: ConflictAnyRejectionBlocks,
			request:    &db.Request{MinApprovals: 2},
			decision:   db.DecisionReject,
			approvals:  0,
			rejections: 1,
			wantStatus: db.StatusRejected,
		},
		{
			name:       "any_rejection_blocks: enough approvals",
			resolution: ConflictAnyRejectionBlocks,
			request:    &db.Request{MinApprovals: 2},
			decision:   db.DecisionApprove,
			approvals:  2,
			rejections: 0,
			wantStatus: db.StatusApproved,
		},
		{
			name:       "any_rejection_blocks: not enough approvals yet",
			resolution: ConflictAnyRejectionBlocks,
			request:    &db.Request{MinApprovals: 2},
			decision:   db.DecisionApprove,
			approvals:  1,
			rejections: 0,
			wantStatus: "",
		},
		{
			name:       "any_rejection_blocks: rejection takes precedence over approvals",
			resolution: ConflictAnyRejectionBlocks,
			request:    &db.Request{MinApprovals: 2},
			decision:   db.DecisionReject,
			approvals:  5,
			rejections: 1,
			wantStatus: db.StatusRejected,
		},

		// ConflictFirstWins tests
		{
			name:       "first_wins: first approval wins",
			resolution: ConflictFirstWins,
			request:    &db.Request{MinApprovals: 2},
			decision:   db.DecisionApprove,
			approvals:  1,
			rejections: 0,
			wantStatus: db.StatusApproved,
		},
		{
			name:       "first_wins: first rejection wins",
			resolution: ConflictFirstWins,
			request:    &db.Request{MinApprovals: 2},
			decision:   db.DecisionReject,
			approvals:  0,
			rejections: 1,
			wantStatus: db.StatusRejected,
		},
		{
			name:       "first_wins: subsequent reviews ignored",
			resolution: ConflictFirstWins,
			request:    &db.Request{MinApprovals: 2},
			decision:   db.DecisionApprove,
			approvals:  2,
			rejections: 1,
			wantStatus: "",
		},

		// ConflictHumanBreaksTie tests
		{
			name:       "human_breaks_tie: mixed reviews escalate",
			resolution: ConflictHumanBreaksTie,
			request:    &db.Request{MinApprovals: 2},
			decision:   db.DecisionApprove,
			approvals:  1,
			rejections: 1,
			wantStatus: db.StatusEscalated,
		},
		{
			name:       "human_breaks_tie: enough approvals",
			resolution: ConflictHumanBreaksTie,
			request:    &db.Request{MinApprovals: 2},
			decision:   db.DecisionApprove,
			approvals:  2,
			rejections: 0,
			wantStatus: db.StatusApproved,
		},
		{
			name:       "human_breaks_tie: pure rejection",
			resolution: ConflictHumanBreaksTie,
			request:    &db.Request{MinApprovals: 2},
			decision:   db.DecisionReject,
			approvals:  0,
			rejections: 1,
			wantStatus: db.StatusRejected,
		},
		{
			name:       "human_breaks_tie: not enough approvals yet",
			resolution: ConflictHumanBreaksTie,
			request:    &db.Request{MinApprovals: 3},
			decision:   db.DecisionApprove,
			approvals:  2,
			rejections: 0,
			wantStatus: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			config := ReviewConfig{ConflictResolution: tc.resolution}
			rs := NewReviewService(dbConn, config)
			got := rs.determineNewStatus(tc.request, tc.decision, tc.approvals, tc.rejections)
			if got != tc.wantStatus {
				t.Errorf("determineNewStatus() = %q, want %q", got, tc.wantStatus)
			}
		})
	}
}

func TestVerifyReview(t *testing.T) {
	// Create a review with known values - use valid hex strings for keys
	sessionKey := "deadbeef0123456789abcdef0123456789abcdef0123456789abcdef01234567"
	wrongKey := "cafebabe0123456789abcdef0123456789abcdef0123456789abcdef01234567"
	requestID := "req-abc-123"
	decision := db.DecisionApprove
	timestamp := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)

	// Compute expected signature
	expectedSig := db.ComputeReviewSignature(sessionKey, requestID, decision, timestamp)

	tests := []struct {
		name       string
		review     *db.Review
		sessionKey string
		want       bool
	}{
		{
			name: "valid signature",
			review: &db.Review{
				RequestID:          requestID,
				Decision:           decision,
				Signature:          expectedSig,
				SignatureTimestamp: timestamp,
			},
			sessionKey: sessionKey,
			want:       true,
		},
		{
			name: "wrong session key",
			review: &db.Review{
				RequestID:          requestID,
				Decision:           decision,
				Signature:          expectedSig,
				SignatureTimestamp: timestamp,
			},
			sessionKey: wrongKey,
			want:       false,
		},
		{
			name: "tampered request ID",
			review: &db.Review{
				RequestID:          "tampered-id",
				Decision:           decision,
				Signature:          expectedSig,
				SignatureTimestamp: timestamp,
			},
			sessionKey: sessionKey,
			want:       false,
		},
		{
			name: "tampered decision",
			review: &db.Review{
				RequestID:          requestID,
				Decision:           db.DecisionReject,
				Signature:          expectedSig,
				SignatureTimestamp: timestamp,
			},
			sessionKey: sessionKey,
			want:       false,
		},
		{
			name: "tampered timestamp",
			review: &db.Review{
				RequestID:          requestID,
				Decision:           decision,
				Signature:          expectedSig,
				SignatureTimestamp: timestamp.Add(time.Hour),
			},
			sessionKey: sessionKey,
			want:       false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := VerifyReview(tc.review, tc.sessionKey)
			if got != tc.want {
				t.Errorf("VerifyReview() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCanReview(t *testing.T) {
	t.Run("session not found", func(t *testing.T) {
		dbConn, _, req := setupReviewTest(t)
		defer dbConn.Close()

		rs := NewReviewService(dbConn, DefaultReviewConfig())
		can, reason := rs.CanReview("nonexistent-session", req.ID)
		if can {
			t.Error("Expected can=false for nonexistent session")
		}
		if reason == "" {
			t.Error("Expected reason to be set")
		}
	})

	t.Run("request not found", func(t *testing.T) {
		dbConn, sess, _ := setupReviewTest(t)
		defer dbConn.Close()

		rs := NewReviewService(dbConn, DefaultReviewConfig())
		can, reason := rs.CanReview(sess.ID, "nonexistent-request")
		if can {
			t.Error("Expected can=false for nonexistent request")
		}
		if reason == "" {
			t.Error("Expected reason to be set")
		}
	})

	t.Run("request not pending", func(t *testing.T) {
		dbConn, _, req := setupReviewTest(t)
		defer dbConn.Close()

		// Create a different reviewer
		reviewerSess := &db.Session{
			AgentName:   "GreenLake",
			Program:     "claude-code",
			Model:       "opus-4.5",
			ProjectPath: "/test/project",
		}
		if err := dbConn.CreateSession(reviewerSess); err != nil {
			t.Fatalf("CreateSession() error = %v", err)
		}

		// Mark request as approved
		if err := dbConn.UpdateRequestStatus(req.ID, db.StatusApproved); err != nil {
			t.Fatalf("UpdateRequestStatus() error = %v", err)
		}

		rs := NewReviewService(dbConn, DefaultReviewConfig())
		can, reason := rs.CanReview(reviewerSess.ID, req.ID)
		if can {
			t.Error("Expected can=false for non-pending request")
		}
		if reason == "" {
			t.Error("Expected reason to be set")
		}
	})

	t.Run("self review not trusted", func(t *testing.T) {
		dbConn, sess, req := setupReviewTest(t)
		defer dbConn.Close()

		rs := NewReviewService(dbConn, DefaultReviewConfig())
		can, reason := rs.CanReview(sess.ID, req.ID)
		if can {
			t.Error("Expected can=false for self-review without trust")
		}
		if reason == "" {
			t.Error("Expected reason to be set")
		}
	})

	t.Run("self review trusted but no delay", func(t *testing.T) {
		dbConn, sess, req := setupReviewTest(t)
		defer dbConn.Close()

		config := ReviewConfig{
			TrustedSelfApprove:      []string{sess.AgentName},
			TrustedSelfApproveDelay: 5 * time.Minute,
		}
		rs := NewReviewService(dbConn, config)
		can, reason := rs.CanReview(sess.ID, req.ID)
		if can {
			t.Error("Expected can=false for self-review without delay")
		}
		if reason == "" {
			t.Error("Expected reason to be set")
		}
	})

	t.Run("already reviewed", func(t *testing.T) {
		dbConn, _, req := setupReviewTest(t)
		defer dbConn.Close()

		// Create a reviewer
		reviewerSess := &db.Session{
			AgentName:   "GreenLake",
			Program:     "claude-code",
			Model:       "opus-4.5",
			ProjectPath: "/test/project",
		}
		if err := dbConn.CreateSession(reviewerSess); err != nil {
			t.Fatalf("CreateSession() error = %v", err)
		}

		// Submit a review
		review := &db.Review{
			RequestID:          req.ID,
			ReviewerSessionID:  reviewerSess.ID,
			ReviewerAgent:      reviewerSess.AgentName,
			ReviewerModel:      reviewerSess.Model,
			Decision:           db.DecisionApprove,
			Signature:          "test-sig",
			SignatureTimestamp: time.Now(),
		}
		if err := dbConn.CreateReview(review); err != nil {
			t.Fatalf("CreateReview() error = %v", err)
		}

		rs := NewReviewService(dbConn, DefaultReviewConfig())
		can, reason := rs.CanReview(reviewerSess.ID, req.ID)
		if can {
			t.Error("Expected can=false for already reviewed")
		}
		if reason == "" {
			t.Error("Expected reason to be set")
		}
	})

	t.Run("can review success", func(t *testing.T) {
		dbConn, _, req := setupReviewTest(t)
		defer dbConn.Close()

		// Create a different reviewer
		reviewerSess := &db.Session{
			AgentName:   "GreenLake",
			Program:     "claude-code",
			Model:       "opus-4.5",
			ProjectPath: "/test/project",
		}
		if err := dbConn.CreateSession(reviewerSess); err != nil {
			t.Fatalf("CreateSession() error = %v", err)
		}

		rs := NewReviewService(dbConn, DefaultReviewConfig())
		can, reason := rs.CanReview(reviewerSess.ID, req.ID)
		if !can {
			t.Errorf("Expected can=true, got reason: %s", reason)
		}
		if reason != "" {
			t.Errorf("Expected empty reason, got: %s", reason)
		}
	})
}

func TestSubmitReview_ValidationErrors(t *testing.T) {
	dbConn, _, req := setupReviewTest(t)
	defer dbConn.Close()

	rs := NewReviewService(dbConn, DefaultReviewConfig())

	t.Run("empty session_id", func(t *testing.T) {
		_, err := rs.SubmitReview(ReviewOptions{
			SessionID:  "",
			SessionKey: "key",
			RequestID:  req.ID,
			Decision:   db.DecisionApprove,
		})
		if err == nil || err.Error() != "session_id is required" {
			t.Errorf("expected session_id required error, got %v", err)
		}
	})

	t.Run("empty request_id", func(t *testing.T) {
		_, err := rs.SubmitReview(ReviewOptions{
			SessionID:  "session",
			SessionKey: "key",
			RequestID:  "",
			Decision:   db.DecisionApprove,
		})
		if err == nil || err.Error() != "request_id is required" {
			t.Errorf("expected request_id required error, got %v", err)
		}
	})

	t.Run("invalid decision", func(t *testing.T) {
		_, err := rs.SubmitReview(ReviewOptions{
			SessionID:  "session",
			SessionKey: "key",
			RequestID:  req.ID,
			Decision:   db.Decision("invalid"),
		})
		if err != ErrInvalidDecision {
			t.Errorf("expected ErrInvalidDecision, got %v", err)
		}
	})
}

func TestSubmitReview_SessionErrors(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	t.Run("session not found", func(t *testing.T) {
		dbConn, _, req := setupReviewTest(t)
		defer dbConn.Close()

		rs := NewReviewService(dbConn, DefaultReviewConfig())
		_, err := rs.SubmitReview(ReviewOptions{
			SessionID:  "nonexistent",
			SessionKey: "key",
			RequestID:  req.ID,
			Decision:   db.DecisionApprove,
		})
		if err == nil {
			t.Error("expected error for nonexistent session")
		}
	})

	t.Run("session inactive", func(t *testing.T) {
		dbConn, _, req := setupReviewTest(t)
		defer dbConn.Close()

		// Create an inactive session
		sess := &db.Session{
			AgentName:   "GreenLake",
			Program:     "claude-code",
			Model:       "opus-4.5",
			ProjectPath: "/test/project",
		}
		if err := dbConn.CreateSession(sess); err != nil {
			t.Fatalf("CreateSession() error = %v", err)
		}
		// End the session
		if err := dbConn.EndSession(sess.ID); err != nil {
			t.Fatalf("EndSession() error = %v", err)
		}

		rs := NewReviewService(dbConn, DefaultReviewConfig())
		_, err := rs.SubmitReview(ReviewOptions{
			SessionID:  sess.ID,
			SessionKey: sess.SessionKey,
			RequestID:  req.ID,
			Decision:   db.DecisionApprove,
		})
		if err != ErrSessionInactive {
			t.Errorf("expected ErrSessionInactive, got %v", err)
		}
	})
}

func TestSubmitReview_RequestErrors(t *testing.T) {
	t.Run("request not pending", func(t *testing.T) {
		dbConn, _, req := setupReviewTest(t)
		defer dbConn.Close()

		// Create a reviewer
		reviewer := &db.Session{
			AgentName:   "GreenLake",
			Program:     "claude-code",
			Model:       "opus-4.5",
			ProjectPath: "/test/project",
		}
		if err := dbConn.CreateSession(reviewer); err != nil {
			t.Fatalf("CreateSession() error = %v", err)
		}

		// Mark request as approved
		if err := dbConn.UpdateRequestStatus(req.ID, db.StatusApproved); err != nil {
			t.Fatalf("UpdateRequestStatus() error = %v", err)
		}

		rs := NewReviewService(dbConn, DefaultReviewConfig())
		_, err := rs.SubmitReview(ReviewOptions{
			SessionID:  reviewer.ID,
			SessionKey: reviewer.SessionKey,
			RequestID:  req.ID,
			Decision:   db.DecisionApprove,
		})
		if err == nil {
			t.Error("expected error for non-pending request")
		}
	})
}

func TestSubmitReview_SelfReview(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	t.Run("self review rejected for untrusted agent", func(t *testing.T) {
		dbConn, sess, req := setupReviewTest(t)
		defer dbConn.Close()

		rs := NewReviewService(dbConn, DefaultReviewConfig())
		_, err := rs.SubmitReview(ReviewOptions{
			SessionID:  sess.ID,
			SessionKey: sess.SessionKey,
			RequestID:  req.ID,
			Decision:   db.DecisionApprove,
		})
		if err != ErrSelfReview {
			t.Errorf("expected ErrSelfReview, got %v", err)
		}
	})

	t.Run("trusted self-approve requires delay", func(t *testing.T) {
		dbConn, sess, req := setupReviewTest(t)
		defer dbConn.Close()

		config := ReviewConfig{
			TrustedSelfApprove:      []string{sess.AgentName},
			TrustedSelfApproveDelay: 5 * time.Minute,
		}
		rs := NewReviewService(dbConn, config)

		_, err := rs.SubmitReview(ReviewOptions{
			SessionID:  sess.ID,
			SessionKey: sess.SessionKey,
			RequestID:  req.ID,
			Decision:   db.DecisionApprove,
		})
		if err == nil {
			t.Error("expected error for trusted self-approve without delay")
		}
	})

	t.Run("trusted self-approve after delay succeeds", func(t *testing.T) {
		dbConn, sess, req := setupReviewTest(t)
		defer dbConn.Close()

		// Backdate request to simulate delay
		old := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
		if _, err := dbConn.Exec(`UPDATE requests SET created_at = ? WHERE id = ?`, old, req.ID); err != nil {
			t.Fatalf("failed to backdate request: %v", err)
		}

		// Update RequireDifferentModel to false so this can succeed
		if _, err := dbConn.Exec(`UPDATE requests SET require_different_model = 0 WHERE id = ?`, req.ID); err != nil {
			t.Fatalf("failed to update request: %v", err)
		}

		config := ReviewConfig{
			TrustedSelfApprove:      []string{sess.AgentName},
			TrustedSelfApproveDelay: 5 * time.Minute,
		}
		rs := NewReviewService(dbConn, config)

		result, err := rs.SubmitReview(ReviewOptions{
			SessionID:  sess.ID,
			SessionKey: sess.SessionKey,
			RequestID:  req.ID,
			Decision:   db.DecisionApprove,
		})
		if err != nil {
			t.Fatalf("SubmitReview() error = %v", err)
		}
		if result.Review == nil {
			t.Error("expected review to be created")
		}
	})
}

func TestSubmitReview_AlreadyReviewed(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	dbConn, _, req := setupReviewTest(t)
	defer dbConn.Close()

	// Set MinApprovals=2 so request stays pending after first review
	req.MinApprovals = 2
	if _, err := dbConn.Exec(`UPDATE requests SET min_approvals = ? WHERE id = ?`, req.MinApprovals, req.ID); err != nil {
		t.Fatalf("failed to update min_approvals: %v", err)
	}

	// Create a reviewer
	reviewer := &db.Session{
		AgentName:   "GreenLake",
		Program:     "claude-code",
		Model:       "opus-4.5",
		ProjectPath: "/test/project",
	}
	if err := dbConn.CreateSession(reviewer); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	rs := NewReviewService(dbConn, DefaultReviewConfig())

	// Submit first review
	_, err := rs.SubmitReview(ReviewOptions{
		SessionID:  reviewer.ID,
		SessionKey: reviewer.SessionKey,
		RequestID:  req.ID,
		Decision:   db.DecisionApprove,
	})
	if err != nil {
		t.Fatalf("First SubmitReview() error = %v", err)
	}

	// Verify request is still pending (needs more approvals)
	updatedReq, err := dbConn.GetRequest(req.ID)
	if err != nil {
		t.Fatalf("GetRequest() error = %v", err)
	}
	if updatedReq.Status != db.StatusPending {
		t.Fatalf("expected request to still be pending, got %s", updatedReq.Status)
	}

	// Try to submit another review from same reviewer
	_, err = rs.SubmitReview(ReviewOptions{
		SessionID:  reviewer.ID,
		SessionKey: reviewer.SessionKey,
		RequestID:  req.ID,
		Decision:   db.DecisionReject,
	})
	if err != ErrAlreadyReviewed {
		t.Errorf("expected ErrAlreadyReviewed, got %v", err)
	}
}

func TestSubmitReview_Rejection(t *testing.T) {
	dbConn, _, req := setupReviewTest(t)
	defer dbConn.Close()

	// Create a reviewer with different model
	reviewer := &db.Session{
		AgentName:   "GreenLake",
		Program:     "claude-code",
		Model:       "opus-4.5",
		ProjectPath: "/test/project",
	}
	if err := dbConn.CreateSession(reviewer); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	rs := NewReviewService(dbConn, DefaultReviewConfig())
	result, err := rs.SubmitReview(ReviewOptions{
		SessionID:  reviewer.ID,
		SessionKey: reviewer.SessionKey,
		RequestID:  req.ID,
		Decision:   db.DecisionReject,
		Comments:   "Dangerous command",
	})
	if err != nil {
		t.Fatalf("SubmitReview() error = %v", err)
	}

	if result.Review == nil {
		t.Fatal("expected review to be created")
	}
	if result.Review.Decision != db.DecisionReject {
		t.Errorf("expected decision=reject, got %s", result.Review.Decision)
	}
	if !result.RequestStatusChanged {
		t.Error("expected request status to change")
	}
	if result.NewRequestStatus != db.StatusRejected {
		t.Errorf("expected status=rejected, got %s", result.NewRequestStatus)
	}
}

func TestSubmitReview_NotifierCalled(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	t.Run("notifier called on approval", func(t *testing.T) {
		dbConn, _, req := setupReviewTest(t)
		defer dbConn.Close()

		reviewer := &db.Session{
			AgentName:   "GreenLake",
			Program:     "claude-code",
			Model:       "opus-4.5",
			ProjectPath: "/test/project",
		}
		if err := dbConn.CreateSession(reviewer); err != nil {
			t.Fatalf("CreateSession() error = %v", err)
		}

		notifier := &mockRequestNotifier{}
		rs := NewReviewService(dbConn, DefaultReviewConfig())
		rs.SetNotifier(notifier)

		_, err := rs.SubmitReview(ReviewOptions{
			SessionID:  reviewer.ID,
			SessionKey: reviewer.SessionKey,
			RequestID:  req.ID,
			Decision:   db.DecisionApprove,
		})
		if err != nil {
			t.Fatalf("SubmitReview() error = %v", err)
		}

		if !notifier.approvedCalled {
			t.Error("expected NotifyRequestApproved to be called")
		}
	})

	t.Run("notifier called on rejection", func(t *testing.T) {
		dbConn, _, req := setupReviewTest(t)
		defer dbConn.Close()

		reviewer := &db.Session{
			AgentName:   "GreenLake",
			Program:     "claude-code",
			Model:       "opus-4.5",
			ProjectPath: "/test/project",
		}
		if err := dbConn.CreateSession(reviewer); err != nil {
			t.Fatalf("CreateSession() error = %v", err)
		}

		notifier := &mockRequestNotifier{}
		rs := NewReviewService(dbConn, DefaultReviewConfig())
		rs.SetNotifier(notifier)

		_, err := rs.SubmitReview(ReviewOptions{
			SessionID:  reviewer.ID,
			SessionKey: reviewer.SessionKey,
			RequestID:  req.ID,
			Decision:   db.DecisionReject,
		})
		if err != nil {
			t.Fatalf("SubmitReview() error = %v", err)
		}

		if !notifier.rejectedCalled {
			t.Error("expected NotifyRequestRejected to be called")
		}
	})
}

func TestGetReviewStatus(t *testing.T) {
	t.Run("request not found", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		rs := NewReviewService(dbConn, DefaultReviewConfig())
		_, err = rs.GetReviewStatus("nonexistent-request")
		if err == nil {
			t.Error("Expected error for nonexistent request")
		}
	})

	t.Run("success with reviews", func(t *testing.T) {
		dbConn, _, req := setupReviewTest(t)
		defer dbConn.Close()

		// Create reviewers and add reviews
		reviewer1 := &db.Session{
			AgentName:   "GreenLake",
			Program:     "claude-code",
			Model:       "opus-4.5",
			ProjectPath: "/test/project",
		}
		reviewer2 := &db.Session{
			AgentName:   "RedCat",
			Program:     "codex-cli",
			Model:       "gpt-5.1",
			ProjectPath: "/test/project",
		}
		if err := dbConn.CreateSession(reviewer1); err != nil {
			t.Fatalf("CreateSession() error = %v", err)
		}
		if err := dbConn.CreateSession(reviewer2); err != nil {
			t.Fatalf("CreateSession() error = %v", err)
		}

		// Add an approval
		review1 := &db.Review{
			RequestID:          req.ID,
			ReviewerSessionID:  reviewer1.ID,
			ReviewerAgent:      reviewer1.AgentName,
			ReviewerModel:      reviewer1.Model,
			Decision:           db.DecisionApprove,
			Signature:          "sig1",
			SignatureTimestamp: time.Now(),
		}
		if err := dbConn.CreateReview(review1); err != nil {
			t.Fatalf("CreateReview() error = %v", err)
		}

		rs := NewReviewService(dbConn, DefaultReviewConfig())
		status, err := rs.GetReviewStatus(req.ID)
		if err != nil {
			t.Fatalf("GetReviewStatus() error = %v", err)
		}

		if status.RequestStatus != db.StatusPending {
			t.Errorf("Expected status=pending, got %s", status.RequestStatus)
		}
		if status.Approvals != 1 {
			t.Errorf("Expected 1 approval, got %d", status.Approvals)
		}
		if status.Rejections != 0 {
			t.Errorf("Expected 0 rejections, got %d", status.Rejections)
		}
		if status.MinApprovals != 1 {
			t.Errorf("Expected MinApprovals=1, got %d", status.MinApprovals)
		}
		if len(status.Reviews) != 1 {
			t.Errorf("Expected 1 review, got %d", len(status.Reviews))
		}
	})

	t.Run("needs more approvals", func(t *testing.T) {
		dbConn, sess, _ := setupReviewTest(t)
		defer dbConn.Close()

		// Create a request requiring 3 approvals
		req := &db.Request{
			ProjectPath:           "/test/project",
			RequestorSessionID:    sess.ID,
			RequestorAgent:        sess.AgentName,
			RequestorModel:        sess.Model,
			RiskTier:              db.RiskTierDangerous,
			MinApprovals:          3,
			RequireDifferentModel: false,
			Command: db.CommandSpec{
				Raw: "rm -rf ./data",
				Cwd: "/test/project",
			},
			Justification: db.Justification{
				Reason: "Cleaning data",
			},
		}
		if err := dbConn.CreateRequest(req); err != nil {
			t.Fatalf("CreateRequest() error = %v", err)
		}

		// Add one approval
		reviewer := &db.Session{
			AgentName:   "GreenLake",
			Program:     "claude-code",
			Model:       "opus-4.5",
			ProjectPath: "/test/project",
		}
		if err := dbConn.CreateSession(reviewer); err != nil {
			t.Fatalf("CreateSession() error = %v", err)
		}
		review := &db.Review{
			RequestID:          req.ID,
			ReviewerSessionID:  reviewer.ID,
			ReviewerAgent:      reviewer.AgentName,
			ReviewerModel:      reviewer.Model,
			Decision:           db.DecisionApprove,
			Signature:          "sig",
			SignatureTimestamp: time.Now(),
		}
		if err := dbConn.CreateReview(review); err != nil {
			t.Fatalf("CreateReview() error = %v", err)
		}

		rs := NewReviewService(dbConn, DefaultReviewConfig())
		status, err := rs.GetReviewStatus(req.ID)
		if err != nil {
			t.Fatalf("GetReviewStatus() error = %v", err)
		}

		if !status.NeedsMoreApprovals {
			t.Error("Expected NeedsMoreApprovals=true")
		}
		if status.Approvals != 1 {
			t.Errorf("Expected 1 approval, got %d", status.Approvals)
		}
		if status.MinApprovals != 3 {
			t.Errorf("Expected MinApprovals=3, got %d", status.MinApprovals)
		}
	})
}

func TestCanReview_AdditionalCases(t *testing.T) {
	dbConn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open(:memory:) error = %v", err)
	}
	defer dbConn.Close()

	rs := NewReviewService(dbConn, DefaultReviewConfig())

	t.Run("empty session_id returns false", func(t *testing.T) {
		canReview, msg := rs.CanReview("", "req-id")
		if canReview {
			t.Errorf("expected canReview=false for empty session_id, got true")
		}
		if msg == "" {
			t.Error("expected message for empty session_id")
		}
	})

	t.Run("empty request_id returns false", func(t *testing.T) {
		canReview, msg := rs.CanReview("session-id", "")
		if canReview {
			t.Errorf("expected canReview=false for empty request_id, got true")
		}
		if msg == "" {
			t.Error("expected message for empty request_id")
		}
	})

	t.Run("session not found returns false", func(t *testing.T) {
		canReview, msg := rs.CanReview("nonexistent", "req-id")
		if canReview {
			t.Errorf("expected canReview=false for nonexistent session, got true")
		}
		if msg == "" {
			t.Error("expected message for nonexistent session")
		}
	})
}

func TestEscalateDifferentModelTimeout_Errors(t *testing.T) {
	t.Run("request not found returns error", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		rs := NewReviewService(dbConn, DefaultReviewConfig())
		err = rs.EscalateDifferentModelTimeout("nonexistent-request")
		if err == nil {
			t.Error("expected error for nonexistent request")
		}
	})
}

func TestGetReviewStatus_Errors(t *testing.T) {
	t.Run("request not found returns error", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		rs := NewReviewService(dbConn, DefaultReviewConfig())
		_, err = rs.GetReviewStatus("nonexistent-request")
		if err == nil {
			t.Error("expected error for nonexistent request")
		}
	})
}

func TestSubmitCrossProjectReviewAuthorization(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	sourceProject, targetProject := t.TempDir(), t.TempDir()
	source, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target, err := db.Open(filepath.Join(targetProject, ".slb", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	writeExecutionPolicy(t, targetProject, "[general]\ncross_project_reviews = true\nreview_pool = ['Reviewer']\n[integrations]\nagent_mail_enabled = false\n")

	reviewer := &db.Session{AgentName: "Reviewer", Program: "codex-cli", Model: "model-b", ProjectPath: sourceProject}
	if err := source.CreateSession(reviewer); err != nil {
		t.Fatal(err)
	}
	requestor := &db.Session{AgentName: "Requestor", Program: "claude-code", Model: "model-a", ProjectPath: targetProject}
	if err := target.CreateSession(requestor); err != nil {
		t.Fatal(err)
	}
	request := &db.Request{
		ProjectPath: targetProject, RequestorSessionID: requestor.ID,
		RequestorAgent: requestor.AgentName, RequestorModel: requestor.Model,
		Command:  db.CommandSpec{Raw: "rm -rf ./build", Cwd: targetProject, Shell: true},
		RiskTier: db.RiskTierDangerous, Status: db.StatusPending, MinApprovals: 1,
		Justification: db.Justification{Reason: "test delegated review"},
	}
	if err := target.CreateRequest(request); err != nil {
		t.Fatal(err)
	}
	opts := ReviewOptions{
		SessionID: reviewer.ID, SessionKey: reviewer.SessionKey, RequestID: request.ID,
		Decision: db.DecisionApprove,
	}

	disabled := NewReviewService(target, DefaultReviewConfig())
	if _, err := disabled.SubmitCrossProjectReview(source, sourceProject, opts); !errors.Is(err, ErrCrossProjectReviewDenied) {
		t.Fatalf("disabled cross-project review accepted: %v", err)
	}

	cfg := DefaultReviewConfig()
	cfg.CrossProjectReviews = true
	cfg.ReviewPool = []string{"SomeoneElse"}
	service := NewReviewService(target, cfg)
	if _, err := service.SubmitCrossProjectReview(source, sourceProject, opts); !errors.Is(err, ErrCrossProjectReviewDenied) {
		t.Fatalf("reviewer outside target allowlist accepted: %v", err)
	}

	cfg.ReviewPool = []string{"Reviewer"}
	service = NewReviewService(target, cfg)
	badKey := opts
	badKey.SessionKey = "not-the-session-key"
	if _, err := service.SubmitCrossProjectReview(source, sourceProject, badKey); !errors.Is(err, ErrSessionKeyMismatch) {
		t.Fatalf("invalid source key accepted: %v", err)
	}

	result, err := service.SubmitCrossProjectReview(source, sourceProject, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !result.RequestStatusChanged || result.NewRequestStatus != db.StatusApproved {
		t.Fatalf("delegated approval did not resolve request: %+v", result)
	}
	if result.Review.ReviewerAgent != reviewer.AgentName || result.Review.ReviewerModel != reviewer.Model ||
		result.Review.ReviewerSessionID == reviewer.ID {
		t.Fatalf("delegated identity was not preserved safely: %+v", result.Review)
	}
	provenance, err := target.GetSession(result.Review.ReviewerSessionID)
	if err != nil || provenance.EndedAt == nil {
		t.Fatalf("delegated provenance became active capacity: %+v err=%v", provenance, err)
	}
	if provenance.SessionKey == reviewer.SessionKey {
		t.Fatal("target provenance copied the source session credential")
	}
	active, err := target.ListActiveSessions(targetProject)
	if err != nil || len(active) != 1 || active[0].ID != requestor.ID {
		t.Fatalf("target active sessions polluted by delegation: %+v err=%v", active, err)
	}
}

func TestSubmitCrossProjectReviewRejectsEndedSourceSession(t *testing.T) {
	source, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()

	reviewer := &db.Session{AgentName: "Reviewer", Model: "other", ProjectPath: "/source"}
	if err := source.CreateSession(reviewer); err != nil {
		t.Fatal(err)
	}
	if err := source.EndSession(reviewer.ID); err != nil {
		t.Fatal(err)
	}
	requestor := &db.Session{AgentName: "Requestor", Model: "request-model", ProjectPath: "/target"}
	if err := target.CreateSession(requestor); err != nil {
		t.Fatal(err)
	}
	request := &db.Request{
		ProjectPath: "/target", RequestorSessionID: requestor.ID, RequestorAgent: requestor.AgentName,
		RequestorModel: requestor.Model, Command: db.CommandSpec{Raw: "rm -rf ./build", Cwd: "/target"},
		RiskTier: db.RiskTierDangerous, Status: db.StatusPending, MinApprovals: 1,
		Justification: db.Justification{Reason: "test"},
	}
	if err := target.CreateRequest(request); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultReviewConfig()
	cfg.CrossProjectReviews = true
	cfg.ReviewPool = []string{"Reviewer"}
	service := NewReviewService(target, cfg)
	_, err = service.SubmitCrossProjectReview(source, "/source", ReviewOptions{
		SessionID: reviewer.ID, SessionKey: reviewer.SessionKey, RequestID: request.ID, Decision: db.DecisionApprove,
	})
	if !errors.Is(err, ErrSessionInactive) {
		t.Fatalf("ended source session authorized delegated review: %v", err)
	}
}

func TestCrossProjectReviewReauthenticatesAfterTargetWriterWait(t *testing.T) {
	for _, decision := range []db.Decision{db.DecisionApprove, db.DecisionReject} {
		for _, change := range []string{"ended", "replaced", "key rotated", "agent changed", "model changed", "project changed"} {
			t.Run(string(decision)+"/"+change, func(t *testing.T) {
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
				var originalEvents int
				if err := target.QueryRow(`SELECT COUNT(*) FROM request_events WHERE request_id = ?`, request.ID).Scan(&originalEvents); err != nil {
					t.Fatal(err)
				}
				contender, err := db.OpenWithOptions(target.Path(), db.OpenOptions{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = contender.Close() })
				service.db = contender
				writer, err := target.Begin()
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = writer.Rollback() }()
				initialRead := make(chan struct{})
				result := make(chan error, 1)
				opts := ReviewOptions{SessionID: reviewer.ID, SessionKey: reviewer.SessionKey,
					RequestID: request.ID, Decision: decision}
				go func() {
					// This is the same private path as the public DB adapter. The
					// latch observes a completed initial read, not elapsed time.
					_, err := service.submitCrossProjectReview(sourceProject, opts, func(tx *sql.Tx) (*db.Session, error) {
						session, err := source.AuthenticateSession(opts.SessionID, opts.SessionKey)
						if tx == nil {
							close(initialRead)
						}
						return session, err
					})
					result <- err
				}()
				select {
				case <-initialRead:
				case <-time.After(5 * time.Second):
					t.Fatal("delegated review did not read the initial source identity")
				}
				want := db.ErrReviewDelegationMismatch
				switch change {
				case "ended":
					err = source.EndSession(reviewer.ID)
					want = ErrSessionInactive
				case "replaced":
					_, err = source.ReplaceSession(reviewer, reviewer.SessionKey, "codex-cli", "replacement-model")
					want = ErrSessionInactive
				case "key rotated":
					digest, ok := db.SessionKeyDigest(strings.Repeat("a", 64))
					if !ok {
						t.Fatal("invalid replacement key fixture")
					}
					_, err = source.Exec(`UPDATE sessions SET session_key_digest = ? WHERE id = ?`, digest, reviewer.ID)
					want = ErrSessionKeyMismatch
				case "agent changed":
					_, err = source.Exec(`UPDATE sessions SET agent_name = 'ChangedReviewer' WHERE id = ?`, reviewer.ID)
				case "model changed":
					_, err = source.Exec(`UPDATE sessions SET model = 'changed-model' WHERE id = ?`, reviewer.ID)
				case "project changed":
					_, err = source.Exec(`UPDATE sessions SET project_path = ? WHERE id = ?`, t.TempDir(), reviewer.ID)
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := writer.Commit(); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-result:
					if !errors.Is(err, want) {
						t.Fatalf("queued delegated review used stale source identity: got %v, want %v", err, want)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("delegated review did not finish after the target writer released")
				}
				stored, reviews, err := target.GetRequestWithReviews(request.ID)
				if err != nil || stored.Status != db.StatusPending || stored.ApprovalExpiresAt != nil || len(reviews) != 0 {
					t.Fatalf("source revocation left review authority: request=%+v reviews=%+v err=%v", stored, reviews, err)
				}
				var provenance int
				if err := target.QueryRow(`SELECT COUNT(*) FROM sessions WHERE program = 'cross-project-review'`).Scan(&provenance); err != nil || provenance != 0 {
					t.Fatalf("source revocation left delegated provenance: count=%d err=%v", provenance, err)
				}
				var events int
				if err := target.QueryRow(`SELECT COUNT(*) FROM request_events WHERE request_id = ?`, request.ID).Scan(&events); err != nil || events != originalEvents {
					t.Fatalf("source revocation changed the request journal: before=%d after=%d err=%v", originalEvents, events, err)
				}
			})
		}
	}
}

func delegatedReviewFixture(t *testing.T, source, target *db.DB, sourceProject, targetProject string) (*db.Session, *db.Request, *ReviewService) {
	t.Helper()
	writeExecutionPolicy(t, targetProject, "[general]\napproval_policy = 'same_host'\ncross_project_reviews = true\nreview_pool = ['Reviewer']\n[integrations]\nagent_mail_enabled = false\n")
	reviewer := &db.Session{AgentName: "Reviewer", Program: "codex-cli", Model: "model-b", ProjectPath: sourceProject}
	if err := source.CreateSession(reviewer); err != nil {
		t.Fatal(err)
	}
	requester := &db.Session{AgentName: "Requester", Program: "claude-code", Model: "model-a", ProjectPath: targetProject}
	if err := target.CreateSession(requester); err != nil {
		t.Fatal(err)
	}
	request := &db.Request{ProjectPath: targetProject, RequestorSessionID: requester.ID,
		RequestorAgent: requester.AgentName, RequestorModel: requester.Model,
		Command:  db.CommandSpec{Raw: "touch reviewed-command", Cwd: targetProject, Shell: true},
		RiskTier: db.RiskTierDangerous, Status: db.StatusPending, MinApprovals: 1,
		RequireDifferentModel: true, ApprovalPolicy: db.ApprovalPolicySameHost,
		Justification: db.Justification{Reason: "test current source identity at delegated review commit"}}
	if err := target.CreateRequest(request); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultReviewConfig()
	cfg.ApprovalPolicy, cfg.CrossProjectReviews, cfg.ReviewPool = db.ApprovalPolicySameHost, true, []string{"Reviewer"}
	return reviewer, request, NewReviewService(target, cfg)
}

func TestCrossProjectReviewSharedStoresPreserveCompletedEvidence(t *testing.T) {
	for _, decision := range []db.Decision{db.DecisionApprove, db.DecisionReject} {
		for _, store := range []string{"same memory handle", "same file handle", "separate shared-file handles", "separate stores"} {
			t.Run(string(decision)+"/"+store, func(t *testing.T) {
				sourceProject, targetProject := t.TempDir(), t.TempDir()
				path := filepath.Join(targetProject, "target.db")
				if store == "same memory handle" {
					path = ":memory:"
				}
				target, err := db.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = target.Close() })
				source := target
				if store == "separate shared-file handles" {
					source, err = db.OpenWithOptions(path, db.OpenOptions{})
				} else if store == "separate stores" {
					source, err = db.Open(filepath.Join(sourceProject, "source.db"))
				}
				if err != nil {
					t.Fatal(err)
				}
				if source != target {
					t.Cleanup(func() { _ = source.Close() })
				}
				reviewer, request, service := delegatedReviewFixture(t, source, target, sourceProject, targetProject)
				result, err := service.SubmitCrossProjectReview(source, sourceProject, ReviewOptions{
					SessionID: reviewer.ID, SessionKey: reviewer.SessionKey, RequestID: request.ID, Decision: decision,
				})
				want := db.StatusApproved
				if decision == db.DecisionReject {
					want = db.StatusRejected
				}
				if err != nil || result == nil || result.NewRequestStatus != want {
					t.Fatalf("valid delegated review failed: %+v %v", result, err)
				}
				before, err := target.GetReview(result.Review.ID)
				if err != nil {
					t.Fatal(err)
				}
				provenance, err := target.GetSession(before.ReviewerSessionID)
				if err != nil || provenance.EndedAt == nil || provenance.ProjectPath != targetProject ||
					before.ApprovalPolicy != db.ApprovalPolicySameHost || before.Assurance != db.AssuranceSameHost {
					t.Fatalf("delegation lost ended, target-scoped same-host provenance: %+v %+v %v", before, provenance, err)
				}
				// Admission must use the current source identity, but a completed
				// review retains its immutable snapshot after the source changes.
				if err := source.EndSession(reviewer.ID); err != nil {
					t.Fatal(err)
				}
				digest, ok := db.SessionKeyDigest(strings.Repeat("b", 64))
				if !ok {
					t.Fatal("invalid replacement key fixture")
				}
				if _, err := source.Exec(`UPDATE sessions SET session_key_digest = ?, model = 'later-model' WHERE id = ?`, digest, reviewer.ID); err != nil {
					t.Fatal(err)
				}
				after, err := target.GetReview(before.ID)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("source changes rewrote signed review evidence: before=%+v after=%+v err=%v", before, after, err)
				}
				storedProvenance, err := target.GetSession(provenance.ID)
				if err != nil || !reflect.DeepEqual(provenance, storedProvenance) {
					t.Fatalf("source changes rewrote completed provenance: before=%+v after=%+v err=%v", provenance, storedProvenance, err)
				}
				if decision == db.DecisionApprove {
					if _, err := target.VerifyRequestApproval(request.ID); err != nil {
						t.Fatalf("source changes invalidated an immutable completed review: %v", err)
					}
				}
			})
		}
	}
}
