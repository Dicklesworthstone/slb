// Package db tests for review CRUD operations.
package db

import (
	"strings"
	"testing"
	"time"
)

func TestCreateReview(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	sess, req := createTestRequest(t, db)

	// Create a different reviewer session
	reviewerSess := &Session{
		AgentName:   "BlueDog",
		Program:     "codex-cli",
		Model:       "gpt-5",
		ProjectPath: "/test/project",
	}
	if err := db.CreateSession(reviewerSess); err != nil {
		t.Fatalf("CreateSession for reviewer failed: %v", err)
	}

	// Create a review
	now := time.Now().UTC()
	signature := ComputeReviewSignature(reviewerSess.SessionKey, req.ID, DecisionApprove, now)

	review := &Review{
		RequestID:          req.ID,
		ReviewerSessionID:  reviewerSess.ID,
		ReviewerAgent:      reviewerSess.AgentName,
		ReviewerModel:      reviewerSess.Model,
		Decision:           DecisionApprove,
		Signature:          signature,
		SignatureTimestamp: now,
		Comments:           "LGTM",
	}

	if err := db.CreateReview(review); err != nil {
		t.Fatalf("CreateReview failed: %v", err)
	}

	// Verify UUID was generated
	if review.ID == "" {
		t.Error("Expected UUID to be generated")
	}

	// Verify timestamps were set
	if review.CreatedAt.IsZero() {
		t.Error("Expected CreatedAt to be set")
	}

	_ = sess // unused but needed for request creation
}

func TestCreateReviewDuplicate(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	_, req := createTestRequest(t, db)

	// Create a reviewer session
	reviewerSess := &Session{
		AgentName:   "BlueDog",
		Program:     "codex-cli",
		Model:       "gpt-5",
		ProjectPath: "/test/project",
	}
	if err := db.CreateSession(reviewerSess); err != nil {
		t.Fatalf("CreateSession for reviewer failed: %v", err)
	}

	// Create first review
	now := time.Now().UTC()
	signature := ComputeReviewSignature(reviewerSess.SessionKey, req.ID, DecisionApprove, now)

	review1 := &Review{
		RequestID:          req.ID,
		ReviewerSessionID:  reviewerSess.ID,
		ReviewerAgent:      reviewerSess.AgentName,
		ReviewerModel:      reviewerSess.Model,
		Decision:           DecisionApprove,
		Signature:          signature,
		SignatureTimestamp: now,
	}
	if err := db.CreateReview(review1); err != nil {
		t.Fatalf("CreateReview first failed: %v", err)
	}

	// Try to create duplicate review
	review2 := &Review{
		RequestID:          req.ID,
		ReviewerSessionID:  reviewerSess.ID,
		ReviewerAgent:      reviewerSess.AgentName,
		ReviewerModel:      reviewerSess.Model,
		Decision:           DecisionReject,
		Signature:          signature,
		SignatureTimestamp: now,
	}
	err := db.CreateReview(review2)
	if err != ErrReviewExists {
		t.Errorf("Expected ErrReviewExists, got: %v", err)
	}
}

func TestGetReview(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	_, req := createTestRequest(t, db)

	// Create a reviewer session and review
	reviewerSess := &Session{
		AgentName:   "BlueDog",
		Program:     "codex-cli",
		Model:       "gpt-5",
		ProjectPath: "/test/project",
	}
	db.CreateSession(reviewerSess)

	now := time.Now().UTC()
	signature := ComputeReviewSignature(reviewerSess.SessionKey, req.ID, DecisionApprove, now)

	original := &Review{
		RequestID:          req.ID,
		ReviewerSessionID:  reviewerSess.ID,
		ReviewerAgent:      reviewerSess.AgentName,
		ReviewerModel:      reviewerSess.Model,
		Decision:           DecisionApprove,
		Signature:          signature,
		SignatureTimestamp: now,
		Comments:           "Approved",
	}
	db.CreateReview(original)

	// Retrieve and verify
	retrieved, err := db.GetReview(original.ID)
	if err != nil {
		t.Fatalf("GetReview failed: %v", err)
	}

	if retrieved.ID != original.ID {
		t.Errorf("ID mismatch: got %s, want %s", retrieved.ID, original.ID)
	}
	if retrieved.Decision != DecisionApprove {
		t.Errorf("Decision mismatch: got %s, want %s", retrieved.Decision, DecisionApprove)
	}
	if retrieved.Comments != "Approved" {
		t.Errorf("Comments mismatch: got %s, want %s", retrieved.Comments, "Approved")
	}
}

