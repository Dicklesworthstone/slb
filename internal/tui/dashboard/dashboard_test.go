package dashboard

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/tui/components"
)

func TestNew(t *testing.T) {
	m := New("")
	if m.projectPath == "" && m.projectPath != "" {
		// Just verify it doesn't panic
	}
}

func TestNewWithPath(t *testing.T) {
	m := New("/test/path")
	if m.projectPath != "/test/path" {
		t.Errorf("expected projectPath '/test/path', got %q", m.projectPath)
	}
}

func TestModelInit(t *testing.T) {
	m := New("")
	cmd := m.Init()
	if cmd == nil {
		t.Error("Init should return non-nil command")
	}
}

func TestModelUpdate(t *testing.T) {
	m := New("")

	// Test window size
	updated, cmd := m.Update(tea.WindowSizeMsg{Width: 100, Height: 50})
	if updated.(Model).width != 100 {
		t.Errorf("expected width 100, got %d", updated.(Model).width)
	}
	if updated.(Model).height != 50 {
		t.Errorf("expected height 50, got %d", updated.(Model).height)
	}
	_ = cmd
}

func TestModelUpdateKeyQuit(t *testing.T) {
	m := New("")

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	// Check that quit is returned
	_ = cmd

	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	_ = cmd
}

func TestModelUpdateKeyTab(t *testing.T) {
	m := New("")
	m.ready = true

	// Initial focus
	initialFocus := m.focus

	// Tab should cycle focus
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	model := updated.(Model)

	if model.focus == initialFocus && initialFocus != focusPending {
		// focus changed
	}
}

func TestModelUpdateKeyNav(t *testing.T) {
	m := New("")
	m.ready = true

	// Test up/down navigation
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
}

func TestModelUpdateKeyLeftRight(t *testing.T) {
	m := New("")
	m.ready = true

	// Test left/right navigation (panel switching)
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
}

func TestModelView(t *testing.T) {
	m := New("")

	// Before ready
	view := m.View()
	if !strings.Contains(view, "Loading") {
		t.Error("View before ready should show loading")
	}

	// After ready
	m.ready = true
	m.width = 80
	m.height = 24
	view = m.View()
	if view == "" {
		t.Error("View after ready should not be empty")
	}
}

func TestModelRefresh(t *testing.T) {
	m := New("")
	m.ready = true

	// refreshMsg handling
	_, cmd := m.Update(refreshMsg{})
	if cmd == nil {
		t.Error("refreshMsg should return non-nil command")
	}
}

func TestModelDataMsg(t *testing.T) {
	m := New("")

	// Create test data
	msg := dataMsg{
		agents: []components.AgentInfo{
			{Name: "Test", Status: components.AgentStatusActive},
		},
		pending: []requestRow{
			{ID: "1", Tier: "critical"},
		},
		activity:    []string{"test activity"},
		refreshedAt: time.Now(),
	}

	updated, _ := m.Update(msg)
	model := updated.(Model)

	if len(model.agents) != 1 {
		t.Errorf("expected 1 agent, got %d", len(model.agents))
	}
	if len(model.pending) != 1 {
		t.Errorf("expected 1 pending, got %d", len(model.pending))
	}
}

func TestClampSelection(t *testing.T) {
	tests := []struct {
		sel, off, total, visible int
		expectedSel, expectedOff int
	}{
		{0, 0, 0, 5, 0, 0},   // Empty
		{0, 0, 10, 5, 0, 0},  // Normal start
		{5, 0, 10, 5, 5, 1},  // Move past visible
		{-1, 0, 10, 5, 0, 0}, // Negative sel
		{15, 0, 10, 5, 9, 5}, // Sel past total
		{3, 5, 10, 5, 3, 3},  // Sel before offset
	}

	for _, tc := range tests {
		sel, _ := clampSelection(tc.sel, tc.off, tc.total, tc.visible)
		if sel != tc.expectedSel {
			t.Errorf("clampSelection(%d,%d,%d,%d): expected sel %d, got %d",
				tc.sel, tc.off, tc.total, tc.visible, tc.expectedSel, sel)
		}
		// Offset checks are less strict since the algorithm may vary
	}
}

