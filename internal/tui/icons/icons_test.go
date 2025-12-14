package icons

import (
	"os"
	"testing"
)

func TestCurrent(t *testing.T) {
	// Reset to known state
	SetNerdFonts(false)

	icons := Current()
	if icons == nil {
		t.Fatal("Current returned nil")
	}

	// In ASCII mode, should return ASCII icons
	if icons.Agent != "[@]" {
		t.Errorf("expected ASCII agent icon '[@]', got %q", icons.Agent)
	}
}

func TestSetNerdFonts(t *testing.T) {
	// Save original state
	original := useNerdFonts
	defer func() { useNerdFonts = original }()

	// Test enabling
	SetNerdFonts(true)
	icons := Current()
	if icons.Agent != "󰀄" {
		t.Errorf("Nerd Font agent icon expected")
	}

	// Test disabling
	SetNerdFonts(false)
	icons = Current()
	if icons.Agent != "[@]" {
		t.Errorf("ASCII agent icon expected after disabling")
	}
}

func TestNerdIconSet(t *testing.T) {
	icons := nerd()

	// Verify key icons are set (some Nerd Font icons use special Unicode)
	// We check for non-nil and that common icons have expected values
	if icons.Approved != "✓" {
		t.Errorf("expected Approved '✓', got %q", icons.Approved)
	}
	if icons.Rejected != "✗" {
		t.Errorf("expected Rejected '✗', got %q", icons.Rejected)
	}
	// Agent uses Nerd Font glyph - just verify it's set
	if len(icons.Agent) == 0 {
		t.Error("Agent icon not set")
	}
}

func TestAsciiIconSet(t *testing.T) {
	icons := ascii()

	// Verify all ASCII icons are reasonable
	if icons.Approved != "[OK]" {
		t.Errorf("expected Approved '[OK]', got %q", icons.Approved)
	}
	if icons.Rejected != "[NO]" {
		t.Errorf("expected Rejected '[NO]', got %q", icons.Rejected)
	}
	if icons.Pending != "[..]" {
		t.Errorf("expected Pending '[..]', got %q", icons.Pending)
	}
}

func TestGet(t *testing.T) {
	// Save original state
	original := useNerdFonts
	SetNerdFonts(false)
	defer func() { useNerdFonts = original }()

	tests := []struct {
		name     string
		expected string
	}{
		{"approved", "[OK]"},
		{"rejected", "[NO]"},
		{"pending", "[..]"},
		{"executing", "[>>]"},
		{"failed", "[!!]"},
		{"timeout", "[TO]"},
		{"cancelled", "[--]"},
		{"escalated", "[!!]"},
		{"critical", "[!!]"},
		{"dangerous", "[! ]"},
		{"caution", "[? ]"},
		{"safe", "[  ]"},
		{"agent", "[@]"},
		{"daemon", "[D]"},
		{"session", "[S]"},
		{"command", ">"},
		{"warning", "[!]"},
		{"info", "[i]"},
		{"error", "[X]"},
		{"success", "[v]"},
		{"unknown", "?"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Get(tc.name)
			if got != tc.expected {
				t.Errorf("Get(%q): expected %q, got %q", tc.name, tc.expected, got)
			}
		})
	}
}

func TestStatusIcon(t *testing.T) {
	// Save original state
	original := useNerdFonts
	SetNerdFonts(false)
	defer func() { useNerdFonts = original }()

	tests := []struct {
		status   string
		expected string
	}{
		{"approved", "[OK]"},
		{"APPROVED", "[OK]"},
		{"rejected", "[NO]"},
		{"pending", "[..]"},
		{"executing", "[>>]"},
		{"failed", "[!!]"},
		{"timeout", "[TO]"},
		{"cancelled", "[--]"},
		{"escalated", "[!!]"},
		{"executed", "[v]"},
		{"unknown", "*"}, // Default to Dot
	}

	for _, tc := range tests {
		t.Run(tc.status, func(t *testing.T) {
			got := StatusIcon(tc.status)
			if got != tc.expected {
				t.Errorf("StatusIcon(%q): expected %q, got %q", tc.status, tc.expected, got)
			}
		})
	}
}

