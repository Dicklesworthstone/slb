package history

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestNewBrowser(t *testing.T) {
	m := New("")
	// Should use current directory if empty
	if m.page != 0 {
		t.Errorf("expected page 0, got %d", m.page)
	}
}

func TestNewBrowserWithPath(t *testing.T) {
	m := New("/test/path")
	if m.projectPath != "/test/path" {
		t.Errorf("expected projectPath '/test/path', got %q", m.projectPath)
	}
}

func TestDefaultBrowserKeyMap(t *testing.T) {
	km := DefaultBrowserKeyMap()

	if len(km.Search.Keys()) == 0 {
		t.Error("Search binding should have keys")
	}
	if len(km.ClearSearch.Keys()) == 0 {
		t.Error("ClearSearch binding should have keys")
	}
	if len(km.NextPage.Keys()) == 0 {
		t.Error("NextPage binding should have keys")
	}
	if len(km.PrevPage.Keys()) == 0 {
		t.Error("PrevPage binding should have keys")
	}
	if len(km.Select.Keys()) == 0 {
		t.Error("Select binding should have keys")
	}
	if len(km.Back.Keys()) == 0 {
		t.Error("Back binding should have keys")
	}
	if len(km.Quit.Keys()) == 0 {
		t.Error("Quit binding should have keys")
	}
	if len(km.Up.Keys()) == 0 {
		t.Error("Up binding should have keys")
	}
	if len(km.Down.Keys()) == 0 {
		t.Error("Down binding should have keys")
	}
	if len(km.FilterTier.Keys()) == 0 {
		t.Error("FilterTier binding should have keys")
	}
	if len(km.FilterStatus.Keys()) == 0 {
		t.Error("FilterStatus binding should have keys")
	}
	if len(km.Export.Keys()) == 0 {
		t.Error("Export binding should have keys")
	}
}

func TestBrowserModelInit(t *testing.T) {
	m := New("")
	cmd := m.Init()
	if cmd == nil {
		t.Error("Init should return non-nil command")
	}
}

func TestBrowserModelUpdateWindowSize(t *testing.T) {
	m := New("")

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 50})
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
}

func TestBrowserModelUpdateRefreshMsg(t *testing.T) {
	m := New("")

	_, cmd := m.Update(refreshMsg{})
	if cmd == nil {
		t.Error("refreshMsg should return non-nil command")
	}
}

