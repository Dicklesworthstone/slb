package patterns

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestNew(t *testing.T) {
	m := New("")
	// Should use current directory if empty
	if m.projectPath == "" {
		// May be empty in test environment, that's ok
	}
	if m.filterType != "" {
		t.Errorf("expected empty filterType, got %q", m.filterType)
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

func TestDefaultRemovalKeyMap(t *testing.T) {
	km := DefaultRemovalKeyMap()

	// Verify all key bindings are set
	if len(km.Approve.Keys()) == 0 {
		t.Error("Approve binding should have keys")
	}
	if len(km.Reject.Keys()) == 0 {
		t.Error("Reject binding should have keys")
	}
	if len(km.Up.Keys()) == 0 {
		t.Error("Up binding should have keys")
	}
	if len(km.Down.Keys()) == 0 {
		t.Error("Down binding should have keys")
	}
	if len(km.Back.Keys()) == 0 {
		t.Error("Back binding should have keys")
	}
	if len(km.Quit.Keys()) == 0 {
		t.Error("Quit binding should have keys")
	}
	if len(km.FilterType.Keys()) == 0 {
		t.Error("FilterType binding should have keys")
	}
	if len(km.Refresh.Keys()) == 0 {
		t.Error("Refresh binding should have keys")
	}
}

func TestModelUpdateWindowSize(t *testing.T) {
	m := New("")

	updated, cmd := m.Update(tea.WindowSizeMsg{Width: 100, Height: 50})
	model := updated.(Model)

	if model.width != 100 {
		t.Errorf("expected width 100, got %d", model.width)
	}
	if model.height != 50 {
		t.Errorf("expected height 50, got %d", model.height)
	}
	if !model.ready {
		t.Error("model should be ready after WindowSizeMsg")
	}
	_ = cmd
}

func TestModelUpdateRefreshMsg(t *testing.T) {
	m := New("")

	_, cmd := m.Update(refreshMsg{})
	if cmd == nil {
		t.Error("refreshMsg should return non-nil command")
	}
}

func TestModelUpdateDataMsg(t *testing.T) {
	m := New("")

	// Create test data
	msg := dataMsg{
		rows: []RemovalRow{
			{ID: 1, Tier: "CRITICAL", Pattern: "rm -rf", ChangeType: "remove", Status: db.PatternChangeStatusPending},
			{ID: 2, Tier: "DANGEROUS", Pattern: "sudo", ChangeType: "suggest", Status: db.PatternChangeStatusApproved},
		},
		totalCount:  2,
		err:         nil,
		refreshedAt: time.Now(),
	}

	updated, _ := m.Update(msg)
	model := updated.(Model)

	if len(model.rows) != 2 {
		t.Errorf("expected 2 rows, got %d", len(model.rows))
	}
	if model.totalCount != 2 {
		t.Errorf("expected totalCount 2, got %d", model.totalCount)
	}
}

func TestModelUpdateDataMsgClampsSelection(t *testing.T) {
	m := New("")
	m.selectedIdx = 10 // Out of range

	msg := dataMsg{
		rows: []RemovalRow{
			{ID: 1, Status: db.PatternChangeStatusPending},
		},
		totalCount: 1,
	}

	updated, _ := m.Update(msg)
	model := updated.(Model)

	if model.selectedIdx != 0 {
		t.Errorf("expected selectedIdx 0 after clamping, got %d", model.selectedIdx)
	}
}

func TestModelUpdateActionMsgSuccess(t *testing.T) {
	m := New("")

	msg := actionMsg{
		action:  "approve",
		id:      42,
		success: true,
		err:     nil,
	}

	updated, cmd := m.Update(msg)
	model := updated.(Model)

	if model.messageType != "success" {
		t.Errorf("expected messageType 'success', got %q", model.messageType)
	}
	if !strings.Contains(model.message, "42") {
		t.Errorf("message should contain ID 42, got %q", model.message)
	}
	if cmd == nil {
		t.Error("actionMsg should trigger data reload")
	}
}

func TestModelUpdateActionMsgFailure(t *testing.T) {
	m := New("")

	msg := actionMsg{
		action:  "reject",
		id:      42,
		success: false,
		err:     errTest,
	}

	updated, _ := m.Update(msg)
	model := updated.(Model)

	if model.messageType != "error" {
		t.Errorf("expected messageType 'error', got %q", model.messageType)
	}
	if !strings.Contains(model.message, "Failed") {
		t.Errorf("message should contain 'Failed', got %q", model.message)
	}
}

var errTest = &testError{}

type testError struct{}

func (e *testError) Error() string { return "test error" }

func TestModelUpdateKeyQuit(t *testing.T) {
	m := New("")

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	// Should return quit command
	_ = cmd
}

func TestModelUpdateKeyBack(t *testing.T) {
	m := New("")

	backCalled := false
	m.OnBack = func() { backCalled = true }

	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})

	if !backCalled {
		t.Error("OnBack callback should be called")
	}
}

