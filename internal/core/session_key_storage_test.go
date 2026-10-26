package core

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

// Issue #30: reading another agent's session row, or the database files,
// must not yield anything slb accepts as that agent's key. This uses only the
// public paths an agent has: session resume, review submission and the
// authenticated execution claim.
func TestStoredSessionRowIsNotACredential(t *testing.T) {
	project := t.TempDir()
	path := filepath.Join(project, ".slb", "state.db")
	database, err := db.OpenAndMigrate(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	victim := &db.Session{AgentName: "Victim", Program: "victim-cli", Model: "victim-model", ProjectPath: project}
	requester := &db.Session{AgentName: "Requester", Program: "requester-cli", Model: "requester-model", ProjectPath: project}
	for _, session := range []*db.Session{victim, requester} {
		if err := database.CreateSession(session); err != nil {
			t.Fatal(err)
		}
	}
	request := &db.Request{ProjectPath: project, RequestorSessionID: requester.ID,
		RequestorAgent: requester.AgentName, RequestorModel: requester.Model,
		Command:  db.CommandSpec{Raw: "echo credential-probe", Argv: []string{"echo", "credential-probe"}, Cwd: project},
		RiskTier: db.RiskTierCaution, Status: db.StatusPending, MinApprovals: 0,
		Justification: db.Justification{Reason: "credential storage test"}}
	if err := database.CreateRequest(request); err != nil {
		t.Fatal(err)
	}
	// A zero-review CAUTION request the victim could claim for execution.
	claimable := &db.Request{ProjectPath: project, RequestorSessionID: victim.ID,
		RequestorAgent: victim.AgentName, RequestorModel: victim.Model,
		Command:  db.CommandSpec{Raw: "echo claim-probe", Argv: []string{"echo", "claim-probe"}, Cwd: project},
		RiskTier: db.RiskTierCaution, Status: db.StatusApproved, MinApprovals: 0,
		Justification: db.Justification{Reason: "credential storage test"}}
	if err := database.CreateRequest(claimable); err != nil {
		t.Fatal(err)
	}

	// Everything an attacker can read from the victim's row.
	rows, err := database.Query(`SELECT * FROM sessions WHERE id = ?`, victim.ID)
	if err != nil {
		t.Fatal(err)
	}
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var stored []string
	for rows.Next() {
		values := make([]sql.NullString, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		for i, value := range values {
			if strings.EqualFold(value.String, victim.SessionKey) {
				t.Errorf("sessions.%s stores the session key in plaintext", columns[i])
			}
			stored = append(stored, value.String)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}

	reviews := NewReviewService(database, DefaultReviewConfig())
	for _, candidate := range stored {
		if _, err := ResumeSession(database, ResumeOptions{AgentName: victim.AgentName, ProjectPath: project,
			SessionID: victim.ID, SessionKey: candidate, Program: victim.Program}); err == nil {
			t.Errorf("session resume accepted the stored value %q as the key", candidate)
		}
		if _, err := reviews.SubmitReview(ReviewOptions{SessionID: victim.ID, SessionKey: candidate,
			RequestID: request.ID, Decision: db.DecisionReject}); err == nil {
			t.Errorf("review accepted the stored value %q as the key", candidate)
		}
		now := time.Now().UTC()
		execution := &db.Execution{ExecutedAt: &now, ExecutedBySessionID: victim.ID, ExecutedByAgent: victim.AgentName,
			ExecutedByModel: victim.Model, LogPath: filepath.Join(project, "claim.log")}
		if err := database.ClaimRequestExecutionAuthenticated(claimable, execution, candidate); err == nil {
			t.Fatalf("execution claim accepted the stored value %q as the key", candidate)
		}
	}
	stillPending, err := database.GetRequest(request.ID)
	if err != nil || stillPending.Status != db.StatusPending {
		t.Fatalf("a rejected credential changed the request: %+v %v", stillPending, err)
	}

	// The key the agent was given still works and is handed back unchanged.
	resumed, err := ResumeSession(database, ResumeOptions{AgentName: victim.AgentName, ProjectPath: project,
		SessionID: victim.ID, SessionKey: victim.SessionKey, Program: victim.Program})
	if err != nil || resumed.ID != victim.ID || resumed.SessionKey != victim.SessionKey {
		t.Fatalf("resume with the real key = %+v, %v", resumed, err)
	}

	// Nor is the key anywhere in the database files.
	if _, err := database.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{path, path + "-wal"} {
		data, err := os.ReadFile(name)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if bytes.Contains(bytes.ToLower(data), []byte(strings.ToLower(victim.SessionKey))) {
			t.Errorf("%s contains the session key", filepath.Base(name))
		}
	}
}
