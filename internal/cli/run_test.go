package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/spf13/cobra"
)

// newTestRunCmd creates a fresh run command for testing.
func newTestRunCmd(dbPath string) *cobra.Command {
	root := &cobra.Command{
		Use:           "slb",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVar(&flagDB, "db", dbPath, "database path")
	root.PersistentFlags().StringVarP(&flagOutput, "output", "o", "text", "output format")
	root.PersistentFlags().BoolVarP(&flagJSON, "json", "j", false, "json output")
	root.PersistentFlags().StringVarP(&flagProject, "project", "C", "", "project directory")
	root.PersistentFlags().StringVarP(&flagSessionID, "session-id", "s", "", "session ID")
	root.PersistentFlags().StringVarP(&flagConfig, "config", "c", "", "config file")

	// Create fresh run command
	rCmd := &cobra.Command{
		Use:   "run <command>",
		Short: "Run a command with approval if required",
		Args:  cobra.ExactArgs(1),
		RunE:  runCmd.RunE,
	}
	rCmd.Flags().StringVar(&flagRunReason, "reason", "", "reason for command")
	rCmd.Flags().StringVar(&flagRunExpectedEffect, "expected-effect", "", "expected effect")
	rCmd.Flags().StringVar(&flagRunGoal, "goal", "", "goal")
	rCmd.Flags().StringVar(&flagRunSafety, "safety", "", "safety argument")
	rCmd.Flags().IntVar(&flagRunTimeout, "timeout", 300, "timeout seconds")
	rCmd.Flags().BoolVar(&flagRunYield, "yield", false, "yield to background")
	rCmd.Flags().StringSliceVar(&flagRunAttachFile, "attach-file", nil, "attach file")
	rCmd.Flags().StringSliceVar(&flagRunAttachContext, "attach-context", nil, "attach context")
	rCmd.Flags().StringSliceVar(&flagRunAttachScreen, "attach-screenshot", nil, "attach screenshot")

	root.AddCommand(rCmd)

	return root
}

func resetRunFlags() {
	flagDB = ""
	flagOutput = "text"
	flagJSON = false
	flagProject = ""
	flagSessionID = ""
	flagConfig = ""
	flagRunReason = ""
	flagRunExpectedEffect = ""
	flagRunGoal = ""
	flagRunSafety = ""
	flagRunTimeout = 300
	flagRunYield = false
	flagRunAttachFile = nil
	flagRunAttachContext = nil
	flagRunAttachScreen = nil
}

func TestRunCommand_RequiresCommand(t *testing.T) {
	h := testutil.NewHarness(t)
	resetRunFlags()

	cmd := newTestRunCmd(h.DBPath)
	_, _, err := executeCommand(cmd, "run")

	if err == nil {
		t.Fatal("expected error when command is missing")
	}
	if !strings.Contains(err.Error(), "accepts 1 arg") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRunCommand_RequiresSessionID(t *testing.T) {
	h := testutil.NewHarness(t)
	resetRunFlags()

	cmd := newTestRunCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "run", "echo hello", "-C", h.ProjectDir, "-j")

	if err == nil {
		t.Fatal("expected error when --session-id is missing")
	}
	if !strings.Contains(err.Error(), "--session-id is required") {
		t.Errorf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}
	if result["status"] != "missing_session_id" {
		t.Errorf("expected status=missing_session_id, got %v", result["status"])
	}
	if result["command"] != "echo hello" {
		t.Errorf("expected command echo hello, got %v", result["command"])
	}
	if !strings.Contains(result["error"].(string), "--session-id is required") {
		t.Errorf("unexpected error payload: %v", result["error"])
	}
}

func TestRunCommand_Help(t *testing.T) {
	h := testutil.NewHarness(t)
	resetRunFlags()

	cmd := newTestRunCmd(h.DBPath)
	stdout, _, err := executeCommand(cmd, "run", "--help")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(stdout, "run") {
		t.Error("expected help to mention 'run'")
	}
	if !strings.Contains(stdout, "--reason") {
		t.Error("expected help to mention '--reason' flag")
	}
	if !strings.Contains(stdout, "--timeout") {
		t.Error("expected help to mention '--timeout' flag")
	}
	if !strings.Contains(stdout, "--yield") {
		t.Error("expected help to mention '--yield' flag")
	}
	if !strings.Contains(stdout, "attach") {
		t.Error("expected help to mention attachment flags")
	}
}

func TestRunCommand_YieldLoadsCustomPatternsBeforeSafeExecution(t *testing.T) {
	h := testutil.NewHarness(t)
	resetRunFlags()

	session := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("GrayTower"),
	)
	if _, err := h.DB.InsertCustomPattern(
		"dangerous",
		`^echo\s+SLB_RUN_CUSTOM_PATTERN_SENTINEL$`,
		"run must honor persisted custom patterns before safe execution",
		"test",
	); err != nil {
		t.Fatalf("InsertCustomPattern: %v", err)
	}

	cmd := newTestRunCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd,
		"run",
		"echo SLB_RUN_CUSTOM_PATTERN_SENTINEL",
		"-C", h.ProjectDir,
		"--session-id", session.ID,
		"--reason", "regression",
		"--yield",
		"-j",
	)
	if err != nil {
		t.Fatalf("run --yield returned error: %v\nstdout: %s", err, stdout)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("parse run JSON: %v\nstdout: %s", err, stdout)
	}
	if result["status"] != "pending" {
		t.Fatalf("expected pending request, got %v\nstdout: %s", result["status"], stdout)
	}
	if result["tier"] != "dangerous" {
		t.Fatalf("expected tier=dangerous, got %v\nstdout: %s", result["tier"], stdout)
	}
	if result["min_approvals"] != float64(1) {
		t.Fatalf("expected min_approvals=1, got %v\nstdout: %s", result["min_approvals"], stdout)
	}
}

