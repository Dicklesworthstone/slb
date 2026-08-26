// Package request provides TUI views for request management.
package request

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/tui/components"
	"github.com/Dicklesworthstone/slb/internal/tui/icons"
	"github.com/Dicklesworthstone/slb/internal/tui/theme"
)

// DetailKeyMap defines keybindings for the detail view.
type DetailKeyMap struct {
	Approve  key.Binding
	Reject   key.Binding
	Copy     key.Binding
	Execute  key.Binding
	Escalate key.Binding
	Back     key.Binding
	ScrollUp key.Binding
	ScrollDn key.Binding
	PageUp   key.Binding
	PageDown key.Binding
	Quit     key.Binding
}

// DefaultDetailKeyMap returns the default keybindings.
func DefaultDetailKeyMap() DetailKeyMap {
	return DetailKeyMap{
		Approve: key.NewBinding(
			key.WithKeys("a"),
			key.WithHelp("a", "approve"),
		),
		Reject: key.NewBinding(
			key.WithKeys("r"),
			key.WithHelp("r", "reject"),
		),
		Copy: key.NewBinding(
			key.WithKeys("c"),
			key.WithHelp("c", "copy command"),
		),
		Execute: key.NewBinding(
			key.WithKeys("x"),
			key.WithHelp("x", "execute"),
		),
		Escalate: key.NewBinding(
			key.WithKeys("e"),
			key.WithHelp("e", "escalate"),
		),
		Back: key.NewBinding(
			key.WithKeys("esc", "q"),
			key.WithHelp("esc/q", "back"),
		),
		ScrollUp: key.NewBinding(
			key.WithKeys("up", "k"),
			key.WithHelp("↑/k", "scroll up"),
		),
		ScrollDn: key.NewBinding(
			key.WithKeys("down", "j"),
			key.WithHelp("↓/j", "scroll down"),
		),
		PageUp: key.NewBinding(
			key.WithKeys("pgup", "ctrl+u"),
			key.WithHelp("pgup", "page up"),
		),
		PageDown: key.NewBinding(
			key.WithKeys("pgdown", "ctrl+d"),
			key.WithHelp("pgdown", "page down"),
		),
		Quit: key.NewBinding(
			key.WithKeys("ctrl+c"),
			key.WithHelp("ctrl+c", "quit"),
		),
	}
}

// DetailMode represents the current view mode.
type DetailMode int

const (
	// DetailModeView is the default view mode.
	DetailModeView DetailMode = iota
	// DetailModeApprove is the approval form mode.
	DetailModeApprove
	// DetailModeReject is the rejection form mode.
	DetailModeReject
)

// DetailModel is the Bubble Tea model for request detail view.
type DetailModel struct {
	Request  *db.Request
	Reviews  []db.Review
	Session  *db.Session // Current session for approval eligibility
	Width    int
	Height   int
	KeyMap   DetailKeyMap
	Mode     DetailMode
	viewport viewport.Model
	ready    bool

	// Sub-models for forms
	approveForm *ApproveModel
	rejectForm  *RejectModel

	// Callbacks
	OnBack    func() tea.Cmd
	OnApprove func(requestID string, comments string) tea.Cmd
	OnReject  func(requestID string, reason string) tea.Cmd
	OnCopy    func(command string) tea.Cmd
	OnExecute func(requestID string) tea.Cmd

	// Copied flag for feedback
	copied bool
}

// NewDetailModel creates a new request detail model.
func NewDetailModel(request *db.Request, reviews []db.Review) *DetailModel {
	return &DetailModel{
		Request: request,
		Reviews: reviews,
		KeyMap:  DefaultDetailKeyMap(),
		Mode:    DetailModeView,
	}
}

// WithSession sets the current session.
func (m *DetailModel) WithSession(s *db.Session) *DetailModel {
	m.Session = s
	return m
}

// Init initializes the model.
func (m *DetailModel) Init() tea.Cmd {
	return nil
}

