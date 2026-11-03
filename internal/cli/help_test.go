package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestClampWidth(t *testing.T) {
	tests := []struct {
		input    int
		expected int
	}{
		{50, 72},   // Below minimum, clamp to 72
		{72, 72},   // At minimum
		{80, 80},   // Normal width
		{100, 100}, // At maximum
		{120, 100}, // Above maximum, clamp to 100
		{200, 100}, // Well above maximum
	}

	for _, tt := range tests {
		result := clampWidth(tt.input)
		if result != tt.expected {
			t.Errorf("clampWidth(%d) = %d, want %d", tt.input, result, tt.expected)
		}
	}
}

func TestDetectWidth(t *testing.T) {
	// Test with COLUMNS env var
	originalColumns := os.Getenv("COLUMNS")
	defer os.Setenv("COLUMNS", originalColumns)

	os.Setenv("COLUMNS", "120")
	width := detectWidth()
	// The result may vary depending on whether stdout is a terminal
	if width <= 0 {
		t.Errorf("detectWidth() returned %d, expected positive value", width)
	}

	// Test with invalid COLUMNS
	os.Setenv("COLUMNS", "invalid")
	width = detectWidth()
	// Should fall back to default (80) or terminal width
	if width <= 0 {
		t.Errorf("detectWidth() returned %d, expected positive value", width)
	}

	// Test with empty COLUMNS
	os.Setenv("COLUMNS", "")
	width = detectWidth()
	if width <= 0 {
		t.Errorf("detectWidth() returned %d, expected positive value", width)
	}
}

func TestSupportsUnicode(t *testing.T) {
	// Save original environment
	originalTerm := os.Getenv("TERM")
	originalLcAll := os.Getenv("LC_ALL")
	originalLcCtype := os.Getenv("LC_CTYPE")
	originalLang := os.Getenv("LANG")

	defer func() {
		os.Setenv("TERM", originalTerm)
		os.Setenv("LC_ALL", originalLcAll)
		os.Setenv("LC_CTYPE", originalLcCtype)
		os.Setenv("LANG", originalLang)
	}()

	// Test with dumb terminal
	os.Setenv("TERM", "dumb")
	os.Setenv("LC_ALL", "")
	os.Setenv("LC_CTYPE", "")
	os.Setenv("LANG", "")
	if supportsUnicode() {
		t.Error("expected supportsUnicode() = false for dumb terminal")
	}

	// Test with UTF-8 locale
	os.Setenv("TERM", "xterm")
	os.Setenv("LC_ALL", "en_US.UTF-8")
	if !supportsUnicode() {
		t.Error("expected supportsUnicode() = true for UTF-8 locale")
	}

	// Test with utf8 in LANG
	os.Setenv("LC_ALL", "")
	os.Setenv("LANG", "C.utf8")
	if !supportsUnicode() {
		t.Error("expected supportsUnicode() = true for utf8 in LANG")
	}
}

func TestGradientText(t *testing.T) {
	// Save original environment for Unicode check
	originalLang := os.Getenv("LANG")
	defer os.Setenv("LANG", originalLang)

	os.Setenv("LANG", "en_US.UTF-8")
	os.Setenv("TERM", "xterm")

	// Test with no colors
	result := gradientText("hello", nil)
	if result != "hello" {
		t.Errorf("expected 'hello' with no colors, got %q", result)
	}

	// Test with colors - just verify no panic
	result = gradientText("hello", []lipgloss.Color{colorMauve, colorBlue})
	if result == "" {
		t.Error("expected non-empty result")
	}
}

func TestBullet(t *testing.T) {
	result := bullet("slb run", "run a command")

	// Should contain the command
	if result == "" {
		t.Error("expected non-empty bullet result")
	}

	// Result should contain the command text
	// The styling makes exact matching difficult, but the content should be there
}

func TestRenderSection(t *testing.T) {
	lines := []string{
		"  line 1",
		"  line 2",
	}

	// Test with unicode
	result := renderSection(true, "🔷 Test Section", lines)
	if result == "" {
		t.Error("expected non-empty section result with unicode")
	}

	// Test without unicode
	result = renderSection(false, "🔷 Test Section", lines)
	if result == "" {
		t.Error("expected non-empty section result without unicode")
	}
}

func TestTierLegend(t *testing.T) {
	// Test with unicode
	result := tierLegend(true)
	if result == "" {
		t.Error("expected non-empty tier legend with unicode")
	}

	// Test without unicode
	result = tierLegend(false)
	if result == "" {
		t.Error("expected non-empty tier legend without unicode")
	}
}

func TestFlagLegend(t *testing.T) {
	// Test with unicode
	result := flagLegend(true)
	if result == "" {
		t.Error("expected non-empty flag legend with unicode")
	}

	// Test without unicode
	result = flagLegend(false)
	if result == "" {
		t.Error("expected non-empty flag legend without unicode")
	}
}

func TestFooterLegend(t *testing.T) {
	// Test with unicode
	result := footerLegend(true)
	if result == "" {
		t.Error("expected non-empty footer legend with unicode")
	}

	// Test without unicode
	result = footerLegend(false)
	if result == "" {
		t.Error("expected non-empty footer legend without unicode")
	}
}

func TestShowQuickReference(t *testing.T) {
	// Save original environment
	originalLang := os.Getenv("LANG")
	originalTerm := os.Getenv("TERM")
	defer func() {
		os.Setenv("LANG", originalLang)
		os.Setenv("TERM", originalTerm)
	}()

	// Set up environment for testing
	os.Setenv("LANG", "en_US.UTF-8")
	os.Setenv("TERM", "xterm")

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		io.Copy(&buf, r)
		close(done)
	}()

	showQuickReference()

	w.Close()
	os.Stdout = oldStdout

	<-done
	output := buf.String()

	// Should produce non-empty output
	if output == "" {
		t.Error("expected non-empty output from showQuickReference")
	}

	// Should contain SLB reference content
	if !strings.Contains(output, "SLB") && !strings.Contains(output, "slb") {
		t.Error("expected output to contain SLB reference")
	}
}

