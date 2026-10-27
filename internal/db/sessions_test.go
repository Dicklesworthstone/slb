// Package db tests for session CRUD operations.
package db

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
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
	// Issue #30: reads never return the key. Only its digest is stored.
	if retrieved.SessionKey != "" {
		t.Errorf("GetSession returned the session key")
	}
	authenticated, err := db.AuthenticateSession(original.ID, original.SessionKey)
	if err != nil || authenticated.ID != original.ID || authenticated.SessionKey != original.SessionKey {
		t.Errorf("AuthenticateSession(original key) = %+v, %v", authenticated, err)
	}
	if _, err := db.AuthenticateSession(original.ID, strings.Repeat("00", 32)); !errors.Is(err, ErrSessionKeyMismatch) {
		t.Errorf("AuthenticateSession(wrong key) error = %v, want %v", err, ErrSessionKeyMismatch)
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

func TestDifferentModelQueriesRequireKnownDistinctLabels(t *testing.T) {
	for _, tc := range []struct {
		name, requester, reviewer string
		wantDifferent             bool
	}{
		{"missing reviewer", "model-a", "", false},
		{"missing requester", "", "model-b", false},
		{"blank reviewer", "model-a", "\t\u00a0 ", false},
		{"equivalent labels", " Model-A ", "model-a", false},
		{"different labels", " Model-A ", "model-b", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := setupTestDB(t)
			defer database.Close()
			session := &Session{AgentName: "reviewer", Model: tc.reviewer, ProjectPath: "/project"}
			if err := database.CreateSession(session); err != nil {
				t.Fatal(err)
			}
			list, err := database.ListActiveSessionsWithDifferentModel(session.ProjectPath, tc.requester)
			if err != nil || (len(list) == 1) != tc.wantDifferent {
				t.Fatalf("different-model list=%+v err=%v wantDifferent=%v", list, err, tc.wantDifferent)
			}
			has, err := database.HasActiveSessionWithDifferentModel(session.ProjectPath, tc.requester)
			if err != nil || has != tc.wantDifferent {
				t.Fatalf("different-model availability=%v err=%v want=%v", has, err, tc.wantDifferent)
			}
			status, err := database.GetDifferentModelStatus(session.ProjectPath, tc.requester)
			if err != nil || status.HasDifferentModel != tc.wantDifferent || (len(status.DifferentModelSessions) == 1) != tc.wantDifferent {
				t.Fatalf("different-model status=%+v err=%v wantDifferent=%v", status, err, tc.wantDifferent)
			}
			stored, err := database.GetSession(session.ID)
			if err != nil || stored.Model != tc.reviewer {
				t.Fatalf("availability queries changed the model label: %+v %v", stored, err)
			}
		})
	}
}

func TestIsUniqueConstraintError_Nil(t *testing.T) {
	if isUniqueConstraintError(nil) {
		t.Fatalf("expected nil error to be non-unique")
	}
}

func TestIsUniqueConstraintError_SQLiteErrors(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()
	if _, err := database.Exec(`CREATE TABLE constraint_fixture (
		id TEXT PRIMARY KEY, value TEXT UNIQUE, required_value TEXT NOT NULL,
		positive INTEGER CHECK(positive > 0), parent_id TEXT REFERENCES constraint_fixture(id)
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO constraint_fixture (id, value, required_value, positive)
		VALUES ('original', 'duplicate', 'present', 1)`); err != nil {
		t.Fatal(err)
	}
	// A trigger can choose arbitrary text. Even a message resembling a unique
	// constraint must remain a trigger/storage failure, not a duplicate result.
	if _, err := database.Exec(`CREATE TRIGGER misleading_constraint BEFORE INSERT ON constraint_fixture
		WHEN NEW.id = 'trigger'
		BEGIN SELECT RAISE(ABORT, 'UNIQUE constraint failed: misleading trigger text'); END`); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, query string
		unique      bool
	}{
		{"primary key", `INSERT INTO constraint_fixture VALUES ('original', 'new', 'present', 1, NULL)`, true},
		{"unique", `INSERT INTO constraint_fixture VALUES ('new', 'duplicate', 'present', 1, NULL)`, true},
		{"rowid", `INSERT INTO constraint_fixture (rowid, id, value, required_value, positive) VALUES (1, 'new', 'new', 'present', 1)`, true},
		{"not null", `INSERT INTO constraint_fixture VALUES ('new', 'new', NULL, 1, NULL)`, false},
		{"check", `INSERT INTO constraint_fixture VALUES ('new', 'new', 'present', 0, NULL)`, false},
		{"foreign key", `INSERT INTO constraint_fixture VALUES ('new', 'new', 'present', 1, 'missing')`, false},
		{"trigger", `INSERT INTO constraint_fixture VALUES ('trigger', 'new', 'present', 1, NULL)`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := database.Exec(test.query)
			if err == nil {
				t.Fatal("fixture did not produce the expected constraint failure")
			}
			if got := isUniqueConstraintError(err); got != test.unique {
				t.Fatalf("constraint classified as unique=%v, want %v: %v", got, test.unique, err)
			}
			if got := isUniqueConstraintError(fmt.Errorf("wrapped insertion: %w", err)); got != test.unique {
				t.Fatalf("wrapped constraint classified as unique=%v, want %v: %v", got, test.unique, err)
			}
		})
	}
}

