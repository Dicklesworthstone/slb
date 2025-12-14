// Package patterns provides TUI views for pattern management.
package patterns

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/tui/components"
	"github.com/Dicklesworthstone/slb/internal/tui/theme"
)

const refreshInterval = 5 * time.Second

// RemovalKeyMap defines keybindings for the removal review panel.
type RemovalKeyMap struct {
	Approve    key.Binding
	Reject     key.Binding
	Up         key.Binding
	Down       key.Binding
	Back       key.Binding
	Quit       key.Binding
	FilterType key.Binding
	Refresh    key.Binding
}

// DefaultRemovalKeyMap returns the default keybindings.
func DefaultRemovalKeyMap() RemovalKeyMap {
	return RemovalKeyMap{
		Approve: key.NewBinding(
			key.WithKeys("a", "enter"),
			key.WithHelp("a", "approve"),
		),
		Reject: key.NewBinding(
			key.WithKeys("r"),
			key.WithHelp("r", "reject"),
		),
		Up: key.NewBinding(
			key.WithKeys("up", "k"),
			key.WithHelp("↑", "up"),
		),
		Down: key.NewBinding(
			key.WithKeys("down", "j"),
			key.WithHelp("↓", "down"),
		),
		Back: key.NewBinding(
			key.WithKeys("esc", "q"),
			key.WithHelp("esc", "back"),
		),
		Quit: key.NewBinding(
			key.WithKeys("ctrl+c"),
			key.WithHelp("ctrl+c", "quit"),
		),
		FilterType: key.NewBinding(
			key.WithKeys("f"),
			key.WithHelp("f", "filter type"),
		),
		Refresh: key.NewBinding(
			key.WithKeys("ctrl+r"),
			key.WithHelp("ctrl+r", "refresh"),
		),
	}
}

// RemovalRow represents a single pattern change row.
type RemovalRow struct {
	ID         int64
	Tier       string
	Pattern    string
	ChangeType string
	Reason     string
	Status     string
	CreatedAt  time.Time
}

// Model is the Bubble Tea model for the pattern removal review panel.
type Model struct {
	projectPath string
	keyMap      RemovalKeyMap

	// View state
	ready  bool
	width  int
	height int

	// Data
	rows       []RemovalRow
	totalCount int

	// Selection
	selectedIdx int

	// Filters
	filterType string // "", "remove", "suggest", "add"

	// Callbacks
	OnBack    func()
	OnApprove func(id int64)
	OnReject  func(id int64)

	// Error/success messages
	message     string
	messageType string // "success", "error"

	// Error state
	lastErr     error
	lastRefresh time.Time
}

// refreshMsg triggers a data refresh.
type refreshMsg struct{}

// dataMsg contains loaded data.
type dataMsg struct {
	rows        []RemovalRow
	totalCount  int
	err         error
	refreshedAt time.Time
}

// actionMsg contains result of an action.
type actionMsg struct {
	action  string // "approve", "reject"
	id      int64
	success bool
	err     error
}

// New creates a new pattern removal review model.
func New(projectPath string) Model {
	if projectPath == "" {
		if pwd, err := os.Getwd(); err == nil {
			projectPath = pwd
		}
	}

	return Model{
		projectPath: projectPath,
		keyMap:      DefaultRemovalKeyMap(),
		filterType:  "", // Show all by default
	}
}

// Init initializes the model.
func (m Model) Init() tea.Cmd {
	return tea.Batch(loadDataCmd(m.projectPath, m.filterType), tickCmd())
}

// Update handles messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		return m, nil

	case refreshMsg:
		return m, tea.Batch(loadDataCmd(m.projectPath, m.filterType), tickCmd())

	case dataMsg:
		m.rows = msg.rows
		m.totalCount = msg.totalCount
		m.lastErr = msg.err
		m.lastRefresh = msg.refreshedAt
		// Clamp selection
		if m.selectedIdx >= len(m.rows) {
			m.selectedIdx = max(0, len(m.rows)-1)
		}
		return m, nil

	case actionMsg:
		if msg.success {
			m.message = fmt.Sprintf("Pattern change #%d %sd", msg.id, msg.action)
			m.messageType = "success"
		} else {
			m.message = fmt.Sprintf("Failed to %s: %v", msg.action, msg.err)
			m.messageType = "error"
		}
		// Refresh data after action
		return m, loadDataCmd(m.projectPath, m.filterType)

	case tea.KeyMsg:
		// Clear message on any keypress
		if m.message != "" {
			m.message = ""
		}

		switch {
		case key.Matches(msg, m.keyMap.Back):
			if m.OnBack != nil {
				m.OnBack()
			}
			return m, nil

		case key.Matches(msg, m.keyMap.Quit):
			return m, tea.Quit

		case key.Matches(msg, m.keyMap.Up):
			if m.selectedIdx > 0 {
				m.selectedIdx--
			}
			return m, nil

		case key.Matches(msg, m.keyMap.Down):
			if m.selectedIdx < len(m.rows)-1 {
				m.selectedIdx++
			}
			return m, nil

		case key.Matches(msg, m.keyMap.Approve):
			if len(m.rows) > 0 && m.selectedIdx < len(m.rows) {
				row := m.rows[m.selectedIdx]
				if row.Status == db.PatternChangeStatusPending {
					return m, approveCmd(m.projectPath, row.ID)
				}
			}
			return m, nil

		case key.Matches(msg, m.keyMap.Reject):
			if len(m.rows) > 0 && m.selectedIdx < len(m.rows) {
				row := m.rows[m.selectedIdx]
				if row.Status == db.PatternChangeStatusPending {
					return m, rejectCmd(m.projectPath, row.ID)
				}
			}
			return m, nil

		case key.Matches(msg, m.keyMap.FilterType):
			m.cycleFilterType()
			m.selectedIdx = 0
			return m, loadDataCmd(m.projectPath, m.filterType)

		case key.Matches(msg, m.keyMap.Refresh):
			return m, loadDataCmd(m.projectPath, m.filterType)
		}
	}

	return m, nil
}

