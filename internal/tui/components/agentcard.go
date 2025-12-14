// Package components provides agent card components.
package components

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/Dicklesworthstone/slb/internal/tui/icons"
	"github.com/Dicklesworthstone/slb/internal/tui/theme"
)

// AgentStatus represents the status of an agent.
type AgentStatus string

const (
	AgentStatusActive AgentStatus = "active"
	AgentStatusIdle   AgentStatus = "idle"
	AgentStatusStale  AgentStatus = "stale"
	AgentStatusEnded  AgentStatus = "ended"
)

// AgentInfo holds information about an agent for display.
type AgentInfo struct {
	Name        string
	Program     string
	Model       string
	Status      AgentStatus
	LastActive  time.Time
	SessionID   string
	ProjectPath string
}

// AgentCard renders an agent as a styled card.
type AgentCard struct {
	Agent    AgentInfo
	Compact  bool
	Selected bool
	Width    int
}

// NewAgentCard creates a new agent card.
func NewAgentCard(agent AgentInfo) *AgentCard {
	return &AgentCard{
		Agent: agent,
		Width: 40,
	}
}

// AsCompact sets the card to compact mode.
func (a *AgentCard) AsCompact() *AgentCard {
	a.Compact = true
	return a
}

// AsSelected sets the card as selected.
func (a *AgentCard) AsSelected(selected bool) *AgentCard {
	a.Selected = selected
	return a
}

// WithWidth sets the card width.
func (a *AgentCard) WithWidth(width int) *AgentCard {
	a.Width = width
	return a
}

// Render renders the agent card.
func (a *AgentCard) Render() string {
	t := theme.Current
	ic := icons.Current()

	if a.Compact {
		return a.renderCompact()
	}

	// Status indicator color
	var statusColor lipgloss.Color
	var statusText string
	switch a.Agent.Status {
	case AgentStatusActive:
		statusColor = t.Green
		statusText = "active"
	case AgentStatusIdle:
		statusColor = t.Yellow
		statusText = "idle"
	case AgentStatusStale:
		statusColor = t.Subtext
		statusText = "stale"
	case AgentStatusEnded:
		statusColor = t.Red
		statusText = "ended"
	default:
		statusColor = t.Subtext
		statusText = "unknown"
	}

	// Build card content
	var lines []string

	// Header: icon + name + status dot
	nameStyle := lipgloss.NewStyle().Foreground(t.Mauve).Bold(true)
	statusDot := lipgloss.NewStyle().Foreground(statusColor).Render("●")
	header := fmt.Sprintf("%s %s %s", ic.Agent, nameStyle.Render(a.Agent.Name), statusDot)
	lines = append(lines, header)

	// Program/Model
	dimStyle := lipgloss.NewStyle().Foreground(t.Subtext)
	programModel := dimStyle.Render(fmt.Sprintf("%s / %s", a.Agent.Program, a.Agent.Model))
	lines = append(lines, programModel)

	// Status and last active
	statusBadge := lipgloss.NewStyle().
		Foreground(statusColor).
		Render(strings.ToUpper(statusText))
	timeAgo := formatTimeAgo(a.Agent.LastActive)
	statusLine := fmt.Sprintf("%s  •  %s", statusBadge, dimStyle.Render(timeAgo))
	lines = append(lines, statusLine)

	content := strings.Join(lines, "\n")

	// Card style
	cardStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.Overlay0).
		Padding(0, 1).
		Width(a.Width)

	if a.Selected {
		cardStyle = cardStyle.
			BorderForeground(t.Mauve).
			Background(t.Surface)
	}

	return cardStyle.Render(content)
}

// renderCompact renders a single-line compact version.
func (a *AgentCard) renderCompact() string {
	t := theme.Current
	ic := icons.Current()

	// Status indicator color
	var statusColor lipgloss.Color
	switch a.Agent.Status {
	case AgentStatusActive:
		statusColor = t.Green
	case AgentStatusIdle:
		statusColor = t.Yellow
	case AgentStatusStale:
		statusColor = t.Subtext
	case AgentStatusEnded:
		statusColor = t.Red
	default:
		statusColor = t.Subtext
	}

	nameStyle := lipgloss.NewStyle().Foreground(t.Mauve)
	dimStyle := lipgloss.NewStyle().Foreground(t.Subtext)
	statusDot := lipgloss.NewStyle().Foreground(statusColor).Render("●")

	compact := fmt.Sprintf("%s %s %s  %s",
		ic.Agent,
		statusDot,
		nameStyle.Render(a.Agent.Name),
		dimStyle.Render(a.Agent.Program),
	)

	if a.Selected {
		return lipgloss.NewStyle().Background(t.Surface).Render(compact)
	}
	return compact
}

// formatTimeAgo formats a time as a human-readable "ago" string.
func formatTimeAgo(t time.Time) string {
	if t.IsZero() {
		return "never"
	}

	d := time.Since(t)

	if d < time.Minute {
		return "just now"
	} else if d < time.Hour {
		mins := int(d.Minutes())
		if mins == 1 {
			return "1 min ago"
		}
		return fmt.Sprintf("%d mins ago", mins)
	} else if d < 24*time.Hour {
		hours := int(d.Hours())
		if hours == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", hours)
	} else {
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	}
}

// RenderAgentCard is a convenience function to render an agent card.
func RenderAgentCard(agent AgentInfo) string {
	return NewAgentCard(agent).Render()
}

// RenderAgentCardCompact is a convenience function for compact cards.
func RenderAgentCardCompact(agent AgentInfo) string {
	return NewAgentCard(agent).AsCompact().Render()
}
