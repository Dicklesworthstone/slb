package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/daemon"
	"github.com/Dicklesworthstone/slb/internal/db"
)

// =============================================================================
// SAFETY-CRITICAL TESTS for shouldAutoApproveCaution
// =============================================================================
//
// These tests are MANDATORY for the security of the SLB system.
// The shouldAutoApproveCaution function guards against unauthorized command
// execution. Every decision branch MUST be tested.
//
// Test coverage requirement: 100%
// =============================================================================

// TestShouldAutoApproveCaution_HappyPath verifies that a CAUTION tier
// request in pending status is eligible for auto-approval.
func TestShouldAutoApproveCaution_HappyPath(t *testing.T) {
	decision := shouldAutoApproveCaution(db.StatusPending, db.RiskTierCaution)

	if !decision.ShouldApprove {
		t.Errorf("expected ShouldApprove=true for pending CAUTION request, got false")
	}
	if decision.Reason == "" {
		t.Error("expected non-empty reason for approval decision")
	}
}

// TestShouldAutoApproveCaution_NotPending_Approved verifies that already-approved
// requests are NOT auto-approved again.
func TestShouldAutoApproveCaution_NotPending_Approved(t *testing.T) {
	decision := shouldAutoApproveCaution(db.StatusApproved, db.RiskTierCaution)

	if decision.ShouldApprove {
		t.Errorf("expected ShouldApprove=false for approved request, got true")
	}
	if decision.Reason == "" {
		t.Error("expected non-empty reason explaining denial")
	}
}

// TestShouldAutoApproveCaution_NotPending_Rejected verifies that rejected
// requests are NOT auto-approved.
func TestShouldAutoApproveCaution_NotPending_Rejected(t *testing.T) {
	decision := shouldAutoApproveCaution(db.StatusRejected, db.RiskTierCaution)

	if decision.ShouldApprove {
		t.Errorf("expected ShouldApprove=false for rejected request, got true")
	}
}

// TestShouldAutoApproveCaution_NotPending_Executed verifies that executed
// requests are NOT auto-approved.
func TestShouldAutoApproveCaution_NotPending_Executed(t *testing.T) {
	decision := shouldAutoApproveCaution(db.StatusExecuted, db.RiskTierCaution)

	if decision.ShouldApprove {
		t.Errorf("expected ShouldApprove=false for executed request, got true")
	}
}

// TestShouldAutoApproveCaution_NotPending_Timeout verifies that timed-out
// requests are NOT auto-approved.
func TestShouldAutoApproveCaution_NotPending_Timeout(t *testing.T) {
	decision := shouldAutoApproveCaution(db.StatusTimeout, db.RiskTierCaution)

	if decision.ShouldApprove {
		t.Errorf("expected ShouldApprove=false for timed out request, got true")
	}
}

// TestShouldAutoApproveCaution_NotPending_Cancelled verifies that cancelled
// requests are NOT auto-approved.
func TestShouldAutoApproveCaution_NotPending_Cancelled(t *testing.T) {
	decision := shouldAutoApproveCaution(db.StatusCancelled, db.RiskTierCaution)

	if decision.ShouldApprove {
		t.Errorf("expected ShouldApprove=false for cancelled request, got true")
	}
}

// TestShouldAutoApproveCaution_NotPending_ExecutionFailed verifies that
// failed execution requests are NOT auto-approved.
func TestShouldAutoApproveCaution_NotPending_ExecutionFailed(t *testing.T) {
	decision := shouldAutoApproveCaution(db.StatusExecutionFailed, db.RiskTierCaution)

	if decision.ShouldApprove {
		t.Errorf("expected ShouldApprove=false for execution-failed request, got true")
	}
}

// =============================================================================
// CRITICAL SECURITY TESTS: Dangerous tiers must NEVER be auto-approved
// =============================================================================

// TestShouldAutoApproveCaution_NotCaution_Dangerous is a CRITICAL security test.
// DANGEROUS tier commands MUST NEVER be auto-approved as they can cause
// significant harm (e.g., rm -rf, DROP TABLE, etc.)
func TestShouldAutoApproveCaution_NotCaution_Dangerous(t *testing.T) {
	decision := shouldAutoApproveCaution(db.StatusPending, db.RiskTierDangerous)

	if decision.ShouldApprove {
		t.Fatalf("SECURITY VIOLATION: DANGEROUS tier request was approved for auto-approval!")
	}
	if decision.Reason == "" {
		t.Error("expected non-empty reason explaining denial for dangerous tier")
	}
}

// TestShouldAutoApproveCaution_NotCaution_Critical is a CRITICAL security test.
// CRITICAL tier commands MUST NEVER be auto-approved as they pose extreme risk
// (e.g., system destruction, data loss, security breaches).
func TestShouldAutoApproveCaution_NotCaution_Critical(t *testing.T) {
	decision := shouldAutoApproveCaution(db.StatusPending, db.RiskTierCritical)

	if decision.ShouldApprove {
		t.Fatalf("SECURITY VIOLATION: CRITICAL tier request was approved for auto-approval!")
	}
	if decision.Reason == "" {
		t.Error("expected non-empty reason explaining denial for critical tier")
	}
}

// TestShouldAutoApproveCaution_NotCaution_Safe verifies that SAFE tier
// commands are NOT handled by auto-approve (they don't need approval at all).
func TestShouldAutoApproveCaution_NotCaution_Safe(t *testing.T) {
	// Safe tier uses a different constant
	decision := shouldAutoApproveCaution(db.StatusPending, db.RiskTier("safe"))

	if decision.ShouldApprove {
		t.Errorf("expected ShouldApprove=false for SAFE tier (should bypass approval entirely)")
	}
}

// =============================================================================
// Edge case and combination tests
// =============================================================================

// TestShouldAutoApproveCaution_DangerousAndNotPending verifies that even if
// a request is both not pending AND dangerous, we correctly reject it.
func TestShouldAutoApproveCaution_DangerousAndNotPending(t *testing.T) {
	decision := shouldAutoApproveCaution(db.StatusApproved, db.RiskTierDangerous)

	if decision.ShouldApprove {
		t.Errorf("expected ShouldApprove=false for non-pending dangerous request")
	}
	// The first check (not pending) should trigger
}