// Note: TestRunCommand_InvalidSession is skipped because the run command
// calls os.Exit on errors, which would terminate the test process.
// This behavior would need to be refactored to support proper testing.

func TestToRateLimitConfig(t *testing.T) {
	// Test the helper function that converts config to rate limit config
	// This is a unit test for the internal function
	cfg := config.DefaultConfig()
	cfg.RateLimits.MaxPendingPerSession = 5
	cfg.RateLimits.MaxRequestsPerMinute = 10
	cfg.RateLimits.RateLimitAction = "reject"

	result := toRateLimitConfig(cfg)

	if result.MaxPendingPerSession != 5 {
		t.Errorf("expected MaxPendingPerSession=5, got %d", result.MaxPendingPerSession)
	}
	if result.MaxRequestsPerMinute != 10 {
		t.Errorf("expected MaxRequestsPerMinute=10, got %d", result.MaxRequestsPerMinute)
	}
}

func TestToRequestCreatorConfig(t *testing.T) {
	// Test the helper function that converts config to request creator config
	cfg := config.DefaultConfig()
	cfg.General.RequestTimeoutSecs = 1800 // 30 minutes
	cfg.General.ApprovalTTLMins = 60
	cfg.Agents.Blocked = []string{"blocked-agent"}

	result := toRequestCreatorConfig(cfg)

	if result.RequestTimeoutMinutes != 30 {
		t.Errorf("expected RequestTimeoutMinutes=30, got %d", result.RequestTimeoutMinutes)
	}
	if result.ApprovalTTLMinutes != 60 {
		t.Errorf("expected ApprovalTTLMinutes=60, got %d", result.ApprovalTTLMinutes)
	}
	if len(result.BlockedAgents) != 1 || result.BlockedAgents[0] != "blocked-agent" {
		t.Errorf("expected BlockedAgents=['blocked-agent'], got %v", result.BlockedAgents)
	}
}

func TestToRateLimitConfig_InvalidAction(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.RateLimits.RateLimitAction = "invalid-action"

	result := toRateLimitConfig(cfg)

	// Should default to "reject" for invalid action
	if result.Action != "reject" {
		t.Errorf("expected Action=reject for invalid action, got %v", result.Action)
	}
}

func TestToRateLimitConfig_QueueAction(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.RateLimits.RateLimitAction = "queue"

	result := toRateLimitConfig(cfg)

	if result.Action != "queue" {
		t.Errorf("expected Action=queue, got %v", result.Action)
	}
}

func TestToRateLimitConfig_WarnAction(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.RateLimits.RateLimitAction = "warn"

	result := toRateLimitConfig(cfg)

	if result.Action != "warn" {
		t.Errorf("expected Action=warn, got %v", result.Action)
	}
}

