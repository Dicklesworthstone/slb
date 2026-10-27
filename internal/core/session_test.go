package core

import (
	"errors"
	"path/filepath"
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
		SessionID:       existing.ID,
		SessionKey:      existing.SessionKey,
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
		SessionID:       existing.ID,
		SessionKey:      existing.SessionKey,
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

func TestResumeSessionRejectsHijackingBeforeMutation(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	victim := &db.Session{AgentName: "Victim", Program: "original", Model: "original", ProjectPath: "/project"}
	if err := database.CreateSession(victim); err != nil {
		t.Fatal(err)
	}
	for _, credentials := range []struct{ id, key string }{
		{"", ""}, {victim.ID, ""}, {victim.ID, "bad"}, {"another-session", victim.SessionKey},
	} {
		for _, force := range []bool{false, true} {
			got, err := ResumeSession(database, ResumeOptions{
				AgentName: victim.AgentName, ProjectPath: victim.ProjectPath,
				SessionID: credentials.id, SessionKey: credentials.key,
				Program: "attacker", Model: "forged-model", ForceEndMismatch: force, CreateIfMissing: true,
			})
			if !errors.Is(err, ErrSessionAuthentication) || got != nil {
				t.Fatalf("hijack returned session or wrong error: %v", err)
			}
			stored, err := database.GetSession(victim.ID)
			if err != nil || stored.EndedAt != nil || stored.Model != victim.Model ||
				stored.Program != victim.Program || !stored.LastActiveAt.Equal(victim.LastActiveAt.Truncate(time.Second)) {
				t.Fatalf("failed authentication mutated victim: %v", err)
			}
		}
	}
	got, err := ResumeSession(database, ResumeOptions{AgentName: victim.AgentName, ProjectPath: victim.ProjectPath,
		SessionID: victim.ID, SessionKey: victim.SessionKey})
	if err != nil || got.ID != victim.ID || got.SessionKey != victim.SessionKey {
		t.Fatalf("authenticated recovery failed: %v", err)
	}
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
		SessionID:        existing.ID,
		SessionKey:       existing.SessionKey,
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

func TestResumeSessionIdentityChangesRotate(t *testing.T) {
	for _, test := range []struct {
		name, storedProgram, program, model, wantProgram, wantModel string
		force, rotated                                              bool
	}{
		{name: "omitted metadata", storedProgram: "original-program", wantProgram: "original-program", wantModel: "original-model"},
		{name: "unchanged metadata", storedProgram: "original-program", program: "original-program", model: "original-model", wantProgram: "original-program", wantModel: "original-model"},
		{name: "model change", storedProgram: "original-program", model: "new-model", wantProgram: "original-program", wantModel: "new-model", rotated: true},
		{name: "forced program change", storedProgram: "original-program", program: "new-program", wantProgram: "new-program", wantModel: "original-model", force: true, rotated: true},
		{name: "fill missing program", program: "new-program", wantProgram: "new-program", wantModel: "original-model", rotated: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, err := db.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = database.Close() })
			original := &db.Session{AgentName: "resume-agent", ProjectPath: "/test/resume", Program: test.storedProgram, Model: "original-model"}
			if err := database.CreateSession(original); err != nil {
				t.Fatal(err)
			}
			oldHeartbeat := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
			if _, err := database.Exec(`UPDATE sessions SET last_active_at = ? WHERE id = ?`, oldHeartbeat.Format(time.RFC3339), original.ID); err != nil {
				t.Fatal(err)
			}
			resumed, err := ResumeSession(database, ResumeOptions{
				AgentName: original.AgentName, ProjectPath: original.ProjectPath,
				SessionID: original.ID, SessionKey: original.SessionKey, Program: test.program, Model: test.model,
				ForceEndMismatch: test.force, CreateIfMissing: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if resumed.Program != test.wantProgram || resumed.Model != test.wantModel ||
				resumed.AgentName != original.AgentName || resumed.ProjectPath != original.ProjectPath || !resumed.LastActiveAt.After(oldHeartbeat) {
				t.Fatal("resume lost requested or omitted identity metadata")
			}
			stored, err := database.GetSession(original.ID)
			if err != nil || stored.Program != original.Program || stored.Model != original.Model {
				t.Fatalf("resume changed historical identity: %v", err)
			}
			if test.rotated {
				if resumed.ID == original.ID || resumed.SessionKey == original.SessionKey || stored.EndedAt == nil || !stored.LastActiveAt.Equal(oldHeartbeat) {
					t.Fatal("identity change failed to rotate credentials while preserving history")
				}
			} else if resumed.ID != original.ID || resumed.SessionKey != original.SessionKey || stored.EndedAt != nil || !stored.LastActiveAt.After(oldHeartbeat) {
				t.Fatal("unchanged resume did not retain credentials and renew heartbeat")
			}
		})
	}
}

func TestResumeSessionOutstandingWorkBlocksOnlyIdentityChanges(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	original := &db.Session{AgentName: "busy-agent", ProjectPath: "/test/resume", Program: "original-program", Model: "original-model"}
	if err := database.CreateSession(original); err != nil {
		t.Fatal(err)
	}
	request := &db.Request{ProjectPath: original.ProjectPath, RequestorSessionID: original.ID,
		RequestorAgent: original.AgentName, RequestorModel: original.Model,
		Command: db.CommandSpec{Raw: "echo live-work", Cwd: original.ProjectPath}, RiskTier: db.RiskTierDangerous,
		Status: db.StatusPending, MinApprovals: 1, Justification: db.Justification{Reason: "live work"}}
	if err := database.CreateRequest(request); err != nil {
		t.Fatal(err)
	}
	base := ResumeOptions{AgentName: original.AgentName, ProjectPath: original.ProjectPath,
		SessionID: original.ID, SessionKey: original.SessionKey, CreateIfMissing: true}
	for _, changeProgram := range []bool{false, true} {
		opts := base
		if changeProgram {
			opts.Program, opts.ForceEndMismatch = "new-program", true
		} else {
			opts.Model = "new-model"
		}
		if resumed, err := ResumeSession(database, opts); resumed != nil || !errors.Is(err, db.ErrSessionHasLiveRequests) {
			t.Fatalf("identity change stranded requester work: %v", err)
		}
	}
	resumed, err := ResumeSession(database, base)
	if err != nil || resumed.ID != original.ID || resumed.SessionKey != original.SessionKey || resumed.Model != original.Model || resumed.Program != original.Program {
		t.Fatalf("unchanged busy session could not resume: %v", err)
	}
}

func TestResumeSessionConcurrentRotationHasOneWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.db")
	database, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	other, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	original := &db.Session{AgentName: "concurrent-agent", ProjectPath: "/test/resume", Program: "program", Model: "original-model"}
	if err := database.CreateSession(original); err != nil {
		t.Fatal(err)
	}
	opts := ResumeOptions{AgentName: original.AgentName, ProjectPath: original.ProjectPath,
		SessionID: original.ID, SessionKey: original.SessionKey, Model: "new-model", CreateIfMissing: true}
	type outcome struct {
		session *db.Session
		err     error
	}
	start, results := make(chan struct{}), make(chan outcome, 2)
	for _, connection := range []*db.DB{database, other} {
		go func(connection *db.DB) {
			<-start
			session, err := ResumeSession(connection, opts)
			results <- outcome{session, err}
		}(connection)
	}
	close(start)
	winners := 0
	for range 2 {
		got := <-results
		if got.err == nil {
			winners++
			if got.session == nil || got.session.ID == original.ID || got.session.SessionKey == original.SessionKey {
				t.Fatal("winner did not receive its replacement identity")
			}
		} else if got.session != nil || !errors.Is(got.err, ErrSessionAuthentication) {
			t.Fatalf("loser received another session or an unexpected error: %v", got.err)
		}
	}
	if winners != 1 {
		t.Fatalf("got %d successful replacements, want 1", winners)
	}
}