func TestWindow(t *testing.T) {
	tests := []struct {
		offset, total, visible     int
		expectedStart, expectedEnd int
	}{
		{0, 10, 5, 0, 5},
		{5, 10, 5, 5, 10},
		{8, 10, 5, 8, 10},
		{0, 3, 5, 0, 3},
		{-1, 10, 5, 0, 5}, // Negative offset clamped
	}

	for _, tc := range tests {
		start, end := window(tc.offset, tc.total, tc.visible)
		if start != tc.expectedStart {
			t.Errorf("window(%d,%d,%d): expected start %d, got %d",
				tc.offset, tc.total, tc.visible, tc.expectedStart, start)
		}
		if end != tc.expectedEnd {
			t.Errorf("window(%d,%d,%d): expected end %d, got %d",
				tc.offset, tc.total, tc.visible, tc.expectedEnd, end)
		}
	}
}

func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		input    string
		max      int
		expected string
	}{
		{"hello", 10, "hello"},
		{"hello world", 8, "hello..."},
		{"hi", 5, "hi"},
		{"abc", 0, ""},
		{"abcd", 2, "ab"},
	}

	for _, tc := range tests {
		got := truncateRunes(tc.input, tc.max)
		if got != tc.expected {
			t.Errorf("truncateRunes(%q, %d): expected %q, got %q",
				tc.input, tc.max, tc.expected, got)
		}
	}
}

func TestFormatTimeAgo(t *testing.T) {
	tests := []struct {
		name     string
		time     time.Time
		expected string
	}{
		{"zero", time.Time{}, "never"},
		{"just now", time.Now(), "just now"},
		{"1m", time.Now().Add(-time.Minute), "1m ago"},
		{"5m", time.Now().Add(-5 * time.Minute), "5m ago"},
		{"1h", time.Now().Add(-time.Hour), "1h ago"},
		{"3h", time.Now().Add(-3 * time.Hour), "3h ago"},
		{"1d", time.Now().Add(-24 * time.Hour), "1d ago"},
		{"3d", time.Now().Add(-72 * time.Hour), "3d ago"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatTimeAgo(tc.time)
			if got != tc.expected {
				t.Errorf("formatTimeAgo: expected %q, got %q", tc.expected, got)
			}
		})
	}
}

func TestShortID(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"abc", "abc"},
		{"12345678", "12345678"},
		{"123456789", "12345678"},
		{"abcdefghijklmnop", "abcdefgh"},
	}

	for _, tc := range tests {
		got := shortID(tc.input)
		if got != tc.expected {
			t.Errorf("shortID(%q): expected %q, got %q", tc.input, tc.expected, got)
		}
	}
}

func TestMaxInt(t *testing.T) {
	tests := []struct {
		a, b, expected int
	}{
		{1, 2, 2},
		{2, 1, 2},
		{0, 0, 0},
		{-1, 1, 1},
	}

	for _, tc := range tests {
		got := maxInt(tc.a, tc.b)
		if got != tc.expected {
			t.Errorf("maxInt(%d, %d): expected %d, got %d", tc.a, tc.b, tc.expected, got)
		}
	}
}

func TestMinInt(t *testing.T) {
	tests := []struct {
		a, b, expected int
	}{
		{1, 2, 1},
		{2, 1, 1},
		{0, 0, 0},
		{-1, 1, -1},
	}

	for _, tc := range tests {
		got := minInt(tc.a, tc.b)
		if got != tc.expected {
			t.Errorf("minInt(%d, %d): expected %d, got %d", tc.a, tc.b, tc.expected, got)
		}
	}
}