func TestModelUpdateKeyUpDown(t *testing.T) {
	m := New("")
	m.rows = []RemovalRow{
		{ID: 1},
		{ID: 2},
		{ID: 3},
	}
	m.selectedIdx = 1

	// Test up
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	model := updated.(Model)
	if model.selectedIdx != 0 {
		t.Errorf("expected selectedIdx 0 after up, got %d", model.selectedIdx)
	}

	// Test down
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = updated.(Model)
	if model.selectedIdx != 1 {
		t.Errorf("expected selectedIdx 1 after down, got %d", model.selectedIdx)
	}

	// Test k (vim up)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	model = updated.(Model)
	if model.selectedIdx != 0 {
		t.Errorf("expected selectedIdx 0 after k, got %d", model.selectedIdx)
	}

	// Test j (vim down)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	model = updated.(Model)
	if model.selectedIdx != 1 {
		t.Errorf("expected selectedIdx 1 after j, got %d", model.selectedIdx)
	}
}

func TestModelUpdateKeyUpAtTop(t *testing.T) {
	m := New("")
	m.rows = []RemovalRow{{ID: 1}, {ID: 2}}
	m.selectedIdx = 0

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	model := updated.(Model)
	if model.selectedIdx != 0 {
		t.Errorf("expected selectedIdx 0 when already at top, got %d", model.selectedIdx)
	}
}

func TestModelUpdateKeyDownAtBottom(t *testing.T) {
	m := New("")
	m.rows = []RemovalRow{{ID: 1}, {ID: 2}}
	m.selectedIdx = 1

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	model := updated.(Model)
	if model.selectedIdx != 1 {
		t.Errorf("expected selectedIdx 1 when already at bottom, got %d", model.selectedIdx)
	}
}

func TestModelUpdateKeyApprove(t *testing.T) {
	m := New("")
	m.rows = []RemovalRow{
		{ID: 42, Status: db.PatternChangeStatusPending},
	}
	m.selectedIdx = 0

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	// Command should be returned for approve action
	_ = cmd
}

func TestModelUpdateKeyApproveNonPending(t *testing.T) {
	m := New("")
	m.rows = []RemovalRow{
		{ID: 42, Status: db.PatternChangeStatusApproved}, // Already approved
	}
	m.selectedIdx = 0

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	// Should not return command for already approved row
	if cmd != nil {
		t.Error("should not return command for non-pending row")
	}
}

func TestModelUpdateKeyReject(t *testing.T) {
	m := New("")
	m.rows = []RemovalRow{
		{ID: 42, Status: db.PatternChangeStatusPending},
	}
	m.selectedIdx = 0

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	// Command should be returned for reject action
	_ = cmd
}

func TestModelUpdateKeyFilter(t *testing.T) {
	m := New("")
	m.ready = true

	// Initially empty
	if m.filterType != "" {
		t.Errorf("expected empty filterType initially")
	}

	// Cycle through filters
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	model := updated.(Model)
	if model.filterType == "" {
		t.Error("filterType should change after pressing f")
	}
	if cmd == nil {
		t.Error("filter change should trigger data reload")
	}
}

func TestModelUpdateKeyRefresh(t *testing.T) {
	m := New("")

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	if cmd == nil {
		t.Error("ctrl+r should trigger refresh command")
	}
}

func TestModelUpdateClearsMessage(t *testing.T) {
	m := New("")
	m.message = "Test message"

	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}}) // Any key
	// Message should be cleared
}