func TestIsUniqueConstraintError_ForeignKeyNotUnique(t *testing.T) {
	// CRITICAL: Foreign key errors should NOT be classified as unique constraint errors
	// This bug was causing confusing error messages like "review already exists" when
	// the actual problem was a missing session (FK violation)
	tests := []struct {
		name   string
		errMsg string
	}{
		{
			name:   "foreign key constraint failed",
			errMsg: "constraint failed: FOREIGN KEY constraint failed (787)",
		},
		{
			name:   "lowercase foreign key",
			errMsg: "foreign key constraint failed: reviews.reviewer_session_id",
		},
		{
			name:   "mixed case foreign key",
			errMsg: "FOREIGN KEY constraint failed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := &testError{msg: tc.errMsg}
			if isUniqueConstraintError(err) {
				t.Errorf("isUniqueConstraintError(%q) = true, want false (FK errors are not unique violations)", tc.errMsg)
			}
		})
	}
}

func TestIsUniqueConstraintError_OtherErrors(t *testing.T) {
	// Test that other errors are not classified as unique constraint errors
	tests := []struct {
		name   string
		errMsg string
	}{
		{
			name:   "generic error",
			errMsg: "something went wrong",
		},
		{
			name:   "database locked",
			errMsg: "database is locked",
		},
		{
			name:   "no rows",
			errMsg: "sql: no rows in result set",
		},
		{
			name:   "untyped unique text",
			errMsg: "UNIQUE constraint failed: sessions.id",
		},
		{
			name:   "generic constraint",
			errMsg: "constraint failed: some_table.some_column",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := &testError{msg: tc.errMsg}
			if isUniqueConstraintError(err) {
				t.Errorf("isUniqueConstraintError(%q) = true, want false", tc.errMsg)
			}
		})
	}
}

// testError is a simple error type for testing
type testError struct {
	msg string
}

