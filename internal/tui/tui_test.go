package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/tui/request"
)

func TestNew(t *testing.T) {
	m := New()
	if m.dashboard == nil {
		t.Error("dashboard model should not be nil")
	}
	if m.view != ViewDashboard {
		t.Error("initial view should be ViewDashboard")
	}
}

func TestNewWithOptions(t *testing.T) {
	opts := Options{
		ProjectPath:     "/tmp/test",
		Theme:           "latte",
		DisableMouse:    true,
		RefreshInterval: 10,
	}
	m := NewWithOptions(opts)
	if m.options.ProjectPath != "/tmp/test" {
		t.Errorf("expected project path /tmp/test, got %s", m.options.ProjectPath)
	}
	if m.options.DisableMouse != true {
		t.Error("expected DisableMouse to be true")
	}
}

func TestModelInit(t *testing.T) {
	m := New()
	cmd := m.Init()
	// Init may return commands - just verify it doesn't panic
	_ = cmd
}

func TestModelUpdate(t *testing.T) {
	m := New()
	updated, cmd := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if updated == nil {
		t.Error("Update should return non-nil model")
	}
	_ = cmd

	// Verify size was stored
	um := updated.(Model)
	if um.width != 80 || um.height != 24 {
		t.Errorf("expected dimensions 80x24, got %dx%d", um.width, um.height)
	}
}

func TestModelView(t *testing.T) {
	m := New()
	// Set window size first
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)

	view := m.View()
	if view == "" {
		t.Error("View should return non-empty string")
	}
}

func TestNavigateToPatterns(t *testing.T) {
	m := New()
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)

	// Simulate 'm' key press to navigate to patterns
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	um := updated.(Model)
	if um.view != ViewPatterns {
		t.Errorf("expected view to be ViewPatterns, got %d", um.view)
	}
}

func TestNavigateToHistory(t *testing.T) {
	m := New()
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)

	// Simulate 'H' key press to navigate to history
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'H'}})
	um := updated.(Model)
	if um.view != ViewHistory {
		t.Errorf("expected view to be ViewHistory, got %d", um.view)
	}
}

func TestNavigateBackFromPatterns(t *testing.T) {
	m := New()
	m.view = ViewPatterns

	// Simulate 'esc' key press to navigate back
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	um := updated.(Model)
	if um.view != ViewDashboard {
		t.Errorf("expected view to be ViewDashboard, got %d", um.view)
	}
}

func TestNavigateBackFromHistory(t *testing.T) {
	m := New()
	m.view = ViewHistory

	// Simulate 'b' key press to navigate back
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	um := updated.(Model)
	if um.view != ViewDashboard {
		t.Errorf("expected view to be ViewDashboard, got %d", um.view)
	}
}

func TestDefaultOptions(t *testing.T) {
	opts := DefaultOptions()
	if opts.RefreshInterval != 5 {
		t.Errorf("expected default refresh interval 5, got %d", opts.RefreshInterval)
	}
	if opts.DisableMouse != false {
		t.Error("expected default DisableMouse to be false")
	}
	if opts.Theme != "" {
		t.Errorf("expected default theme to be empty, got %s", opts.Theme)
	}
}

// ============== Placeholder Model Tests ==============

func TestPlaceholderModelInit(t *testing.T) {
	m := placeholderModel{}
	cmd := m.Init()
	if cmd != nil {
		t.Error("Init should return nil")
	}
}

func TestPlaceholderModelUpdate(t *testing.T) {
	m := placeholderModel{}

	// Test non-quit key
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if cmd != nil {
		t.Error("Non-quit key should not return quit command")
	}
	_ = updated

	// Test 'q' key
	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	// Note: tea.Quit is a function, so we check if cmd is non-nil for quit
	_ = cmd
	_ = updated

	// Test ctrl+c
	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	_ = cmd
	_ = updated

	// Test esc
	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	_ = cmd
	_ = updated
}

func TestPlaceholderModelView(t *testing.T) {
	m := placeholderModel{}
	view := m.View()
	if view == "" {
		t.Error("View should return non-empty string")
	}
	if len(view) < 10 {
		t.Error("View should return a meaningful message")
	}
}

// ============== Init Tests for Different Views ==============

func TestModelInitHistory(t *testing.T) {
	m := New()
	m.view = ViewHistory
	cmd := m.Init()
	// Init may return commands for history view
	_ = cmd
}

