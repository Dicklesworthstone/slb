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
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ErrActiveSessionExists is returned when creating a session that would duplicate
// an active session for the same agent+project combination.
var ErrActiveSessionExists = errors.New("active session already exists for this agent and project")

// ErrSessionNotFound is returned when a session is not found.
var ErrSessionNotFound = errors.New("session not found")

// ErrSessionChanged means a recovery operation no longer owns its original
// active session snapshot. It must not return a replacement session's key.
var ErrSessionChanged = errors.New("session changed or is no longer active")

// ErrSessionHasLiveRequests prevents identity replacement from stranding
// pending timers or native Git approvals bound to the original requester.
var ErrSessionHasLiveRequests = errors.New("session has outstanding requests")

// sessionColumns are the session fields every read returns. The key's digest
// is read only by the queries that authenticate a supplied key.
const sessionColumns = "id, agent_name, program, model, project_path, started_at, last_active_at, ended_at"

// CreateSession creates a new session in the database.
// Generates a UUID and a random session key. Only the key's digest is stored;
// s.SessionKey is the one copy of the key, for the caller to hand to the agent.
// Returns ErrActiveSessionExists if an active session already exists for the agent+project.
func (db *DB) CreateSession(s *Session) error {
	digest, err := prepareSession(s)
	if err != nil {
		return err
	}
	return insertSession(db, s, digest)
}

func prepareSession(s *Session) (string, error) {
	if s == nil {
		return "", fmt.Errorf("session is required")
	}
	if s.AgentName == "" {
		return "", fmt.Errorf("agent_name is required")
	}
	if s.ProjectPath == "" {
		return "", fmt.Errorf("project_path is required")
	}

	// Generate UUID if not set
	if s.ID == "" {
		s.ID = uuid.New().String()
	}

	// Generate session key (32 bytes = 256 bits for HMAC-SHA256)
	if s.SessionKey == "" {
		key := make([]byte, sessionKeyBytes)
		if _, err := rand.Read(key); err != nil {
			return "", fmt.Errorf("generating session key: %w", err)
		}
		s.SessionKey = hex.EncodeToString(key)
	}
	digest, ok := SessionKeyDigest(s.SessionKey)
	if !ok {
		return "", ErrMalformedSessionKey
	}

	// Set timestamps
	now := time.Now().UTC()
	s.StartedAt = now
	s.LastActiveAt = now
	s.EndedAt = nil
	return digest, nil
}

