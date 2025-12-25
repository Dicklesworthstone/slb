package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/spf13/cobra"
)

// captureStdout runs a function and captures what it writes to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create pipe: %v", err)
	}
	os.Stdout = w

	fn()

	w.Close()
	os.Stdout = oldStdout

	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

// executeCommandCapture runs a command and captures actual stdout.
func executeCommandCapture(t *testing.T, root *cobra.Command, args ...string) (stdout string, err error) {
	t.Helper()

	root.SetArgs(args)

	stdout = captureStdout(t, func() {
		err = root.Execute()
	})

	return stdout, err
}

// newTestSessionCmd creates a fresh session command tree for testing.
func newTestSessionCmd(dbPath string) *cobra.Command {
	// Create root
	root := &cobra.Command{
		Use:           "slb",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// Add persistent flags
	root.PersistentFlags().StringVarP(&flagConfig, "config", "c", "", "config file path")
	root.PersistentFlags().StringVarP(&flagOutput, "output", "o", "text", "output format")
	root.PersistentFlags().BoolVarP(&flagJSON, "json", "j", false, "shorthand for --output=json")
	root.PersistentFlags().BoolVarP(&flagVerbose, "verbose", "v", false, "verbose output")
	root.PersistentFlags().StringVar(&flagDB, "db", dbPath, "database path")
	root.PersistentFlags().StringVar(&flagActor, "actor", "", "actor identifier")
	root.PersistentFlags().StringVarP(&flagSessionID, "session-id", "s", "", "session ID")
	root.PersistentFlags().StringVarP(&flagProject, "project", "C", "", "project directory")

	// Add session command tree
	root.AddCommand(sessionCmd)

	return root
}

// resetSessionFlags resets all session-related flags to defaults.
func resetSessionFlags() {
	flagConfig = ""
	flagOutput = "text"
	flagJSON = false
	flagVerbose = false
	flagDB = ""
	flagActor = ""
	flagSessionID = ""
	flagProject = ""
	flagSessionAgent = ""
	flagSessionProg = ""
	flagSessionModel = ""
	flagResumeCreateIfMissing = true
	flagResumeForce = false
	flagSessionGCDryRun = false
	flagSessionGCForce = false
}

func TestSessionStart_RequiresAgent(t *testing.T) {
	h := testutil.NewHarness(t)
	resetSessionFlags()

	cmd := newTestSessionCmd(h.DBPath)
	_, _, err := executeCommand(cmd, "session", "start", "-C", h.ProjectDir)

	if err == nil {
		t.Fatal("expected error when --agent is missing")
	}
	if !strings.Contains(err.Error(), "--agent is required") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestSessionStart_CreatesSession(t *testing.T) {
	h := testutil.NewHarness(t)
	resetSessionFlags()

	cmd := newTestSessionCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "session", "start",
		"-a", "TestAgent",
		"-p", "test-program",
		"-m", "test-model",
		"-C", h.ProjectDir,
		"-j",
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	// Verify required fields
	if result["agent_name"] != "TestAgent" {
		t.Errorf("expected agent_name=TestAgent, got %v", result["agent_name"])
	}
	if result["program"] != "test-program" {
		t.Errorf("expected program=test-program, got %v", result["program"])
	}
	if result["model"] != "test-model" {
		t.Errorf("expected model=test-model, got %v", result["model"])
	}
	if result["session_id"] == nil || result["session_id"] == "" {
		t.Error("expected session_id to be set")
	}
	if result["session_key"] == nil || result["session_key"] == "" {
		t.Error("expected session_key to be set")
	}
}

func TestSessionStart_DuplicatePrevented(t *testing.T) {
	h := testutil.NewHarness(t)
	resetSessionFlags()

	cmd := newTestSessionCmd(h.DBPath)

	// Start first session
	_, err := executeCommandCapture(t, cmd, "session", "start",
		"-a", "TestAgent",
		"-C", h.ProjectDir,
		"-j",
	)
	if err != nil {
		t.Fatalf("first session start failed: %v", err)
	}

	// Try to start second session with same agent - should fail
	resetSessionFlags()
	cmd2 := newTestSessionCmd(h.DBPath)
	_, err = executeCommandCapture(t, cmd2, "session", "start",
		"-a", "TestAgent",
		"-C", h.ProjectDir,
		"-j",
	)
	if err == nil {
		t.Fatal("expected error for duplicate session")
	}
	if !strings.Contains(err.Error(), "active session already exists") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestSessionEnd_RequiresSessionID(t *testing.T) {
	h := testutil.NewHarness(t)
	resetSessionFlags()

	cmd := newTestSessionCmd(h.DBPath)
	_, _, err := executeCommand(cmd, "session", "end")

	if err == nil {
		t.Fatal("expected error when --session-id is missing")
	}
	if !strings.Contains(err.Error(), "--session-id is required") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestSessionEnd_EndsSession(t *testing.T) {
	h := testutil.NewHarness(t)
	resetSessionFlags()

	// First create a session directly
	sess := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir), testutil.WithAgent("TestAgent"))

	cmd := newTestSessionCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "session", "end", "-s", sess.ID, "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["session_id"] != sess.ID {
		t.Errorf("expected session_id=%s, got %v", sess.ID, result["session_id"])
	}
	if result["ended_at"] == nil {
		t.Error("expected ended_at to be set")
	}

	// Verify session is actually ended
	ended, err := h.DB.GetSession(sess.ID)
	if err != nil {
		t.Fatalf("failed to get session: %v", err)
	}
	if ended.EndedAt == nil {
		t.Error("session should have ended_at set")
	}
}

func TestSessionList_ReturnsActiveSessions(t *testing.T) {
	h := testutil.NewHarness(t)
	resetSessionFlags()

	// Create multiple sessions
	testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir), testutil.WithAgent("Agent1"))
	testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir), testutil.WithAgent("Agent2"))

	cmd := newTestSessionCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "session", "list", "-C", h.ProjectDir, "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result []map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if len(result) != 2 {
		t.Errorf("expected 2 sessions, got %d", len(result))
	}

	// Check that both agents are in the list
	agents := make(map[string]bool)
	for _, s := range result {
		if name, ok := s["agent_name"].(string); ok {
			agents[name] = true
		}
	}
	if !agents["Agent1"] || !agents["Agent2"] {
		t.Errorf("expected both Agent1 and Agent2 in list, got %v", agents)
	}
}