func TestModelInitPatterns(t *testing.T) {
	m := New()
	m.view = ViewPatterns
	cmd := m.Init()
	// Init may return commands for patterns view
	_ = cmd
}

func TestModelInitRequestDetail(t *testing.T) {
	m := New()
	m.view = ViewRequestDetail
	// detail is nil, should just return nil
	cmd := m.Init()
	if cmd != nil {
		t.Error("Init with nil detail should return nil")
	}
}

func TestModelInitRequestDetailWithModel(t *testing.T) {
	m := New()
	m.view = ViewRequestDetail
	// Create a mock request
	req := &db.Request{
		ID:        "test-123",
		Command:   db.CommandSpec{Raw: "test command"},
		RiskTier:  db.RiskTierCaution,
		Status:    db.StatusPending,
		CreatedAt: time.Now(),
	}
	m.detail = request.NewDetailModel(req, nil)
	cmd := m.Init()
	// Should return the detail's init command
	_ = cmd
}

// ============== View Tests for Different Views ==============

func TestModelViewHistory(t *testing.T) {
	m := New()
	m.view = ViewHistory
	view := m.View()
	if view == "" {
		t.Error("History view should return non-empty string")
	}
}

func TestModelViewPatterns(t *testing.T) {
	m := New()
	m.view = ViewPatterns
	view := m.View()
	if view == "" {
		t.Error("Patterns view should return non-empty string")
	}
}

func TestModelViewRequestDetailNil(t *testing.T) {
	m := New()
	m.view = ViewRequestDetail
	m.detail = nil
	view := m.View()
	if view != "Loading..." {
		t.Errorf("expected 'Loading...' for nil detail, got '%s'", view)
	}
}

func TestModelViewRequestDetailWithModel(t *testing.T) {
	m := New()
	m.view = ViewRequestDetail
	req := &db.Request{
		ID:        "test-123",
		Command:   db.CommandSpec{Raw: "test command"},
		RiskTier:  db.RiskTierCaution,
		Status:    db.StatusPending,
		CreatedAt: time.Now(),
	}
	m.detail = request.NewDetailModel(req, nil)
	// Set window size for proper rendering
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)
	m.view = ViewRequestDetail // Reset view after update
	view := m.View()
	if view == "" {
		t.Error("RequestDetail view should return non-empty string")
	}
}

func TestModelViewDashboardNil(t *testing.T) {
	m := New()
	m.dashboard = nil
	view := m.View()
	if view != "Loading..." {
		t.Errorf("expected 'Loading...' for nil dashboard, got '%s'", view)
	}
}

// ============== Navigation Tests ==============

func TestNavigateBackFromRequestDetail(t *testing.T) {
	m := New()
	m.view = ViewRequestDetail
	req := &db.Request{
		ID:        "test-123",
		Command:   db.CommandSpec{Raw: "test command"},
		RiskTier:  db.RiskTierCaution,
		Status:    db.StatusPending,
		CreatedAt: time.Now(),
	}
	m.detail = request.NewDetailModel(req, nil)

	// Simulate 'esc' key press to navigate back
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	um := updated.(Model)
	if um.view != ViewDashboard {
		t.Errorf("expected view to be ViewDashboard, got %d", um.view)
	}
}

func TestNavigateBackFromHistoryEsc(t *testing.T) {
	m := New()
	m.view = ViewHistory

	// Simulate 'esc' key press to navigate back
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	um := updated.(Model)
	if um.view != ViewDashboard {
		t.Errorf("expected view to be ViewDashboard, got %d", um.view)
	}
}

func TestNavigateBackFromPatternsB(t *testing.T) {
	m := New()
	m.view = ViewPatterns

	// Simulate 'b' key press to navigate back
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	um := updated.(Model)
	if um.view != ViewDashboard {
		t.Errorf("expected view to be ViewDashboard, got %d", um.view)
	}
}

func TestNavigateBackFromRequestDetailB(t *testing.T) {
	m := New()
	m.view = ViewRequestDetail

	// Simulate 'b' key press to navigate back
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	um := updated.(Model)
	if um.view != ViewDashboard {
		t.Errorf("expected view to be ViewDashboard, got %d", um.view)
	}
}