// TestShouldAutoApproveCaution_CriticalAndTimeout verifies rejection for
// critical tier requests that have timed out.
func TestShouldAutoApproveCaution_CriticalAndTimeout(t *testing.T) {
	decision := shouldAutoApproveCaution(db.StatusTimeout, db.RiskTierCritical)

	if decision.ShouldApprove {
		t.Errorf("expected ShouldApprove=false for timed-out critical request")
	}
}

// TestShouldAutoApproveCaution_ReasonContainsTier verifies that the reason
// includes the actual tier when rejecting based on tier.
func TestShouldAutoApproveCaution_ReasonContainsTier(t *testing.T) {
	decision := shouldAutoApproveCaution(db.StatusPending, db.RiskTierDangerous)

	if decision.ShouldApprove {
		t.Fatal("expected rejection for dangerous tier")
	}
	if decision.Reason == "" {
		t.Error("expected non-empty reason")
	}
	// The reason should mention the tier for debugging purposes
	if !contains(decision.Reason, "dangerous") && !contains(decision.Reason, "tier") {
		t.Errorf("expected reason to mention tier, got: %s", decision.Reason)
	}
}

// TestShouldAutoApproveCaution_ReasonContainsStatus verifies that the reason
// includes the actual status when rejecting based on status.
func TestShouldAutoApproveCaution_ReasonContainsStatus(t *testing.T) {
	decision := shouldAutoApproveCaution(db.StatusRejected, db.RiskTierCaution)

	if decision.ShouldApprove {
		t.Fatal("expected rejection for non-pending status")
	}
	if decision.Reason == "" {
		t.Error("expected non-empty reason")
	}
	// The reason should mention the status for debugging purposes
	if !contains(decision.Reason, "rejected") && !contains(decision.Reason, "pending") {
		t.Errorf("expected reason to mention status, got: %s", decision.Reason)
	}
}

// TestAutoApproveDecision_StructFields verifies the AutoApproveDecision struct
// can be properly initialized and accessed.
func TestAutoApproveDecision_StructFields(t *testing.T) {
	d := AutoApproveDecision{
		ShouldApprove: true,
		Reason:        "test reason",
	}

	if !d.ShouldApprove {
		t.Error("expected ShouldApprove to be true")
	}
	if d.Reason != "test reason" {
		t.Errorf("expected Reason='test reason', got %q", d.Reason)
	}
}

// =============================================================================
// Table-driven comprehensive test
// =============================================================================

// TestShouldAutoApproveCaution_AllCombinations is a comprehensive table-driven
// test that verifies ALL status/tier combinations behave correctly.
func TestShouldAutoApproveCaution_AllCombinations(t *testing.T) {
	statuses := []db.RequestStatus{
		db.StatusPending,
		db.StatusApproved,
		db.StatusRejected,
		db.StatusExecuted,
		db.StatusExecutionFailed,
		db.StatusTimeout,
		db.StatusCancelled,
	}

	tiers := []db.RiskTier{
		db.RiskTierCaution,
		db.RiskTierDangerous,
		db.RiskTierCritical,
		db.RiskTier("safe"),
	}

	for _, status := range statuses {
		for _, tier := range tiers {
			t.Run(string(status)+"_"+string(tier), func(t *testing.T) {
				decision := shouldAutoApproveCaution(status, tier)

				// ONLY pending + caution should be approved
				expectedApprove := status == db.StatusPending && tier == db.RiskTierCaution

				if decision.ShouldApprove != expectedApprove {
					t.Errorf(
						"status=%s tier=%s: expected ShouldApprove=%v, got %v (reason: %s)",
						status, tier, expectedApprove, decision.ShouldApprove, decision.Reason,
					)
				}

				// Reason should never be empty
				if decision.Reason == "" {
					t.Errorf("status=%s tier=%s: reason should not be empty", status, tier)
				}
			})
		}
	}
}

// =============================================================================
// TESTS for evaluateRequestForPolling
// =============================================================================
//
// These tests verify the polling business logic is correct.
// Every decision branch MUST be tested.
//
// Test coverage requirement: 100%
// =============================================================================

// TestEvaluateRequestForPolling_NewRequest verifies that a request not in the
// seen map is identified as new and should emit a pending event.
func TestEvaluateRequestForPolling_NewRequest(t *testing.T) {
	seen := make(map[string]db.RequestStatus)
	result := evaluateRequestForPolling("req-123", db.StatusPending, seen)

	if result.Action != PollActionEmitNew {
		t.Errorf("expected Action=PollActionEmitNew for new request, got %v", result.Action)
	}
	if result.EventType != "request_pending" {
		t.Errorf("expected EventType='request_pending', got %q", result.EventType)
	}
	if result.Reason == "" {
		t.Error("expected non-empty reason")
	}
}

// TestEvaluateRequestForPolling_StatusUnchanged verifies that a request with
// unchanged status is skipped.
func TestEvaluateRequestForPolling_StatusUnchanged(t *testing.T) {
	seen := map[string]db.RequestStatus{
		"req-123": db.StatusPending,
	}
	result := evaluateRequestForPolling("req-123", db.StatusPending, seen)

	if result.Action != PollActionSkip {
		t.Errorf("expected Action=PollActionSkip for unchanged status, got %v", result.Action)
	}
	if result.Reason == "" {
		t.Error("expected non-empty reason")
	}
}

// TestEvaluateRequestForPolling_StatusChangedToApproved verifies that a status
// change to approved emits the correct event.
func TestEvaluateRequestForPolling_StatusChangedToApproved(t *testing.T) {
	seen := map[string]db.RequestStatus{
		"req-123": db.StatusPending,
	}
	result := evaluateRequestForPolling("req-123", db.StatusApproved, seen)

	if result.Action != PollActionEmitStatusChange {
		t.Errorf("expected Action=PollActionEmitStatusChange, got %v", result.Action)
	}
	if result.EventType != "request_approved" {
		t.Errorf("expected EventType='request_approved', got %q", result.EventType)
	}
}