// View renders the model.
func (m Model) View() string {
	if !m.ready {
		return "Loading..."
	}

	th := theme.Current

	header := m.renderHeader()
	filterBar := m.renderFilterBar()
	table := m.renderTable()
	footer := m.renderFooter()

	content := lipgloss.JoinVertical(lipgloss.Left,
		header,
		filterBar,
		table,
		footer,
	)

	return lipgloss.NewStyle().
		Background(th.Base).
		Width(m.width).
		Height(m.height).
		Render(content)
}

func (m Model) renderHeader() string {
	th := theme.Current

	title := lipgloss.NewStyle().
		Foreground(th.Mauve).
		Bold(true).
		Render("Pattern Change Review")

	count := lipgloss.NewStyle().
		Foreground(th.Subtext).
		Render(fmt.Sprintf("%d pending", m.countPending()))

	spacer := lipgloss.NewStyle().
		Width(max(0, m.width-lipgloss.Width(title)-lipgloss.Width(count)-4)).
		Render("")

	return lipgloss.NewStyle().
		Background(th.Mantle).
		Padding(0, 1).
		Width(m.width).
		Render(lipgloss.JoinHorizontal(lipgloss.Top, title, spacer, count))
}

func (m Model) renderFilterBar() string {
	th := theme.Current

	// Filter badge
	label := "All Types"
	bg := th.Surface0
	fg := th.Subtext

	if m.filterType != "" {
		label = m.filterType
		switch m.filterType {
		case db.PatternChangeTypeRemove:
			bg = th.Red
			fg = th.Base
		case db.PatternChangeTypeSuggest:
			bg = th.Blue
			fg = th.Base
		case db.PatternChangeTypeAdd:
			bg = th.Green
			fg = th.Base
		}
	}

	badge := lipgloss.NewStyle().
		Background(bg).
		Foreground(fg).
		Padding(0, 1).
		Bold(m.filterType != "").
		Render(label)

	// Message (if any)
	msgStyled := ""
	if m.message != "" {
		msgColor := th.Green
		if m.messageType == "error" {
			msgColor = th.Red
		}
		msgStyled = lipgloss.NewStyle().
			Foreground(msgColor).
			Bold(true).
			Render(m.message)
	}

	return lipgloss.NewStyle().
		Padding(1, 1).
		Width(m.width).
		Render(lipgloss.JoinHorizontal(lipgloss.Center, badge, "  ", msgStyled))
}

func (m Model) renderTable() string {
	th := theme.Current

	// Calculate available height for table
	tableHeight := max(5, m.height-10)

	columns := []components.Column{
		{Header: "ID", Width: 6},
		{Header: "Type", Width: 8},
		{Header: "Tier", Width: 10},
		{Header: "Pattern", MinWidth: 20, MaxWidth: 40},
		{Header: "Reason", MinWidth: 15, MaxWidth: 30},
		{Header: "Status", Width: 10},
	}

	var rows [][]string
	for _, row := range m.rows {
		pattern := row.Pattern
		if len(pattern) > 37 {
			pattern = pattern[:37] + "..."
		}

		reason := row.Reason
		if len(reason) > 27 {
			reason = reason[:27] + "..."
		}

		statusIcon := statusIcon(row.Status)
		typeIcon := typeIcon(row.ChangeType)

		rows = append(rows, []string{
			fmt.Sprintf("#%d", row.ID),
			typeIcon + " " + row.ChangeType,
			row.Tier,
			pattern,
			reason,
			statusIcon + " " + row.Status,
		})
	}

	table := components.NewTable(columns).
		WithRows(rows).
		WithSelection(m.selectedIdx).
		WithMaxWidth(m.width - 4)

	tableView := table.Render()

	// Add empty state if no results
	if len(m.rows) == 0 {
		emptyStyle := lipgloss.NewStyle().
			Foreground(th.Subtext).
			Align(lipgloss.Center).
			Width(m.width - 4).
			Height(tableHeight)

		if m.filterType != "" {
			tableView = emptyStyle.Render("No " + m.filterType + " requests")
		} else {
			tableView = emptyStyle.Render("No pattern change requests")
		}
	}

	return lipgloss.NewStyle().
		Padding(0, 1).
		Height(tableHeight).
		Render(tableView)
}