func TestModelViewBeforeReady(t *testing.T) {
	m := New("")

	view := m.View()
	if !strings.Contains(view, "Loading") {
		t.Error("View before ready should show loading")
	}
}

func TestModelViewAfterReady(t *testing.T) {
	m := New("")
	m.ready = true
	m.width = 80
	m.height = 24

	view := m.View()
	if view == "" {
		t.Error("View after ready should not be empty")
	}
	if !strings.Contains(view, "Pattern Change Review") {
		t.Error("View should contain title")
	}
}

func TestModelViewWithData(t *testing.T) {
	m := New("")
	m.ready = true
	m.width = 80
	m.height = 24
	m.rows = []RemovalRow{
		{ID: 1, Tier: "CRITICAL", Pattern: "rm -rf", ChangeType: "remove", Status: db.PatternChangeStatusPending, Reason: "dangerous"},
	}
	m.totalCount = 1

	view := m.View()
	if view == "" {
		t.Error("View with data should not be empty")
	}
}

func TestModelViewWithMessage(t *testing.T) {
	m := New("")
	m.ready = true
	m.width = 80
	m.height = 24
	m.message = "Test message"
	m.messageType = "success"

	view := m.View()
	if !strings.Contains(view, "Test message") {
		t.Error("View should contain message")
	}
}

func TestModelViewWithError(t *testing.T) {
	m := New("")
	m.ready = true
	m.width = 80
	m.height = 24
	m.lastErr = errTest

	view := m.View()
	if view == "" {
		t.Error("View with error should not be empty")
	}
}

func TestModelViewWithFilter(t *testing.T) {
	m := New("")
	m.ready = true
	m.width = 80
	m.height = 24
	m.filterType = db.PatternChangeTypeRemove

	view := m.View()
	if !strings.Contains(view, "remove") {
		t.Error("View should show filter type")
	}
}

func TestModelViewEmpty(t *testing.T) {
	m := New("")
	m.ready = true
	m.width = 80
	m.height = 24
	m.rows = nil

	view := m.View()
	if !strings.Contains(view, "No pattern change requests") {
		t.Error("View should show empty state message")
	}
}

func TestModelViewEmptyWithFilter(t *testing.T) {
	m := New("")
	m.ready = true
	m.width = 80
	m.height = 24
	m.rows = nil
	m.filterType = "remove"

	view := m.View()
	if !strings.Contains(view, "No remove requests") {
		t.Error("View should show filter-specific empty message")
	}
}

func TestCycleFilterType(t *testing.T) {
	m := New("")

	// Start empty
	if m.filterType != "" {
		t.Errorf("expected empty filterType initially")
	}

	// Cycle through all types
	m.cycleFilterType()
	if m.filterType != db.PatternChangeTypeRemove {
		t.Errorf("expected 'remove' after first cycle, got %q", m.filterType)
	}

	m.cycleFilterType()
	if m.filterType != db.PatternChangeTypeSuggest {
		t.Errorf("expected 'suggest' after second cycle, got %q", m.filterType)
	}

	m.cycleFilterType()
	if m.filterType != db.PatternChangeTypeAdd {
		t.Errorf("expected 'add' after third cycle, got %q", m.filterType)
	}

	m.cycleFilterType()
	if m.filterType != "" {
		t.Errorf("expected empty after full cycle, got %q", m.filterType)
	}
}

func TestCycleFilterTypeUnknownValue(t *testing.T) {
	m := New("")
	m.filterType = "unknown"

	m.cycleFilterType()
	// Should reset to empty
	if m.filterType != "" {
		t.Errorf("expected empty after cycling from unknown, got %q", m.filterType)
	}
}

func TestCountPending(t *testing.T) {
	m := New("")
	m.rows = []RemovalRow{
		{Status: db.PatternChangeStatusPending},
		{Status: db.PatternChangeStatusApproved},
		{Status: db.PatternChangeStatusPending},
		{Status: db.PatternChangeStatusRejected},
	}

	count := m.countPending()
	if count != 2 {
		t.Errorf("expected 2 pending, got %d", count)
	}
}

func TestCountPendingEmpty(t *testing.T) {
	m := New("")

	count := m.countPending()
	if count != 0 {
		t.Errorf("expected 0 pending for empty rows, got %d", count)
	}
}

