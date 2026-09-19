package dashboard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/tui/components"
	"github.com/Dicklesworthstone/slb/internal/tui/theme"
)

const refreshInterval = 2 * time.Second

type focusPanel int

const (
	focusAgents focusPanel = iota
	focusPending
	focusActivity
)

type requestRow struct {
	ID        string
	Tier      string
	Status    string
	Command   string
	Requestor string
	CreatedAt time.Time
}

type refreshMsg struct{}

type dataMsg struct {
	agents      []components.AgentInfo
	pending     []requestRow
	activity    []string
	err         error
	refreshedAt time.Time
}

// Model is the main dashboard Bubble Tea model.
type Model struct {
	projectPath string
	ready       bool
	width       int
	height      int
	focus       focusPanel
	agents      []components.AgentInfo
	pending     []requestRow
	activity    []string
	agentSel    int
	agentOff    int
	pendingSel  int
	pendingOff  int
	activitySel int
	activityOff int
	lastErr     error
	lastRefresh time.Time

	OnPatterns func()
	OnHistory  func()
}

func New(projectPath string) Model {
	if projectPath == "" {
		if pwd, err := os.Getwd(); err == nil {
			projectPath = pwd
		}
	}
	return Model{projectPath: projectPath, focus: focusPending}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(loadCmd(m.projectPath), tickCmd())
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height, m.ready = msg.Width, msg.Height, true
		return m, nil
	case refreshMsg:
		return m, tea.Batch(loadCmd(m.projectPath), tickCmd())
	case dataMsg:
		m.lastErr = msg.err
		if msg.err != nil {
			return m, nil // Keep last-known state; do not pretend the queue emptied.
		}
		selectedID := ""
		if m.pendingSel >= 0 && m.pendingSel < len(m.pending) {
			selectedID = m.pending[m.pendingSel].ID
		}
		m.agents, m.pending, m.activity = msg.agents, msg.pending, msg.activity
		m.lastRefresh = msg.refreshedAt
		// New escalations can reorder the queue. Preserve the chosen request,
		// not merely its old row number, before the user presses Enter.
		for i, row := range m.pending {
			if row.ID == selectedID {
				m.pendingSel = i
				break
			}
		}
		m.agentSel, m.agentOff = clampSelection(m.agentSel, m.agentOff, len(m.agents), m.visibleRows())
		m.pendingSel, m.pendingOff = clampSelection(m.pendingSel, m.pendingOff, len(m.pending), m.visibleRows())
		m.activitySel, m.activityOff = clampSelection(m.activitySel, m.activityOff, len(m.activity), m.visibleRows())
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "tab":
			m.focus = (m.focus + 1) % 3
		case "shift+tab", "left":
			m.focus = (m.focus + 2) % 3
		case "right", "l":
			m.focus = (m.focus + 1) % 3
		case "up", "k":
			m.moveSelection(-1)
		case "down", "j":
			m.moveSelection(1)
		case "m":
			if m.OnPatterns != nil {
				m.OnPatterns()
			}
		case "h":
			if m.OnHistory != nil {
				m.OnHistory()
			} else {
				m.focus = (m.focus + 2) % 3
			}
		}
		return m, nil
	}
	return m, nil
}

func (m Model) View() string {
	if !m.ready {
		return "Loading..."
	}
	th := theme.Current
	header, footer := m.renderHeader(), m.renderFooter()
	bodyHeight := maxInt(6, m.height-lipgloss.Height(header)-lipgloss.Height(footer))
	gap := 1
	leftW, rightW := maxInt(28, m.width/4), maxInt(28, m.width/4)
	centerW := maxInt(30, m.width-leftW-rightW-2*gap)
	body := lipgloss.JoinHorizontal(lipgloss.Top,
		m.renderAgentsPanel(leftW, bodyHeight),
		lipgloss.NewStyle().Width(gap).Render(""),
		m.renderPendingPanel(centerW, bodyHeight),
		lipgloss.NewStyle().Width(gap).Render(""),
		m.renderActivityPanel(rightW, bodyHeight),
	)
	return lipgloss.NewStyle().Background(th.Base).Render(lipgloss.JoinVertical(lipgloss.Left, header, body, footer))
}

