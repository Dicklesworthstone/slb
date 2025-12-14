// Package components provides status badge components.
package components

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/Dicklesworthstone/slb/internal/tui/icons"
	"github.com/Dicklesworthstone/slb/internal/tui/theme"
)

// StatusBadge renders a status as a colored badge.
type StatusBadge struct {
	Status   string
	Compact  bool
	ShowIcon bool
}

// NewStatusBadge creates a new status badge.
func NewStatusBadge(status string) *StatusBadge {
	return &StatusBadge{
		Status:   status,
		ShowIcon: true,
	}
}

// AsCompact sets the badge to compact mode.
func (s *StatusBadge) AsCompact() *StatusBadge {
	s.Compact = true
	return s
}

// WithIcon enables or disables the icon.
func (s *StatusBadge) WithIcon(show bool) *StatusBadge {
	s.ShowIcon = show
	return s
}

// Render renders the status badge.
func (s *StatusBadge) Render() string {
	t := theme.Current
	status := strings.ToLower(s.Status)

	// Get colors based on status
	var fg, bg lipgloss.Color
	switch status {
	case "pending":
		fg, bg = t.Base, t.Blue
	case "approved":
		fg, bg = t.Base, t.Green
	case "rejected":
		fg, bg = t.Base, t.Red
	case "executed":
		fg, bg = t.Base, t.Green
	case "failed":
		fg, bg = t.Base, t.Red
	case "timeout":
		fg, bg = t.Base, t.Yellow
	case "cancelled":
		fg, bg = t.Text, t.Overlay0
	case "escalated":
		fg, bg = t.Base, t.Peach
	case "executing":
		fg, bg = t.Base, t.Teal
	default:
		fg, bg = t.Text, t.Surface
	}

	style := lipgloss.NewStyle().
		Foreground(fg).
		Background(bg).
		Bold(true).
		Padding(0, 1)

	// Build content
	var content string
	if s.ShowIcon {
		icon := icons.StatusIcon(status)
		if s.Compact {
			content = icon
		} else {
			content = icon + " " + strings.ToUpper(s.Status)
		}
	} else {
		if s.Compact {
			content = strings.ToUpper(s.Status[:1])
		} else {
			content = strings.ToUpper(s.Status)
		}
	}

	return style.Render(content)
}

// RenderStatusBadge is a convenience function to render a status badge.
func RenderStatusBadge(status string) string {
	return NewStatusBadge(status).Render()
}

// RenderStatusBadgeCompact is a convenience function for compact badges.
func RenderStatusBadgeCompact(status string) string {
	return NewStatusBadge(status).AsCompact().Render()
}