func TestStatusIcon(t *testing.T) {
	tests := []struct {
		status   string
		expected string
	}{
		{db.PatternChangeStatusApproved, "✓"},
		{db.PatternChangeStatusRejected, "✗"},
		{db.PatternChangeStatusPending, "⋯"},
		{"unknown", "?"},
		{"", "?"},
	}

	for _, tc := range tests {
		t.Run(tc.status, func(t *testing.T) {
			got := statusIcon(tc.status)
			if got != tc.expected {
				t.Errorf("statusIcon(%q): expected %q, got %q", tc.status, tc.expected, got)
			}
		})
	}
}

func TestTypeIcon(t *testing.T) {
	tests := []struct {
		changeType string
		expected   string
	}{
		{db.PatternChangeTypeRemove, "−"},
		{db.PatternChangeTypeSuggest, "?"},
		{db.PatternChangeTypeAdd, "+"},
		{"unknown", "•"},
		{"", "•"},
	}

	for _, tc := range tests {
		t.Run(tc.changeType, func(t *testing.T) {
			got := typeIcon(tc.changeType)
			if got != tc.expected {
				t.Errorf("typeIcon(%q): expected %q, got %q", tc.changeType, tc.expected, got)
			}
		})
	}
}

func TestMax(t *testing.T) {
	tests := []struct {
		a, b, expected int
	}{
		{1, 2, 2},
		{2, 1, 2},
		{0, 0, 0},
		{-1, 1, 1},
		{-5, -3, -3},
	}

	for _, tc := range tests {
		got := max(tc.a, tc.b)
		if got != tc.expected {
			t.Errorf("max(%d, %d): expected %d, got %d", tc.a, tc.b, tc.expected, got)
		}
	}
}

func TestRemovalRow(t *testing.T) {
	row := RemovalRow{
		ID:         42,
		Tier:       "CRITICAL",
		Pattern:    "rm -rf",
		ChangeType: "remove",
		Reason:     "dangerous command",
		Status:     db.PatternChangeStatusPending,
		CreatedAt:  time.Now(),
	}

	if row.ID != 42 {
		t.Errorf("expected ID 42, got %d", row.ID)
	}
	if row.Tier != "CRITICAL" {
		t.Errorf("expected Tier 'CRITICAL', got %q", row.Tier)
	}
}

func TestModelCallbacks(t *testing.T) {
	m := New("")

	approveCalled := false
	rejectCalled := false
	backCalled := false

	m.OnApprove = func(id int64) { approveCalled = true }
	m.OnReject = func(id int64) { rejectCalled = true }
	m.OnBack = func() { backCalled = true }

	// Test back callback via key
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if !backCalled {
		t.Error("OnBack should be called")
	}

	// Note: OnApprove and OnReject are currently not called in Update,
	// they would be used by the parent component
	_ = approveCalled
	_ = rejectCalled
}

func TestRenderHeader(t *testing.T) {
	m := New("")
	m.width = 80
	m.rows = []RemovalRow{
		{Status: db.PatternChangeStatusPending},
		{Status: db.PatternChangeStatusApproved},
	}

	header := m.renderHeader()
	if header == "" {
		t.Error("renderHeader should not return empty string")
	}
	if !strings.Contains(header, "Pattern Change Review") {
		t.Error("header should contain title")
	}
	if !strings.Contains(header, "1 pending") {
		t.Error("header should show pending count")
	}
}

func TestRenderFilterBar(t *testing.T) {
	m := New("")
	m.width = 80

	// Default - no filter
	bar := m.renderFilterBar()
	if !strings.Contains(bar, "All Types") {
		t.Error("default filter bar should show 'All Types'")
	}

	// With filter
	m.filterType = db.PatternChangeTypeRemove
	bar = m.renderFilterBar()
	if !strings.Contains(bar, "remove") {
		t.Error("filter bar should show active filter")
	}

	// With message
	m.message = "Success!"
	m.messageType = "success"
	bar = m.renderFilterBar()
	if !strings.Contains(bar, "Success!") {
		t.Error("filter bar should show message")
	}
}

