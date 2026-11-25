// Package db tests for execution outcome CRUD operations.
package db

import (
	"math"
	"testing"
	"time"
)

func TestCreateOutcome(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	_, req := createTestRequest(t, db)

	// Execute the request to make it valid for outcomes
	db.UpdateRequestStatus(req.ID, StatusApproved)
	db.UpdateRequestStatus(req.ID, StatusExecuting)
	db.UpdateRequestStatus(req.ID, StatusExecuted)

	outcome := &ExecutionOutcome{
		RequestID:          req.ID,
		CausedProblems:     false,
		ProblemDescription: "",
		HumanNotes:         "Worked as expected",
	}

	if err := db.CreateOutcome(outcome); err != nil {
		t.Fatalf("CreateOutcome failed: %v", err)
	}

	if outcome.ID == 0 {
		t.Error("Expected ID to be generated")
	}

	if outcome.CreatedAt.IsZero() {
		t.Error("Expected CreatedAt to be set")
	}
}

func TestGetOutcome(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	_, req := createTestRequest(t, db)
	db.UpdateRequestStatus(req.ID, StatusApproved)
	db.UpdateRequestStatus(req.ID, StatusExecuting)
	db.UpdateRequestStatus(req.ID, StatusExecuted)

	rating := 4
	original := &ExecutionOutcome{
		RequestID:          req.ID,
		CausedProblems:     true,
		ProblemDescription: "Minor issue",
		HumanRating:        &rating,
		HumanNotes:         "Needed manual fix",
	}
	db.CreateOutcome(original)

	retrieved, err := db.GetOutcome(original.ID)
	if err != nil {
		t.Fatalf("GetOutcome failed: %v", err)
	}

	if retrieved.ID != original.ID {
		t.Errorf("ID mismatch: got %d, want %d", retrieved.ID, original.ID)
	}
	if retrieved.RequestID != req.ID {
		t.Errorf("RequestID mismatch: got %s, want %s", retrieved.RequestID, req.ID)
	}
	if !retrieved.CausedProblems {
		t.Error("Expected CausedProblems to be true")
	}
	if retrieved.ProblemDescription != "Minor issue" {
		t.Errorf("ProblemDescription mismatch: got %s", retrieved.ProblemDescription)
	}
	if retrieved.HumanRating == nil || *retrieved.HumanRating != 4 {
		t.Error("HumanRating mismatch")
	}
}

func TestGetOutcomeNotFound(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	_, err := db.GetOutcome(99999)
	if err != ErrOutcomeNotFound {
		t.Errorf("Expected ErrOutcomeNotFound, got: %v", err)
	}
}

func TestGetOutcomeForRequest(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	_, req := createTestRequest(t, db)
	db.UpdateRequestStatus(req.ID, StatusApproved)
	db.UpdateRequestStatus(req.ID, StatusExecuting)
	db.UpdateRequestStatus(req.ID, StatusExecuted)

	original := &ExecutionOutcome{
		RequestID:      req.ID,
		CausedProblems: false,
		HumanNotes:     "All good",
	}
	db.CreateOutcome(original)

	retrieved, err := db.GetOutcomeForRequest(req.ID)
	if err != nil {
		t.Fatalf("GetOutcomeForRequest failed: %v", err)
	}

	if retrieved.RequestID != req.ID {
		t.Errorf("RequestID mismatch: got %s, want %s", retrieved.RequestID, req.ID)
	}
}

func TestListOutcomes(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create multiple requests and outcomes
	for i := 0; i < 3; i++ {
		_, req := createTestRequest(t, db)
		db.UpdateRequestStatus(req.ID, StatusApproved)
		db.UpdateRequestStatus(req.ID, StatusExecuting)
		db.UpdateRequestStatus(req.ID, StatusExecuted)

		outcome := &ExecutionOutcome{
			RequestID:      req.ID,
			CausedProblems: i%2 == 0,
		}
		db.CreateOutcome(outcome)
		// Small delay to ensure different timestamps
		time.Sleep(10 * time.Millisecond)
	}

	outcomes, err := db.ListOutcomes(10)
	if err != nil {
		t.Fatalf("ListOutcomes failed: %v", err)
	}

	if len(outcomes) != 3 {
		t.Errorf("Expected 3 outcomes, got %d", len(outcomes))
	}
}

