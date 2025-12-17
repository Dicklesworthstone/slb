package core

import (
	"errors"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestResumeSession_CreateIfMissingFalse(t *testing.T) {
	dbConn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open(:memory:) error = %v", err)
	}
	defer dbConn.Close()

	_, err = ResumeSession(dbConn, ResumeOptions{
		AgentName:       "BlueSnow",
		Program:         "codex-cli",
		Model:           "gpt-5.2",
		ProjectPath:     "/test/project",
		CreateIfMissing: false,
	})
	if !errors.Is(err, db.ErrSessionNotFound) {
		t.Fatalf("expected db.ErrSessionNotFound, got %v", err)
	}
}

func TestResumeSession_CreatesNewSession(t *testing.T) {
	dbConn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open(:memory:) error = %v", err)
	}
	defer dbConn.Close()

	sess, err := ResumeSession(dbConn, ResumeOptions{
		AgentName:       "BlueSnow",
		Program:         "codex-cli",
		Model:           "gpt-5.2",
		ProjectPath:     "/test/project",
		CreateIfMissing: true,
	})
	if err != nil {
		t.Fatalf("ResumeSession() error = %v", err)
	}
	if sess.ID == "" || sess.SessionKey == "" {
		t.Fatalf("expected session to have id and session_key")
	}
}

func TestResumeSession_ProgramMismatch(t *testing.T) {
	dbConn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open(:memory:) error = %v", err)
	}
	defer dbConn.Close()

	// Create an existing session.
	existing := &db.Session{
		AgentName:   "BlueSnow",
		Program:     "claude-code",
		Model:       "opus-4.5",
		ProjectPath: "/test/project",
	}
	if err := dbConn.CreateSession(existing); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	_, err = ResumeSession(dbConn, ResumeOptions{
		AgentName:       "BlueSnow",
		Program:         "codex-cli",
		Model:           "gpt-5.2",
		ProjectPath:     "/test/project",
		CreateIfMissing: true,
	})
	if !errors.Is(err, ErrSessionProgramMismatch) {
		t.Fatalf("expected ErrSessionProgramMismatch, got %v", err)
	}
}

func TestResumeSession_UpdatesHeartbeat(t *testing.T) {
	dbConn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open(:memory:) error = %v", err)
	}
	defer dbConn.Close()

	existing := &db.Session{
		AgentName:   "BlueSnow",
		Program:     "codex-cli",
		Model:       "gpt-5.2",
		ProjectPath: "/test/project",
	}
	if err := dbConn.CreateSession(existing); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	old := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	if _, err := dbConn.Exec(`UPDATE sessions SET last_active_at = ? WHERE id = ?`, old, existing.ID); err != nil {
		t.Fatalf("failed to backdate session: %v", err)
	}

	sess, err := ResumeSession(dbConn, ResumeOptions{
		AgentName:       "BlueSnow",
		Program:         "codex-cli",
		Model:           "gpt-5.2",
		ProjectPath:     "/test/project",
		CreateIfMissing: true,
	})
	if err != nil {
		t.Fatalf("ResumeSession() error = %v", err)
	}

	if !sess.LastActiveAt.After(time.Now().UTC().Add(-5 * time.Minute)) {
		t.Fatalf("expected LastActiveAt to be recent, got %s", sess.LastActiveAt.Format(time.RFC3339))
	}
}

