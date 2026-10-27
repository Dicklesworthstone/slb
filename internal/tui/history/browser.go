// Package history provides TUI views for request history browsing.
package history

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/tui/components"
	"github.com/Dicklesworthstone/slb/internal/tui/theme"
)

const (
	pageSize        = 20
	refreshInterval = 5 * time.Second
)

// BrowserKeyMap defines keybindings for the history browser.
type BrowserKeyMap struct {
	Search       key.Binding
	ClearSearch  key.Binding
	NextPage     key.Binding
	PrevPage     key.Binding
	Select       key.Binding
	Back         key.Binding
	Quit         key.Binding
	Up           key.Binding
	Down         key.Binding
	FilterTier   key.Binding
	FilterStatus key.Binding
	Export       key.Binding
}

// DefaultBrowserKeyMap returns the default keybindings.
func DefaultBrowserKeyMap() BrowserKeyMap {
	return BrowserKeyMap{
		Search: key.NewBinding(
			key.WithKeys("/"),
			key.WithHelp("/", "search"),
		),
		ClearSearch: key.NewBinding(
			key.WithKeys("esc"),
			key.WithHelp("esc", "clear/back"),
		),
		NextPage: key.NewBinding(
			key.WithKeys("right", "l", "pgdown"),
			key.WithHelp("→", "next page"),
		),
		PrevPage: key.NewBinding(
			key.WithKeys("left", "h", "pgup"),
			key.WithHelp("←", "prev page"),
		),
		Select: key.NewBinding(
			key.WithKeys("enter"),
			key.WithHelp("enter", "view"),
		),
		Back: key.NewBinding(
			key.WithKeys("esc", "q"),
			key.WithHelp("esc", "back"),
		),
		Quit: key.NewBinding(
			key.WithKeys("ctrl+c"),
			key.WithHelp("ctrl+c", "quit"),
		),
		Up: key.NewBinding(
			key.WithKeys("up", "k"),
			key.WithHelp("↑", "up"),
		),
		Down: key.NewBinding(
			key.WithKeys("down", "j"),
			key.WithHelp("↓", "down"),
		),
		FilterTier: key.NewBinding(
			key.WithKeys("t"),
			key.WithHelp("t", "tier filter"),
		),
		FilterStatus: key.NewBinding(
			key.WithKeys("s"),
			key.WithHelp("s", "status filter"),
		),
		Export: key.NewBinding(
			key.WithKeys("e"),
			key.WithHelp("e", "export"),
		),
	}
}

// HistoryRow represents a single row in the history table.
type HistoryRow struct {
	ID        string
	Command   string
	Agent     string
	Status    db.RequestStatus
	Tier      db.RiskTier
	CreatedAt time.Time
	Request   *db.Request
}

// Model is the Bubble Tea model for the history browser.
type Model struct {
	projectPath  string
	databasePath string
	keyMap       BrowserKeyMap

	// View state
	ready  bool
	width  int
	height int

	// Data
	rows       []HistoryRow
	totalCount int

	// Pagination
	page      int
	pageCount int

	// Selection
	selectedIdx int

	// Search
	searchInput textinput.Model
	searching   bool
	searchQuery string

	// Filters
	filters Filters

	// Callbacks
	OnBack   func()
	OnSelect func(requestID string)

	// Error state
	lastErr     error
	lastRefresh time.Time
}

// refreshMsg triggers a data refresh.
type refreshMsg struct{}

// dataMsg contains loaded data.
type dataMsg struct {
	rows        []HistoryRow
	totalCount  int
	err         error
	refreshedAt time.Time
}

// New creates a new history browser model.
func New(projectPath string) Model {
	if projectPath == "" {
		if pwd, err := os.Getwd(); err == nil {
			projectPath = pwd
		}
	}

	ti := textinput.New()
	ti.Placeholder = "Search commands, agents, reasons..."
	ti.CharLimit = 100
	ti.Width = 40

	return Model{
		projectPath:  projectPath,
		databasePath: filepath.Join(projectPath, ".slb", "state.db"),
		keyMap:       DefaultBrowserKeyMap(),
		searchInput:  ti,
		filters:      NewFilters(),
		page:         0,
	}
}

// WithDatabasePath selects the store while retaining this view's project scope.
func (m Model) WithDatabasePath(path string) Model {
	if path != "" {
		m.databasePath = path
	}
	return m
}

// Init initializes the model.
func (m Model) Init() tea.Cmd {
	return tea.Batch(loadDataCmd(m.projectPath, m.databasePath, m.searchQuery, m.filters, m.page), tickCmd())
}