// Update handles messages.
func (m *DetailModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.Width = msg.Width
		m.Height = msg.Height
		if !m.ready {
			m.viewport = viewport.New(msg.Width, msg.Height-4) // Reserve space for header/footer
			m.viewport.SetContent(m.renderContent())
			m.ready = true
		} else {
			m.viewport.Width = msg.Width
			m.viewport.Height = msg.Height - 4
			m.viewport.SetContent(m.renderContent())
		}
		return m, nil

	case tea.KeyMsg:
		// Handle form modes first
		if m.Mode == DetailModeApprove && m.approveForm != nil {
			updated, cmd := m.approveForm.Update(msg)
			m.approveForm = updated.(*ApproveModel)
			if m.approveForm.Submitted {
				if m.OnApprove != nil {
					cmds = append(cmds, m.OnApprove(m.Request.ID, m.approveForm.Comments))
				}
				m.Mode = DetailModeView
				m.approveForm = nil
			} else if m.approveForm.Cancelled {
				m.Mode = DetailModeView
				m.approveForm = nil
			}
			return m, cmd
		}

		if m.Mode == DetailModeReject && m.rejectForm != nil {
			updated, cmd := m.rejectForm.Update(msg)
			m.rejectForm = updated.(*RejectModel)
			if m.rejectForm.Submitted {
				if m.OnReject != nil {
					cmds = append(cmds, m.OnReject(m.Request.ID, m.rejectForm.Reason))
				}
				m.Mode = DetailModeView
				m.rejectForm = nil
			} else if m.rejectForm.Cancelled {
				m.Mode = DetailModeView
				m.rejectForm = nil
			}
			return m, cmd
		}

		// Handle main view keybindings
		switch {
		case key.Matches(msg, m.KeyMap.Approve):
			if m.canApprove() {
				m.Mode = DetailModeApprove
				m.approveForm = NewApproveModel(m.Request)
				m.approveForm.Width = m.Width
				return m, m.approveForm.Init()
			}

		case key.Matches(msg, m.KeyMap.Reject):
			if m.canReject() {
				m.Mode = DetailModeReject
				m.rejectForm = NewRejectModel(m.Request)
				m.rejectForm.Width = m.Width
				return m, m.rejectForm.Init()
			}

		case key.Matches(msg, m.KeyMap.Copy):
			m.copied = true
			if m.OnCopy != nil {
				cmds = append(cmds, m.OnCopy(m.Request.Command.Raw))
			}
			// Clear copied flag after a short time
			cmds = append(cmds, tea.Tick(2*time.Second, func(t time.Time) tea.Msg {
				return clearCopiedMsg{}
			}))

		case key.Matches(msg, m.KeyMap.Execute):
			if m.canExecute() && m.OnExecute != nil {
				cmds = append(cmds, m.OnExecute(m.Request.ID))
			}

		case key.Matches(msg, m.KeyMap.Back):
			if m.OnBack != nil {
				cmds = append(cmds, m.OnBack())
			}
			return m, tea.Batch(cmds...)

		case key.Matches(msg, m.KeyMap.Quit):
			return m, tea.Quit

		case key.Matches(msg, m.KeyMap.ScrollUp):
			m.viewport.LineUp(1)

		case key.Matches(msg, m.KeyMap.ScrollDn):
			m.viewport.LineDown(1)

		case key.Matches(msg, m.KeyMap.PageUp):
			m.viewport.HalfViewUp()

		case key.Matches(msg, m.KeyMap.PageDown):
			m.viewport.HalfViewDown()
		}

	case clearCopiedMsg:
		m.copied = false
	}

	// Update viewport
	if m.ready {
		var vpCmd tea.Cmd
		m.viewport, vpCmd = m.viewport.Update(msg)
		cmds = append(cmds, vpCmd)
	}

	return m, tea.Batch(cmds...)
}

type clearCopiedMsg struct{}

// View renders the model.
func (m *DetailModel) View() string {
	th := theme.Current
	var b strings.Builder

	// Handle form modes
	if m.Mode == DetailModeApprove && m.approveForm != nil {
		return m.approveForm.View()
	}
	if m.Mode == DetailModeReject && m.rejectForm != nil {
		return m.rejectForm.View()
	}

	// Header
	header := m.renderHeader()
	b.WriteString(header)
	b.WriteString("\n")

	// Scrollable content. Before the first size message there is no viewport
	// to scroll, so render the content directly rather than a placeholder;
	// the viewport takes over as soon as a size is known.
	if m.ready {
		b.WriteString(m.viewport.View())
	} else {
		b.WriteString(m.renderContent())
	}
	b.WriteString("\n")

	// Footer with keybindings
	footer := m.renderFooter()
	footerStyle := lipgloss.NewStyle().
		Foreground(th.Subtext).
		Background(th.Surface).
		Width(m.Width).
		Padding(0, 1)
	b.WriteString(footerStyle.Render(footer))

	return b.String()
}

