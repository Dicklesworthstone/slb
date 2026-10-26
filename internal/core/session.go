package core

import (
	"errors"
	"fmt"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

// ErrSessionProgramMismatch indicates an active session exists, but belongs to a different program.
var ErrSessionProgramMismatch = errors.New("active session belongs to a different program")

// ErrSessionAuthentication never includes credentials or the recovered session.
var ErrSessionAuthentication = errors.New("session authentication failed: original session ID and key are required")

// SessionSummary is a safe-to-serialize view of a session (excludes session_key).
type SessionSummary struct {
	ID           string
	AgentName    string
	Program      string
	Model        string
	ProjectPath  string
	StartedAt    time.Time
	LastActiveAt time.Time
}

// SessionGCOptions configures stale session garbage collection.
type SessionGCOptions struct {
	ProjectPath string
	Threshold   time.Duration
	DryRun      bool
}

// SessionGCResult reports stale session garbage collection results.
type SessionGCResult struct {
	ProjectPath string
	Threshold   time.Duration
	Cutoff      time.Time
	Sessions    []SessionSummary
	EndedIDs    []string
	SkippedIDs  []string
}

// ResumeOptions configures session resume behavior.
type ResumeOptions struct {
	SessionID        string
	SessionKey       string
	AgentName        string
	Program          string
	Model            string
	ProjectPath      string
	CreateIfMissing  bool
	ForceEndMismatch bool
}

// ResumeSession resumes an existing active session (agent_name + project_path) or creates a new one.
//
// Behavior:
// - If an active session exists and Program is specified, it must match (unless ForceEndMismatch is true).
// - Existing sessions require proof of possession of their original ID and key before any mutation.
// - On successful resume, updates the session heartbeat (last_active_at) and returns the authenticated session.
// - If no active session exists:
//   - CreateIfMissing=true → creates a new session and returns it
//   - CreateIfMissing=false → returns db.ErrSessionNotFound
func ResumeSession(dbConn *db.DB, opts ResumeOptions) (*db.Session, error) {
	if opts.AgentName == "" {
		return nil, fmt.Errorf("agent_name is required")
	}
	if opts.ProjectPath == "" {
		return nil, fmt.Errorf("project_path is required")
	}

	sess, err := dbConn.GetActiveSession(opts.AgentName, opts.ProjectPath)
	if err != nil {
		if errors.Is(err, db.ErrSessionNotFound) {
			if !opts.CreateIfMissing {
				return nil, db.ErrSessionNotFound
			}

			newSess := &db.Session{
				AgentName:   opts.AgentName,
				Program:     opts.Program,
				Model:       opts.Model,
				ProjectPath: opts.ProjectPath,
			}
			if err := dbConn.CreateSession(newSess); err != nil {
				return nil, err
			}
			return newSess, nil
		}
		return nil, err
	}

	// Authenticate before any mutation, including --force and model changes.
	// Names and program/model labels are public metadata, not credentials.
	// The database holds only the key's digest, so nothing read here can be
	// handed back as a credential; the caller must already have the key.
	if opts.SessionID != sess.ID {
		return nil, ErrSessionAuthentication
	}
	authenticated, err := dbConn.AuthenticateSession(sess.ID, opts.SessionKey)
	if err != nil {
		if errors.Is(err, db.ErrSessionKeyMismatch) || errors.Is(err, db.ErrSessionNotFound) {
			return nil, ErrSessionAuthentication
		}
		return nil, err
	}
	sess = authenticated
	digest, _ := db.SessionKeyDigest(opts.SessionKey)

	if opts.Program != "" && sess.Program != "" && sess.Program != opts.Program {
		if !opts.ForceEndMismatch {
			return nil, fmt.Errorf("%w: active=%q requested=%q", ErrSessionProgramMismatch, sess.Program, opts.Program)
		}

		// Force: end the old session and create a new one.
		result, err := dbConn.Exec(`UPDATE sessions SET ended_at = ? WHERE id = ? AND session_key_digest = ? AND ended_at IS NULL`,
			time.Now().UTC().Format(time.RFC3339), sess.ID, digest)
		if err != nil {
			return nil, err
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			return nil, ErrSessionAuthentication
		}
		newSess := &db.Session{
			AgentName:   opts.AgentName,
			Program:     opts.Program,
			Model:       opts.Model,
			ProjectPath: opts.ProjectPath,
		}
		if err := dbConn.CreateSession(newSess); err != nil {
			return nil, err
		}
		return newSess, nil
	}

	if opts.Model != "" {
		sess.Model = opts.Model
	}
	// Fence mutations against credential rotation/session termination after
	// the initial read. Never reread and return a replacement credential.
	sess.LastActiveAt = time.Now().UTC().Truncate(time.Second)
	result, err := dbConn.Exec(`UPDATE sessions SET model = ?, last_active_at = ?
		WHERE id = ? AND session_key_digest = ? AND ended_at IS NULL`,
		sess.Model, sess.LastActiveAt.Format(time.RFC3339), sess.ID, digest)
	if err != nil {
		return nil, err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return nil, ErrSessionAuthentication
	}
	return sess, nil
}

// GarbageCollectStaleSessions finds stale sessions for a project and ends them unless DryRun is set.
func GarbageCollectStaleSessions(dbConn *db.DB, opts SessionGCOptions) (*SessionGCResult, error) {
	if dbConn == nil {
		return nil, fmt.Errorf("dbConn is required")
	}
	if opts.ProjectPath == "" {
		return nil, fmt.Errorf("project_path is required")
	}
	if opts.Threshold <= 0 {
		return nil, fmt.Errorf("threshold must be > 0")
	}

	now := time.Now().UTC()
	res := &SessionGCResult{
		ProjectPath: opts.ProjectPath,
		Threshold:   opts.Threshold,
		Cutoff:      now.Add(-opts.Threshold),
	}

	stale, err := dbConn.FindStaleSessions(opts.Threshold)
	if err != nil {
		return nil, err
	}

	for _, s := range stale {
		if s.ProjectPath != opts.ProjectPath {
			continue
		}
		res.Sessions = append(res.Sessions, SessionSummary{
			ID:           s.ID,
			AgentName:    s.AgentName,
			Program:      s.Program,
			Model:        s.Model,
			ProjectPath:  s.ProjectPath,
			StartedAt:    s.StartedAt,
			LastActiveAt: s.LastActiveAt,
		})
	}

	if opts.DryRun || len(res.Sessions) == 0 {
		return res, nil
	}

	for _, s := range res.Sessions {
		if err := dbConn.EndSession(s.ID); err != nil {
			if errors.Is(err, db.ErrSessionNotFound) {
				res.SkippedIDs = append(res.SkippedIDs, s.ID)
				continue
			}
			return nil, err
		}
		res.EndedIDs = append(res.EndedIDs, s.ID)
	}

	return res, nil
}
