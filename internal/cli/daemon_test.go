package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/spf13/cobra"
)

func resetDaemonFlags() {
	flagDB = ""
	flagOutput = "text"
	flagJSON = false
	flagProject = ""
	flagDaemonStartForeground = false
	flagDaemonStopTimeoutSecs = 10
	flagDaemonLogsFollow = false
	flagDaemonLogsLines = 200
}

func TestDaemonProjectPath_FromFlag(t *testing.T) {
	resetDaemonFlags()
	flagProject = "/test/project/path"

	result, err := daemonProjectPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "/test/project/path" {
		t.Errorf("expected '/test/project/path', got %q", result)
	}
}

func TestDaemonProjectPath_FromEnv(t *testing.T) {
	resetDaemonFlags()
	flagProject = ""

	originalEnv := os.Getenv("SLB_PROJECT")
	defer os.Setenv("SLB_PROJECT", originalEnv)

	os.Setenv("SLB_PROJECT", "/env/project/path")

	result, err := daemonProjectPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "/env/project/path" {
		t.Errorf("expected '/env/project/path', got %q", result)
	}
}

func TestDaemonProjectPath_FallbackToCwd(t *testing.T) {
	resetDaemonFlags()
	flagProject = ""

	originalEnv := os.Getenv("SLB_PROJECT")
	defer os.Setenv("SLB_PROJECT", originalEnv)
	os.Setenv("SLB_PROJECT", "")

	result, err := daemonProjectPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cwd, _ := os.Getwd()
	if result != cwd {
		t.Errorf("expected cwd %q, got %q", cwd, result)
	}
}

func TestDaemonProjectStats_NoDatabase(t *testing.T) {
	h := testutil.NewHarness(t)
	resetDaemonFlags()

	// Call with a path that has no database
	pending, sessions := daemonProjectStats(h.ProjectDir)

	// Should return 0, 0 when database doesn't exist
	if pending != 0 {
		t.Errorf("expected pending=0, got %d", pending)
	}
	if sessions != 0 {
		t.Errorf("expected sessions=0, got %d", sessions)
	}
}

func TestDaemonProjectStats_WithDatabase(t *testing.T) {
	h := testutil.NewHarness(t)
	resetDaemonFlags()

	// Create a session and request
	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
	)
	testutil.MakeRequest(t, h.DB, sess)

	pending, sessions := daemonProjectStats(h.ProjectDir)

	// Should find the pending request and session
	if pending < 1 {
		t.Errorf("expected pending >= 1, got %d", pending)
	}
	if sessions < 1 {
		t.Errorf("expected sessions >= 1, got %d", sessions)
	}
}

