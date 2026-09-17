package db

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAuthenticatedClaimRechecksKeyAfterWriterWait(t *testing.T) {
	database, request, execution, session := authenticatedClaimFixture(t)
	other, err := Open(database.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	started, outcome := make(chan struct{}), make(chan error, 1)
	err = database.Transaction(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE sessions SET session_key=? WHERE id=?`, strings.Repeat("19", 32), session.ID); err != nil {
			return err
		}
		go func() {
			close(started)
			outcome <- other.ClaimRequestExecutionAuthenticated(request, execution, session.SessionKey)
		}()
		<-started
		select {
		case err := <-outcome:
			return fmt.Errorf("claim completed while writer was reserved: %v", err)
		case <-time.After(25 * time.Millisecond):
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-outcome:
		if !errors.Is(err, ErrExecutionAuthentication) {
			t.Fatalf("stale key authorized after lock wait: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("claim did not finish after writer released")
	}
}

func authenticatedClaimFixture(t *testing.T) (*DB, *Request, *Execution, *Session) {
	t.Helper()
	project := t.TempDir()
	database, err := Open(filepath.Join(project, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	session := &Session{AgentName: "executor", Model: "one", Program: "test", ProjectPath: project}
	if err := database.CreateSession(session); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	expires := now.Add(time.Minute)
	request := &Request{
		ProjectPath: project, RequestorSessionID: session.ID,
		RequestorAgent: session.AgentName, RequestorModel: session.Model,
		Command: CommandSpec{Raw: "echo fixture", Argv: []string{"echo", "fixture"}, Cwd: project},
		Status:  StatusApproved, RiskTier: RiskTierCaution, ApprovalExpiresAt: &expires,
		Justification: Justification{Reason: "authenticated claim fixture"},
	}
	if err := database.CreateRequest(request); err != nil {
		t.Fatal(err)
	}
	return database, request, &Execution{
		ExecutedAt: &now, ExecutedBySessionID: session.ID,
		ExecutedByAgent: session.AgentName, ExecutedByModel: session.Model,
		LogPath: filepath.Join(project, "claim.log"),
	}, session
}

func TestExecutionSessionKeyMatches(t *testing.T) {
	key := strings.Repeat("ab", 32)
	for _, test := range []struct {
		stored, supplied string
		want             bool
	}{
		{key, key, true}, {key, strings.ToUpper(key), true}, {key, "", false},
		{key, strings.Repeat("cd", 32), false}, {"broken", "broken", false},
		{"", "", false}, {strings.Repeat("ab", 16), strings.Repeat("ab", 16), false},
	} {
		if got := ExecutionSessionKeyMatches(test.stored, test.supplied); got != test.want {
			t.Errorf("key comparison = %v, want %v", got, test.want)
		}
	}
}

func TestAuthenticatedClaimRejectsIdentityChanges(t *testing.T) {
	for _, kind := range []string{"wrong key", "empty key", "rotated key", "ended", "other project", "unknown", "missing evidence"} {
		t.Run(kind, func(t *testing.T) {
			database, request, execution, session := authenticatedClaimFixture(t)
			key := session.SessionKey
			var query string
			switch kind {
			case "wrong key":
				key = strings.Repeat("00", 32)
			case "empty key":
				key = ""
			case "rotated key":
				query = "UPDATE sessions SET session_key='" + strings.Repeat("01", 32) + "'"
			case "ended":
				query = "UPDATE sessions SET ended_at='2020-01-01T00:00:00Z'"
			case "other project":
				query = "UPDATE sessions SET project_path='/not-the-project'"
			case "unknown":
				execution.ExecutedBySessionID = "not-a-session"
			case "missing evidence":
				query = "UPDATE requests SET risk_tier='dangerous', min_approvals=1"
				request.RiskTier = RiskTierDangerous
				request.MinApprovals = 1
			}
			if query != "" {
				if _, err := database.Exec(query); err != nil {
					t.Fatal(err)
				}
			}
			err := database.ClaimRequestExecutionAuthenticated(request, execution, key)
			if kind == "missing evidence" {
				if !errors.Is(err, ErrInvalidApprovalProof) {
					t.Fatalf("unsupported status authorized: %v", err)
				}
			} else if !errors.Is(err, ErrExecutionAuthentication) {
				t.Fatalf("identity change authorized: %v", err)
			}
			stored, err := database.GetRequest(request.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Status != StatusApproved || stored.Execution != nil {
				t.Fatal("denied claim wrote execution metadata")
			}
		})
	}
}

func TestAuthenticatedClaimIndependentConnectionsSingleWinner(t *testing.T) {
	database, request, execution, session := authenticatedClaimFixture(t)
	other, err := Open(database.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	connections := []*DB{database, other}
	start := make(chan struct{})
	out := make(chan error, 16)
	var workers sync.WaitGroup
	for i := 0; i < 16; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			candidate := *execution
			candidate.LogPath = fmt.Sprintf("%s-%d", execution.LogPath, i)
			<-start
			out <- connections[i%2].ClaimRequestExecutionAuthenticated(request, &candidate, session.SessionKey)
		}(i)
	}
	close(start)
	workers.Wait()
	close(out)
	wins := 0
	for err := range out {
		if err == nil {
			wins++
		} else if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("unexpected claim error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("got %d claim winners", wins)
	}
}

func TestAuthenticatedCompletionFencesAndPreservesIdentity(t *testing.T) {
	database, request, execution, session := authenticatedClaimFixture(t)
	if err := database.ClaimRequestExecutionAuthenticated(request, execution, session.SessionKey); err != nil {
		t.Fatal(err)
	}
	code, duration := 0, int64(1200)
	execution.ExitCode = &code
	execution.DurationMs = &duration
	other := &Session{AgentName: "other", ProjectPath: request.ProjectPath, Model: "two", Program: "test"}
	if err := database.CreateSession(other); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"wrong key", "wrong claim", "wrong session", "negative duration", "bad success"} {
		candidate := *execution
		key := session.SessionKey
		switch kind {
		case "wrong key":
			key = strings.Repeat("00", 32)
		case "wrong claim":
			candidate.LogPath += "-stale"
		case "wrong session":
			candidate.ExecutedBySessionID = other.ID
			key = other.SessionKey
		case "negative duration":
			n := int64(-1)
			candidate.DurationMs = &n
		case "bad success":
			n := 7
			candidate.ExitCode = &n
		}
		if err := database.CompleteRequestExecutionAuthenticated(request.ID, StatusExecuted, &candidate, key); err == nil {
			t.Errorf("accepted %s", kind)
		}
	}
	// Session end prohibits new claims, not reporting the outcome of this one.
	if _, err := database.Exec(`UPDATE sessions SET ended_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339), session.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteRequestExecutionAuthenticated(request.ID, StatusExecuted, execution, session.SessionKey); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusExecuted || stored.Execution.ExecutedByAgent != execution.ExecutedByAgent || stored.Execution.ExecutedAt == nil || !stored.Execution.ExecutedAt.Equal(execution.ExecutedAt.Truncate(time.Second)) || *stored.Execution.DurationMs != duration {
		t.Fatalf("outcome lost original identity: %+v", stored.Execution)
	}
	if err := database.CompleteRequestExecutionAuthenticated(request.ID, StatusExecuted, execution, session.SessionKey); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("replayed completion: %v", err)
	}
}

func TestAuthenticatedCompletionIsAtomic(t *testing.T) {
	database, request, execution, session := authenticatedClaimFixture(t)
	if err := database.ClaimRequestExecutionAuthenticated(request, execution, session.SessionKey); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`CREATE TRIGGER deny_outcome BEFORE UPDATE OF execution_exit_code ON requests BEGIN SELECT RAISE(ABORT,'outcome failure'); END`); err != nil {
		t.Fatal(err)
	}
	code := 7
	execution.ExitCode = &code
	if err := database.CompleteRequestExecutionAuthenticated(request.ID, StatusExecutionFailed, execution, session.SessionKey); err == nil {
		t.Fatal("accepted failed persistence")
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusExecuting || stored.Execution.ExitCode != nil || stored.ResolvedAt != nil {
		t.Fatal("outcome partially committed")
	}
}