// TestEvaluateRequestForPolling_StatusChangedToRejected verifies rejection events.
func TestEvaluateRequestForPolling_StatusChangedToRejected(t *testing.T) {
	seen := map[string]db.RequestStatus{
		"req-123": db.StatusPending,
	}
	result := evaluateRequestForPolling("req-123", db.StatusRejected, seen)

	if result.Action != PollActionEmitStatusChange {
		t.Errorf("expected Action=PollActionEmitStatusChange, got %v", result.Action)
	}
	if result.EventType != "request_rejected" {
		t.Errorf("expected EventType='request_rejected', got %q", result.EventType)
	}
}

// TestEvaluateRequestForPolling_StatusChangedToExecuted verifies execution events.
func TestEvaluateRequestForPolling_StatusChangedToExecuted(t *testing.T) {
	seen := map[string]db.RequestStatus{
		"req-123": db.StatusApproved,
	}
	result := evaluateRequestForPolling("req-123", db.StatusExecuted, seen)

	if result.Action != PollActionEmitStatusChange {
		t.Errorf("expected Action=PollActionEmitStatusChange, got %v", result.Action)
	}
	if result.EventType != "request_executed" {
		t.Errorf("expected EventType='request_executed', got %q", result.EventType)
	}
}

// TestEvaluateRequestForPolling_StatusChangedToExecutionFailed verifies failed execution.
func TestEvaluateRequestForPolling_StatusChangedToExecutionFailed(t *testing.T) {
	seen := map[string]db.RequestStatus{
		"req-123": db.StatusApproved,
	}
	result := evaluateRequestForPolling("req-123", db.StatusExecutionFailed, seen)

	if result.Action != PollActionEmitStatusChange {
		t.Errorf("expected Action=PollActionEmitStatusChange, got %v", result.Action)
	}
	if result.EventType != "request_executed" {
		t.Errorf("expected EventType='request_executed' for failed execution, got %q", result.EventType)
	}
}

// TestEvaluateRequestForPolling_StatusChangedToTimeout verifies timeout events.
func TestEvaluateRequestForPolling_StatusChangedToTimeout(t *testing.T) {
	seen := map[string]db.RequestStatus{
		"req-123": db.StatusPending,
	}
	result := evaluateRequestForPolling("req-123", db.StatusTimeout, seen)

	if result.Action != PollActionEmitStatusChange {
		t.Errorf("expected Action=PollActionEmitStatusChange, got %v", result.Action)
	}
	if result.EventType != "request_timeout" {
		t.Errorf("expected EventType='request_timeout', got %q", result.EventType)
	}
}

// TestEvaluateRequestForPolling_StatusChangedToCancelled verifies cancellation events.
func TestEvaluateRequestForPolling_StatusChangedToCancelled(t *testing.T) {
	seen := map[string]db.RequestStatus{
		"req-123": db.StatusPending,
	}
	result := evaluateRequestForPolling("req-123", db.StatusCancelled, seen)

	if result.Action != PollActionEmitStatusChange {
		t.Errorf("expected Action=PollActionEmitStatusChange, got %v", result.Action)
	}
	if result.EventType != "request_cancelled" {
		t.Errorf("expected EventType='request_cancelled', got %q", result.EventType)
	}
}

// TestEvaluateRequestForPolling_UnknownStatusTransition verifies unknown status is skipped.
func TestEvaluateRequestForPolling_UnknownStatusTransition(t *testing.T) {
	seen := map[string]db.RequestStatus{
		"req-123": db.StatusPending,
	}
	// Use an unknown status
	result := evaluateRequestForPolling("req-123", db.RequestStatus("unknown"), seen)

	if result.Action != PollActionSkip {
		t.Errorf("expected Action=PollActionSkip for unknown status, got %v", result.Action)
	}
	if result.Reason == "" {
		t.Error("expected non-empty reason for unknown status")
	}
}

// TestEvaluateRequestForPolling_ReasonContainsStatusInfo verifies the reason is informative.
func TestEvaluateRequestForPolling_ReasonContainsStatusInfo(t *testing.T) {
	seen := map[string]db.RequestStatus{
		"req-123": db.StatusPending,
	}
	result := evaluateRequestForPolling("req-123", db.StatusApproved, seen)

	// Reason should mention the status transition
	if !contains(result.Reason, "pending") || !contains(result.Reason, "approved") {
		t.Errorf("expected reason to mention status transition, got: %s", result.Reason)
	}
}

// =============================================================================
// TESTS for statusToEventType
// =============================================================================

// TestStatusToEventType_AllStatuses tests all status to event type mappings.
func TestStatusToEventType_AllStatuses(t *testing.T) {
	tests := []struct {
		status   db.RequestStatus
		expected string
	}{
		{db.StatusApproved, "request_approved"},
		{db.StatusRejected, "request_rejected"},
		{db.StatusExecuted, "request_executed"},
		{db.StatusExecutionFailed, "request_executed"},
		{db.StatusTimeout, "request_timeout"},
		{db.StatusCancelled, "request_cancelled"},
		{db.StatusPending, ""},              // Pending is not a status change event
		{db.RequestStatus("unknown"), ""},   // Unknown status returns empty
	}

	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			result := statusToEventType(tt.status)
			if result != tt.expected {
				t.Errorf("statusToEventType(%s) = %q, want %q", tt.status, result, tt.expected)
			}
		})
	}
}

// TestPollAction_Constants verifies the PollAction constants are defined correctly.
func TestPollAction_Constants(t *testing.T) {
	// Verify the constants have distinct non-empty values
	if PollActionEmitNew == "" {
		t.Error("PollActionEmitNew should not be empty")
	}
	if PollActionEmitStatusChange == "" {
		t.Error("PollActionEmitStatusChange should not be empty")
	}
	if PollActionSkip == "" {
		t.Error("PollActionSkip should not be empty")
	}
	if PollActionEmitNew == PollActionEmitStatusChange {
		t.Error("PollActionEmitNew and PollActionEmitStatusChange should be different")
	}
	if PollActionEmitNew == PollActionSkip {
		t.Error("PollActionEmitNew and PollActionSkip should be different")
	}
}

// TestRequestPollResult_StructFields verifies the struct can be initialized.
func TestRequestPollResult_StructFields(t *testing.T) {
	r := RequestPollResult{
		Action:    PollActionEmitNew,
		EventType: "request_pending",
		Reason:    "test reason",
	}

	if r.Action != PollActionEmitNew {
		t.Error("expected Action=PollActionEmitNew")
	}
	if r.EventType != "request_pending" {
		t.Errorf("expected EventType='request_pending', got %q", r.EventType)
	}
	if r.Reason != "test reason" {
		t.Errorf("expected Reason='test reason', got %q", r.Reason)
	}
}

