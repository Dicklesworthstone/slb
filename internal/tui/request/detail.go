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
		Approve:  key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "approve")),
		Reject:   key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "reject")),
		Copy:     key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "copy command")),
		Execute:  key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "execute")),
		Escalate: key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "escalate")),
		Back:     key.NewBinding(key.WithKeys("esc", "q"), key.WithHelp("esc/q", "back")),
		ScrollUp: key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "scroll up")),
		ScrollDn: key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "scroll down")),
		PageUp:   key.NewBinding(key.WithKeys("pgup", "ctrl+u"), key.WithHelp("pgup", "page up")),
		PageDown: key.NewBinding(key.WithKeys("pgdown", "ctrl+d"), key.WithHelp("pgdown", "page down")),
		Quit:     key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
	}
}

// DetailMode represents the current view mode.
type DetailMode int

const (
	DetailModeView DetailMode = iota
	DetailModeApprove
	DetailModeReject
)

// ReviewSubmittedMsg carries a completed submission back onto the UI thread.
// Recorded is true only after the review and its decision commit together.
// A result for another request is never applied to the currently displayed one.
type ReviewSubmittedMsg struct {
	RequestID             string
	Decision              db.Decision
	Recorded              bool
	Request               *db.Request
	Review                *db.Review
	Approvals, Rejections int
	Err                   error
}

// DetailModel is the Bubble Tea model for request detail view.
type DetailModel struct {
	Request  *db.Request
	Reviews  []db.Review
	Session  *db.Session
	Width    int
	Height   int
	KeyMap   DetailKeyMap
	Mode     DetailMode
	viewport viewport.Model
	ready    bool

	approveForm *ApproveModel
	rejectForm  *RejectModel

	OnBack    func() tea.Cmd
	OnApprove func(requestID string, comments string) tea.Cmd
	OnReject  func(requestID string, reason string) tea.Cmd
	OnCopy    func(command string) tea.Cmd
	OnExecute func(requestID string) tea.Cmd

	ReviewPending  bool
	ReviewFeedback string
	SnapshotError  string
	reviewFailed   bool
	copied         bool
}

func NewDetailModel(request *db.Request, reviews []db.Review) *DetailModel {
	return &DetailModel{Request: request, Reviews: reviews, KeyMap: DefaultDetailKeyMap(), Mode: DetailModeView}
}

func (m *DetailModel) WithSession(s *db.Session) *DetailModel {
	m.Session = s
	return m
}

func (m *DetailModel) Init() tea.Cmd { return nil }