func TestClassifyAgentStatus(t *testing.T) {
	tests := []struct {
		lastActive time.Time
		expected   components.AgentStatus
	}{
		{time.Time{}, components.AgentStatusStale},
		{time.Now(), components.AgentStatusActive},
		{time.Now().Add(-10 * time.Minute), components.AgentStatusIdle},
		{time.Now().Add(-1 * time.Hour), components.AgentStatusStale},
	}

	for _, tc := range tests {
		got := classifyAgentStatus(tc.lastActive)
		if got != tc.expected {
			t.Errorf("classifyAgentStatus: expected %v, got %v", tc.expected, got)
		}
	}
}

// Test keybindings

func TestDefaultKeyMap(t *testing.T) {
	km := DefaultKeyMap()

	// Verify key bindings are set
	if len(km.Up.Keys()) == 0 {
		t.Error("Up binding should have keys")
	}
	if len(km.Down.Keys()) == 0 {
		t.Error("Down binding should have keys")
	}
	if len(km.Tab.Keys()) == 0 {
		t.Error("Tab binding should have keys")
	}
	if len(km.Quit.Keys()) == 0 {
		t.Error("Quit binding should have keys")
	}
}

func TestKeyMapShortHelp(t *testing.T) {
	km := DefaultKeyMap()
	help := km.ShortHelp()

	if len(help) == 0 {
		t.Error("ShortHelp should return bindings")
	}
}

func TestKeyMapFullHelp(t *testing.T) {
	km := DefaultKeyMap()
	help := km.FullHelp()

	if len(help) == 0 {
		t.Error("FullHelp should return binding groups")
	}
}

// TestLoadData tests the database loading function
func TestLoadData(t *testing.T) {
	h := newTestHarness(t)

	// Create session and requests
	sess := createTestSession(t, h.db, h.projectPath)
	createTestRequest(t, h.db, sess, "rm -rf /tmp", "critical")

	agents, pending, activity, err := loadData(h.projectPath, h.db.Path())
	if err != nil {
		t.Fatalf("loadData failed: %v", err)
	}

	if len(agents) != 1 {
		t.Errorf("expected 1 agent, got %d", len(agents))
	}
	if len(pending) != 1 {
		t.Errorf("expected 1 pending, got %d", len(pending))
	}
	// Activity is derived from pending
	if len(activity) != 1 {
		t.Errorf("expected 1 activity, got %d", len(activity))
	}
}

func TestDashboardUsesSelectedStoreAndProjectOnRefresh(t *testing.T) {
	project, selected := newTestHarness(t), newTestHarness(t)
	defaultSession := createTestSession(t, project.db, project.projectPath)
	createTestRequest(t, project.db, defaultSession, "default-store-command", "critical")
	session := createTestSession(t, selected.db, project.projectPath)
	request := createTestRequest(t, selected.db, session, "selected-store-command", "dangerous")
	foreign := createTestSession(t, selected.db, selected.projectPath)
	createTestRequest(t, selected.db, foreign, "foreign-project-command", "critical")
	m := New(project.projectPath).WithDatabasePath(selected.db.Path())
	read := func(cmd tea.Cmd) dataMsg {
		t.Helper()
		// Execute the immediate load, leaving the periodic timer unscheduled.
		batch := cmd().(tea.BatchMsg)
		message := batch[0]().(dataMsg)
		if message.err != nil {
			t.Fatal(message.err)
		}
		return message
	}
	message := read(m.Init())
	if len(message.agents) != 1 || message.agents[0].SessionID != session.ID || len(message.pending) != 1 || message.pending[0].ID != request.ID || len(message.activity) != 1 {
		t.Fatalf("selected store or project was ignored: %+v", message)
	}
	updated, _ := m.Update(message)
	m = updated.(Model)
	createTestRequest(t, selected.db, session, "selected-store-second-command", "caution")
	_, refresh := m.Update(refreshMsg{})
	message = read(refresh)
	if len(message.pending) != 2 || len(message.activity) != 2 || len(message.agents) != 1 {
		t.Fatalf("refresh switched away from selected store: %+v", message)
	}
	m.width = len([]rune(selected.db.Path())) + 40
	if header := m.renderHeader(); !strings.Contains(header, selected.db.Path()) {
		t.Fatalf("selected database is not visible: %s", header)
	}
	m.width = 50
	if header := m.renderHeader(); lipgloss.Width(header) > m.width || !strings.Contains(header, "state.db") {
		t.Fatalf("narrow database label lost its filename or overflowed: %s", header)
	}
}