func TestGetReviewNotFound(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	_, err := db.GetReview("nonexistent-id")
	if err != ErrReviewNotFound {
		t.Errorf("Expected ErrReviewNotFound, got: %v", err)
	}
}

func TestListReviewsForRequest(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	_, req := createTestRequest(t, db)

	// Create multiple reviewer sessions and reviews
	for i, agent := range []string{"BlueDog", "RedCat", "GreenBear"} {
		sess := &Session{
			AgentName:   agent,
			Program:     "codex-cli",
			Model:       "gpt-5",
			ProjectPath: "/test/project",
		}
		db.CreateSession(sess)

		now := time.Now().UTC().Add(time.Duration(i) * time.Second)
		signature := ComputeReviewSignature(sess.SessionKey, req.ID, DecisionApprove, now)

		review := &Review{
			RequestID:          req.ID,
			ReviewerSessionID:  sess.ID,
			ReviewerAgent:      sess.AgentName,
			ReviewerModel:      sess.Model,
			Decision:           DecisionApprove,
			Signature:          signature,
			SignatureTimestamp: now,
		}
		db.CreateReview(review)
	}

	reviews, err := db.ListReviewsForRequest(req.ID)
	if err != nil {
		t.Fatalf("ListReviewsForRequest failed: %v", err)
	}

	if len(reviews) != 3 {
		t.Errorf("Expected 3 reviews, got %d", len(reviews))
	}
}

func TestCountReviewsByDecision(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	_, req := createTestRequest(t, db)

	// Create reviewers with different decisions
	decisions := []Decision{DecisionApprove, DecisionApprove, DecisionReject}
	for i, decision := range decisions {
		sess := &Session{
			AgentName:   "Agent" + string(rune('A'+i)),
			Program:     "codex-cli",
			Model:       "gpt-5",
			ProjectPath: "/test/project",
		}
		db.CreateSession(sess)

		now := time.Now().UTC()
		signature := ComputeReviewSignature(sess.SessionKey, req.ID, decision, now)

		review := &Review{
			RequestID:          req.ID,
			ReviewerSessionID:  sess.ID,
			ReviewerAgent:      sess.AgentName,
			ReviewerModel:      sess.Model,
			Decision:           decision,
			Signature:          signature,
			SignatureTimestamp: now,
		}
		db.CreateReview(review)
	}

	approvals, rejections, err := db.CountReviewsByDecision(req.ID)
	if err != nil {
		t.Fatalf("CountReviewsByDecision failed: %v", err)
	}

	if approvals != 2 {
		t.Errorf("Expected 2 approvals, got %d", approvals)
	}
	if rejections != 1 {
		t.Errorf("Expected 1 rejection, got %d", rejections)
	}
}

