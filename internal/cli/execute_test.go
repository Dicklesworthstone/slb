package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/spf13/cobra"
)

// newTestExecuteCmd creates a fresh execute command for testing.
func newTestExecuteCmd(dbPath string) *cobra.Command {
	root := &cobra.Command{
		Use:           "slb",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVar(&flagDB, "db", dbPath, "database path")
	root.PersistentFlags().StringVarP(&flagOutput, "output", "o", "text", "output format")
	root.PersistentFlags().BoolVarP(&flagJSON, "json", "j", false, "json output")
	root.PersistentFlags().BoolVarP(&flagTOON, "toon", "t", false, "toon output")
	root.PersistentFlags().StringVarP(&flagProject, "project", "C", "", "project directory")
	root.PersistentFlags().StringVarP(&flagConfig, "config", "c", "", "config file")
	root.PersistentFlags().StringVarP(&flagSessionID, "session-id", "s", "", "session ID")

	// Mirror production: execute declares no local --session-id (the executor
	// session is the root persistent --session-id/-s) and no -t shorthand
	// (-t is the persistent --toon, so --timeout is long-only).
	execCmd := &cobra.Command{
		Use:   "execute <request-id>",
		Short: "Execute an approved request",
		Args:  cobra.ExactArgs(1),
		RunE:  executeCmd.RunE,
	}
	execCmd.Flags().IntVar(&flagExecuteTimeout, "timeout", 300, "timeout seconds")
	execCmd.Flags().BoolVar(&flagExecuteBackground, "background", false, "run in background")
	execCmd.Flags().StringVar(&flagExecuteLogDir, "log-dir", ".slb/logs", "log directory")
	execCmd.Flags().String("expected-command-hash", "", "require the observed command hash")

	root.AddCommand(execCmd)

	return root
}

func resetExecuteFlags() {
	flagDB = ""
	flagOutput = "text"
	flagJSON = false
	flagTOON = false
	flagProject = ""
	flagConfig = ""
	flagSessionID = ""
	flagExecuteTimeout = 300
	flagExecuteBackground = false
	flagExecuteLogDir = ".slb/logs"
}

func TestExecuteCommand_RequiresRequestID(t *testing.T) {
	h := testutil.NewHarness(t)
	resetExecuteFlags()

	cmd := newTestExecuteCmd(h.DBPath)
	_, _, err := executeCommand(cmd, "execute")

	if err == nil {
		t.Fatal("expected error when request ID is missing")
	}
	if !strings.Contains(err.Error(), "accepts 1 arg") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestExecuteCommand_RequiresSessionID(t *testing.T) {
	h := testutil.NewHarness(t)
	resetExecuteFlags()

	cmd := newTestExecuteCmd(h.DBPath)
	_, _, err := executeCommand(cmd, "execute", "some-request-id")

	if err == nil {
		t.Fatal("expected error when --session-id is missing")
	}
	if !strings.Contains(err.Error(), "--session-id is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestExecuteCommand_RequestNotFound(t *testing.T) {
	h := testutil.NewHarness(t)
	resetExecuteFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
	)

	cmd := newTestExecuteCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "execute", "nonexistent-request-id",
		"--session-id", sess.ID,
		"-j",
	)

	if err == nil {
		t.Fatal("expected error for nonexistent request")
	}
}

func TestExecuteCommand_CannotExecutePending(t *testing.T) {
	h := testutil.NewHarness(t)
	resetExecuteFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
	)
	req := testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("echo hello", h.ProjectDir, true),
	)
	// Request is pending by default

	cmd := newTestExecuteCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "execute", req.ID,
		"--session-id", sess.ID,
		"-j",
	)

	if err == nil {
		t.Fatal("expected error when executing pending request")
	}
	if !strings.Contains(err.Error(), "cannot execute") && !strings.Contains(err.Error(), "approved") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestExecuteCommand_ExecutesApprovedRequest(t *testing.T) {
	h := testutil.NewHarness(t)
	resetExecuteFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
	)
	// Use cross-platform 'true' command which should always succeed
	req := testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand(testutil.TruePath(), h.ProjectDir, true),
	)
	// Recompute hash using core.ComputeCommandHash (executor uses this, not db's version)
	req.Command.Hash = db.ComputeCommandHash(req.Command)
	h.DB.Exec(`UPDATE requests SET command_hash = ? WHERE id = ?`, req.Command.Hash, req.ID)
	// Approve the request
	testutil.ApproveRequest(t, h.DB, req)

	cmd := newTestExecuteCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "execute", req.ID,
		"--session-id", sess.ID,
		"-j",
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	// Verify result structure
	if result["request_id"] != req.ID {
		t.Errorf("expected request_id=%s, got %v", req.ID, result["request_id"])
	}
	if result["exit_code"].(float64) != 0 {
		t.Errorf("expected exit_code=0, got %v", result["exit_code"])
	}
	if result["log_path"] == nil || result["log_path"] == "" {
		t.Error("expected log_path to be set")
	}

	// Verify request status was updated
	updated, err := h.DB.GetRequest(req.ID)
	if err != nil {
		t.Fatalf("failed to get request: %v", err)
	}
	if updated.Status != db.StatusExecuted {
		t.Errorf("expected request status=executed, got %s", updated.Status)
	}
}