// =============================================================================
// Table-driven comprehensive test for evaluateRequestForPolling
// =============================================================================

// TestEvaluateRequestForPolling_AllStatusTransitions tests all possible status
// transitions to ensure correct event types are generated.
func TestEvaluateRequestForPolling_AllStatusTransitions(t *testing.T) {
	statuses := []db.RequestStatus{
		db.StatusPending,
		db.StatusApproved,
		db.StatusRejected,
		db.StatusExecuted,
		db.StatusExecutionFailed,
		db.StatusTimeout,
		db.StatusCancelled,
	}

	for _, prevStatus := range statuses {
		for _, newStatus := range statuses {
			if prevStatus == newStatus {
				continue // Skip unchanged
			}
			t.Run(string(prevStatus)+"_to_"+string(newStatus), func(t *testing.T) {
				seen := map[string]db.RequestStatus{
					"req-test": prevStatus,
				}
				result := evaluateRequestForPolling("req-test", newStatus, seen)

				expectedEventType := statusToEventType(newStatus)
				if expectedEventType == "" {
					// Unknown status should skip
					if result.Action != PollActionSkip {
						t.Errorf("expected PollActionSkip for unknown status %s, got %v", newStatus, result.Action)
					}
				} else {
					// Known status should emit event
					if result.Action != PollActionEmitStatusChange {
						t.Errorf("expected PollActionEmitStatusChange for %s->%s, got %v", prevStatus, newStatus, result.Action)
					}
					if result.EventType != expectedEventType {
						t.Errorf("expected EventType=%q, got %q", expectedEventType, result.EventType)
					}
				}

				// Reason should never be empty
				if result.Reason == "" {
					t.Error("reason should not be empty")
				}
			})
		}
	}
}

// contains is a helper function to check if a string contains a substring.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// =============================================================================
// INTEGRATION TESTS for pollRequests
// =============================================================================
//
// These tests verify the side-effectful polling logic works correctly
// with a real database.
// =============================================================================

func TestPollRequests_EmitsNewRequest(t *testing.T) {
	// Create test database
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	dbConn, err := db.OpenAndMigrate(dbPath)
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer dbConn.Close()

	// Create a session first (required for foreign key)
	session := &db.Session{
		ID:          "test-session-123",
		AgentName:   "test-agent",
		Program:     "test",
		Model:       "test",
		ProjectPath: tmpDir,
		StartedAt:   time.Now(),
	}
	if err := dbConn.CreateSession(session); err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	// Create a pending request
	request := &db.Request{
		ID:                 "req-poll-test-1",
		RequestorSessionID: session.ID,
		Status:             db.StatusPending,
		RiskTier:           db.RiskTierCaution,
		MinApprovals:       1,
		RequestorAgent:     "test-agent",
		Command: db.CommandSpec{
			Raw:  "echo test",
			Hash: "abc123",
		},
		ProjectPath: tmpDir,
		CreatedAt:   time.Now(),
	}
	if err := dbConn.CreateRequest(request); err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	// Set up encoder to capture output
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	seen := make(map[string]db.RequestStatus)

	// Call pollRequests
	ctx := context.Background()
	if err := pollRequests(ctx, dbConn, enc, seen); err != nil {
		t.Fatalf("pollRequests failed: %v", err)
	}

	// Verify event was emitted
	output := buf.String()
	if output == "" {
		t.Fatal("expected output from pollRequests, got empty")
	}

	// Parse the output
	var event daemon.RequestStreamEvent
	if err := json.Unmarshal([]byte(output), &event); err != nil {
		t.Fatalf("failed to parse event: %v (output: %s)", err, output)
	}

	if event.Event != "request_pending" {
		t.Errorf("expected event='request_pending', got %q", event.Event)
	}
	if event.RequestID != request.ID {
		t.Errorf("expected request_id=%q, got %q", request.ID, event.RequestID)
	}
	if event.RiskTier != "caution" {
		t.Errorf("expected risk_tier='caution', got %q", event.RiskTier)
	}

	// Verify request was added to seen map
	if _, ok := seen[request.ID]; !ok {
		t.Error("request should be in seen map after polling")
	}
}

func TestPollRequests_SkipsSeenRequest(t *testing.T) {
	// Create test database
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	dbConn, err := db.OpenAndMigrate(dbPath)
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer dbConn.Close()

	// Create a session
	session := &db.Session{
		ID:          "test-session-skip",
		AgentName:   "test-agent",
		Program:     "test",
		Model:       "test",
		ProjectPath: tmpDir,
		StartedAt:   time.Now(),
	}
	if err := dbConn.CreateSession(session); err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	// Create a pending request
	request := &db.Request{
		ID:                 "req-poll-skip-1",
		RequestorSessionID: session.ID,
		Status:             db.StatusPending,
		RiskTier:           db.RiskTierCaution,
		MinApprovals:       1,
		RequestorAgent:     "test-agent",
		Command: db.CommandSpec{
			Raw:  "echo test",
			Hash: "def456",
		},
		ProjectPath: tmpDir,
		CreatedAt:   time.Now(),
	}
	if err := dbConn.CreateRequest(request); err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	// Pre-populate seen map
	seen := map[string]db.RequestStatus{
		request.ID: db.StatusPending,
	}

	// Set up encoder to capture output
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)

	// Call pollRequests
	ctx := context.Background()
	if err := pollRequests(ctx, dbConn, enc, seen); err != nil {
		t.Fatalf("pollRequests failed: %v", err)
	}

	// Verify NO event was emitted (request already seen with same status)
	output := buf.String()
	if output != "" {
		t.Errorf("expected no output for already-seen request, got: %s", output)
	}
}

