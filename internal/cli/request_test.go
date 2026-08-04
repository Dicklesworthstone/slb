package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/core"
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
	// Since GH #9, only an explicit SAFE pattern match skips approval; use a
	// command matching a built-in safe pattern instead of relying on the
	// removed unmatched-command skip.
	stdout, err := executeCommandCapture(t, cmd, "request", "rm build-output.log",
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

// TestRequestCommand_HonorsCustomPattern is the regression guard for issue #7
// Bug 2: `request`'s classification path must merge the project's custom
// patterns into the default engine, not classify against builtins only.
//
// The command `echo slbcustompat_request` matches no builtin pattern (builtins
// classify it as an unmatched/safe command that `request` would SKIP). After a
// custom DANGEROUS pattern is added for it, `request` must instead create a
// real pending DANGEROUS request. Before the fix the load call was missing, so
// the custom pattern was invisible and the command was skipped.
func TestRequestCommand_HonorsCustomPattern(t *testing.T) {
	h := testutil.NewHarness(t)
	resetRequestFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
	)

	// Persist a custom DANGEROUS pattern matching an otherwise-unmatched command.
	const customCmd = "echo slbcustompat_request"
	if _, err := h.DB.InsertCustomPattern("dangerous", "slbcustompat_request", "test custom pattern", "test"); err != nil {
		t.Fatalf("InsertCustomPattern: %v", err)
	}

	cmd := newTestRequestCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "request", customCmd,
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

	// With the custom pattern honored, the command must NOT be skipped — it
	// must produce a pending DANGEROUS request.
	if result["status"] == "skipped" {
		t.Fatalf("custom pattern ignored: command was skipped (classified against builtins only); result=%+v", result)
	}
	if result["status"] != string(db.StatusPending) {
		t.Errorf("expected status=pending, got %v (result=%+v)", result["status"], result)
	}
	if result["tier"] != string(db.RiskTierDangerous) {
		t.Errorf("expected tier=dangerous from custom pattern, got %v", result["tier"])
	}
	if result["request_id"] == nil || result["request_id"] == "" {
		t.Error("expected a request_id (a real request was created)")
	}
}

// TestSkippedRequestResponse_NilClassification is the regression guard for
// issue #7 Bug 3: the skipped-request rendering used to dereference
// result.Classification.Tier unconditionally. Classification is a pointer, so a
// skipped result with a nil Classification panicked. The rendering now guards
// the nil and omits the "tier" field rather than crashing.
func TestSkippedRequestResponse_NilClassification(t *testing.T) {
	// Must not panic on a nil Classification.
	resp := skippedRequestResponse(&core.CreateRequestResult{
		Skipped:        true,
		SkipReason:     "Command is classified as safe and does not require approval",
		Classification: nil,
	}, "ls -la")

	if resp["status"] != "skipped" {
		t.Errorf("expected status=skipped, got %v", resp["status"])
	}
	if resp["command"] != "ls -la" {
		t.Errorf("expected command to be echoed, got %v", resp["command"])
	}
	if _, ok := resp["tier"]; ok {
		t.Errorf("expected tier to be omitted when Classification is nil, got %v", resp["tier"])
	}

	// And the present-classification path still emits the tier.
	resp2 := skippedRequestResponse(&core.CreateRequestResult{
		Skipped:        true,
		SkipReason:     "safe",
		Classification: &core.MatchResult{Tier: core.RiskTier(core.RiskSafe)},
	}, "ls")
	if resp2["tier"] == nil {
		t.Error("expected tier to be present when Classification is set")
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
