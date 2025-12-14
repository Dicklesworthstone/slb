// Package icons provides icon constants with Nerd Font and ASCII fallbacks.
package icons

import (
	"os"
	"strings"
)

// IconSet defines a set of icons for the TUI.
type IconSet struct {
	// Status icons
	Approved   string
	Rejected   string
	Pending    string
	Executing  string
	Failed     string
	Timeout    string
	Cancelled  string
	Escalated  string

	// Tier icons
	Critical  string
	Dangerous string
	Caution   string
	Safe      string

	// UI icons
	Agent      string
	Daemon     string
	Session    string
	Command    string
	Time       string
	Warning    string
	Info       string
	Error      string
	Success    string
	Loading    string
	Refresh    string
	Search     string
	Filter     string
	Sort       string
	Expand     string
	Collapse   string
	Copy       string
	Edit       string
	Delete     string
	Add        string
	Check      string
	Cross      string
	Arrow      string
	ArrowRight string
	ArrowDown  string
	ArrowUp    string
	Dot        string
	Circle     string
	Square     string
	Diamond    string
	Star       string
	Lock       string
	Unlock     string
	Key        string
	User       string
	Users      string
	Terminal   string
	File       string
	Folder     string
	Git        string
	Database   string
	Cloud      string
}

// useNerdFonts checks if Nerd Fonts are likely available.
var useNerdFonts = detectNerdFonts()

// detectNerdFonts checks environment hints for Nerd Font support.
func detectNerdFonts() bool {
	// Check for terminal hints that suggest Nerd Font support
	term := os.Getenv("TERM")
	termProgram := os.Getenv("TERM_PROGRAM")
	slbIcons := os.Getenv("SLB_ICONS")

	// Explicit setting takes precedence
	if slbIcons != "" {
		return strings.ToLower(slbIcons) == "nerd" || slbIcons == "1" || strings.ToLower(slbIcons) == "true"
	}

	// Some terminals that often have Nerd Fonts
	nerdTerminals := []string{"kitty", "wezterm", "alacritty", "iTerm.app"}
	for _, t := range nerdTerminals {
		if strings.Contains(termProgram, t) || strings.Contains(term, t) {
			return true
		}
	}

	// Default to ASCII for safety
	return false
}

// SetNerdFonts explicitly enables or disables Nerd Font icons.
func SetNerdFonts(enabled bool) {
	useNerdFonts = enabled
}

// nerd returns the Nerd Font icon set.
func nerd() *IconSet {
	return &IconSet{
		// Status icons (using Nerd Font symbols)
		Approved:  "✓",
		Rejected:  "✗",
		Pending:   "",   // nf-fa-hourglass_half
		Executing: "",   // nf-fa-cog_spin (use spinner)
		Failed:    "",   // nf-fa-times_circle
		Timeout:   "",   // nf-fa-clock
		Cancelled: "",   // nf-fa-ban
		Escalated: "",   // nf-fa-exclamation_triangle

		// Tier icons
		Critical:  "",   // nf-fa-skull_crossbones or circle
		Dangerous: "",   // nf-fa-radiation
		Caution:   "",   // nf-fa-exclamation_triangle
		Safe:      "",   // nf-fa-check_circle

		// UI icons
		Agent:      "󰀄", // nf-md-robot
		Daemon:     "󰒍", // nf-md-cogs
		Session:    "", // nf-fa-plug
		Command:    "", // nf-fa-terminal
		Time:       "", // nf-fa-clock
		Warning:    "", // nf-fa-exclamation_triangle
		Info:       "", // nf-fa-info_circle
		Error:      "", // nf-fa-times_circle
		Success:    "", // nf-fa-check_circle
		Loading:    "", // nf-fa-spinner
		Refresh:    "", // nf-fa-refresh
		Search:     "", // nf-fa-search
		Filter:     "", // nf-fa-filter
		Sort:       "", // nf-fa-sort
		Expand:     "", // nf-fa-chevron_down
		Collapse:   "", // nf-fa-chevron_up
		Copy:       "", // nf-fa-copy
		Edit:       "", // nf-fa-edit
		Delete:     "", // nf-fa-trash
		Add:        "", // nf-fa-plus
		Check:      "", // nf-fa-check
		Cross:      "", // nf-fa-times
		Arrow:      "", // nf-fa-arrow_right
		ArrowRight: "", // nf-fa-arrow_right
		ArrowDown:  "", // nf-fa-arrow_down
		ArrowUp:    "", // nf-fa-arrow_up
		Dot:        "", // nf-fa-circle (small)
		Circle:     "", // nf-fa-circle
		Square:     "", // nf-fa-square
		Diamond:    "", // nf-fa-diamond
		Star:       "", // nf-fa-star
		Lock:       "", // nf-fa-lock
		Unlock:     "", // nf-fa-unlock
		Key:        "", // nf-fa-key
		User:       "", // nf-fa-user
		Users:      "", // nf-fa-users
		Terminal:   "", // nf-fa-terminal
		File:       "", // nf-fa-file
		Folder:     "", // nf-fa-folder
		Git:        "", // nf-fa-git
		Database:   "", // nf-fa-database
		Cloud:      "", // nf-fa-cloud
	}
}

