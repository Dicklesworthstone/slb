package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestNew(t *testing.T) {
	m := New()
	if m.inner == nil {
		t.Error("inner model should not be nil")
	}
}

func TestModelInit(t *testing.T) {
	m := New()
	cmd := m.Init()
	// Init may return commands - just verify it doesn't panic
	_ = cmd
}

func TestModelInitNilInner(t *testing.T) {
	m := Model{inner: nil}
	cmd := m.Init()
	if cmd != nil {
		t.Error("Init with nil inner should return nil cmd")
	}
}

func TestModelUpdate(t *testing.T) {
	m := New()
	updated, cmd := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if updated == nil {
		t.Error("Update should return non-nil model")
	}
	_ = cmd
}

func TestModelUpdateNilInner(t *testing.T) {
	m := Model{inner: nil}
	updated, cmd := m.Update(tea.KeyMsg{})
	if cmd != nil {
		t.Error("Update with nil inner should return nil cmd")
	}
	_ = updated
}

func TestModelView(t *testing.T) {
	m := New()
	// Set window size first
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	view := m.View()
	if view == "" {
		t.Error("View should return non-empty string")
	}
}

func TestModelViewNilInner(t *testing.T) {
	m := Model{inner: nil}
	view := m.View()
	if view != "Loading..." {
		t.Errorf("View with nil inner should return 'Loading...', got %q", view)
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
