package core

import (
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