func TestPollRequests_MultipleRequests(t *testing.T) {
	// Create test database
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	dbConn, err := db.OpenAndMigrate(dbPath)
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer dbConn.Close()

	// Create a session
	session := &db.Session{
		ID:          "test-session-multi",
		AgentName:   "test-agent",
		Program:     "test",
		Model:       "test",
		ProjectPath: tmpDir,
		StartedAt:   time.Now(),
	}
	if err := dbConn.CreateSession(session); err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	// Create two pending requests
	request1 := &db.Request{
		ID:                 "req-multi-1",
		RequestorSessionID: session.ID,
		Status:             db.StatusPending,
		RiskTier:           db.RiskTierCaution,
		MinApprovals:       1,
		RequestorAgent:     "test-agent",
		Command: db.CommandSpec{
			Raw:  "echo first",
			Hash: "hash1",
		},
		ProjectPath: tmpDir,
		CreatedAt:   time.Now(),
	}
	request2 := &db.Request{
		ID:                 "req-multi-2",
		RequestorSessionID: session.ID,
		Status:             db.StatusPending,
		RiskTier:           db.RiskTierDangerous,
		MinApprovals:       2,
		RequestorAgent:     "test-agent",
		Command: db.CommandSpec{
			Raw:  "rm -rf /tmp",
			Hash: "hash2",
		},
		ProjectPath: tmpDir,
		CreatedAt:   time.Now(),
	}
	if err := dbConn.CreateRequest(request1); err != nil {
		t.Fatalf("failed to create request1: %v", err)
	}
	if err := dbConn.CreateRequest(request2); err != nil {
		t.Fatalf("failed to create request2: %v", err)
	}

	// Pre-populate seen map with request1 only
	seen := map[string]db.RequestStatus{
		request1.ID: db.StatusPending,
	}

	// Set up encoder to capture output
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)

	// Call pollRequests
	ctx := context.Background()
	if err := pollRequests(ctx, dbConn, enc, seen); err != nil {
		t.Fatalf("pollRequests failed: %v", err)
	}

	// Should only emit event for request2 (request1 is already seen)
	output := buf.String()
	if output == "" {
		t.Fatal("expected output for new request, got empty")
	}

	var event daemon.RequestStreamEvent
	if err := json.Unmarshal([]byte(output), &event); err != nil {
		t.Fatalf("failed to parse event: %v", err)
	}

	// Should be request2 (the new one)
	if event.RequestID != request2.ID {
		t.Errorf("expected request_id=%q (the new one), got %q", request2.ID, event.RequestID)
	}
	if event.RiskTier != "dangerous" {
		t.Errorf("expected risk_tier='dangerous', got %q", event.RiskTier)
	}

	// Both should be in seen map now
	if _, ok := seen[request1.ID]; !ok {
		t.Error("request1 should still be in seen map")
	}
	if _, ok := seen[request2.ID]; !ok {
		t.Error("request2 should be added to seen map")
	}
}

func TestPollRequests_DatabaseError(t *testing.T) {
	// Create test database then close it to cause error
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	dbConn, err := db.OpenAndMigrate(dbPath)
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	dbConn.Close() // Close to cause error on query

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	seen := make(map[string]db.RequestStatus)

	ctx := context.Background()
	err = pollRequests(ctx, dbConn, enc, seen)
	if err == nil {
		t.Error("expected error when database is closed")
	}
}

func TestPollRequests_UsesDisplayRedacted(t *testing.T) {
	// Create test database
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	dbConn, err := db.OpenAndMigrate(dbPath)
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer dbConn.Close()

	session := &db.Session{
		ID:          "test-session-redact",
		AgentName:   "test-agent",
		Program:     "test",
		Model:       "test",
		ProjectPath: tmpDir,
		StartedAt:   time.Now(),
	}
	if err := dbConn.CreateSession(session); err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	// Create request with redacted display
	request := &db.Request{
		ID:                 "req-redacted-1",
		RequestorSessionID: session.ID,
		Status:             db.StatusPending,
		RiskTier:           db.RiskTierCaution,
		MinApprovals:       1,
		RequestorAgent:     "test-agent",
		Command: db.CommandSpec{
			Raw:             "curl -H 'Authorization: Bearer secret123' https://api.example.com",
			DisplayRedacted: "curl -H 'Authorization: [REDACTED]' https://api.example.com",
			Hash:            "redacted123",
		},
		ProjectPath: tmpDir,
		CreatedAt:   time.Now(),
	}
	if err := dbConn.CreateRequest(request); err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	seen := make(map[string]db.RequestStatus)

	ctx := context.Background()
	if err := pollRequests(ctx, dbConn, enc, seen); err != nil {
		t.Fatalf("pollRequests failed: %v", err)
	}

	var event daemon.RequestStreamEvent
	if err := json.Unmarshal([]byte(buf.String()), &event); err != nil {
		t.Fatalf("failed to parse event: %v", err)
	}

	// Should use redacted display command, not raw
	if event.Command != request.Command.DisplayRedacted {
		t.Errorf("expected command to be redacted %q, got %q", request.Command.DisplayRedacted, event.Command)
	}
}

// =============================================================================
// pollRequests Additional Tests
// =============================================================================

// NOTE: The status change code path in pollRequests (lines 280-288) is
// unreachable with the current implementation because:
// 1. ListPendingRequestsAllProjects() only returns pending requests
// 2. statusToEventType(db.StatusPending) returns ""
// 3. Therefore, status transitions cannot be detected via polling
//
// This is a documented limitation. Status changes are detected via daemon IPC
// (runWatchDaemon) which uses real-time event streaming instead.

func TestPollRequests_EmptyDisplayRedactedFallback(t *testing.T) {
	// Create test database
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	dbConn, err := db.OpenAndMigrate(dbPath)
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer dbConn.Close()

	session := &db.Session{
		ID:          "test-session-raw",
		AgentName:   "test-agent",
		Program:     "test",
		Model:       "test",
		ProjectPath: tmpDir,
		StartedAt:   time.Now(),
	}
	if err := dbConn.CreateSession(session); err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	// Create request with empty DisplayRedacted - should fall back to Raw
	request := &db.Request{
		ID:                 "req-raw-fallback-1",
		RequestorSessionID: session.ID,
		Status:             db.StatusPending,
		RiskTier:           db.RiskTierCaution,
		MinApprovals:       1,
		RequestorAgent:     "test-agent",
		Command: db.CommandSpec{
			Raw:             "echo raw command",
			DisplayRedacted: "", // Empty - should fall back to Raw
			Hash:            "raw123",
		},
		ProjectPath: tmpDir,
		CreatedAt:   time.Now(),
	}
	if err := dbConn.CreateRequest(request); err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	seen := make(map[string]db.RequestStatus)

	ctx := context.Background()
	if err := pollRequests(ctx, dbConn, enc, seen); err != nil {
		t.Fatalf("pollRequests failed: %v", err)
	}

	var event daemon.RequestStreamEvent
	if err := json.Unmarshal([]byte(buf.String()), &event); err != nil {
		t.Fatalf("failed to parse event: %v", err)
	}

	// Should use Raw when DisplayRedacted is empty
	if event.Command != request.Command.Raw {
		t.Errorf("expected command to fall back to Raw %q, got %q", request.Command.Raw, event.Command)
	}
}