func (m Model) renderFooter() string {
	th := theme.Current

	// Key hints
	keys := []string{
		"[a] approve",
		"[r] reject",
		"[f] filter",
		"[↑/↓] navigate",
		"[esc] back",
	}
	hint := lipgloss.NewStyle().
		Foreground(th.Subtext).
		Render(strings.Join(keys, "  "))

	// Stats
	stats := ""
	if m.totalCount > 0 {
		stats = fmt.Sprintf("%d total", m.totalCount)
	}
	if m.lastErr != nil {
		stats = "Error: " + m.lastErr.Error()
	}
	statsStyled := lipgloss.NewStyle().Foreground(th.Subtext).Render(stats)

	spacer := lipgloss.NewStyle().
		Width(max(0, m.width-lipgloss.Width(hint)-lipgloss.Width(statsStyled)-4)).
		Render("")

	return lipgloss.NewStyle().
		Background(th.Mantle).
		Padding(0, 1).
		Width(m.width).
		Render(lipgloss.JoinHorizontal(lipgloss.Top, hint, spacer, statsStyled))
}

// Helper methods

func (m *Model) cycleFilterType() {
	types := []string{"", db.PatternChangeTypeRemove, db.PatternChangeTypeSuggest, db.PatternChangeTypeAdd}
	for i, t := range types {
		if t == m.filterType {
			m.filterType = types[(i+1)%len(types)]
			return
		}
	}
	m.filterType = ""
}

func (m Model) countPending() int {
	count := 0
	for _, r := range m.rows {
		if r.Status == db.PatternChangeStatusPending {
			count++
		}
	}
	return count
}

// Commands

func tickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(time.Time) tea.Msg {
		return refreshMsg{}
	})
}

func loadDataCmd(projectPath, filterType string) tea.Cmd {
	return func() tea.Msg {
		rows, total, err := loadPatternChanges(projectPath, filterType)
		return dataMsg{
			rows:        rows,
			totalCount:  total,
			err:         err,
			refreshedAt: time.Now().UTC(),
		}
	}
}

func approveCmd(projectPath string, id int64) tea.Cmd {
	return func() tea.Msg {
		err := performAction(projectPath, id, "approve")
		return actionMsg{
			action:  "approve",
			id:      id,
			success: err == nil,
			err:     err,
		}
	}
}

func rejectCmd(projectPath string, id int64) tea.Cmd {
	return func() tea.Msg {
		err := performAction(projectPath, id, "reject")
		return actionMsg{
			action:  "reject",
			id:      id,
			success: err == nil,
			err:     err,
		}
	}
}

func loadPatternChanges(projectPath, filterType string) ([]RemovalRow, int, error) {
	dbPath := filepath.Join(projectPath, ".slb", "state.db")
	dbConn, err := db.OpenWithOptions(dbPath, db.OpenOptions{
		CreateIfNotExists: false,
		InitSchema:        false,
		ReadOnly:          true,
	})
	if err != nil {
		return nil, 0, err
	}
	defer dbConn.Close()

	var changes []*db.PatternChange
	if filterType != "" {
		changes, err = dbConn.ListPatternChangesByType(filterType)
	} else {
		changes, err = dbConn.ListAllPatternChanges()
	}
	if err != nil {
		return nil, 0, err
	}

	rows := make([]RemovalRow, 0, len(changes))
	for _, c := range changes {
		rows = append(rows, RemovalRow{
			ID:         c.ID,
			Tier:       c.Tier,
			Pattern:    c.Pattern,
			ChangeType: c.ChangeType,
			Reason:     c.Reason,
			Status:     c.Status,
			CreatedAt:  c.CreatedAt,
		})
	}

	return rows, len(rows), nil
}

func performAction(projectPath string, id int64, action string) error {
	dbPath := filepath.Join(projectPath, ".slb", "state.db")
	dbConn, err := db.OpenWithOptions(dbPath, db.OpenOptions{
		CreateIfNotExists: false,
		InitSchema:        true,
		ReadOnly:          false,
	})
	if err != nil {
		return err
	}
	defer dbConn.Close()

	switch action {
	case "approve":
		return dbConn.ApprovePatternChange(id)
	case "reject":
		return dbConn.RejectPatternChange(id)
	default:
		return fmt.Errorf("unknown action: %s", action)
	}
}

func statusIcon(s string) string {
	switch s {
	case db.PatternChangeStatusApproved:
		return "✓"
	case db.PatternChangeStatusRejected:
		return "✗"
	case db.PatternChangeStatusPending:
		return "⋯"
	default:
		return "?"
	}
}

func typeIcon(t string) string {
	switch t {
	case db.PatternChangeTypeRemove:
		return "−"
	case db.PatternChangeTypeSuggest:
		return "?"
	case db.PatternChangeTypeAdd:
		return "+"
	default:
		return "•"
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
