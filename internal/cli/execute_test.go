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
	root.PersistentFlags().StringVarP(&flagProject, "project", "C", "", "project directory")
	root.PersistentFlags().StringVarP(&flagConfig, "config", "c", "", "config file")

	// Create a fresh executeCmd
	execCmd := &cobra.Command{
		Use:   "execute <request-id>",
		Short: "Execute an approved request",
		Args:  cobra.ExactArgs(1),
		RunE:  executeCmd.RunE,
	}
	execCmd.Flags().StringVar(&flagExecuteSessionID, "session-id", "", "executor session ID")
	execCmd.Flags().IntVar(&flagExecuteTimeout, "timeout", 300, "timeout seconds")
	execCmd.Flags().BoolVar(&flagExecuteBackground, "background", false, "run in background")
	execCmd.Flags().StringVar(&flagExecuteLogDir, "log-dir", ".slb/logs", "log directory")

	root.AddCommand(execCmd)

	return root
}

func resetExecuteFlags() {
	flagDB = ""
	flagOutput = "text"
	flagJSON = false
	flagProject = ""
	flagConfig = ""
	flagExecuteSessionID = ""
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
	h.DB.UpdateRequestStatus(req.ID, db.StatusApproved)

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
	h.DB.UpdateRequestStatus(req.ID, db.StatusApproved)

	cmd := newTestExecuteCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "execute", req.ID,
		"--session-id", sess.ID,
		"--timeout", "10",
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