func (m *DetailModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case ReviewSubmittedMsg:
		if m.Request == nil || msg.RequestID != m.Request.ID || !m.ReviewPending {
			return m, nil
		}
		m.ReviewPending = false
		if msg.Recorded && msg.Request != nil && msg.Review != nil && msg.Request.ID == m.Request.ID && msg.Review.RequestID == m.Request.ID {
			m.Request = msg.Request
			found := false
			for _, review := range m.Reviews {
				found = found || review.ID == msg.Review.ID
			}
			if !found {
				m.Reviews = append(m.Reviews, *msg.Review)
			}
			m.ReviewFeedback = fmt.Sprintf("Review recorded: %s. Request: %s (%d/%d approvals, %d rejections).", msg.Review.Decision, msg.Request.Status, msg.Approvals, msg.Request.MinApprovals, msg.Rejections)
			m.reviewFailed = false
			m.approveForm, m.rejectForm = nil, nil
			m.Mode = DetailModeView
		} else {
			m.reviewFailed = true
			m.ReviewFeedback = "Review was not recorded."
			if msg.Err != nil {
				m.ReviewFeedback += " " + msg.Err.Error()
			}
			// Preserve typed comments/reason so a transient failure can be
			// corrected and retried explicitly, never automatically.
			if msg.Decision == db.DecisionApprove && m.approveForm != nil {
				m.approveForm.Submitted = false
				m.Mode = DetailModeApprove
			} else if msg.Decision == db.DecisionReject && m.rejectForm != nil {
				m.rejectForm.Submitted = false
				m.Mode = DetailModeReject
			}
		}
		if m.ready {
			m.viewport.SetContent(m.renderContent())
		}
		return m, nil

	case tea.WindowSizeMsg:
		m.Width, m.Height = msg.Width, msg.Height
		if !m.ready {
			m.viewport = viewport.New(max(1, msg.Width), max(1, msg.Height-4))
			m.ready = true
		} else {
			m.viewport.Width, m.viewport.Height = max(1, msg.Width), max(1, msg.Height-4)
		}
		m.viewport.SetContent(m.renderContent())
		if m.approveForm != nil {
			_, cmd := m.approveForm.Update(msg)
			cmds = append(cmds, cmd)
		}
		if m.rejectForm != nil {
			_, cmd := m.rejectForm.Update(msg)
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)

	case tea.KeyMsg:
		if key.Matches(msg, m.KeyMap.Quit) {
			return m, tea.Quit
		}
		if m.Mode == DetailModeApprove && m.approveForm != nil {
			updated, cmd := m.approveForm.Update(msg)
			m.approveForm = updated.(*ApproveModel)
			cmds = append(cmds, cmd)
			if m.approveForm.Submitted && !m.ReviewPending {
				if !m.canApprove() {
					m.approveForm.Submitted = false
					m.ReviewFeedback, m.reviewFailed = "Approval is unavailable; cancel the form and refresh the request.", true
				} else if m.OnApprove == nil {
					m.approveForm.Submitted = false
					m.ReviewFeedback, m.reviewFailed = "Approval is unavailable in this view.", true
				} else {
					submit := m.OnApprove(m.Request.ID, m.approveForm.Comments)
					if submit == nil {
						m.approveForm.Submitted = false
						m.ReviewFeedback, m.reviewFailed = "Approval did not start.", true
					} else {
						m.ReviewPending, m.reviewFailed = true, false
						m.ReviewFeedback = "Recording approval..."
						m.Mode = DetailModeView
						cmds = append(cmds, submit)
					}
				}
			} else if m.approveForm.Cancelled {
				m.Mode, m.approveForm = DetailModeView, nil
			}
			// Return the submission command, not just the textarea command.
			return m, tea.Batch(cmds...)
		}
		if m.Mode == DetailModeReject && m.rejectForm != nil {
			updated, cmd := m.rejectForm.Update(msg)
			m.rejectForm = updated.(*RejectModel)
			cmds = append(cmds, cmd)
			if m.rejectForm.Submitted && !m.ReviewPending {
				if !m.canReject() {
					m.rejectForm.Submitted = false
					m.ReviewFeedback, m.reviewFailed = "Rejection is unavailable; cancel the form and refresh the request.", true
				} else if m.OnReject == nil {
					m.rejectForm.Submitted = false
					m.ReviewFeedback, m.reviewFailed = "Rejection is unavailable in this view.", true
				} else {
					submit := m.OnReject(m.Request.ID, m.rejectForm.Reason)
					if submit == nil {
						m.rejectForm.Submitted = false
						m.ReviewFeedback, m.reviewFailed = "Rejection did not start.", true
					} else {
						m.ReviewPending, m.reviewFailed = true, false
						m.ReviewFeedback = "Recording rejection..."
						m.Mode = DetailModeView
						cmds = append(cmds, submit)
					}
				}
			} else if m.rejectForm.Cancelled {
				m.Mode, m.rejectForm = DetailModeView, nil
			}
			return m, tea.Batch(cmds...)
		}

		switch {
		case key.Matches(msg, m.KeyMap.Approve):
			if m.canApprove() {
				m.Mode = DetailModeApprove
				m.approveForm = NewApproveModel(m.Request)
				m.approveForm.Width = m.Width
				m.ReviewFeedback = ""
				return m, m.approveForm.Init()
			}
		case key.Matches(msg, m.KeyMap.Reject):
			if m.canReject() {
				m.Mode = DetailModeReject
				m.rejectForm = NewRejectModel(m.Request)
				m.rejectForm.Width = m.Width
				m.ReviewFeedback = ""
				return m, m.rejectForm.Init()
			}
		case key.Matches(msg, m.KeyMap.Copy):
			m.copied = true
			if m.OnCopy != nil {
				cmds = append(cmds, m.OnCopy(m.Request.Command.Raw))
			}
			cmds = append(cmds, tea.Tick(2*time.Second, func(t time.Time) tea.Msg { return clearCopiedMsg{} }))
		case key.Matches(msg, m.KeyMap.Execute):
			if m.canExecute() && m.OnExecute != nil {
				cmds = append(cmds, m.OnExecute(m.Request.ID))
			}
		case key.Matches(msg, m.KeyMap.Back):
			if m.OnBack != nil {
				cmds = append(cmds, m.OnBack())
			}
			return m, tea.Batch(cmds...)
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
	if m.ready {
		var vpCmd tea.Cmd
		m.viewport, vpCmd = m.viewport.Update(msg)
		cmds = append(cmds, vpCmd)
	}
	return m, tea.Batch(cmds...)
}

type clearCopiedMsg struct{}

func (m *DetailModel) View() string {
	th := theme.Current
	feedback := ""
	if m.SnapshotError != "" {
		feedback = lipgloss.NewStyle().Foreground(th.Red).Render(m.SnapshotError) + "\n"
	}
	if m.ReviewFeedback != "" {
		color := th.Green
		if m.reviewFailed {
			color = th.Red
		} else if m.ReviewPending {
			color = th.Yellow
		}
		feedback += lipgloss.NewStyle().Foreground(color).Render(m.ReviewFeedback) + "\n"
	}
	if m.Mode == DetailModeApprove && m.approveForm != nil {
		return feedback + m.approveForm.View()
	}
	if m.Mode == DetailModeReject && m.rejectForm != nil {
		return feedback + m.rejectForm.View()
	}
	var b strings.Builder
	b.WriteString(m.renderHeader())
	b.WriteString("\n")
	b.WriteString(feedback)
	if m.ready {
		b.WriteString(m.viewport.View())
	} else {
		b.WriteString(m.renderContent())
	}
	b.WriteString("\n")
	footerStyle := lipgloss.NewStyle().Foreground(th.Subtext).Background(th.Surface).Width(m.Width).Padding(0, 1)
	b.WriteString(footerStyle.Render(m.renderFooter()))
	return b.String()
}

func (m *DetailModel) renderHeader() string {
	th := theme.Current
	idStyle := lipgloss.NewStyle().Foreground(th.Mauve).Bold(true)
	statusBadge := components.RenderStatusBadge(string(m.Request.Status))
	tierIndicator := components.RenderRiskIndicator(string(m.Request.RiskTier))
	header := fmt.Sprintf("%s  %s  %s", idStyle.Render(m.Request.ID), statusBadge, tierIndicator)
	return lipgloss.NewStyle().Background(th.Surface).Width(m.Width).Padding(0, 1).Render(header)
}

func (m *DetailModel) renderContent() string {
	th := theme.Current
	var sections []string
	cmdBox := components.NewCommandBox(m.Request.Command.Raw).WithHint(true)
	if m.Request.Command.DisplayRedacted != "" {
		cmdBox = cmdBox.WithRedacted(m.Request.Command.DisplayRedacted)
	}
	if m.Width > 0 {
		cmdBox = cmdBox.WithMaxWidth(m.Width - 4)
	}
	sections = append(sections, cmdBox.Render())
	sections = append(sections, m.renderRequestorInfo())
	if justification := m.renderJustification(); justification != "" {
		sections = append(sections, justification)
	}
	if m.Request.DryRun != nil && m.Request.DryRun.Output != "" {
		sections = append(sections, m.renderDryRun())
	}
	if len(m.Request.Attachments) > 0 {
		sections = append(sections, m.renderAttachments())
	}
	sections = append(sections, m.renderTimeline())
	if len(m.Reviews) > 0 {
		sections = append(sections, m.renderReviews())
	}
	divider := lipgloss.NewStyle().Foreground(th.Overlay0).Render(strings.Repeat("─", max(0, m.Width-4)))
	return strings.Join(sections, "\n"+divider+"\n\n")
}

func (m *DetailModel) renderRequestorInfo() string {
	th := theme.Current
	sectionTitle := lipgloss.NewStyle().Foreground(th.Blue).Bold(true).Render("Requestor")
	agentStyle := lipgloss.NewStyle().Foreground(th.Text)
	metaStyle := lipgloss.NewStyle().Foreground(th.Subtext)
	info := fmt.Sprintf("%s %s (%s)\n%s", icons.Current().Agent, agentStyle.Render(m.Request.RequestorAgent), metaStyle.Render(m.Request.RequestorModel), metaStyle.Render("Requested "+formatTimeAgo(m.Request.CreatedAt)))
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

func (m *DetailModel) renderJustification() string {
	th := theme.Current
	j := m.Request.Justification
	if j.Reason == "" && j.ExpectedEffect == "" && j.Goal == "" && j.SafetyArgument == "" {
		return ""
	}
	sectionTitle := lipgloss.NewStyle().Foreground(th.Blue).Bold(true).Render("Justification")
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

func (m *DetailModel) renderDryRun() string {
	th := theme.Current
	sectionTitle := lipgloss.NewStyle().Foreground(th.Blue).Bold(true).Render("Dry Run Output")
	cmdStyle := lipgloss.NewStyle().Foreground(th.Subtext).Italic(true)
	outputStyle := lipgloss.NewStyle().Foreground(th.Text).Background(th.Surface0).Padding(0, 1)
	output := m.Request.DryRun.Output
	if len(output) > 500 {
		output = output[:500] + "\n... (truncated)"
	}
	return sectionTitle + "\n" + cmdStyle.Render("$ "+m.Request.DryRun.Command) + "\n" + outputStyle.Render(output)
}

func (m *DetailModel) renderAttachments() string {
	th := theme.Current
	sectionTitle := lipgloss.NewStyle().Foreground(th.Blue).Bold(true).Render(fmt.Sprintf("Attachments (%d)", len(m.Request.Attachments)))
	var lines []string
	for i, att := range m.Request.Attachments {
		typeIcon := attachmentIcon(string(att.Type))
		typeBadge := lipgloss.NewStyle().Foreground(th.Peach).Render(string(att.Type))
		preview := att.Content
		if len(preview) > 100 {
			preview = preview[:100] + "..."
		}
		preview = strings.ReplaceAll(preview, "\n", " ")
		lines = append(lines, fmt.Sprintf("%d. %s %s: %s", i+1, typeIcon, typeBadge, lipgloss.NewStyle().Foreground(th.Subtext).Render(preview)))
	}
	return sectionTitle + "\n" + strings.Join(lines, "\n")
}

func (m *DetailModel) renderTimeline() string {
	th := theme.Current
	sectionTitle := lipgloss.NewStyle().Foreground(th.Blue).Bold(true).Render("Timeline")
	tl := components.NewTimeline().WithCurrent(string(m.Request.Status))
	tl.AddEvent("created", m.Request.CreatedAt, m.Request.RequestorAgent, "Request submitted")
	if m.Request.Status != db.StatusPending {
		tl.AddEvent("pending", m.Request.CreatedAt.Add(time.Millisecond), "", "Waiting for approval")
	} else {
		tl.AddEvent("pending", time.Time{}, "", "Awaiting review")
	}
	for _, rev := range m.Reviews {
		if rev.Decision == db.DecisionApprove {
			tl.AddEvent("approved", rev.CreatedAt, rev.ReviewerAgent, rev.Comments)
		} else {
			tl.AddEvent("rejected", rev.CreatedAt, rev.ReviewerAgent, rev.Comments)
		}
	}
	if m.Request.Execution != nil && m.Request.Execution.ExecutedAt != nil {
		exitInfo := ""
		if m.Request.Execution.ExitCode != nil {
			exitInfo = fmt.Sprintf("exit code %d", *m.Request.Execution.ExitCode)
		}
		tl.AddEvent("executed", *m.Request.Execution.ExecutedAt, m.Request.Execution.ExecutedByAgent, exitInfo)
	}
	return sectionTitle + "\n" + tl.Render()
}

func (m *DetailModel) renderReviews() string {
	th := theme.Current
	approvals := 0
	for _, r := range m.Reviews {
		if r.Decision == db.DecisionApprove {
			approvals++
		}
	}
	sectionTitle := lipgloss.NewStyle().Foreground(th.Blue).Bold(true).Render(fmt.Sprintf("Reviews (%d/%d required)", approvals, m.Request.MinApprovals))
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

func (m *DetailModel) renderFooter() string {
	th := theme.Current
	var keys []string
	keyStyle := lipgloss.NewStyle().Foreground(th.Mauve).Bold(true)
	descStyle := lipgloss.NewStyle().Foreground(th.Subtext)
	if m.canApprove() {
		keys = append(keys, keyStyle.Render("[a]")+descStyle.Render("pprove"))
	}
	if m.canReject() {
		keys = append(keys, keyStyle.Render("[r]")+descStyle.Render("eject"))
	}
	if m.canExecute() && m.OnExecute != nil {
		keys = append(keys, keyStyle.Render("[x]")+descStyle.Render(" execute"))
	}
	if m.Session == nil {
		keys = append(keys, descStyle.Render("Read-only: no authenticated reviewer session"))
	}
	if m.copied {
		keys = append(keys, lipgloss.NewStyle().Foreground(th.Green).Render("Copied!"))
	} else {
		keys = append(keys, keyStyle.Render("[c]")+descStyle.Render("opy"))
	}
	keys = append(keys, keyStyle.Render("[esc]")+descStyle.Render(" back"))
	keys = append(keys, descStyle.Render(fmt.Sprintf(" %d%%", int(m.viewport.ScrollPercent()*100))))
	return strings.Join(keys, "  ")
}

// Eligibility is only a UI hint. ApplyReview revalidates under a write lock.
func (m *DetailModel) canApprove() bool {
	return m.canReject() && (!m.Request.RequireDifferentModel || m.Session.Model != m.Request.RequestorModel)
}

func (m *DetailModel) canReject() bool {
	if m.SnapshotError != "" || m.ReviewPending || m.Request == nil || m.Session == nil || !m.Session.IsActive() {
		return false
	}
	if m.Request.Status != db.StatusPending && m.Request.Status != db.StatusEscalated {
		return false
	}
	if m.Request.Status == db.StatusPending && m.Request.ExpiresAt != nil && !time.Now().Before(*m.Request.ExpiresAt) {
		return false
	}
	if m.Session.ID == m.Request.RequestorSessionID || (m.Session.AgentName != "" && m.Session.AgentName == m.Request.RequestorAgent) {
		return false
	}
	for _, rev := range m.Reviews {
		if rev.ReviewerSessionID == m.Session.ID || (m.Session.AgentName != "" && rev.ReviewerAgent == m.Session.AgentName) {
			return false
		}
	}
	return true
}

// ApplySnapshot refreshes display evidence without silently retargeting an open
// review form. A failed read keeps the last display but disables new votes.
// A committing review owns the view until its result arrives; the root also
// rejects reads that started before that commit.
func (m *DetailModel) ApplySnapshot(target *db.Request, reviews []db.Review, session *db.Session, err error) {
	if m.ReviewPending {
		return
	}
	if err != nil {
		m.SnapshotError = "Request refresh failed; review disabled: " + err.Error()
		return
	}
	if target == nil || m.Request == nil || target.ID != m.Request.ID || target.ProjectPath != m.Request.ProjectPath {
		m.SnapshotError = "Request refresh returned a different target; review disabled."
		return
	}
	if m.Mode != DetailModeView && target.Command.Hash != m.Request.Command.Hash {
		m.SnapshotError = "Command changed while this form was open. Cancel the form and refresh before reviewing."
		return
	}
	m.Request, m.Reviews, m.Session = target, reviews, session
	m.SnapshotError = ""
	if m.approveForm != nil {
		m.approveForm.Request = target
	}
	if m.rejectForm != nil {
		m.rejectForm.Request = target
	}
	if m.ready {
		m.viewport.SetContent(m.renderContent())
	}
}

func (m *DetailModel) canExecute() bool {
	if m.Request == nil || m.Request.Status != db.StatusApproved {
		return false
	}
	return m.Request.ApprovalExpiresAt == nil || time.Now().Before(*m.Request.ApprovalExpiresAt)
}

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
