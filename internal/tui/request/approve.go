// Package request provides TUI views for request management.
package request

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/tui/components"
	"github.com/Dicklesworthstone/slb/internal/tui/theme"
)

// ApproveKeyMap defines keybindings for the approval form.
type ApproveKeyMap struct {
	Submit key.Binding
	Cancel key.Binding
	Tab    key.Binding
}

// DefaultApproveKeyMap returns the default keybindings.
func DefaultApproveKeyMap() ApproveKeyMap {
	return ApproveKeyMap{
		Submit: key.NewBinding(
			key.WithKeys("ctrl+s", "ctrl+enter"),
			key.WithHelp("ctrl+s", "submit"),
		),
		Cancel: key.NewBinding(
			key.WithKeys("esc"),
			key.WithHelp("esc", "cancel"),
		),
		Tab: key.NewBinding(
			key.WithKeys("tab"),
			key.WithHelp("tab", "next field"),
		),
	}
}

// ApproveModel is the Bubble Tea model for the approval form.
type ApproveModel struct {
	Request   *db.Request
	Width     int
	Height    int
	KeyMap    ApproveKeyMap
	Submitted bool
	Cancelled bool

	// Form fields
	Comments      string
	commentsInput textarea.Model

	// Field focus
	focused int
}

// NewApproveModel creates a new approval form model.
func NewApproveModel(request *db.Request) *ApproveModel {
	// Create textarea for comments
	ti := textarea.New()
	ti.Placeholder = "Optional: Add comments about your approval..."
	ti.ShowLineNumbers = false
	ti.SetHeight(4)
	ti.Focus()

	return &ApproveModel{
		Request:       request,
		KeyMap:        DefaultApproveKeyMap(),
		commentsInput: ti,
		focused:       0,
	}
}

// Init initializes the model.
func (m *ApproveModel) Init() tea.Cmd {
	return textarea.Blink
}

// Update handles messages.
func (m *ApproveModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.Width = msg.Width
		m.Height = msg.Height
		m.commentsInput.SetWidth(m.Width - 8)
		return m, nil

	case tea.KeyMsg:
		switch {
		case key.Matches(msg, m.KeyMap.Submit):
			m.Comments = m.commentsInput.Value()
			m.Submitted = true
			return m, nil

		case key.Matches(msg, m.KeyMap.Cancel):
			m.Cancelled = true
			return m, nil
		}
	}

	// Update textarea
	var taCmd tea.Cmd
	m.commentsInput, taCmd = m.commentsInput.Update(msg)
	cmds = append(cmds, taCmd)

	return m, tea.Batch(cmds...)
}

// View renders the model.
func (m *ApproveModel) View() string {
	th := theme.Current
	var b strings.Builder

	// Title
	titleStyle := lipgloss.NewStyle().
		Foreground(th.Green).
		Bold(true).
		Padding(1, 0)
	b.WriteString(titleStyle.Render("Approve Request"))
	b.WriteString("\n\n")

	// Request summary
	summaryStyle := lipgloss.NewStyle().
		Foreground(th.Subtext).
		Padding(0, 2)

	cmdPreview := m.Request.Command.Raw
	if len(cmdPreview) > 60 {
		cmdPreview = cmdPreview[:60] + "..."
	}

	tierBadge := components.RenderRiskIndicator(string(m.Request.RiskTier))

	summary := lipgloss.JoinVertical(lipgloss.Left,
		"Request: "+m.Request.ID,
		"Command: "+cmdPreview,
		"Tier: "+tierBadge,
	)
	b.WriteString(summaryStyle.Render(summary))
	b.WriteString("\n\n")

	// Confirmation message
	confirmStyle := lipgloss.NewStyle().
		Foreground(th.Yellow).
		Bold(true).
		Padding(0, 2)

	confirmMsg := "You are about to APPROVE this request."
	confirmMsg += "\n" + m.Request.ApprovalPolicy.Label() + "."
	if m.Request.RiskTier == db.RiskTierCritical {
		confirmMsg += "\nThis is a CRITICAL tier request requiring 2+ approvals."
	}
	b.WriteString(confirmStyle.Render(confirmMsg))
	b.WriteString("\n\n")

	// Comments input
	labelStyle := lipgloss.NewStyle().
		Foreground(th.Blue).
		Bold(true).
		Padding(0, 2)
	b.WriteString(labelStyle.Render("Comments (optional):"))
	b.WriteString("\n")

	inputStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(th.Overlay0).
		Padding(0, 1).
		Margin(0, 2)
	b.WriteString(inputStyle.Render(m.commentsInput.View()))
	b.WriteString("\n\n")

	// Footer with keybindings
	footerStyle := lipgloss.NewStyle().
		Foreground(th.Subtext).
		Padding(0, 2)

	keyStyle := lipgloss.NewStyle().Foreground(th.Mauve).Bold(true)
	descStyle := lipgloss.NewStyle().Foreground(th.Subtext)

	footer := keyStyle.Render("[ctrl+s]") + descStyle.Render(" submit") + "  " +
		keyStyle.Render("[esc]") + descStyle.Render(" cancel")
	b.WriteString(footerStyle.Render(footer))

	// Wrap in a panel
	panelStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(th.Green).
		Padding(1, 2).
		Width(m.Width - 4)

	return panelStyle.Render(b.String())
}