func TestNavigateToRequestDetailEmptyID(t *testing.T) {
	m := New()
	// Dashboard with no selected request - enter should not navigate
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)

	// Try to navigate to detail with no selection
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	um := updated.(Model)
	// Should still be on dashboard since no request selected
	if um.view != ViewDashboard {
		t.Errorf("expected view to remain ViewDashboard, got %d", um.view)
	}
}

// ============== forwardUpdate Tests ==============

func TestForwardUpdateHistory(t *testing.T) {
	m := New()
	m.view = ViewHistory
	// Forward a message to history view
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	um := updated.(Model)
	if um.view != ViewHistory {
		t.Errorf("expected view to remain ViewHistory, got %d", um.view)
	}
}

func TestForwardUpdatePatterns(t *testing.T) {
	m := New()
	m.view = ViewPatterns
	// Forward a message to patterns view
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	um := updated.(Model)
	if um.view != ViewPatterns {
		t.Errorf("expected view to remain ViewPatterns, got %d", um.view)
	}
}

func TestForwardUpdateRequestDetail(t *testing.T) {
	m := New()
	m.view = ViewRequestDetail
	req := &db.Request{
		ID:        "test-123",
		Command:   db.CommandSpec{Raw: "test command"},
		RiskTier:  db.RiskTierCaution,
		Status:    db.StatusPending,
		CreatedAt: time.Now(),
	}
	m.detail = request.NewDetailModel(req, nil)
	// Forward a window size message to detail view
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	um := updated.(Model)
	if um.view != ViewRequestDetail {
		t.Errorf("expected view to remain ViewRequestDetail, got %d", um.view)
	}
}

func TestForwardUpdateRequestDetailNil(t *testing.T) {
	m := New()
	m.view = ViewRequestDetail
	m.detail = nil
	// Forward a message with nil detail - should not panic
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	um := updated.(Model)
	if um.view != ViewRequestDetail {
		t.Errorf("expected view to remain ViewRequestDetail, got %d", um.view)
	}
}

// ============== handleNavigation Tests ==============

func TestHandleNavigationToDashboard(t *testing.T) {
	m := New()
	m.view = ViewPatterns

	// Manually trigger navigation via navigateMsg
	updated, _ := m.Update(navigateMsg{view: ViewDashboard})
	um := updated.(Model)
	if um.view != ViewDashboard {
		t.Errorf("expected view to be ViewDashboard, got %d", um.view)
	}
	if um.dashboard == nil {
		t.Error("dashboard should be reinitialized")
	}
}

func TestHandleNavigationToRequestDetailWithID(t *testing.T) {
	m := New()
	// Navigation to detail with an ID (will fail to load since no DB)
	updated, _ := m.Update(navigateMsg{view: ViewRequestDetail, requestID: "nonexistent-id"})
	um := updated.(Model)
	// Should fall back to dashboard since request not found
	if um.view != ViewDashboard {
		t.Errorf("expected fallback to ViewDashboard, got %d", um.view)
	}
}

func TestHandleNavigationToRequestDetailNoID(t *testing.T) {
	m := New()
	// Navigation to detail without ID - should fall back to dashboard
	updated, _ := m.Update(navigateMsg{view: ViewRequestDetail, requestID: ""})
	um := updated.(Model)
	if um.view != ViewDashboard {
		t.Errorf("expected fallback to ViewDashboard, got %d", um.view)
	}
}

// ============== Callback Setup Tests ==============

func TestSetupDashboardCallbacksNil(t *testing.T) {
	m := New()
	m.dashboard = nil
	// Should not panic
	m.setupDashboardCallbacks()
}

func TestSetupDashboardCallbacks(t *testing.T) {
	m := New()
	m.setupDashboardCallbacks()
	// Verify callbacks are set
	if m.dashboard.OnPatterns == nil {
		t.Error("OnPatterns callback should be set")
	}
	if m.dashboard.OnHistory == nil {
		t.Error("OnHistory callback should be set")
	}
}

func TestSetupDetailCallbacksNil(t *testing.T) {
	m := New()
	m.detail = nil
	// Should not panic
	m.setupDetailCallbacks()
}