func TestComputeReviewSignature(t *testing.T) {
	sessionKey := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	requestID := "test-request-id"
	decision := DecisionApprove
	timestamp := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	sig1 := ComputeReviewSignature(sessionKey, requestID, decision, timestamp)
	if sig1 == "" {
		t.Error("Expected non-empty signature")
	}

	// Same inputs should produce same signature
	sig2 := ComputeReviewSignature(sessionKey, requestID, decision, timestamp)
	if sig1 != sig2 {
		t.Error("Expected same signature for same inputs")
	}

	// Different decision should produce different signature
	sig3 := ComputeReviewSignature(sessionKey, requestID, DecisionReject, timestamp)
	if sig1 == sig3 {
		t.Error("Expected different signature for different decision")
	}

	// Verify signature
	if !VerifyReviewSignature(sessionKey, requestID, decision, timestamp, sig1) {
		t.Error("Expected signature to verify")
	}

	// Wrong signature should fail
	if VerifyReviewSignature(sessionKey, requestID, decision, timestamp, "wrong-signature") {
		t.Error("Expected wrong signature to fail verification")
	}
}

func TestHasDifferentModelApproval(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	_, req := createTestRequest(t, db)

	// Create reviewers with same and different models
	models := []string{"opus-4.5", "gpt-5", "opus-4.5"}
	for i, model := range models {
		sess := &Session{
			AgentName:   "Agent" + string(rune('A'+i)),
			Program:     "codex-cli",
			Model:       model,
			ProjectPath: "/test/project",
		}
		db.CreateSession(sess)

		now := time.Now().UTC()
		signature := ComputeReviewSignature(sess.SessionKey, req.ID, DecisionApprove, now)

		review := &Review{
			RequestID:          req.ID,
			ReviewerSessionID:  sess.ID,
			ReviewerAgent:      sess.AgentName,
			ReviewerModel:      model,
			Decision:           DecisionApprove,
			Signature:          signature,
			SignatureTimestamp: now,
		}
		db.CreateReview(review)
	}

	// Should find different model approval
	hasDiff, err := db.HasDifferentModelApproval(req.ID, "opus-4.5")
	if err != nil {
		t.Fatalf("HasDifferentModelApproval failed: %v", err)
	}
	if !hasDiff {
		t.Error("Expected to find different model approval")
	}

	// Should not find different model if excluding the only different one
	hasDiff, err = db.HasDifferentModelApproval(req.ID, "gpt-5")
	if err != nil {
		t.Fatalf("HasDifferentModelApproval failed: %v", err)
	}
	if !hasDiff {
		t.Error("Expected to find different model approval (opus-4.5)")
	}
}

func TestCheckRequestApprovalStatus(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	_, req := createTestRequest(t, db)

	// Initially should not be approved or rejected
	approved, rejected, err := db.CheckRequestApprovalStatus(req.ID)
	if err != nil {
		t.Fatalf("CheckRequestApprovalStatus failed: %v", err)
	}
	if approved || rejected {
		t.Error("Expected neither approved nor rejected initially")
	}

	// Add an approval
	sess1 := &Session{
		AgentName:   "BlueDog",
		Program:     "codex-cli",
		Model:       "gpt-5",
		ProjectPath: "/test/project",
	}
	db.CreateSession(sess1)

	now := time.Now().UTC()
	signature := ComputeReviewSignature(sess1.SessionKey, req.ID, DecisionApprove, now)

	review := &Review{
		RequestID:          req.ID,
		ReviewerSessionID:  sess1.ID,
		ReviewerAgent:      sess1.AgentName,
		ReviewerModel:      sess1.Model,
		Decision:           DecisionApprove,
		Signature:          signature,
		SignatureTimestamp: now,
	}
	db.CreateReview(review)

	// Request requires 1 approval, should be approved now
	approved, rejected, err = db.CheckRequestApprovalStatus(req.ID)
	if err != nil {
		t.Fatalf("CheckRequestApprovalStatus failed: %v", err)
	}
	if !approved {
		t.Error("Expected approved after 1 approval (min is 1)")
	}
	if rejected {
		t.Error("Expected not rejected")
	}
}

