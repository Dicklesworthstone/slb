package styles

import (
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/tui/theme"
)

func TestNew(t *testing.T) {
	s := New()
	if s == nil {
		t.Fatal("New returned nil")
	}

	// Check that styles are initialized
	result := s.Title.Render("Test")
	if result == "" {
		t.Error("Title style should render")
	}
}

func TestFromTheme(t *testing.T) {
	themes := []*theme.Theme{
		theme.Mocha(),
		theme.Macchiato(),
		theme.Frappe(),
		theme.Latte(),
	}

	for _, th := range themes {
		t.Run(th.Name, func(t *testing.T) {
			s := FromTheme(th)
			if s == nil {
				t.Fatal("FromTheme returned nil")
			}

			// Verify basic styles work
			_ = s.Normal.Render("test")
			_ = s.Bold.Render("test")
			_ = s.Panel.Render("test")
		})
	}
}

func TestStatusBadge(t *testing.T) {
	s := New()

	statuses := []string{
		"pending", "PENDING",
		"approved", "APPROVED",
		"rejected", "REJECTED",
		"executed", "EXECUTED",
		"failed", "FAILED",
		"timeout", "TIMEOUT",
		"cancelled", "CANCELLED",
		"escalated", "ESCALATED",
		"unknown", // Should return dimmed
	}

	for _, status := range statuses {
		t.Run(status, func(t *testing.T) {
			style := s.StatusBadge(status)
			result := style.Render(status)
			if result == "" {
				t.Errorf("StatusBadge(%q) should render", status)
			}
		})
	}
}

func TestTierBadge(t *testing.T) {
	s := New()

	tiers := []string{
		"critical", "CRITICAL",
		"dangerous", "DANGEROUS",
		"caution", "CAUTION",
		"safe", "SAFE",
		"unknown", // Should return dimmed
	}

	for _, tier := range tiers {
		t.Run(tier, func(t *testing.T) {
			style := s.TierBadge(tier)
			result := style.Render(tier)
			if result == "" {
				t.Errorf("TierBadge(%q) should render", tier)
			}
		})
	}
}

func TestRenderStatusBadge(t *testing.T) {
	s := New()

	result := s.RenderStatusBadge("pending")
	if result == "" {
		t.Error("RenderStatusBadge returned empty string")
	}
	if !strings.Contains(result, "pending") {
		t.Error("RenderStatusBadge should contain status text")
	}
}

func TestRenderTierBadge(t *testing.T) {
	s := New()

	result := s.RenderTierBadge("critical")
	if result == "" {
		t.Error("RenderTierBadge returned empty string")
	}
	if !strings.Contains(result, "critical") {
		t.Error("RenderTierBadge should contain tier text")
	}
}

func TestStylesFields(t *testing.T) {
	s := New()

	// Test title styles
	_ = s.Title.Render("title")
	_ = s.Subtitle.Render("subtitle")
	_ = s.SectionHead.Render("section")

	// Test text styles
	_ = s.Normal.Render("normal")
	_ = s.Dimmed.Render("dimmed")
	_ = s.Bold.Render("bold")
	_ = s.Highlight.Render("highlight")

	// Test container styles
	_ = s.Panel.Render("panel")
	_ = s.CommandBox.Render("cmd")
	_ = s.Card.Render("card")
	_ = s.Selected.Render("selected")

	// Test layout helpers
	_ = s.Border.Render("border")
	_ = s.NoBorder.Render("noborder")
	_ = s.Padded.Render("padded")
	_ = s.Centered.Render("centered")
}

// ============== Gradient Tests ==============

func TestNewGradient(t *testing.T) {
	th := theme.Current
	g := NewGradient(th.Red, th.Yellow, th.Green)

	if len(g.Colors) != 3 {
		t.Errorf("expected 3 colors, got %d", len(g.Colors))
	}
}

func TestMauveBlueGradient(t *testing.T) {
	g := MauveBlueGradient()
	if g == nil {
		t.Fatal("MauveBlueGradient returned nil")
	}
	if len(g.Colors) != 3 {
		t.Errorf("expected 3 colors, got %d", len(g.Colors))
	}
}

func TestRainbowGradient(t *testing.T) {
	g := RainbowGradient()
	if g == nil {
		t.Fatal("RainbowGradient returned nil")
	}
	if len(g.Colors) != 7 {
		t.Errorf("expected 7 colors, got %d", len(g.Colors))
	}
}

func TestTierGradient(t *testing.T) {
	g := TierGradient()
	if g == nil {
		t.Fatal("TierGradient returned nil")
	}
	if len(g.Colors) != 4 {
		t.Errorf("expected 4 colors, got %d", len(g.Colors))
	}
}

func TestGradientRender(t *testing.T) {
	th := theme.Current
	g := NewGradient(th.Red, th.Green)

	// Test normal render
	result := g.Render("Hello World")
	if result == "" {
		t.Error("Render returned empty string")
	}

	// Test empty string
	empty := g.Render("")
	if empty != "" {
		t.Error("Render of empty string should return empty")
	}
}

func TestGradientRenderEmpty(t *testing.T) {
	g := NewGradient() // No colors

	result := g.Render("test")
	if result != "test" {
		t.Error("Render with no colors should return original string")
	}
}