func TestPollRequests_ContextCancellation(t *testing.T) {
	// Create test database
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	dbConn, err := db.OpenAndMigrate(dbPath)
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer dbConn.Close()

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	seen := make(map[string]db.RequestStatus)

	// Create already-cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Should return without error even with cancelled context
	// (context is only used for auto-approve, which won't trigger with no requests)
	err = pollRequests(ctx, dbConn, enc, seen)
	if err != nil {
		t.Fatalf("pollRequests should handle empty request list gracefully: %v", err)
	}
}

func TestPollRequests_AutoApproveCaution(t *testing.T) {
	// Create test database
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	dbConn, err := db.OpenAndMigrate(dbPath)
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}

	// Create a session for the requestor
	session := &db.Session{
		ID:          "test-session-poll-auto",
		AgentName:   "test-agent",
		Program:     "test",
		Model:       "test",
		ProjectPath: tmpDir,
		StartedAt:   time.Now(),
	}
	if err := dbConn.CreateSession(session); err != nil {
		dbConn.Close()
		t.Fatalf("failed to create session: %v", err)
	}

	// Create the auto-approve session (needed for foreign key constraint)
	autoSession := &db.Session{
		ID:          "auto-approve",
		AgentName:   "auto-reviewer",
		Program:     "slb-watch",
		Model:       "auto",
		ProjectPath: tmpDir,
		StartedAt:   time.Now(),
	}
	if err := dbConn.CreateSession(autoSession); err != nil {
		dbConn.Close()
		t.Fatalf("failed to create auto-approve session: %v", err)
	}

	// Create a CAUTION tier request
	request := &db.Request{
		ID:                 "req-poll-auto-approve",
		RequestorSessionID: session.ID,
		Status:             db.StatusPending,
		RiskTier:           db.RiskTierCaution,
		MinApprovals:       1,
		RequestorAgent:     "test-agent",
		Command: db.CommandSpec{
			Raw:  "echo caution",
			Hash: "caution123",
		},
		ProjectPath: tmpDir,
		CreatedAt:   time.Now(),
	}
	if err := dbConn.CreateRequest(request); err != nil {
		dbConn.Close()
		t.Fatalf("failed to create request: %v", err)
	}
	dbConn.Close()

	// Save and restore global flags
	origDB := flagDB
	origSession := flagWatchSessionID
	origAutoApprove := flagWatchAutoApproveCaution
	defer func() {
		flagDB = origDB
		flagWatchSessionID = origSession
		flagWatchAutoApproveCaution = origAutoApprove
	}()
	flagDB = dbPath
	flagWatchSessionID = "" // Use default auto-approve
	flagWatchAutoApproveCaution = true

	// Reopen database for polling
	dbConn, err = db.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to reopen database: %v", err)
	}
	defer dbConn.Close()

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	seen := make(map[string]db.RequestStatus)

	err = pollRequests(context.Background(), dbConn, enc, seen)
	if err != nil {
		t.Fatalf("pollRequests failed: %v", err)
	}

	// Check output includes the pending event
	output := buf.String()
	if !strings.Contains(output, "pending") {
		t.Errorf("expected pending event in output, got: %s", output)
	}
	if !strings.Contains(output, "req-poll-auto-approve") {
		t.Errorf("expected request ID in output, got: %s", output)
	}

	// Verify auto-approval happened
	updatedReq, err := dbConn.GetRequest(request.ID)
	if err != nil {
		t.Fatalf("failed to get request: %v", err)
	}
	if updatedReq.Status != db.StatusApproved {
		t.Errorf("expected request to be approved, got %s", updatedReq.Status)
	}
}

func TestPollRequests_AutoApproveCautionError(t *testing.T) {
	// Create test database
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	dbConn, err := db.OpenAndMigrate(dbPath)
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}

	// Create a session for the requestor
	session := &db.Session{
		ID:          "test-session-poll-error",
		AgentName:   "test-agent",
		Program:     "test",
		Model:       "test",
		ProjectPath: tmpDir,
		StartedAt:   time.Now(),
	}
	if err := dbConn.CreateSession(session); err != nil {
		dbConn.Close()
		t.Fatalf("failed to create session: %v", err)
	}

	// NOTE: Intentionally NOT creating auto-approve session to trigger FK error

	// Create a CAUTION tier request
	request := &db.Request{
		ID:                 "req-poll-auto-error",
		RequestorSessionID: session.ID,
		Status:             db.StatusPending,
		RiskTier:           db.RiskTierCaution,
		MinApprovals:       1,
		RequestorAgent:     "test-agent",
		Command: db.CommandSpec{
			Raw:  "echo caution error",
			Hash: "caution-error123",
		},
		ProjectPath: tmpDir,
		CreatedAt:   time.Now(),
	}
	if err := dbConn.CreateRequest(request); err != nil {
		dbConn.Close()
		t.Fatalf("failed to create request: %v", err)
	}
	dbConn.Close()

	// Save and restore global flags
	origDB := flagDB
	origSession := flagWatchSessionID
	origAutoApprove := flagWatchAutoApproveCaution
	defer func() {
		flagDB = origDB
		flagWatchSessionID = origSession
		flagWatchAutoApproveCaution = origAutoApprove
	}()
	flagDB = dbPath
	flagWatchSessionID = "" // Use default auto-approve
	flagWatchAutoApproveCaution = true

	// Reopen database for polling
	dbConn, err = db.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to reopen database: %v", err)
	}
	defer dbConn.Close()

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	seen := make(map[string]db.RequestStatus)

	// Should not return error even if auto-approve fails (error is emitted as event)
	err = pollRequests(context.Background(), dbConn, enc, seen)
	if err != nil {
		t.Fatalf("pollRequests should not fail on auto-approve error: %v", err)
	}

	// Check output includes both the pending event and the auto_approve_error event
	output := buf.String()
	if !strings.Contains(output, "pending") {
		t.Errorf("expected pending event in output, got: %s", output)
	}
	if !strings.Contains(output, "auto_approve_error") {
		t.Errorf("expected auto_approve_error event in output, got: %s", output)
	}
}