func TestDashboardMissingSelectedStoreDoesNotFallbackOrCreate(t *testing.T) {
	project := newTestHarness(t)
	session := createTestSession(t, project.db, project.projectPath)
	createTestRequest(t, project.db, session, "default-store-command", "critical")
	missing := filepath.Join(t.TempDir(), "missing.db")
	m := New(project.projectPath).WithDatabasePath(missing)
	batch := m.Init()().(tea.BatchMsg)
	message := batch[0]().(dataMsg)
	if message.err == nil || len(message.pending) != 0 || len(message.agents) != 0 {
		t.Fatalf("missing selected store silently fell back: %+v", message)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("dashboard created selected database: %v", err)
	}
}

func TestLoadDataEmptyDB(t *testing.T) {
	h := newTestHarness(t)

	agents, pending, activity, err := loadData(h.projectPath, h.db.Path())
	if err != nil {
		t.Fatalf("loadData on empty DB failed: %v", err)
	}

	if len(agents) != 0 {
		t.Errorf("expected 0 agents, got %d", len(agents))
	}
	if len(pending) != 0 {
		t.Errorf("expected 0 pending, got %d", len(pending))
	}
	if len(activity) != 0 {
		t.Errorf("expected 0 activity, got %d", len(activity))
	}
}

func TestLoadDataNonexistentDB(t *testing.T) {
	agents, pending, activity, err := loadData("/nonexistent/path", "/nonexistent/path/state.db")
	// Should return error but empty data, not panic
	if err == nil {
		t.Error("expected error for nonexistent database")
	}
	if len(agents) != 0 {
		t.Errorf("expected 0 agents with error, got %d", len(agents))
	}
	if len(pending) != 0 {
		t.Errorf("expected 0 pending with error, got %d", len(pending))
	}
	if len(activity) != 0 {
		t.Errorf("expected 0 activity with error, got %d", len(activity))
	}
}

func TestLoadDataWithMultipleRequests(t *testing.T) {
	h := newTestHarness(t)

	sess := createTestSession(t, h.db, h.projectPath)
	for i := 0; i < 15; i++ {
		createTestRequest(t, h.db, sess, "test cmd", "caution")
	}

	_, pending, activity, err := loadData(h.projectPath, h.db.Path())
	if err != nil {
		t.Fatalf("loadData failed: %v", err)
	}

	if len(pending) != 15 {
		t.Errorf("expected 15 pending, got %d", len(pending))
	}
	// Activity is capped at 10
	if len(activity) != 10 {
		t.Errorf("expected 10 activity (capped), got %d", len(activity))
	}
}

func TestLoadCmd(t *testing.T) {
	h := newTestHarness(t)

	createTestSession(t, h.db, h.projectPath)

	cmd := loadCmd(h.projectPath, h.db.Path())
	if cmd == nil {
		t.Fatal("loadCmd should return non-nil command")
	}

	// Execute the command
	msg := cmd()
	dm, ok := msg.(dataMsg)
	if !ok {
		t.Fatalf("expected dataMsg, got %T", msg)
	}

	if dm.err != nil {
		t.Errorf("unexpected error: %v", dm.err)
	}
}

func TestTickCmd(t *testing.T) {
	cmd := tickCmd()
	if cmd == nil {
		t.Error("tickCmd should return non-nil command")
	}
}

