package dashboard

import (
	"fmt"
	"os"
	"path/filepath"
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

	ready  bool
	width  int
	height int

	focus focusPanel

	agents   []components.AgentInfo
	pending  []requestRow
	activity []string

	agentSel int
	agentOff int

	pendingSel int
	pendingOff int

	activitySel int
	activityOff int

	lastErr     error
	lastRefresh time.Time

	// Callbacks
	OnPatterns func() // Navigate to pattern management view
	OnHistory  func() // Navigate to history view
}

// New creates a dashboard model for a project.
func New(projectPath string) Model {
	if projectPath == "" {
		if pwd, err := os.Getwd(); err == nil {
			projectPath = pwd
		}
	}
	return Model{
		projectPath: projectPath,
		focus:       focusPending,
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(loadCmd(m.projectPath), tickCmd())
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		return m, nil
	case refreshMsg:
		return m, tea.Batch(loadCmd(m.projectPath), tickCmd())
	case dataMsg:
		m.agents = msg.agents
		m.pending = msg.pending
		m.activity = msg.activity
		m.lastErr = msg.err
		m.lastRefresh = msg.refreshedAt

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
			return m, nil
		case "shift+tab":
			m.focus = (m.focus + 2) % 3
			return m, nil
		case "left":
			m.focus = (m.focus + 2) % 3
			return m, nil
		case "right", "l":
			m.focus = (m.focus + 1) % 3
			return m, nil
		case "up", "k":
			m.moveSelection(-1)
			return m, nil
		case "down", "j":
			m.moveSelection(1)
			return m, nil
		case "m":
			if m.OnPatterns != nil {
				m.OnPatterns()
			}
			return m, nil
		case "h":
			if m.OnHistory != nil {
				m.OnHistory()
			} else {
				// Fallback to left focus if no handler
				m.focus = (m.focus + 2) % 3
			}
			return m, nil
		}
	}

	return m, nil
}

func (m Model) View() string {
	if !m.ready {
		return "Loading..."
	}

	th := theme.Current

	header := m.renderHeader()
	footer := m.renderFooter()

	bodyHeight := m.height - lipgloss.Height(header) - lipgloss.Height(footer)
	if bodyHeight < 6 {
		bodyHeight = 6
	}

	gap := 1
	leftW := maxInt(28, m.width/4)
	rightW := maxInt(28, m.width/4)
	centerW := m.width - leftW - rightW - (2 * gap)
	if centerW < 30 {
		centerW = 30
	}

	agentsPanel := m.renderAgentsPanel(leftW, bodyHeight)
	pendingPanel := m.renderPendingPanel(centerW, bodyHeight)
	activityPanel := m.renderActivityPanel(rightW, bodyHeight)

	body := lipgloss.JoinHorizontal(lipgloss.Top,
		agentsPanel,
		lipgloss.NewStyle().Width(gap).Render(""),
		pendingPanel,
		lipgloss.NewStyle().Width(gap).Render(""),
		activityPanel,
	)

	// Keep the whole view on a consistent background.
	page := lipgloss.NewStyle().Background(th.Base).Render(
		lipgloss.JoinVertical(lipgloss.Left, header, body, footer),
	)
	return page
}

func (m Model) renderHeader() string {
	th := theme.Current

	title := lipgloss.NewStyle().Foreground(th.Mauve).Bold(true).Render("SLB Dashboard")
	statusDot := lipgloss.NewStyle().Foreground(th.Yellow).Render("●")
	daemon := lipgloss.NewStyle().Foreground(th.Subtext).Render(fmt.Sprintf("%s Daemon: unknown", statusDot))

	row := lipgloss.JoinHorizontal(lipgloss.Top,
		title,
		lipgloss.NewStyle().Width(maxInt(0, m.width-lipgloss.Width(title)-lipgloss.Width(daemon))).Render(""),
		daemon,
	)

	return lipgloss.NewStyle().
		Background(th.Mantle).
		Foreground(th.Text).
		Padding(0, 1).
		Width(maxInt(0, m.width)).
		Render(row)
}

func (m Model) renderFooter() string {
	th := theme.Current

	hint := lipgloss.NewStyle().Foreground(th.Subtext).Render("[tab] focus  [↑/↓] navigate  [enter] details  [m] patterns  [h] history  [q] quit")

	right := ""
	if !m.lastRefresh.IsZero() {
		right = "refreshed " + formatTimeAgo(m.lastRefresh)
	}
	if m.lastErr != nil {
		right = "error: " + m.lastErr.Error()
	}
	rightStyled := lipgloss.NewStyle().Foreground(th.Subtext).Render(right)

	row := lipgloss.JoinHorizontal(lipgloss.Top,
		hint,
		lipgloss.NewStyle().Width(maxInt(0, m.width-lipgloss.Width(hint)-lipgloss.Width(rightStyled))).Render(""),
		rightStyled,
	)

	return lipgloss.NewStyle().
		Background(th.Mantle).
		Padding(0, 1).
		Width(maxInt(0, m.width)).
		Render(row)
}

func (m Model) renderAgentsPanel(width, height int) string {
	th := theme.Current

	title := lipgloss.NewStyle().Foreground(th.Blue).Bold(true).Render(fmt.Sprintf("Agents (%d)", len(m.agents)))

	lines := []string{title}
	visible := maxInt(1, height-4) // title + border padding
	start, end := window(m.agentOff, len(m.agents), visible)

	for i := start; i < end; i++ {
		card := components.NewAgentCard(m.agents[i]).
			AsCompact().
			AsSelected(i == m.agentSel && m.focus == focusAgents).
			WithWidth(width - 4)
		lines = append(lines, card.Render())
	}
	if len(m.agents) == 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(th.Subtext).Render("No active sessions"))
	}

	borderColor := th.Overlay0
	if m.focus == focusAgents {
		borderColor = th.Mauve
	}

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Padding(0, 1).
		Width(width).
		Height(height).
		Render(strings.Join(lines, "\n"))
}

