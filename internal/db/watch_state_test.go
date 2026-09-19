package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestProjectWatchStateScopeRedactionAndCounts(t *testing.T) {
	database, session := admissionFixture(t)
	r := admissionRequest(session)
	r.Command.Raw = "password=never-publish-this"
	r.Command.ContainsSensitive = true
	if _, err := database.AdmitRequest(context.Background(), r, RequestAdmissionLimits{MaxPending: 10, MaxPerMinute: 10}); err != nil {
		t.Fatal(err)
	}
	// Exercise the real reviews table without depending on signature helpers:
	// these are submitted vote COUNTS, not approval proof.
	if _, err := database.Exec(`INSERT INTO reviews (id, request_id, reviewer_session_id, reviewer_agent, reviewer_model, decision, signature, signature_timestamp, created_at)
		VALUES ('watch-vote', ?, ?, 'reviewer', 'model', 'approve', 'not-a-proof', ?, ?)`, r.ID, session.ID, time.Now().UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	other := &Session{AgentName: "other-project", ProjectPath: session.ProjectPath + "/other", Model: "other"}
	if err := database.CreateSession(other); err != nil {
		t.Fatal(err)
	}
	if _, err := database.AdmitRequest(context.Background(), admissionRequest(other), RequestAdmissionLimits{MaxPending: 10, MaxPerMinute: 10}); err != nil {
		t.Fatal(err)
	}
	state, err := database.ReadProjectWatchState(context.Background(), session.ProjectPath, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if state.ActiveSessions != 1 || len(state.Requests) != 1 || state.Requests[0].ID != r.ID || state.Requests[0].Command != "[REDACTED]" || state.Requests[0].Approvals != 1 || state.Requests[0].Rejections != 0 {
		t.Fatalf("incorrect or unsafe projection: %+v", state)
	}
	if _, err := database.Exec(`UPDATE requests SET command_display_redacted = 'custom-safe-display', execution_exit_code = 7 WHERE id = ?`, r.ID); err != nil {
		t.Fatal(err)
	}
	state, err = database.ReadProjectWatchState(context.Background(), session.ProjectPath, time.Now().Add(-time.Hour))
	if err != nil || state.Requests[0].Command != "custom-safe-display" || state.Requests[0].ExitCode == nil || *state.Requests[0].ExitCode != 7 {
		t.Fatalf("lost display/outcome: %+v %v", state, err)
	}
}

func TestProjectWatchStateKeepsOldUnresolvedAndRecentCompletions(t *testing.T) {
	database, session := admissionFixture(t)
	for i, status := range []RequestStatus{StatusPending, StatusApproved, StatusExecuting, StatusEscalated, StatusExecuted, StatusExecuted} {
		r := admissionRequest(session)
		if _, err := database.AdmitRequest(context.Background(), r, RequestAdmissionLimits{MaxPending: 10, MaxPerMinute: 10}); err != nil {
			t.Fatal(err)
		}
		stamp := time.Now().Add(-48 * time.Hour)
		if i == 5 {
			stamp = time.Now()
		}
		if _, err := database.Exec(`UPDATE requests SET status = ?, created_at = ?, resolved_at = ? WHERE id = ?`, string(status), stamp.UTC().Format(time.RFC3339), stamp.UTC().Format(time.RFC3339), r.ID); err != nil {
			t.Fatal(err)
		}
	}
	state, err := database.ReadProjectWatchState(context.Background(), session.ProjectPath, time.Now().Add(-15*time.Minute))
	if err != nil || len(state.Requests) != 5 {
		t.Fatalf("lost unresolved or retained stale history: %+v %v", state, err)
	}
}

func TestProjectWatchStateFailsExplicitly(t *testing.T) {
	database, session := admissionFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if state, err := database.ReadProjectWatchState(ctx, session.ProjectPath, time.Now()); state != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled snapshot: %+v %v", state, err)
	}
	if state, err := database.ReadProjectWatchState(context.Background(), "", time.Now()); state != nil || err == nil {
		t.Fatal("unscoped snapshot accepted")
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if state, err := database.ReadProjectWatchState(context.Background(), session.ProjectPath, time.Now()); state != nil || err == nil {
		t.Fatal("storage failure returned an empty successful snapshot")
	}
}
