// Package cli implements colorized help and quick reference card using lipgloss.
package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
)

// Catppuccin Mocha color palette
var (
	colorMauve   = lipgloss.Color("#cba6f7") // Title
	colorBlue    = lipgloss.Color("#89b4fa") // Section headers
	colorGreen   = lipgloss.Color("#a6e3a1") // Commands
	colorYellow  = lipgloss.Color("#f9e2af") // Flags
	colorRed     = lipgloss.Color("#f38ba8") // CRITICAL tier
	colorPeach   = lipgloss.Color("#fab387") // DANGEROUS tier
	colorCaution = lipgloss.Color("#f9e2af") // CAUTION tier
	colorOverlay = lipgloss.Color("#6c7086") // Muted text
	colorText    = lipgloss.Color("#cdd6f4") // Normal text
	colorBase    = lipgloss.Color("#1e1e2e") // Background
)

// Styles
var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorMauve).
			MarginBottom(1)

	sectionStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorBlue).
			MarginTop(1)

	commandStyle = lipgloss.NewStyle().
			Foreground(colorGreen)

	flagStyle = lipgloss.NewStyle().
			Foreground(colorYellow)

	criticalStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorRed)

	dangerousStyle = lipgloss.NewStyle().
			Foreground(colorPeach)

	cautionStyle = lipgloss.NewStyle().
			Foreground(colorCaution)

	mutedStyle = lipgloss.NewStyle().
			Foreground(colorOverlay)

	boxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorBlue).
			Background(colorBase).
			Padding(1, 2).
			MarginTop(1).
			MarginBottom(1)
)

func showQuickReference() {
	width := clampWidth(detectWidth())
	useUnicode := supportsUnicode()

	border := lipgloss.RoundedBorder()
	if !useUnicode {
		border = lipgloss.Border{
			Top:         "-",
			Bottom:      "-",
			Left:        "|",
			Right:       "|",
			TopLeft:     "+",
			TopRight:    "+",
			BottomLeft:  "+",
			BottomRight: "+",
		}
	}

	container := boxStyle.Copy().Border(border).Width(width)

	titleText := " SLB QUICK REFERENCE — Dangerous Command Approval "
	titleRendered := gradientText(titleText, []lipgloss.Color{colorMauve, colorBlue})
	if !useUnicode {
		titleRendered = "SLB QUICK REFERENCE - Dangerous Command Approval"
	}
	title := titleStyle.Copy().Width(width - 4).Align(lipgloss.Center).Render(titleRendered)

	setup := renderSection(useUnicode, "🔷 SETUP (once per session)", []string{
		bullet("slb session start -a <name> -p <program> -m <model> -j", "start an agent session (get session_id)"),
		bullet("slb daemon start", "optional: start background daemon (faster + notifications)"),
		bullet("slb patterns test \"rm -rf ./build\" --json", "see if command needs approval"),
		bullet("slb -C /path/to/repo pending -j", "operate on a different project (optional)"),
	})

	requestor := renderSection(useUnicode, "🔶 AS REQUESTOR (dangerous commands)", []string{
		bullet("slb run \"rm -rf ./build\" -s $SID --reason \"Cleanup\" --timeout 300 -j", "classify, request approval, wait, then execute"),
		bullet("slb status <request-id> --wait -j", "block until approved/rejected/timeout"),
		bullet("slb execute <request-id> -s $SID -j", "execute once approved (client-side)"),
	})

	plumbing := renderSection(useUnicode, "🔧 PLUMBING (advanced)", []string{
		bullet("slb request \"...\" --wait --execute -s $SID --reason \"...\"", "submit without shorthand"),
		bullet("slb cancel <request-id>", "cancel pending"),
		bullet("slb rollback <request-id>", "apply rollback capture (if available)"),
	})

	reviewer := renderSection(useUnicode, "🔷 AS REVIEWER (check frequently)", []string{
		bullet("slb pending -j", "list pending approvals"),
		bullet("slb review <id> -j", "inspect details"),
		bullet("slb approve <id> -s $SID -k $SKEY --reason-response \"Verified\"", "approve (signed)"),
		bullet("slb reject <id> -s $SID -k $SKEY --reason \"Need safer path\"", "reject (signed)"),
	})

	patterns := renderSection(useUnicode, "🛡️ PATTERNS (agents can add, not remove)", []string{
		bullet("slb patterns add --tier critical \"^helm upgrade.*--force\" --reason \"Avoid outages\"", "tighten safety net"),
		bullet("slb patterns list --json", "see current patterns and tiers"),
	})

	tiers := tierLegend(useUnicode)
	flags := flagLegend(useUnicode)
	footer := footerLegend(useUnicode)

	content := lipgloss.JoinVertical(lipgloss.Left,
		title,
		setup,
		requestor,
		plumbing,
		reviewer,
		patterns,
		tiers,
		flags,
		footer,
	)

	fmt.Println(container.Render(content))
}