func TestListProblematicOutcomes(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create multiple requests and outcomes
	for i := 0; i < 4; i++ {
		_, req := createTestRequest(t, db)
		db.UpdateRequestStatus(req.ID, StatusApproved)
		db.UpdateRequestStatus(req.ID, StatusExecuting)
		db.UpdateRequestStatus(req.ID, StatusExecuted)

		outcome := &ExecutionOutcome{
			RequestID:      req.ID,
			CausedProblems: i < 2, // First 2 are problematic
		}
		db.CreateOutcome(outcome)
	}

	problematic, err := db.ListProblematicOutcomes(10)
	if err != nil {
		t.Fatalf("ListProblematicOutcomes failed: %v", err)
	}

	if len(problematic) != 2 {
		t.Errorf("Expected 2 problematic outcomes, got %d", len(problematic))
	}

	for _, o := range problematic {
		if !o.CausedProblems {
			t.Error("Expected all outcomes to be problematic")
		}
	}
}

func TestUpdateOutcome(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	_, req := createTestRequest(t, db)
	db.UpdateRequestStatus(req.ID, StatusApproved)
	db.UpdateRequestStatus(req.ID, StatusExecuting)
	db.UpdateRequestStatus(req.ID, StatusExecuted)

	outcome := &ExecutionOutcome{
		RequestID:      req.ID,
		CausedProblems: false,
	}
	db.CreateOutcome(outcome)

	// Update the outcome
	rating := 5
	outcome.CausedProblems = true
	outcome.ProblemDescription = "Found a bug"
	outcome.HumanRating = &rating
	outcome.HumanNotes = "Fixed manually"

	if err := db.UpdateOutcome(outcome); err != nil {
		t.Fatalf("UpdateOutcome failed: %v", err)
	}

	// Verify update
	retrieved, _ := db.GetOutcome(outcome.ID)
	if !retrieved.CausedProblems {
		t.Error("Expected CausedProblems to be updated to true")
	}
	if retrieved.ProblemDescription != "Found a bug" {
		t.Errorf("ProblemDescription mismatch: got %s", retrieved.ProblemDescription)
	}
	if retrieved.HumanRating == nil || *retrieved.HumanRating != 5 {
		t.Error("HumanRating not updated correctly")
	}
}

func TestRecordOutcome(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	_, req := createTestRequest(t, db)
	db.UpdateRequestStatus(req.ID, StatusApproved)
	db.UpdateRequestStatus(req.ID, StatusExecuting)
	db.UpdateRequestStatus(req.ID, StatusExecuted)

	// First record - creates new
	rating := 4
	outcome1, err := db.RecordOutcome(req.ID, false, "", &rating, "First record")
	if err != nil {
		t.Fatalf("RecordOutcome (create) failed: %v", err)
	}

	if outcome1.ID == 0 {
		t.Error("Expected ID to be generated")
	}

	// Second record - updates existing
	rating2 := 3
	outcome2, err := db.RecordOutcome(req.ID, true, "Actually had issues", &rating2, "Updated")
	if err != nil {
		t.Fatalf("RecordOutcome (update) failed: %v", err)
	}

	if outcome2.ID != outcome1.ID {
		t.Errorf("Expected same ID %d, got %d", outcome1.ID, outcome2.ID)
	}
	if !outcome2.CausedProblems {
		t.Error("Expected CausedProblems to be updated")
	}
}

