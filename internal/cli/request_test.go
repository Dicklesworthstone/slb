package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/spf13/cobra"
)

// newTestRequestCmd creates a fresh request command for testing.
func newTestRequestCmd(dbPath string) *cobra.Command {
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

	// Create a fresh requestCmd to avoid flag pollution between tests
	reqCmd := &cobra.Command{
		Use:   "request <command>",
		Short: "Create a command approval request",
		Args:  cobra.ExactArgs(1),
		RunE:  requestCmd.RunE,
	}
	reqCmd.Flags().StringVar(&flagRequestReason, "reason", "", "reason/justification")
	reqCmd.Flags().StringVar(&flagRequestExpectedEffect, "expected-effect", "", "expected effect")
	reqCmd.Flags().StringVar(&flagRequestGoal, "goal", "", "goal")
	reqCmd.Flags().StringVar(&flagRequestSafety, "safety", "", "safety argument")
	reqCmd.Flags().StringSliceVar(&flagRequestRedact, "redact", nil, "redact patterns")
	reqCmd.Flags().BoolVar(&flagRequestWait, "wait", false, "wait for decision")
	reqCmd.Flags().BoolVar(&flagRequestExecute, "execute", false, "execute if approved")
	reqCmd.Flags().IntVar(&flagRequestTimeout, "timeout", 300, "timeout seconds")
	reqCmd.Flags().StringSliceVar(&flagRequestAttachFile, "attach-file", nil, "attach files")
	reqCmd.Flags().StringSliceVar(&flagRequestAttachContext, "attach-context", nil, "attach context")
	reqCmd.Flags().StringSliceVar(&flagRequestAttachScreen, "attach-screenshot", nil, "attach screenshots")

	root.AddCommand(reqCmd)

	return root
}

func resetRequestFlags() {
	flagDB = ""
	flagOutput = "text"
	flagJSON = false
	flagProject = ""
	flagSessionID = ""
	flagConfig = ""
	flagRequestReason = ""
	flagRequestExpectedEffect = ""
	flagRequestGoal = ""
	flagRequestSafety = ""
	flagRequestRedact = nil
	flagRequestWait = false
	flagRequestExecute = false
	flagRequestTimeout = 300
	flagRequestAttachFile = nil
	flagRequestAttachContext = nil
	flagRequestAttachScreen = nil
}

func TestRequestCommand_RequiresCommand(t *testing.T) {
	h := testutil.NewHarness(t)
	resetRequestFlags()

	cmd := newTestRequestCmd(h.DBPath)
	_, _, err := executeCommand(cmd, "request")

	if err == nil {
		t.Fatal("expected error when command is missing")
	}
	if !strings.Contains(err.Error(), "accepts 1 arg") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRequestCommand_RequiresSessionID(t *testing.T) {
	h := testutil.NewHarness(t)
	resetRequestFlags()

	cmd := newTestRequestCmd(h.DBPath)
	_, _, err := executeCommand(cmd, "request", "ls -la", "-C", h.ProjectDir)

	if err == nil {
		t.Fatal("expected error when --session-id is missing")
	}
	if !strings.Contains(err.Error(), "--session-id is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRequestCommand_CreatesDangerousRequest(t *testing.T) {
	h := testutil.NewHarness(t)
	resetRequestFlags()

	// Create a session
	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
	)

	cmd := newTestRequestCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "request", "rm -rf ./build",
		"-s", sess.ID,
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

	// Verify result
	if result["request_id"] == nil || result["request_id"] == "" {
		t.Error("expected request_id to be set")
	}
	if result["status"] != string(db.StatusPending) {
		t.Errorf("expected status=pending, got %v", result["status"])
	}
	if result["command"] != "rm -rf ./build" {
		t.Errorf("expected command='rm -rf ./build', got %v", result["command"])
	}
	// Should be dangerous tier
	tier := result["tier"].(string)
	if tier != string(db.RiskTierDangerous) && tier != string(db.RiskTierCritical) {
		t.Errorf("expected tier=dangerous or critical, got %v", tier)
	}
}

func TestRequestCommand_WithJustification(t *testing.T) {
	h := testutil.NewHarness(t)
	resetRequestFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
	)

	cmd := newTestRequestCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "request", "rm -rf ./build",
		"-s", sess.ID,
		"-C", h.ProjectDir,
		"--reason", "Cleaning up old build artifacts",
		"--expected-effect", "Removes ./build directory",
		"--goal", "Free up disk space",
		"--safety", "Only removes local build directory",
		"-j",
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	// Verify request was created
	requestID := result["request_id"].(string)
	if requestID == "" {
		t.Fatal("expected request_id to be set")
	}

	// Verify justification was stored
	req, err := h.DB.GetRequest(requestID)
	if err != nil {
		t.Fatalf("failed to get request: %v", err)
	}
	if req.Justification.Reason != "Cleaning up old build artifacts" {
		t.Errorf("expected reason to be set, got %q", req.Justification.Reason)
	}
	if req.Justification.ExpectedEffect != "Removes ./build directory" {
		t.Errorf("expected expected_effect to be set, got %q", req.Justification.ExpectedEffect)
	}
}