// renderHeader renders the header bar.
func (m *DetailModel) renderHeader() string {
	th := theme.Current

	// Request ID
	idStyle := lipgloss.NewStyle().
		Foreground(th.Mauve).
		Bold(true)

	// Status badge
	statusBadge := components.RenderStatusBadge(string(m.Request.Status))

	// Tier indicator
	tierIndicator := components.RenderRiskIndicator(string(m.Request.RiskTier))

	header := fmt.Sprintf("%s  %s  %s",
		idStyle.Render(m.Request.ID),
		statusBadge,
		tierIndicator,
	)

	headerStyle := lipgloss.NewStyle().
		Background(th.Surface).
		Width(m.Width).
		Padding(0, 1)

	return headerStyle.Render(header)
}

// renderContent renders the main scrollable content.
func (m *DetailModel) renderContent() string {
	th := theme.Current
	var sections []string

	// Command box
	cmdBox := components.NewCommandBox(m.Request.Command.Raw).
		WithHint(true)
	if m.Request.Command.DisplayRedacted != "" {
		cmdBox = cmdBox.WithRedacted(m.Request.Command.DisplayRedacted)
	}
	if m.Width > 0 {
		cmdBox = cmdBox.WithMaxWidth(m.Width - 4)
	}
	sections = append(sections, cmdBox.Render())

	// Requestor info
	requestorInfo := m.renderRequestorInfo()
	sections = append(sections, requestorInfo)

	// Justification
	justification := m.renderJustification()
	if justification != "" {
		sections = append(sections, justification)
	}

	// Dry run output
	if m.Request.DryRun != nil && m.Request.DryRun.Output != "" {
		dryRun := m.renderDryRun()
		sections = append(sections, dryRun)
	}

	// Attachments
	if len(m.Request.Attachments) > 0 {
		attachments := m.renderAttachments()
		sections = append(sections, attachments)
	}

	// Timeline
	timeline := m.renderTimeline()
	sections = append(sections, timeline)

	// Reviews
	if len(m.Reviews) > 0 {
		reviews := m.renderReviews()
		sections = append(sections, reviews)
	}

	// Join sections with dividers. Before a size is known the divider is
	// omitted rather than computed from a zero width.
	divider := lipgloss.NewStyle().
		Foreground(th.Overlay0).
		Render(strings.Repeat("─", max(0, m.Width-4)))

	return strings.Join(sections, "\n"+divider+"\n\n")
}

// renderRequestorInfo renders requestor information.
func (m *DetailModel) renderRequestorInfo() string {
	th := theme.Current

	sectionTitle := lipgloss.NewStyle().
		Foreground(th.Blue).
		Bold(true).
		Render("Requestor")

	agentStyle := lipgloss.NewStyle().Foreground(th.Text)
	metaStyle := lipgloss.NewStyle().Foreground(th.Subtext)

	agentIcon := icons.Current().Agent
	timeAgo := formatTimeAgo(m.Request.CreatedAt)

	info := fmt.Sprintf("%s %s (%s)\n%s",
		agentIcon,
		agentStyle.Render(m.Request.RequestorAgent),
		metaStyle.Render(m.Request.RequestorModel),
		metaStyle.Render("Requested "+timeAgo),
	)

	// Add expiry info if pending
	if m.Request.Status == db.StatusPending && m.Request.ExpiresAt != nil {
		expiresIn := time.Until(*m.Request.ExpiresAt)
		if expiresIn > 0 {
			info += metaStyle.Render(fmt.Sprintf(" (expires in %s)", formatDuration(expiresIn)))
		} else {
			info += lipgloss.NewStyle().Foreground(th.Red).Render(" (EXPIRED)")
		}
	}

	return sectionTitle + "\n" + info
}

