// Package db tests for session CRUD operations.
package db

import (
	"path/filepath"
	"testing"
	"time"
)

func TestCreateSession(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	s := &Session{
		AgentName:   "GreenLake",
		Program:     "claude-code",
		Model:       "opus-4.5",
		ProjectPath: "/test/project",
	}

	err := db.CreateSession(s)
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	// Verify UUID was generated
	if s.ID == "" {
		t.Error("Expected UUID to be generated")
	}

	// Verify session key was generated
	if s.SessionKey == "" {
		t.Error("Expected session key to be generated")
	}
	if len(s.SessionKey) != 64 { // 32 bytes = 64 hex chars
		t.Errorf("Expected session key length 64, got %d", len(s.SessionKey))
	}

	// Verify timestamps were set
	if s.StartedAt.IsZero() {
		t.Error("Expected StartedAt to be set")
	}
	if s.LastActiveAt.IsZero() {
		t.Error("Expected LastActiveAt to be set")
	}
	if s.EndedAt != nil {
		t.Error("Expected EndedAt to be nil for new session")
	}
}

func TestCreateSessionDuplicateActive(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	s1 := &Session{
		AgentName:   "GreenLake",
		Program:     "claude-code",
		Model:       "opus-4.5",
		ProjectPath: "/test/project",
	}
	if err := db.CreateSession(s1); err != nil {
		t.Fatalf("CreateSession s1 failed: %v", err)
	}

	// Try to create another active session for same agent+project
	s2 := &Session{
		AgentName:   "GreenLake",
		Program:     "claude-code",
		Model:       "opus-4.5",
		ProjectPath: "/test/project",
	}
	err := db.CreateSession(s2)
	if err != ErrActiveSessionExists {
		t.Errorf("Expected ErrActiveSessionExists, got: %v", err)
	}

	// But should work for different agent
	s3 := &Session{
		AgentName:   "BlueDog",
		Program:     "codex-cli",
		Model:       "gpt-5.1",
		ProjectPath: "/test/project",
	}
	if err := db.CreateSession(s3); err != nil {
		t.Fatalf("CreateSession s3 (different agent) failed: %v", err)
	}

	// And should work for different project
	s4 := &Session{
		AgentName:   "GreenLake",
		Program:     "claude-code",
		Model:       "opus-4.5",
		ProjectPath: "/test/other-project",
	}
	if err := db.CreateSession(s4); err != nil {
		t.Fatalf("CreateSession s4 (different project) failed: %v", err)
	}
}

func TestGetSession(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	original := &Session{
		AgentName:   "GreenLake",
		Program:     "claude-code",
		Model:       "opus-4.5",
		ProjectPath: "/test/project",
	}
	if err := db.CreateSession(original); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	retrieved, err := db.GetSession(original.ID)
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}

	if retrieved.ID != original.ID {
		t.Errorf("ID mismatch: got %s, want %s", retrieved.ID, original.ID)
	}
	if retrieved.AgentName != original.AgentName {
		t.Errorf("AgentName mismatch: got %s, want %s", retrieved.AgentName, original.AgentName)
	}
	if retrieved.SessionKey != original.SessionKey {
		t.Errorf("SessionKey mismatch")
	}
}

func TestGetSessionNotFound(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	_, err := db.GetSession("nonexistent-id")
	if err != ErrSessionNotFound {
		t.Errorf("Expected ErrSessionNotFound, got: %v", err)
	}
}

func TestGetActiveSession(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	s := &Session{
		AgentName:   "GreenLake",
		Program:     "claude-code",
		Model:       "opus-4.5",
		ProjectPath: "/test/project",
	}
	if err := db.CreateSession(s); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	retrieved, err := db.GetActiveSession("GreenLake", "/test/project")
	if err != nil {
		t.Fatalf("GetActiveSession failed: %v", err)
	}

	if retrieved.ID != s.ID {
		t.Errorf("ID mismatch: got %s, want %s", retrieved.ID, s.ID)
	}

	// After ending, should not be found
	if err := db.EndSession(s.ID); err != nil {
		t.Fatalf("EndSession failed: %v", err)
	}

	_, err = db.GetActiveSession("GreenLake", "/test/project")
	if err != ErrSessionNotFound {
		t.Errorf("Expected ErrSessionNotFound after ending session, got: %v", err)
	}
}