func (m Model) renderHeader() string {
	th := theme.Current
	title := lipgloss.NewStyle().Foreground(th.Mauve).Bold(true).Render("SLB Dashboard")
	status := lipgloss.NewStyle().Foreground(th.Subtext).Render("Project database • polling")
	row := lipgloss.JoinHorizontal(lipgloss.Top, title,
		lipgloss.NewStyle().Width(maxInt(0, m.width-lipgloss.Width(title)-lipgloss.Width(status))).Render(""), status)
	return lipgloss.NewStyle().Background(th.Mantle).Foreground(th.Text).Padding(0, 1).Width(maxInt(0, m.width)).Render(row)
}

func (m Model) renderFooter() string {
	th := theme.Current
	hint := lipgloss.NewStyle().Foreground(th.Subtext).Render("[tab] focus  [↑/↓] navigate  [enter] details  [m] patterns  [H] history  [q] quit")
	right := ""
	if !m.lastRefresh.IsZero() {
		right = "refreshed " + formatTimeAgo(m.lastRefresh)
	}
	if m.lastErr != nil {
		right = "error (showing last known state): " + m.lastErr.Error()
	}
	rightStyled := lipgloss.NewStyle().Foreground(th.Subtext).Render(right)
	row := lipgloss.JoinHorizontal(lipgloss.Top, hint,
		lipgloss.NewStyle().Width(maxInt(0, m.width-lipgloss.Width(hint)-lipgloss.Width(rightStyled))).Render(""), rightStyled)
	return lipgloss.NewStyle().Background(th.Mantle).Padding(0, 1).Width(maxInt(0, m.width)).Render(row)
}

func (m Model) renderAgentsPanel(width, height int) string {
	th := theme.Current
	title := lipgloss.NewStyle().Foreground(th.Blue).Bold(true).Render(fmt.Sprintf("Agents (%d)", len(m.agents)))
	lines := []string{title}
	start, end := window(m.agentOff, len(m.agents), maxInt(1, height-4))
	for i := start; i < end; i++ {
		card := components.NewAgentCard(m.agents[i]).AsCompact().AsSelected(i == m.agentSel && m.focus == focusAgents).WithWidth(width - 4)
		lines = append(lines, card.Render())
	}
	if len(m.agents) == 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(th.Subtext).Render("No active sessions"))
	}
	borderColor := th.Overlay0
	if m.focus == focusAgents {
		borderColor = th.Mauve
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(borderColor).Padding(0, 1).Width(width).Height(height).Render(strings.Join(lines, "\n"))
}

func (m Model) renderPendingPanel(width, height int) string {
	th := theme.Current
	title := lipgloss.NewStyle().Foreground(th.Blue).Bold(true).Render(fmt.Sprintf("Pending Requests (%d, incl. escalated)", len(m.pending)))
	lines := []string{title}
	start, end := window(m.pendingOff, len(m.pending), maxInt(1, height-4))
	lineStyle := lipgloss.NewStyle().Foreground(th.Text)
	selectedStyle := lipgloss.NewStyle().Foreground(th.Text).Background(th.Surface1).Bold(true)
	for i := start; i < end; i++ {
		r := m.pending[i]
		prefix := theme.TierEmoji(r.Tier)
		if r.Status == string(db.StatusEscalated) {
			prefix = "ESCALATED " + prefix
		}
		label := truncateRunes(fmt.Sprintf("%s %s  •  %s  •  %s", prefix, r.Command, r.Requestor, formatTimeAgo(r.CreatedAt)), width-4)
		style := lineStyle
		if i == m.pendingSel && m.focus == focusPending {
			style = selectedStyle
		}
		lines = append(lines, style.Render(label))
	}
	if len(m.pending) == 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(th.Subtext).Render("No pending requests"))
	}
	borderColor := th.Overlay0
	if m.focus == focusPending {
		borderColor = th.Mauve
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(borderColor).Padding(0, 1).Width(width).Height(height).Render(strings.Join(lines, "\n"))
}

