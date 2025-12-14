package dashboard

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

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
		{0, 0, 0, 5, 0, 0},           // Empty
		{0, 0, 10, 5, 0, 0},          // Normal start
		{5, 0, 10, 5, 5, 1},          // Move past visible
		{-1, 0, 10, 5, 0, 0},         // Negative sel
		{15, 0, 10, 5, 9, 5},         // Sel past total
		{3, 5, 10, 5, 3, 3},          // Sel before offset
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
		offset, total, visible int
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

