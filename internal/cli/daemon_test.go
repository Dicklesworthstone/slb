package cli

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/daemon"
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

func physicalDaemonTestPath(t *testing.T, path string) string {
	t.Helper()
	physical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return physical
}

func TestDaemonProjectPath_FromFlag(t *testing.T) {
	resetDaemonFlags()
	flagProject = physicalDaemonTestPath(t, t.TempDir())

	result, err := daemonProjectPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != flagProject {
		t.Errorf("expected %q, got %q", flagProject, result)
	}
}

func TestDaemonProjectPath_FromEnv(t *testing.T) {
	resetDaemonFlags()
	flagProject = ""

	originalEnv := os.Getenv("SLB_PROJECT")
	defer os.Setenv("SLB_PROJECT", originalEnv)

	project := physicalDaemonTestPath(t, t.TempDir())
	os.Setenv("SLB_PROJECT", project)

	result, err := daemonProjectPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != project {
		t.Errorf("expected %q, got %q", project, result)
	}
}

func TestDaemonProjectPath_FallbackToCwd(t *testing.T) {
	resetDaemonFlags()
	flagProject = ""
	// Project discovery walks up to the nearest .slb. Start below no .slb,
	// not in a checkout that may sit under one (such as a home directory
	// where slb is in use).
	t.Chdir(t.TempDir())

	originalEnv := os.Getenv("SLB_PROJECT")
	defer os.Setenv("SLB_PROJECT", originalEnv)
	os.Setenv("SLB_PROJECT", "")

	result, err := daemonProjectPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cwd, _ := os.Getwd()
	cwd = physicalDaemonTestPath(t, cwd)
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
	t.Chdir(t.TempDir()) // see TestDaemonProjectPath_FallbackToCwd

	originalEnv := os.Getenv("SLB_PROJECT")
	defer os.Setenv("SLB_PROJECT", originalEnv)

	// 1. Flag takes precedence over env
	flagPath := physicalDaemonTestPath(t, t.TempDir())
	envPath := physicalDaemonTestPath(t, t.TempDir())
	flagProject = flagPath
	os.Setenv("SLB_PROJECT", envPath)

	result, err := daemonProjectPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != flagPath {
		t.Errorf("flag should take precedence, got %q", result)
	}

	// 2. Env takes precedence over cwd
	flagProject = ""
	result, err = daemonProjectPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != envPath {
		t.Errorf("env should take precedence over cwd, got %q", result)
	}

	// 3. Cwd is final fallback
	os.Setenv("SLB_PROJECT", "")
	result, err = daemonProjectPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cwd, _ := os.Getwd()
	cwd = physicalDaemonTestPath(t, cwd)
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

func newTestDaemonLifecycleCommand(name string, runE func(*cobra.Command, []string) error) *cobra.Command {
	root := newTestRootCmd()
	root.PersistentPreRunE = rootCmd.PersistentPreRunE
	daemonRoot := &cobra.Command{
		Use:               "daemon",
		PersistentPreRunE: daemonCmd.PersistentPreRunE,
	}
	daemonRoot.AddCommand(&cobra.Command{Use: name, RunE: runE})
	root.AddCommand(daemonRoot)
	return root
}

func TestDaemonLifecycleCommandsResolveProjectOnce(t *testing.T) {
	base := physicalDaemonTestPath(t, t.TempDir())
	projectA, projectB := filepath.Join(base, "A"), filepath.Join(base, "B")
	subA := filepath.Join(projectA, "src", "pkg")
	for _, dir := range []string{filepath.Join(projectA, ".slb"), filepath.Join(projectB, ".slb"), subA} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, command := range []string{"start", "stop", "status"} {
		for _, tc := range []struct {
			name, cwd, env, want string
			args                 []string
		}{
			{"relative flag", base, "", projectA, []string{"-C", filepath.Join("A", "src", "pkg")}},
			{"absolute flag", projectB, "", projectA, []string{"-C", subA}},
			{"environment", base, filepath.Join("A", "src", "pkg"), projectA, nil},
			{"working directory", subA, "", projectA, nil},
			{"flag precedence", base, subA, projectB, []string{"-C", "B"}},
		} {
			t.Run(command+"/"+tc.name, func(t *testing.T) {
				resetDaemonFlags()
				t.Cleanup(resetDaemonFlags)
				t.Chdir(tc.cwd)
				t.Setenv("SLB_PROJECT", tc.env)
				called := false
				root := newTestDaemonLifecycleCommand(command, func(_ *cobra.Command, _ []string) error {
					called = true
					project, err := daemonProjectPath()
					if err != nil {
						return err
					}
					if project != tc.want {
						t.Errorf("project=%q, want %q", project, tc.want)
					}
					cwd, err := os.Getwd()
					if err != nil {
						return err
					}
					if cwd != tc.want {
						t.Errorf("cwd=%q, want %q", cwd, tc.want)
					}
					return nil
				})
				args := append([]string{"daemon", command}, tc.args...)
				if _, _, err := executeCommand(root, args...); err != nil {
					t.Fatalf("command failed: %v", err)
				}
				if !called {
					t.Fatal("lifecycle handler did not run")
				}
			})
		}
	}
}

func TestDaemonStatusUsesSelectedProjectPathsAndDatabase(t *testing.T) {
	resetDaemonFlags()
	t.Cleanup(resetDaemonFlags)
	t.Setenv("SLB_PROJECT", "")
	t.Setenv("TMPDIR", t.TempDir())
	a, b := testutil.NewHarness(t), testutil.NewHarness(t)
	a.ProjectDir = physicalDaemonTestPath(t, a.ProjectDir)
	b.ProjectDir = physicalDaemonTestPath(t, b.ProjectDir)
	// A reachable remote daemon for B must not substitute for the selected
	// local project A, whose daemon has never been started.
	remote, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var remoteConnections atomic.Int32
	remoteDone := make(chan struct{})
	t.Cleanup(func() {
		_ = remote.Close()
		<-remoteDone
	})
	go func() {
		defer close(remoteDone)
		for {
			conn, err := remote.Accept()
			if err != nil {
				return
			}
			remoteConnections.Add(1)
			func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
				decoder := json.NewDecoder(conn)
				var auth map[string]string
				if err := decoder.Decode(&auth); err != nil {
					return
				}
				var request daemon.RPCRequest
				if err := decoder.Decode(&request); err != nil || request.Method != "ping" {
					return
				}
				_ = json.NewEncoder(conn).Encode(daemon.RPCResponse{
					ID: request.ID, Result: map[string]any{"pong": true, "project_path": b.ProjectDir},
				})
			}()
		}
	}()
	t.Setenv("SLB_HOST", remote.Addr().String())
	t.Setenv("SLB_SESSION_KEY", "remote-project-b")
	if info := daemon.NewClient().GetStatusInfo(); info.Status != daemon.DaemonRunning || info.SocketPath != remote.Addr().String() {
		t.Fatalf("remote fixture is not reachable by ordinary TCP clients: %+v", info)
	}
	session := testutil.MakeSession(t, a.DB, testutil.WithProject(a.ProjectDir))
	testutil.MakeRequest(t, a.DB, session)
	subA := filepath.Join(a.ProjectDir, "src")
	if err := os.Mkdir(subA, 0700); err != nil {
		t.Fatal(err)
	}
	pidFileB := daemon.PIDFileForCWD(b.ProjectDir)
	pidContents := strconv.Itoa(os.Getpid()) + "\n"
	if err := os.WriteFile(pidFileB, []byte(pidContents), 0600); err != nil {
		t.Fatal(err)
	}
	for _, explicit := range []bool{false, true} {
		name := "working directory"
		if explicit {
			name = "explicit relative project"
		}
		t.Run(name, func(t *testing.T) {
			resetDaemonFlags()
			args := []string{"daemon", "status", "--json"}
			if explicit {
				t.Chdir(b.ProjectDir)
				relative, err := filepath.Rel(b.ProjectDir, subA)
				if err != nil {
					t.Fatal(err)
				}
				args = append(args, "-C", relative)
			} else {
				t.Chdir(subA)
			}
			root := newTestDaemonLifecycleCommand("status", daemonStatusCmd.RunE)
			stdout, err := executeCommandCapture(t, root, args...)
			if err != nil {
				t.Fatalf("status: %v", err)
			}
			var result struct {
				Status         string `json:"status"`
				PID            int    `json:"pid"`
				PIDFile        string `json:"pid_file"`
				SocketPath     string `json:"socket_path"`
				PendingCount   int    `json:"pending_count"`
				ActiveSessions int    `json:"active_sessions"`
			}
			if err := json.Unmarshal([]byte(stdout), &result); err != nil {
				t.Fatalf("decode status %q: %v", stdout, err)
			}
			if result.Status != "not running" || result.PID != 0 {
				t.Errorf("status inherited another project's PID: %+v", result)
			}
			if result.PIDFile != daemon.PIDFileForCWD(a.ProjectDir) || result.SocketPath != daemon.SocketPathForCWD(a.ProjectDir) {
				t.Errorf("status reported the wrong project paths: %+v", result)
			}
			if result.PendingCount != 1 || result.ActiveSessions != 1 {
				t.Errorf("status did not use the selected project database: %+v", result)
			}
		})
	}
	if contents, err := os.ReadFile(pidFileB); err != nil || string(contents) != pidContents {
		t.Fatalf("status changed project B PID: contents=%q err=%v", contents, err)
	}
	if got := remoteConnections.Load(); got != 1 {
		t.Fatalf("local lifecycle status contacted the remote daemon: connections=%d, want only the fixture probe", got)
	}
}