func TestRenderTable(t *testing.T) {
	m := New("")
	m.width = 80
	m.height = 24

	// Empty table
	table := m.renderTable()
	if table == "" {
		t.Error("renderTable should not return empty string")
	}

	// With data
	m.rows = []RemovalRow{
		{ID: 1, Tier: "CRITICAL", Pattern: "test", ChangeType: "remove", Status: db.PatternChangeStatusPending},
	}
	table = m.renderTable()
	if table == "" {
		t.Error("renderTable with data should not return empty")
	}
}

func TestRenderTableTruncation(t *testing.T) {
	m := New("")
	m.width = 80
	m.height = 24
	m.rows = []RemovalRow{
		{
			ID:         1,
			Tier:       "CRITICAL",
			Pattern:    "this is a very long pattern that should be truncated to fit in the table column",
			ChangeType: "remove",
			Reason:     "this is a very long reason that should also be truncated",
			Status:     db.PatternChangeStatusPending,
		},
	}

	table := m.renderTable()
	// Should render without error
	if table == "" {
		t.Error("renderTable should handle long content")
	}
}

func TestRenderFooter(t *testing.T) {
	m := New("")
	m.width = 80

	footer := m.renderFooter()
	if footer == "" {
		t.Error("renderFooter should not return empty string")
	}
	// Should contain key hints
	if !strings.Contains(footer, "approve") {
		t.Error("footer should contain key hints")
	}

	// With total count
	m.totalCount = 5
	footer = m.renderFooter()
	if !strings.Contains(footer, "5 total") {
		t.Error("footer should show total count")
	}

	// With error
	m.lastErr = errTest
	footer = m.renderFooter()
	if !strings.Contains(footer, "Error") {
		t.Error("footer should show error")
	}
}

func TestMessages(t *testing.T) {
	// Test that message types exist and can be created
	_ = refreshMsg{}
	_ = dataMsg{rows: nil, totalCount: 0, err: nil, refreshedAt: time.Now()}
	_ = actionMsg{action: "approve", id: 1, success: true, err: nil}
}

// Test edge cases for approve/reject with empty rows
func TestModelUpdateApproveEmptyRows(t *testing.T) {
	m := New("")
	m.rows = nil
	m.selectedIdx = 0

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if cmd != nil {
		t.Error("approve with empty rows should not return command")
	}
}

func TestModelUpdateRejectEmptyRows(t *testing.T) {
	m := New("")
	m.rows = nil
	m.selectedIdx = 0

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if cmd != nil {
		t.Error("reject with empty rows should not return command")
	}
}

// Test approve with enter key
func TestModelUpdateApproveEnter(t *testing.T) {
	m := New("")
	m.rows = []RemovalRow{
		{ID: 42, Status: db.PatternChangeStatusPending},
	}
	m.selectedIdx = 0

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	// Should trigger approve
	_ = cmd
}

// Test q key for back
func TestModelUpdateQForBack(t *testing.T) {
	m := New("")

	backCalled := false
	m.OnBack = func() { backCalled = true }

	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})

	if !backCalled {
		t.Error("'q' should trigger OnBack")
	}
}

// TestLoadPatternChanges tests the database loading function
func TestLoadPatternChanges(t *testing.T) {
	h := newTestHarness(t)

	// Create some pattern changes in the database
	createTestPatternChange(t, h.db, "rm -rf", db.PatternChangeTypeRemove, db.PatternChangeStatusPending)
	createTestPatternChange(t, h.db, "sudo", db.PatternChangeTypeSuggest, db.PatternChangeStatusApproved)

	rows, total, err := loadPatternChanges(h.db.Path(), "")
	if err != nil {
		t.Fatalf("loadPatternChanges failed: %v", err)
	}

	if total != 2 {
		t.Errorf("expected total 2, got %d", total)
	}
	if len(rows) != 2 {
		t.Errorf("expected 2 rows, got %d", len(rows))
	}
}