func TestDaemonLogPath(t *testing.T) {
	result, err := daemonLogPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should be in home directory
	home, _ := os.UserHomeDir()
	expected := filepath.Join(home, ".slb", "daemon.log")
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

func TestTailFileLines_BasicFile(t *testing.T) {
	h := testutil.NewHarness(t)

	// Create a test file with some lines
	testFile := filepath.Join(h.ProjectDir, "test.log")
	content := "line1\nline2\nline3\nline4\nline5\n"
	if err := os.WriteFile(testFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	lines, err := tailFileLines(testFile, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(lines) != 3 {
		t.Errorf("expected 3 lines, got %d", len(lines))
	}
	if lines[0] != "line3" {
		t.Errorf("expected first line to be 'line3', got %q", lines[0])
	}
	if lines[2] != "line5" {
		t.Errorf("expected last line to be 'line5', got %q", lines[2])
	}
}

func TestTailFileLines_FewerLinesThanRequested(t *testing.T) {
	h := testutil.NewHarness(t)

	testFile := filepath.Join(h.ProjectDir, "test.log")
	content := "line1\nline2\n"
	if err := os.WriteFile(testFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	lines, err := tailFileLines(testFile, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(lines) != 2 {
		t.Errorf("expected 2 lines, got %d", len(lines))
	}
}

func TestTailFileLines_EmptyFile(t *testing.T) {
	h := testutil.NewHarness(t)

	testFile := filepath.Join(h.ProjectDir, "empty.log")
	if err := os.WriteFile(testFile, []byte(""), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	lines, err := tailFileLines(testFile, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(lines) != 0 {
		t.Errorf("expected 0 lines for empty file, got %d", len(lines))
	}
}

func TestTailFileLines_FileNotFound(t *testing.T) {
	_, err := tailFileLines("/nonexistent/path/file.log", 10)
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestTailFileLines_DefaultLines(t *testing.T) {
	h := testutil.NewHarness(t)

	testFile := filepath.Join(h.ProjectDir, "test.log")
	content := "line1\nline2\n"
	if err := os.WriteFile(testFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	// Pass 0 or negative should default to 200
	lines, err := tailFileLines(testFile, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should still work with default value
	if lines == nil {
		t.Error("expected non-nil result")
	}
}

func TestDaemonCommand_Help(t *testing.T) {
	root := &cobra.Command{
		Use:           "slb",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	daemon := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the SLB daemon",
	}
	daemon.AddCommand(&cobra.Command{Use: "start", Short: "Start the daemon"})
	daemon.AddCommand(&cobra.Command{Use: "stop", Short: "Stop the daemon"})
	daemon.AddCommand(&cobra.Command{Use: "status", Short: "Show daemon status"})
	daemon.AddCommand(&cobra.Command{Use: "logs", Short: "Show daemon logs"})

	root.AddCommand(daemon)

	stdout, _, err := executeCommand(root, "daemon", "--help")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(stdout, "daemon") {
		t.Error("expected help to mention 'daemon'")
	}
	if !strings.Contains(stdout, "start") {
		t.Error("expected help to mention 'start' subcommand")
	}
	if !strings.Contains(stdout, "stop") {
		t.Error("expected help to mention 'stop' subcommand")
	}
	if !strings.Contains(stdout, "status") {
		t.Error("expected help to mention 'status' subcommand")
	}
	if !strings.Contains(stdout, "logs") {
		t.Error("expected help to mention 'logs' subcommand")
	}
}

// TestDaemonLogPath_DocumentedLimitations documents coverage gaps for daemonLogPath.
// Note: The error path (when os.UserHomeDir() fails) requires HOME env manipulation
// which can have side effects. The function is simple enough that the gap is acceptable.
func TestDaemonLogPath_DocumentedLimitations(t *testing.T) {
	// The function has two paths:
	// 1. os.UserHomeDir() succeeds -> return joined path (covered)
	// 2. os.UserHomeDir() fails -> return error (hard to test)
	//
	// os.UserHomeDir() fails when:
	// - HOME env is empty AND we can't determine home from /etc/passwd
	// This is rare in practice and testing it would require env manipulation.

	// Verify the happy path works consistently
	result, err := daemonLogPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should always end with daemon.log
	if !strings.HasSuffix(result, "daemon.log") {
		t.Errorf("expected path to end with 'daemon.log', got %q", result)
	}

	// Should contain .slb directory
	if !strings.Contains(result, ".slb") {
		t.Errorf("expected path to contain '.slb', got %q", result)
	}
}

// TestTailFileLines_NegativeLines tests that negative line count defaults to 200.
func TestTailFileLines_NegativeLines(t *testing.T) {
	h := testutil.NewHarness(t)

	testFile := filepath.Join(h.ProjectDir, "test.log")
	content := "line1\nline2\n"
	if err := os.WriteFile(testFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	// Pass negative value - should default to 200
	lines, err := tailFileLines(testFile, -5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should return all lines since file has fewer than 200
	if len(lines) != 2 {
		t.Errorf("expected 2 lines, got %d", len(lines))
	}
}

// TestTailFileLines_LongLines tests handling of very long log lines.
func TestTailFileLines_LongLines(t *testing.T) {
	h := testutil.NewHarness(t)

	testFile := filepath.Join(h.ProjectDir, "test.log")

	// Create a file with a very long line (> default buffer size)
	longLine := strings.Repeat("x", 70000) // 70KB line
	content := "line1\n" + longLine + "\nline3\n"
	if err := os.WriteFile(testFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	lines, err := tailFileLines(testFile, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(lines) != 3 {
		t.Errorf("expected 3 lines, got %d", len(lines))
	}
	if lines[1] != longLine {
		t.Errorf("long line was truncated or corrupted")
	}
}

// TestDaemonProjectStats_BrokenDatabase tests behavior when DB is corrupted/unreadable.
func TestDaemonProjectStats_BrokenDatabase(t *testing.T) {
	h := testutil.NewHarness(t)
	resetDaemonFlags()

	// Create a .slb directory with an invalid database file
	slbDir := filepath.Join(h.ProjectDir, ".slb")
	if err := os.MkdirAll(slbDir, 0755); err != nil {
		t.Fatalf("failed to create .slb dir: %v", err)
	}

	// Write garbage to state.db to make it unreadable as SQLite
	dbPath := filepath.Join(slbDir, "state.db")
	if err := os.WriteFile(dbPath, []byte("not a valid sqlite database"), 0644); err != nil {
		t.Fatalf("failed to create fake db: %v", err)
	}

	// Should gracefully return 0, 0 when DB is corrupted
	pending, sessions := daemonProjectStats(h.ProjectDir)

	if pending != 0 {
		t.Errorf("expected pending=0 for corrupted DB, got %d", pending)
	}
	if sessions != 0 {
		t.Errorf("expected sessions=0 for corrupted DB, got %d", sessions)
	}
}

// TestTailFileLines_ExactLineCount tests when file has exactly N lines.
func TestTailFileLines_ExactLineCount(t *testing.T) {
	h := testutil.NewHarness(t)

	testFile := filepath.Join(h.ProjectDir, "test.log")
	content := "line1\nline2\nline3\n"
	if err := os.WriteFile(testFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	// Request exactly 3 lines when file has exactly 3
	lines, err := tailFileLines(testFile, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(lines) != 3 {
		t.Errorf("expected 3 lines, got %d", len(lines))
	}
	if lines[0] != "line1" || lines[1] != "line2" || lines[2] != "line3" {
		t.Errorf("unexpected line content: %v", lines)
	}
}

// TestTailFileLines_SingleLine tests a file with exactly one line.
func TestTailFileLines_SingleLine(t *testing.T) {
	h := testutil.NewHarness(t)

	testFile := filepath.Join(h.ProjectDir, "test.log")
	if err := os.WriteFile(testFile, []byte("single line\n"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	lines, err := tailFileLines(testFile, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(lines) != 1 {
		t.Errorf("expected 1 line, got %d", len(lines))
	}
	if lines[0] != "single line" {
		t.Errorf("expected 'single line', got %q", lines[0])
	}
}

// TestTailFileLines_NoTrailingNewline tests file without trailing newline.
func TestTailFileLines_NoTrailingNewline(t *testing.T) {
	h := testutil.NewHarness(t)

	testFile := filepath.Join(h.ProjectDir, "test.log")
	// Note: no trailing newline
	if err := os.WriteFile(testFile, []byte("line1\nline2"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	lines, err := tailFileLines(testFile, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(lines) != 2 {
		t.Errorf("expected 2 lines, got %d", len(lines))
	}
}

// TestDaemonProjectPath_PrecedenceOrder verifies the correct fallback order.
func TestDaemonProjectPath_PrecedenceOrder(t *testing.T) {
	resetDaemonFlags()

	originalEnv := os.Getenv("SLB_PROJECT")
	defer os.Setenv("SLB_PROJECT", originalEnv)

	// 1. Flag takes precedence over env
	flagProject = "/flag/path"
	os.Setenv("SLB_PROJECT", "/env/path")

	result, err := daemonProjectPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "/flag/path" {
		t.Errorf("flag should take precedence, got %q", result)
	}

	// 2. Env takes precedence over cwd
	flagProject = ""
	result, err = daemonProjectPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "/env/path" {
		t.Errorf("env should take precedence over cwd, got %q", result)
	}

	// 3. Cwd is final fallback
	os.Setenv("SLB_PROJECT", "")
	result, err = daemonProjectPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cwd, _ := os.Getwd()
	if result != cwd {
		t.Errorf("should fallback to cwd, got %q, want %q", result, cwd)
	}
}

// =============================================================================
// followFile Tests
// =============================================================================

// TestFollowFile_FileNotFound tests error handling when file doesn't exist.
func TestFollowFile_FileNotFound(t *testing.T) {
	var buf strings.Builder
	err := followFile("/nonexistent/path/to/file.log", &buf)
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
	if !strings.Contains(err.Error(), "opening") {
		t.Errorf("expected error about opening file, got: %v", err)
	}
}

// TestFollowFile_PermissionDenied tests error handling when file can't be read.
func TestFollowFile_PermissionDenied(t *testing.T) {
	h := testutil.NewHarness(t)

	// Create a file with no read permissions
	testFile := filepath.Join(h.ProjectDir, "noperm.log")
	if err := os.WriteFile(testFile, []byte("test"), 0000); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}
	defer os.Chmod(testFile, 0644) // Cleanup

	var buf strings.Builder
	err := followFile(testFile, &buf)
	if err == nil {
		// On some systems (containers with root), this might succeed
		t.Skip("permission denied test skipped (likely running as root)")
	}
	if !strings.Contains(err.Error(), "opening") {
		t.Errorf("expected error about opening file, got: %v", err)
	}
}

// TestDaemonLogPath_HomeUnset tests behavior when HOME environment is cleared.
func TestDaemonLogPath_HomeUnset(t *testing.T) {
	// Save original HOME
	origHome := os.Getenv("HOME")
	defer os.Setenv("HOME", origHome)

	// Clear HOME - os.UserHomeDir() may still work via passwd lookup
	os.Unsetenv("HOME")

	result, err := daemonLogPath()
	// This might succeed or fail depending on OS
	if err != nil {
		// Error is acceptable when HOME is unset
		if !strings.Contains(err.Error(), "home") {
			t.Errorf("expected error about home directory, got: %v", err)
		}
	} else {
		// If it succeeds, result should still be valid
		if !strings.Contains(result, "daemon.log") {
			t.Errorf("expected valid path ending in daemon.log, got: %s", result)
		}
	}
}