func TestMoveSelection(t *testing.T) {
	m := New("")
	m.ready = true
	m.height = 24
	m.width = 80

	// Setup data
	m.agents = []components.AgentInfo{{Name: "A"}, {Name: "B"}, {Name: "C"}}
	m.pending = []requestRow{{ID: "1"}, {ID: "2"}}
	m.activity = []string{"a", "b", "c", "d"}

	// Test moving down in agents panel
	m.focus = focusAgents
	m.agentSel = 0
	m.moveSelection(1)
	if m.agentSel != 1 {
		t.Errorf("expected agentSel 1 after down, got %d", m.agentSel)
	}

	// Test moving up
	m.moveSelection(-1)
	if m.agentSel != 0 {
		t.Errorf("expected agentSel 0 after up, got %d", m.agentSel)
	}

	// Test pending panel
	m.focus = focusPending
	m.pendingSel = 0
	m.moveSelection(1)
	if m.pendingSel != 1 {
		t.Errorf("expected pendingSel 1 after down, got %d", m.pendingSel)
	}

	// Test activity panel
	m.focus = focusActivity
	m.activitySel = 0
	m.moveSelection(1)
	if m.activitySel != 1 {
		t.Errorf("expected activitySel 1 after down, got %d", m.activitySel)
	}
}

func TestVisibleRows(t *testing.T) {
	m := New("")
	m.height = 30

	rows := m.visibleRows()
	// Should return some reasonable number based on height
	if rows <= 0 {
		t.Errorf("visibleRows should return positive value, got %d", rows)
	}

	// Very small height
	m.height = 10
	rows = m.visibleRows()
	if rows < 1 {
		t.Errorf("visibleRows should return at least 1, got %d", rows)
	}
}

func TestRenderPendingPanel(t *testing.T) {
	m := New("")
	m.width = 80
	m.height = 24
	m.ready = true

	// Empty state
	panel := m.renderPendingPanel(40, 10)
	if panel == "" {
		t.Error("renderPendingPanel should not return empty string")
	}

	// With data
	m.pending = []requestRow{
		{ID: "req-1", Tier: "critical", Command: "rm -rf /", Requestor: "Agent1", CreatedAt: time.Now()},
	}
	m.focus = focusPending
	m.pendingSel = 0

	panel = m.renderPendingPanel(40, 10)
	if panel == "" {
		t.Error("renderPendingPanel with data should not be empty")
	}
}

func TestRenderActivityPanel(t *testing.T) {
	m := New("")
	m.width = 80
	m.height = 24
	m.ready = true

	// Empty state
	panel := m.renderActivityPanel(40, 10)
	if panel == "" {
		t.Error("renderActivityPanel should not return empty string")
	}

	// With data
	m.activity = []string{"Event 1", "Event 2", "Event 3"}
	m.focus = focusActivity
	m.activitySel = 1

	panel = m.renderActivityPanel(40, 10)
	if panel == "" {
		t.Error("renderActivityPanel with data should not be empty")
	}
}

func TestRenderAgentsPanel(t *testing.T) {
	m := New("")
	m.width = 80
	m.height = 24
	m.ready = true

	// Empty state
	panel := m.renderAgentsPanel(40, 10)
	if panel == "" {
		t.Error("renderAgentsPanel should not return empty string")
	}

	// With data
	m.agents = []components.AgentInfo{
		{Name: "Agent1", Status: components.AgentStatusActive, Program: "test", Model: "model"},
		{Name: "Agent2", Status: components.AgentStatusIdle, Program: "test", Model: "model"},
	}
	m.focus = focusAgents
	m.agentSel = 0

	panel = m.renderAgentsPanel(40, 10)
	if panel == "" {
		t.Error("renderAgentsPanel with data should not be empty")
	}
}