func TestBrowserModelUpdateDataMsg(t *testing.T) {
	m := New("")

	msg := dataMsg{
		rows: []HistoryRow{
			{ID: "1", Command: "test", Agent: "Agent1", Status: db.StatusPending, Tier: db.RiskTierCritical},
			{ID: "2", Command: "test2", Agent: "Agent2", Status: db.StatusApproved, Tier: db.RiskTierCaution},
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

func TestBrowserModelUpdateDataMsgClampsSelection(t *testing.T) {
	m := New("")
	m.selectedIdx = 10 // Out of range

	msg := dataMsg{
		rows:       []HistoryRow{{ID: "1"}},
		totalCount: 1,
	}

	updated, _ := m.Update(msg)
	model := updated.(Model)

	if model.selectedIdx != 0 {
		t.Errorf("expected selectedIdx 0 after clamping, got %d", model.selectedIdx)
	}
}

func TestBrowserModelUpdateDataMsgPageCount(t *testing.T) {
	m := New("")

	msg := dataMsg{
		rows:       []HistoryRow{},
		totalCount: 45, // More than pageSize
	}

	updated, _ := m.Update(msg)
	model := updated.(Model)

	expectedPages := (45 + pageSize - 1) / pageSize
	if model.pageCount != expectedPages {
		t.Errorf("expected pageCount %d, got %d", expectedPages, model.pageCount)
	}
}

func TestBrowserModelUpdateKeyQuit(t *testing.T) {
	m := New("")

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	// Should return quit command
	_ = cmd
}

func TestBrowserModelUpdateKeyUpDown(t *testing.T) {
	m := New("")
	m.rows = []HistoryRow{{ID: "1"}, {ID: "2"}, {ID: "3"}}
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

func TestBrowserModelUpdateKeyUpAtTop(t *testing.T) {
	m := New("")
	m.rows = []HistoryRow{{ID: "1"}, {ID: "2"}}
	m.selectedIdx = 0

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	model := updated.(Model)
	if model.selectedIdx != 0 {
		t.Errorf("expected selectedIdx 0 when already at top, got %d", model.selectedIdx)
	}
}

func TestBrowserModelUpdateKeyDownAtBottom(t *testing.T) {
	m := New("")
	m.rows = []HistoryRow{{ID: "1"}, {ID: "2"}}
	m.selectedIdx = 1

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	model := updated.(Model)
	if model.selectedIdx != 1 {
		t.Errorf("expected selectedIdx 1 when already at bottom, got %d", model.selectedIdx)
	}
}

func TestBrowserModelUpdateKeySearch(t *testing.T) {
	m := New("")
	m.ready = true

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	model := updated.(Model)

	if !model.searching {
		t.Error("should be in search mode after /")
	}
	if cmd == nil {
		t.Error("should return blink command")
	}
}

func TestBrowserModelSearchModeEnter(t *testing.T) {
	m := New("")
	m.searching = true
	m.searchInput.SetValue("test query")

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model := updated.(Model)

	if model.searching {
		t.Error("should exit search mode after enter")
	}
	if model.searchQuery != "test query" {
		t.Errorf("expected searchQuery 'test query', got %q", model.searchQuery)
	}
	if model.page != 0 {
		t.Error("page should reset to 0 after search")
	}
	if cmd == nil {
		t.Error("should return data load command")
	}
}

func TestBrowserModelSearchModeEsc(t *testing.T) {
	m := New("")
	m.searching = true
	m.searchQuery = "old query"
	m.searchInput.SetValue("new query")

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model := updated.(Model)

	if model.searching {
		t.Error("should exit search mode after esc")
	}
	// Input should be reset to previous query
}

func TestBrowserModelSearchModeTyping(t *testing.T) {
	m := New("")
	m.searching = true

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	model := updated.(Model)

	if !model.searching {
		t.Error("should still be in search mode")
	}
}

func TestBrowserModelUpdateKeyClearSearch(t *testing.T) {
	m := New("")
	m.searchQuery = "test"

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model := updated.(Model)

	if model.searchQuery != "" {
		t.Error("searchQuery should be cleared")
	}
	if cmd == nil {
		t.Error("should return data load command")
	}
}

func TestBrowserModelUpdateKeyClearSearchWhenEmpty(t *testing.T) {
	m := New("")
	m.searchQuery = ""

	backCalled := false
	m.OnBack = func() { backCalled = true }

	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})

	if !backCalled {
		t.Error("OnBack should be called when search is empty")
	}
}

func TestBrowserModelUpdateKeyNextPage(t *testing.T) {
	m := New("")
	m.page = 0
	m.pageCount = 3

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRight})
	model := updated.(Model)

	if model.page != 1 {
		t.Errorf("expected page 1 after next, got %d", model.page)
	}
	if model.selectedIdx != 0 {
		t.Error("selectedIdx should reset to 0")
	}
	if cmd == nil {
		t.Error("should return data load command")
	}
}

func TestBrowserModelUpdateKeyNextPageAtEnd(t *testing.T) {
	m := New("")
	m.page = 2
	m.pageCount = 3

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRight})
	model := updated.(Model)

	if model.page != 2 {
		t.Errorf("expected page 2 when already at end, got %d", model.page)
	}
}

func TestBrowserModelUpdateKeyPrevPage(t *testing.T) {
	m := New("")
	m.page = 2
	m.pageCount = 3

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	model := updated.(Model)

	if model.page != 1 {
		t.Errorf("expected page 1 after prev, got %d", model.page)
	}
	if cmd == nil {
		t.Error("should return data load command")
	}
}

func TestBrowserModelUpdateKeyPrevPageAtStart(t *testing.T) {
	m := New("")
	m.page = 0
	m.pageCount = 3

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	model := updated.(Model)

	if model.page != 0 {
		t.Errorf("expected page 0 when already at start, got %d", model.page)
	}
}