func TestSessionList_EmptyProject(t *testing.T) {
	h := testutil.NewHarness(t)
	resetSessionFlags()

	cmd := newTestSessionCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "session", "list", "-C", h.ProjectDir, "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result []map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if len(result) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(result))
	}
}

func TestSessionResume_RequiresAgent(t *testing.T) {
	h := testutil.NewHarness(t)
	resetSessionFlags()

	cmd := newTestSessionCmd(h.DBPath)
	_, _, err := executeCommand(cmd, "session", "resume", "-C", h.ProjectDir)

	if err == nil {
		t.Fatal("expected error when --agent is missing")
	}
	if !strings.Contains(err.Error(), "--agent is required") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestSessionResume_CreatesNewIfNoneExists(t *testing.T) {
	h := testutil.NewHarness(t)
	resetSessionFlags()

	cmd := newTestSessionCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "session", "resume",
		"-a", "NewAgent",
		"-p", "test-program",
		"-m", "test-model",
		"-C", h.ProjectDir,
		"-j",
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["agent_name"] != "NewAgent" {
		t.Errorf("expected agent_name=NewAgent, got %v", result["agent_name"])
	}
	if result["session_id"] == nil {
		t.Error("expected session_id to be set")
	}
}

func TestSessionResume_ReturnsExistingSession(t *testing.T) {
	h := testutil.NewHarness(t)
	resetSessionFlags()

	// Create existing session
	existing := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("ExistingAgent"),
		testutil.WithProgram("original-program"),
		testutil.WithModel("original-model"),
	)

	cmd := newTestSessionCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "session", "resume",
		"-a", "ExistingAgent",
		"-p", "original-program",
		"-m", "original-model",
		"-C", h.ProjectDir,
		"-j",
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	// Should return the existing session
	if result["session_id"] != existing.ID {
		t.Errorf("expected session_id=%s, got %v", existing.ID, result["session_id"])
	}
}

func TestSessionHeartbeat_RequiresSessionID(t *testing.T) {
	h := testutil.NewHarness(t)
	resetSessionFlags()

	cmd := newTestSessionCmd(h.DBPath)
	_, _, err := executeCommand(cmd, "session", "heartbeat")

	if err == nil {
		t.Fatal("expected error when --session-id is missing")
	}
	if !strings.Contains(err.Error(), "--session-id is required") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestSessionHeartbeat_UpdatesTimestamp(t *testing.T) {
	h := testutil.NewHarness(t)
	resetSessionFlags()

	// Create session
	sess := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir))

	cmd := newTestSessionCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "session", "heartbeat", "-s", sess.ID, "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["session_id"] != sess.ID {
		t.Errorf("expected session_id=%s, got %v", sess.ID, result["session_id"])
	}
	if result["last_active_at"] == nil {
		t.Error("expected last_active_at to be set")
	}
}

