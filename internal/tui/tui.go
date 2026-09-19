// Package tui implements the Bubble Tea terminal UI for SLB.
// Uses the Charmbracelet ecosystem: Bubble Tea, Bubbles, Lip Gloss, Glamour.
package tui

import (
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/tui/dashboard"
	"github.com/Dicklesworthstone/slb/internal/tui/history"
	"github.com/Dicklesworthstone/slb/internal/tui/patterns"
	"github.com/Dicklesworthstone/slb/internal/tui/request"
	"github.com/Dicklesworthstone/slb/internal/tui/theme"
)

// View represents the current view in the TUI.
type View int

const (
	ViewDashboard View = iota
	ViewRequestDetail
	ViewHistory
	ViewPatterns
)

// Options configures the TUI behavior.
type Options struct {
	ProjectPath     string
	ConfigPath      string
	Theme           string
	DisableMouse    bool
	RefreshInterval int
	SessionID       string
	SessionKey      string
}

// DefaultOptions returns the default TUI options.
func DefaultOptions() Options {
	pwd, _ := os.Getwd()
	return Options{
		ProjectPath:     pwd,
		Theme:           "",
		DisableMouse:    false,
		RefreshInterval: 5,
		SessionID:       "",
		SessionKey:      "",
	}
}

// Model represents the main TUI model with multi-view navigation.
type Model struct {
	options Options
	view    View
	width   int
	height  int

	// View models
	dashboard *dashboard.Model
	detail    *request.DetailModel
	history   history.Model
	patterns  patterns.Model

	// Navigation state
	selectedRequestID string
}

// New creates a new TUI model with options.
func New() Model {
	return NewWithOptions(DefaultOptions())
}

// NewWithOptions creates a new TUI model with custom options.
func NewWithOptions(opts Options) Model {
	// Apply theme if specified
	if opts.Theme != "" {
		theme.SetTheme(theme.FlavorName(opts.Theme))
	}

	// Create dashboard model
	dash := dashboard.New(opts.ProjectPath)

	return Model{
		options:   opts,
		view:      ViewDashboard,
		dashboard: &dash,
		history:   history.New(opts.ProjectPath),
		patterns:  patterns.New(opts.ProjectPath),
	}
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	// Initialize the current view
	switch m.view {
	case ViewDashboard:
		if m.dashboard != nil {
			return m.dashboard.Init()
		}
	case ViewHistory:
		return m.history.Init()
	case ViewPatterns:
		return m.patterns.Init()
	case ViewRequestDetail:
		if m.detail != nil {
			return m.detail.Init()
		}
	}
	return nil
}

// navigateMsg is sent when navigating to a different view.
type navigateMsg struct {
	view      View
	requestID string
}

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Forward to current view
		return m.forwardUpdate(msg)

	case navigateMsg:
		return m.handleNavigation(msg)

	case request.ReviewSubmittedMsg:
		// A late worker result must not navigate away from another request.
		if m.view == ViewRequestDetail && m.detail != nil && m.detail.Request != nil && m.detail.Request.ID == msg.RequestID {
			return m.forwardUpdate(msg)
		}
		return m, nil

	case tea.KeyMsg:
		// Form input owns ALL keys until submission/cancellation. In particular,
		// typing 'b' in a reason is not global navigation and Esc cancels the
		// form before it leaves the detail view.
		if m.view == ViewRequestDetail && m.detail != nil && m.detail.Mode != request.DetailModeView {
			return m.forwardUpdate(msg)
		}
		// Handle global navigation keys based on current view
		if m.view == ViewDashboard {
			switch msg.String() {
			case "m":
				return m.handleNavigation(navigateMsg{view: ViewPatterns})
			case "H":
				return m.handleNavigation(navigateMsg{view: ViewHistory})
			case "enter":
				if m.dashboard != nil && len(m.dashboard.SelectedRequestID()) > 0 {
					return m.handleNavigation(navigateMsg{
						view: ViewRequestDetail, requestID: m.dashboard.SelectedRequestID(),
					})
				}
			}
		}

		if m.view == ViewHistory || m.view == ViewPatterns || m.view == ViewRequestDetail {
			switch msg.String() {
			case "esc", "b":
				return m.handleNavigation(navigateMsg{view: ViewDashboard})
			}
		}
		return m.forwardUpdate(msg)

	default:
		return m.forwardUpdate(msg)
	}
}

// forwardUpdate forwards messages to the current view.
func (m Model) forwardUpdate(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch m.view {
	case ViewDashboard:
		if m.dashboard != nil {
			m.setupDashboardCallbacks()
			next, c := m.dashboard.Update(msg)
			if dm, ok := next.(dashboard.Model); ok {
				m.dashboard = &dm
			}
			cmd = c
		}

	case ViewRequestDetail:
		if m.detail != nil {
			next, c := m.detail.Update(msg)
			if dm, ok := next.(*request.DetailModel); ok {
				m.detail = dm
			}
			cmd = c
		}

	case ViewHistory:
		m.setupHistoryCallbacks()
		next, c := m.history.Update(msg)
		if hm, ok := next.(history.Model); ok {
			m.history = hm
		}
		cmd = c

	case ViewPatterns:
		m.setupPatternsCallbacks()
		next, c := m.patterns.Update(msg)
		if pm, ok := next.(patterns.Model); ok {
			m.patterns = pm
		}
		cmd = c
	}

	return m, cmd
}

