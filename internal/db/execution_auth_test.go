package db

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
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
		if _, err := tx.Exec(`UPDATE sessions SET session_key_digest=? WHERE id=?`, strings.Repeat("19", 32), session.ID); err != nil {
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

func TestSessionKeyMatches(t *testing.T) {
	key := strings.Repeat("ab", 32)
	digest, ok := SessionKeyDigest(key)
	if !ok || digest == key || strings.Contains(digest, key) {
		t.Fatalf("digest of a valid key = %q, %v", digest, ok)
	}
	short := strings.Repeat("ab", 16)
	if _, ok := SessionKeyDigest(short); ok {
		t.Fatal("short key produced a digest")
	}
	for _, test := range []struct {
		stored, supplied string
		want             bool
	}{
		{digest, key, true}, {digest, strings.ToUpper(key), true}, {strings.ToUpper(digest), key, true},
		{digest, "", false}, {digest, strings.Repeat("cd", 32), false}, {digest, short, false},
		// The stored digest is not itself a credential, nor is the key a digest.
		{digest, digest, false}, {key, key, false},
		{"broken", "broken", false}, {"", "", false}, {"", key, false},
	} {
		if got := SessionKeyMatches(test.stored, test.supplied); got != test.want {
			t.Errorf("SessionKeyMatches(%q, %q) = %v, want %v", test.stored, test.supplied, got, test.want)
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
				query = "UPDATE sessions SET session_key_digest='" + strings.Repeat("01", 32) + "'"
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
		if _, err := database.CompleteRequestExecutionAuthenticated(request.ID, StatusExecuted, &candidate, key); err == nil {
			t.Errorf("accepted %s", kind)
		}
	}
	// Session end prohibits new claims, not reporting the outcome of this one.
	if _, err := database.Exec(`UPDATE sessions SET ended_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339), session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CompleteRequestExecutionAuthenticated(request.ID, StatusExecuted, execution, session.SessionKey); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusExecuted || stored.Execution.ExecutedByAgent != execution.ExecutedByAgent || stored.Execution.ExecutedAt == nil || !stored.Execution.ExecutedAt.Equal(execution.ExecutedAt.Truncate(time.Second)) || *stored.Execution.DurationMs != duration {
		t.Fatalf("outcome lost original identity: %+v", stored.Execution)
	}
	if recovered, err := database.CompleteRequestExecutionAuthenticated(request.ID, StatusExecuted, execution, session.SessionKey); err != nil || !reflect.DeepEqual(stored, recovered) {
		t.Fatalf("exact completion retry did not recover original identity: %+v %v", recovered, err)
	}
}

func TestAuthenticatedCompletionIsAtomic(t *testing.T) {
	database, request, execution, session := authenticatedClaimFixture(t)
	if err := database.ClaimRequestExecutionAuthenticated(request, execution, session.SessionKey); err != nil {
		t.Fatal(err)
	}
	before := journalPage(t, database, request.ProjectPath, "", 100)
	if _, err := database.Exec(`CREATE TRIGGER deny_outcome BEFORE UPDATE OF execution_exit_code ON requests BEGIN SELECT RAISE(ABORT,'outcome failure'); END`); err != nil {
		t.Fatal(err)
	}
	code := 7
	execution.ExitCode = &code
	if completed, err := database.CompleteRequestExecutionAuthenticated(request.ID, StatusExecutionFailed, execution, session.SessionKey); err == nil || completed != nil {
		t.Fatalf("acknowledged failed persistence: %+v %v", completed, err)
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusExecuting || stored.Execution.ExitCode != nil || stored.ResolvedAt != nil {
		t.Fatal("outcome partially committed")
	}
	if after := journalPage(t, database, request.ProjectPath, "", 100); !reflect.DeepEqual(before, after) {
		t.Fatalf("failed completion published an audit event: before=%+v after=%+v", before, after)
	}
}

func TestAuthenticatedCompletionRetryPreservesOutcome(t *testing.T) {
	for _, status := range []RequestStatus{StatusExecuted, StatusExecutionFailed, StatusTimedOut} {
		t.Run(string(status), func(t *testing.T) {
			database, request, execution, session := authenticatedClaimFixture(t)
			if err := database.ClaimRequestExecutionAuthenticated(request, execution, session.SessionKey); err != nil {
				t.Fatal(err)
			}
			code, duration := 0, int64(1200)
			if status == StatusExecuted {
				execution.ExitCode = &code
			}
			execution.DurationMs = &duration
			first, err := database.CompleteRequestExecutionAuthenticated(request.ID, status, execution, session.SessionKey)
			if err != nil {
				t.Fatal(err)
			}
			original, err := database.GetRequest(request.ID)
			if err != nil || !reflect.DeepEqual(first, original) {
				t.Fatalf("first completion did not return its persisted outcome: %+v %v", first, err)
			}
			before := journalPage(t, database, request.ProjectPath, "", 100)
			// The first reply can be lost after its transaction committed. An
			// identical authenticated report recovers that result, never a new
			// authorization or a second outcome timestamp.
			if recovered, err := database.CompleteRequestExecutionAuthenticated(request.ID, status, execution, session.SessionKey); err != nil || !reflect.DeepEqual(original, recovered) {
				t.Fatalf("exact completion retry could not recover a committed result: %+v %v", recovered, err)
			}
			if _, err := database.Exec(`CREATE TRIGGER immutable_terminal_request BEFORE UPDATE ON requests
				WHEN OLD.status IN ('executed', 'execution_failed', 'timed_out')
				BEGIN SELECT RAISE(ABORT, 'terminal requests cannot be rewritten'); END`); err != nil {
				t.Fatal(err)
			}
			if _, err := database.CompleteRequestExecutionAuthenticated(request.ID, status, execution, session.SessionKey); err != nil {
				t.Fatalf("retry attempted to rewrite terminal evidence: %v", err)
			}
			if execution.ExitCode == nil {
				// An unavailable child result is not a reported exit code zero.
				changed := *execution
				changed.ExitCode = &code
				if completed, err := database.CompleteRequestExecutionAuthenticated(request.ID, status, &changed, session.SessionKey); err == nil || completed != nil {
					t.Fatalf("retry replaced an unknown exit with zero: %+v %v", completed, err)
				}
			}
			after, err := database.GetRequest(request.ID)
			if err != nil || !reflect.DeepEqual(original, after) {
				t.Fatalf("retry changed the original outcome: before=%+v after=%+v err=%v", original, after, err)
			}
			if events := journalPage(t, database, request.ProjectPath, "", 100); !reflect.DeepEqual(before, events) {
				t.Fatalf("retry appended or changed an audit event: before=%+v after=%+v", before, events)
			}
		})
	}
}

func TestAuthenticatedCompletionRetryRejectsChangedReport(t *testing.T) {
	database, request, execution, session := authenticatedClaimFixture(t)
	if err := database.ClaimRequestExecutionAuthenticated(request, execution, session.SessionKey); err != nil {
		t.Fatal(err)
	}
	code, duration := 7, int64(1200)
	execution.ExitCode, execution.DurationMs = &code, &duration
	original, err := database.CompleteRequestExecutionAuthenticated(request.ID, StatusExecutionFailed, execution, session.SessionKey)
	if err != nil {
		t.Fatal(err)
	}
	other := &Session{AgentName: "other", ProjectPath: request.ProjectPath, Model: "two", Program: "test"}
	foreign := &Session{AgentName: "foreign", ProjectPath: t.TempDir(), Model: "three", Program: "test"}
	for _, candidate := range []*Session{other, foreign} {
		if err := database.CreateSession(candidate); err != nil {
			t.Fatal(err)
		}
	}
	before := journalPage(t, database, request.ProjectPath, "", 100)
	for _, kind := range []string{
		"status", "exit code", "missing exit code", "duration", "missing duration",
		"claim fence", "other session", "other project", "wrong key", "empty key",
		"claim timestamp", "agent", "model", "assurance", "approval policy",
	} {
		t.Run(kind, func(t *testing.T) {
			report := *execution
			status, key := StatusExecutionFailed, session.SessionKey
			switch kind {
			case "status":
				status = StatusTimedOut
			case "exit code":
				changed := 0
				report.ExitCode = &changed
			case "missing exit code":
				report.ExitCode = nil
			case "duration":
				changed := duration + 1
				report.DurationMs = &changed
			case "missing duration":
				report.DurationMs = nil
			case "claim fence":
				report.LogPath += "-different"
			case "other session":
				report.ExecutedBySessionID, key = other.ID, other.SessionKey
			case "other project":
				report.ExecutedBySessionID, key = foreign.ID, foreign.SessionKey
			case "wrong key":
				key = strings.Repeat("00", 32)
			case "empty key":
				key = ""
			case "claim timestamp":
				changed := execution.ExecutedAt.Add(time.Second)
				report.ExecutedAt = &changed
			case "agent":
				report.ExecutedByAgent += "-different"
			case "model":
				report.ExecutedByModel += "-different"
			case "assurance":
				report.Assurance = AssuranceIndependent
			case "approval policy":
				report.ApprovalPolicy = ApprovalPolicySameHost
			}
			if completed, err := database.CompleteRequestExecutionAuthenticated(request.ID, status, &report, key); err == nil || completed != nil {
				t.Fatalf("changed report was treated as an exact retry: %+v %v", completed, err)
			}
		})
	}
	// The unauthenticated local API remains a one-shot state transition.
	if err := database.CompleteRequestExecution(request.ID, StatusExecutionFailed, execution); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("local completion unexpectedly became replayable: %v", err)
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil || !reflect.DeepEqual(original, stored) {
		t.Fatalf("conflicting report altered the original receipt: %+v %v", stored, err)
	}
	if after := journalPage(t, database, request.ProjectPath, "", 100); !reflect.DeepEqual(before, after) {
		t.Fatalf("conflicting report changed the audit journal: before=%+v after=%+v", before, after)
	}
}

func TestAuthenticatedCompletionRetryRechecksKeyAfterWriterWait(t *testing.T) {
	database, request, execution, session := authenticatedClaimFixture(t)
	if err := database.ClaimRequestExecutionAuthenticated(request, execution, session.SessionKey); err != nil {
		t.Fatal(err)
	}
	zero := 0
	execution.ExitCode = &zero
	original, err := database.CompleteRequestExecutionAuthenticated(request.ID, StatusExecuted, execution, session.SessionKey)
	if err != nil {
		t.Fatal(err)
	}
	before := journalPage(t, database, request.ProjectPath, "", 100)
	other, err := Open(database.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	started, outcome := make(chan struct{}), make(chan error, 1)
	err = database.Transaction(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE sessions SET session_key_digest=? WHERE id=?`, strings.Repeat("19", 32), session.ID); err != nil {
			return err
		}
		go func() {
			close(started)
			completed, err := other.CompleteRequestExecutionAuthenticated(request.ID, StatusExecuted, execution, session.SessionKey)
			if completed != nil {
				outcome <- fmt.Errorf("revoked retry returned a receipt: %+v", completed)
				return
			}
			outcome <- err
		}()
		<-started
		select {
		case err := <-outcome:
			return fmt.Errorf("retry completed while writer was reserved: %v", err)
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
			t.Fatalf("stale key recovered completion after lock wait: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("retry did not finish after writer released")
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil || !reflect.DeepEqual(original, stored) {
		t.Fatalf("revoked retry altered the original receipt: %+v %v", stored, err)
	}
	if after := journalPage(t, database, request.ProjectPath, "", 100); !reflect.DeepEqual(before, after) {
		t.Fatalf("revoked retry changed the audit journal: before=%+v after=%+v", before, after)
	}
}

func TestAuthenticatedCompletionConcurrentRetryReturnsOneOutcome(t *testing.T) {
	database, request, execution, session := authenticatedClaimFixture(t)
	if err := database.ClaimRequestExecutionAuthenticated(request, execution, session.SessionKey); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`CREATE TRIGGER immutable_terminal_request BEFORE UPDATE ON requests
		WHEN OLD.status IN ('executed', 'execution_failed', 'timed_out')
		BEGIN SELECT RAISE(ABORT, 'terminal requests cannot be rewritten'); END`); err != nil {
		t.Fatal(err)
	}
	before := journalPage(t, database, request.ProjectPath, "", 100)
	other, err := Open(database.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	zero, duration := 0, int64(23)
	execution.ExitCode, execution.DurationMs = &zero, &duration
	type result struct {
		request *Request
		err     error
	}
	start := make(chan struct{})
	outcomes := make(chan result, 2)
	for _, connection := range []*DB{database, other} {
		go func(connection *DB) {
			<-start
			completed, err := connection.CompleteRequestExecutionAuthenticated(request.ID, StatusExecuted, execution, session.SessionKey)
			outcomes <- result{completed, err}
		}(connection)
	}
	close(start)
	for i := 0; i < 2; i++ {
		outcome := <-outcomes
		if outcome.err != nil {
			t.Fatalf("identical concurrent report failed: %v", outcome.err)
		}
		stored, err := database.GetRequest(request.ID)
		if err != nil || !reflect.DeepEqual(stored, outcome.request) {
			t.Fatalf("concurrent report returned another result: %+v %v", outcome.request, err)
		}
	}
	after := journalPage(t, database, request.ProjectPath, before.Cursor, 100)
	if len(after.Events) != 1 || after.Events[0].State.Status != string(StatusExecuted) {
		t.Fatalf("concurrent completions did not publish exactly one outcome: %+v", after.Events)
	}
}