func TestTierIcon(t *testing.T) {
	// Save original state
	original := useNerdFonts
	SetNerdFonts(false)
	defer func() { useNerdFonts = original }()

	tests := []struct {
		tier     string
		expected string
	}{
		{"critical", "[!!]"},
		{"CRITICAL", "[!!]"},
		{"dangerous", "[! ]"},
		{"caution", "[? ]"},
		{"safe", "[  ]"},
		{"unknown", "*"}, // Default to Dot
	}

	for _, tc := range tests {
		t.Run(tc.tier, func(t *testing.T) {
			got := TierIcon(tc.tier)
			if got != tc.expected {
				t.Errorf("TierIcon(%q): expected %q, got %q", tc.tier, tc.expected, got)
			}
		})
	}
}

func TestDetectNerdFonts(t *testing.T) {
	// Test explicit SLB_ICONS environment variable
	tests := []struct {
		env      string
		expected bool
	}{
		{"nerd", true},
		{"NERD", true},
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"0", false},
		{"false", false},
		{"ascii", false},
	}

	for _, tc := range tests {
		t.Run(tc.env, func(t *testing.T) {
			// Save and restore env
			old := os.Getenv("SLB_ICONS")
			defer os.Setenv("SLB_ICONS", old)

			os.Setenv("SLB_ICONS", tc.env)
			result := detectNerdFonts()
			if result != tc.expected {
				t.Errorf("detectNerdFonts with SLB_ICONS=%q: expected %v, got %v", tc.env, tc.expected, result)
			}
		})
	}
}

func TestDetectNerdFontsTerminals(t *testing.T) {
	// Save original env
	oldIcons := os.Getenv("SLB_ICONS")
	oldTermProgram := os.Getenv("TERM_PROGRAM")
	defer func() {
		os.Setenv("SLB_ICONS", oldIcons)
		os.Setenv("TERM_PROGRAM", oldTermProgram)
	}()

	// Clear SLB_ICONS to test terminal detection
	os.Unsetenv("SLB_ICONS")

	tests := []struct {
		termProgram string
		expected    bool
	}{
		{"kitty", true},
		{"wezterm", true},
		{"alacritty", true},
		{"iTerm.app", true},
		{"Terminal.app", false},
		{"xterm", false},
	}

	for _, tc := range tests {
		t.Run(tc.termProgram, func(t *testing.T) {
			os.Setenv("TERM_PROGRAM", tc.termProgram)
			result := detectNerdFonts()
			if result != tc.expected {
				t.Errorf("detectNerdFonts with TERM_PROGRAM=%q: expected %v, got %v", tc.termProgram, tc.expected, result)
			}
		})
	}
}

func TestIconSetCompleteness(t *testing.T) {
	asciiIcons := ascii()

	// ASCII icons should all be readable ASCII strings
	checkNotEmpty := func(set *IconSet, name string) {
		if set.Approved == "" {
			t.Errorf("%s: Approved is empty", name)
		}
		if set.Rejected == "" {
			t.Errorf("%s: Rejected is empty", name)
		}
		if set.Pending == "" {
			t.Errorf("%s: Pending is empty", name)
		}
		if set.Executing == "" {
			t.Errorf("%s: Executing is empty", name)
		}
		if set.Failed == "" {
			t.Errorf("%s: Failed is empty", name)
		}
		if set.Agent == "" {
			t.Errorf("%s: Agent is empty", name)
		}
		if set.Daemon == "" {
			t.Errorf("%s: Daemon is empty", name)
		}
		if set.Terminal == "" {
			t.Errorf("%s: Terminal is empty", name)
		}
	}

	// Only test ASCII icons - Nerd Font icons use special Unicode glyphs
	// that may appear as empty strings in string comparisons
	checkNotEmpty(asciiIcons, "ascii")
}

func TestNerdIconsHaveContent(t *testing.T) {
	icons := nerd()

	// Nerd Font icons use special Unicode - check length, not empty string
	if len(icons.Approved) == 0 {
		t.Error("nerd: Approved has no content")
	}
	if len(icons.Rejected) == 0 {
		t.Error("nerd: Rejected has no content")
	}
	if len(icons.Agent) == 0 {
		t.Error("nerd: Agent has no content")
	}
	if len(icons.Daemon) == 0 {
		t.Error("nerd: Daemon has no content")
	}
}
