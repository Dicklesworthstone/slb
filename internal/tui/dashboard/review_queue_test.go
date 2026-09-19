package dashboard

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	tea "github.com/charmbracelet/bubbletea"
)

func TestDashboardQueueIncludesEscalationsAndActualOutcomes(t *testing.T) {
	project := t.TempDir()
	database, err := db.OpenAndMigrate(filepath.Join(project, ".slb", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	author := &db.Session{AgentName: "author", Model: "model-a", ProjectPath: project}
	if err := database.CreateSession(author); err != nil {
		t.Fatal(err)
	}
	for i, status := range []db.RequestStatus{db.StatusPending, db.StatusEscalated, db.StatusExecuted, db.StatusRejected, db.StatusExecutionFailed, db.StatusCancelled} {
		r := &db.Request{ProjectPath: project, RequestorSessionID: author.ID, RequestorAgent: author.AgentName, RequestorModel: author.Model,
			Command: db.CommandSpec{Raw: "echo secret", Cwd: project, ContainsSensitive: true}, Status: db.StatusPending, RiskTier: db.RiskTierDangerous, MinApprovals: 1, Justification: db.Justification{Reason: "queue test"}}
		if _, err := database.AdmitRequest(context.Background(), r, db.RequestAdmissionLimits{MaxPending: 10, MaxPerMinute: 10}); err != nil {
			t.Fatal(err)
		}
		resolved := time.Now().Add(-time.Duration(i) * time.Minute).UTC().Format(time.RFC3339)
		if _, err := database.Exec(`UPDATE requests SET status=?,resolved_at=? WHERE id=?`, string(status), resolved, r.ID); err != nil {
			t.Fatal(err)
		}
	}
	// A foreign request must not join this project's human review queue.
	if _, err := database.Exec(`UPDATE requests SET project_path=? WHERE status=?`, "/foreign", string(db.StatusCancelled)); err != nil {
		t.Fatal(err)
	}
	agents, pending, activity, err := loadData(project)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || len(pending) != 2 || pending[0].Status != string(db.StatusEscalated) || pending[1].Status != string(db.StatusPending) {
		t.Fatalf("wrong review queue: %+v", pending)
	}
	for _, row := range pending {
		if row.Command != "[REDACTED]" {
			t.Fatalf("sensitive command leaked in queue: %+v", row)
		}
	}
	joined := strings.Join(activity, "\n")
	for _, status := range []string{"executed", "rejected", "execution_failed", "escalated"} {
		if !strings.Contains(joined, status) {
			t.Fatalf("real outcome %s missing: %s", status, joined)
		}
	}
	if strings.Contains(joined, "cancelled") {
		t.Fatal("foreign request leaked into activity")
	}
	m := New(project)
	m.ready = true
	m.width = 120
	m.height = 40
	m.pending = pending
	if !strings.Contains(m.View(), "ESCALATED") {
		t.Fatal("escalated request not visibly marked")
	}
}

func TestDashboardSelectionSurvivesEscalationReorderingAndReadFailure(t *testing.T) {
	m := New("/project")
	m.pending = []requestRow{{ID: "chosen"}, {ID: "other"}}
	next, _ := m.Update(dataMsg{pending: []requestRow{{ID: "new-escalation", Status: "escalated"}, {ID: "other"}, {ID: "chosen"}}, refreshedAt: time.Now()})
	m = next.(Model)
	if m.SelectedRequestID() != "chosen" {
		t.Fatal("refresh changed selected request by row position")
	}
	next, _ = m.Update(dataMsg{err: errors.New("database unavailable")})
	m = next.(Model)
	if len(m.pending) != 3 || m.SelectedRequestID() != "chosen" || m.lastErr == nil {
		t.Fatal("failed read pretended review queue was empty")
	}
	next, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if !strings.Contains(next.(Model).View(), "last known state") {
		t.Fatal("stale data not labelled")
	}
}

func TestDashboardStateRowsRejectCorruptTimesAndBoundActivity(t *testing.T) {
	state := &db.ProjectWatchState{}
	for i := 0; i < 20; i++ {
		state.Requests = append(state.Requests, db.WatchRequestState{ID: time.Unix(int64(i), 0).String(), Status: db.StatusApproved, CreatedAt: time.Now().UTC().Format(time.RFC3339)})
	}
	pending, activity, err := projectStateRows(state)
	if err != nil || len(pending) != 0 || len(activity) != 10 {
		t.Fatalf("activity not bounded: %d %v", len(activity), err)
	}
	state.Requests[0].CreatedAt = "invalid"
	if _, _, err := projectStateRows(state); err == nil {
		t.Fatal("corrupt timestamp silently displayed")
	}
}