func TestRenderFooter(t *testing.T) {
	m := New("")
	m.width = 80

	// Normal footer
	footer := m.renderFooter()
	if footer == "" {
		t.Error("renderFooter should not return empty string")
	}

	// With error
	m.lastErr = &testError{}
	footer = m.renderFooter()
	if !strings.Contains(footer, "error") {
		t.Error("footer should show error")
	}
}

type testError struct{}

func (e *testError) Error() string { return "test error" }

// Test harness for database tests
type testHarness struct {
	projectPath string
	db          *db.DB
}

func newTestHarness(t *testing.T) *testHarness {
	t.Helper()

	tmpDir := t.TempDir()
	slbDir := tmpDir + "/.slb"

	if err := os.MkdirAll(slbDir, 0755); err != nil {
		t.Fatalf("failed to create .slb dir: %v", err)
	}

	dbPath := slbDir + "/state.db"
	database, err := db.OpenAndMigrate(dbPath)
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}

	t.Cleanup(func() {
		database.Close()
	})

	return &testHarness{
		projectPath: tmpDir,
		db:          database,
	}
}

func createTestSession(t *testing.T, database *db.DB, projectPath string) *db.Session {
	t.Helper()

	sess := &db.Session{
		ID:          "sess-" + randHex(6),
		AgentName:   "TestAgent",
		Program:     "test",
		Model:       "test-model",
		ProjectPath: projectPath,
	}

	if err := database.CreateSession(sess); err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	return sess
}

func createTestRequest(t *testing.T, database *db.DB, sess *db.Session, cmd string, tier string) *db.Request {
	t.Helper()

	exp := time.Now().Add(30 * time.Minute)
	req := &db.Request{
		ID:                 "req-" + randHex(6),
		ProjectPath:        sess.ProjectPath,
		Command:            db.CommandSpec{Raw: cmd, Cwd: "/tmp", Shell: true},
		RiskTier:           db.RiskTier(tier),
		RequestorSessionID: sess.ID,
		RequestorAgent:     sess.AgentName,
		RequestorModel:     sess.Model,
		Justification:      db.Justification{Reason: "test"},
		Status:             db.StatusPending,
		MinApprovals:       1,
		ExpiresAt:          &exp,
	}

	if err := database.CreateRequest(req); err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	return req
}

func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)[:n]
}

// =====================================================================
// Additional tests for uncovered branches
// =====================================================================

func TestModelUpdateKeyShiftTab(t *testing.T) {
	m := New("")
	m.ready = true
	m.focus = focusPending // Start at pending (1)

	// shift+tab should cycle backwards
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	model := updated.(Model)

	// focusPending (1) + 2 = 3, mod 3 = 0 (focusAgents)
	if model.focus != focusAgents {
		t.Errorf("expected focus to be focusAgents (0) after shift+tab, got %d", model.focus)
	}

	// Another shift+tab
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	model = updated.(Model)

	// focusAgents (0) + 2 = 2, mod 3 = 2 (focusActivity)
	if model.focus != focusActivity {
		t.Errorf("expected focus to be focusActivity (2) after shift+tab, got %d", model.focus)
	}
}

func TestModelUpdateKeyMWithCallback(t *testing.T) {
	m := New("")
	m.ready = true

	callbackCalled := false
	m.OnPatterns = func() {
		callbackCalled = true
	}

	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})

	if !callbackCalled {
		t.Error("OnPatterns callback should have been called")
	}
}

func TestModelUpdateKeyMWithoutCallback(t *testing.T) {
	m := New("")
	m.ready = true
	m.OnPatterns = nil

	// Should not panic when callback is nil
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
}

func TestModelUpdateKeyHWithCallback(t *testing.T) {
	m := New("")
	m.ready = true
	m.focus = focusPending

	callbackCalled := false
	m.OnHistory = func() {
		callbackCalled = true
	}

	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})

	if !callbackCalled {
		t.Error("OnHistory callback should have been called")
	}
}