func TestGetOutcomeStats(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create outcomes with various ratings and problems
	for i := 0; i < 5; i++ {
		_, req := createTestRequest(t, db)
		db.UpdateRequestStatus(req.ID, StatusApproved)
		db.UpdateRequestStatus(req.ID, StatusExecuting)
		db.UpdateRequestStatus(req.ID, StatusExecuted)

		var rating *int
		if i < 3 {
			r := i + 3 // ratings: 3, 4, 5
			rating = &r
		}

		outcome := &ExecutionOutcome{
			RequestID:      req.ID,
			CausedProblems: i == 0, // 1 problematic
			HumanRating:    rating,
		}
		db.CreateOutcome(outcome)
	}

	stats, err := db.GetOutcomeStats()
	if err != nil {
		t.Fatalf("GetOutcomeStats failed: %v", err)
	}

	if stats.TotalOutcomes != 5 {
		t.Errorf("Expected 5 total outcomes, got %d", stats.TotalOutcomes)
	}
	if stats.ProblematicCount != 1 {
		t.Errorf("Expected 1 problematic, got %d", stats.ProblematicCount)
	}
	if stats.ProblematicPercent != 20 {
		t.Errorf("Expected 20%% problematic, got %.1f%%", stats.ProblematicPercent)
	}
	if stats.RatedCount != 3 {
		t.Errorf("Expected 3 rated, got %d", stats.RatedCount)
	}
	// Average of 3, 4, 5 = 4.0
	if stats.AvgHumanRating != 4.0 {
		t.Errorf("Expected avg rating 4.0, got %.1f", stats.AvgHumanRating)
	}
}

func TestGetRequestStatsByAgent(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create a session for consistent agent name
	sess := &Session{
		AgentName:   "TestAgent",
		Program:     "claude-code",
		Model:       "opus-4.5",
		ProjectPath: "/test/project",
	}
	db.CreateSession(sess)

	// Create multiple requests with this agent
	for i := 0; i < 3; i++ {
		r := &Request{
			ProjectPath:        "/test/project",
			RequestorSessionID: sess.ID,
			RequestorAgent:     "TestAgent",
			RequestorModel:     "opus-4.5",
			RiskTier:           RiskTierDangerous,
			MinApprovals:       1,
			Command: CommandSpec{
				Raw: "rm -rf ./build",
				Cwd: "/test/project",
			},
			Justification: Justification{
				Reason: "Test request",
			},
		}
		db.CreateRequest(r)

		switch i {
		case 0:
			// First one rejected
			db.UpdateRequestStatus(r.ID, StatusRejected)
		case 1:
			// Second one executed
			db.UpdateRequestStatus(r.ID, StatusApproved)
			db.UpdateRequestStatus(r.ID, StatusExecuting)
			db.UpdateRequestStatus(r.ID, StatusExecuted)

			// Add outcome
			outcome := &ExecutionOutcome{
				RequestID:      r.ID,
				CausedProblems: true,
			}
			db.CreateOutcome(outcome)
		}
		// Third stays pending
	}

	stats, err := db.GetRequestStatsByAgent("TestAgent")
	if err != nil {
		t.Fatalf("GetRequestStatsByAgent failed: %v", err)
	}

	if stats.TotalRequests != 3 {
		t.Errorf("Expected 3 total requests, got %d", stats.TotalRequests)
	}
	if stats.RejectedCount != 1 {
		t.Errorf("Expected 1 rejected, got %d", stats.RejectedCount)
	}
	if stats.ExecutedCount != 1 {
		t.Errorf("Expected 1 executed, got %d", stats.ExecutedCount)
	}
	if stats.ProblematicPct != 100 {
		t.Errorf("Expected 100%% problematic (1/1), got %.1f%%", stats.ProblematicPct)
	}
}

func TestGetTimeToApprovalStats(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create a few requests with approval reviews
	for i := 0; i < 3; i++ {
		_, req := createTestRequest(t, db)

		// Create a reviewer session and approval review
		reviewerSess := &Session{
			AgentName:   "Reviewer" + string(rune('A'+i)),
			Program:     "codex-cli",
			Model:       "gpt-5",
			ProjectPath: "/test/project",
		}
		db.CreateSession(reviewerSess)

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
		}
		db.CreateReview(review)

		db.UpdateRequestStatus(req.ID, StatusApproved)
	}

	stats, err := db.GetTimeToApprovalStats()
	if err != nil {
		t.Fatalf("GetTimeToApprovalStats failed: %v", err)
	}

	if stats.SampleSize != 3 {
		t.Errorf("Expected 3 samples, got %d", stats.SampleSize)
	}
	// Since all were approved nearly instantly, times should be very small
	if stats.AvgMinutes < 0 {
		t.Error("Average minutes should not be negative")
	}
}