// =============================================================================
// autoApproveCaution Integration Tests
// =============================================================================

func TestAutoApproveCaution_InvalidDatabase(t *testing.T) {
	// Save and restore global flags
	origDB := flagDB
	defer func() { flagDB = origDB }()

	// Point to a non-existent database path
	flagDB = "/nonexistent/path/to/database.db"

	ctx := context.Background()
	err := autoApproveCaution(ctx, "req-123")
	if err == nil {
		t.Error("expected error for invalid database path")
	}
	if !contains(err.Error(), "database") && !contains(err.Error(), "opening") {
		t.Errorf("expected error about database, got: %v", err)
	}
}

func TestAutoApproveCaution_RequestNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	dbConn, err := db.OpenAndMigrate(dbPath)
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	dbConn.Close() // We just need the file to exist

	// Save and restore global flags
	origDB := flagDB
	defer func() { flagDB = origDB }()

	flagDB = dbPath

	ctx := context.Background()
	err = autoApproveCaution(ctx, "nonexistent-request")
	if err == nil {
		t.Error("expected error for nonexistent request")
	}
	if !contains(err.Error(), "request") && !contains(err.Error(), "getting") {
		t.Errorf("expected error about getting request, got: %v", err)
	}
}

func TestAutoApproveCaution_AlreadyResolved(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	dbConn, err := db.OpenAndMigrate(dbPath)
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer dbConn.Close()

	// Create a session
	session := &db.Session{
		ID:          "test-session-resolved",
		AgentName:   "test-agent",
		Program:     "test",
		Model:       "test",
		ProjectPath: tmpDir,
		StartedAt:   time.Now(),
	}
	if err := dbConn.CreateSession(session); err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	// Create an already-approved request
	request := &db.Request{
		ID:                 "req-already-approved",
		RequestorSessionID: session.ID,
		Status:             db.StatusApproved, // Already resolved
		RiskTier:           db.RiskTierCaution,
		MinApprovals:       1,
		RequestorAgent:     "test-agent",
		Command: db.CommandSpec{
			Raw:  "echo approved",
			Hash: "approved123",
		},
		ProjectPath: tmpDir,
		CreatedAt:   time.Now(),
	}
	if err := dbConn.CreateRequest(request); err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	// Save and restore global flags
	origDB := flagDB
	defer func() { flagDB = origDB }()
	flagDB = dbPath

	ctx := context.Background()
	err = autoApproveCaution(ctx, request.ID)
	// Should return nil (not an error) for already-resolved requests
	if err != nil {
		t.Errorf("expected no error for already-resolved request, got: %v", err)
	}
}

func TestAutoApproveCaution_WrongTier(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	dbConn, err := db.OpenAndMigrate(dbPath)
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer dbConn.Close()

	// Create a session
	session := &db.Session{
		ID:          "test-session-wrong-tier",
		AgentName:   "test-agent",
		Program:     "test",
		Model:       "test",
		ProjectPath: tmpDir,
		StartedAt:   time.Now(),
	}
	if err := dbConn.CreateSession(session); err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	// Create a dangerous tier request (should NOT be auto-approved)
	request := &db.Request{
		ID:                 "req-dangerous-tier",
		RequestorSessionID: session.ID,
		Status:             db.StatusPending,
		RiskTier:           db.RiskTierDangerous, // Wrong tier
		MinApprovals:       1,
		RequestorAgent:     "test-agent",
		Command: db.CommandSpec{
			Raw:  "rm -rf /",
			Hash: "dangerous123",
		},
		ProjectPath: tmpDir,
		CreatedAt:   time.Now(),
	}
	if err := dbConn.CreateRequest(request); err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	// Save and restore global flags
	origDB := flagDB
	defer func() { flagDB = origDB }()
	flagDB = dbPath

	ctx := context.Background()
	err = autoApproveCaution(ctx, request.ID)
	if err == nil {
		t.Error("expected error when trying to auto-approve dangerous tier")
	}
	if !contains(err.Error(), "denied") {
		t.Errorf("expected error about denial, got: %v", err)
	}
}