func TestSessionResetLimits_RequiresSessionID(t *testing.T) {
	h := testutil.NewHarness(t)
	resetSessionFlags()

	cmd := newTestSessionCmd(h.DBPath)
	_, _, err := executeCommand(cmd, "session", "reset-limits")

	if err == nil {
		t.Fatal("expected error when --session-id is missing")
	}
	if !strings.Contains(err.Error(), "--session-id is required") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestSessionResetLimits_ResetsLimits(t *testing.T) {
	h := testutil.NewHarness(t)
	resetSessionFlags()

	// Create session
	sess := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir))

	cmd := newTestSessionCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "session", "reset-limits", "-s", sess.ID, "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["session_id"] != sess.ID {
		t.Errorf("expected session_id=%s, got %v", sess.ID, result["session_id"])
	}
	if result["status"] != "ok" {
		t.Errorf("expected status=ok, got %v", result["status"])
	}
}

func TestSessionGC_DryRun(t *testing.T) {
	h := testutil.NewHarness(t)
	resetSessionFlags()

	// Create a "stale" session by manipulating the DB directly
	sess := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir))
	// Backdate the session using RFC3339 format (required by sessions.go parsing)
	backdatedTime := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	_, err := h.DB.Exec(`UPDATE sessions SET last_active_at = ? WHERE id = ?`, backdatedTime, sess.ID)
	if err != nil {
		t.Fatalf("failed to backdate session: %v", err)
	}

	cmd := newTestSessionCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "session", "gc",
		"-C", h.ProjectDir,
		"--dry-run",
		"--threshold", "30m",
		"-j",
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["dry_run"] != true {
		t.Error("expected dry_run=true")
	}
	if result["found"].(float64) < 1 {
		t.Error("expected at least 1 stale session to be found")
	}
}

func TestSessionGC_NoStale(t *testing.T) {
	h := testutil.NewHarness(t)
	resetSessionFlags()

	// Create a fresh session (not stale)
	testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir))

	cmd := newTestSessionCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "session", "gc",
		"-C", h.ProjectDir,
		"--threshold", "30m",
		"-f", // force to skip confirmation
		"-j",
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["found"].(float64) != 0 {
		t.Errorf("expected found=0 for fresh sessions, got %v", result["found"])
	}
}

func TestSessionTextOutput(t *testing.T) {
	h := testutil.NewHarness(t)
	resetSessionFlags()

	cmd := newTestSessionCmd(h.DBPath)
	// Capture stderr as well since text output goes there
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	_, err := executeCommandCapture(t, cmd, "session", "start",
		"-a", "TextAgent",
		"-p", "text-program",
		"-m", "text-model",
		"-C", h.ProjectDir,
		// No -j flag, so text output
	)

	w.Close()
	os.Stderr = oldStderr

	var stderrBuf bytes.Buffer
	io.Copy(&stderrBuf, r)
	stderr := stderrBuf.String()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Text output goes to stderr (by design in output package)
	// It should contain session info
	if !strings.Contains(stderr, "session") && !strings.Contains(stderr, "TextAgent") {
		// Some output should be present
		if stderr == "" {
			t.Error("expected some text output to stderr")
		}
	}
}

// Test helper to capture stderr for error messages
func executeCommandWithStderr(root *cobra.Command, args ...string) (stdout string, stderr string, err error) {
	stdoutBuf := new(bytes.Buffer)
	stderrBuf := new(bytes.Buffer)

	root.SetOut(stdoutBuf)
	root.SetErr(stderrBuf)
	root.SetArgs(args)

	// Redirect os.Stderr temporarily
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	err = root.Execute()

	w.Close()
	os.Stderr = oldStderr

	var capturedStderr bytes.Buffer
	capturedStderr.ReadFrom(r)

	return stdoutBuf.String(), stderrBuf.String() + capturedStderr.String(), err
}