func insertSession(writer interface {
	Exec(string, ...any) (sql.Result, error)
}, s *Session, digest string) error {
	// Insert into database. session_key stays empty: older binaries read it
	// as the key, and an empty key never authenticates there.
	_, err := writer.Exec(`
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

// ReplaceSession atomically ends an authenticated active identity and creates
// its replacement. The original identity and key digest remain unchanged for
// historical review verification and completion of already-claimed work.
// program and model are the complete replacement values; callers preserve any
// omitted options before invoking this method. No key is returned on failure.
func (db *DB) ReplaceSession(expected *Session, sessionKey, program, model string) (*Session, error) {
	if expected == nil || expected.ID == "" {
		return nil, fmt.Errorf("original session snapshot is required")
	}
	replacement := &Session{
		AgentName: expected.AgentName, ProjectPath: expected.ProjectPath,
		Program: program, Model: model,
	}
	digest, err := prepareSession(replacement)
	if err != nil {
		return nil, err
	}
	err = db.Transaction(func(tx *sql.Tx) error {
		// DB write transactions reserve the writer before this read. Session
		// admission, review and execution therefore cannot appear between the
		// live-work check and replacement, and concurrent replacements queue.
		current, currentDigest, err := scanSessionWithDigest(tx.QueryRow(`SELECT `+sessionColumns+`, session_key_digest
			FROM sessions WHERE id = ?`, expected.ID))
		if err != nil {
			return err
		}
		if !SessionKeyMatches(currentDigest, sessionKey) {
			return ErrSessionKeyMismatch
		}
		if !current.IsActive() || current.AgentName != expected.AgentName || current.ProjectPath != expected.ProjectPath ||
			current.Program != expected.Program || current.Model != expected.Model {
			return ErrSessionChanged
		}
		// Requester identity is part of signed evidence and cannot be rebound.
		// Pending automatic approval and native Git tokens need that original
		// session. An already-started execution can still finish with its old
		// key, so executing and terminal requests do not prevent replacement.
		var requestID, status string
		err = tx.QueryRow(`SELECT id, status FROM requests
			WHERE requestor_session_id = ? AND status IN ('pending', 'approved', 'escalated')
			ORDER BY created_at, id LIMIT 1`, current.ID).Scan(&requestID, &status)
		if err == nil {
			return fmt.Errorf("%w: request %s is %s; resolve it before changing program or model; the original session is unchanged",
				ErrSessionHasLiveRequests, requestID, status)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("checking session requests: %w", err)
		}
		// Timestamp the actual replacement after waiting for the writer, not
		// the earlier credential generation or an intervening heartbeat.
		replacement.StartedAt = time.Now().UTC()
		replacement.LastActiveAt = replacement.StartedAt
		result, err := tx.Exec(`UPDATE sessions SET ended_at = ?
			WHERE id = ? AND session_key_digest = ? AND ended_at IS NULL`,
			replacement.StartedAt.UTC().Format(time.RFC3339), current.ID, currentDigest)
		if err != nil {
			return fmt.Errorf("ending replaced session: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("checking session replacement: %w", err)
		}
		if count != 1 {
			return ErrSessionChanged
		}
		return insertSession(tx, replacement, digest)
	})
	if err != nil {
		return nil, err
	}
	return replacement, nil
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

// EndStaleSession ends only a still-stale session in the selected project.
// A heartbeat or resume after candidate selection wins over cleanup. False
// reports a refreshed, missing, already-ended or different-project session.
func (db *DB) EndStaleSession(id, project string, cutoff time.Time) (bool, error) {
	if id == "" || project == "" || cutoff.IsZero() {
		return false, fmt.Errorf("session ID, project and stale cutoff are required")
	}
	// Heartbeat and resume persist whole seconds. Round the cutoff down so a
	// refresh within its fractional second cannot still appear stale.
	cutoff = cutoff.UTC().Truncate(time.Second)
	result, err := db.Exec(`UPDATE sessions SET ended_at = ?
		WHERE id = ? AND project_path = ? AND ended_at IS NULL
			AND julianday(last_active_at) < julianday(?)`,
		time.Now().UTC().Format(time.RFC3339), id, project, cutoff.Format(time.RFC3339))
	if err != nil {
		return false, fmt.Errorf("ending stale session: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("checking stale session cleanup: %w", err)
	}
	return count == 1, nil
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

// FindStaleProjectSessions uses the same fixed cutoff as conditional cleanup.
func (db *DB) FindStaleProjectSessions(project string, cutoff time.Time) ([]*Session, error) {
	if project == "" || cutoff.IsZero() {
		return nil, fmt.Errorf("project and stale cutoff are required")
	}
	// Match conditional cleanup's conservative whole-second boundary.
	cutoff = cutoff.UTC().Truncate(time.Second)
	rows, err := db.Query(`SELECT `+sessionColumns+` FROM sessions
		WHERE project_path = ? AND ended_at IS NULL
			AND julianday(last_active_at) < julianday(?)
		ORDER BY last_active_at, id`, project, cutoff.Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("finding stale project sessions: %w", err)
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

// isUniqueConstraintError recognizes only SQLite's specific uniqueness codes.
// Other constraints, including RAISE(ABORT) triggers, are storage failures whose
// original error must survive rather than masquerading as an existing session
// or review. errors.As also handles callers that wrap the driver error.
func isUniqueConstraintError(err error) bool {
	var sqliteError *sqlite.Error
	if !errors.As(err, &sqliteError) {
		return false
	}
	switch sqliteError.Code() {
	case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY, sqlite3.SQLITE_CONSTRAINT_ROWID:
		return true
	default:
		return false
	}
}