func TestAutoApproveCaution_SuccessfulApproval(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	dbConn, err := db.OpenAndMigrate(dbPath)
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}

	// Create a session for the requestor
	session := &db.Session{
		ID:          "test-session-success",
		AgentName:   "test-agent",
		Program:     "test",
		Model:       "test",
		ProjectPath: tmpDir,
		StartedAt:   time.Now(),
	}
	if err := dbConn.CreateSession(session); err != nil {
		dbConn.Close()
		t.Fatalf("failed to create session: %v", err)
	}

	// Create the auto-approve session (needed for foreign key constraint)
	autoApproveSession := &db.Session{
		ID:          "auto-approve",
		AgentName:   "auto-reviewer",
		Program:     "slb-watch",
		Model:       "auto",
		ProjectPath: tmpDir,
		StartedAt:   time.Now(),
	}
	if err := dbConn.CreateSession(autoApproveSession); err != nil {
		dbConn.Close()
		t.Fatalf("failed to create auto-approve session: %v", err)
	}

	// Create a CAUTION tier pending request
	request := &db.Request{
		ID:                 "req-auto-success",
		RequestorSessionID: session.ID,
		Status:             db.StatusPending,
		RiskTier:           db.RiskTierCaution,
		MinApprovals:       1, // Only 1 approval needed
		RequestorAgent:     "test-agent",
		Command: db.CommandSpec{
			Raw:  "echo hello",
			Hash: "caution123",
		},
		ProjectPath: tmpDir,
		CreatedAt:   time.Now(),
	}
	if err := dbConn.CreateRequest(request); err != nil {
		dbConn.Close()
		t.Fatalf("failed to create request: %v", err)
	}

	// Close the setup connection before calling autoApproveCaution
	// which opens its own connection
	dbConn.Close()

	// Save and restore global flags
	origDB := flagDB
	origSession := flagWatchSessionID
	defer func() {
		flagDB = origDB
		flagWatchSessionID = origSession
	}()
	flagDB = dbPath
	flagWatchSessionID = "" // Test the default session fallback

	ctx := context.Background()
	err = autoApproveCaution(ctx, request.ID)
	if err != nil {
		t.Fatalf("expected successful auto-approval, got error: %v", err)
	}

	// Reopen to verify
	dbConn, err = db.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to reopen database: %v", err)
	}
	defer dbConn.Close()

	// Verify the request was approved
	updatedReq, err := dbConn.GetRequest(request.ID)
	if err != nil {
		t.Fatalf("failed to get updated request: %v", err)
	}
	if updatedReq.Status != db.StatusApproved {
		t.Errorf("expected request status to be approved, got %s", updatedReq.Status)
	}

	// Verify a review was created
	reviews, err := dbConn.ListReviewsForRequest(request.ID)
	if err != nil {
		t.Fatalf("failed to list reviews: %v", err)
	}
	if len(reviews) != 1 {
		t.Errorf("expected 1 review, got %d", len(reviews))
	}
	if reviews[0].Decision != db.DecisionApprove {
		t.Errorf("expected approve decision, got %s", reviews[0].Decision)
	}
	if reviews[0].ReviewerSessionID != "auto-approve" {
		t.Errorf("expected session 'auto-approve', got %s", reviews[0].ReviewerSessionID)
	}
}

func TestAutoApproveCaution_WithCustomSession(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	dbConn, err := db.OpenAndMigrate(dbPath)
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer dbConn.Close()

	session := &db.Session{
		ID:          "test-session-custom",
		AgentName:   "test-agent",
		Program:     "test",
		Model:       "test",
		ProjectPath: tmpDir,
		StartedAt:   time.Now(),
	}
	if err := dbConn.CreateSession(session); err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	// Create the reviewer session (needed for foreign key constraint)
	reviewerSession := &db.Session{
		ID:          "custom-watch-session",
		AgentName:   "auto-reviewer",
		Program:     "slb-watch",
		Model:       "auto",
		ProjectPath: tmpDir,
		StartedAt:   time.Now(),
	}
	if err := dbConn.CreateSession(reviewerSession); err != nil {
		t.Fatalf("failed to create reviewer session: %v", err)
	}

	request := &db.Request{
		ID:                 "req-custom-session",
		RequestorSessionID: session.ID,
		Status:             db.StatusPending,
		RiskTier:           db.RiskTierCaution,
		MinApprovals:       1,
		RequestorAgent:     "test-agent",
		Command: db.CommandSpec{
			Raw:  "echo custom",
			Hash: "custom123",
		},
		ProjectPath: tmpDir,
		CreatedAt:   time.Now(),
	}
	if err := dbConn.CreateRequest(request); err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	origDB := flagDB
	origSession := flagWatchSessionID
	defer func() {
		flagDB = origDB
		flagWatchSessionID = origSession
	}()
	flagDB = dbPath
	flagWatchSessionID = "custom-watch-session" // Custom session ID

	ctx := context.Background()
	err = autoApproveCaution(ctx, request.ID)
	if err != nil {
		t.Fatalf("expected successful auto-approval, got error: %v", err)
	}

	// Verify the custom session was used
	reviews, err := dbConn.ListReviewsForRequest(request.ID)
	if err != nil {
		t.Fatalf("failed to list reviews: %v", err)
	}
	if reviews[0].ReviewerSessionID != "custom-watch-session" {
		t.Errorf("expected custom session, got %s", reviews[0].ReviewerSessionID)
	}
}

func TestAutoApproveCaution_MultipleApprovalsNeeded(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	dbConn, err := db.OpenAndMigrate(dbPath)
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer dbConn.Close()

	session := &db.Session{
		ID:          "test-session-multi",
		AgentName:   "test-agent",
		Program:     "test",
		Model:       "test",
		ProjectPath: tmpDir,
		StartedAt:   time.Now(),
	}
	if err := dbConn.CreateSession(session); err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	// Create the reviewer session (needed for foreign key constraint)
	reviewerSession := &db.Session{
		ID:          "multi-approval-session",
		AgentName:   "auto-reviewer",
		Program:     "slb-watch",
		Model:       "auto",
		ProjectPath: tmpDir,
		StartedAt:   time.Now(),
	}
	if err := dbConn.CreateSession(reviewerSession); err != nil {
		t.Fatalf("failed to create reviewer session: %v", err)
	}

	// Request needs 2 approvals
	request := &db.Request{
		ID:                 "req-multi-approvals",
		RequestorSessionID: session.ID,
		Status:             db.StatusPending,
		RiskTier:           db.RiskTierCaution,
		MinApprovals:       2, // Needs 2 approvals
		RequestorAgent:     "test-agent",
		Command: db.CommandSpec{
			Raw:  "echo multi",
			Hash: "multi123",
		},
		ProjectPath: tmpDir,
		CreatedAt:   time.Now(),
	}
	if err := dbConn.CreateRequest(request); err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	origDB := flagDB
	origSession := flagWatchSessionID
	defer func() {
		flagDB = origDB
		flagWatchSessionID = origSession
	}()
	flagDB = dbPath
	flagWatchSessionID = "multi-approval-session" // Unique session to avoid conflicts

	ctx := context.Background()
	err = autoApproveCaution(ctx, request.ID)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Request should still be pending (only 1 of 2 approvals)
	updatedReq, err := dbConn.GetRequest(request.ID)
	if err != nil {
		t.Fatalf("failed to get updated request: %v", err)
	}
	if updatedReq.Status != db.StatusPending {
		t.Errorf("expected request to remain pending (needs 2 approvals), got %s", updatedReq.Status)
	}

	// But review should be created
	reviews, err := dbConn.ListReviewsForRequest(request.ID)
	if err != nil {
		t.Fatalf("failed to list reviews: %v", err)
	}
	if len(reviews) != 1 {
		t.Errorf("expected 1 review, got %d", len(reviews))
	}
}