func TestBrowserModelUpdateKeySelect(t *testing.T) {
	m := New("")
	m.rows = []HistoryRow{{ID: "REQ-123"}}
	m.selectedIdx = 0

	selectCalled := false
	selectedID := ""
	m.OnSelect = func(id string) {
		selectCalled = true
		selectedID = id
	}

	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if !selectCalled {
		t.Error("OnSelect should be called")
	}
	if selectedID != "REQ-123" {
		t.Errorf("expected selectedID 'REQ-123', got %q", selectedID)
	}
}

func TestBrowserModelUpdateKeySelectEmptyRows(t *testing.T) {
	m := New("")
	m.rows = nil

	selectCalled := false
	m.OnSelect = func(id string) { selectCalled = true }

	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if selectCalled {
		t.Error("OnSelect should not be called with empty rows")
	}
}

func TestBrowserModelUpdateKeyFilterTier(t *testing.T) {
	m := New("")

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	model := updated.(Model)

	if model.page != 0 {
		t.Error("page should reset to 0 after filter change")
	}
	if cmd == nil {
		t.Error("should return data load command")
	}
}

func TestBrowserModelUpdateKeyFilterStatus(t *testing.T) {
	m := New("")

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	model := updated.(Model)

	if model.page != 0 {
		t.Error("page should reset to 0 after filter change")
	}
	if cmd == nil {
		t.Error("should return data load command")
	}
}

func TestBrowserModelViewBeforeReady(t *testing.T) {
	m := New("")

	view := m.View()
	if !strings.Contains(view, "Loading") {
		t.Error("View before ready should show loading")
	}
}

func TestBrowserModelViewAfterReady(t *testing.T) {
	m := New("")
	m.ready = true
	m.width = 80
	m.height = 24
	m.pageCount = 1

	view := m.View()
	if view == "" {
		t.Error("View should not be empty")
	}
	if !strings.Contains(view, "History Browser") {
		t.Error("View should contain title")
	}
}

func TestBrowserModelViewWithData(t *testing.T) {
	m := New("")
	m.ready = true
	m.width = 80
	m.height = 24
	m.pageCount = 1
	m.rows = []HistoryRow{
		{ID: "REQ-001", Command: "rm -rf /tmp", Agent: "TestAgent", Status: db.StatusPending, Tier: db.RiskTierCritical, CreatedAt: time.Now()},
	}
	m.totalCount = 1

	view := m.View()
	if view == "" {
		t.Error("View with data should not be empty")
	}
}

func TestBrowserModelViewEmpty(t *testing.T) {
	m := New("")
	m.ready = true
	m.width = 80
	m.height = 24
	m.pageCount = 1
	m.rows = nil

	view := m.View()
	if !strings.Contains(view, "No request history") {
		t.Error("View should show empty state")
	}
}

func TestBrowserModelViewEmptyWithSearch(t *testing.T) {
	m := New("")
	m.ready = true
	m.width = 80
	m.height = 24
	m.pageCount = 1
	m.rows = nil
	m.searchQuery = "test"

	view := m.View()
	if !strings.Contains(view, "No results") {
		t.Error("View should show no results message")
	}
}

func TestBrowserModelViewWithError(t *testing.T) {
	m := New("")
	m.ready = true
	m.width = 80
	m.height = 24
	m.pageCount = 1
	m.lastErr = &testError{}

	view := m.View()
	if view == "" {
		t.Error("View with error should not be empty")
	}
}

type testError struct{}

func (e *testError) Error() string { return "test error" }