func (m Model) renderPendingPanel(width, height int) string {
	th := theme.Current

	title := lipgloss.NewStyle().Foreground(th.Blue).Bold(true).Render(fmt.Sprintf("Pending Requests (%d)", len(m.pending)))
	lines := []string{title}

	visible := maxInt(1, height-4)
	start, end := window(m.pendingOff, len(m.pending), visible)

	lineStyle := lipgloss.NewStyle().Foreground(th.Text)
	selectedStyle := lipgloss.NewStyle().Foreground(th.Text).Background(th.Surface1).Bold(true)

	for i := start; i < end; i++ {
		r := m.pending[i]
		emoji := theme.TierEmoji(r.Tier)
		age := formatTimeAgo(r.CreatedAt)
		label := fmt.Sprintf("%s %s  •  %s  •  %s", emoji, r.Command, r.Requestor, age)
		label = truncateRunes(label, width-4)

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

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Padding(0, 1).
		Width(width).
		Height(height).
		Render(strings.Join(lines, "\n"))
}

func (m Model) renderActivityPanel(width, height int) string {
	th := theme.Current

	title := lipgloss.NewStyle().Foreground(th.Blue).Bold(true).Render("Recent Activity")
	lines := []string{title}

	visible := maxInt(1, height-4)
	start, end := window(m.activityOff, len(m.activity), visible)

	lineStyle := lipgloss.NewStyle().Foreground(th.Text)
	selectedStyle := lipgloss.NewStyle().Foreground(th.Text).Background(th.Surface1).Bold(true)

	for i := start; i < end; i++ {
		line := truncateRunes(m.activity[i], width-4)
		style := lineStyle
		if i == m.activitySel && m.focus == focusActivity {
			style = selectedStyle
		}
		lines = append(lines, style.Render(line))
	}

	if len(m.activity) == 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(th.Subtext).Render("No recent activity"))
	}

	borderColor := th.Overlay0
	if m.focus == focusActivity {
		borderColor = th.Mauve
	}

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Padding(0, 1).
		Width(width).
		Height(height).
		Render(strings.Join(lines, "\n"))
}

func (m *Model) visibleRows() int {
	// A conservative estimate used for keeping selection/offset stable.
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

// SelectedRequestID returns the ID of the currently selected pending request.
// Returns empty string if no request is selected or if the pending requests panel is not focused.
func (m *Model) SelectedRequestID() string {
	if m.focus != focusPending {
		return ""
	}
	if m.pendingSel < 0 || m.pendingSel >= len(m.pending) {
		return ""
	}
	return m.pending[m.pendingSel].ID
}

// IsPendingFocused returns true if the pending requests panel is focused.
func (m *Model) IsPendingFocused() bool {
	return m.focus == focusPending
}

func tickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(time.Time) tea.Msg { return refreshMsg{} })
}

func loadCmd(projectPath string) tea.Cmd {
	return func() tea.Msg {
		agents, pending, activity, err := loadData(projectPath)
		return dataMsg{
			agents:      agents,
			pending:     pending,
			activity:    activity,
			err:         err,
			refreshedAt: time.Now().UTC(),
		}
	}
}

func loadData(projectPath string) ([]components.AgentInfo, []requestRow, []string, error) {
	dbPath := filepath.Join(projectPath, ".slb", "state.db")
	dbConn, err := db.OpenWithOptions(dbPath, db.OpenOptions{
		CreateIfNotExists: false,
		InitSchema:        false,
		ReadOnly:          true,
	})
	if err != nil {
		// Dashboard is still useful without a DB; treat as empty data.
		return []components.AgentInfo{}, []requestRow{}, []string{}, err
	}
	defer dbConn.Close()

	sessions, err := dbConn.ListActiveSessions(projectPath)
	if err != nil {
		return []components.AgentInfo{}, []requestRow{}, []string{}, err
	}
	agents := make([]components.AgentInfo, 0, len(sessions))
	for _, s := range sessions {
		agents = append(agents, components.AgentInfo{
			Name:        s.AgentName,
			Program:     s.Program,
			Model:       s.Model,
			Status:      classifyAgentStatus(s.LastActiveAt),
			LastActive:  s.LastActiveAt,
			SessionID:   s.ID,
			ProjectPath: s.ProjectPath,
		})
	}

	reqs, err := dbConn.ListPendingRequests(projectPath)
	if err != nil {
		return agents, []requestRow{}, []string{}, err
	}
	pending := make([]requestRow, 0, len(reqs))
	for _, r := range reqs {
		cmd := r.Command.DisplayRedacted
		if cmd == "" {
			cmd = r.Command.Raw
		}
		pending = append(pending, requestRow{
			ID:        r.ID,
			Tier:      string(r.RiskTier),
			Command:   cmd,
			Requestor: r.RequestorAgent,
			CreatedAt: r.CreatedAt,
		})
	}

	// Minimal activity stream: derive from pending requests for now.
	activity := make([]string, 0, minInt(10, len(pending)))
	for i := 0; i < len(pending) && i < 10; i++ {
		p := pending[i]
		activity = append(activity, fmt.Sprintf("Pending %s by %s (%s)", shortID(p.ID), p.Requestor, formatTimeAgo(p.CreatedAt)))
	}

	return agents, pending, activity, nil
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
	start = offset
	end = start + visible
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
