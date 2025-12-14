// Package db tests for request CRUD operations.
package db

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestCreateRequest(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create a session first
	sess := &Session{
		AgentName:   "GreenLake",
		Program:     "claude-code",
		Model:       "opus-4.5",
		ProjectPath: "/test/project",
	}
	if err := db.CreateSession(sess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	r := &Request{
		ProjectPath:        "/test/project",
		RequestorSessionID: sess.ID,
		RequestorAgent:     "GreenLake",
		RequestorModel:     "opus-4.5",
		RiskTier:           RiskTierCritical,
		MinApprovals:       2,
		Command: CommandSpec{
			Raw:  "rm -rf /tmp/test",
			Cwd:  "/tmp",
			Argv: []string{"rm", "-rf", "/tmp/test"},
		},
		Justification: Justification{
			Reason:         "Need to clean up test files",
			ExpectedEffect: "Removes test directory",
		},
	}

	err := db.CreateRequest(r)
	if err != nil {
		t.Fatalf("CreateRequest failed: %v", err)
	}

	// Verify UUID was generated
	if r.ID == "" {
		t.Error("Expected UUID to be generated")
	}

	// Verify hash was computed
	if r.Command.Hash == "" {
		t.Error("Expected command hash to be computed")
	}

	// Verify timestamps
	if r.CreatedAt.IsZero() {
		t.Error("Expected CreatedAt to be set")
	}
	if r.ExpiresAt == nil {
		t.Error("Expected ExpiresAt to be set")
	}
}

func TestGetRequest(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	sess, r := createTestRequest(t, db)

	retrieved, err := db.GetRequest(r.ID)
	if err != nil {
		t.Fatalf("GetRequest failed: %v", err)
	}

	if retrieved.ID != r.ID {
		t.Errorf("ID mismatch: got %s, want %s", retrieved.ID, r.ID)
	}
	if retrieved.Command.Raw != r.Command.Raw {
		t.Errorf("Command.Raw mismatch: got %s, want %s", retrieved.Command.Raw, r.Command.Raw)
	}
	if retrieved.RequestorAgent != r.RequestorAgent {
		t.Errorf("RequestorAgent mismatch: got %s, want %s", retrieved.RequestorAgent, r.RequestorAgent)
	}
	if retrieved.RequestorSessionID != sess.ID {
		t.Errorf("RequestorSessionID mismatch: got %s, want %s", retrieved.RequestorSessionID, sess.ID)
	}
}

func TestGetRequestNotFound(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	_, err := db.GetRequest("nonexistent-id")
	if err != ErrRequestNotFound {
		t.Errorf("Expected ErrRequestNotFound, got: %v", err)
	}
}

func TestListPendingRequests(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create multiple requests
	for i := 0; i < 3; i++ {
		createTestRequest(t, db)
	}

	requests, err := db.ListPendingRequests("/test/project")
	if err != nil {
		t.Fatalf("ListPendingRequests failed: %v", err)
	}

	if len(requests) != 3 {
		t.Errorf("Expected 3 pending requests, got %d", len(requests))
	}

	for _, r := range requests {
		if r.Status != StatusPending {
			t.Errorf("Expected status pending, got %s", r.Status)
		}
	}
}

func TestUpdateRequestStatus(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	_, r := createTestRequest(t, db)

	// Valid transition: pending -> approved
	err := db.UpdateRequestStatus(r.ID, StatusApproved)
	if err != nil {
		t.Fatalf("UpdateRequestStatus failed: %v", err)
	}

	retrieved, err := db.GetRequest(r.ID)
	if err != nil {
		t.Fatalf("GetRequest failed: %v", err)
	}
	if retrieved.Status != StatusApproved {
		t.Errorf("Expected status approved, got %s", retrieved.Status)
	}

	// Valid transition: approved -> executing
	err = db.UpdateRequestStatus(r.ID, StatusExecuting)
	if err != nil {
		t.Fatalf("UpdateRequestStatus to executing failed: %v", err)
	}

	// Valid transition: executing -> executed (terminal)
	err = db.UpdateRequestStatus(r.ID, StatusExecuted)
	if err != nil {
		t.Fatalf("UpdateRequestStatus to executed failed: %v", err)
	}

	retrieved, err = db.GetRequest(r.ID)
	if err != nil {
		t.Fatalf("GetRequest failed: %v", err)
	}
	if retrieved.ResolvedAt == nil {
		t.Error("Expected ResolvedAt to be set for terminal state")
	}
}

func TestUpdateRequestStatusInvalid(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	_, r := createTestRequest(t, db)

	// Invalid transition: pending -> executed (skip approved)
	err := db.UpdateRequestStatus(r.ID, StatusExecuted)
	if err == nil {
		t.Error("Expected error for invalid transition")
	}
}

func TestCountPendingBySession(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	sess, _ := createTestRequest(t, db)
	createTestRequest(t, db) // Creates another with a new session

	count, err := db.CountPendingBySession(sess.ID)
	if err != nil {
		t.Fatalf("CountPendingBySession failed: %v", err)
	}

	// First session should have 1 pending request
	if count != 1 {
		t.Errorf("Expected 1 pending request for session, got %d", count)
	}
}

func TestFindExpiredRequests(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	_, r := createTestRequest(t, db)

	// Manually set expiry to past
	pastTime := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	_, err := db.Exec(`UPDATE requests SET expires_at = ? WHERE id = ?`, pastTime, r.ID)
	if err != nil {
		t.Fatalf("Failed to set old expires_at: %v", err)
	}

	expired, err := db.FindExpiredRequests()
	if err != nil {
		t.Fatalf("FindExpiredRequests failed: %v", err)
	}

	if len(expired) != 1 {
		t.Errorf("Expected 1 expired request, got %d", len(expired))
	}
}

func TestComputeCommandHash(t *testing.T) {
	cmd := CommandSpec{
		Raw:   "rm -rf /tmp/test",
		Cwd:   "/tmp",
		Argv:  []string{"rm", "-rf", "/tmp/test"},
		Shell: false,
	}

	hash1 := ComputeCommandHash(cmd)
	if hash1 == "" {
		t.Error("Expected non-empty hash")
	}

	// Same command should produce same hash
	hash2 := ComputeCommandHash(cmd)
	if hash1 != hash2 {
		t.Error("Expected same hash for same command")
	}

	// Different command should produce different hash
	cmd.Raw = "rm -rf /tmp/other"
	hash3 := ComputeCommandHash(cmd)
	if hash1 == hash3 {
		t.Error("Expected different hash for different command")
	}
}

func TestSearchRequests(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	_, r := createTestRequest(t, db)

	// Search by command
	results, err := db.SearchRequests("rm")
	if err != nil {
		t.Fatalf("SearchRequests failed: %v", err)
	}

	if len(results) != 1 {
		t.Errorf("Expected 1 result, got %d", len(results))
	}

	// Search by justification
	results, err = db.SearchRequests("clean")
	if err != nil {
		t.Fatalf("SearchRequests by justification failed: %v", err)
	}

	if len(results) != 1 {
		t.Errorf("Expected 1 result for justification search, got %d", len(results))
	}
	if len(results) > 0 && results[0].ID != r.ID {
		t.Errorf("Expected result ID %s, got %s", r.ID, results[0].ID)
	}

	// Search with no matches
	results, err = db.SearchRequests("nonexistent")
	if err != nil {
		t.Fatalf("SearchRequests for nonexistent failed: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("Expected 0 results for nonexistent search, got %d", len(results))
	}
}

func TestGetRequestWithReviews(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	_, r := createTestRequest(t, db)

	// Get request with no reviews yet
	retrieved, reviews, err := db.GetRequestWithReviews(r.ID)
	if err != nil {
		t.Fatalf("GetRequestWithReviews failed: %v", err)
	}

	if retrieved.ID != r.ID {
		t.Errorf("Request ID mismatch: got %s, want %s", retrieved.ID, r.ID)
	}
	if len(reviews) != 0 {
		t.Errorf("Expected 0 reviews, got %d", len(reviews))
	}

	// Test with nonexistent request
	_, _, err = db.GetRequestWithReviews("nonexistent-id")
	if err != ErrRequestNotFound {
		t.Errorf("Expected ErrRequestNotFound, got: %v", err)
	}
}

func TestListRequestsByStatus(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create requests
	for i := 0; i < 3; i++ {
		createTestRequest(t, db)
	}

	// Get one and approve it
	requests, _ := db.ListPendingRequests("/test/project")
	if len(requests) > 0 {
		db.UpdateRequestStatus(requests[0].ID, StatusApproved)
	}

	// List by different statuses
	pending, err := db.ListRequestsByStatus(StatusPending, "/test/project")
	if err != nil {
		t.Fatalf("ListRequestsByStatus(pending) failed: %v", err)
	}
	if len(pending) != 2 {
		t.Errorf("Expected 2 pending, got %d", len(pending))
	}

	approved, err := db.ListRequestsByStatus(StatusApproved, "/test/project")
	if err != nil {
		t.Fatalf("ListRequestsByStatus(approved) failed: %v", err)
	}
	if len(approved) != 1 {
		t.Errorf("Expected 1 approved, got %d", len(approved))
	}
}

func TestListAllRequests(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	project1 := "/test/project"
	project2 := "/test/other"

	sess1 := &Session{AgentName: "ListAll1", Program: "codex-cli", Model: "gpt-5", ProjectPath: project1}
	if err := db.CreateSession(sess1); err != nil {
		t.Fatalf("CreateSession sess1 failed: %v", err)
	}
	sess2 := &Session{AgentName: "ListAll2", Program: "codex-cli", Model: "gpt-5", ProjectPath: project2}
	if err := db.CreateSession(sess2); err != nil {
		t.Fatalf("CreateSession sess2 failed: %v", err)
	}

	makeReq := func(sess *Session, raw string) *Request {
		r := &Request{
			ProjectPath:        sess.ProjectPath,
			RequestorSessionID: sess.ID,
			RequestorAgent:     sess.AgentName,
			RequestorModel:     sess.Model,
			RiskTier:           RiskTierDangerous,
			MinApprovals:       1,
			Command:            CommandSpec{Raw: raw, Cwd: sess.ProjectPath},
			Justification:      Justification{Reason: "list all"},
		}
		if err := db.CreateRequest(r); err != nil {
			t.Fatalf("CreateRequest failed: %v", err)
		}
		return r
	}

	r1 := makeReq(sess1, "rm -rf ./build")
	r2 := makeReq(sess1, "rm -rf ./dist")
	_ = makeReq(sess2, "rm -rf ./other") // different project, should be excluded

	base := time.Now().UTC().Truncate(time.Second)
	if _, err := db.Exec(`UPDATE requests SET created_at = ? WHERE id = ?`, base.Format(time.RFC3339), r1.ID); err != nil {
		t.Fatalf("update created_at r1 failed: %v", err)
	}
	if _, err := db.Exec(`UPDATE requests SET created_at = ? WHERE id = ?`, base.Add(1*time.Minute).Format(time.RFC3339), r2.ID); err != nil {
		t.Fatalf("update created_at r2 failed: %v", err)
	}

	all, err := db.ListAllRequests(project1)
	if err != nil {
		t.Fatalf("ListAllRequests failed: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(all))
	}
	if all[0].ID != r2.ID || all[1].ID != r1.ID {
		t.Fatalf("unexpected order: got [%s,%s] want [%s,%s]", all[0].ID, all[1].ID, r2.ID, r1.ID)
	}
}

func TestListPendingRequestsAllProjects(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create requests in multiple projects
	sess1 := &Session{
		AgentName:   "Agent1",
		Program:     "claude-code",
		Model:       "opus-4.5",
		ProjectPath: "/test/project1",
	}
	db.CreateSession(sess1)

	sess2 := &Session{
		AgentName:   "Agent2",
		Program:     "claude-code",
		Model:       "opus-4.5",
		ProjectPath: "/test/project2",
	}
	db.CreateSession(sess2)

	r1 := &Request{
		ProjectPath:        "/test/project1",
		RequestorSessionID: sess1.ID,
		RequestorAgent:     sess1.AgentName,
		RequestorModel:     "opus-4.5",
		RiskTier:           RiskTierDangerous,
		MinApprovals:       1,
		Command: CommandSpec{
			Raw: "rm -rf /tmp/test1",
			Cwd: "/tmp",
		},
		Justification: Justification{Reason: "Test 1"},
	}
	db.CreateRequest(r1)

	r2 := &Request{
		ProjectPath:        "/test/project2",
		RequestorSessionID: sess2.ID,
		RequestorAgent:     sess2.AgentName,
		RequestorModel:     "opus-4.5",
		RiskTier:           RiskTierDangerous,
		MinApprovals:       1,
		Command: CommandSpec{
			Raw: "rm -rf /tmp/test2",
			Cwd: "/tmp",
		},
		Justification: Justification{Reason: "Test 2"},
	}
	db.CreateRequest(r2)

	// List all pending
	all, err := db.ListPendingRequestsAllProjects()
	if err != nil {
		t.Fatalf("ListPendingRequestsAllProjects failed: %v", err)
	}

	if len(all) != 2 {
		t.Errorf("Expected 2 pending across all projects, got %d", len(all))
	}
}

func TestListPendingRequestsByProjects(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Empty input should return empty slice (not nil).
	results, err := db.ListPendingRequestsByProjects(nil)
	if err != nil {
		t.Fatalf("ListPendingRequestsByProjects(nil) failed: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results, got %d", len(results))
	}

	// Create requests in multiple projects.
	sess1 := &Session{AgentName: "Agent1", Program: "codex-cli", Model: "gpt-5", ProjectPath: "/test/project1"}
	sess2 := &Session{AgentName: "Agent2", Program: "codex-cli", Model: "gpt-5", ProjectPath: "/test/project2"}
	if err := db.CreateSession(sess1); err != nil {
		t.Fatalf("CreateSession sess1 failed: %v", err)
	}
	if err := db.CreateSession(sess2); err != nil {
		t.Fatalf("CreateSession sess2 failed: %v", err)
	}

	r1 := &Request{
		ProjectPath:        sess1.ProjectPath,
		RequestorSessionID: sess1.ID,
		RequestorAgent:     sess1.AgentName,
		RequestorModel:     sess1.Model,
		RiskTier:           RiskTierDangerous,
		MinApprovals:       1,
		Command:            CommandSpec{Raw: "rm -rf /tmp/test1", Cwd: "/tmp"},
		Justification:      Justification{Reason: "Test 1"},
	}
	r2 := &Request{
		ProjectPath:        sess2.ProjectPath,
		RequestorSessionID: sess2.ID,
		RequestorAgent:     sess2.AgentName,
		RequestorModel:     sess2.Model,
		RiskTier:           RiskTierDangerous,
		MinApprovals:       1,
		Command:            CommandSpec{Raw: "rm -rf /tmp/test2", Cwd: "/tmp"},
		Justification:      Justification{Reason: "Test 2"},
	}
	if err := db.CreateRequest(r1); err != nil {
		t.Fatalf("CreateRequest r1 failed: %v", err)
	}
	if err := db.CreateRequest(r2); err != nil {
		t.Fatalf("CreateRequest r2 failed: %v", err)
	}

	results, err = db.ListPendingRequestsByProjects([]string{sess1.ProjectPath})
	if err != nil {
		t.Fatalf("ListPendingRequestsByProjects failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].ID != r1.ID {
		t.Fatalf("expected %s, got %s", r1.ID, results[0].ID)
	}

	results, err = db.ListPendingRequestsByProjects([]string{sess1.ProjectPath, sess2.ProjectPath})
	if err != nil {
		t.Fatalf("ListPendingRequestsByProjects failed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
}

func TestUpdateRequestExecutionAndRollback(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	_, r := createTestRequest(t, db)

	execAt := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	exitCode := 7
	durationMs := int64(1234)
	exec := &Execution{
		ExecutedAt:          &execAt,
		ExecutedBySessionID: "sess-exec",
		ExecutedByAgent:     "ExecAgent",
		ExecutedByModel:     "gpt-5",
		LogPath:             "/tmp/slb.log",
		ExitCode:            &exitCode,
		DurationMs:          &durationMs,
	}
	if err := db.UpdateRequestExecution(r.ID, exec); err != nil {
		t.Fatalf("UpdateRequestExecution failed: %v", err)
	}

	if err := db.UpdateRequestRollbackPath(r.ID, "/tmp/rollback"); err != nil {
		t.Fatalf("UpdateRequestRollbackPath failed: %v", err)
	}
	rolledAt := time.Date(2024, 1, 2, 4, 5, 6, 0, time.UTC)
	if err := db.UpdateRequestRolledBackAt(r.ID, rolledAt); err != nil {
		t.Fatalf("UpdateRequestRolledBackAt failed: %v", err)
	}

	retrieved, err := db.GetRequest(r.ID)
	if err != nil {
		t.Fatalf("GetRequest failed: %v", err)
	}
	if retrieved.Execution == nil {
		t.Fatalf("expected execution info to be present")
	}
	if retrieved.Execution.LogPath != "/tmp/slb.log" {
		t.Fatalf("LogPath=%q want %q", retrieved.Execution.LogPath, "/tmp/slb.log")
	}
	if retrieved.Execution.ExitCode == nil || *retrieved.Execution.ExitCode != 7 {
		t.Fatalf("ExitCode=%v want 7", retrieved.Execution.ExitCode)
	}
	if retrieved.Execution.DurationMs == nil || *retrieved.Execution.DurationMs != 1234 {
		t.Fatalf("DurationMs=%v want 1234", retrieved.Execution.DurationMs)
	}
	if retrieved.Execution.ExecutedAt == nil || !retrieved.Execution.ExecutedAt.Equal(execAt) {
		t.Fatalf("ExecutedAt=%v want %v", retrieved.Execution.ExecutedAt, execAt)
	}
	if retrieved.Execution.ExecutedBySessionID != "sess-exec" {
		t.Fatalf("ExecutedBySessionID=%q want %q", retrieved.Execution.ExecutedBySessionID, "sess-exec")
	}
	if retrieved.Execution.ExecutedByAgent != "ExecAgent" {
		t.Fatalf("ExecutedByAgent=%q want %q", retrieved.Execution.ExecutedByAgent, "ExecAgent")
	}
	if retrieved.Execution.ExecutedByModel != "gpt-5" {
		t.Fatalf("ExecutedByModel=%q want %q", retrieved.Execution.ExecutedByModel, "gpt-5")
	}

	if retrieved.Rollback == nil {
		t.Fatalf("expected rollback info to be present")
	}
	if retrieved.Rollback.Path != "/tmp/rollback" {
		t.Fatalf("Rollback.Path=%q want %q", retrieved.Rollback.Path, "/tmp/rollback")
	}
	if retrieved.Rollback.RolledBackAt == nil || !retrieved.Rollback.RolledBackAt.Equal(rolledAt) {
		t.Fatalf("Rollback.RolledBackAt=%v want %v", retrieved.Rollback.RolledBackAt, rolledAt)
	}
}

func TestSessionRateLimitTimeWindowQueries(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	sess := &Session{AgentName: "RateAgent", Program: "codex-cli", Model: "gpt-5", ProjectPath: "/test/project"}
	if err := db.CreateSession(sess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	makeRequest := func() *Request {
		r := &Request{
			ProjectPath:        sess.ProjectPath,
			RequestorSessionID: sess.ID,
			RequestorAgent:     sess.AgentName,
			RequestorModel:     sess.Model,
			RiskTier:           RiskTierDangerous,
			MinApprovals:       1,
			Command:            CommandSpec{Raw: "rm -rf ./build", Cwd: sess.ProjectPath},
			Justification:      Justification{Reason: "rate test"},
		}
		if err := db.CreateRequest(r); err != nil {
			t.Fatalf("CreateRequest failed: %v", err)
		}
		return r
	}

	r1 := makeRequest()
	r2 := makeRequest()
	r3 := makeRequest()

	now := time.Now().UTC().Truncate(time.Second)
	t1 := now.Add(-10 * time.Minute)
	t2 := now.Add(-5 * time.Minute)
	t3 := now.Add(-1 * time.Minute)

	for id, ts := range map[string]time.Time{r1.ID: t1, r2.ID: t2, r3.ID: t3} {
		if _, err := db.Exec(`UPDATE requests SET created_at = ? WHERE id = ?`, ts.Format(time.RFC3339), id); err != nil {
			t.Fatalf("update created_at failed: %v", err)
		}
	}

	count, err := db.CountRequestsSince(sess.ID, t2)
	if err != nil {
		t.Fatalf("CountRequestsSince failed: %v", err)
	}
	if count != 2 {
		t.Fatalf("CountRequestsSince=%d want 2", count)
	}

	oldest, err := db.OldestRequestCreatedAtSince(sess.ID, t2)
	if err != nil {
		t.Fatalf("OldestRequestCreatedAtSince failed: %v", err)
	}
	if oldest == nil || !oldest.Equal(t2) {
		t.Fatalf("oldest=%v want %v", oldest, t2)
	}

	oldest, err = db.OldestRequestCreatedAtSince(sess.ID, now.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("OldestRequestCreatedAtSince failed: %v", err)
	}
	if oldest != nil {
		t.Fatalf("expected nil oldest for empty window, got %v", oldest)
	}

	recent, err := db.CountRecentRequestsBySession(sess.ID, 60*60) // last hour
	if err != nil {
		t.Fatalf("CountRecentRequestsBySession failed: %v", err)
	}
	if recent != 3 {
		t.Fatalf("CountRecentRequestsBySession=%d want 3", recent)
	}
}

func TestRequestHelpersAndEnums(t *testing.T) {
	if !RiskTierCritical.Valid() || RiskTier("nope").Valid() {
		t.Fatalf("RiskTier.Valid unexpected results")
	}
	if RiskTierCritical.MinApprovals() != 2 || RiskTierDangerous.MinApprovals() != 1 || RiskTierCaution.MinApprovals() != 0 {
		t.Fatalf("RiskTier.MinApprovals unexpected results")
	}
	if RiskTier("unknown").MinApprovals() != 2 {
		t.Fatalf("RiskTier.MinApprovals default should be 2")
	}

	if !StatusPending.Valid() || RequestStatus("nope").Valid() {
		t.Fatalf("RequestStatus.Valid unexpected results")
	}
	if !StatusExecuted.IsTerminal() || StatusPending.IsTerminal() {
		t.Fatalf("RequestStatus.IsTerminal unexpected results")
	}
	if !StatusPending.IsPending() || !StatusApproved.IsPending() || StatusExecuting.IsPending() {
		t.Fatalf("RequestStatus.IsPending unexpected results")
	}

	if !DecisionApprove.Valid() || Decision("nope").Valid() {
		t.Fatalf("Decision.Valid unexpected results")
	}

	req := &Request{ID: "req-1"}
	if req.IsExpired() {
		t.Fatalf("expected IsExpired=false when ExpiresAt is nil")
	}
	past := time.Now().UTC().Add(-1 * time.Minute)
	req.ExpiresAt = &past
	if !req.IsExpired() {
		t.Fatalf("expected IsExpired=true for past expiry")
	}
	future := time.Now().UTC().Add(1 * time.Minute)
	req.ExpiresAt = &future
	if req.IsExpired() {
		t.Fatalf("expected IsExpired=false for future expiry")
	}

	reviews := []Review{
		{RequestID: "req-1", Decision: DecisionApprove},
		{RequestID: "req-1", Decision: DecisionReject},
		{RequestID: "req-1", Decision: DecisionApprove},
		{RequestID: "req-2", Decision: DecisionApprove},
	}
	if got := req.ApprovalCount(reviews); got != 2 {
		t.Fatalf("ApprovalCount=%d want 2", got)
	}

	createdAt := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	resolvedAt := createdAt.Add(1 * time.Minute)
	expiresAt := createdAt.Add(2 * time.Minute)
	approvalExpiresAt := createdAt.Add(3 * time.Minute)
	req.CreatedAt = createdAt
	req.ResolvedAt = &resolvedAt
	req.ExpiresAt = &expiresAt
	req.ApprovalExpiresAt = &approvalExpiresAt

	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("MarshalJSON failed: %v", err)
	}

	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if out["created_at"] != createdAt.Format(time.RFC3339) {
		t.Fatalf("created_at=%v want %v", out["created_at"], createdAt.Format(time.RFC3339))
	}
	if out["resolved_at"] != resolvedAt.Format(time.RFC3339) {
		t.Fatalf("resolved_at=%v want %v", out["resolved_at"], resolvedAt.Format(time.RFC3339))
	}
	if out["expires_at"] != expiresAt.Format(time.RFC3339) {
		t.Fatalf("expires_at=%v want %v", out["expires_at"], expiresAt.Format(time.RFC3339))
	}
	if out["approval_expires_at"] != approvalExpiresAt.Format(time.RFC3339) {
		t.Fatalf("approval_expires_at=%v want %v", out["approval_expires_at"], approvalExpiresAt.Format(time.RFC3339))
	}
}

func TestListRequestsByStatus_ParsesOptionalFields(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	sess := &Session{AgentName: "OptAgent", Program: "codex-cli", Model: "gpt-5", ProjectPath: "/test/project"}
	if err := db.CreateSession(sess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	approvalExpiresAt := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	req := &Request{
		ProjectPath:        sess.ProjectPath,
		RequestorSessionID: sess.ID,
		RequestorAgent:     sess.AgentName,
		RequestorModel:     sess.Model,
		RiskTier:           RiskTierDangerous,
		MinApprovals:       1,
		Command: CommandSpec{
			Raw:              "echo secret",
			Argv:             []string{"echo", "secret"},
			Cwd:              sess.ProjectPath,
			Shell:            true,
			DisplayRedacted:  "echo [REDACTED]",
			ContainsSensitive: true,
		},
		Justification: Justification{
			Reason:         "Need to test parsing",
			ExpectedEffect: "Optional fields are persisted",
			Goal:           "Coverage",
			SafetyArgument: "Test-only",
		},
		DryRun: &DryRunResult{Command: "echo dry", Output: "ok"},
		Attachments: []Attachment{
			{Type: AttachmentTypeFile, Content: "README.md", Metadata: map[string]any{"path": "README.md"}},
		},
		ApprovalExpiresAt: &approvalExpiresAt,
	}
	if err := db.CreateRequest(req); err != nil {
		t.Fatalf("CreateRequest failed: %v", err)
	}

	list, err := db.ListRequestsByStatus(StatusPending, sess.ProjectPath)
	if err != nil {
		t.Fatalf("ListRequestsByStatus failed: %v", err)
	}

	var got *Request
	for _, r := range list {
		if r.ID == req.ID {
			got = r
			break
		}
	}
	if got == nil {
		t.Fatalf("expected request %s to be present", req.ID)
	}

	if got.Command.Shell != true {
		t.Fatalf("Shell=%v want true", got.Command.Shell)
	}
	if got.Command.DisplayRedacted != "echo [REDACTED]" {
		t.Fatalf("DisplayRedacted=%q want %q", got.Command.DisplayRedacted, "echo [REDACTED]")
	}
	if got.Command.ContainsSensitive != true {
		t.Fatalf("ContainsSensitive=%v want true", got.Command.ContainsSensitive)
	}
	if got.Justification.ExpectedEffect != "Optional fields are persisted" {
		t.Fatalf("ExpectedEffect=%q", got.Justification.ExpectedEffect)
	}
	if got.Justification.Goal != "Coverage" {
		t.Fatalf("Goal=%q", got.Justification.Goal)
	}
	if got.Justification.SafetyArgument != "Test-only" {
		t.Fatalf("SafetyArgument=%q", got.Justification.SafetyArgument)
	}
	if got.DryRun == nil || got.DryRun.Command != "echo dry" || got.DryRun.Output != "ok" {
		t.Fatalf("DryRun=%#v", got.DryRun)
	}
	if got.Attachments == nil || len(got.Attachments) != 1 {
		t.Fatalf("Attachments=%#v", got.Attachments)
	}
	if got.ApprovalExpiresAt == nil || !got.ApprovalExpiresAt.Equal(approvalExpiresAt) {
		t.Fatalf("ApprovalExpiresAt=%v want %v", got.ApprovalExpiresAt, approvalExpiresAt)
	}
}

func TestUpdateRequestStatus_TimeoutEscalatedAndTerminalNoTransition(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	_, r := createTestRequest(t, db)
	if err := db.UpdateRequestStatus(r.ID, StatusTimeout); err != nil {
		t.Fatalf("UpdateRequestStatus(timeout) failed: %v", err)
	}
	if err := db.UpdateRequestStatus(r.ID, StatusEscalated); err != nil {
		t.Fatalf("UpdateRequestStatus(escalated) failed: %v", err)
	}

	_, r2 := createTestRequest(t, db)
	if err := db.UpdateRequestStatus(r2.ID, StatusCancelled); err != nil {
		t.Fatalf("UpdateRequestStatus(cancelled) failed: %v", err)
	}

	err := db.UpdateRequestStatus(r2.ID, StatusApproved)
	if err == nil {
		t.Fatalf("expected invalid transition from terminal state")
	}
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition, got %v", err)
	}
}

// createTestRequest creates a test session and request.
func createTestRequest(t *testing.T, db *DB) (*Session, *Request) {
	t.Helper()

	// Each call creates a unique session
	sess := &Session{
		AgentName:   "Agent-" + time.Now().Format("150405.000000"),
		Program:     "claude-code",
		Model:       "opus-4.5",
		ProjectPath: "/test/project",
	}
	if err := db.CreateSession(sess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	r := &Request{
		ProjectPath:        "/test/project",
		RequestorSessionID: sess.ID,
		RequestorAgent:     sess.AgentName,
		RequestorModel:     "opus-4.5",
		RiskTier:           RiskTierDangerous,
		MinApprovals:       1,
		Command: CommandSpec{
			Raw:  "rm -rf ./build",
			Cwd:  "/test/project",
			Argv: []string{"rm", "-rf", "./build"},
		},
		Justification: Justification{
			Reason: "Clean build directory",
		},
	}
	if err := db.CreateRequest(r); err != nil {
		t.Fatalf("CreateRequest failed: %v", err)
	}

	return sess, r
}