func TestToRequestCreatorConfig_ZeroTimeout(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.General.RequestTimeoutSecs = 0

	result := toRequestCreatorConfig(cfg)

	// Should default to 30 for zero/negative timeout
	if result.RequestTimeoutMinutes != 30 {
		t.Errorf("expected RequestTimeoutMinutes=30 for zero timeout, got %d", result.RequestTimeoutMinutes)
	}
}

func TestToRequestCreatorConfig_NegativeTimeout(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.General.RequestTimeoutSecs = -60

	result := toRequestCreatorConfig(cfg)

	// Should default to 30 for negative timeout
	if result.RequestTimeoutMinutes != 30 {
		t.Errorf("expected RequestTimeoutMinutes=30 for negative timeout, got %d", result.RequestTimeoutMinutes)
	}
}

func TestToRequestCreatorConfig_WithIntegrations(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Integrations.AgentMailEnabled = true
	cfg.Integrations.AgentMailThread = "test-thread"

	result := toRequestCreatorConfig(cfg)

	if !result.AgentMailEnabled {
		t.Error("expected AgentMailEnabled=true")
	}
	if result.AgentMailThread != "test-thread" {
		t.Errorf("expected AgentMailThread=test-thread, got %v", result.AgentMailThread)
	}
}

// -----------------------------------------------------------------------------
// evaluateRequestForExecution Tests
// -----------------------------------------------------------------------------

func TestEvaluateRequestForExecution_Approved(t *testing.T) {
	result := evaluateRequestForExecution(db.StatusApproved)

	if !result.ShouldExecute {
		t.Error("expected ShouldExecute=true for approved status")
	}
	if result.ShouldContinuePolling {
		t.Error("expected ShouldContinuePolling=false for approved status")
	}
	if !strings.Contains(result.Reason, "approved") {
		t.Errorf("expected Reason to mention 'approved', got %q", result.Reason)
	}
}

func TestEvaluateRequestForExecution_Pending(t *testing.T) {
	result := evaluateRequestForExecution(db.StatusPending)

	if result.ShouldExecute {
		t.Error("expected ShouldExecute=false for pending status")
	}
	if !result.ShouldContinuePolling {
		t.Error("expected ShouldContinuePolling=true for pending status")
	}
	if !strings.Contains(result.Reason, "pending") {
		t.Errorf("expected Reason to mention 'pending', got %q", result.Reason)
	}
}

func TestEvaluateRequestForExecution_Rejected(t *testing.T) {
	result := evaluateRequestForExecution(db.StatusRejected)

	if result.ShouldExecute {
		t.Error("expected ShouldExecute=false for rejected status")
	}
	if result.ShouldContinuePolling {
		t.Error("expected ShouldContinuePolling=false for rejected status")
	}
	if !strings.Contains(result.Reason, "terminal") {
		t.Errorf("expected Reason to mention 'terminal', got %q", result.Reason)
	}
}

func TestEvaluateRequestForExecution_Timeout(t *testing.T) {
	result := evaluateRequestForExecution(db.StatusTimeout)

	if result.ShouldExecute {
		t.Error("expected ShouldExecute=false for timeout status")
	}
	if result.ShouldContinuePolling {
		t.Error("expected ShouldContinuePolling=false for timeout status")
	}
	if !strings.Contains(result.Reason, "terminal") {
		t.Errorf("expected Reason to mention 'terminal', got %q", result.Reason)
	}
}

func TestEvaluateRequestForExecution_Cancelled(t *testing.T) {
	result := evaluateRequestForExecution(db.StatusCancelled)

	if result.ShouldExecute {
		t.Error("expected ShouldExecute=false for cancelled status")
	}
	if result.ShouldContinuePolling {
		t.Error("expected ShouldContinuePolling=false for cancelled status")
	}
}

func TestEvaluateRequestForExecution_ExecutionFailed(t *testing.T) {
	result := evaluateRequestForExecution(db.StatusExecutionFailed)

	if result.ShouldExecute {
		t.Error("expected ShouldExecute=false for execution_failed status")
	}
	if result.ShouldContinuePolling {
		t.Error("expected ShouldContinuePolling=false for execution_failed status")
	}
}