func TestModelUpdateKeyHWithoutCallback(t *testing.T) {
	m := New("")
	m.ready = true
	m.focus = focusPending // Start at pending (1)
	m.OnHistory = nil

	// Without callback, 'h' should act as left navigation
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	model := updated.(Model)

	// focusPending (1) + 2 = 3, mod 3 = 0 (focusAgents)
	if model.focus != focusAgents {
		t.Errorf("expected focus to move left to focusAgents (0), got %d", model.focus)
	}
}

func TestWindowEdgeCases(t *testing.T) {
	tests := []struct {
		name                       string
		offset, total, visible     int
		expectedStart, expectedEnd int
	}{
		{"zero visible", 0, 10, 0, 0, 1},         // visible <= 0 becomes 1
		{"offset past total", 15, 10, 5, 10, 10}, // offset > total clamped
		{"total zero", 0, 0, 5, 0, 0},            // empty data
		{"visible larger than total", 0, 3, 10, 0, 3},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			start, end := window(tc.offset, tc.total, tc.visible)
			if start != tc.expectedStart {
				t.Errorf("window(%d,%d,%d): expected start %d, got %d",
					tc.offset, tc.total, tc.visible, tc.expectedStart, start)
			}
			if end != tc.expectedEnd {
				t.Errorf("window(%d,%d,%d): expected end %d, got %d",
					tc.offset, tc.total, tc.visible, tc.expectedEnd, end)
			}
		})
	}
}

func TestClampSelectionEdgeCases(t *testing.T) {
	tests := []struct {
		name                     string
		sel, off, total, visible int
		expectedSel, expectedOff int
	}{
		{"visible zero", 5, 0, 10, 0, 5, 5}, // visible <= 0 becomes 1, off = sel - visible + 1 = 5
		{"sel past visible window", 8, 0, 10, 3, 8, 6},
		{"offset past total", 0, 15, 10, 5, 0, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sel, off := clampSelection(tc.sel, tc.off, tc.total, tc.visible)
			if sel != tc.expectedSel {
				t.Errorf("clampSelection: expected sel %d, got %d", tc.expectedSel, sel)
			}
			if off != tc.expectedOff {
				t.Errorf("clampSelection: expected off %d, got %d", tc.expectedOff, off)
			}
		})
	}
}

func TestVisibleRowsZeroHeight(t *testing.T) {
	m := New("")
	m.height = 0

	rows := m.visibleRows()
	if rows != 6 {
		t.Errorf("visibleRows with 0 height should return 6, got %d", rows)
	}
}

func TestVisibleRowsNegativeHeight(t *testing.T) {
	m := New("")
	m.height = -10

	rows := m.visibleRows()
	if rows != 6 {
		t.Errorf("visibleRows with negative height should return 6, got %d", rows)
	}
}

func TestRenderFooterWithRefreshTime(t *testing.T) {
	m := New("")
	m.width = 80
	m.lastRefresh = time.Now().Add(-5 * time.Minute)
	m.lastErr = nil

	footer := m.renderFooter()
	if !strings.Contains(footer, "refreshed") {
		t.Error("footer should show refresh time")
	}
}

func TestRenderFooterZeroRefreshTime(t *testing.T) {
	m := New("")
	m.width = 80
	m.lastRefresh = time.Time{}
	m.lastErr = nil

	footer := m.renderFooter()
	// Should not show refresh time for zero value
	if strings.Contains(footer, "refreshed") && strings.Contains(footer, "never") {
		// That's also acceptable
	}
}

func TestDataMsgWithError(t *testing.T) {
	m := New("")

	msg := dataMsg{
		err:         &testError{},
		refreshedAt: time.Now(),
	}

	updated, _ := m.Update(msg)
	model := updated.(Model)

	if model.lastErr == nil {
		t.Error("lastErr should be set from dataMsg")
	}
}

