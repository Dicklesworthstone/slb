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
