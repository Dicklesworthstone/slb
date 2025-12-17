package components

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// ============== AgentCard Tests ==============

func TestNewAgentCard(t *testing.T) {
	agent := AgentInfo{
		Name:    "TestAgent",
		Program: "claude-code",
		Model:   "opus-4",
		Status:  AgentStatusActive,
	}

	card := NewAgentCard(agent)
	if card.Agent.Name != "TestAgent" {
		t.Errorf("expected agent name 'TestAgent', got %q", card.Agent.Name)
	}
	if card.Width != 40 {
		t.Errorf("expected default width 40, got %d", card.Width)
	}
	if card.Compact {
		t.Error("expected Compact to be false by default")
	}
	if card.Selected {
		t.Error("expected Selected to be false by default")
	}
}

func TestAgentCardChaining(t *testing.T) {
	agent := AgentInfo{Name: "Test"}
	card := NewAgentCard(agent).AsCompact().AsSelected(true).WithWidth(60)

	if !card.Compact {
		t.Error("expected Compact to be true")
	}
	if !card.Selected {
		t.Error("expected Selected to be true")
	}
	if card.Width != 60 {
		t.Errorf("expected width 60, got %d", card.Width)
	}
}

func TestAgentCardRender(t *testing.T) {
	tests := []struct {
		name     string
		agent    AgentInfo
		compact  bool
		selected bool
	}{
		{
			name:     "active agent",
			agent:    AgentInfo{Name: "Agent1", Program: "claude", Model: "opus", Status: AgentStatusActive, LastActive: time.Now()},
			compact:  false,
			selected: false,
		},
		{
			name:     "idle agent compact",
			agent:    AgentInfo{Name: "Agent2", Program: "codex", Model: "gpt5", Status: AgentStatusIdle},
			compact:  true,
			selected: false,
		},
		{
			name:     "idle agent full",
			agent:    AgentInfo{Name: "Agent2b", Program: "codex", Model: "gpt5", Status: AgentStatusIdle},
			compact:  false,
			selected: false,
		},
		{
			name:     "stale agent selected",
			agent:    AgentInfo{Name: "Agent3", Program: "test", Model: "test", Status: AgentStatusStale},
			compact:  false,
			selected: true,
		},
		{
			name:     "stale agent compact",
			agent:    AgentInfo{Name: "Agent3b", Program: "test", Model: "test", Status: AgentStatusStale},
			compact:  true,
			selected: false,
		},
		{
			name:     "ended agent compact",
			agent:    AgentInfo{Name: "Agent4", Program: "test", Model: "test", Status: AgentStatusEnded},
			compact:  true,
			selected: true,
		},
		{
			name:     "ended agent full",
			agent:    AgentInfo{Name: "Agent4b", Program: "test", Model: "test", Status: AgentStatusEnded},
			compact:  false,
			selected: false,
		},
		{
			name:     "unknown status",
			agent:    AgentInfo{Name: "Agent5", Program: "test", Model: "test", Status: "unknown"},
			compact:  false,
			selected: false,
		},
		{
			name:     "unknown status compact",
			agent:    AgentInfo{Name: "Agent5b", Program: "test", Model: "test", Status: "unknown"},
			compact:  true,
			selected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			card := NewAgentCard(tc.agent)
			if tc.compact {
				card.AsCompact()
			}
			card.AsSelected(tc.selected)

			result := card.Render()
			if result == "" {
				t.Error("Render returned empty string")
			}
			// Basic sanity check - should contain agent name
			if !strings.Contains(result, tc.agent.Name) {
				t.Errorf("Render output should contain agent name %q", tc.agent.Name)
			}
		})
	}
}

func TestRenderAgentCard(t *testing.T) {
	agent := AgentInfo{Name: "Test", Program: "test", Model: "test", Status: AgentStatusActive}
	result := RenderAgentCard(agent)
	if result == "" {
		t.Error("RenderAgentCard returned empty string")
	}
}

func TestRenderAgentCardCompact(t *testing.T) {
	agent := AgentInfo{Name: "Test", Program: "test", Model: "test", Status: AgentStatusActive}
	result := RenderAgentCardCompact(agent)
	if result == "" {
		t.Error("RenderAgentCardCompact returned empty string")
	}
}

