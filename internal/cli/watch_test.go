package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/daemon"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/testutil"
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

func TestWatchStatePreservesExactOutcomes(t *testing.T) {
	oldAuto := flagWatchAutoApproveCaution
	flagWatchAutoApproveCaution = false
	t.Cleanup(func() { flagWatchAutoApproveCaution = oldAuto })
	for _, tc := range []struct {
		status db.RequestStatus
		event  string
	}{
		{db.StatusPending, "request_pending"},
		{db.StatusApproved, "request_approved"},
		{db.StatusExecuting, "request_executing"},
		{db.StatusExecuted, "request_executed"},
		{db.StatusExecutionFailed, "request_execution_failed"},
		{db.StatusTimedOut, "request_timed_out"},
		{db.StatusTimeout, "request_timeout"},
		{db.StatusEscalated, "request_escalated"},
		{db.StatusRejected, "request_rejected"},
		{db.StatusCancelled, "request_cancelled"},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			code := 7
			r := db.WatchRequestState{
				ID: "request", ProjectPath: "/project", Status: tc.status,
				Command: "safe display", CommandHash: "bound-command", MinApprovals: 2,
				Approvals: 1, ExitCode: &code, CreatedAt: "2026-01-01T00:00:00Z",
				ResolvedAt: time.Now().UTC().Format(time.RFC3339), ApprovalPolicy: db.ApprovalPolicySameHost,
				ExecutionAssurance: db.AssuranceSameHost,
			}
			state := newWatchState(r.ProjectPath)
			var buf bytes.Buffer
			for i := 0; i < 2; i++ {
				if err := processPolledRequest(context.Background(), r, json.NewEncoder(&buf), state); err != nil {
					t.Fatal(err)
				}
			}
			var event daemon.RequestStreamEvent
			if err := json.Unmarshal(buf.Bytes(), &event); err != nil {
				// A duplicate would also make this single-object decode fail.
				t.Fatalf("invalid or duplicated state event: %s %v", buf.String(), err)
			}
			if event.Event != tc.event || event.Status != tc.status || event.CommandHash != r.CommandHash ||
				event.MinApprovals == nil || *event.MinApprovals != 2 || event.Approvals == nil || *event.Approvals != 1 ||
				event.Rejections == nil || *event.Rejections != 0 || event.ExitCode == nil || *event.ExitCode != 7 ||
				event.CreatedAt != r.CreatedAt || event.ResolvedAt != r.ResolvedAt || event.ObservedAt == "" ||
				event.ApprovalPolicy != db.ApprovalPolicySameHost || event.ExecutionAssurance != db.AssuranceSameHost ||
				!strings.Contains(event.ReviewNotice, "not independently authenticated") {
				t.Fatalf("watch lost outcome, command binding, quorum, timestamps or assurance: %+v", event)
			}
		})
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
	seen := newWatchState(tmpDir)

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
	if _, ok := seen.requests[request.ID]; !ok {
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

	seen := newWatchState(tmpDir)

	// Set up encoder to capture output
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)

	// Call pollRequests
	ctx := context.Background()
	if err := pollRequests(ctx, dbConn, enc, seen); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
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
	seen := newWatchState(tmpDir)

	// Set up encoder to capture output
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)

	// Call pollRequests
	ctx := context.Background()
	if err := pollRequests(ctx, dbConn, enc, seen); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if err := dbConn.CreateRequest(request2); err != nil {
		t.Fatalf("failed to create request2: %v", err)
	}
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
	if _, ok := seen.requests[request1.ID]; !ok {
		t.Error("request1 should still be in seen map")
	}
	if _, ok := seen.requests[request2.ID]; !ok {
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
	seen := newWatchState(tmpDir)

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
	seen := newWatchState(tmpDir)

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
	seen := newWatchState(tmpDir)

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
	seen := newWatchState(tmpDir)

	// Create already-cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// The database snapshot honors cancellation, even before any requests exist.
	err = pollRequests(ctx, dbConn, enc, seen)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pollRequests ignored cancellation: %v", err)
	}
}

// Since cf22a99 watch auto-approval is a pending-policy transition, not a
// review: it applies only to due, zero-quorum CAUTION requests from an active
// requester, never manufactures reviewer evidence, and treats an ineligible
// request as "no decision" rather than an error. watchPolicyFixture (in
// watch_policy_test.go) builds such an eligible request, created two minutes
// ago so the default 30s CAUTION delay has elapsed.

// assertAutoApprovedWithoutReviews checks the complete policy decision.
func assertAutoApprovedWithoutReviews(t *testing.T, database *db.DB, id string) {
	t.Helper()
	stored, err := database.GetRequest(id)
	if err != nil {
		t.Fatalf("failed to get request: %v", err)
	}
	if stored.Status != db.StatusApproved {
		t.Fatalf("expected request to be approved, got %s", stored.Status)
	}
	if stored.ApprovalExpiresAt == nil || !stored.ApprovalExpiresAt.After(time.Now()) || stored.ResolvedAt == nil {
		t.Fatalf("auto-approval is not a complete policy decision: %+v", stored)
	}
	reviews, err := database.ListReviewsForRequest(id)
	if err != nil {
		t.Fatalf("failed to list reviews: %v", err)
	}
	if len(reviews) != 0 {
		t.Fatalf("auto-approval manufactured %d reviewer record(s)", len(reviews))
	}
}