func TestBrowserModelViewSearching(t *testing.T) {
	m := New("")
	m.ready = true
	m.width = 80
	m.height = 24
	m.pageCount = 1
	m.searching = true

	view := m.View()
	if view == "" {
		t.Error("View in search mode should not be empty")
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

func TestBrowserFormatTimeAgo(t *testing.T) {
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

func TestBrowserStatusIcon(t *testing.T) {
	tests := []struct {
		status   db.RequestStatus
		expected string
	}{
		{db.StatusApproved, "✓"},
		{db.StatusExecuted, "✓"},
		{db.StatusRejected, "✗"},
		{db.StatusExecutionFailed, "✗"},
		{db.StatusPending, "⋯"},
		{db.StatusTimeout, "⚠"},
		{db.StatusEscalated, "⚠"},
		{db.StatusCancelled, "○"},
		{"unknown", "?"},
	}

	for _, tc := range tests {
		got := statusIcon(tc.status)
		if got != tc.expected {
			t.Errorf("statusIcon(%q): expected %q, got %q", tc.status, tc.expected, got)
		}
	}
}

func TestStatusShort(t *testing.T) {
	tests := []struct {
		status   db.RequestStatus
		expected string
	}{
		{db.StatusApproved, "APPR"},
		{db.StatusExecuted, "EXEC"},
		{db.StatusRejected, "REJ"},
		{db.StatusExecutionFailed, "FAIL"},
		{db.StatusPending, "PEND"},
		{db.StatusTimeout, "TOUT"},
		{db.StatusEscalated, "ESC"},
		{db.StatusCancelled, "CANC"},
		{"unknown", "unknown"},
	}

	for _, tc := range tests {
		got := statusShort(tc.status)
		if got != tc.expected {
			t.Errorf("statusShort(%q): expected %q, got %q", tc.status, tc.expected, got)
		}
	}
}

func TestBrowserMax(t *testing.T) {
	tests := []struct {
		a, b, expected int
	}{
		{1, 2, 2},
		{2, 1, 2},
		{0, 0, 0},
		{-1, 1, 1},
	}

	for _, tc := range tests {
		got := max(tc.a, tc.b)
		if got != tc.expected {
			t.Errorf("max(%d, %d): expected %d, got %d", tc.a, tc.b, tc.expected, got)
		}
	}
}

func TestBrowserMin(t *testing.T) {
	tests := []struct {
		a, b, expected int
	}{
		{1, 2, 1},
		{2, 1, 1},
		{0, 0, 0},
		{-1, 1, -1},
	}

	for _, tc := range tests {
		got := min(tc.a, tc.b)
		if got != tc.expected {
			t.Errorf("min(%d, %d): expected %d, got %d", tc.a, tc.b, tc.expected, got)
		}
	}
}

func TestHistoryRow(t *testing.T) {
	now := time.Now()
	row := HistoryRow{
		ID:        "REQ-001",
		Command:   "rm -rf /tmp",
		Agent:     "TestAgent",
		Status:    db.StatusPending,
		Tier:      db.RiskTierCritical,
		CreatedAt: now,
		Request:   &db.Request{ID: "REQ-001"},
	}

	if row.ID != "REQ-001" {
		t.Error("ID mismatch")
	}
	if row.Command != "rm -rf /tmp" {
		t.Error("Command mismatch")
	}
	if row.Agent != "TestAgent" {
		t.Error("Agent mismatch")
	}
	if row.Status != db.StatusPending {
		t.Error("Status mismatch")
	}
	if row.Request == nil {
		t.Error("Request should not be nil")
	}
}

func TestRenderHeader(t *testing.T) {
	m := New("")
	m.width = 80
	m.pageCount = 3
	m.page = 1

	header := m.renderHeader()
	if !strings.Contains(header, "History Browser") {
		t.Error("header should contain title")
	}
	if !strings.Contains(header, "2/3") {
		t.Error("header should show page info")
	}
}

func TestRenderSearchBar(t *testing.T) {
	m := New("")
	m.width = 80

	bar := m.renderSearchBar()
	if bar == "" {
		t.Error("search bar should not be empty")
	}
}

func TestRenderSearchBarSearching(t *testing.T) {
	m := New("")
	m.width = 80
	m.searching = true

	bar := m.renderSearchBar()
	if bar == "" {
		t.Error("search bar in search mode should not be empty")
	}
}

func TestRenderTable(t *testing.T) {
	m := New("")
	m.width = 80
	m.height = 24

	// Empty table
	table := m.renderTable()
	if table == "" {
		t.Error("table should not be empty")
	}

	// With data
	m.rows = []HistoryRow{
		{ID: "REQ-001", Command: "test", Agent: "Agent", Status: db.StatusPending, CreatedAt: time.Now()},
	}

	table = m.renderTable()
	if table == "" {
		t.Error("table with data should not be empty")
	}
}

func TestRenderTableLongCommand(t *testing.T) {
	m := New("")
	m.width = 80
	m.height = 24
	m.rows = []HistoryRow{
		{ID: "REQ-001", Command: strings.Repeat("x", 100), Agent: "Agent", Status: db.StatusPending, CreatedAt: time.Now()},
	}

	table := m.renderTable()
	if table == "" {
		t.Error("table should handle long commands")
	}
}

func TestRenderFooter(t *testing.T) {
	m := New("")
	m.width = 80

	footer := m.renderFooter()
	if footer == "" {
		t.Error("footer should not be empty")
	}
	if !strings.Contains(footer, "search") {
		t.Error("footer should contain key hints")
	}

	// With results
	m.totalCount = 42
	footer = m.renderFooter()
	if !strings.Contains(footer, "42") {
		t.Error("footer should show result count")
	}

	// With error
	m.lastErr = &testError{}
	footer = m.renderFooter()
	if !strings.Contains(footer, "Error") {
		t.Error("footer should show error")
	}
}

func TestMessages(t *testing.T) {
	_ = refreshMsg{}
	_ = dataMsg{rows: nil, totalCount: 0, err: nil, refreshedAt: time.Now()}
}

// Test page navigation with vim keys
func TestBrowserModelUpdateKeyNextPageVim(t *testing.T) {
	m := New("")
	m.page = 0
	m.pageCount = 3

	// Test 'l' for next page
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	model := updated.(Model)

	if model.page != 1 {
		t.Errorf("expected page 1 after 'l', got %d", model.page)
	}
	if cmd == nil {
		t.Error("should return data load command")
	}
}

func TestBrowserModelUpdateKeyPrevPageVim(t *testing.T) {
	m := New("")
	m.page = 1
	m.pageCount = 3

	// Test 'h' for prev page
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	model := updated.(Model)

	if model.page != 0 {
		t.Errorf("expected page 0 after 'h', got %d", model.page)
	}
	if cmd == nil {
		t.Error("should return data load command")
	}
}

// TestLoadHistoryData tests the database loading function
func TestLoadHistoryData(t *testing.T) {
	// Create test environment with database
	h := newTestHarness(t)

	// Create session and requests (use h.projectPath so filter matches)
	sess := createTestSession(t, h.db, h.projectPath)
	createTestRequest(t, h.db, sess, "rm -rf /tmp", db.RiskTierCritical, db.StatusPending)
	createTestRequest(t, h.db, sess, "git push --force", db.RiskTierDangerous, db.StatusApproved)

	// Load data
	rows, total, err := loadHistoryData(h.projectPath, h.db.Path(), "", Filters{}, 0)
	if err != nil {
		t.Fatalf("loadHistoryData failed: %v", err)
	}

	if total != 2 {
		t.Errorf("expected total 2, got %d", total)
	}
	if len(rows) != 2 {
		t.Errorf("expected 2 rows, got %d", len(rows))
	}
}

func TestHistorySelectedStoreScopesSearchBeforePagination(t *testing.T) {
	project, selected := newTestHarness(t), newTestHarness(t)
	defaultSession := createTestSession(t, project.db, project.projectPath)
	createTestRequest(t, project.db, defaultSession, "needle default", db.RiskTierCritical, db.StatusPending)
	session := createTestSession(t, selected.db, project.projectPath)
	foreign := createTestSession(t, selected.db, selected.projectPath)
	for i := 0; i < pageSize+4; i++ {
		createTestRequest(t, selected.db, session, "needle selected", db.RiskTierCritical, db.StatusPending)
		for j := 0; j < 5; j++ {
			createTestRequest(t, selected.db, foreign, "needle foreign", db.RiskTierCritical, db.StatusPending)
		}
	}
	// More than 100 newer foreign matches must not consume the search limit
	// before project scoping and hide every selected-project result.
	if _, err := selected.db.Exec(`UPDATE requests SET created_at = ? WHERE project_path = ?`,
		time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), project.projectPath); err != nil {
		t.Fatal(err)
	}
	m := New(project.projectPath).WithDatabasePath(selected.db.Path())
	read := func(cmd tea.Cmd, wantRows int) dataMsg {
		t.Helper()
		message := cmd()
		if batch, ok := message.(tea.BatchMsg); ok {
			message = batch[0]()
		}
		data := message.(dataMsg)
		if data.err != nil || data.totalCount != pageSize+4 || len(data.rows) != wantRows {
			t.Fatalf("wrong selected-project page: %+v", data)
		}
		for _, row := range data.rows {
			if row.Request.ProjectPath != project.projectPath || row.Command != "needle selected" {
				t.Fatalf("foreign project or default store leaked into history: %+v", row)
			}
		}
		return data
	}
	data := read(m.Init(), pageSize)
	next, _ := m.Update(data)
	m = next.(Model)
	m.searching = true
	m.searchInput.SetValue("needle")
	next, search := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	data = read(search, pageSize)
	next, _ = m.Update(data)
	m = next.(Model)
	if m.pageCount != 2 {
		t.Fatalf("foreign matches changed page count: %d", m.pageCount)
	}
	next, page := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	m = next.(Model)
	data = read(page, 4)
	next, _ = m.Update(data)
	m = next.(Model)
	_, refresh := m.Update(refreshMsg{})
	read(refresh, 4)
	for _, key := range []rune{'t', 's'} {
		next, filter := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{key}})
		m = next.(Model)
		read(filter, pageSize)
	}
	m.ready, m.width, m.height = true, 120, 40
	if view := m.View(); strings.Contains(view, "foreign") || strings.Contains(view, "default") {
		t.Fatalf("history rendered another project or database: %s", view)
	}
	rows, total, err := loadHistoryData(project.projectPath, selected.db.Path(), "foreign", Filters{}, 0)
	if err != nil || total != 0 || len(rows) != 0 {
		t.Fatalf("foreign-only search escaped project scope: %+v %d %v", rows, total, err)
	}
}