func TestListActiveSessions(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	projectPath := "/test/project"

	// Create multiple sessions
	for _, agent := range []string{"GreenLake", "BlueDog", "RedCat"} {
		s := &Session{
			AgentName:   agent,
			Program:     "claude-code",
			Model:       "opus-4.5",
			ProjectPath: projectPath,
		}
		if err := db.CreateSession(s); err != nil {
			t.Fatalf("CreateSession for %s failed: %v", agent, err)
		}
	}

	sessions, err := db.ListActiveSessions(projectPath)
	if err != nil {
		t.Fatalf("ListActiveSessions failed: %v", err)
	}

	if len(sessions) != 3 {
		t.Errorf("Expected 3 sessions, got %d", len(sessions))
	}
}

func TestListAllActiveSessions(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	s1 := &Session{
		AgentName:   "Agent1",
		Program:     "claude-code",
		Model:       "opus-4.5",
		ProjectPath: "/test/project1",
	}
	if err := db.CreateSession(s1); err != nil {
		t.Fatalf("CreateSession s1 failed: %v", err)
	}

	s2 := &Session{
		AgentName:   "Agent2",
		Program:     "codex-cli",
		Model:       "gpt-5",
		ProjectPath: "/test/project2",
	}
	if err := db.CreateSession(s2); err != nil {
		t.Fatalf("CreateSession s2 failed: %v", err)
	}

	s3 := &Session{
		AgentName:   "Agent3",
		Program:     "codex-cli",
		Model:       "gpt-5",
		ProjectPath: "/test/project1",
	}
	if err := db.CreateSession(s3); err != nil {
		t.Fatalf("CreateSession s3 failed: %v", err)
	}

	if err := db.EndSession(s2.ID); err != nil {
		t.Fatalf("EndSession failed: %v", err)
	}

	sessions, err := db.ListAllActiveSessions()
	if err != nil {
		t.Fatalf("ListAllActiveSessions failed: %v", err)
	}

	if len(sessions) != 2 {
		t.Fatalf("Expected 2 active sessions, got %d", len(sessions))
	}
	for _, s := range sessions {
		if s.ID == s2.ID {
			t.Fatalf("Expected ended session to be excluded")
		}
	}
}

func TestUpdateSessionHeartbeat(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	s := &Session{
		AgentName:   "GreenLake",
		Program:     "claude-code",
		Model:       "opus-4.5",
		ProjectPath: "/test/project",
	}
	if err := db.CreateSession(s); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	// Manually set an old timestamp to ensure clear difference
	oldTime := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	_, err := db.Exec(`UPDATE sessions SET last_active_at = ? WHERE id = ?`, oldTime, s.ID)
	if err != nil {
		t.Fatalf("Failed to set old last_active_at: %v", err)
	}

	// Now update via heartbeat
	if err := db.UpdateSessionHeartbeat(s.ID); err != nil {
		t.Fatalf("UpdateSessionHeartbeat failed: %v", err)
	}

	// Retrieve and verify
	retrieved, err := db.GetSession(s.ID)
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}

	// The heartbeat should have updated it to "now", which is more recent than 1 hour ago
	oldTimeParsed, _ := time.Parse(time.RFC3339, oldTime)
	if !retrieved.LastActiveAt.After(oldTimeParsed) {
		t.Errorf("Expected LastActiveAt to be updated. Old: %v, New: %v", oldTimeParsed, retrieved.LastActiveAt)
	}
}

func TestUpdateSessionHeartbeatNotFound(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	err := db.UpdateSessionHeartbeat("nonexistent-id")
	if err != ErrSessionNotFound {
		t.Errorf("Expected ErrSessionNotFound, got: %v", err)
	}
}