func TestOutcomeAndStats_EdgeCases(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Empty outcome stats should return zeros without error.
	outcomeStats, err := db.GetOutcomeStats()
	if err != nil {
		t.Fatalf("GetOutcomeStats failed: %v", err)
	}
	if outcomeStats.TotalOutcomes != 0 {
		t.Fatalf("expected 0 outcomes, got %d", outcomeStats.TotalOutcomes)
	}

	// Empty request stats for agent should return zeros.
	reqStats, err := db.GetRequestStatsByAgent("NoSuchAgent")
	if err != nil {
		t.Fatalf("GetRequestStatsByAgent failed: %v", err)
	}
	if reqStats.TotalRequests != 0 {
		t.Fatalf("expected 0 requests, got %d", reqStats.TotalRequests)
	}

	// Agent with requests but no executed requests should skip problematic percent computation.
	sess := &Session{AgentName: "StatsAgent", Program: "codex-cli", Model: "gpt-5", ProjectPath: "/test/project"}
	if err := db.CreateSession(sess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}
	r := &Request{
		ProjectPath:        sess.ProjectPath,
		RequestorSessionID: sess.ID,
		RequestorAgent:     sess.AgentName,
		RequestorModel:     sess.Model,
		RiskTier:           RiskTierDangerous,
		MinApprovals:       1,
		Command:            CommandSpec{Raw: "rm -rf ./build", Cwd: sess.ProjectPath},
		Justification:      Justification{Reason: "stats"},
	}
	if err := db.CreateRequest(r); err != nil {
		t.Fatalf("CreateRequest failed: %v", err)
	}
	if err := db.UpdateRequestStatus(r.ID, StatusRejected); err != nil {
		t.Fatalf("UpdateRequestStatus failed: %v", err)
	}

	reqStats, err = db.GetRequestStatsByAgent(sess.AgentName)
	if err != nil {
		t.Fatalf("GetRequestStatsByAgent failed: %v", err)
	}
	if reqStats.TotalRequests != 1 {
		t.Fatalf("expected 1 request, got %d", reqStats.TotalRequests)
	}
	if reqStats.ExecutedCount != 0 {
		t.Fatalf("expected 0 executed, got %d", reqStats.ExecutedCount)
	}
	if reqStats.ProblematicPct != 0 {
		t.Fatalf("expected 0 problematic pct when none executed, got %.1f", reqStats.ProblematicPct)
	}

	// Outcome stats when there are outcomes but none have ratings.
	_, execReq := createTestRequest(t, db)
	db.UpdateRequestStatus(execReq.ID, StatusApproved)
	db.UpdateRequestStatus(execReq.ID, StatusExecuting)
	db.UpdateRequestStatus(execReq.ID, StatusExecuted)
	if err := db.CreateOutcome(&ExecutionOutcome{RequestID: execReq.ID, CausedProblems: false}); err != nil {
		t.Fatalf("CreateOutcome failed: %v", err)
	}

	outcomeStats, err = db.GetOutcomeStats()
	if err != nil {
		t.Fatalf("GetOutcomeStats failed: %v", err)
	}
	if outcomeStats.TotalOutcomes != 1 {
		t.Fatalf("expected 1 outcome, got %d", outcomeStats.TotalOutcomes)
	}
	if outcomeStats.RatedCount != 0 {
		t.Fatalf("expected 0 rated outcomes, got %d", outcomeStats.RatedCount)
	}

	// Default limit branches.
	if _, err := db.ListOutcomes(0); err != nil {
		t.Fatalf("ListOutcomes(0) failed: %v", err)
	}
	if _, err := db.ListProblematicOutcomes(0); err != nil {
		t.Fatalf("ListProblematicOutcomes(0) failed: %v", err)
	}

	// UpdateOutcome should return ErrOutcomeNotFound when missing.
	if err := db.UpdateOutcome(&ExecutionOutcome{ID: 9999999}); err != ErrOutcomeNotFound {
		t.Fatalf("expected ErrOutcomeNotFound, got %v", err)
	}
}

func TestGetTimeToApprovalStats_Empty(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	stats, err := db.GetTimeToApprovalStats()
	if err != nil {
		t.Fatalf("GetTimeToApprovalStats failed: %v", err)
	}
	if stats.SampleSize != 0 {
		t.Fatalf("expected 0 samples, got %d", stats.SampleSize)
	}
}

