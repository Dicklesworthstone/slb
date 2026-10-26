// Package db provides session CRUD operations.
package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ErrActiveSessionExists is returned when creating a session that would duplicate
// an active session for the same agent+project combination.
var ErrActiveSessionExists = errors.New("active session already exists for this agent and project")

// ErrSessionNotFound is returned when a session is not found.
var ErrSessionNotFound = errors.New("session not found")

// sessionColumns are the session fields every read returns. The key's digest
// is read only by the queries that authenticate a supplied key.
const sessionColumns = "id, agent_name, program, model, project_path, started_at, last_active_at, ended_at"

// CreateSession creates a new session in the database.
// Generates a UUID and a random session key. Only the key's digest is stored;
// s.SessionKey is the one copy of the key, for the caller to hand to the agent.
// Returns ErrActiveSessionExists if an active session already exists for the agent+project.
func (db *DB) CreateSession(s *Session) error {
	if s.AgentName == "" {
		return fmt.Errorf("agent_name is required")
	}
	if s.ProjectPath == "" {
		return fmt.Errorf("project_path is required")
	}

	// Generate UUID if not set
	if s.ID == "" {
		s.ID = uuid.New().String()
	}

	// Generate session key (32 bytes = 256 bits for HMAC-SHA256)
	if s.SessionKey == "" {
		key := make([]byte, sessionKeyBytes)
		if _, err := rand.Read(key); err != nil {
			return fmt.Errorf("generating session key: %w", err)
		}
		s.SessionKey = hex.EncodeToString(key)
	}
	digest, ok := SessionKeyDigest(s.SessionKey)
	if !ok {
		return ErrMalformedSessionKey
	}

	// Set timestamps
	now := time.Now().UTC()
	s.StartedAt = now
	s.LastActiveAt = now
	s.EndedAt = nil

	// Insert into database. session_key stays empty: older binaries read it
	// as the key, and an empty key never authenticates there.
	_, err := db.Exec(`
		INSERT INTO sessions (id, agent_name, program, model, project_path, session_key, session_key_digest, started_at, last_active_at, ended_at)
		VALUES (?, ?, ?, ?, ?, '', ?, ?, ?, NULL)
	`, s.ID, s.AgentName, s.Program, s.Model, s.ProjectPath, digest, s.StartedAt.Format(time.RFC3339), s.LastActiveAt.Format(time.RFC3339))

	if err != nil {
		// Check for unique constraint violation (active session already exists)
		if isUniqueConstraintError(err) {
			return ErrActiveSessionExists
		}
		return fmt.Errorf("creating session: %w", err)
	}

	return nil
}