func TestRequestCommand_SafeCommandSkipped(t *testing.T) {
	h := testutil.NewHarness(t)
	resetRequestFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
	)

	cmd := newTestRequestCmd(h.DBPath)
	// "ls" should be classified as safe and skipped
	stdout, err := executeCommandCapture(t, cmd, "request", "ls",
		"-s", sess.ID,
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

	// Safe commands should be skipped
	if result["status"] != "skipped" {
		t.Logf("Full result: %+v", result)
		t.Errorf("expected status=skipped for safe command, got %v", result["status"])
	}
	// Tier should be "safe" (not a db constant since safe commands are skipped)
	tier := result["tier"]
	if tier == nil || tier == "" {
		t.Logf("Full result: %+v", result)
		// Some commands might not have tier in skipped response
		// so we just verify the status is skipped
	} else if tier != "safe" {
		t.Errorf("expected tier=safe, got %v", tier)
	}
}

func TestRequestCommand_ExecuteSkippedCommandRefusesTyped(t *testing.T) {
	h := testutil.NewHarness(t)
	resetRequestFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
	)

	cmd := newTestRequestCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "request", "ls",
		"-s", sess.ID,
		"-C", h.ProjectDir,
		"--execute",
		"-j",
	)

	if err == nil {
		t.Fatal("expected typed refusal when --execute is used on a skipped command")
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["status"] != "refused" {
		t.Errorf("expected status=refused, got %v", result["status"])
	}
	if result["failure_class"] != "skipped_command_not_reviewable" {
		t.Errorf("expected skipped_command_not_reviewable failure_class, got %v", result["failure_class"])
	}
	if actions, ok := result["actual_actions"].([]any); !ok || len(actions) != 0 {
		t.Errorf("expected actual_actions=[], got %#v", result["actual_actions"])
	}
	if result["remediation"] == "" {
		t.Error("expected remediation to be set")
	}
}

func TestRequestCommand_InvalidSession(t *testing.T) {
	h := testutil.NewHarness(t)
	resetRequestFlags()

	cmd := newTestRequestCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "request", "rm -rf ./build",
		"-s", "nonexistent-session-id",
		"-C", h.ProjectDir,
		"-j",
	)

	if err == nil {
		t.Fatal("expected error for nonexistent session")
	}
	// Should fail because session doesn't exist
	if !strings.Contains(err.Error(), "session") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRequestCommand_Help(t *testing.T) {
	h := testutil.NewHarness(t)
	resetRequestFlags()

	cmd := newTestRequestCmd(h.DBPath)
	stdout, _, err := executeCommand(cmd, "request", "--help")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(stdout, "request") {
		t.Error("expected help to mention 'request'")
	}
	if !strings.Contains(stdout, "--session-id") {
		t.Error("expected help to mention '--session-id' flag")
	}
	if !strings.Contains(stdout, "--reason") {
		t.Error("expected help to mention '--reason' flag")
	}
	if !strings.Contains(stdout, "--wait") {
		t.Error("expected help to mention '--wait' flag")
	}
	if !strings.Contains(stdout, "--execute") {
		t.Error("expected help to mention '--execute' flag")
	}
	if !strings.Contains(stdout, "--attach-file") {
		t.Error("expected help to mention '--attach-file' flag")
	}
}

func TestRequestCommand_WithRedaction(t *testing.T) {
	h := testutil.NewHarness(t)
	resetRequestFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
	)

	// Use a dangerous command so it doesn't get skipped
	dangerousCmd := "rm -rf /tmp/secret123"

	cmd := newTestRequestCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "request", dangerousCmd,
		"-s", sess.ID,
		"-C", h.ProjectDir,
		"--redact", "secret123",
		"-j",
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	// Check that redacted version exists if redaction was applied
	if redacted, ok := result["command_redacted"].(string); ok && redacted != "" {
		if strings.Contains(redacted, "secret123") {
			t.Error("expected redacted command to not contain 'secret123'")
		}
	}
}