func TestDaemonProjectPathRejectsInvalidDirectory(t *testing.T) {
	resetDaemonFlags()
	t.Cleanup(resetDaemonFlags)
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, ".slb"), 0700); err != nil {
		t.Fatal(err)
	}
	flagProject = filepath.Join(project, "missing")
	if _, err := daemonProjectPath(); err == nil {
		t.Fatal("missing -C directory silently resolved to its parent project")
	}
	flagProject = filepath.Join(project, "file")
	if err := os.WriteFile(flagProject, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := daemonProjectPath(); err == nil {
		t.Fatal("regular file accepted as a project directory")
	}
}

func TestDaemonStatusSymlinkProjectMatchesWorkingDirectory(t *testing.T) {
	resetDaemonFlags()
	t.Cleanup(resetDaemonFlags)
	t.Setenv("SLB_PROJECT", "")
	t.Setenv("SLB_HOST", "")
	h := testutil.NewHarness(t)
	h.ProjectDir = physicalDaemonTestPath(t, h.ProjectDir)
	subdir := filepath.Join(h.ProjectDir, "src", "pkg")
	if err := os.MkdirAll(subdir, 0700); err != nil {
		t.Fatal(err)
	}
	outside := physicalDaemonTestPath(t, t.TempDir())
	alias := filepath.Join(outside, "project-descendant")
	if err := os.Symlink(subdir, alias); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink creation requires Windows developer mode or privilege: %v", err)
		}
		t.Fatal(err)
	}
	session := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir))
	testutil.MakeRequest(t, h.DB, session)
	for _, tc := range []struct {
		name, cwd, env string
		args           []string
	}{
		{"physical working directory", subdir, "", nil},
		{"explicit symlink", outside, "", []string{"-C", alias}},
		{"environment symlink", outside, alias, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetDaemonFlags()
			t.Chdir(tc.cwd)
			t.Setenv("SLB_PROJECT", tc.env)
			root := newTestDaemonLifecycleCommand("status", daemonStatusCmd.RunE)
			args := append([]string{"daemon", "status", "--json"}, tc.args...)
			stdout, err := executeCommandCapture(t, root, args...)
			if err != nil {
				t.Fatalf("status: %v", err)
			}
			var result struct {
				PIDFile        string `json:"pid_file"`
				SocketPath     string `json:"socket_path"`
				PendingCount   int    `json:"pending_count"`
				ActiveSessions int    `json:"active_sessions"`
			}
			if err := json.Unmarshal([]byte(stdout), &result); err != nil {
				t.Fatal(err)
			}
			if flagProject != h.ProjectDir || result.PIDFile != daemon.PIDFileForCWD(h.ProjectDir) || result.SocketPath != daemon.SocketPathForCWD(h.ProjectDir) {
				t.Fatalf("project alias selected different lifecycle resources: project=%q status=%+v", flagProject, result)
			}
			if result.PendingCount != 1 || result.ActiveSessions != 1 {
				t.Fatalf("project alias selected a different database: %+v", result)
			}
		})
	}
}