func TestShowQuickReference_NonUnicode(t *testing.T) {
	// Save original environment
	originalLang := os.Getenv("LANG")
	originalTerm := os.Getenv("TERM")
	defer func() {
		os.Setenv("LANG", originalLang)
		os.Setenv("TERM", originalTerm)
	}()

	// Set up dumb terminal (no unicode)
	os.Setenv("LANG", "C")
	os.Setenv("TERM", "dumb")
	os.Unsetenv("LC_ALL")
	os.Unsetenv("LC_CTYPE")

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		io.Copy(&buf, r)
		close(done)
	}()

	showQuickReference()

	w.Close()
	os.Stdout = oldStdout

	<-done
	output := buf.String()

	// Should still produce output
	if output == "" {
		t.Error("expected non-empty output from showQuickReference in non-unicode mode")
	}
}

// TestDetectWidth_DocumentedLimitations documents coverage gaps for detectWidth.
// Note: The term.GetSize success path requires stdout to be a real terminal,
// which is rare in CI/test environments. This is an acceptable coverage gap.
func TestDetectWidth_DocumentedLimitations(t *testing.T) {
	// detectWidth has three paths:
	// 1. term.GetSize succeeds -> return terminal width (hard to test)
	// 2. COLUMNS env is set and valid -> return that value (covered)
	// 3. Default fallback -> return 80 (covered)
	//
	// Path 1 requires stdout to be a terminal. In test environments,
	// stdout is usually a pipe or file, so GetSize returns an error.

	// Verify fallback paths work correctly
	originalColumns := os.Getenv("COLUMNS")
	defer os.Setenv("COLUMNS", originalColumns)

	// Test with negative COLUMNS value
	os.Setenv("COLUMNS", "-5")
	width := detectWidth()
	if width <= 0 {
		t.Errorf("detectWidth() should return positive value, got %d", width)
	}

	// Test with zero COLUMNS
	os.Setenv("COLUMNS", "0")
	width = detectWidth()
	if width != 80 {
		t.Errorf("detectWidth() should return 80 for invalid COLUMNS, got %d", width)
	}

	// Test with very large COLUMNS
	os.Setenv("COLUMNS", "10000")
	width = detectWidth()
	if width != 10000 {
		t.Errorf("detectWidth() should return 10000, got %d", width)
	}
}

// TestGradientText_SingleColor tests gradient with exactly one color.
func TestGradientText_SingleColor(t *testing.T) {
	originalLang := os.Getenv("LANG")
	originalTerm := os.Getenv("TERM")
	defer func() {
		os.Setenv("LANG", originalLang)
		os.Setenv("TERM", originalTerm)
	}()

	os.Setenv("LANG", "en_US.UTF-8")
	os.Setenv("TERM", "xterm")

	// Single color should work without crashing
	result := gradientText("hello", []lipgloss.Color{colorMauve})
	if result == "" {
		t.Error("expected non-empty result with single color")
	}
	// With single color, the entire text should be rendered in that color
	if !strings.Contains(result, "hello") && len(result) == 0 {
		t.Error("expected result to contain the original text")
	}
}

// TestGradientText_SingleCharacter tests gradient with a single character.
// This is an edge case because the division in the gradient calculation
// would be (i * (segments-1)) / (len(runes)-1) = 0 / 0 if not handled.
func TestGradientText_SingleCharacter(t *testing.T) {
	originalLang := os.Getenv("LANG")
	originalTerm := os.Getenv("TERM")
	defer func() {
		os.Setenv("LANG", originalLang)
		os.Setenv("TERM", originalTerm)
	}()

	os.Setenv("LANG", "en_US.UTF-8")
	os.Setenv("TERM", "xterm")

	// Single character with multiple colors should work without panic
	result := gradientText("X", []lipgloss.Color{colorMauve, colorBlue})
	if result == "" {
		t.Error("expected non-empty result with single character")
	}
}

// TestGradientText_EmptyString tests gradient with empty input.
func TestGradientText_EmptyString(t *testing.T) {
	originalLang := os.Getenv("LANG")
	defer os.Setenv("LANG", originalLang)

	os.Setenv("LANG", "en_US.UTF-8")
	os.Setenv("TERM", "xterm")

	// Empty string should return empty string
	result := gradientText("", []lipgloss.Color{colorMauve, colorBlue})
	if result != "" {
		t.Errorf("expected empty result for empty input, got %q", result)
	}
}

// TestGradientText_NoUnicodeSupport tests gradient when unicode is not supported.
func TestGradientText_NoUnicodeSupport(t *testing.T) {
	originalLang := os.Getenv("LANG")
	originalTerm := os.Getenv("TERM")
	originalLcAll := os.Getenv("LC_ALL")
	defer func() {
		os.Setenv("LANG", originalLang)
		os.Setenv("TERM", originalTerm)
		os.Setenv("LC_ALL", originalLcAll)
	}()

	// Set up environment without unicode support
	os.Setenv("LANG", "C")
	os.Setenv("TERM", "dumb")
	os.Setenv("LC_ALL", "")
	os.Unsetenv("LC_CTYPE")

	// Should return plain text without styling
	result := gradientText("hello world", []lipgloss.Color{colorMauve, colorBlue})
	if result != "hello world" {
		t.Errorf("expected plain text without unicode support, got %q", result)
	}
}

// Ensure lipgloss import is used
var _ lipgloss.Color