// UpdateSessionModel updates the model for an active session.
func (db *DB) UpdateSessionModel(id, newModel string) error {
	result, err := db.Exec(`
		UPDATE sessions SET model = ? WHERE id = ? AND ended_at IS NULL
	`, newModel, id)
	if err != nil {
		return fmt.Errorf("updating session model: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("getting rows affected: %w", err)
	}
	if rowsAffected == 0 {
		return ErrSessionNotFound
	}

	return nil
}

// GetSession retrieves a session by ID.
func (db *DB) GetSession(id string) (*Session, error) {
	row := db.QueryRow(`
		SELECT `+sessionColumns+`
		FROM sessions WHERE id = ?
	`, id)

	return scanSession(row)
}

// GetActiveSession retrieves the active session for an agent and project.
// Returns ErrSessionNotFound if no active session exists.
func (db *DB) GetActiveSession(agentName, projectPath string) (*Session, error) {
	row := db.QueryRow(`
		SELECT `+sessionColumns+`
		FROM sessions
		WHERE agent_name = ? AND project_path = ? AND ended_at IS NULL
	`, agentName, projectPath)

	return scanSession(row)
}

// ListActiveSessions returns all active sessions for a project.
func (db *DB) ListActiveSessions(projectPath string) ([]*Session, error) {
	rows, err := db.Query(`
		SELECT `+sessionColumns+`
		FROM sessions
		WHERE project_path = ? AND ended_at IS NULL
		ORDER BY last_active_at DESC
	`, projectPath)
	if err != nil {
		return nil, fmt.Errorf("querying active sessions: %w", err)
	}
	defer rows.Close()

	return scanSessions(rows)
}

// ListAllActiveSessions returns all active sessions across all projects.
func (db *DB) ListAllActiveSessions() ([]*Session, error) {
	rows, err := db.Query(`
		SELECT ` + sessionColumns + `
		FROM sessions
		WHERE ended_at IS NULL
		ORDER BY last_active_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("querying all active sessions: %w", err)
	}
	defer rows.Close()

	return scanSessions(rows)
}

// UpdateSessionHeartbeat updates the last_active_at timestamp for a session.
func (db *DB) UpdateSessionHeartbeat(id string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := db.Exec(`
		UPDATE sessions SET last_active_at = ? WHERE id = ? AND ended_at IS NULL
	`, now, id)
	if err != nil {
		return fmt.Errorf("updating session heartbeat: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("getting rows affected: %w", err)
	}
	if rowsAffected == 0 {
		return ErrSessionNotFound
	}

	return nil
}

// EndSession marks a session as ended by setting ended_at.
func (db *DB) EndSession(id string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := db.Exec(`
		UPDATE sessions SET ended_at = ? WHERE id = ? AND ended_at IS NULL
	`, now, id)
	if err != nil {
		return fmt.Errorf("ending session: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("getting rows affected: %w", err)
	}
	if rowsAffected == 0 {
		return ErrSessionNotFound
	}

	return nil
}

// GetSessionRateLimitResetAt returns the stored per-minute rate limit reset timestamp (if any)
// for an active session.
func (db *DB) GetSessionRateLimitResetAt(id string) (*time.Time, error) {
	var resetAt sql.NullString
	err := db.QueryRow(`
		SELECT rate_limit_reset_at
		FROM sessions
		WHERE id = ? AND ended_at IS NULL
	`, id).Scan(&resetAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSessionNotFound
		}
		return nil, fmt.Errorf("reading session rate_limit_reset_at: %w", err)
	}
	if !resetAt.Valid || resetAt.String == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, resetAt.String)
	if err != nil {
		return nil, fmt.Errorf("parsing session rate_limit_reset_at: %w", err)
	}
	t = t.UTC()
	return &t, nil
}

// ResetSessionRateLimits records a reset timestamp used to ignore requests created before this time
// when enforcing per-minute limits.
func (db *DB) ResetSessionRateLimits(id string, now time.Time) (time.Time, error) {
	now = now.UTC()

	result, err := db.Exec(`
		UPDATE sessions
		SET rate_limit_reset_at = ?
		WHERE id = ? AND ended_at IS NULL
	`, now.Format(time.RFC3339), id)
	if err != nil {
		return time.Time{}, fmt.Errorf("resetting session rate limits: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return time.Time{}, fmt.Errorf("getting rows affected: %w", err)
	}
	if rowsAffected == 0 {
		return time.Time{}, ErrSessionNotFound
	}

	return now, nil
}

// FindStaleSessions returns active sessions that haven't been active within the threshold.
func (db *DB) FindStaleSessions(threshold time.Duration) ([]*Session, error) {
	cutoff := time.Now().UTC().Add(-threshold).Format(time.RFC3339)
	rows, err := db.Query(`
		SELECT `+sessionColumns+`
		FROM sessions
		WHERE ended_at IS NULL AND last_active_at < ?
		ORDER BY last_active_at ASC
	`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("finding stale sessions: %w", err)
	}
	defer rows.Close()

	return scanSessions(rows)
}

// ListActiveSessionsWithDifferentModel returns active sessions in a project
// that have a different model than the specified one.
func (db *DB) ListActiveSessionsWithDifferentModel(projectPath, excludeModel string) ([]*Session, error) {
	rows, err := db.Query(`
		SELECT `+sessionColumns+`
		FROM sessions
		WHERE project_path = ? AND ended_at IS NULL AND model != ?
		ORDER BY last_active_at DESC
	`, projectPath, excludeModel)
	if err != nil {
		return nil, fmt.Errorf("querying sessions with different model: %w", err)
	}
	defer rows.Close()

	return scanSessions(rows)
}

// HasActiveSessionWithDifferentModel checks if there's any active session
// in the project with a different model.
func (db *DB) HasActiveSessionWithDifferentModel(projectPath, excludeModel string) (bool, error) {
	var count int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM sessions
		WHERE project_path = ? AND ended_at IS NULL AND model != ?
	`, projectPath, excludeModel).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("checking different model sessions: %w", err)
	}
	return count > 0, nil
}