func TestGarbageCollectStaleSessions_DryRun(t *testing.T) {
	dbConn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open(:memory:) error = %v", err)
	}
	defer dbConn.Close()

	projectA := "/test/project-a"
	projectB := "/test/project-b"

	staleA := &db.Session{AgentName: "GreenLake", Program: "codex-cli", Model: "gpt-5.2", ProjectPath: projectA}
	freshA := &db.Session{AgentName: "BlueDog", Program: "codex-cli", Model: "gpt-5.2", ProjectPath: projectA}
	staleB := &db.Session{AgentName: "RedCat", Program: "codex-cli", Model: "gpt-5.2", ProjectPath: projectB}
	for _, s := range []*db.Session{staleA, freshA, staleB} {
		if err := dbConn.CreateSession(s); err != nil {
			t.Fatalf("CreateSession() error = %v", err)
		}
	}

	old := time.Now().UTC().Add(-10 * time.Hour).Format(time.RFC3339)
	if _, err := dbConn.Exec(`UPDATE sessions SET last_active_at = ? WHERE id = ?`, old, staleA.ID); err != nil {
		t.Fatalf("failed to backdate staleA: %v", err)
	}
	if _, err := dbConn.Exec(`UPDATE sessions SET last_active_at = ? WHERE id = ?`, old, staleB.ID); err != nil {
		t.Fatalf("failed to backdate staleB: %v", err)
	}

	res, err := GarbageCollectStaleSessions(dbConn, SessionGCOptions{
		ProjectPath: projectA,
		Threshold:   30 * time.Minute,
		DryRun:      true,
	})
	if err != nil {
		t.Fatalf("GarbageCollectStaleSessions() error = %v", err)
	}
	if len(res.Sessions) != 1 {
		t.Fatalf("expected 1 stale session for project A, got %d", len(res.Sessions))
	}
	if res.Sessions[0].ID != staleA.ID {
		t.Fatalf("expected staleA id %s, got %s", staleA.ID, res.Sessions[0].ID)
	}
	if len(res.EndedIDs) != 0 || len(res.SkippedIDs) != 0 {
		t.Fatalf("expected no ended/skipped in dry-run, got ended=%d skipped=%d", len(res.EndedIDs), len(res.SkippedIDs))
	}

	// Ensure stale session was not ended in dry-run.
	got, err := dbConn.GetSession(staleA.ID)
	if err != nil {
		t.Fatalf("GetSession(staleA) error = %v", err)
	}
	if got.EndedAt != nil {
		t.Fatalf("expected staleA to remain active in dry-run")
	}
}

func TestGarbageCollectStaleSessions_EndsOnlyProjectSessions(t *testing.T) {
	dbConn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open(:memory:) error = %v", err)
	}
	defer dbConn.Close()

	projectA := "/test/project-a"
	projectB := "/test/project-b"

	staleA := &db.Session{AgentName: "GreenLake", Program: "codex-cli", Model: "gpt-5.2", ProjectPath: projectA}
	freshA := &db.Session{AgentName: "BlueDog", Program: "codex-cli", Model: "gpt-5.2", ProjectPath: projectA}
	staleB := &db.Session{AgentName: "RedCat", Program: "codex-cli", Model: "gpt-5.2", ProjectPath: projectB}
	for _, s := range []*db.Session{staleA, freshA, staleB} {
		if err := dbConn.CreateSession(s); err != nil {
			t.Fatalf("CreateSession() error = %v", err)
		}
	}

	old := time.Now().UTC().Add(-10 * time.Hour).Format(time.RFC3339)
	if _, err := dbConn.Exec(`UPDATE sessions SET last_active_at = ? WHERE id = ?`, old, staleA.ID); err != nil {
		t.Fatalf("failed to backdate staleA: %v", err)
	}
	if _, err := dbConn.Exec(`UPDATE sessions SET last_active_at = ? WHERE id = ?`, old, staleB.ID); err != nil {
		t.Fatalf("failed to backdate staleB: %v", err)
	}

	res, err := GarbageCollectStaleSessions(dbConn, SessionGCOptions{
		ProjectPath: projectA,
		Threshold:   30 * time.Minute,
		DryRun:      false,
	})
	if err != nil {
		t.Fatalf("GarbageCollectStaleSessions() error = %v", err)
	}
	if len(res.Sessions) != 1 || len(res.EndedIDs) != 1 {
		t.Fatalf("expected 1 stale and 1 ended for project A, got stale=%d ended=%d", len(res.Sessions), len(res.EndedIDs))
	}
	if res.EndedIDs[0] != staleA.ID {
		t.Fatalf("expected ended id %s, got %s", staleA.ID, res.EndedIDs[0])
	}

	ended, err := dbConn.GetSession(staleA.ID)
	if err != nil {
		t.Fatalf("GetSession(staleA) error = %v", err)
	}
	if ended.EndedAt == nil {
		t.Fatalf("expected staleA to be ended")
	}

	stillActive, err := dbConn.GetSession(freshA.ID)
	if err != nil {
		t.Fatalf("GetSession(freshA) error = %v", err)
	}
	if stillActive.EndedAt != nil {
		t.Fatalf("expected freshA to remain active")
	}

	otherProject, err := dbConn.GetSession(staleB.ID)
	if err != nil {
		t.Fatalf("GetSession(staleB) error = %v", err)
	}
	if otherProject.EndedAt != nil {
		t.Fatalf("expected staleB (other project) to remain active")
	}
}