// Update handles messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		m.searchInput.Width = min(60, m.width-20)
		return m, nil

	case refreshMsg:
		return m, tea.Batch(loadDataCmd(m.projectPath, m.databasePath, m.searchQuery, m.filters, m.page), tickCmd())

	case dataMsg:
		m.rows = msg.rows
		m.totalCount = msg.totalCount
		m.lastErr = msg.err
		m.lastRefresh = msg.refreshedAt
		m.pageCount = (m.totalCount + pageSize - 1) / pageSize
		if m.pageCount == 0 {
			m.pageCount = 1
		}
		// Clamp selection
		if m.selectedIdx >= len(m.rows) {
			m.selectedIdx = max(0, len(m.rows)-1)
		}
		return m, nil

	case tea.KeyMsg:
		// Handle search mode
		if m.searching {
			switch msg.String() {
			case "enter":
				m.searchQuery = m.searchInput.Value()
				m.searching = false
				m.page = 0
				m.selectedIdx = 0
				return m, loadDataCmd(m.projectPath, m.databasePath, m.searchQuery, m.filters, m.page)
			case "esc":
				m.searching = false
				m.searchInput.SetValue(m.searchQuery)
				return m, nil
			default:
				var cmd tea.Cmd
				m.searchInput, cmd = m.searchInput.Update(msg)
				cmds = append(cmds, cmd)
				return m, tea.Batch(cmds...)
			}
		}

		// Normal mode
		switch {
		case key.Matches(msg, m.keyMap.Search):
			m.searching = true
			m.searchInput.Focus()
			return m, textinput.Blink

		case key.Matches(msg, m.keyMap.ClearSearch):
			if m.searchQuery != "" {
				m.searchQuery = ""
				m.searchInput.SetValue("")
				m.page = 0
				m.selectedIdx = 0
				return m, loadDataCmd(m.projectPath, m.databasePath, m.searchQuery, m.filters, m.page)
			}
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

		case key.Matches(msg, m.keyMap.NextPage):
			if m.page < m.pageCount-1 {
				m.page++
				m.selectedIdx = 0
				return m, loadDataCmd(m.projectPath, m.databasePath, m.searchQuery, m.filters, m.page)
			}
			return m, nil

		case key.Matches(msg, m.keyMap.PrevPage):
			if m.page > 0 {
				m.page--
				m.selectedIdx = 0
				return m, loadDataCmd(m.projectPath, m.databasePath, m.searchQuery, m.filters, m.page)
			}
			return m, nil

		case key.Matches(msg, m.keyMap.Select):
			if len(m.rows) > 0 && m.selectedIdx < len(m.rows) {
				if m.OnSelect != nil {
					m.OnSelect(m.rows[m.selectedIdx].ID)
				}
			}
			return m, nil

		case key.Matches(msg, m.keyMap.FilterTier):
			m.filters.CycleTier()
			m.page = 0
			m.selectedIdx = 0
			return m, loadDataCmd(m.projectPath, m.databasePath, m.searchQuery, m.filters, m.page)

		case key.Matches(msg, m.keyMap.FilterStatus):
			m.filters.CycleStatus()
			m.page = 0
			m.selectedIdx = 0
			return m, loadDataCmd(m.projectPath, m.databasePath, m.searchQuery, m.filters, m.page)
		}
	}

	return m, tea.Batch(cmds...)
}