func TestAgentStatusConstants(t *testing.T) {
	if AgentStatusActive != "active" {
		t.Errorf("AgentStatusActive: expected 'active', got %q", AgentStatusActive)
	}
	if AgentStatusIdle != "idle" {
		t.Errorf("AgentStatusIdle: expected 'idle', got %q", AgentStatusIdle)
	}
	if AgentStatusStale != "stale" {
		t.Errorf("AgentStatusStale: expected 'stale', got %q", AgentStatusStale)
	}
	if AgentStatusEnded != "ended" {
		t.Errorf("AgentStatusEnded: expected 'ended', got %q", AgentStatusEnded)
	}
}

func TestFormatTimeAgo(t *testing.T) {
	tests := []struct {
		name     string
		time     time.Time
		expected string
	}{
		{"zero time", time.Time{}, "never"},
		{"just now", time.Now(), "just now"},
		{"1 min ago", time.Now().Add(-1 * time.Minute), "1 min ago"},
		{"5 mins ago", time.Now().Add(-5 * time.Minute), "5 mins ago"},
		{"1 hour ago", time.Now().Add(-1 * time.Hour), "1 hour ago"},
		{"3 hours ago", time.Now().Add(-3 * time.Hour), "3 hours ago"},
		{"1 day ago", time.Now().Add(-24 * time.Hour), "1 day ago"},
		{"3 days ago", time.Now().Add(-72 * time.Hour), "3 days ago"},
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

// ============== Table Tests ==============

func TestNewTable(t *testing.T) {
	columns := []Column{
		{Header: "ID", Width: 5},
		{Header: "Name", MinWidth: 10},
	}

	table := NewTable(columns)
	if len(table.Columns) != 2 {
		t.Errorf("expected 2 columns, got %d", len(table.Columns))
	}
	if !table.ShowHeader {
		t.Error("ShowHeader should be true by default")
	}
	if !table.Striped {
		t.Error("Striped should be true by default")
	}
	if table.SelectedRow != -1 {
		t.Errorf("SelectedRow should be -1 by default, got %d", table.SelectedRow)
	}
}

func TestTableChaining(t *testing.T) {
	columns := []Column{{Header: "Test"}}
	rows := [][]string{{"row1"}, {"row2"}}

	table := NewTable(columns).
		WithRows(rows).
		WithSelection(1).
		AsCompact().
		WithoutStripes().
		WithMaxWidth(100)

	if len(table.Rows) != 2 {
		t.Errorf("expected 2 rows, got %d", len(table.Rows))
	}
	if table.SelectedRow != 1 {
		t.Errorf("expected selected row 1, got %d", table.SelectedRow)
	}
	if !table.Compact {
		t.Error("expected Compact to be true")
	}
	if table.Striped {
		t.Error("expected Striped to be false")
	}
	if table.MaxWidth != 100 {
		t.Errorf("expected MaxWidth 100, got %d", table.MaxWidth)
	}
}

func TestTableAddRow(t *testing.T) {
	table := NewTable([]Column{{Header: "A"}, {Header: "B"}})
	table.AddRow("val1", "val2")
	table.AddRow("val3", "val4")

	if len(table.Rows) != 2 {
		t.Errorf("expected 2 rows, got %d", len(table.Rows))
	}
	if table.Rows[0][0] != "val1" {
		t.Errorf("expected first cell 'val1', got %q", table.Rows[0][0])
	}
}

func TestTableRender(t *testing.T) {
	columns := []Column{
		{Header: "ID", Width: 5},
		{Header: "Status", Width: 10},
	}
	rows := [][]string{
		{"1", "active"},
		{"2", "pending"},
	}

	table := NewTable(columns).WithRows(rows)
	result := table.Render()

	if result == "" {
		t.Error("Render returned empty string")
	}
	if !strings.Contains(result, "ID") {
		t.Error("Render should contain header 'ID'")
	}
	if !strings.Contains(result, "active") {
		t.Error("Render should contain 'active'")
	}
}

func TestTableRenderEmpty(t *testing.T) {
	// Empty columns should return empty string
	table := NewTable([]Column{})
	result := table.Render()
	if result != "" {
		t.Errorf("expected empty string for empty columns, got %q", result)
	}
}

func TestTableRenderWithSelection(t *testing.T) {
	columns := []Column{{Header: "Name"}}
	rows := [][]string{{"Row1"}, {"Row2"}, {"Row3"}}

	table := NewTable(columns).WithRows(rows).WithSelection(1)
	result := table.Render()

	if result == "" {
		t.Error("Render returned empty string")
	}
}

func TestTableCalculateWidths(t *testing.T) {
	columns := []Column{
		{Header: "ID", Width: 5},              // Fixed
		{Header: "Name", MinWidth: 10},        // Auto with min
		{Header: "Desc", MinWidth: 5, MaxWidth: 20}, // Auto with min/max
	}
	rows := [][]string{
		{"1", "Short", "Very long description here"},
		{"2", "Much longer name here", "Short"},
	}

	table := NewTable(columns).WithRows(rows)
	widths := table.calculateWidths()

	if widths[0] != 5 {
		t.Errorf("expected fixed width 5, got %d", widths[0])
	}
	if widths[1] < 10 {
		t.Errorf("expected min width 10, got %d", widths[1])
	}
	if widths[2] > 20 {
		t.Errorf("expected max width 20, got %d", widths[2])
	}
}

func TestTablePadCell(t *testing.T) {
	table := &Table{}

	tests := []struct {
		content  string
		width    int
		align    lipgloss.Position
		expected string
	}{
		{"test", 10, lipgloss.Left, "test      "},
		{"test", 10, lipgloss.Right, "      test"},
		{"test", 10, lipgloss.Center, "   test   "},
		{"very long text", 7, lipgloss.Left, "very..."},
		{"abc", 2, lipgloss.Left, "ab"},
	}

	for _, tc := range tests {
		t.Run(tc.content, func(t *testing.T) {
			got := table.padCell(tc.content, tc.width, tc.align)
			if got != tc.expected {
				t.Errorf("padCell(%q, %d): expected %q, got %q", tc.content, tc.width, tc.expected, got)
			}
		})
	}
}

func TestRenderTable(t *testing.T) {
	columns := []Column{{Header: "Test"}}
	rows := [][]string{{"value"}}

	result := RenderTable(columns, rows)
	if result == "" {
		t.Error("RenderTable returned empty string")
	}
}

// ============== StatusBadge Tests ==============

func TestNewStatusBadge(t *testing.T) {
	badge := NewStatusBadge("pending")
	if badge.Status != "pending" {
		t.Errorf("expected status 'pending', got %q", badge.Status)
	}
	if !badge.ShowIcon {
		t.Error("ShowIcon should be true by default")
	}
	if badge.Compact {
		t.Error("Compact should be false by default")
	}
}

func TestStatusBadgeChaining(t *testing.T) {
	badge := NewStatusBadge("approved").AsCompact().WithIcon(false)
	if !badge.Compact {
		t.Error("expected Compact to be true")
	}
	if badge.ShowIcon {
		t.Error("expected ShowIcon to be false")
	}
}

func TestStatusBadgeRender(t *testing.T) {
	statuses := []string{
		"pending", "approved", "rejected", "executed",
		"failed", "timeout", "cancelled", "escalated",
		"executing", "unknown",
	}

	for _, status := range statuses {
		t.Run(status, func(t *testing.T) {
			badge := NewStatusBadge(status)
			result := badge.Render()
			if result == "" {
				t.Errorf("Render returned empty string for status %q", status)
			}
		})
	}
}

func TestStatusBadgeRenderCompact(t *testing.T) {
	badge := NewStatusBadge("approved").AsCompact()
	result := badge.Render()
	if result == "" {
		t.Error("Render returned empty string")
	}
}

func TestStatusBadgeRenderNoIcon(t *testing.T) {
	badge := NewStatusBadge("approved").WithIcon(false)
	result := badge.Render()
	if result == "" {
		t.Error("Render returned empty string")
	}
}

func TestRenderStatusBadge(t *testing.T) {
	result := RenderStatusBadge("pending")
	if result == "" {
		t.Error("RenderStatusBadge returned empty string")
	}
}

func TestRenderStatusBadgeCompact(t *testing.T) {
	result := RenderStatusBadgeCompact("pending")
	if result == "" {
		t.Error("RenderStatusBadgeCompact returned empty string")
	}
}

// ============== RiskIndicator Tests ==============

func TestNewRiskIndicator(t *testing.T) {
	indicator := NewRiskIndicator("critical")
	if indicator.Tier != "critical" {
		t.Errorf("expected tier 'critical', got %q", indicator.Tier)
	}
	if !indicator.ShowEmoji {
		t.Error("ShowEmoji should be true by default")
	}
	if !indicator.ShowLabel {
		t.Error("ShowLabel should be true by default")
	}
	if indicator.Compact {
		t.Error("Compact should be false by default")
	}
}

func TestRiskIndicatorChaining(t *testing.T) {
	indicator := NewRiskIndicator("dangerous").AsCompact().WithEmoji(false).WithLabel(false)
	if !indicator.Compact {
		t.Error("expected Compact to be true")
	}
	if indicator.ShowEmoji {
		t.Error("expected ShowEmoji to be false")
	}
	if indicator.ShowLabel {
		t.Error("expected ShowLabel to be false")
	}
}

func TestRiskIndicatorRender(t *testing.T) {
	tiers := []string{"critical", "dangerous", "caution", "safe", "unknown"}

	for _, tier := range tiers {
		t.Run(tier, func(t *testing.T) {
			indicator := NewRiskIndicator(tier)
			result := indicator.Render()
			if result == "" {
				t.Errorf("Render returned empty string for tier %q", tier)
			}
		})
	}
}

func TestRiskIndicatorRenderNoEmojiNoLabel(t *testing.T) {
	// When both are false, should fall back to tier name
	indicator := NewRiskIndicator("critical").WithEmoji(false).WithLabel(false)
	result := indicator.Render()
	if result == "" {
		t.Error("Render returned empty string")
	}
}

func TestRenderRiskIndicator(t *testing.T) {
	result := RenderRiskIndicator("critical")
	if result == "" {
		t.Error("RenderRiskIndicator returned empty string")
	}
}

func TestRenderRiskIndicatorCompact(t *testing.T) {
	result := RenderRiskIndicatorCompact("dangerous")
	if result == "" {
		t.Error("RenderRiskIndicatorCompact returned empty string")
	}
}

func TestTierDescription(t *testing.T) {
	tests := []struct {
		tier     string
		contains string
	}{
		{"critical", "2+"},
		{"dangerous", "1 approval"},
		{"caution", "Auto-approved"},
		{"safe", "No approval"},
		{"unknown", "Unknown"},
	}

	for _, tc := range tests {
		t.Run(tc.tier, func(t *testing.T) {
			desc := TierDescription(tc.tier)
			if !strings.Contains(desc, tc.contains) {
				t.Errorf("TierDescription(%q) should contain %q, got %q", tc.tier, tc.contains, desc)
			}
		})
	}
}

func TestMinApprovals(t *testing.T) {
	tests := []struct {
		tier     string
		expected int
	}{
		{"critical", 2},
		{"dangerous", 1},
		{"caution", 0},
		{"safe", 0},
		{"unknown", 1},
	}

	for _, tc := range tests {
		t.Run(tc.tier, func(t *testing.T) {
			got := MinApprovals(tc.tier)
			if got != tc.expected {
				t.Errorf("MinApprovals(%q): expected %d, got %d", tc.tier, tc.expected, got)
			}
		})
	}
}

// ============== Timeline Tests ==============

func TestNewTimeline(t *testing.T) {
	tl := NewTimeline()
	if tl == nil {
		t.Fatal("NewTimeline returned nil")
	}
	if len(tl.Events) != 0 {
		t.Error("new timeline should have no events")
	}
}

func TestTimelineAddEvent(t *testing.T) {
	tl := NewTimeline()
	now := time.Now()
	tl.AddEvent("created", now, "user", "details")

	if len(tl.Events) != 1 {
		t.Errorf("expected 1 event, got %d", len(tl.Events))
	}
	if tl.Events[0].State != "created" {
		t.Errorf("expected state 'created', got %q", tl.Events[0].State)
	}
	if tl.Events[0].Actor != "user" {
		t.Errorf("expected actor 'user', got %q", tl.Events[0].Actor)
	}
}

func TestTimelineChaining(t *testing.T) {
	tl := NewTimeline().
		WithCurrent("pending").
		AsCompact()

	if tl.Current != "pending" {
		t.Errorf("expected current 'pending', got %q", tl.Current)
	}
	if !tl.Compact {
		t.Error("expected Compact to be true")
	}
}

func TestTimelineRenderNormal(t *testing.T) {
	now := time.Now()
	tl := NewTimeline().
		AddEvent("created", now.Add(-5*time.Minute), "user1", "").
		AddEvent("pending", now.Add(-3*time.Minute), "user1", "").
		AddEvent("approved", now, "user2", "LGTM").
		WithCurrent("approved")

	result := tl.Render()
	if result == "" {
		t.Error("Render returned empty string")
	}
	if !strings.Contains(result, "APPROVED") {
		t.Error("Render should contain 'APPROVED'")
	}
}

func TestTimelineRenderCompact(t *testing.T) {
	now := time.Now()
	tl := NewTimeline().
		AddEvent("created", now, "", "").
		AddEvent("pending", now, "", "").
		WithCurrent("pending").
		AsCompact()

	result := tl.Render()
	if result == "" {
		t.Error("Render returned empty string")
	}
}

func TestTimelineRenderExpanded(t *testing.T) {
	now := time.Now()
	tl := NewTimeline().
		AddEvent("created", now, "user1", "details").
		AddEvent("approved", now, "user2", "more details").
		WithCurrent("approved").
		AsExpanded()

	result := tl.Render()
	if result == "" {
		t.Error("Render returned empty string")
	}
}

func TestTimelineHasReachedState(t *testing.T) {
	tl := NewTimeline().
		AddEvent("created", time.Now(), "", "").
		AddEvent("pending", time.Now(), "", "")

	if !tl.hasReachedState("created") {
		t.Error("should have reached 'created'")
	}
	if !tl.hasReachedState("PENDING") { // Case insensitive
		t.Error("should have reached 'pending'")
	}
	if tl.hasReachedState("approved") {
		t.Error("should not have reached 'approved'")
	}
}

func TestTimelineRenderVariousStates(t *testing.T) {
	states := []string{"approved", "executed", "rejected", "failed", "pending", "executing", "timeout", "escalated", "unknown"}

	for _, state := range states {
		t.Run(state, func(t *testing.T) {
			tl := NewTimeline().
				AddEvent(state, time.Now(), "actor", "").
				WithCurrent(state)

			result := tl.Render()
			if result == "" {
				t.Errorf("Render returned empty for state %q", state)
			}
		})
	}
}

func TestRenderTimeline(t *testing.T) {
	events := []TimelineEvent{
		{State: "created", Timestamp: time.Now()},
		{State: "pending", Timestamp: time.Now()},
	}
	result := RenderTimeline(events, "pending")
	if result == "" {
		t.Error("RenderTimeline returned empty string")
	}
}

func TestRenderTimelineCompact(t *testing.T) {
	events := []TimelineEvent{
		{State: "created", Timestamp: time.Now()},
	}
	result := RenderTimelineCompact(events, "created")
	if result == "" {
		t.Error("RenderTimelineCompact returned empty string")
	}
}

// ============== CommandBox Tests ==============

func TestNewCommandBox(t *testing.T) {
	box := NewCommandBox("ls -la")
	if box.Command != "ls -la" {
		t.Errorf("expected command 'ls -la', got %q", box.Command)
	}
	if box.MaxWidth != 80 {
		t.Errorf("expected default MaxWidth 80, got %d", box.MaxWidth)
	}
	if !box.ShowHint {
		t.Error("ShowHint should be true by default")
	}
}

func TestCommandBoxChaining(t *testing.T) {
	box := NewCommandBox("cmd").
		WithRedacted("***").
		WithMaxWidth(50).
		WithHint(false)

	if box.Redacted != "***" {
		t.Errorf("expected redacted '***', got %q", box.Redacted)
	}
	if box.MaxWidth != 50 {
		t.Errorf("expected MaxWidth 50, got %d", box.MaxWidth)
	}
	if box.ShowHint {
		t.Error("expected ShowHint to be false")
	}
}

func TestCommandBoxRender(t *testing.T) {
	box := NewCommandBox("ls -la")
	result := box.Render()
	if result == "" {
		t.Error("Render returned empty string")
	}
	if !strings.Contains(result, "ls -la") {
		t.Error("Render should contain command")
	}
}

func TestCommandBoxRenderRedacted(t *testing.T) {
	box := NewCommandBox("secret command").WithRedacted("***redacted***")
	result := box.Render()
	if !strings.Contains(result, "***redacted***") {
		t.Error("Render should show redacted version")
	}
}

func TestCommandBoxRenderTruncated(t *testing.T) {
	longCmd := strings.Repeat("x", 100)
	box := NewCommandBox(longCmd).WithMaxWidth(20)
	result := box.Render()
	if result == "" {
		t.Error("Render returned empty string")
	}
}

func TestCommandBoxRenderCompact(t *testing.T) {
	box := NewCommandBox("ls -la")
	result := box.RenderCompact()
	if result == "" {
		t.Error("RenderCompact returned empty string")
	}
}

func TestCommandBoxRenderCompactRedacted(t *testing.T) {
	box := NewCommandBox("secret").WithRedacted("***")
	result := box.RenderCompact()
	if !strings.Contains(result, "***") {
		t.Error("RenderCompact should show redacted version")
	}
}

func TestCommandBoxRenderCompactTruncated(t *testing.T) {
	longCmd := strings.Repeat("x", 100)
	box := NewCommandBox(longCmd)
	result := box.RenderCompact()
	if !strings.Contains(result, "...") {
		t.Error("RenderCompact should truncate long commands")
	}
}

func TestCommandBoxRenderFull(t *testing.T) {
	box := NewCommandBox("ls -la")
	result := box.RenderFull()
	if result == "" {
		t.Error("RenderFull returned empty string")
	}
}

func TestCommandBoxRenderFullWithDifferentRedacted(t *testing.T) {
	box := NewCommandBox("secret command").WithRedacted("public view")
	result := box.RenderFull()
	if !strings.Contains(result, "redacted") {
		t.Error("RenderFull should note when content is redacted")
	}
}

// ============== Spinner Tests ==============

func TestNewSpinner(t *testing.T) {
	styles := []SpinnerStyle{
		SpinnerStyleDots, SpinnerStyleLine, SpinnerStyleMiniDot,
		SpinnerStyleJump, SpinnerStylePulse, SpinnerStylePoints,
		SpinnerStyleGlobe, SpinnerStyleMoon, SpinnerStyleMonkey,
		SpinnerStyleMeter, SpinnerStyleHamburger,
	}

	for _, style := range styles {
		t.Run("style", func(t *testing.T) {
			s := NewSpinner(style)
			// Just verify it creates without panic
			_ = s.View()
		})
	}
}

func TestNewSpinnerDefault(t *testing.T) {
	s := NewSpinner(SpinnerStyle(999)) // Invalid style
	_ = s.View()                       // Should not panic, uses default
}

func TestDefaultSpinner(t *testing.T) {
	s := DefaultSpinner()
	_ = s.View()
}

func TestLoadingSpinner(t *testing.T) {
	s := LoadingSpinner()
	_ = s.View()
}

func TestProcessingSpinner(t *testing.T) {
	s := ProcessingSpinner()
	_ = s.View()
}

func TestWaitingSpinner(t *testing.T) {
	s := WaitingSpinner()
	_ = s.View()
}

func TestSpinnerWithLabel(t *testing.T) {
	s := DefaultSpinner()
	result := SpinnerWithLabel(s, "Loading...")
	if !strings.Contains(result, "Loading...") {
		t.Error("SpinnerWithLabel should contain label")
	}
}

func TestSpinnerStyleConstants(t *testing.T) {
	// Verify constants are distinct
	styles := []SpinnerStyle{
		SpinnerStyleDots, SpinnerStyleLine, SpinnerStyleMiniDot,
		SpinnerStyleJump, SpinnerStylePulse, SpinnerStylePoints,
		SpinnerStyleGlobe, SpinnerStyleMoon, SpinnerStyleMonkey,
		SpinnerStyleMeter, SpinnerStyleHamburger,
	}

	seen := make(map[SpinnerStyle]bool)
	for _, s := range styles {
		if seen[s] {
			t.Errorf("duplicate spinner style value: %d", s)
		}
		seen[s] = true
	}
}