// DifferentModelStatus provides information about available different-model reviewers.
type DifferentModelStatus struct {
	// HasDifferentModel indicates if any active session has a different model.
	HasDifferentModel bool `json:"has_different_model"`
	// AvailableModels lists the distinct models of active sessions.
	AvailableModels []string `json:"available_models"`
	// SameModelSessions lists sessions with the same model as the requestor.
	SameModelSessions []*Session `json:"-"`
	// DifferentModelSessions lists sessions with different models.
	DifferentModelSessions []*Session `json:"-"`
}

// GetDifferentModelStatus returns information about available different-model reviewers.
func (db *DB) GetDifferentModelStatus(projectPath, requestorModel string) (*DifferentModelStatus, error) {
	sessions, err := db.ListActiveSessions(projectPath)
	if err != nil {
		return nil, fmt.Errorf("listing active sessions: %w", err)
	}

	status := &DifferentModelStatus{
		AvailableModels: []string{},
	}
	modelSet := make(map[string]bool)

	for _, s := range sessions {
		if !modelSet[s.Model] {
			modelSet[s.Model] = true
			status.AvailableModels = append(status.AvailableModels, s.Model)
		}

		if s.Model == requestorModel {
			status.SameModelSessions = append(status.SameModelSessions, s)
		} else {
			status.DifferentModelSessions = append(status.DifferentModelSessions, s)
			status.HasDifferentModel = true
		}
	}

	return status, nil
}

// scanSession scans a single session row.
func scanSession(row *sql.Row) (*Session, error) {
	s := &Session{}
	var startedAt, lastActiveAt string
	var endedAt sql.NullString

	err := row.Scan(&s.ID, &s.AgentName, &s.Program, &s.Model, &s.ProjectPath, &startedAt, &lastActiveAt, &endedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSessionNotFound
		}
		return nil, fmt.Errorf("scanning session: %w", err)
	}
	if err := parseSessionTimes(s, startedAt, lastActiveAt, endedAt); err != nil {
		return nil, err
	}
	return s, nil
}

// parseSessionTimes parses the stored timestamps of a scanned session.
func parseSessionTimes(s *Session, startedAt, lastActiveAt string, endedAt sql.NullString) error {
	var err error
	s.StartedAt, err = time.Parse(time.RFC3339, startedAt)
	if err != nil {
		return fmt.Errorf("parsing started_at: %w", err)
	}

	s.LastActiveAt, err = time.Parse(time.RFC3339, lastActiveAt)
	if err != nil {
		return fmt.Errorf("parsing last_active_at: %w", err)
	}

	if endedAt.Valid {
		t, err := time.Parse(time.RFC3339, endedAt.String)
		if err != nil {
			return fmt.Errorf("parsing ended_at: %w", err)
		}
		s.EndedAt = &t
	}
	return nil
}

// scanSessions scans multiple session rows.
func scanSessions(rows *sql.Rows) ([]*Session, error) {
	var sessions []*Session
	for rows.Next() {
		s := &Session{}
		var startedAt, lastActiveAt string
		var endedAt sql.NullString

		err := rows.Scan(&s.ID, &s.AgentName, &s.Program, &s.Model, &s.ProjectPath, &startedAt, &lastActiveAt, &endedAt)
		if err != nil {
			return nil, fmt.Errorf("scanning session row: %w", err)
		}
		if err := parseSessionTimes(s, startedAt, lastActiveAt, endedAt); err != nil {
			return nil, err
		}
		sessions = append(sessions, s)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating sessions: %w", err)
	}

	return sessions, nil
}

// isUniqueConstraintError checks if the error is a unique constraint violation.
// Note: We explicitly exclude FOREIGN KEY errors which also contain "constraint failed".
func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	// Exclude FOREIGN KEY errors - they also contain "constraint failed" but are not unique violations
	if containsIgnoreCase(errStr, "FOREIGN KEY") {
		return false
	}
	// modernc.org/sqlite returns errors containing this message
	return containsIgnoreCase(errStr, "UNIQUE constraint failed") ||
		containsIgnoreCase(errStr, "constraint failed")
}

// containsIgnoreCase checks if s contains substr (case-insensitive).
func containsIgnoreCase(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || findIgnoreCase(s, substr))
}

func findIgnoreCase(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if equalIgnoreCase(s[i:i+len(substr)], substr) {
			return true
		}
	}
	return false
}

func equalIgnoreCase(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