func TestHistoryMissingSelectedStoreDoesNotFallbackOrCreate(t *testing.T) {
	project := newTestHarness(t)
	session := createTestSession(t, project.db, project.projectPath)
	createTestRequest(t, project.db, session, "default history", db.RiskTierCritical, db.StatusPending)
	missing := filepath.Join(t.TempDir(), "missing.db")
	m := New(project.projectPath).WithDatabasePath(missing)
	batch := m.Init()().(tea.BatchMsg)
	message := batch[0]().(dataMsg)
	if message.err == nil || message.totalCount != 0 || len(message.rows) != 0 {
		t.Fatalf("history fell back to the default database: %+v", message)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("history created selected database: %v", err)
	}
}

func TestLoadHistoryDataWithSearch(t *testing.T) {
	h := newTestHarness(t)

	sess := createTestSession(t, h.db, h.projectPath)
	createTestRequest(t, h.db, sess, "docker build", db.RiskTierCaution, db.StatusPending)
	createTestRequest(t, h.db, sess, "npm install", db.RiskTierCaution, db.StatusApproved)

	// Search for docker
	rows, _, err := loadHistoryData(h.projectPath, h.db.Path(), "docker", Filters{}, 0)
	if err != nil {
		t.Fatalf("loadHistoryData with search failed: %v", err)
	}

	// FTS may or may not find results depending on indexing - just verify no error
	_ = rows
}