// ascii returns the ASCII fallback icon set.
func ascii() *IconSet {
	return &IconSet{
		// Status icons
		Approved:  "[OK]",
		Rejected:  "[NO]",
		Pending:   "[..]",
		Executing: "[>>]",
		Failed:    "[!!]",
		Timeout:   "[TO]",
		Cancelled: "[--]",
		Escalated: "[!!]",

		// Tier icons
		Critical:  "[!!]",
		Dangerous: "[! ]",
		Caution:   "[? ]",
		Safe:      "[  ]",

		// UI icons
		Agent:      "[@]",
		Daemon:     "[D]",
		Session:    "[S]",
		Command:    ">",
		Time:       "[T]",
		Warning:    "[!]",
		Info:       "[i]",
		Error:      "[X]",
		Success:    "[v]",
		Loading:    "[*]",
		Refresh:    "[R]",
		Search:     "[?]",
		Filter:     "[F]",
		Sort:       "[^]",
		Expand:     "[v]",
		Collapse:   "[^]",
		Copy:       "[C]",
		Edit:       "[E]",
		Delete:     "[X]",
		Add:        "[+]",
		Check:      "[v]",
		Cross:      "[x]",
		Arrow:      "->",
		ArrowRight: "->",
		ArrowDown:  "v",
		ArrowUp:    "^",
		Dot:        "*",
		Circle:     "o",
		Square:     "#",
		Diamond:    "<>",
		Star:       "*",
		Lock:       "[L]",
		Unlock:     "[U]",
		Key:        "[K]",
		User:       "[@]",
		Users:      "[@@]",
		Terminal:   ">_",
		File:       "[=]",
		Folder:     "[/]",
		Git:        "[G]",
		Database:   "[DB]",
		Cloud:      "[C]",
	}
}

// Current returns the current icon set based on configuration.
func Current() *IconSet {
	if useNerdFonts {
		return nerd()
	}
	return ascii()
}

// Get returns a specific icon from the current set.
func Get(name string) string {
	icons := Current()
	switch name {
	case "approved":
		return icons.Approved
	case "rejected":
		return icons.Rejected
	case "pending":
		return icons.Pending
	case "executing":
		return icons.Executing
	case "failed":
		return icons.Failed
	case "timeout":
		return icons.Timeout
	case "cancelled":
		return icons.Cancelled
	case "escalated":
		return icons.Escalated
	case "critical":
		return icons.Critical
	case "dangerous":
		return icons.Dangerous
	case "caution":
		return icons.Caution
	case "safe":
		return icons.Safe
	case "agent":
		return icons.Agent
	case "daemon":
		return icons.Daemon
	case "session":
		return icons.Session
	case "command":
		return icons.Command
	case "warning":
		return icons.Warning
	case "info":
		return icons.Info
	case "error":
		return icons.Error
	case "success":
		return icons.Success
	default:
		return "?"
	}
}

// StatusIcon returns the icon for a status.
func StatusIcon(status string) string {
	icons := Current()
	switch strings.ToLower(status) {
	case "approved":
		return icons.Approved
	case "rejected":
		return icons.Rejected
	case "pending":
		return icons.Pending
	case "executing":
		return icons.Executing
	case "failed":
		return icons.Failed
	case "timeout":
		return icons.Timeout
	case "cancelled":
		return icons.Cancelled
	case "escalated":
		return icons.Escalated
	case "executed":
		return icons.Success
	default:
		return icons.Dot
	}
}

// TierIcon returns the icon for a risk tier.
func TierIcon(tier string) string {
	icons := Current()
	switch strings.ToLower(tier) {
	case "critical":
		return icons.Critical
	case "dangerous":
		return icons.Dangerous
	case "caution":
		return icons.Caution
	case "safe":
		return icons.Safe
	default:
		return icons.Dot
	}
}
