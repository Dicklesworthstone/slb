package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/tui/request"
	tea "github.com/charmbracelet/bubbletea"
)

// Run one refresh worker/result but deliberately not its next timer.
func refreshInteractiveOnce(t *testing.T, m Model) Model {
	t.Helper()
	m.detailRefreshToken++
	next, read := m.beginDetailRefresh(detailRefreshTick{m.detailGeneration, m.detailRefreshToken})
	m = next.(Model)
	if read == nil || !m.detailRefreshInFlight {
		t.Fatal("refresh worker not scheduled")
	}
	next, _ = m.Update(read())
	return next.(Model)
}

func TestDetailRefreshObservesPeerReviewAndEndedSession(t *testing.T) {
	database, target, reviewer, opts := interactiveReviewFixture(t, 1)
	m := interactiveReviewModel(opts, target, reviewer)
	peer := &db.Session{AgentName: "peer", Model: "other", ProjectPath: opts.ProjectPath}
	if err := database.CreateSession(peer); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ApplyReview(&db.Review{RequestID: target.ID, ReviewerSessionID: peer.ID, Decision: db.DecisionReject}, peer.SessionKey, db.ReviewPolicy{}); err != nil {
		t.Fatal(err)
	}
	if err := database.EndSession(reviewer.ID); err != nil {
		t.Fatal(err)
	}
	m = refreshInteractiveOnce(t, m)
	if m.detail.Request.Status != db.StatusRejected || len(m.detail.Reviews) != 1 || m.detail.Session != nil || m.detail.SnapshotError != "" {
		t.Fatalf("peer state was stale: %+v", m.detail)
	}
}

func TestDetailRefreshIgnoresReadsBeforeLocalCommitAndOldViews(t *testing.T) {
	_, target, reviewer, opts := interactiveReviewFixture(t, 1)
	m := interactiveReviewModel(opts, target, reviewer)
	m.detailRefreshToken++
	next, read := m.beginDetailRefresh(detailRefreshTick{m.detailGeneration, m.detailRefreshToken})
	m = next.(Model)
	before := read().(detailRefreshResult)
	m.detail.ReviewPending = true
	committed := submitInteractiveReview(opts, target.ID, target.Command.Hash, db.DecisionApprove, "approved")
	if committed.Err != nil {
		t.Fatal(committed.Err)
	}
	next, _ = m.Update(committed)
	m = next.(Model)
	next, _ = m.Update(before)
	m = next.(Model)
	if m.detail.Request.Status != db.StatusApproved || len(m.detail.Reviews) != 1 {
		t.Fatal("pre-commit snapshot regressed the committed decision")
	}
	oldGeneration := m.detailGeneration
	next, _ = m.handleNavigation(navigateMsg{view: ViewDashboard})
	m = next.(Model)
	if m.detailGeneration == oldGeneration {
		t.Fatal("navigation did not invalidate readers")
	}
	next, cmd := m.Update(before)
	if next.(Model).view != ViewDashboard || cmd != nil {
		t.Fatal("old refresh navigated or started another polling chain")
	}
}

func TestDetailRefreshFailureDisablesReviewThenRecovers(t *testing.T) {
	_, target, reviewer, opts := interactiveReviewFixture(t, 1)
	m := interactiveReviewModel(opts, target, reviewer)
	m.detailRefreshInFlight = true
	msg := detailRefreshResult{generation: m.detailGeneration, token: m.detailRefreshToken, revision: m.detailRevision, requestID: target.ID, err: errors.New("read unavailable")}
	next, _ := m.Update(msg)
	m = next.(Model)
	if !strings.Contains(m.View(), "read unavailable") || m.detail.Request.ID != target.ID {
		t.Fatal("read failure erased last known state or was hidden")
	}
	m, _ = interactiveKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if m.detail.Mode != request.DetailModeView {
		t.Fatal("stale read enabled approval")
	}
	m = refreshInteractiveOnce(t, m)
	if m.detail.SnapshotError != "" || m.detail.Session == nil || m.detail.Session.SessionKey != "" {
		t.Fatal("refresh did not recover or retained a signing key")
	}
	m, _ = interactiveKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if m.detail.Mode != request.DetailModeApprove {
		t.Fatal("fresh snapshot failed to restore review")
	}
}

func TestDetailRefreshTimerHasSingleOwnedGeneration(t *testing.T) {
	_, target, reviewer, opts := interactiveReviewFixture(t, 1)
	m := interactiveReviewModel(opts, target, reviewer)
	m.options.RefreshInterval = 7
	if m.detailRefreshInterval() != 7*time.Second {
		t.Fatal("configured detail refresh ignored")
	}
	m, timer := interactiveKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f5")})
	if timer == nil {
		t.Fatal("manual refresh not scheduled")
	}
	tick := timer().(detailRefreshTick)
	if tick.token != m.detailRefreshToken {
		t.Fatal("returned model lost scheduled refresh token")
	}
	next, read := m.Update(tick)
	m = next.(Model)
	if read == nil || !m.detailRefreshInFlight {
		t.Fatal("owned timer did not launch a read")
	}
	_, repeated := m.Update(tick)
	if repeated != nil {
		t.Fatal("duplicate timer launched concurrent readers")
	}
	next, later := m.Update(read())
	m = next.(Model)
	if later == nil || m.detailRefreshToken == tick.token || m.detailRefreshInFlight {
		t.Fatal("refresh did not schedule exactly one successor")
	}
	_, obsolete := m.Update(tick)
	if obsolete != nil {
		t.Fatal("obsolete tick restarted polling")
	}
}

func TestDashboardEscalationCanBeOpenedAndReviewed(t *testing.T) {
	database, target, _, opts := interactiveReviewFixture(t, 1)
	if _, err := database.Exec(`UPDATE requests SET status=? WHERE id=?`, string(db.StatusEscalated), target.ID); err != nil {
		t.Fatal(err)
	}
	m := NewWithOptions(opts)
	// The dashboard Init batch puts the database load before its periodic tick.
	batch := m.Init()().(tea.BatchMsg)
	next, _ := m.Update(batch[0]())
	m = next.(Model)
	if m.dashboard.SelectedRequestID() != target.ID {
		t.Fatal("escalated request unreachable on dashboard")
	}
	m, _ = interactiveKey(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.view != ViewRequestDetail || m.detail.Request.Status != db.StatusEscalated {
		t.Fatal("escalated request did not open")
	}
	m, _ = interactiveKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	m, _ = interactiveKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("human reviewed escalation")})
	m, submit := interactiveKey(m, tea.KeyMsg{Type: tea.KeyCtrlS})
	if submit == nil {
		t.Fatal("escalation could not be submitted")
	}
	m = interactiveRun(m, submit)
	stored, err := database.GetRequest(target.ID)
	if err != nil || stored.Status != db.StatusRejected || !strings.Contains(m.View(), "Review recorded") {
		t.Fatalf("escalation review failed: %+v %v", stored, err)
	}
}

func TestDetailNavigationFailureIsVisible(t *testing.T) {
	_, _, _, opts := interactiveReviewFixture(t, 1)
	m := NewWithOptions(opts)
	next, _ := m.handleNavigation(navigateMsg{view: ViewRequestDetail, requestID: "missing"})
	m = next.(Model)
	if m.view != ViewDashboard || !strings.Contains(m.View(), "Could not open request") {
		t.Fatal("failed detail navigation was silently swallowed")
	}
}