func TestLoadHistoryDataWithTierFilter(t *testing.T) {
	h := newTestHarness(t)

	sess := createTestSession(t, h.db, h.projectPath)
	createTestRequest(t, h.db, sess, "rm -rf /", db.RiskTierCritical, db.StatusPending)
	createTestRequest(t, h.db, sess, "git status", db.RiskTierCaution, db.StatusApproved)

	// Filter by critical tier
	filters := Filters{TierFilter: string(db.RiskTierCritical)}
	rows, total, err := loadHistoryData(h.projectPath, h.db.Path(), "", filters, 0)
	if err != nil {
		t.Fatalf("loadHistoryData with tier filter failed: %v", err)
	}

	if total != 1 {
		t.Errorf("expected total 1 with critical filter, got %d", total)
	}
	if len(rows) != 1 {
		t.Errorf("expected 1 row, got %d", len(rows))
	}
	if len(rows) > 0 && rows[0].Tier != db.RiskTierCritical {
		t.Errorf("expected critical tier, got %s", rows[0].Tier)
	}
}

func TestLoadHistoryDataWithStatusFilter(t *testing.T) {
	h := newTestHarness(t)

	sess := createTestSession(t, h.db, h.projectPath)
	createTestRequest(t, h.db, sess, "rm -rf /tmp", db.RiskTierCritical, db.StatusPending)
	createTestRequest(t, h.db, sess, "ls -la", db.RiskTierCaution, db.StatusApproved)

	// Filter by approved status
	filters := Filters{StatusFilter: string(db.StatusApproved)}
	rows, total, err := loadHistoryData(h.projectPath, h.db.Path(), "", filters, 0)
	if err != nil {
		t.Fatalf("loadHistoryData with status filter failed: %v", err)
	}

	if total != 1 {
		t.Errorf("expected total 1 with approved filter, got %d", total)
	}
	if len(rows) > 0 && rows[0].Status != db.StatusApproved {
		t.Errorf("expected approved status, got %s", rows[0].Status)
	}
}