func TestIsRequestorSameAsReviewer(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	sess, req := createTestRequest(t, db)

	// Same session should return true
	same, err := db.IsRequestorSameAsReviewer(req.ID, sess.ID)
	if err != nil {
		t.Fatalf("IsRequestorSameAsReviewer failed: %v", err)
	}
	if !same {
		t.Error("Expected same session to return true")
	}

	// Different session should return false
	sess2 := &Session{
		AgentName:   "BlueDog",
		Program:     "codex-cli",
		Model:       "gpt-5",
		ProjectPath: "/test/project",
	}
	db.CreateSession(sess2)

	same, err = db.IsRequestorSameAsReviewer(req.ID, sess2.ID)
	if err != nil {
		t.Fatalf("IsRequestorSameAsReviewer failed: %v", err)
	}
	if same {
		t.Error("Expected different session to return false")
	}
}

func TestIsRequestorSameAsReviewer_RequestNotFound(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	_, err := db.IsRequestorSameAsReviewer("nonexistent-request", "sess")
	if err != ErrRequestNotFound {
		t.Fatalf("expected ErrRequestNotFound, got %v", err)
	}
}

func TestCreateReview_ForeignKeyError(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	reviewerSess := &Session{
		AgentName:   "ReviewerFK",
		Program:     "codex-cli",
		Model:       "gpt-5",
		ProjectPath: "/test/project",
	}
	if err := db.CreateSession(reviewerSess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	now := time.Now().UTC()
	sig := ComputeReviewSignature(reviewerSess.SessionKey, "missing-request", DecisionApprove, now)
	review := &Review{
		RequestID:          "missing-request",
		ReviewerSessionID:  reviewerSess.ID,
		ReviewerAgent:      reviewerSess.AgentName,
		ReviewerModel:      reviewerSess.Model,
		Decision:           DecisionApprove,
		Signature:          sig,
		SignatureTimestamp: now,
	}
	err := db.CreateReview(review)
	if err == nil {
		t.Fatalf("expected foreign key error")
	}
}

func TestScanReviewList_BadRows(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	_, req := createTestRequest(t, db)
	reviewerSess := &Session{
		AgentName:   "ReviewerBadRows",
		Program:     "codex-cli",
		Model:       "gpt-5",
		ProjectPath: "/test/project",
	}
	if err := db.CreateSession(reviewerSess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	now := time.Now().UTC()
	sig := ComputeReviewSignature(reviewerSess.SessionKey, req.ID, DecisionApprove, now)
	if err := db.CreateReview(&Review{
		RequestID:          req.ID,
		ReviewerSessionID:  reviewerSess.ID,
		ReviewerAgent:      reviewerSess.AgentName,
		ReviewerModel:      reviewerSess.Model,
		Decision:           DecisionApprove,
		Signature:          sig,
		SignatureTimestamp: now,
	}); err != nil {
		t.Fatalf("CreateReview failed: %v", err)
	}

	rows, err := db.Query(`SELECT id FROM reviews`)
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}
	defer rows.Close()

	if _, err := scanReviewList(rows); err == nil {
		t.Fatalf("expected scanReviewList to fail with wrong column count")
	}
}

func TestCreateReviewWithValidation_ApproveRejectAndValidationErrors(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Approve path (min approvals = 1).
	_, req := createTestRequest(t, db)
	reviewer := &Session{
		AgentName:   "Reviewer1",
		Program:     "codex-cli",
		Model:       "gpt-5",
		ProjectPath: "/test/project",
	}
	if err := db.CreateSession(reviewer); err != nil {
		t.Fatalf("CreateSession reviewer failed: %v", err)
	}
	now := time.Now().UTC()
	sig := ComputeReviewSignature(reviewer.SessionKey, req.ID, DecisionApprove, now)
	review := &Review{
		RequestID:          req.ID,
		ReviewerSessionID:  reviewer.ID,
		ReviewerAgent:      reviewer.AgentName,
		ReviewerModel:      reviewer.Model,
		Decision:           DecisionApprove,
		Signature:          sig,
		SignatureTimestamp: now,
	}
	if err := db.CreateReviewWithValidation(review, reviewer.SessionKey); err != nil {
		t.Fatalf("CreateReviewWithValidation approve failed: %v", err)
	}
	updated, err := db.GetRequest(req.ID)
	if err != nil {
		t.Fatalf("GetRequest failed: %v", err)
	}
	if updated.Status != StatusApproved {
		t.Fatalf("Status=%s want %s", updated.Status, StatusApproved)
	}

	// Reject path.
	_, req2 := createTestRequest(t, db)
	reviewer2 := &Session{AgentName: "Reviewer2", Program: "codex-cli", Model: "gpt-5", ProjectPath: "/test/project"}
	if err := db.CreateSession(reviewer2); err != nil {
		t.Fatalf("CreateSession reviewer2 failed: %v", err)
	}
	now2 := time.Now().UTC()
	sig2 := ComputeReviewSignature(reviewer2.SessionKey, req2.ID, DecisionReject, now2)
	review2 := &Review{
		RequestID:          req2.ID,
		ReviewerSessionID:  reviewer2.ID,
		ReviewerAgent:      reviewer2.AgentName,
		ReviewerModel:      reviewer2.Model,
		Decision:           DecisionReject,
		Signature:          sig2,
		SignatureTimestamp: now2,
	}
	if err := db.CreateReviewWithValidation(review2, reviewer2.SessionKey); err != nil {
		t.Fatalf("CreateReviewWithValidation reject failed: %v", err)
	}
	updated2, err := db.GetRequest(req2.ID)
	if err != nil {
		t.Fatalf("GetRequest failed: %v", err)
	}
	if updated2.Status != StatusRejected {
		t.Fatalf("Status=%s want %s", updated2.Status, StatusRejected)
	}

	// Self-review is blocked.
	requestor, req3 := createTestRequest(t, db)
	selfReview := &Review{
		RequestID:          req3.ID,
		ReviewerSessionID:  requestor.ID,
		ReviewerAgent:      requestor.AgentName,
		ReviewerModel:      requestor.Model,
		Decision:           DecisionApprove,
		Signature:          "ignored",
		SignatureTimestamp: time.Now().UTC(),
	}
	if err := db.CreateReviewWithValidation(selfReview, requestor.SessionKey); err != ErrSelfReview {
		t.Fatalf("expected ErrSelfReview, got %v", err)
	}

	// Invalid signature is rejected.
	_, req4 := createTestRequest(t, db)
	reviewer4 := &Session{AgentName: "Reviewer4", Program: "codex-cli", Model: "gpt-5", ProjectPath: "/test/project"}
	if err := db.CreateSession(reviewer4); err != nil {
		t.Fatalf("CreateSession reviewer4 failed: %v", err)
	}
	invalid := &Review{
		RequestID:          req4.ID,
		ReviewerSessionID:  reviewer4.ID,
		ReviewerAgent:      reviewer4.AgentName,
		ReviewerModel:      reviewer4.Model,
		Decision:           DecisionApprove,
		Signature:          "bad",
		SignatureTimestamp: time.Now().UTC(),
	}
	if err := db.CreateReviewWithValidation(invalid, reviewer4.SessionKey); err != ErrInvalidSignature {
		t.Fatalf("expected ErrInvalidSignature, got %v", err)
	}

	// Request must be pending.
	_, req5 := createTestRequest(t, db)
	if err := db.UpdateRequestStatus(req5.ID, StatusApproved); err != nil {
		t.Fatalf("UpdateRequestStatus failed: %v", err)
	}
	reviewer5 := &Session{AgentName: "Reviewer5", Program: "codex-cli", Model: "gpt-5", ProjectPath: "/test/project"}
	if err := db.CreateSession(reviewer5); err != nil {
		t.Fatalf("CreateSession reviewer5 failed: %v", err)
	}
	now5 := time.Now().UTC()
	sig5 := ComputeReviewSignature(reviewer5.SessionKey, req5.ID, DecisionApprove, now5)
	notPending := &Review{
		RequestID:          req5.ID,
		ReviewerSessionID:  reviewer5.ID,
		ReviewerAgent:      reviewer5.AgentName,
		ReviewerModel:      reviewer5.Model,
		Decision:           DecisionApprove,
		Signature:          sig5,
		SignatureTimestamp: now5,
	}
	if err := db.CreateReviewWithValidation(notPending, reviewer5.SessionKey); err == nil || !strings.Contains(err.Error(), "request is not pending") {
		t.Fatalf("expected not-pending error, got %v", err)
	}
}

func TestCreateReviewWithValidation_RequireDifferentModel(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	project := "/test/project"

	requestor := &Session{AgentName: "Req", Program: "codex-cli", Model: "opus-4.5", ProjectPath: project}
	if err := db.CreateSession(requestor); err != nil {
		t.Fatalf("CreateSession requestor failed: %v", err)
	}

	req := &Request{
		ProjectPath:           project,
		RequestorSessionID:    requestor.ID,
		RequestorAgent:        requestor.AgentName,
		RequestorModel:        requestor.Model,
		RiskTier:              RiskTierDangerous,
		MinApprovals:          1,
		RequireDifferentModel: true,
		Command:               CommandSpec{Raw: "rm -rf ./build", Cwd: project},
		Justification:         Justification{Reason: "test"},
	}
	if err := db.CreateRequest(req); err != nil {
		t.Fatalf("CreateRequest failed: %v", err)
	}

	sameModel := &Session{AgentName: "Same", Program: "codex-cli", Model: "opus-4.5", ProjectPath: project}
	if err := db.CreateSession(sameModel); err != nil {
		t.Fatalf("CreateSession sameModel failed: %v", err)
	}

	ts1 := time.Now().UTC()
	sig1 := ComputeReviewSignature(sameModel.SessionKey, req.ID, DecisionApprove, ts1)
	r1 := &Review{
		RequestID:          req.ID,
		ReviewerSessionID:  sameModel.ID,
		ReviewerAgent:      sameModel.AgentName,
		ReviewerModel:      sameModel.Model,
		Decision:           DecisionApprove,
		Signature:          sig1,
		SignatureTimestamp: ts1,
	}
	if err := db.CreateReviewWithValidation(r1, sameModel.SessionKey); err != nil {
		t.Fatalf("CreateReviewWithValidation (same model) failed: %v", err)
	}
	stillPending, err := db.GetRequest(req.ID)
	if err != nil {
		t.Fatalf("GetRequest failed: %v", err)
	}
	if stillPending.Status != StatusPending {
		t.Fatalf("Status=%s want %s", stillPending.Status, StatusPending)
	}

	diffModel := &Session{AgentName: "Diff", Program: "codex-cli", Model: "gpt-5", ProjectPath: project}
	if err := db.CreateSession(diffModel); err != nil {
		t.Fatalf("CreateSession diffModel failed: %v", err)
	}

	ts2 := time.Now().UTC()
	sig2 := ComputeReviewSignature(diffModel.SessionKey, req.ID, DecisionApprove, ts2)
	r2 := &Review{
		RequestID:          req.ID,
		ReviewerSessionID:  diffModel.ID,
		ReviewerAgent:      diffModel.AgentName,
		ReviewerModel:      diffModel.Model,
		Decision:           DecisionApprove,
		Signature:          sig2,
		SignatureTimestamp: ts2,
	}
	if err := db.CreateReviewWithValidation(r2, diffModel.SessionKey); err != nil {
		t.Fatalf("CreateReviewWithValidation (different model) failed: %v", err)
	}
	approved, err := db.GetRequest(req.ID)
	if err != nil {
		t.Fatalf("GetRequest failed: %v", err)
	}
	if approved.Status != StatusApproved {
		t.Fatalf("Status=%s want %s", approved.Status, StatusApproved)
	}
}
