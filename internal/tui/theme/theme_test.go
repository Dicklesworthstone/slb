package theme

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestMocha(t *testing.T) {
	th := Mocha()

	if th.Name != "Catppuccin Mocha" {
		t.Errorf("expected name 'Catppuccin Mocha', got %q", th.Name)
	}
	if !th.IsDark {
		t.Error("Mocha should be a dark theme")
	}

	// Verify all primary colors are set
	if th.Mauve == "" {
		t.Error("Mauve color not set")
	}
	if th.Blue == "" {
		t.Error("Blue color not set")
	}
	if th.Green == "" {
		t.Error("Green color not set")
	}
	if th.Yellow == "" {
		t.Error("Yellow color not set")
	}
	if th.Red == "" {
		t.Error("Red color not set")
	}
	if th.Peach == "" {
		t.Error("Peach color not set")
	}
	if th.Teal == "" {
		t.Error("Teal color not set")
	}
	if th.Pink == "" {
		t.Error("Pink color not set")
	}
	if th.Flamingo == "" {
		t.Error("Flamingo color not set")
	}

	// Verify text colors
	if th.Text == "" {
		t.Error("Text color not set")
	}
	if th.Subtext == "" {
		t.Error("Subtext color not set")
	}

	// Verify surface colors
	if th.Surface == "" {
		t.Error("Surface color not set")
	}
	if th.Base == "" {
		t.Error("Base color not set")
	}
	if th.Mantle == "" {
		t.Error("Mantle color not set")
	}
	if th.Crust == "" {
		t.Error("Crust color not set")
	}
}

func TestMacchiato(t *testing.T) {
	th := Macchiato()

	if th.Name != "Catppuccin Macchiato" {
		t.Errorf("expected name 'Catppuccin Macchiato', got %q", th.Name)
	}
	if !th.IsDark {
		t.Error("Macchiato should be a dark theme")
	}

	// Verify essential colors are set
	if th.Red == "" || th.Green == "" || th.Blue == "" {
		t.Error("Primary colors not set")
	}
}

func TestFrappe(t *testing.T) {
	th := Frappe()

	if th.Name != "Catppuccin Frappe" {
		t.Errorf("expected name 'Catppuccin Frappe', got %q", th.Name)
	}
	if !th.IsDark {
		t.Error("Frappe should be a dark theme")
	}

	// Verify essential colors are set
	if th.Red == "" || th.Green == "" || th.Blue == "" {
		t.Error("Primary colors not set")
	}
}

func TestLatte(t *testing.T) {
	th := Latte()

	if th.Name != "Catppuccin Latte" {
		t.Errorf("expected name 'Catppuccin Latte', got %q", th.Name)
	}
	if th.IsDark {
		t.Error("Latte should be a light theme")
	}

	// Verify essential colors are set
	if th.Red == "" || th.Green == "" || th.Blue == "" {
		t.Error("Primary colors not set")
	}
}

func TestSetTheme(t *testing.T) {
	tests := []struct {
		flavor   FlavorName
		expected string
	}{
		{FlavorMocha, "Catppuccin Mocha"},
		{FlavorMacchiato, "Catppuccin Macchiato"},
		{FlavorFrappe, "Catppuccin Frappe"},
		{FlavorLatte, "Catppuccin Latte"},
		{"unknown", "Catppuccin Mocha"}, // Default
		{"", "Catppuccin Mocha"},        // Empty defaults to Mocha
	}

	for _, tc := range tests {
		t.Run(string(tc.flavor), func(t *testing.T) {
			SetTheme(tc.flavor)
			if Current.Name != tc.expected {
				t.Errorf("SetTheme(%q): expected name %q, got %q", tc.flavor, tc.expected, Current.Name)
			}
		})
	}

	// Reset to default
	SetTheme(FlavorMocha)
}

func TestTierColor(t *testing.T) {
	th := Mocha()

	tests := []struct {
		tier     string
		expected lipgloss.Color
	}{
		{"critical", th.Red},
		{"CRITICAL", th.Red},
		{"dangerous", th.Peach},
		{"DANGEROUS", th.Peach},
		{"caution", th.Yellow},
		{"CAUTION", th.Yellow},
		{"safe", th.Green},
		{"SAFE", th.Green},
		{"unknown", th.Text},
		{"", th.Text},
	}

	for _, tc := range tests {
		t.Run(tc.tier, func(t *testing.T) {
			got := th.TierColor(tc.tier)
			if got != tc.expected {
				t.Errorf("TierColor(%q): expected %v, got %v", tc.tier, tc.expected, got)
			}
		})
	}
}