// renderJustification renders the justification section.
func (m *DetailModel) renderJustification() string {
	th := theme.Current
	j := m.Request.Justification

	// Skip if no justification provided
	if j.Reason == "" && j.ExpectedEffect == "" && j.Goal == "" && j.SafetyArgument == "" {
		return ""
	}

	sectionTitle := lipgloss.NewStyle().
		Foreground(th.Blue).
		Bold(true).
		Render("Justification")

	labelStyle := lipgloss.NewStyle().Foreground(th.Subtext).Width(16)
	valueStyle := lipgloss.NewStyle().Foreground(th.Text)

	var lines []string

	if j.Reason != "" {
		lines = append(lines, labelStyle.Render("Reason:")+" "+valueStyle.Render(j.Reason))
	}
	if j.ExpectedEffect != "" {
		lines = append(lines, labelStyle.Render("Expected Effect:")+" "+valueStyle.Render(j.ExpectedEffect))
	}
	if j.Goal != "" {
		lines = append(lines, labelStyle.Render("Goal:")+" "+valueStyle.Render(j.Goal))
	}
	if j.SafetyArgument != "" {
		lines = append(lines, labelStyle.Render("Safety:")+" "+valueStyle.Render(j.SafetyArgument))
	}

	return sectionTitle + "\n" + strings.Join(lines, "\n")
}

// renderDryRun renders the dry run output section.
func (m *DetailModel) renderDryRun() string {
	th := theme.Current

	sectionTitle := lipgloss.NewStyle().
		Foreground(th.Blue).
		Bold(true).
		Render("Dry Run Output")

	cmdStyle := lipgloss.NewStyle().
		Foreground(th.Subtext).
		Italic(true)

	outputStyle := lipgloss.NewStyle().
		Foreground(th.Text).
		Background(th.Surface0).
		Padding(0, 1)

	output := m.Request.DryRun.Output
	if len(output) > 500 {
		output = output[:500] + "\n... (truncated)"
	}

	return sectionTitle + "\n" +
		cmdStyle.Render("$ "+m.Request.DryRun.Command) + "\n" +
		outputStyle.Render(output)
}

// renderAttachments renders the attachments section.
func (m *DetailModel) renderAttachments() string {
	th := theme.Current

	sectionTitle := lipgloss.NewStyle().
		Foreground(th.Blue).
		Bold(true).
		Render(fmt.Sprintf("Attachments (%d)", len(m.Request.Attachments)))

	var lines []string
	for i, att := range m.Request.Attachments {
		typeIcon := attachmentIcon(string(att.Type))
		typeBadge := lipgloss.NewStyle().
			Foreground(th.Peach).
			Render(string(att.Type))

		preview := att.Content
		if len(preview) > 100 {
			preview = preview[:100] + "..."
		}
		preview = strings.ReplaceAll(preview, "\n", " ")

		line := fmt.Sprintf("%d. %s %s: %s",
			i+1, typeIcon, typeBadge,
			lipgloss.NewStyle().Foreground(th.Subtext).Render(preview),
		)
		lines = append(lines, line)
	}

	return sectionTitle + "\n" + strings.Join(lines, "\n")
}

// renderTimeline renders the request timeline.
func (m *DetailModel) renderTimeline() string {
	th := theme.Current

	sectionTitle := lipgloss.NewStyle().
		Foreground(th.Blue).
		Bold(true).
		Render("Timeline")

	tl := components.NewTimeline().WithCurrent(string(m.Request.Status))

	// Add created event
	tl.AddEvent("created", m.Request.CreatedAt, m.Request.RequestorAgent, "Request submitted")

	// Add pending event if status progressed beyond pending
	if m.Request.Status != db.StatusPending {
		tl.AddEvent("pending", m.Request.CreatedAt.Add(time.Millisecond), "", "Waiting for approval")
	} else {
		tl.AddEvent("pending", time.Time{}, "", "Awaiting review")
	}

	// Add approval/rejection events from reviews
	for _, rev := range m.Reviews {
		if rev.Decision == db.DecisionApprove {
			tl.AddEvent("approved", rev.CreatedAt, rev.ReviewerAgent, rev.Comments)
		} else {
			tl.AddEvent("rejected", rev.CreatedAt, rev.ReviewerAgent, rev.Comments)
		}
	}

	// Add execution event if applicable
	if m.Request.Execution != nil && m.Request.Execution.ExecutedAt != nil {
		exitInfo := ""
		if m.Request.Execution.ExitCode != nil {
			exitInfo = fmt.Sprintf("exit code %d", *m.Request.Execution.ExitCode)
		}
		tl.AddEvent("executed", *m.Request.Execution.ExecutedAt, m.Request.Execution.ExecutedByAgent, exitInfo)
	}

	return sectionTitle + "\n" + tl.Render()
}