func TestEndSession(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	s := &Session{
		AgentName:   "GreenLake",
		Program:     "claude-code",
		Model:       "opus-4.5",
		ProjectPath: "/test/project",
	}
	if err := db.CreateSession(s); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	if !s.IsActive() {
		t.Error("Expected session to be active initially")
	}

	if err := db.EndSession(s.ID); err != nil {
		t.Fatalf("EndSession failed: %v", err)
	}

	// Retrieve and verify
	retrieved, err := db.GetSession(s.ID)
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}

	if retrieved.IsActive() {
		t.Error("Expected session to be inactive after ending")
	}
	if retrieved.EndedAt == nil {
		t.Error("Expected EndedAt to be set")
	}
}

func TestEndSessionNotFound(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	if err := db.EndSession("nonexistent-id"); err != ErrSessionNotFound {
		t.Fatalf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestFindStaleSessions(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create a session and manually set old last_active_at
	s := &Session{
		AgentName:   "GreenLake",
		Program:     "claude-code",
		Model:       "opus-4.5",
		ProjectPath: "/test/project",
	}
	if err := db.CreateSession(s); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	// Manually update to an old timestamp
	oldTime := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	_, err := db.Exec(`UPDATE sessions SET last_active_at = ? WHERE id = ?`, oldTime, s.ID)
	if err != nil {
		t.Fatalf("Failed to set old last_active_at: %v", err)
	}

	// Find stale sessions (threshold: 1 hour)
	staleSessions, err := db.FindStaleSessions(1 * time.Hour)
	if err != nil {
		t.Fatalf("FindStaleSessions failed: %v", err)
	}

	if len(staleSessions) != 1 {
		t.Errorf("Expected 1 stale session, got %d", len(staleSessions))
	}
	if len(staleSessions) > 0 && staleSessions[0].ID != s.ID {
		t.Errorf("Stale session ID mismatch")
	}

	// With longer threshold, should not find it
	notStale, err := db.FindStaleSessions(3 * time.Hour)
	if err != nil {
		t.Fatalf("FindStaleSessions failed: %v", err)
	}
	if len(notStale) != 0 {
		t.Errorf("Expected 0 stale sessions with longer threshold, got %d", len(notStale))
	}
}

func TestCreateSessionAllowsAfterEnd(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create and end a session
	s1 := &Session{
		AgentName:   "GreenLake",
		Program:     "claude-code",
		Model:       "opus-4.5",
		ProjectPath: "/test/project",
	}
	if err := db.CreateSession(s1); err != nil {
		t.Fatalf("CreateSession s1 failed: %v", err)
	}
	if err := db.EndSession(s1.ID); err != nil {
		t.Fatalf("EndSession failed: %v", err)
	}

	// Should be able to create a new session for same agent+project
	s2 := &Session{
		AgentName:   "GreenLake",
		Program:     "claude-code",
		Model:       "opus-4.5",
		ProjectPath: "/test/project",
	}
	if err := db.CreateSession(s2); err != nil {
		t.Fatalf("CreateSession s2 (after ending s1) failed: %v", err)
	}
}

func TestSessionRateLimitResetAt(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	s := &Session{
		AgentName:   "RateLimitAgent",
		Program:     "codex-cli",
		Model:       "gpt-5",
		ProjectPath: "/test/project",
	}
	if err := db.CreateSession(s); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	got, err := db.GetSessionRateLimitResetAt(s.ID)
	if err != nil {
		t.Fatalf("GetSessionRateLimitResetAt failed: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil resetAt, got %v", got)
	}

	resetAt := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	recorded, err := db.ResetSessionRateLimits(s.ID, resetAt)
	if err != nil {
		t.Fatalf("ResetSessionRateLimits failed: %v", err)
	}
	if !recorded.Equal(resetAt) {
		t.Fatalf("recorded=%v want %v", recorded, resetAt)
	}

	got, err = db.GetSessionRateLimitResetAt(s.ID)
	if err != nil {
		t.Fatalf("GetSessionRateLimitResetAt failed: %v", err)
	}
	if got == nil || !got.Equal(resetAt) {
		t.Fatalf("resetAt=%v want %v", got, resetAt)
	}

	if _, err := db.Exec(`UPDATE sessions SET rate_limit_reset_at = ? WHERE id = ?`, "not-a-time", s.ID); err != nil {
		t.Fatalf("set invalid rate_limit_reset_at failed: %v", err)
	}
	if _, err := db.GetSessionRateLimitResetAt(s.ID); err == nil {
		t.Fatalf("expected parse error")
	}

	if err := db.EndSession(s.ID); err != nil {
		t.Fatalf("EndSession failed: %v", err)
	}
	if _, err := db.ResetSessionRateLimits(s.ID, time.Now()); err != ErrSessionNotFound {
		t.Fatalf("expected ErrSessionNotFound after ending session, got %v", err)
	}
	if _, err := db.GetSessionRateLimitResetAt(s.ID); err != ErrSessionNotFound {
		t.Fatalf("expected ErrSessionNotFound after ending session, got %v", err)
	}
}

func TestDifferentModelSessionQueries(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	projectPath := "/test/project"

	same1 := &Session{AgentName: "Same1", Program: "codex-cli", Model: "opus-4.5", ProjectPath: projectPath}
	same2 := &Session{AgentName: "Same2", Program: "codex-cli", Model: "opus-4.5", ProjectPath: projectPath}
	diff := &Session{AgentName: "Diff", Program: "codex-cli", Model: "gpt-5", ProjectPath: projectPath}

	for _, s := range []*Session{same1, same2, diff} {
		if err := db.CreateSession(s); err != nil {
			t.Fatalf("CreateSession failed: %v", err)
		}
	}

	list, err := db.ListActiveSessionsWithDifferentModel(projectPath, "opus-4.5")
	if err != nil {
		t.Fatalf("ListActiveSessionsWithDifferentModel failed: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 different-model session, got %d", len(list))
	}
	if list[0].ID != diff.ID {
		t.Fatalf("expected different-model session %s, got %s", diff.ID, list[0].ID)
	}

	has, err := db.HasActiveSessionWithDifferentModel(projectPath, "opus-4.5")
	if err != nil {
		t.Fatalf("HasActiveSessionWithDifferentModel failed: %v", err)
	}
	if !has {
		t.Fatalf("expected HasDifferentModel=true")
	}

	status, err := db.GetDifferentModelStatus(projectPath, "opus-4.5")
	if err != nil {
		t.Fatalf("GetDifferentModelStatus failed: %v", err)
	}
	if !status.HasDifferentModel {
		t.Fatalf("expected HasDifferentModel=true")
	}
	if len(status.AvailableModels) != 2 {
		t.Fatalf("expected 2 available models, got %d", len(status.AvailableModels))
	}
	if len(status.SameModelSessions) != 2 {
		t.Fatalf("expected 2 same-model sessions, got %d", len(status.SameModelSessions))
	}
	if len(status.DifferentModelSessions) != 1 {
		t.Fatalf("expected 1 different-model session, got %d", len(status.DifferentModelSessions))
	}

	if err := db.EndSession(diff.ID); err != nil {
		t.Fatalf("EndSession failed: %v", err)
	}

	has, err = db.HasActiveSessionWithDifferentModel(projectPath, "opus-4.5")
	if err != nil {
		t.Fatalf("HasActiveSessionWithDifferentModel failed: %v", err)
	}
	if has {
		t.Fatalf("expected HasDifferentModel=false after ending different-model session")
	}
	list, err = db.ListActiveSessionsWithDifferentModel(projectPath, "opus-4.5")
	if err != nil {
		t.Fatalf("ListActiveSessionsWithDifferentModel failed: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected 0 different-model sessions, got %d", len(list))
	}
}

func TestIsUniqueConstraintError_Nil(t *testing.T) {
	if isUniqueConstraintError(nil) {
		t.Fatalf("expected nil error to be non-unique")
	}
}

func TestScanSessions_BadRows(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	s := &Session{
		AgentName:   "BadRows",
		Program:     "codex-cli",
		Model:       "gpt-5",
		ProjectPath: "/test/project",
	}
	if err := db.CreateSession(s); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	rows, err := db.Query(`SELECT id FROM sessions`)
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}
	defer rows.Close()

	if _, err := scanSessions(rows); err == nil {
		t.Fatalf("expected scanSessions to fail with wrong column count")
	}
}

// setupTestDB creates a temporary database for testing.
func setupTestDB(t *testing.T) *DB {
	t.Helper()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}

	return db
}