func TestStatusColor(t *testing.T) {
	th := Mocha()

	tests := []struct {
		status   string
		expected lipgloss.Color
	}{
		{"pending", th.Blue},
		{"PENDING", th.Blue},
		{"approved", th.Green},
		{"APPROVED", th.Green},
		{"rejected", th.Red},
		{"REJECTED", th.Red},
		{"executed", th.Green},
		{"EXECUTED", th.Green},
		{"failed", th.Red},
		{"FAILED", th.Red},
		{"timeout", th.Yellow},
		{"TIMEOUT", th.Yellow},
		{"cancelled", th.Subtext},
		{"CANCELLED", th.Subtext},
		{"escalated", th.Peach},
		{"ESCALATED", th.Peach},
		{"unknown", th.Text},
		{"", th.Text},
	}

	for _, tc := range tests {
		t.Run(tc.status, func(t *testing.T) {
			got := th.StatusColor(tc.status)
			if got != tc.expected {
				t.Errorf("StatusColor(%q): expected %v, got %v", tc.status, tc.expected, got)
			}
		})
	}
}

func TestTierEmoji(t *testing.T) {
	tests := []struct {
		tier     string
		expected string
	}{
		{"critical", "🔴"},
		{"CRITICAL", "🔴"},
		{"dangerous", "🟠"},
		{"DANGEROUS", "🟠"},
		{"caution", "🟡"},
		{"CAUTION", "🟡"},
		{"safe", "🟢"},
		{"SAFE", "🟢"},
		{"unknown", "⚪"},
		{"", "⚪"},
	}

	for _, tc := range tests {
		t.Run(tc.tier, func(t *testing.T) {
			got := TierEmoji(tc.tier)
			if got != tc.expected {
				t.Errorf("TierEmoji(%q): expected %q, got %q", tc.tier, tc.expected, got)
			}
		})
	}
}

func TestStatusIcon(t *testing.T) {
	tests := []struct {
		status   string
		expected string
	}{
		{"pending", "⏳"},
		{"PENDING", "⏳"},
		{"approved", "✓"},
		{"APPROVED", "✓"},
		{"rejected", "✗"},
		{"REJECTED", "✗"},
		{"executed", "✓"},
		{"EXECUTED", "✓"},
		{"failed", "✗"},
		{"FAILED", "✗"},
		{"timeout", "⏰"},
		{"TIMEOUT", "⏰"},
		{"cancelled", "⊘"},
		{"CANCELLED", "⊘"},
		{"escalated", "⚠"},
		{"ESCALATED", "⚠"},
		{"unknown", "?"},
		{"", "?"},
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

func TestFlavorNameConstants(t *testing.T) {
	// Verify flavor constants are correctly defined
	if FlavorMocha != "mocha" {
		t.Errorf("FlavorMocha: expected 'mocha', got %q", FlavorMocha)
	}
	if FlavorMacchiato != "macchiato" {
		t.Errorf("FlavorMacchiato: expected 'macchiato', got %q", FlavorMacchiato)
	}
	if FlavorFrappe != "frappe" {
		t.Errorf("FlavorFrappe: expected 'frappe', got %q", FlavorFrappe)
	}
	if FlavorLatte != "latte" {
		t.Errorf("FlavorLatte: expected 'latte', got %q", FlavorLatte)
	}
}

func TestCurrentDefault(t *testing.T) {
	// Reset to ensure we test default state
	SetTheme(FlavorMocha)

	if Current == nil {
		t.Fatal("Current theme should not be nil")
	}
	if Current.Name != "Catppuccin Mocha" {
		t.Errorf("Default theme should be Mocha, got %q", Current.Name)
	}
}

func TestThemeColorConsistency(t *testing.T) {
	themes := []*Theme{
		Mocha(),
		Macchiato(),
		Frappe(),
		Latte(),
	}

	for _, th := range themes {
		t.Run(th.Name, func(t *testing.T) {
			// All themes should have distinct tier colors
			if th.Red == th.Yellow || th.Yellow == th.Peach || th.Red == th.Peach {
				t.Error("Tier colors should be distinct")
			}

			// Surface0 and Surface should match (per current implementation)
			if th.Surface != th.Surface0 {
				t.Error("Surface and Surface0 should match")
			}

			// Overlay colors should be in sequence (increasingly lighter)
			// This is a basic sanity check - actual values depend on theme
			if th.Overlay0 == "" || th.Overlay1 == "" || th.Overlay2 == "" {
				t.Error("All overlay colors should be set")
			}
		})
	}
}
