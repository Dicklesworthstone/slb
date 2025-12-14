// Package components provides risk tier indicator components.
package components

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/Dicklesworthstone/slb/internal/tui/theme"
)

// RiskIndicator renders a risk tier as a colored indicator.
type RiskIndicator struct {
	Tier       string
	Compact    bool
	ShowEmoji  bool
	ShowLabel  bool
}

// NewRiskIndicator creates a new risk indicator.
func NewRiskIndicator(tier string) *RiskIndicator {
	return &RiskIndicator{
		Tier:      tier,
		ShowEmoji: true,
		ShowLabel: true,
	}
}

// AsCompact sets the indicator to compact mode.
func (r *RiskIndicator) AsCompact() *RiskIndicator {
	r.Compact = true
	return r
}

// WithEmoji enables or disables the emoji.
func (r *RiskIndicator) WithEmoji(show bool) *RiskIndicator {
	r.ShowEmoji = show
	return r
}

// WithLabel enables or disables the label.
func (r *RiskIndicator) WithLabel(show bool) *RiskIndicator {
	r.ShowLabel = show
	return r
}

// Render renders the risk indicator.
func (r *RiskIndicator) Render() string {
	t := theme.Current
	tier := strings.ToLower(r.Tier)

	// Get colors based on tier
	var fg, bg lipgloss.Color
	var emoji string

	switch tier {
	case "critical":
		fg, bg = t.Base, t.Red
		emoji = "🔴"
	case "dangerous":
		fg, bg = t.Base, t.Peach
		emoji = "🟠"
	case "caution":
		fg, bg = t.Base, t.Yellow
		emoji = "🟡"
	case "safe":
		fg, bg = t.Base, t.Green
		emoji = "🟢"
	default:
		fg, bg = t.Text, t.Surface
		emoji = "⚪"
	}

	style := lipgloss.NewStyle().
		Foreground(fg).
		Background(bg).
		Bold(true).
		Padding(0, 1)

	// Build content
	var parts []string

	if r.ShowEmoji {
		parts = append(parts, emoji)
	}

	if r.ShowLabel {
		if r.Compact {
			parts = append(parts, strings.ToUpper(tier[:1]))
		} else {
			parts = append(parts, strings.ToUpper(tier))
		}
	}

	content := strings.Join(parts, " ")
	if len(parts) == 0 {
		content = strings.ToUpper(tier)
	}

	return style.Render(content)
}

// RenderRiskIndicator is a convenience function to render a tier indicator.
func RenderRiskIndicator(tier string) string {
	return NewRiskIndicator(tier).Render()
}

// RenderRiskIndicatorCompact is a convenience function for compact indicators.
func RenderRiskIndicatorCompact(tier string) string {
	return NewRiskIndicator(tier).AsCompact().Render()
}

// TierDescription returns a human-readable description for a tier.
func TierDescription(tier string) string {
	switch strings.ToLower(tier) {
	case "critical":
		return "Requires 2+ approvals. Data destruction, production deploys."
	case "dangerous":
		return "Requires 1 approval. Force pushes, schema changes."
	case "caution":
		return "Auto-approved after 30s. Minor changes with notification."
	case "safe":
		return "No approval needed. Read-only commands."
	default:
		return "Unknown risk tier."
	}
}

// MinApprovals returns the minimum approvals for a tier.
func MinApprovals(tier string) int {
	switch strings.ToLower(tier) {
	case "critical":
		return 2
	case "dangerous":
		return 1
	case "caution":
		return 0 // Auto-approved
	case "safe":
		return 0 // Skipped
	default:
		return 1
	}
}