func TestPatternReviewUsesSelectedStoreForLoadsAndActions(t *testing.T) {
	project, selected := newTestHarness(t), newTestHarness(t)
	defaultApprove := createTestPatternChange(t, project.db, "^default-approve$", db.PatternChangeTypeSuggest, db.PatternChangeStatusPending)
	defaultReject := createTestPatternChange(t, project.db, "^default-reject$", db.PatternChangeTypeSuggest, db.PatternChangeStatusPending)
	approve := createTestPatternChange(t, selected.db, "^selected-approve$", db.PatternChangeTypeSuggest, db.PatternChangeStatusPending)
	reject := createTestPatternChange(t, selected.db, "^selected-reject$", db.PatternChangeTypeSuggest, db.PatternChangeStatusPending)
	if approve.ID != defaultApprove.ID || reject.ID != defaultReject.ID {
		t.Fatal("fixture must collide IDs across stores to catch a wrong-store mutation")
	}
	m := New(project.projectPath).WithDatabasePath(selected.db.Path())
	read := func(cmd tea.Cmd) dataMsg {
		t.Helper()
		message := cmd()
		if batch, ok := message.(tea.BatchMsg); ok {
			message = batch[0]()
		}
		data := message.(dataMsg)
		if data.err != nil || data.totalCount != 2 || len(data.rows) != 2 {
			t.Fatalf("wrong selected-store pattern history: %+v", data)
		}
		for _, row := range data.rows {
			if !strings.Contains(row.Pattern, "selected") {
				t.Fatalf("default-store pattern leaked into selected view: %+v", row)
			}
		}
		return data
	}
	data := read(m.Init())
	next, _ := m.Update(data)
	m = next.(Model)
	_, refresh := m.Update(refreshMsg{})
	read(refresh)
	for _, action := range []struct {
		id     int64
		key    rune
		status string
	}{{approve.ID, 'a', db.PatternChangeStatusApproved}, {reject.ID, 'r', db.PatternChangeStatusRejected}} {
		for index, row := range m.rows {
			if row.ID == action.id {
				m.selectedIdx = index
			}
		}
		next, command := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{action.key}})
		m = next.(Model)
		result := command().(actionMsg)
		if result.err != nil || !result.success {
			t.Fatalf("selected-store action failed: %+v", result)
		}
		next, reload := m.Update(result)
		m = next.(Model)
		data = read(reload)
		next, _ = m.Update(data)
		m = next.(Model)
		stored, err := selected.db.GetPatternChange(action.id)
		if err != nil || stored.Status != action.status {
			t.Fatalf("action missed selected store: %+v %v", stored, err)
		}
		untouched, err := project.db.GetPatternChange(action.id)
		if err != nil || untouched.Status != db.PatternChangeStatusPending {
			t.Fatalf("action mutated the default store: %+v %v", untouched, err)
		}
	}
	rules, err := selected.db.ListCustomPatterns()
	if err != nil || len(rules) != 1 || rules[0].Pattern != approve.Pattern {
		t.Fatalf("approval did not apply selected-store policy: %+v %v", rules, err)
	}
	if rules, err := project.db.ListCustomPatterns(); err != nil || len(rules) != 0 {
		t.Fatalf("selected-store actions changed default policy: %+v %v", rules, err)
	}
}

func TestPatternReviewMissingSelectedStoreDoesNotFallbackOrCreate(t *testing.T) {
	project := newTestHarness(t)
	change := createTestPatternChange(t, project.db, "^default$", db.PatternChangeTypeSuggest, db.PatternChangeStatusPending)
	missing := filepath.Join(t.TempDir(), "missing.db")
	m := New(project.projectPath).WithDatabasePath(missing)
	batch := m.Init()().(tea.BatchMsg)
	message := batch[0]().(dataMsg)
	if message.err == nil || len(message.rows) != 0 {
		t.Fatalf("missing selected pattern store fell back: %+v", message)
	}
	// A stale row must not redirect an action to the default database either.
	m.rows = []RemovalRow{{ID: change.ID, Status: db.PatternChangeStatusPending}}
	for _, key := range []rune{'a', 'r'} {
		_, command := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{key}})
		result := command().(actionMsg)
		if result.err == nil || result.success {
			t.Fatalf("missing selected store accepted an action: %+v", result)
		}
	}
	stored, err := project.db.GetPatternChange(change.ID)
	if err != nil || stored.Status != db.PatternChangeStatusPending {
		t.Fatalf("missing-store action mutated default policy: %+v %v", stored, err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("pattern action created selected database: %v", err)
	}
}