// renderReviews renders the reviews section.
func (m *DetailModel) renderReviews() string {
	th := theme.Current

	approvals := 0
	rejections := 0
	for _, r := range m.Reviews {
		if r.Decision == db.DecisionApprove {
			approvals++
		} else {
			rejections++
		}
	}

	sectionTitle := lipgloss.NewStyle().
		Foreground(th.Blue).
		Bold(true).
		Render(fmt.Sprintf("Reviews (%d/%d required)", approvals, m.Request.MinApprovals))

	var reviewLines []string
	for _, rev := range m.Reviews {
		icon := icons.StatusIcon(string(rev.Decision))
		decisionColor := th.Green
		if rev.Decision == db.DecisionReject {
			decisionColor = th.Red
		}

		reviewer := lipgloss.NewStyle().Foreground(th.Text).Bold(true).Render(rev.ReviewerAgent)
		decision := lipgloss.NewStyle().Foreground(decisionColor).Render(strings.ToUpper(string(rev.Decision)))
		timeStr := lipgloss.NewStyle().Foreground(th.Subtext).Render(formatTimeAgo(rev.CreatedAt))

		line := fmt.Sprintf("%s %s %s  %s", icon, reviewer, decision, timeStr)
		if rev.Comments != "" {
			line += "\n   " + lipgloss.NewStyle().Foreground(th.Subtext).Italic(true).Render(rev.Comments)
		}
		reviewLines = append(reviewLines, line)
	}

	return sectionTitle + "\n" + strings.Join(reviewLines, "\n")
}

// renderFooter renders the footer with keybindings.
func (m *DetailModel) renderFooter() string {
	th := theme.Current
	var keys []string

	keyStyle := lipgloss.NewStyle().Foreground(th.Mauve).Bold(true)
	descStyle := lipgloss.NewStyle().Foreground(th.Subtext)

	// Conditional keys based on state
	if m.canApprove() {
		keys = append(keys, keyStyle.Render("[a]")+descStyle.Render("pprove"))
	}
	if m.canReject() {
		keys = append(keys, keyStyle.Render("[r]")+descStyle.Render("eject"))
	}
	if m.canExecute() {
		keys = append(keys, keyStyle.Render("[x]")+descStyle.Render(" execute"))
	}

	// Copy key with feedback
	if m.copied {
		keys = append(keys, lipgloss.NewStyle().Foreground(th.Green).Render("Copied!"))
	} else {
		keys = append(keys, keyStyle.Render("[c]")+descStyle.Render("opy"))
	}

	keys = append(keys, keyStyle.Render("[esc]")+descStyle.Render(" back"))

	// Scroll indicator
	scrollInfo := fmt.Sprintf(" %d%%", int(m.viewport.ScrollPercent()*100))
	keys = append(keys, descStyle.Render(scrollInfo))

	return strings.Join(keys, "  ")
}

// canApprove returns true if the current session can approve.
func (m *DetailModel) canApprove() bool {
	// Must be pending
	if m.Request.Status != db.StatusPending {
		return false
	}
	// Must have a session
	if m.Session == nil {
		return false
	}
	// Cannot approve own request
	if m.Session.ID == m.Request.RequestorSessionID {
		return false
	}
	// Check if already reviewed
	for _, rev := range m.Reviews {
		if rev.ReviewerSessionID == m.Session.ID {
			return false
		}
	}
	return true
}

// canReject returns true if the current session can reject.
func (m *DetailModel) canReject() bool {
	// Same logic as canApprove
	return m.canApprove()
}

// canExecute returns true if the request can be executed.
func (m *DetailModel) canExecute() bool {
	// Must be approved
	if m.Request.Status != db.StatusApproved {
		return false
	}
	// Check if approval expired
	if m.Request.ApprovalExpiresAt != nil && time.Now().After(*m.Request.ApprovalExpiresAt) {
		return false
	}
	return true
}

// Helpers

// attachmentIcon returns an icon for an attachment type.
func attachmentIcon(attType string) string {
	ic := icons.Current()
	switch attType {
	case "file":
		return ic.File
	case "git_diff":
		return ic.Git
	case "context":
		return ic.Terminal
	case "screenshot":
		return ic.File
	default:
		return ic.File
	}
}

func formatTimeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		mins := int(d.Minutes())
		if mins == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", mins)
	case d < 24*time.Hour:
		hours := int(d.Hours())
		if hours == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", hours)
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	}
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}