func TestGarbageCollectionSkipsSessionResumedAfterSelection(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	original := &db.Session{AgentName: "resumed-agent", ProjectPath: "/test/resume", Model: "model"}
	if err := database.CreateSession(original); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE sessions SET last_active_at = ? WHERE id = ?`, time.Now().UTC().Add(-2*time.Hour).Format(time.RFC3339), original.ID); err != nil {
		t.Fatal(err)
	}
	candidates, err := GarbageCollectStaleSessions(database, SessionGCOptions{ProjectPath: original.ProjectPath, Threshold: time.Hour + 250*time.Millisecond, DryRun: true})
	if err != nil || len(candidates.Sessions) != 1 {
		t.Fatalf("missing stale candidate: %v", err)
	}
	if candidates.Cutoff.Location() != time.UTC || candidates.Cutoff.Nanosecond() != 0 {
		t.Fatal("reported GC cutoff does not match the whole-second cleanup boundary")
	}
	if _, err := ResumeSession(database, ResumeOptions{AgentName: original.AgentName, ProjectPath: original.ProjectPath, SessionID: original.ID, SessionKey: original.SessionKey}); err != nil {
		t.Fatal(err)
	}
	if ended, err := database.EndStaleSession(original.ID, original.ProjectPath, candidates.Cutoff); err != nil || ended {
		t.Fatalf("conditional cleanup ended resumed session: %v", err)
	}
	stored, err := database.GetSession(original.ID)
	if err != nil || !stored.IsActive() {
		t.Fatalf("resumed session became unusable: %v", err)
	}
}