// assertLeftPendingWithoutReviews checks that no decision of any kind was made.
func assertLeftPendingWithoutReviews(t *testing.T, database *db.DB, id string) {
	t.Helper()
	stored, err := database.GetRequest(id)
	if err != nil {
		t.Fatalf("failed to get request: %v", err)
	}
	if stored.Status != db.StatusPending || stored.ApprovalExpiresAt != nil || stored.ResolvedAt != nil {
		t.Fatalf("ineligible request was resolved: %+v", stored)
	}
	reviews, err := database.ListReviewsForRequest(id)
	if err != nil {
		t.Fatalf("failed to list reviews: %v", err)
	}
	if len(reviews) != 0 {
		t.Fatalf("auto-approval manufactured %d reviewer record(s)", len(reviews))
	}
}

func TestPollRequests_AutoApproveCaution(t *testing.T) {
	database, request := watchPolicyFixture(t)

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	seen := newWatchState(request.ProjectPath)
	if err := pollRequests(context.Background(), database, enc, seen); err != nil {
		t.Fatalf("pollRequests failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, `"event":"request_pending"`) {
		t.Errorf("expected pending event in output, got: %s", output)
	}
	if !strings.Contains(output, request.ID) {
		t.Errorf("expected request ID in output, got: %s", output)
	}
	if strings.Contains(output, "auto_approve_error") {
		t.Errorf("unexpected auto-approval error: %s", output)
	}
	assertAutoApprovedWithoutReviews(t, database, request.ID)
}

func TestPollRequests_AutoApproveCautionError(t *testing.T) {
	database, request := watchPolicyFixture(t)
	// A project policy that cannot be loaded must never grant permission; the
	// failure is reported as an event and polling continues.
	if err := os.WriteFile(filepath.Join(request.ProjectPath, ".slb", "config.toml"), []byte("[patterns.caution\n"), 0600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	seen := newWatchState(request.ProjectPath)
	if err := pollRequests(context.Background(), database, enc, seen); err != nil {
		t.Fatalf("pollRequests should not fail on auto-approve error: %v", err)
	}

	var sawPending, sawError bool
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("invalid event line %q: %v", line, err)
		}
		switch event["event"] {
		case "request_pending":
			sawPending = sawPending || event["request_id"] == request.ID
		case "auto_approve_error":
			message, _ := event["error"].(string)
			sawError = sawError || (event["request_id"] == request.ID && message != "")
		}
	}
	if !sawPending {
		t.Errorf("expected pending event in output, got: %s", buf.String())
	}
	if !sawError {
		t.Errorf("expected auto_approve_error event in output, got: %s", buf.String())
	}
	assertLeftPendingWithoutReviews(t, database, request.ID)
}

// =============================================================================
// autoApproveCaution Integration Tests
// =============================================================================

func TestAutoApproveCaution_InvalidDatabase(t *testing.T) {
	// Save and restore global flags
	origDB := flagDB
	defer func() { flagDB = origDB }()

	// A missing directory may be created by db.Open, especially as root.
	// A regular file cannot contain a database, regardless of host permissions.
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parent, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	flagDB = filepath.Join(parent, "database.db")

	ctx := context.Background()
	err := autoApproveCaution(ctx, watchDatabasePath(t), "req-123")
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
	err = autoApproveCaution(ctx, watchDatabasePath(t), "nonexistent-request")
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
	err = autoApproveCaution(ctx, watchDatabasePath(t), request.ID)
	// Should return nil (not an error) for already-resolved requests
	if err != nil {
		t.Errorf("expected no error for already-resolved request, got: %v", err)
	}
}

func TestAutoApproveCaution_WrongTier(t *testing.T) {
	database, request := watchPolicyFixture(t)
	// A DANGEROUS request (even one otherwise eligible) must never be
	// auto-approved. Ineligibility is "no decision", not an error.
	watchPolicySQL(t, database, `UPDATE requests SET risk_tier = 'dangerous', min_approvals = 1 WHERE id = ?`, request.ID)

	if err := autoApproveCaution(context.Background(), watchDatabasePath(t), request.ID); err != nil {
		t.Fatalf("ineligible request should be left alone without error, got: %v", err)
	}
	assertLeftPendingWithoutReviews(t, database, request.ID)
}

func TestAutoApproveCaution_SuccessfulApproval(t *testing.T) {
	database, request := watchPolicyFixture(t)
	flagWatchSessionID = "" // No reviewer identity is needed for a policy decision.

	if err := autoApproveCaution(context.Background(), watchDatabasePath(t), request.ID); err != nil {
		t.Fatalf("expected successful auto-approval, got error: %v", err)
	}
	assertAutoApprovedWithoutReviews(t, database, request.ID)
}

func TestAutoApproveCaution_WithCustomSession(t *testing.T) {
	database, request := watchPolicyFixture(t)
	// A watcher's own session is not a reviewer: automatic decisions are not
	// attributed to it and create no review under its identity.
	watcher := testutil.MakeSession(t, database,
		testutil.WithProject(request.ProjectPath),
		testutil.WithAgent("custom-watcher"),
		testutil.WithModel("auto"),
	)
	flagWatchSessionID = watcher.ID

	if err := autoApproveCaution(context.Background(), watchDatabasePath(t), request.ID); err != nil {
		t.Fatalf("expected successful auto-approval, got error: %v", err)
	}
	assertAutoApprovedWithoutReviews(t, database, request.ID)
}

func TestAutoApproveCaution_MultipleApprovalsNeeded(t *testing.T) {
	database, request := watchPolicyFixture(t)
	// A stored quorum is part of the request's review contract; watch cannot
	// satisfy it, partially or fully.
	watchPolicySQL(t, database, `UPDATE requests SET min_approvals = 2 WHERE id = ?`, request.ID)

	if err := autoApproveCaution(context.Background(), watchDatabasePath(t), request.ID); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	assertLeftPendingWithoutReviews(t, database, request.ID)
}