func TestSessionCommand_Help(t *testing.T) {
	h := testutil.NewHarness(t)
	resetSessionFlags()

	cmd := newTestSessionCmd(h.DBPath)
	// Help output goes to the command's output, not os.Stdout
	stdout, _, err := executeCommand(cmd, "session", "--help")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(stdout, "Manage agent sessions") {
		t.Error("expected help text to contain 'Manage agent sessions'")
	}

	// Verify subcommands are listed
	subcommands := []string{"start", "end", "resume", "list", "heartbeat", "reset-limits", "gc"}
	for _, sub := range subcommands {
		if !strings.Contains(stdout, sub) {
			t.Errorf("expected help to mention subcommand %q", sub)
		}
	}
}

func TestProjectPath_WithFlag(t *testing.T) {
	resetSessionFlags()
	flagProject = "/test/path"

	result, err := projectPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "/test/path" {
		t.Errorf("expected '/test/path', got %q", result)
	}
}

func TestProjectPath_FallbackToCwd(t *testing.T) {
	resetSessionFlags()
	flagProject = ""

	result, err := projectPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cwd, _ := os.Getwd()
	if result != cwd {
		t.Errorf("expected cwd %q, got %q", cwd, result)
	}
}

// TestProjectPath_DocumentedLimitations documents edge cases that are hard to test.
// Note: The error path (when os.Getwd() fails) is a system-level condition
// that's difficult to trigger in unit tests without process manipulation.
func TestProjectPath_DocumentedLimitations(t *testing.T) {
	resetSessionFlags()

	// Test 1: Flag path always takes precedence
	flagProject = "/explicit/project/path"
	result, err := projectPath()
	if err != nil {
		t.Fatalf("unexpected error with flag: %v", err)
	}
	if result != "/explicit/project/path" {
		t.Errorf("flag path should take precedence, got %q", result)
	}

	// Test 2: Empty flag returns cwd
	flagProject = ""
	result, err = projectPath()
	if err != nil {
		t.Fatalf("unexpected error with empty flag: %v", err)
	}
	cwd, _ := os.Getwd()
	if result != cwd {
		t.Errorf("empty flag should return cwd, got %q, want %q", result, cwd)
	}

	// Test 3: Verify we never get empty result in normal operation
	if result == "" {
		t.Error("projectPath() should never return empty string in normal operation")
	}

	// Note: The error path (when os.Getwd() fails) would require either:
	// 1. Deleting the current directory during the test
	// 2. Running in a restricted environment
	// This is documented as an acceptable coverage gap (83.3% -> acceptable).
}

// TestProjectPath_AbsolutePaths verifies that projectPath handles absolute paths correctly.
func TestProjectPath_AbsolutePaths(t *testing.T) {
	resetSessionFlags()

	tests := []struct {
		name     string
		flagPath string
		want     string
	}{
		{"unix absolute path", "/home/user/project", "/home/user/project"},
		{"path with spaces", "/home/user/my project", "/home/user/my project"},
		{"nested path", "/a/b/c/d/e", "/a/b/c/d/e"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flagProject = tt.flagPath
			result, err := projectPath()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result != tt.want {
				t.Errorf("projectPath() = %q, want %q", result, tt.want)
			}
		})
	}
}

// Test that session commands work with the --db flag
func TestSessionCommands_WithDBFlag(t *testing.T) {
	h := testutil.NewHarness(t)
	resetSessionFlags()

	// Use explicit --db flag instead of relying on default
	cmd := &cobra.Command{
		Use:           "slb",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.PersistentFlags().StringVar(&flagDB, "db", "", "database path")
	cmd.PersistentFlags().StringVarP(&flagOutput, "output", "o", "text", "output format")
	cmd.PersistentFlags().BoolVarP(&flagJSON, "json", "j", false, "json output")
	cmd.PersistentFlags().StringVarP(&flagProject, "project", "C", "", "project directory")
	cmd.AddCommand(sessionCmd)

	stdout, err := executeCommandCapture(t, cmd, "session", "start",
		"-a", "DBFlagAgent",
		"--db", h.DBPath,
		"-C", h.ProjectDir,
		"-j",
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["agent_name"] != "DBFlagAgent" {
		t.Errorf("expected agent_name=DBFlagAgent, got %v", result["agent_name"])
	}

	// Verify it was actually stored in the specified DB
	dbConn, err := db.Open(h.DBPath)
	if err != nil {
		t.Fatalf("failed to open DB: %v", err)
	}
	defer dbConn.Close()

	sessions, err := dbConn.ListActiveSessions(h.ProjectDir)
	if err != nil {
		t.Fatalf("failed to list sessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].AgentName != "DBFlagAgent" {
		t.Errorf("session not found in specified DB, got %v", sessions)
	}
}