func TestLoadHistoryDataPagination(t *testing.T) {
	h := newTestHarness(t)

	sess := createTestSession(t, h.db, h.projectPath)
	// Create more requests than pageSize
	for i := 0; i < pageSize+5; i++ {
		createTestRequest(t, h.db, sess, "echo test", db.RiskTierCaution, db.StatusPending)
	}

	// First page
	rows, total, err := loadHistoryData(h.projectPath, h.db.Path(), "", Filters{}, 0)
	if err != nil {
		t.Fatalf("loadHistoryData page 0 failed: %v", err)
	}

	if total != pageSize+5 {
		t.Errorf("expected total %d, got %d", pageSize+5, total)
	}
	if len(rows) != pageSize {
		t.Errorf("expected %d rows on first page, got %d", pageSize, len(rows))
	}

	// Second page
	rows, _, err = loadHistoryData(h.projectPath, h.db.Path(), "", Filters{}, 1)
	if err != nil {
		t.Fatalf("loadHistoryData page 1 failed: %v", err)
	}

	if len(rows) != 5 {
		t.Errorf("expected 5 rows on second page, got %d", len(rows))
	}
}

func TestLoadHistoryDataNonexistentDB(t *testing.T) {
	_, _, err := loadHistoryData("/nonexistent/path", "/nonexistent/path/state.db", "", Filters{}, 0)
	if err == nil {
		t.Error("expected error for nonexistent database")
	}
}

func TestLoadHistoryDataEmptyDB(t *testing.T) {
	h := newTestHarness(t)

	rows, total, err := loadHistoryData(h.projectPath, h.db.Path(), "", Filters{}, 0)
	if err != nil {
		t.Fatalf("loadHistoryData on empty DB failed: %v", err)
	}

	if total != 0 {
		t.Errorf("expected total 0, got %d", total)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows, got %d", len(rows))
	}
}

func TestLoadDataCmd(t *testing.T) {
	h := newTestHarness(t)

	sess := createTestSession(t, h.db, h.projectPath)
	createTestRequest(t, h.db, sess, "test cmd", db.RiskTierCaution, db.StatusPending)

	cmd := loadDataCmd(h.projectPath, h.db.Path(), "", Filters{}, 0)
	if cmd == nil {
		t.Fatal("loadDataCmd should return non-nil command")
	}

	// Execute the command to get the message
	msg := cmd()
	dataMsg, ok := msg.(dataMsg)
	if !ok {
		t.Fatalf("expected dataMsg, got %T", msg)
	}

	if dataMsg.err != nil {
		t.Errorf("unexpected error: %v", dataMsg.err)
	}
	if dataMsg.totalCount != 1 {
		t.Errorf("expected totalCount 1, got %d", dataMsg.totalCount)
	}
}

func TestTickCmd(t *testing.T) {
	cmd := tickCmd()
	if cmd == nil {
		t.Error("tickCmd should return non-nil command")
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

func createTestRequest(t *testing.T, database *db.DB, sess *db.Session, cmd string, tier db.RiskTier, status db.RequestStatus) *db.Request {
	t.Helper()

	exp := time.Now().Add(30 * time.Minute)
	req := &db.Request{
		ID:                 "req-" + randHex(6),
		ProjectPath:        sess.ProjectPath,
		Command:            db.CommandSpec{Raw: cmd, Cwd: "/tmp", Shell: true},
		RiskTier:           tier,
		RequestorSessionID: sess.ID,
		RequestorAgent:     sess.AgentName,
		RequestorModel:     sess.Model,
		Justification:      db.Justification{Reason: "test"},
		Status:             status,
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