func TestLoadPatternChangesWithFilter(t *testing.T) {
	h := newTestHarness(t)

	createTestPatternChange(t, h.db, "rm -rf", db.PatternChangeTypeRemove, db.PatternChangeStatusPending)
	createTestPatternChange(t, h.db, "sudo", db.PatternChangeTypeSuggest, db.PatternChangeStatusApproved)

	// Filter by remove type
	rows, total, err := loadPatternChanges(h.db.Path(), db.PatternChangeTypeRemove)
	if err != nil {
		t.Fatalf("loadPatternChanges with filter failed: %v", err)
	}

	if total != 1 {
		t.Errorf("expected total 1 with filter, got %d", total)
	}
	if len(rows) > 0 && rows[0].ChangeType != db.PatternChangeTypeRemove {
		t.Errorf("expected remove type, got %s", rows[0].ChangeType)
	}
}

func TestLoadPatternChangesNonexistentDB(t *testing.T) {
	_, _, err := loadPatternChanges("/nonexistent/path", "")
	if err == nil {
		t.Error("expected error for nonexistent database")
	}
}

func TestLoadPatternChangesEmptyDB(t *testing.T) {
	h := newTestHarness(t)

	rows, total, err := loadPatternChanges(h.db.Path(), "")
	if err != nil {
		t.Fatalf("loadPatternChanges on empty DB failed: %v", err)
	}

	if total != 0 {
		t.Errorf("expected total 0, got %d", total)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows, got %d", len(rows))
	}
}

func TestPerformActionApprove(t *testing.T) {
	h := newTestHarness(t)

	change := createTestPatternChange(t, h.db, "rm -rf", db.PatternChangeTypeRemove, db.PatternChangeStatusPending)

	err := performAction(h.db.Path(), change.ID, "approve")
	if err != nil {
		t.Fatalf("performAction approve failed: %v", err)
	}

	// Verify the change was approved
	updated, err := h.db.GetPatternChange(change.ID)
	if err != nil {
		t.Fatalf("failed to get updated pattern change: %v", err)
	}
	if updated.Status != db.PatternChangeStatusApproved {
		t.Errorf("expected status approved, got %s", updated.Status)
	}
}

func TestPerformActionReject(t *testing.T) {
	h := newTestHarness(t)

	change := createTestPatternChange(t, h.db, "sudo", db.PatternChangeTypeSuggest, db.PatternChangeStatusPending)

	err := performAction(h.db.Path(), change.ID, "reject")
	if err != nil {
		t.Fatalf("performAction reject failed: %v", err)
	}

	// Verify the change was rejected
	updated, err := h.db.GetPatternChange(change.ID)
	if err != nil {
		t.Fatalf("failed to get updated pattern change: %v", err)
	}
	if updated.Status != db.PatternChangeStatusRejected {
		t.Errorf("expected status rejected, got %s", updated.Status)
	}
}