func clampWidth(w int) int {
	if w < 72 {
		return 72
	}
	if w > 100 {
		return 100
	}
	return w
}

func detectWidth() int {
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		return w
	}
	// fall back to environment or default
	if cols := os.Getenv("COLUMNS"); cols != "" {
		if v, err := strconv.Atoi(cols); err == nil && v > 0 {
			return v
		}
	}
	return 80
}

func supportsUnicode() bool {
	termEnv := strings.ToLower(os.Getenv("TERM"))
	locale := strings.ToLower(strings.Join([]string{
		os.Getenv("LC_ALL"),
		os.Getenv("LC_CTYPE"),
		os.Getenv("LANG"),
	}, " "))
	if strings.Contains(termEnv, "dumb") {
		return false
	}
	return strings.Contains(locale, "utf-8") || strings.Contains(locale, "utf8")
}

func gradientText(text string, colors []lipgloss.Color) string {
	if len(colors) == 0 || !supportsUnicode() {
		return text
	}
	runes := []rune(text)
	segments := len(colors)
	if segments == 1 {
		return lipgloss.NewStyle().Foreground(colors[0]).Render(text)
	}

	var b strings.Builder
	for i, r := range runes {
		// simple linear gradient selection
		idx := i * (segments - 1) / (len(runes) - 1)
		b.WriteString(lipgloss.NewStyle().Foreground(colors[idx]).Render(string(r)))
	}
	return b.String()
}

func bullet(command, desc string) string {
	return commandStyle.Render("  "+command) + mutedStyle.Render("  "+desc)
}

func renderSection(useUnicode bool, title string, lines []string) string {
	if !useUnicode {
		title = strings.TrimLeft(title, "🔷🔶🛡️ ") // strip icons for ASCII fallback
	}
	header := sectionStyle.Render(title)
	body := strings.Join(lines, "\n")
	return lipgloss.JoinVertical(lipgloss.Left, header, body)
}

func tierLegend(useUnicode bool) string {
	crit := "CRITICAL (2+)"
	dang := "DANGEROUS (1)"
	caut := "CAUTION (auto)"
	if useUnicode {
		crit = "🔴 " + crit
		dang = "🟠 " + dang
		caut = "🟡 " + caut
	}
	return lipgloss.JoinVertical(lipgloss.Left,
		sectionStyle.Render("🎯 RISK TIERS"),
		fmt.Sprintf("  %s   %s   %s", criticalStyle.Render(crit), dangerousStyle.Render(dang), cautionStyle.Render(caut)),
	)
}

func flagLegend(useUnicode bool) string {
	prefix := "🚩 GLOBAL FLAGS"
	if !useUnicode {
		prefix = "FLAGS"
	}
	return lipgloss.JoinVertical(lipgloss.Left,
		sectionStyle.Render(prefix),
		flagStyle.Render("  -j, --json")+mutedStyle.Render("              structured output"),
		flagStyle.Render("  -C, --project <dir>")+mutedStyle.Render("   override project path"),
		flagStyle.Render("  -s, --session-id <id>")+mutedStyle.Render(" session binding"),
		flagStyle.Render("  --actor <name>")+mutedStyle.Render("            actor identifier"),
		flagStyle.Render("  --db <path>")+mutedStyle.Render("               database path"),
	)
}

func footerLegend(useUnicode bool) string {
	human := "slb tui"
	help := "slb <command> --help"
	if !useUnicode {
		return mutedStyle.Render("HUMAN: " + human + "   HELP: " + help)
	}
	return lipgloss.JoinHorizontal(lipgloss.Left,
		mutedStyle.Render("HUMAN: "), commandStyle.Render(human),
		mutedStyle.Render("   HELP: "), commandStyle.Render(help),
	)
}