func (m Model) renderActivityPanel(width, height int) string {
	th := theme.Current
	title := lipgloss.NewStyle().Foreground(th.Blue).Bold(true).Render("Recent Activity")
	lines := []string{title}
	start, end := window(m.activityOff, len(m.activity), maxInt(1, height-4))
	lineStyle := lipgloss.NewStyle().Foreground(th.Text)
	selectedStyle := lipgloss.NewStyle().Foreground(th.Text).Background(th.Surface1).Bold(true)
	for i := start; i < end; i++ {
		style := lineStyle
		if i == m.activitySel && m.focus == focusActivity {
			style = selectedStyle
		}
		lines = append(lines, style.Render(truncateRunes(m.activity[i], width-4)))
	}
	if len(m.activity) == 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(th.Subtext).Render("No recent activity"))
	}
	borderColor := th.Overlay0
	if m.focus == focusActivity {
		borderColor = th.Mauve
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(borderColor).Padding(0, 1).Width(width).Height(height).Render(strings.Join(lines, "\n"))
}

func (m *Model) visibleRows() int {
	if m.height <= 0 {
		return 6
	}
	return maxInt(3, m.height-8)
}

func (m *Model) moveSelection(delta int) {
	switch m.focus {
	case focusAgents:
		m.agentSel += delta
		m.agentSel, m.agentOff = clampSelection(m.agentSel, m.agentOff, len(m.agents), m.visibleRows())
	case focusPending:
		m.pendingSel += delta
		m.pendingSel, m.pendingOff = clampSelection(m.pendingSel, m.pendingOff, len(m.pending), m.visibleRows())
	case focusActivity:
		m.activitySel += delta
		m.activitySel, m.activityOff = clampSelection(m.activitySel, m.activityOff, len(m.activity), m.visibleRows())
	}
}

func (m *Model) SelectedRequestID() string {
	if m.focus != focusPending || m.pendingSel < 0 || m.pendingSel >= len(m.pending) {
		return ""
	}
	return m.pending[m.pendingSel].ID
}

func (m *Model) IsPendingFocused() bool { return m.focus == focusPending }

func tickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(time.Time) tea.Msg { return refreshMsg{} })
}

func loadCmd(projectPath string) tea.Cmd {
	return func() tea.Msg {
		agents, pending, activity, err := loadData(projectPath)
		return dataMsg{agents: agents, pending: pending, activity: activity, err: err, refreshedAt: time.Now().UTC()}
	}
}