// handleNavigation handles view navigation. New views inherit the last known
// terminal size rather than waiting until the terminal is resized again.
func (m Model) handleNavigation(nav navigateMsg) (tea.Model, tea.Cmd) {
	m.view = nav.view

	switch nav.view {
	case ViewDashboard:
		dash := dashboard.New(m.options.ProjectPath)
		m.dashboard = &dash
		m.setupDashboardCallbacks()
		m, sizeCmd := m.seedViewSize()
		return m, tea.Batch(sizeCmd, m.dashboard.Init())

	case ViewRequestDetail:
		if nav.requestID != "" {
			m.selectedRequestID = nav.requestID
			detail := m.loadRequestDetail(nav.requestID)
			if detail != nil {
				m.detail = detail
				m.setupDetailCallbacks()
				m, sizeCmd := m.seedViewSize()
				return m, tea.Batch(sizeCmd, m.detail.Init())
			}
		}
		m.view = ViewDashboard
		return m, nil

	case ViewHistory:
		m.history = history.New(m.options.ProjectPath)
		m.setupHistoryCallbacks()
		m, sizeCmd := m.seedViewSize()
		return m, tea.Batch(sizeCmd, m.history.Init())

	case ViewPatterns:
		m.patterns = patterns.New(m.options.ProjectPath)
		m.setupPatternsCallbacks()
		m, sizeCmd := m.seedViewSize()
		return m, tea.Batch(sizeCmd, m.patterns.Init())
	}

	return m, nil
}

// seedViewSize replays the root model's last known terminal size into the
// current view. It is a no-op until a real size message has been received.
func (m Model) seedViewSize() (Model, tea.Cmd) {
	if m.width <= 0 || m.height <= 0 {
		return m, nil
	}
	next, cmd := m.forwardUpdate(tea.WindowSizeMsg{Width: m.width, Height: m.height})
	if nm, ok := next.(Model); ok {
		return nm, cmd
	}
	return m, cmd
}

// setupDashboardCallbacks wires up dashboard navigation callbacks.
func (m *Model) setupDashboardCallbacks() {
	if m.dashboard == nil {
		return
	}
	m.dashboard.OnPatterns = func() {}
	m.dashboard.OnHistory = func() {}
}

// setupDetailCallbacks wires up request detail callbacks.
func (m *Model) setupDetailCallbacks() {
	if m.detail == nil {
		return
	}
	m.detail.OnBack = func() tea.Cmd {
		return func() tea.Msg { return navigateMsg{view: ViewDashboard} }
	}
	// Snapshot the model values used to build a submission. The returned
	// workers capture only options/command identity, never the mutable model.
	m.detail.OnApprove = func(requestID string, comments string) tea.Cmd {
		return m.approveRequest(requestID, comments)
	}
	m.detail.OnReject = func(requestID string, reason string) tea.Cmd {
		return m.rejectRequest(requestID, reason)
	}
}

// setupHistoryCallbacks wires up history browser callbacks.
func (m *Model) setupHistoryCallbacks() {
	m.history.OnBack = func() {}
	m.history.OnSelect = func(requestID string) {}
}

// setupPatternsCallbacks wires up patterns view callbacks.
func (m *Model) setupPatternsCallbacks() {
	m.patterns.OnBack = func() {}
}

// loadRequestDetail loads a request and creates a detail model.
func (m *Model) loadRequestDetail(requestID string) *request.DetailModel {
	dbPath := filepath.Join(m.options.ProjectPath, ".slb", "state.db")
	dbConn, err := db.OpenWithOptions(dbPath, db.OpenOptions{
		CreateIfNotExists: false, InitSchema: false, ReadOnly: true,
	})
	if err != nil {
		return nil
	}
	defer dbConn.Close()

	var currentSession *db.Session
	if m.options.SessionID != "" && m.options.SessionKey != "" {
		s, err := dbConn.GetSession(m.options.SessionID)
		if err == nil && s.IsActive() && db.ExecutionSessionKeyMatches(s.SessionKey, m.options.SessionKey) {
			currentSession = s
		}
	}
	req, err := dbConn.GetRequest(requestID)
	if err != nil || req.ProjectPath != m.options.ProjectPath {
		return nil
	}
	reviewPtrs, err := dbConn.ListReviewsForRequest(requestID)
	if err != nil {
		return nil // An incomplete review snapshot must not enable another vote.
	}
	reviews := make([]db.Review, 0, len(reviewPtrs))
	for _, r := range reviewPtrs {
		if r != nil {
			reviews = append(reviews, *r)
		}
	}
	detail := request.NewDetailModel(req, reviews)
	if currentSession != nil {
		detail.WithSession(currentSession)
	}
	return detail
}

// View implements tea.Model.
func (m Model) View() string {
	switch m.view {
	case ViewDashboard:
		if m.dashboard != nil {
			return m.dashboard.View()
		}
	case ViewRequestDetail:
		if m.detail != nil {
			return m.detail.View()
		}
	case ViewHistory:
		return m.history.View()
	case ViewPatterns:
		return m.patterns.View()
	}
	return "Loading..."
}

// Run starts the TUI with default options.
func Run() error { return RunWithOptions(DefaultOptions()) }

// RunWithOptions starts the TUI with custom options.
func RunWithOptions(opts Options) error {
	if opts.Theme != "" {
		theme.SetTheme(theme.FlavorName(opts.Theme))
	}
	m := NewWithOptions(opts)
	teaOpts := []tea.ProgramOption{tea.WithAltScreen()}
	if !opts.DisableMouse {
		teaOpts = append(teaOpts, tea.WithMouseCellMotion())
	}
	p := tea.NewProgram(m, teaOpts...)
	_, err := p.Run()
	return err
}
