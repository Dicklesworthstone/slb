package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func admissionFixture(t *testing.T) (*DB, *Session) {
	t.Helper()
	project := t.TempDir()
	database, err := OpenAndMigrate(filepath.Join(project, "admission.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	session := &Session{AgentName: "admission-agent", Model: "model-a", ProjectPath: project}
	if err := database.CreateSession(session); err != nil {
		t.Fatal(err)
	}
	return database, session
}

func admissionRequest(session *Session) *Request {
	return &Request{
		ProjectPath:        session.ProjectPath,
		RequestorSessionID: session.ID, RequestorAgent: session.AgentName, RequestorModel: session.Model,
		Command:  CommandSpec{Raw: "echo admission", Argv: []string{"echo", "admission"}, Cwd: session.ProjectPath},
		RiskTier: RiskTierDangerous, Status: StatusPending, MinApprovals: 1,
		Justification: Justification{Reason: "test admission without executing a command"},
	}
}

func admissionCount(t *testing.T, database *DB) int {
	t.Helper()
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestAdmitRequestConcurrentConnections(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits RequestAdmissionLimits
	}{
		{"pending", RequestAdmissionLimits{MaxPending: 3, MaxPerMinute: 100}},
		{"per-minute", RequestAdmissionLimits{MaxPending: 100, MaxPerMinute: 3}},
		{"both", RequestAdmissionLimits{MaxPending: 3, MaxPerMinute: 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, session := admissionFixture(t)
			const clients = 16
			connections := make([]*DB, clients)
			for i := range connections {
				other, err := OpenWithOptions(database.Path(), OpenOptions{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = other.Close() })
				connections[i] = other
			}
			start := make(chan struct{})
			results := make(chan error, clients)
			var wg sync.WaitGroup
			for _, connection := range connections {
				wg.Add(1)
				go func(connection *DB) {
					defer wg.Done()
					<-start
					r := admissionRequest(session)
					_, err := connection.AdmitRequest(context.Background(), r, tc.limits)
					if err != nil && (r.ID != "" || !r.CreatedAt.IsZero() || r.Command.Hash != "") {
						err = fmt.Errorf("rejected candidate was mutated: %+v: %w", r, err)
					}
					results <- err
				}(connection)
			}
			close(start)
			wg.Wait()
			close(results)
			accepted, rejected := 0, 0
			for err := range results {
				var denied *RequestAdmissionLimitError
				switch {
				case err == nil:
					accepted++
				case errors.As(err, &denied):
					rejected++
					if !denied.Stats.Exceeded {
						t.Errorf("missing quota diagnostic: %+v", denied)
					}
				default:
					t.Errorf("unexpected admission error: %v", err)
				}
			}
			if accepted != 3 || rejected != clients-3 || admissionCount(t, database) != 3 {
				t.Fatalf("accepted=%d rejected=%d; want 3 and %d", accepted, rejected, clients-3)
			}
		})
	}
}

func TestAdmitRequestPreservesFields(t *testing.T) {
	database, session := admissionFixture(t)
	r := admissionRequest(session)
	r.Command.Shell = true
	r.Command.ContainsSensitive = true
	r.Command.DisplayRedacted = "[REDACTED]"
	r.MinApprovals = 2
	r.RequireDifferentModel = true
	r.Justification = Justification{Reason: "reason", ExpectedEffect: "effect", Goal: "goal", SafetyArgument: "safety"}
	r.Attachments = []Attachment{{Type: "context", Content: "evidence", Metadata: map[string]any{"source": "test"}}}
	r.DryRun = &DryRunResult{Command: "echo preview", Output: "preview"}
	stats, err := database.AdmitRequest(context.Background(), r, RequestAdmissionLimits{MaxPending: 1, MaxPerMinute: 1})
	if err != nil {
		t.Fatal(err)
	}
	if r.ID == "" || r.Command.Hash != ComputeCommandHash(r.Command) || r.CreatedAt.IsZero() || r.ExpiresAt == nil || stats.Pending != 0 || stats.Recent != 0 {
		t.Fatalf("incomplete admission: %+v, %+v", r, stats)
	}
	var argv, attachments, raw, redacted, dryCommand, dryOutput string
	var approvals, shell, sensitive, different int
	err = database.QueryRow(`SELECT command_raw, command_argv_json, command_display_redacted, command_shell,
		command_contains_sensitive, min_approvals, require_different_model, attachments_json, dry_run_command, dry_run_output
		FROM requests WHERE id = ?`, r.ID).Scan(&raw, &argv, &redacted, &shell, &sensitive, &approvals, &different, &attachments, &dryCommand, &dryOutput)
	if err != nil {
		t.Fatal(err)
	}
	var decodedArgv []string
	var decodedAttachments []Attachment
	if err := json.Unmarshal([]byte(argv), &decodedArgv); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(attachments), &decodedAttachments); err != nil {
		t.Fatal(err)
	}
	if raw != r.Command.Raw || redacted != "[REDACTED]" || shell != 1 || sensitive != 1 || approvals != 2 || different != 1 || dryCommand != r.DryRun.Command || dryOutput != r.DryRun.Output || !reflect.DeepEqual(decodedArgv, r.Command.Argv) || !reflect.DeepEqual(decodedAttachments, r.Attachments) {
		t.Fatal("admission lost request evidence, binding or policy fields")
	}
	var ftsCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM requests_fts WHERE requests_fts MATCH 'admission'`).Scan(&ftsCount); err != nil || ftsCount != 1 {
		t.Fatalf("FTS insert trigger: count=%d err=%v", ftsCount, err)
	}
}

func TestAdmitRequestSessionBinding(t *testing.T) {
	for _, name := range []string{"ended", "project", "agent", "model", "missing"} {
		t.Run(name, func(t *testing.T) {
			database, session := admissionFixture(t)
			r := admissionRequest(session)
			want := ErrRequestAdmissionIdentity
			switch name {
			case "ended":
				if err := database.EndSession(session.ID); err != nil {
					t.Fatal(err)
				}
			case "project":
				r.ProjectPath += "-other"
			case "agent":
				r.RequestorAgent = "other-agent"
			case "model":
				if err := database.UpdateSessionModel(session.ID, "changed-model"); err != nil {
					t.Fatal(err)
				}
			case "missing":
				r.RequestorSessionID = "missing"
				want = ErrSessionNotFound
			}
			_, err := database.AdmitRequest(context.Background(), r, RequestAdmissionLimits{MaxPending: 5, MaxPerMinute: 10, WarnOnly: true})
			if !errors.Is(err, want) || admissionCount(t, database) != 0 {
				t.Fatalf("identity bypass: err=%v want=%v", err, want)
			}
		})
	}
}

func TestAdmitRequestWarningAndSessionIsolation(t *testing.T) {
	database, session := admissionFixture(t)
	limits := RequestAdmissionLimits{MaxPending: 1, MaxPerMinute: 1}
	if _, err := database.AdmitRequest(context.Background(), admissionRequest(session), limits); err != nil {
		t.Fatal(err)
	}
	limits.WarnOnly = true
	stats, err := database.AdmitRequest(context.Background(), admissionRequest(session), limits)
	if err != nil || !stats.Exceeded || stats.Pending != 1 || stats.Recent != 1 || stats.ResetAt.IsZero() {
		t.Fatalf("warning admission: stats=%+v err=%v", stats, err)
	}
	other := &Session{AgentName: "other", Model: session.Model, ProjectPath: session.ProjectPath}
	if err := database.CreateSession(other); err != nil {
		t.Fatal(err)
	}
	limits.WarnOnly = false
	stats, err = database.AdmitRequest(context.Background(), admissionRequest(other), limits)
	if err != nil || stats.Exceeded || stats.Pending != 0 || stats.Recent != 0 || admissionCount(t, database) != 3 {
		t.Fatalf("sessions share quota: stats=%+v err=%v", stats, err)
	}
}

func TestAdmitRequestWindowAndReset(t *testing.T) {
	for _, name := range []string{"old", "recent-terminal", "reset-recent", "reset-pending", "future-reset", "corrupt-reset"} {
		t.Run(name, func(t *testing.T) {
			database, session := admissionFixture(t)
			seed := admissionRequest(session)
			if _, err := database.AdmitRequest(context.Background(), seed, RequestAdmissionLimits{MaxPending: 5, MaxPerMinute: 5}); err != nil {
				t.Fatal(err)
			}
			created := time.Now().UTC().Add(-20 * time.Second)
			status := StatusCancelled
			limits := RequestAdmissionLimits{MaxPending: 1, MaxPerMinute: 1}
			wantLimit, wantOtherError := false, false
			switch name {
			case "old":
				created = time.Now().UTC().Add(-2 * time.Minute)
			case "recent-terminal":
				wantLimit = true
			case "reset-recent", "reset-pending":
				if _, err := database.ResetSessionRateLimits(session.ID, time.Now().UTC().Add(-5*time.Second)); err != nil {
					t.Fatal(err)
				}
				if name == "reset-pending" {
					status = StatusPending
					wantLimit = true
				}
			case "future-reset", "corrupt-reset":
				value := "broken timestamp"
				if name == "future-reset" {
					value = time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
				}
				if _, err := database.Exec(`UPDATE sessions SET rate_limit_reset_at = ? WHERE id = ?`, value, session.ID); err != nil {
					t.Fatal(err)
				}
				wantOtherError = true
			}
			if _, err := database.Exec(`UPDATE requests SET created_at = ?, status = ? WHERE id = ?`, created.Format(time.RFC3339), string(status), seed.ID); err != nil {
				t.Fatal(err)
			}
			_, err := database.AdmitRequest(context.Background(), admissionRequest(session), limits)
			var denied *RequestAdmissionLimitError
			if errors.As(err, &denied) != wantLimit || (err != nil && !errors.As(err, &denied)) != wantOtherError {
				t.Fatalf("unexpected result: err=%v wantLimit=%v wantOtherError=%v", err, wantLimit, wantOtherError)
			}
			wantCount := 2
			if wantLimit || wantOtherError {
				wantCount = 1
			}
			if admissionCount(t, database) != wantCount {
				t.Fatal("failed admission left a request")
			}
		})
	}
}

func TestAdmitRequestRollbackAndValidation(t *testing.T) {
	for _, name := range []string{"cancelled", "write-failure", "duplicate", "hash", "status", "expired", "invalid-json", "invalid-limits", "nil"} {
		t.Run(name, func(t *testing.T) {
			database, session := admissionFixture(t)
			r := admissionRequest(session)
			ctx := context.Background()
			limits := RequestAdmissionLimits{MaxPending: 5, MaxPerMinute: 10}
			wantCount := 0
			switch name {
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "write-failure":
				if _, err := database.Exec(`CREATE TRIGGER admission_failure BEFORE INSERT ON requests BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
					t.Fatal(err)
				}
			case "duplicate":
				seed := admissionRequest(session)
				if _, err := database.AdmitRequest(ctx, seed, limits); err != nil {
					t.Fatal(err)
				}
				r.ID = seed.ID
				wantCount = 1
			case "hash":
				r.Command.Hash = "incorrect"
			case "status":
				r.Status = StatusApproved
			case "expired":
				expired := time.Now().Add(-time.Minute)
				r.ExpiresAt = &expired
			case "invalid-json":
				r.Attachments = []Attachment{{Metadata: map[string]any{"invalid": make(chan int)}}}
			case "invalid-limits":
				limits.MaxPending = 0
			case "nil":
				r = nil
			}
			var before Request
			if r != nil {
				before = *r
			}
			_, err := database.AdmitRequest(ctx, r, limits)
			if err == nil || admissionCount(t, database) != wantCount {
				t.Fatalf("failed admission committed: err=%v", err)
			}
			if r != nil && !reflect.DeepEqual(before, *r) {
				t.Fatal("failed admission mutated caller request")
			}
			// A failed transaction must release its writer reservation.
			if _, err := database.Exec(`UPDATE sessions SET last_active_at = last_active_at WHERE id = ?`, session.ID); err != nil {
				t.Fatalf("writer lock leaked: %v", err)
			}
		})
	}
}