func TestSetupDetailCallbacks(t *testing.T) {
	m := New()
	req := &db.Request{
		ID:        "test-123",
		Command:   db.CommandSpec{Raw: "test command"},
		RiskTier:  db.RiskTierCaution,
		Status:    db.StatusPending,
		CreatedAt: time.Now(),
	}
	m.detail = request.NewDetailModel(req, nil)
	m.setupDetailCallbacks()

	// Verify callbacks are set
	if m.detail.OnBack == nil {
		t.Error("OnBack callback should be set")
	}
	if m.detail.OnApprove == nil {
		t.Error("OnApprove callback should be set")
	}
	if m.detail.OnReject == nil {
		t.Error("OnReject callback should be set")
	}
}

func TestSetupDetailCallbacksOnBack(t *testing.T) {
	m := New()
	req := &db.Request{
		ID:        "test-123",
		Command:   db.CommandSpec{Raw: "test command"},
		RiskTier:  db.RiskTierCaution,
		Status:    db.StatusPending,
		CreatedAt: time.Now(),
	}
	m.detail = request.NewDetailModel(req, nil)
	m.setupDetailCallbacks()

	// Test OnBack callback
	cmd := m.detail.OnBack()
	if cmd == nil {
		t.Error("OnBack should return a command")
	}
	// Execute the command to get the message
	msg := cmd()
	if navMsg, ok := msg.(navigateMsg); ok {
		if navMsg.view != ViewDashboard {
			t.Errorf("OnBack should navigate to dashboard, got view %d", navMsg.view)
		}
	} else {
		t.Errorf("OnBack should return navigateMsg, got %T", msg)
	}
}

func TestSetupHistoryCallbacks(t *testing.T) {
	m := New()
	m.setupHistoryCallbacks()
	// Verify callbacks are set
	if m.history.OnBack == nil {
		t.Error("OnBack callback should be set")
	}
	if m.history.OnSelect == nil {
		t.Error("OnSelect callback should be set")
	}
}

func TestSetupPatternsCallbacks(t *testing.T) {
	m := New()
	m.setupPatternsCallbacks()
	// Verify callback is set
	if m.patterns.OnBack == nil {
		t.Error("OnBack callback should be set")
	}
}

// ============== Approve/Reject Request Tests ==============

func TestApproveRequestCommand(t *testing.T) {
	m := New()
	cmd := m.approveRequest("test-123", "approved")
	if cmd == nil {
		t.Error("approveRequest should return a command")
	}
	// Execute the command
	msg := cmd()
	if navMsg, ok := msg.(navigateMsg); ok {
		if navMsg.view != ViewDashboard {
			t.Errorf("approveRequest should navigate to dashboard, got view %d", navMsg.view)
		}
	}
}

func TestRejectRequestCommand(t *testing.T) {
	m := New()
	cmd := m.rejectRequest("test-123", "rejected")
	if cmd == nil {
		t.Error("rejectRequest should return a command")
	}
	// Execute the command
	msg := cmd()
	if navMsg, ok := msg.(navigateMsg); ok {
		if navMsg.view != ViewDashboard {
			t.Errorf("rejectRequest should navigate to dashboard, got view %d", navMsg.view)
		}
	}
}

// ============== Other Message Types ==============

func TestUpdateWithOtherMessage(t *testing.T) {
	m := New()
	// Test with a generic message type
	type customMsg struct{}
	updated, _ := m.Update(customMsg{})
	if updated == nil {
		t.Error("Update should return non-nil model")
	}
}

func TestUpdateKeyPressOnDashboardNonNavigation(t *testing.T) {
	m := New()
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(Model)

	// Press a non-navigation key on dashboard
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	um := updated.(Model)
	if um.view != ViewDashboard {
		t.Errorf("non-navigation key should keep dashboard view, got %d", um.view)
	}
}