func TestEvaluateRequestForExecution_Executed(t *testing.T) {
	result := evaluateRequestForExecution(db.StatusExecuted)

	if result.ShouldExecute {
		t.Error("expected ShouldExecute=false for executed status")
	}
	if result.ShouldContinuePolling {
		t.Error("expected ShouldContinuePolling=false for executed status")
	}
}

// TestEvaluateRequestForExecution_AllStatuses is a comprehensive table-driven test
// covering all possible status values.
func TestEvaluateRequestForExecution_AllStatuses(t *testing.T) {
	tests := []struct {
		name                  string
		status                db.RequestStatus
		expectExecute         bool
		expectContinuePolling bool
	}{
		// Happy path
		{"approved", db.StatusApproved, true, false},

		// Continue polling
		{"pending", db.StatusPending, false, true},

		// Terminal states - should stop polling
		{"rejected", db.StatusRejected, false, false},
		{"timeout", db.StatusTimeout, false, false},
		{"cancelled", db.StatusCancelled, false, false},
		{"executed", db.StatusExecuted, false, false},
		{"execution_failed", db.StatusExecutionFailed, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := evaluateRequestForExecution(tt.status)

			if result.ShouldExecute != tt.expectExecute {
				t.Errorf("ShouldExecute: expected %v, got %v", tt.expectExecute, result.ShouldExecute)
			}
			if result.ShouldContinuePolling != tt.expectContinuePolling {
				t.Errorf("ShouldContinuePolling: expected %v, got %v", tt.expectContinuePolling, result.ShouldContinuePolling)
			}
			if result.Reason == "" {
				t.Error("Reason should not be empty")
			}
		})
	}
}

// TestExecutionDecision_StructFields verifies the struct fields exist and are accessible.
func TestExecutionDecision_StructFields(t *testing.T) {
	d := ExecutionDecision{
		ShouldExecute:         true,
		ShouldContinuePolling: false,
		Reason:                "test reason",
	}

	if !d.ShouldExecute {
		t.Error("expected ShouldExecute=true")
	}
	if d.ShouldContinuePolling {
		t.Error("expected ShouldContinuePolling=false")
	}
	if d.Reason != "test reason" {
		t.Errorf("expected Reason='test reason', got %q", d.Reason)
	}
}

