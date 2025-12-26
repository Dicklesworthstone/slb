// Package components provides reusable TUI components for SLB.
package components

import (
	"strings"

	"github.com/Dicklesworthstone/slb/internal/tui/theme"
	"github.com/Dicklesworthstone/slb/internal/utils"
	"github.com/charmbracelet/lipgloss"
)

// CommandBox renders a command in a styled box.
type CommandBox struct {
	Command    string
	Redacted   string // Optional redacted version
	ShowHint   bool   // Show copy hint
	MaxWidth   int
	Scrollable bool
}

// NewCommandBox creates a new command box component.
func NewCommandBox(command string) *CommandBox {
	return &CommandBox{
		Command:  command,
		MaxWidth: 80,
		ShowHint: true,
	}
}

// WithRedacted sets a redacted display version.
func (c *CommandBox) WithRedacted(redacted string) *CommandBox {
	c.Redacted = redacted
	return c
}

// WithMaxWidth sets the maximum width.
func (c *CommandBox) WithMaxWidth(width int) *CommandBox {
	c.MaxWidth = width
	return c
}

// WithHint enables or disables the copy hint.
func (c *CommandBox) WithHint(show bool) *CommandBox {
	c.ShowHint = show
	return c
}

// Render renders the command box as a string.
func (c *CommandBox) Render() string {
	t := theme.Current

	// Use redacted version if available
	displayCmd := c.Command
	if c.Redacted != "" {
		displayCmd = c.Redacted
	}

	displayCmd = utils.SanitizeInput(displayCmd)

	// Truncate if needed
	if c.MaxWidth > 0 && len(displayCmd) > c.MaxWidth {
		if c.MaxWidth > 3 {
			displayCmd = displayCmd[:c.MaxWidth-3] + "..."
		} else {
			// For very small max widths, just truncate without ellipsis
			displayCmd = displayCmd[:c.MaxWidth]
		}
	}

	// Command style
	cmdStyle := lipgloss.NewStyle().
		Foreground(t.Green).
		Background(t.Mantle).
		Padding(0, 1)

	// Box style
	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.Overlay0).
		Padding(0, 1)

	content := cmdStyle.Render(displayCmd)

	// Add hint if enabled
	if c.ShowHint {
		hintStyle := lipgloss.NewStyle().
			Foreground(t.Subtext).
			Italic(true)
		hint := hintStyle.Render("  (Ctrl+C to copy)")
		content = content + hint
	}

	return boxStyle.Render(content)
}

// RenderCompact renders a minimal command display.
func (c *CommandBox) RenderCompact() string {
	t := theme.Current

	displayCmd := c.Command
	if c.Redacted != "" {
		displayCmd = c.Redacted
	}

	displayCmd = utils.SanitizeInput(displayCmd)

	// Truncate more aggressively for compact view
	maxLen := 40
	if len(displayCmd) > maxLen {
		displayCmd = displayCmd[:maxLen-3] + "..."
	}

	style := lipgloss.NewStyle().
		Foreground(t.Green).
		Background(t.Surface).
		Padding(0, 1)

	return style.Render(displayCmd)
}

// RenderFull renders the full command with all details.
func (c *CommandBox) RenderFull() string {
	t := theme.Current

	var lines []string

	// Full command (possibly wrapped)
	displayCmd := c.Command
	if c.Redacted != "" {
		displayCmd = c.Redacted
	}

	displayCmd = utils.SanitizeInput(displayCmd)

	cmdStyle := lipgloss.NewStyle().
		Foreground(t.Green)

	lines = append(lines, cmdStyle.Render(displayCmd))

	// Show original vs redacted if different
	if c.Redacted != "" && c.Redacted != c.Command {
		redactedNote := lipgloss.NewStyle().
			Foreground(t.Yellow).
			Italic(true).
			Render("(sensitive content redacted)")
		lines = append(lines, redactedNote)
	}

	content := strings.Join(lines, "\n")

	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.Overlay0).
		Padding(1, 2)

	return boxStyle.Render(content)
}