func TestGetTimeToApprovalStats_EvenMedian(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	project := "/test/project"
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	// Two approved requests with different approval delays.
	_, req1 := createTestRequest(t, db)
	reviewer1 := &Session{AgentName: "ReviewerEven1", Program: "codex-cli", Model: "gpt-5", ProjectPath: project}
	if err := db.CreateSession(reviewer1); err != nil {
		t.Fatalf("CreateSession reviewer1 failed: %v", err)
	}
	sig1 := ComputeReviewSignature(reviewer1.SessionKey, req1.ID, DecisionApprove, base)
	if err := db.CreateReview(&Review{
		RequestID:          req1.ID,
		ReviewerSessionID:  reviewer1.ID,
		ReviewerAgent:      reviewer1.AgentName,
		ReviewerModel:      reviewer1.Model,
		Decision:           DecisionApprove,
		Signature:          sig1,
		SignatureTimestamp: base,
	}); err != nil {
		t.Fatalf("CreateReview req1 failed: %v", err)
	}
	if err := db.UpdateRequestStatus(req1.ID, StatusApproved); err != nil {
		t.Fatalf("UpdateRequestStatus req1 failed: %v", err)
	}

	_, req2 := createTestRequest(t, db)
	reviewer2 := &Session{AgentName: "ReviewerEven2", Program: "codex-cli", Model: "gpt-5", ProjectPath: project}
	if err := db.CreateSession(reviewer2); err != nil {
		t.Fatalf("CreateSession reviewer2 failed: %v", err)
	}
	sig2 := ComputeReviewSignature(reviewer2.SessionKey, req2.ID, DecisionApprove, base)
	if err := db.CreateReview(&Review{
		RequestID:          req2.ID,
		ReviewerSessionID:  reviewer2.ID,
		ReviewerAgent:      reviewer2.AgentName,
		ReviewerModel:      reviewer2.Model,
		Decision:           DecisionApprove,
		Signature:          sig2,
		SignatureTimestamp: base,
	}); err != nil {
		t.Fatalf("CreateReview req2 failed: %v", err)
	}
	if err := db.UpdateRequestStatus(req2.ID, StatusApproved); err != nil {
		t.Fatalf("UpdateRequestStatus req2 failed: %v", err)
	}

	// Force deterministic created_at timestamps so the durations are predictable.
	if _, err := db.Exec(`UPDATE requests SET created_at = ? WHERE id = ?`, base.Format(time.RFC3339), req1.ID); err != nil {
		t.Fatalf("update req1 created_at failed: %v", err)
	}
	if _, err := db.Exec(`UPDATE requests SET created_at = ? WHERE id = ?`, base.Format(time.RFC3339), req2.ID); err != nil {
		t.Fatalf("update req2 created_at failed: %v", err)
	}
	if _, err := db.Exec(`UPDATE reviews SET created_at = ? WHERE request_id = ?`, base.Add(10*time.Minute).Format(time.RFC3339), req1.ID); err != nil {
		t.Fatalf("update req1 review created_at failed: %v", err)
	}
	if _, err := db.Exec(`UPDATE reviews SET created_at = ? WHERE request_id = ?`, base.Add(30*time.Minute).Format(time.RFC3339), req2.ID); err != nil {
		t.Fatalf("update req2 review created_at failed: %v", err)
	}

	stats, err := db.GetTimeToApprovalStats()
	if err != nil {
		t.Fatalf("GetTimeToApprovalStats failed: %v", err)
	}
	if stats.SampleSize != 2 {
		t.Fatalf("expected 2 samples, got %d", stats.SampleSize)
	}

	// Median of [10,30] is 20.
	if math.Abs(stats.MedianMinutes-20) > 0.01 {
		t.Fatalf("MedianMinutes=%.3f want ~20", stats.MedianMinutes)
	}
	if stats.MinMinutes > stats.MedianMinutes || stats.MedianMinutes > stats.MaxMinutes {
		t.Fatalf("expected min<=median<=max, got min=%.3f median=%.3f max=%.3f", stats.MinMinutes, stats.MedianMinutes, stats.MaxMinutes)
	}
}