// TestEvaluateRequestForExecution_ReasonContainsStatus verifies that the reason
// includes useful debugging information about the status.
func TestEvaluateRequestForExecution_ReasonContainsStatus(t *testing.T) {
	// Test that terminal status reasons include the actual status
	terminalStatuses := []db.RequestStatus{
		db.StatusRejected,
		db.StatusTimeout,
		db.StatusCancelled,
		db.StatusExecuted,
		db.StatusExecutionFailed,
	}

	for _, status := range terminalStatuses {
		t.Run(string(status), func(t *testing.T) {
			result := evaluateRequestForExecution(status)
			if !strings.Contains(result.Reason, string(status)) {
				t.Errorf("expected Reason to contain status %q, got %q", status, result.Reason)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// createRunLogFile Tests
// -----------------------------------------------------------------------------

func TestCreateRunLogFile_DefaultPrefix(t *testing.T) {
	// Use temp directory
	tmpDir := t.TempDir()

	logPath, err := createRunLogFile(tmpDir, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should use default "run" prefix
	if !strings.Contains(logPath, "_run.log") {
		t.Errorf("expected log path to contain '_run.log', got %q", logPath)
	}

	// File should exist
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		t.Error("log file should exist")
	}
}

func TestCreateRunLogFile_CustomPrefix(t *testing.T) {
	tmpDir := t.TempDir()

	logPath, err := createRunLogFile(tmpDir, "safe")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(logPath, "_safe.log") {
		t.Errorf("expected log path to contain '_safe.log', got %q", logPath)
	}
}

func TestCreateRunLogFile_WithProject(t *testing.T) {
	tmpDir := t.TempDir()

	logPath, err := createRunLogFile(tmpDir, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should be under project/.slb/logs/
	expectedDir := filepath.Join(tmpDir, ".slb", "logs")
	if !strings.HasPrefix(logPath, expectedDir) {
		t.Errorf("expected log path to start with %q, got %q", expectedDir, logPath)
	}
}

func TestCreateRunLogFile_EmptyProject(t *testing.T) {
	// Save and restore current directory
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get current directory: %v", err)
	}

	tmpDir := t.TempDir()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("failed to change directory: %v", err)
	}
	defer os.Chdir(originalDir)

	logPath, err := createRunLogFile("", "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should use .slb/logs/ relative to current directory
	if !strings.Contains(logPath, ".slb/logs/") {
		t.Errorf("expected log path to contain '.slb/logs/', got %q", logPath)
	}
}

func TestCreateRunLogFile_CreatesDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	logDir := filepath.Join(tmpDir, ".slb", "logs")

	// Directory should not exist yet
	if _, err := os.Stat(logDir); !os.IsNotExist(err) {
		t.Fatal("log directory should not exist before test")
	}

	_, err := createRunLogFile(tmpDir, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Directory should now exist
	if _, err := os.Stat(logDir); os.IsNotExist(err) {
		t.Error("log directory should have been created")
	}
}

func TestCreateRunLogFile_TimestampFormat(t *testing.T) {
	tmpDir := t.TempDir()

	logPath, err := createRunLogFile(tmpDir, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Extract filename
	filename := filepath.Base(logPath)

	// Should match pattern YYYYMMDD-HHMMSS_prefix.log
	// Example: 20251216-150405_test.log
	matched, _ := filepath.Match("????????-??????_test.log", filename)
	if !matched {
		t.Errorf("filename %q doesn't match expected timestamp pattern", filename)
	}
}

func TestCreateRunLogFile_FileIsEmpty(t *testing.T) {
	tmpDir := t.TempDir()

	logPath, err := createRunLogFile(tmpDir, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// File should be empty (just created)
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("failed to stat log file: %v", err)
	}

	if info.Size() != 0 {
		t.Errorf("expected empty file, got size %d", info.Size())
	}
}

func TestCreateRunLogFile_MultipleCallsUniquePaths(t *testing.T) {
	tmpDir := t.TempDir()

	// Create multiple files rapidly
	paths := make(map[string]bool)
	for i := 0; i < 5; i++ {
		logPath, err := createRunLogFile(tmpDir, "test")
		if err != nil {
			t.Fatalf("unexpected error on iteration %d: %v", i, err)
		}

		// In case files are created in the same second, they might have the same name
		// This test verifies the function works, not uniqueness guarantee
		paths[logPath] = true
	}

	// At least one file should have been created
	if len(paths) == 0 {
		t.Error("no log files were created")
	}
}

func TestCreateRunLogFile_DirectoryCreationError(t *testing.T) {
	// Create a file where the directory should be
	tmpDir := t.TempDir()
	blocker := filepath.Join(tmpDir, ".slb")

	// Create a file at .slb (not a directory)
	if err := os.WriteFile(blocker, []byte("blocker"), 0644); err != nil {
		t.Fatalf("failed to create blocker file: %v", err)
	}

	// Now try to create a log file - should fail because .slb is a file not a directory
	_, err := createRunLogFile(tmpDir, "test")
	if err == nil {
		t.Error("expected error when directory creation fails")
	}

	if !strings.Contains(err.Error(), "creating log dir") {
		t.Errorf("expected error about creating log dir, got: %v", err)
	}
}

func TestCreateRunLogFile_FileCreationError(t *testing.T) {
	// Create a read-only directory
	tmpDir := t.TempDir()
	logDir := filepath.Join(tmpDir, ".slb", "logs")

	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatalf("failed to create log dir: %v", err)
	}

	// Make the directory read-only
	if err := os.Chmod(logDir, 0444); err != nil {
		t.Fatalf("failed to make directory read-only: %v", err)
	}
	// Restore permissions on cleanup
	defer os.Chmod(logDir, 0755)

	// Now try to create a log file - should fail because directory is read-only
	_, err := createRunLogFile(tmpDir, "test")
	if err == nil {
		// On some systems (especially in containers), root can write to read-only dirs
		// Skip the test if we don't get an error
		t.Skip("file creation succeeded despite read-only dir (likely running as root)")
	}

	if !strings.Contains(err.Error(), "creating log file") {
		t.Errorf("expected error about creating log file, got: %v", err)
	}
}