func loadData(projectPath string) ([]components.AgentInfo, []requestRow, []string, error) {
	database, err := db.OpenWithOptions(filepath.Join(projectPath, ".slb", "state.db"), db.OpenOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, nil, err
	}
	defer database.Close()
	sessions, err := database.ListActiveSessions(projectPath)
	if err != nil {
		return nil, nil, nil, err
	}
	agents := make([]components.AgentInfo, 0, len(sessions))
	for _, s := range sessions {
		agents = append(agents, components.AgentInfo{Name: s.AgentName, Program: s.Program, Model: s.Model,
			Status: classifyAgentStatus(s.LastActiveAt), LastActive: s.LastActiveAt, SessionID: s.ID, ProjectPath: s.ProjectPath})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	state, err := database.ReadProjectWatchState(ctx, projectPath, time.Now().Add(-24*time.Hour))
	if err != nil {
		return nil, nil, nil, err
	}
	pending, activity, err := projectStateRows(state)
	return agents, pending, activity, err
}

// projectStateRows uses the shared bounded, redacted DB projection. Escalated
// requests stay actionable; activity reflects persisted state, not fabricated
// events derived solely from the pending list. This is not a durable journal.
func projectStateRows(state *db.ProjectWatchState) ([]requestRow, []string, error) {
	pending := make([]requestRow, 0)
	type recentState struct {
		id, status, requestor string
		at                    time.Time
	}
	recent := make([]recentState, 0, len(state.Requests))
	for _, r := range state.Requests {
		created, err := time.Parse(time.RFC3339Nano, r.CreatedAt)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid creation time for request %s: %w", r.ID, err)
		}
		at := created
		if r.ResolvedAt != "" {
			at, err = time.Parse(time.RFC3339Nano, r.ResolvedAt)
			if err != nil {
				return nil, nil, fmt.Errorf("invalid resolution time for request %s: %w", r.ID, err)
			}
		}
		if r.Status == db.StatusPending || r.Status == db.StatusEscalated {
			pending = append(pending, requestRow{ID: r.ID, Tier: string(r.RiskTier), Status: string(r.Status), Command: r.Command, Requestor: r.Requestor, CreatedAt: created})
		}
		recent = append(recent, recentState{r.ID, string(r.Status), r.Requestor, at})
	}
	sort.Slice(pending, func(i, j int) bool {
		ei, ej := pending[i].Status == string(db.StatusEscalated), pending[j].Status == string(db.StatusEscalated)
		if ei != ej {
			return ei
		}
		if !pending[i].CreatedAt.Equal(pending[j].CreatedAt) {
			return pending[i].CreatedAt.Before(pending[j].CreatedAt)
		}
		return pending[i].ID < pending[j].ID
	})
	sort.Slice(recent, func(i, j int) bool {
		if !recent[i].at.Equal(recent[j].at) {
			return recent[i].at.After(recent[j].at)
		}
		return recent[i].id < recent[j].id
	})
	activity := make([]string, 0, minInt(10, len(recent)))
	for i := 0; i < len(recent) && i < 10; i++ {
		r := recent[i]
		activity = append(activity, fmt.Sprintf("%s %s by %s (%s)", r.status, shortID(r.id), r.requestor, formatTimeAgo(r.at)))
	}
	return pending, activity, nil
}

func classifyAgentStatus(lastActive time.Time) components.AgentStatus {
	if lastActive.IsZero() {
		return components.AgentStatusStale
	}
	d := time.Since(lastActive)
	switch {
	case d < 5*time.Minute:
		return components.AgentStatusActive
	case d < 30*time.Minute:
		return components.AgentStatusIdle
	default:
		return components.AgentStatusStale
	}
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func window(offset, total, visible int) (start, end int) {
	if visible <= 0 {
		visible = 1
	}
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	start, end = offset, offset+visible
	if end > total {
		end = total
	}
	return start, end
}

func clampSelection(sel, off, total, visible int) (newSel, newOff int) {
	if total <= 0 {
		return 0, 0
	}
	if sel < 0 {
		sel = 0
	}
	if sel >= total {
		sel = total - 1
	}
	if visible <= 0 {
		visible = 1
	}
	if sel < off {
		off = sel
	}
	if sel >= off+visible {
		off = sel - visible + 1
	}
	if off < 0 {
		off = 0
	}
	if off > total-1 {
		off = total - 1
	}
	return sel, off
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	if max <= 3 {
		return string(rs[:max])
	}
	return string(rs[:max-3]) + "..."
}

func formatTimeAgo(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		mins := int(d.Minutes())
		if mins == 1 {
			return "1m ago"
		}
		return fmt.Sprintf("%dm ago", mins)
	case d < 24*time.Hour:
		hours := int(d.Hours())
		if hours == 1 {
			return "1h ago"
		}
		return fmt.Sprintf("%dh ago", hours)
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1d ago"
		}
		return fmt.Sprintf("%dd ago", days)
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