// TestExecuteCommand_HonorsCustomPattern is the regression guard for issue #7
// Bug 2 on the execute path. execute's CanExecute re-classifies the command
// against the default engine (the "current policy must not require a higher
// tier than approved" gate). It must merge the project's custom patterns first;
// otherwise a project that escalates a command's risk via `slb patterns add`
// after approval is silently bypassed and the stale low-tier approval executes.
//
// Here a request is approved at CAUTION tier for a command matching no builtin.
// A custom CRITICAL pattern is then added for that command. execute must REFUSE
// with a policy-escalation error instead of running. Before the fix the custom
// pattern was invisible to CanExecute, so the gate passed and it executed.
func TestExecuteCommand_HonorsCustomPattern(t *testing.T) {
	h := testutil.NewHarness(t)
	resetExecuteFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
	)
	// Command matches no builtin; approved at the low CAUTION tier.
	req := testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand(testutil.TruePath()+" slbcustompat_execute", h.ProjectDir, true),
		testutil.WithRisk(db.RiskTierCaution),
	)
	req.Command.Hash = db.ComputeCommandHash(req.Command)
	h.DB.Exec(`UPDATE requests SET command_hash = ?, risk_tier = ? WHERE id = ?`, req.Command.Hash, string(db.RiskTierCaution), req.ID)
	testutil.ApproveRequest(t, h.DB, req)

	// Project later escalates this command to CRITICAL via a custom pattern.
	if _, err := h.DB.InsertCustomPattern("critical", "slbcustompat_execute", "test custom pattern", "test"); err != nil {
		t.Fatalf("InsertCustomPattern: %v", err)
	}

	cmd := newTestExecuteCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "execute", req.ID,
		"--session-id", sess.ID,
		"-j",
	)

	if err == nil {
		t.Fatal("custom pattern ignored: execute ran a command the project escalated to CRITICAL after a CAUTION-tier approval")
	}
	if !strings.Contains(err.Error(), "cannot execute") && !strings.Contains(err.Error(), "escalation") && !strings.Contains(err.Error(), "higher tier") {
		t.Errorf("expected a policy-escalation refusal, got: %v", err)
	}

	// The request must NOT have executed.
	updated, gerr := h.DB.GetRequest(req.ID)
	if gerr != nil {
		t.Fatalf("failed to get request: %v", gerr)
	}
	if updated.Status == db.StatusExecuted {
		t.Errorf("request was executed despite the custom-pattern escalation gate")
	}
}

func TestExecuteCommand_Help(t *testing.T) {
	h := testutil.NewHarness(t)
	resetExecuteFlags()

	cmd := newTestExecuteCmd(h.DBPath)
	stdout, _, err := executeCommand(cmd, "execute", "--help")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(stdout, "execute") {
		t.Error("expected help to mention 'execute'")
	}
	if !strings.Contains(stdout, "--session-id") {
		t.Error("expected help to mention '--session-id' flag")
	}
	if !strings.Contains(stdout, "--timeout") {
		t.Error("expected help to mention '--timeout' flag")
	}
	if !strings.Contains(stdout, "--background") {
		t.Error("expected help to mention '--background' flag")
	}
	if !strings.Contains(stdout, "--log-dir") {
		t.Error("expected help to mention '--log-dir' flag")
	}
}

func TestExecuteCommand_CustomTimeout(t *testing.T) {
	h := testutil.NewHarness(t)
	resetExecuteFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
	)
	// Use cross-platform 'true' command for reliable quick execution
	req := testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand(testutil.TruePath(), h.ProjectDir, true),
	)
	// Recompute hash using core.ComputeCommandHash
	req.Command.Hash = db.ComputeCommandHash(req.Command)
	h.DB.Exec(`UPDATE requests SET command_hash = ? WHERE id = ?`, req.Command.Hash, req.ID)
	testutil.ApproveRequest(t, h.DB, req)

	cmd := newTestExecuteCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "execute", req.ID,
		"--session-id", sess.ID,
		"--timeout", "10", // Short timeout (-t is now persistent --toon)
		"-j",
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["exit_code"].(float64) != 0 {
		t.Errorf("expected exit_code=0, got %v", result["exit_code"])
	}
}