func TestResumeSession_ValidationErrors(t *testing.T) {
	dbConn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open(:memory:) error = %v", err)
	}
	defer dbConn.Close()

	t.Run("empty agent name returns error", func(t *testing.T) {
		_, err := ResumeSession(dbConn, ResumeOptions{
			AgentName:       "",
			Program:         "codex-cli",
			Model:           "gpt-5.2",
			ProjectPath:     "/test/project",
			CreateIfMissing: true,
		})
		if err == nil {
			t.Error("expected error for empty agent name")
		}
	})

	t.Run("empty project path returns error", func(t *testing.T) {
		_, err := ResumeSession(dbConn, ResumeOptions{
			AgentName:       "BlueSnow",
			Program:         "codex-cli",
			Model:           "gpt-5.2",
			ProjectPath:     "",
			CreateIfMissing: true,
		})
		if err == nil {
			t.Error("expected error for empty project path")
		}
	})
}

func TestResumeSession_ForceEndMismatch(t *testing.T) {
	dbConn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open(:memory:) error = %v", err)
	}
	defer dbConn.Close()

	// Create an existing session with a different program.
	existing := &db.Session{
		AgentName:   "BlueSnow",
		Program:     "claude-code",
		Model:       "opus-4.5",
		ProjectPath: "/test/project",
	}
	if err := dbConn.CreateSession(existing); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	// Resume with ForceEndMismatch=true should end old session and create new one.
	sess, err := ResumeSession(dbConn, ResumeOptions{
		AgentName:        "BlueSnow",
		Program:          "codex-cli",
		Model:            "gpt-5.2",
		ProjectPath:      "/test/project",
		CreateIfMissing:  true,
		ForceEndMismatch: true,
	})
	if err != nil {
		t.Fatalf("ResumeSession() error = %v", err)
	}

	// Verify old session was ended.
	oldSess, err := dbConn.GetSession(existing.ID)
	if err != nil {
		t.Fatalf("GetSession(existing) error = %v", err)
	}
	if oldSess.EndedAt == nil {
		t.Fatalf("expected old session to be ended")
	}

	// Verify new session has the requested program.
	if sess.Program != "codex-cli" {
		t.Fatalf("expected new session program to be codex-cli, got %s", sess.Program)
	}
	if sess.ID == existing.ID {
		t.Fatalf("expected new session to have different ID")
	}
}

func TestGarbageCollectStaleSessions_ValidationErrors(t *testing.T) {
	dbConn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open(:memory:) error = %v", err)
	}
	defer dbConn.Close()

	t.Run("nil dbConn returns error", func(t *testing.T) {
		_, err := GarbageCollectStaleSessions(nil, SessionGCOptions{
			ProjectPath: "/test/project",
			Threshold:   30 * time.Minute,
		})
		if err == nil {
			t.Error("expected error for nil dbConn")
		}
	})

	t.Run("empty project path returns error", func(t *testing.T) {
		_, err := GarbageCollectStaleSessions(dbConn, SessionGCOptions{
			ProjectPath: "",
			Threshold:   30 * time.Minute,
		})
		if err == nil {
			t.Error("expected error for empty project path")
		}
	})

	t.Run("zero threshold returns error", func(t *testing.T) {
		_, err := GarbageCollectStaleSessions(dbConn, SessionGCOptions{
			ProjectPath: "/test/project",
			Threshold:   0,
		})
		if err == nil {
			t.Error("expected error for zero threshold")
		}
	})

	t.Run("negative threshold returns error", func(t *testing.T) {
		_, err := GarbageCollectStaleSessions(dbConn, SessionGCOptions{
			ProjectPath: "/test/project",
			Threshold:   -1 * time.Minute,
		})
		if err == nil {
			t.Error("expected error for negative threshold")
		}
	})
}

func TestGarbageCollectStaleSessions_NoStaleSessions(t *testing.T) {
	dbConn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open(:memory:) error = %v", err)
	}
	defer dbConn.Close()

	// Create a fresh session.
	fresh := &db.Session{
		AgentName:   "BlueSnow",
		Program:     "codex-cli",
		Model:       "gpt-5.2",
		ProjectPath: "/test/project",
	}
	if err := dbConn.CreateSession(fresh); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	res, err := GarbageCollectStaleSessions(dbConn, SessionGCOptions{
		ProjectPath: "/test/project",
		Threshold:   30 * time.Minute,
		DryRun:      false,
	})
	if err != nil {
		t.Fatalf("GarbageCollectStaleSessions() error = %v", err)
	}
	if len(res.Sessions) != 0 {
		t.Fatalf("expected no stale sessions, got %d", len(res.Sessions))
	}
	if len(res.EndedIDs) != 0 {
		t.Fatalf("expected no ended sessions, got %d", len(res.EndedIDs))
	}
}