func TestGradientRenderSingleColor(t *testing.T) {
	th := theme.Current
	g := NewGradient(th.Red)

	result := g.Render("test")
	if result == "" {
		t.Error("Single color render should work")
	}
}

func TestGradientRenderInterpolated(t *testing.T) {
	th := theme.Current
	g := NewGradient(th.Red, th.Yellow, th.Green)

	result := g.RenderInterpolated("Hello World")
	if result == "" {
		t.Error("RenderInterpolated returned empty string")
	}

	// Test with fewer than 2 colors - should fallback
	g2 := NewGradient(th.Red)
	result2 := g2.RenderInterpolated("test")
	if result2 == "" {
		t.Error("RenderInterpolated with single color should work")
	}
}

func TestGradientTitle(t *testing.T) {
	result := GradientTitle("SLB Dashboard")
	if result == "" {
		t.Error("GradientTitle returned empty string")
	}
}

func TestMax(t *testing.T) {
	tests := []struct {
		a, b, expected int
	}{
		{1, 2, 2},
		{2, 1, 2},
		{0, 0, 0},
		{-1, 1, 1},
		{-1, -2, -1},
	}

	for _, tc := range tests {
		got := max(tc.a, tc.b)
		if got != tc.expected {
			t.Errorf("max(%d, %d): expected %d, got %d", tc.a, tc.b, tc.expected, got)
		}
	}
}

// ============== Shimmer Tests ==============

func TestNewShimmerState(t *testing.T) {
	s := NewShimmerState(20)

	if s.Width != 20 {
		t.Errorf("expected width 20, got %d", s.Width)
	}
	if s.Position != 0 {
		t.Errorf("expected position 0, got %d", s.Position)
	}
	if !s.Forward {
		t.Error("expected Forward to be true")
	}
}

func TestShimmerAdvance(t *testing.T) {
	s := NewShimmerState(5)

	// Advance forward
	for i := 0; i < 4; i++ {
		completed := s.Advance()
		if completed && i < 4 {
			t.Error("should not complete before reaching width")
		}
	}

	// Should complete at width
	completed := s.Advance()
	if !completed {
		t.Error("should complete when reaching width")
	}

	// Now going backward
	if s.Forward {
		t.Error("should be going backward after reaching width")
	}

	// Advance backward
	for s.Position > 0 {
		s.Advance()
	}

	// Should be forward again
	if !s.Forward {
		t.Error("should be forward after reaching 0")
	}
}

func TestShimmerReset(t *testing.T) {
	s := NewShimmerState(10)
	s.Position = 5
	s.Forward = false

	s.Reset()

	if s.Position != 0 {
		t.Errorf("expected position 0 after reset, got %d", s.Position)
	}
	if !s.Forward {
		t.Error("expected Forward to be true after reset")
	}
}

func TestShimmerRenderShimmer(t *testing.T) {
	th := theme.Current
	s := NewShimmerState(20)

	result := s.RenderShimmer("Hello World", th.Mauve)
	if result == "" {
		t.Error("RenderShimmer returned empty string")
	}

	// Test with empty text
	empty := s.RenderShimmer("", th.Mauve)
	if empty != "" {
		t.Error("RenderShimmer of empty string should return empty")
	}

	// Test with text shorter than width
	short := s.RenderShimmer("Hi", th.Mauve)
	if short == "" {
		t.Error("RenderShimmer of short text should work")
	}
}

func TestShimmerRenderAtDifferentPositions(t *testing.T) {
	th := theme.Current
	s := NewShimmerState(20)

	text := "Hello World Test"

	// Render at different positions
	for i := 0; i < 10; i++ {
		result := s.RenderShimmer(text, th.Mauve)
		if result == "" {
			t.Errorf("RenderShimmer at position %d returned empty", s.Position)
		}
		s.Advance()
	}
}

func TestGlowStyle(t *testing.T) {
	th := theme.Current
	style := GlowStyle(th.Red)

	result := style.Render("Test")
	if result == "" {
		t.Error("GlowStyle should render")
	}
}

func TestFocusGlow(t *testing.T) {
	style := FocusGlow()
	result := style.Render("Focus")
	if result == "" {
		t.Error("FocusGlow should render")
	}
}

func TestSuccessGlow(t *testing.T) {
	style := SuccessGlow()
	result := style.Render("Success")
	if result == "" {
		t.Error("SuccessGlow should render")
	}
}

func TestWarningGlow(t *testing.T) {
	style := WarningGlow()
	result := style.Render("Warning")
	if result == "" {
		t.Error("WarningGlow should render")
	}
}

func TestErrorGlow(t *testing.T) {
	style := ErrorGlow()
	result := style.Render("Error")
	if result == "" {
		t.Error("ErrorGlow should render")
	}
}

func TestAbs(t *testing.T) {
	tests := []struct {
		n, expected int
	}{
		{5, 5},
		{-5, 5},
		{0, 0},
		{-1, 1},
	}

	for _, tc := range tests {
		got := abs(tc.n)
		if got != tc.expected {
			t.Errorf("abs(%d): expected %d, got %d", tc.n, tc.expected, got)
		}
	}
}