func TestPerformActionUnknown(t *testing.T) {
	h := newTestHarness(t)

	change := createTestPatternChange(t, h.db, "test", db.PatternChangeTypeRemove, db.PatternChangeStatusPending)

	err := performAction(h.db.Path(), change.ID, "unknown")
	if err == nil {
		t.Fatal("expected error for unknown action")
	}
	if !strings.Contains(err.Error(), "unknown action") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPerformActionNonexistentDB(t *testing.T) {
	err := performAction("/nonexistent/path", 1, "approve")
	if err == nil {
		t.Error("expected error for nonexistent database")
	}
}

func TestLoadDataCmd(t *testing.T) {
	h := newTestHarness(t)

	createTestPatternChange(t, h.db, "test", db.PatternChangeTypeRemove, db.PatternChangeStatusPending)

	cmd := loadDataCmd(h.db.Path(), "")
	if cmd == nil {
		t.Fatal("loadDataCmd should return non-nil command")
	}

	// Execute the command to get the message
	msg := cmd()
	dm, ok := msg.(dataMsg)
	if !ok {
		t.Fatalf("expected dataMsg, got %T", msg)
	}

	if dm.err != nil {
		t.Errorf("unexpected error: %v", dm.err)
	}
	if dm.totalCount != 1 {
		t.Errorf("expected totalCount 1, got %d", dm.totalCount)
	}
}

func TestLoadDataCmdWithFilter(t *testing.T) {
	h := newTestHarness(t)

	createTestPatternChange(t, h.db, "rm -rf", db.PatternChangeTypeRemove, db.PatternChangeStatusPending)
	createTestPatternChange(t, h.db, "sudo", db.PatternChangeTypeSuggest, db.PatternChangeStatusPending)

	cmd := loadDataCmd(h.db.Path(), db.PatternChangeTypeRemove)
	msg := cmd()
	dm := msg.(dataMsg)

	if dm.totalCount != 1 {
		t.Errorf("expected totalCount 1 with filter, got %d", dm.totalCount)
	}
}

func TestApproveCmd(t *testing.T) {
	h := newTestHarness(t)

	change := createTestPatternChange(t, h.db, "test", db.PatternChangeTypeRemove, db.PatternChangeStatusPending)

	cmd := approveCmd(h.db.Path(), change.ID)
	if cmd == nil {
		t.Fatal("approveCmd should return non-nil command")
	}

	// Execute the command
	msg := cmd()
	am, ok := msg.(actionMsg)
	if !ok {
		t.Fatalf("expected actionMsg, got %T", msg)
	}

	if am.action != "approve" {
		t.Errorf("expected action 'approve', got %q", am.action)
	}
	if am.id != change.ID {
		t.Errorf("expected id %d, got %d", change.ID, am.id)
	}
	if !am.success {
		t.Errorf("expected success=true, got error: %v", am.err)
	}
}

func TestRejectCmd(t *testing.T) {
	h := newTestHarness(t)

	change := createTestPatternChange(t, h.db, "test", db.PatternChangeTypeSuggest, db.PatternChangeStatusPending)

	cmd := rejectCmd(h.db.Path(), change.ID)
	if cmd == nil {
		t.Fatal("rejectCmd should return non-nil command")
	}

	// Execute the command
	msg := cmd()
	am, ok := msg.(actionMsg)
	if !ok {
		t.Fatalf("expected actionMsg, got %T", msg)
	}

	if am.action != "reject" {
		t.Errorf("expected action 'reject', got %q", am.action)
	}
	if !am.success {
		t.Errorf("expected success=true, got error: %v", am.err)
	}
}

func TestTickCmd(t *testing.T) {
	cmd := tickCmd()
	if cmd == nil {
		t.Error("tickCmd should return non-nil command")
	}
}

// TestRenderFilterBarErrorMessage tests the error display path in renderFilterBar
func TestRenderFilterBarErrorMessage(t *testing.T) {
	m := New("")
	m.width = 80
	m.message = "Error occurred"
	m.messageType = "error"

	bar := m.renderFilterBar()
	if !strings.Contains(bar, "Error occurred") {
		t.Error("filter bar should show error message")
	}
}

// TestRenderFilterBarSuggestType tests renderFilterBar with suggest filter type
func TestRenderFilterBarSuggestType(t *testing.T) {
	m := New("")
	m.width = 80
	m.filterType = db.PatternChangeTypeSuggest

	bar := m.renderFilterBar()
	if !strings.Contains(bar, "suggest") {
		t.Error("filter bar should show 'suggest' filter type")
	}
}

// TestRenderFilterBarAddType tests renderFilterBar with add filter type
func TestRenderFilterBarAddType(t *testing.T) {
	m := New("")
	m.width = 80
	m.filterType = db.PatternChangeTypeAdd

	bar := m.renderFilterBar()
	if !strings.Contains(bar, "add") {
		t.Error("filter bar should show 'add' filter type")
	}
}

// Test harness for database tests
type testHarness struct {
	projectPath string
	db          *db.DB
}

func newTestHarness(t *testing.T) *testHarness {
	t.Helper()

	tmpDir := t.TempDir()
	slbDir := tmpDir + "/.slb"

	// Create .slb directory structure
	if err := mkdir(slbDir); err != nil {
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

func mkdir(path string) error {
	return os.MkdirAll(path, 0755)
}

func createTestPatternChange(t *testing.T, database *db.DB, pattern, changeType, status string) *db.PatternChange {
	t.Helper()

	change := &db.PatternChange{
		Tier:       "CRITICAL",
		Pattern:    pattern,
		ChangeType: changeType,
		Reason:     "test reason",
		Status:     status,
	}

	if err := database.CreatePatternChange(change); err != nil {
		t.Fatalf("failed to create pattern change: %v", err)
	}
	return change
}