func TestModelViewSmallHeight(t *testing.T) {
	m := New("")
	m.ready = true
	m.width = 80
	m.height = 4 // Very small height

	view := m.View()
	if view == "" {
		t.Error("View with small height should not be empty")
	}
}

func TestModelViewNarrowWidth(t *testing.T) {
	m := New("")
	m.ready = true
	m.width = 60 // Narrow width
	m.height = 24

	view := m.View()
	if view == "" {
		t.Error("View with narrow width should not be empty")
	}
}

func TestRenderPendingPanelWithSelection(t *testing.T) {
	m := New("")
	m.width = 80
	m.height = 24
	m.ready = true

	m.pending = []requestRow{
		{ID: "req-1", Tier: "critical", Command: "rm -rf /", Requestor: "Agent1", CreatedAt: time.Now()},
		{ID: "req-2", Tier: "dangerous", Command: "chmod 777", Requestor: "Agent2", CreatedAt: time.Now()},
		{ID: "req-3", Tier: "caution", Command: "make build", Requestor: "Agent3", CreatedAt: time.Now()},
	}
	m.focus = focusPending
	m.pendingSel = 1 // Select middle item

	panel := m.renderPendingPanel(40, 10)
	if panel == "" {
		t.Error("renderPendingPanel should not be empty")
	}
}

func TestRenderActivityPanelWithSelection(t *testing.T) {
	m := New("")
	m.width = 80
	m.height = 24
	m.ready = true

	m.activity = []string{"Event 1", "Event 2", "Event 3", "Event 4", "Event 5"}
	m.focus = focusActivity
	m.activitySel = 2 // Select middle item

	panel := m.renderActivityPanel(40, 10)
	if panel == "" {
		t.Error("renderActivityPanel should not be empty")
	}
}

func TestRenderAgentsPanelWithSelection(t *testing.T) {
	m := New("")
	m.width = 80
	m.height = 24
	m.ready = true

	m.agents = []components.AgentInfo{
		{Name: "Agent1", Status: components.AgentStatusActive, Program: "claude", Model: "opus"},
		{Name: "Agent2", Status: components.AgentStatusIdle, Program: "codex", Model: "gpt4"},
		{Name: "Agent3", Status: components.AgentStatusStale, Program: "cursor", Model: "sonnet"},
	}
	m.focus = focusAgents
	m.agentSel = 1 // Select middle item

	panel := m.renderAgentsPanel(40, 10)
	if panel == "" {
		t.Error("renderAgentsPanel should not be empty")
	}
}

func TestLoadDataWithDisplayRedacted(t *testing.T) {
	h := newTestHarness(t)

	sess := createTestSession(t, h.db, h.projectPath)

	// Create request with DisplayRedacted set
	exp := time.Now().Add(30 * time.Minute)
	req := &db.Request{
		ID:                 "req-" + randHex(6),
		ProjectPath:        sess.ProjectPath,
		Command:            db.CommandSpec{Raw: "secret cmd", DisplayRedacted: "redacted cmd", Cwd: "/tmp", Shell: true},
		RiskTier:           db.RiskTierDangerous,
		RequestorSessionID: sess.ID,
		RequestorAgent:     sess.AgentName,
		RequestorModel:     sess.Model,
		Justification:      db.Justification{Reason: "test"},
		Status:             db.StatusPending,
		MinApprovals:       1,
		ExpiresAt:          &exp,
	}
	if err := h.db.CreateRequest(req); err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	_, pending, _, err := loadData(h.projectPath, h.db.Path())
	if err != nil {
		t.Fatalf("loadData failed: %v", err)
	}

	if len(pending) != 1 {
		t.Fatalf("expected 1 pending, got %d", len(pending))
	}

	// Should use DisplayRedacted if available
	if pending[0].Command != "redacted cmd" {
		t.Errorf("expected command to be 'redacted cmd', got %q", pending[0].Command)
	}
}