// newNavTestProject creates a project directory with a migrated SLB database
// holding one pending request, and returns the project path and request ID.
func newNavTestProject(t *testing.T) (string, string) {
	t.Helper()

	projectPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectPath, ".slb"), 0o755); err != nil {
		t.Fatalf("mkdir .slb: %v", err)
	}
	database, err := db.OpenAndMigrate(filepath.Join(projectPath, ".slb", "state.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	sess := &db.Session{
		ID:          "sess-nav-test",
		AgentName:   "nav-agent",
		Program:     "nav-program",
		Model:       "nav-model",
		ProjectPath: projectPath,
	}
	if err := database.CreateSession(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	exp := time.Now().Add(30 * time.Minute)
	req := &db.Request{
		ID:                 "req-nav-test",
		ProjectPath:        projectPath,
		Command:            db.CommandSpec{Raw: "rm -rf ./data", Cwd: projectPath, Shell: true},
		RiskTier:           db.RiskTierDangerous,
		RequestorSessionID: sess.ID,
		RequestorAgent:     sess.AgentName,
		RequestorModel:     sess.Model,
		Justification:      db.Justification{Reason: "navigation regression test"},
		Status:             db.StatusPending,
		MinApprovals:       1,
		ExpiresAt:          &exp,
	}
	if err := database.CreateRequest(req); err != nil {
		t.Fatalf("create request: %v", err)
	}
	return projectPath, req.ID
}

// runCmdInto executes a command the way the Bubble Tea runtime would and
// feeds each resulting message back into the model. Batched commands run
// concurrently; anything that has not produced a message within the timeout
// (such as the periodic refresh tick) is left alone.
func runCmdInto(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		return m
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		next, _ := m.Update(msg)
		return next.(Model)
	}
	results := make(chan tea.Msg, len(batch))
	launched := 0
	for _, c := range batch {
		if c == nil {
			continue
		}
		launched++
		go func(c tea.Cmd) { results <- c() }(c)
	}
	deadline := time.After(2 * time.Second)
	for i := 0; i < launched; i++ {
		select {
		case got := <-results:
			if got == nil {
				continue
			}
			next, _ := m.Update(got)
			m = next.(Model)
		case <-deadline:
			return m
		}
	}
	return m
}

// Regression test for the "Loading..." wedge: a request detail view opened
// with Enter after startup never receives the initial tea.WindowSizeMsg, so
// it must be laid out from the root model's known size instead.
func TestEnterOnPendingRequestRendersDetailWithoutResize(t *testing.T) {
	projectPath, reqID := newNavTestProject(t)

	m := NewWithOptions(Options{ProjectPath: projectPath, RefreshInterval: 5})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = updated.(Model)

	// Load dashboard data exactly as the runtime would from Init.
	m = runCmdInto(t, m, m.Init())
	if got := m.dashboard.SelectedRequestID(); got != reqID {
		t.Fatalf("dashboard should select the pending request, got %q", got)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if m.view != ViewRequestDetail {
		t.Fatalf("expected ViewRequestDetail, got %d", m.view)
	}

	view := m.View()
	if strings.Contains(view, "Loading...") {
		t.Fatal("detail view is stuck at Loading... without a resize")
	}
	if !strings.Contains(view, "rm -rf ./data") {
		t.Errorf("detail view should render the request command, got:\n%s", view)
	}
	if m.detail.Width != 120 || m.detail.Height != 40 {
		t.Errorf("detail model should inherit the root size, got %dx%d", m.detail.Width, m.detail.Height)
	}

	// Escape must return to a dashboard that is itself renderable, not a
	// re-created dashboard waiting for a size message of its own.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.view != ViewDashboard {
		t.Fatalf("expected ViewDashboard after esc, got %d", m.view)
	}
	if view := m.View(); strings.Contains(view, "Loading...") {
		t.Fatal("dashboard re-created on esc is stuck at Loading...")
	}
}

// History and patterns views are created on navigation too and share the
// same size gate, so they must also be seeded with the known size.
func TestNavigationSeedsSizeIntoHistoryAndPatterns(t *testing.T) {
	projectPath, _ := newNavTestProject(t)

	m := NewWithOptions(Options{ProjectPath: projectPath})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(Model)

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'H'}})
	m = updated.(Model)
	if m.view != ViewHistory {
		t.Fatalf("expected ViewHistory, got %d", m.view)
	}
	if strings.Contains(m.View(), "Loading...") {
		t.Error("history view created on navigation is stuck at Loading...")
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	m = updated.(Model)
	if m.view != ViewPatterns {
		t.Fatalf("expected ViewPatterns, got %d", m.view)
	}
	if strings.Contains(m.View(), "Loading...") {
		t.Error("patterns view created on navigation is stuck at Loading...")
	}
}

// Before any size is known the seeding is a no-op and navigation still works.
func TestNavigationWithoutKnownSizeDoesNotPanic(t *testing.T) {
	m := New()
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'H'}})
	um := updated.(Model)
	if um.view != ViewHistory {
		t.Fatalf("expected ViewHistory, got %d", um.view)
	}
	if um.history.View() == "" {
		t.Error("history view should still render something")
	}
}