// View renders the model.
func (m Model) View() string {
	if !m.ready {
		return "Loading..."
	}

	th := theme.Current

	header := m.renderHeader()
	searchBar := m.renderSearchBar()
	table := m.renderTable()
	footer := m.renderFooter()

	content := lipgloss.JoinVertical(lipgloss.Left,
		header,
		searchBar,
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
		Render("History Browser")

	pageInfo := lipgloss.NewStyle().
		Foreground(th.Subtext).
		Render(fmt.Sprintf("Page %d/%d", m.page+1, m.pageCount))

	spacer := lipgloss.NewStyle().
		Width(max(0, m.width-lipgloss.Width(title)-lipgloss.Width(pageInfo)-4)).
		Render("")

	return lipgloss.NewStyle().
		Background(th.Mantle).
		Padding(0, 1).
		Width(m.width).
		Render(lipgloss.JoinHorizontal(lipgloss.Top, title, spacer, pageInfo))
}

func (m Model) renderSearchBar() string {
	th := theme.Current

	// Search input
	var searchStyle lipgloss.Style
	if m.searching {
		searchStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(th.Mauve).
			Padding(0, 1)
	} else {
		searchStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(th.Overlay0).
			Padding(0, 1)
	}

	searchBox := searchStyle.Render(m.searchInput.View())

	// Filter badges
	tierBadge := m.filters.RenderTierBadge()
	statusBadge := m.filters.RenderStatusBadge()

	filterSection := lipgloss.JoinHorizontal(lipgloss.Center, tierBadge, "  ", statusBadge)

	return lipgloss.NewStyle().
		Padding(1, 1).
		Width(m.width).
		Render(lipgloss.JoinHorizontal(lipgloss.Center, searchBox, "  ", filterSection))
}

func (m Model) renderTable() string {
	th := theme.Current

	// Calculate available height for table
	tableHeight := max(5, m.height-10)

	columns := []components.Column{
		{Header: "ID", Width: 10},
		{Header: "Command", MinWidth: 20, MaxWidth: 50},
		{Header: "Agent", Width: 12},
		{Header: "Status", Width: 10},
		{Header: "When", Width: 10},
	}

	var rows [][]string
	for _, row := range m.rows {
		cmd := row.Command
		if len(cmd) > 47 {
			cmd = cmd[:47] + "..."
		}

		statusIcon := statusIcon(row.Status)
		when := formatTimeAgo(row.CreatedAt)

		rows = append(rows, []string{
			shortID(row.ID),
			cmd,
			row.Agent,
			statusIcon + " " + statusShort(row.Status),
			when,
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

		if m.searchQuery != "" {
			tableView = emptyStyle.Render("No results for \"" + m.searchQuery + "\"")
		} else {
			tableView = emptyStyle.Render("No request history")
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
		"[/] search",
		"[t] tier",
		"[s] status",
		"[←→] page",
		"[enter] view",
		"[esc] back",
	}
	hint := lipgloss.NewStyle().
		Foreground(th.Subtext).
		Render(strings.Join(keys, "  "))

	// Stats
	stats := ""
	if m.totalCount > 0 {
		stats = fmt.Sprintf("%d results", m.totalCount)
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

// Helper functions

func tickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(time.Time) tea.Msg {
		return refreshMsg{}
	})
}

func loadDataCmd(projectPath, databasePath, query string, filters Filters, page int) tea.Cmd {
	return func() tea.Msg {
		rows, total, err := loadHistoryData(projectPath, databasePath, query, filters, page)
		return dataMsg{
			rows:        rows,
			totalCount:  total,
			err:         err,
			refreshedAt: time.Now().UTC(),
		}
	}
}

func loadHistoryData(projectPath, databasePath, query string, filters Filters, page int) ([]HistoryRow, int, error) {
	dbConn, err := db.OpenWithOptions(databasePath, db.OpenOptions{
		CreateIfNotExists: false,
		InitSchema:        false,
		ReadOnly:          true,
	})
	if err != nil {
		return nil, 0, err
	}
	defer dbConn.Close()

	// Build search query
	var requests []*db.Request
	if query != "" {
		requests, err = dbConn.SearchRequestsForProject(query, projectPath)
	} else {
		requests, err = dbConn.ListAllRequests(projectPath)
	}
	if err != nil {
		return nil, 0, err
	}

	// Apply filters
	filtered := make([]*db.Request, 0, len(requests))
	for _, r := range requests {
		// Retain the view's project scope before filters and pagination even
		// if a future loader returns broader results from this shared store.
		if r.ProjectPath != projectPath {
			continue
		}
		if filters.TierFilter != "" && string(r.RiskTier) != filters.TierFilter {
			continue
		}
		if filters.StatusFilter != "" && string(r.Status) != filters.StatusFilter {
			continue
		}
		filtered = append(filtered, r)
	}

	// Paginate
	total := len(filtered)
	start := page * pageSize
	end := start + pageSize
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}

	page_requests := filtered[start:end]

	rows := make([]HistoryRow, 0, len(page_requests))
	for _, r := range page_requests {
		cmd := r.Command.DisplayRedacted
		if cmd == "" {
			cmd = r.Command.Raw
		}
		rows = append(rows, HistoryRow{
			ID:        r.ID,
			Command:   cmd,
			Agent:     r.RequestorAgent,
			Status:    r.Status,
			Tier:      r.RiskTier,
			CreatedAt: r.CreatedAt,
			Request:   r,
		})
	}

	return rows, total, nil
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
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

func statusIcon(s db.RequestStatus) string {
	switch s {
	case db.StatusApproved, db.StatusExecuted:
		return "✓"
	case db.StatusRejected, db.StatusExecutionFailed:
		return "✗"
	case db.StatusPending:
		return "⋯"
	case db.StatusTimeout, db.StatusEscalated:
		return "⚠"
	case db.StatusCancelled:
		return "○"
	default:
		return "?"
	}
}

func statusShort(s db.RequestStatus) string {
	switch s {
	case db.StatusApproved:
		return "APPR"
	case db.StatusExecuted:
		return "EXEC"
	case db.StatusRejected:
		return "REJ"
	case db.StatusExecutionFailed:
		return "FAIL"
	case db.StatusPending:
		return "PEND"
	case db.StatusTimeout:
		return "TOUT"
	case db.StatusEscalated:
		return "ESC"
	case db.StatusCancelled:
		return "CANC"
	default:
		return string(s)
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