func (e *testError) Error() string {
	return e.msg
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

func replacementSessionFixture(t *testing.T) (*DB, *Session) {
	t.Helper()
	database := setupTestDB(t)
	t.Cleanup(func() { _ = database.Close() })
	session := &Session{AgentName: "rotating-agent", Program: "original-program", Model: "original-model", ProjectPath: "/test/session-rotation"}
	if err := database.CreateSession(session); err != nil {
		t.Fatal(err)
	}
	return database, session
}

func replacementRequestFixture(t *testing.T, database *DB, session *Session, status RequestStatus) *Request {
	t.Helper()
	request := &Request{
		ProjectPath: session.ProjectPath, RequestorSessionID: session.ID,
		RequestorAgent: session.AgentName, RequestorModel: session.Model,
		Command:  CommandSpec{Raw: "echo session-work", Cwd: session.ProjectPath},
		RiskTier: RiskTierDangerous, MinApprovals: 1, Status: status,
		Justification: Justification{Reason: "outstanding session work"},
	}
	if err := database.CreateRequest(request); err != nil {
		t.Fatal(err)
	}
	return request
}

func TestReplaceSessionPreservesHistoricalIdentity(t *testing.T) {
	database, original := replacementSessionFixture(t)
	before, err := database.GetSession(original.ID)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := database.ReplaceSession(original, original.SessionKey, "replacement-program", "replacement-model")
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ID == original.ID || replacement.SessionKey == original.SessionKey || len(replacement.SessionKey) != 64 ||
		replacement.AgentName != original.AgentName || replacement.ProjectPath != original.ProjectPath ||
		replacement.Program != "replacement-program" || replacement.Model != "replacement-model" || !replacement.IsActive() {
		t.Fatal("replacement did not create the requested distinct identity")
	}
	ended, err := database.GetSession(original.ID)
	if err != nil || ended.EndedAt == nil {
		t.Fatalf("original session was not ended: %v", err)
	}
	ended.EndedAt = nil
	if !reflect.DeepEqual(before, ended) {
		t.Fatal("replacement rewrote historical identity or heartbeat")
	}
	active, err := database.GetActiveSession(original.AgentName, original.ProjectPath)
	if err != nil || active.ID != replacement.ID {
		t.Fatalf("replacement is not the unique active identity: %v", err)
	}
	for _, session := range []*Session{original, replacement} {
		var plaintext, digest string
		if err := database.QueryRow(`SELECT session_key, session_key_digest FROM sessions WHERE id = ?`, session.ID).Scan(&plaintext, &digest); err != nil {
			t.Fatal(err)
		}
		if plaintext != "" || !SessionKeyMatches(digest, session.SessionKey) {
			t.Fatal("replacement lost the original digest or persisted plaintext credentials")
		}
	}
	if _, err := database.AuthenticateSession(replacement.ID, original.SessionKey); !errors.Is(err, ErrSessionKeyMismatch) {
		t.Fatalf("original key authenticated replacement: %v", err)
	}
}

func TestReplaceSessionFailureKeepsOriginalActive(t *testing.T) {
	database, original := replacementSessionFixture(t)
	before, err := database.GetSession(original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`CREATE TRIGGER replacement_insert_failure BEFORE INSERT ON sessions
		BEGIN SELECT RAISE(ABORT, 'replacement insertion failed'); END`); err != nil {
		t.Fatal(err)
	}
	got, err := database.ReplaceSession(original, original.SessionKey, original.Program, "changed-model")
	if got != nil || err == nil || !strings.Contains(err.Error(), "replacement insertion failed") {
		t.Fatalf("failed insert returned a session or hid storage error: %v", err)
	}
	after, err := database.GetSession(original.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("replacement failure did not roll back original identity: %v", err)
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("failed replacement left a second row: count=%d err=%v", count, err)
	}
}

func TestReplaceSessionRechecksCredentialsAndSnapshot(t *testing.T) {
	for _, name := range []string{"wrong-key", "missing", "ended", "changed-model", "wrong-project"} {
		t.Run(name, func(t *testing.T) {
			database, original := replacementSessionFixture(t)
			expected := *original
			key, want := original.SessionKey, ErrSessionChanged
			switch name {
			case "wrong-key":
				key, want = strings.Repeat("00", 32), ErrSessionKeyMismatch
			case "missing":
				expected.ID, want = "missing-session", ErrSessionNotFound
			case "ended":
				if err := database.EndSession(original.ID); err != nil {
					t.Fatal(err)
				}
			case "changed-model":
				if err := database.UpdateSessionModel(original.ID, "changed-before-transaction"); err != nil {
					t.Fatal(err)
				}
			case "wrong-project":
				expected.ProjectPath = "/other-project"
			}
			before, err := database.GetSession(original.ID)
			if err != nil {
				t.Fatal(err)
			}
			got, err := database.ReplaceSession(&expected, key, original.Program, "replacement-model")
			if got != nil || !errors.Is(err, want) {
				t.Fatalf("stale or unauthenticated replacement: %v", err)
			}
			after, err := database.GetSession(original.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("rejected replacement changed original session: %v", err)
			}
		})
	}
}

func TestReplaceSessionRefusesOutstandingRequests(t *testing.T) {
	for _, status := range []RequestStatus{StatusPending, StatusApproved, StatusEscalated} {
		t.Run(string(status), func(t *testing.T) {
			database, original := replacementSessionFixture(t)
			request := replacementRequestFixture(t, database, original, status)
			before, err := database.GetSession(original.ID)
			if err != nil {
				t.Fatal(err)
			}
			got, err := database.ReplaceSession(original, original.SessionKey, original.Program, "replacement-model")
			if got != nil || !errors.Is(err, ErrSessionHasLiveRequests) || !strings.Contains(err.Error(), request.ID) ||
				!strings.Contains(err.Error(), string(status)) || !strings.Contains(err.Error(), "original session is unchanged") {
				t.Fatalf("outstanding work was stranded or guidance was lost: %v", err)
			}
			after, err := database.GetSession(original.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("blocked rotation changed original session: %v", err)
			}
			stored, err := database.GetRequest(request.ID)
			if err != nil || stored.RequestorSessionID != original.ID || stored.RequestorModel != original.Model || stored.Status != status {
				t.Fatalf("blocked rotation rebound outstanding request: %v", err)
			}
		})
	}
}

func TestReplaceSessionAllowsResolvedRequests(t *testing.T) {
	for _, status := range []RequestStatus{StatusExecuted, StatusExecutionFailed, StatusCancelled, StatusRejected, StatusTimeout, StatusTimedOut} {
		t.Run(string(status), func(t *testing.T) {
			database, original := replacementSessionFixture(t)
			request := replacementRequestFixture(t, database, original, status)
			if _, err := database.ReplaceSession(original, original.SessionKey, original.Program, "replacement-model"); err != nil {
				t.Fatalf("resolved request blocked replacement: %v", err)
			}
			stored, err := database.GetRequest(request.ID)
			if err != nil || stored.RequestorSessionID != original.ID || stored.RequestorModel != original.Model || stored.Status != status {
				t.Fatalf("replacement rebound historical request: %v", err)
			}
		})
	}
}

func TestReplaceSessionConcurrentSingleWinner(t *testing.T) {
	database, original := replacementSessionFixture(t)
	other, err := Open(database.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	type result struct {
		session *Session
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, connection := range []*DB{database, other} {
		go func(connection *DB) {
			<-start
			session, err := connection.ReplaceSession(original, original.SessionKey, original.Program, "replacement-model")
			results <- result{session, err}
		}(connection)
	}
	close(start)
	var winner *Session
	for range 2 {
		got := <-results
		if got.err == nil {
			if winner != nil || got.session == nil {
				t.Fatal("replacement did not have exactly one winner")
			}
			winner = got.session
		} else if got.session != nil || !errors.Is(got.err, ErrSessionChanged) {
			t.Fatalf("loser returned credentials or an unexpected error: %v", got.err)
		}
	}
	if winner == nil {
		t.Fatal("no replacement succeeded")
	}
	active, err := database.ListActiveSessions(original.ProjectPath)
	if err != nil || len(active) != 1 || active[0].ID != winner.ID {
		t.Fatalf("concurrent replacement violated unique active identity: %v", err)
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("concurrent replacement persisted more than one new identity: count=%d err=%v", count, err)
	}
}

func TestEndStaleSessionRechecksActivityAndProject(t *testing.T) {
	database, original := replacementSessionFixture(t)
	cutoff := time.Now().UTC().Add(-time.Hour)
	if _, err := database.Exec(`UPDATE sessions SET last_active_at = ? WHERE id = ?`, cutoff.Add(-time.Hour).Format(time.RFC3339), original.ID); err != nil {
		t.Fatal(err)
	}
	candidates, err := database.FindStaleProjectSessions(original.ProjectPath, cutoff)
	if err != nil || len(candidates) != 1 || candidates[0].ID != original.ID {
		t.Fatalf("stale candidate not found: %v", err)
	}
	if ended, err := database.EndStaleSession(original.ID, "/other-project", cutoff); err != nil || ended {
		t.Fatalf("cleanup crossed project boundary: %v", err)
	}
	if err := database.UpdateSessionHeartbeat(original.ID); err != nil {
		t.Fatal(err)
	}
	if ended, err := database.EndStaleSession(original.ID, original.ProjectPath, cutoff); err != nil || ended {
		t.Fatalf("cleanup ended a session refreshed after selection: %v", err)
	}
	if _, err := database.Exec(`UPDATE sessions SET last_active_at = ? WHERE id = ?`, cutoff.Add(-time.Hour).Format(time.RFC3339), original.ID); err != nil {
		t.Fatal(err)
	}
	if ended, err := database.EndStaleSession(original.ID, original.ProjectPath, cutoff); err != nil || !ended {
		t.Fatalf("still-stale session was not ended: %v", err)
	}
	for _, id := range []string{original.ID, "missing-session"} {
		if ended, err := database.EndStaleSession(id, original.ProjectPath, cutoff); err != nil || ended {
			t.Fatalf("missing or ended session was not skipped: %v", err)
		}
	}
}

func TestEndStaleSessionPreservesRefreshInCutoffSecond(t *testing.T) {
	database, original := replacementSessionFixture(t)
	cutoff := time.Date(2026, time.October, 27, 12, 0, 0, 700_000_000, time.UTC)
	// A heartbeat at 12:00:00.800 persists as 12:00:00. It must survive a
	// 12:00:00.700 cutoff, including when selected before the heartbeat.
	refreshed := cutoff.Add(100 * time.Millisecond).Format(time.RFC3339)
	if _, err := database.Exec(`UPDATE sessions SET last_active_at = ? WHERE id = ?`, refreshed, original.ID); err != nil {
		t.Fatal(err)
	}
	candidates, err := database.FindStaleProjectSessions(original.ProjectPath, cutoff)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("fractional cutoff selected a refreshed session: count=%d err=%v", len(candidates), err)
	}
	if ended, err := database.EndStaleSession(original.ID, original.ProjectPath, cutoff); err != nil || ended {
		t.Fatalf("fractional cutoff ended a refreshed session: %v", err)
	}
	// Rounding is conservative by one second, not an indefinite exemption.
	if _, err := database.Exec(`UPDATE sessions SET last_active_at = ? WHERE id = ?`, cutoff.Add(-time.Second).Format(time.RFC3339), original.ID); err != nil {
		t.Fatal(err)
	}
	candidates, err = database.FindStaleProjectSessions(original.ProjectPath, cutoff)
	if err != nil || len(candidates) != 1 || candidates[0].ID != original.ID {
		t.Fatalf("prior-second activity was not stale: count=%d err=%v", len(candidates), err)
	}
	if ended, err := database.EndStaleSession(original.ID, original.ProjectPath, cutoff); err != nil || !ended {
		t.Fatalf("prior-second stale session was not ended: %v", err)
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
