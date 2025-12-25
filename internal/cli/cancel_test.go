package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/spf13/cobra"
)

// newTestCancelCmd creates a fresh cancel command for testing.
func newTestCancelCmd(dbPath string) *cobra.Command {
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

	root.AddCommand(cancelCmd)

	return root
}

func resetCancelFlags() {
	flagDB = ""
	flagOutput = "text"
	flagJSON = false
	flagProject = ""
	flagSessionID = ""
}

func TestCancelCommand_RequiresRequestID(t *testing.T) {
	h := testutil.NewHarness(t)
	resetCancelFlags()

	cmd := newTestCancelCmd(h.DBPath)
	_, _, err := executeCommand(cmd, "cancel")

	if err == nil {
		t.Fatal("expected error when request ID is missing")
	}
	if !strings.Contains(err.Error(), "accepts 1 arg") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCancelCommand_RequiresSessionID(t *testing.T) {
	h := testutil.NewHarness(t)
	resetCancelFlags()

	cmd := newTestCancelCmd(h.DBPath)
	_, _, err := executeCommand(cmd, "cancel", "some-request-id")

	if err == nil {
		t.Fatal("expected error when --session-id is missing")
	}
	if !strings.Contains(err.Error(), "--session-id is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCancelCommand_CancelsRequest(t *testing.T) {
	h := testutil.NewHarness(t)
	resetCancelFlags()

	// Create session and request
	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
	)
	req := testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("rm -rf ./build", h.ProjectDir, true),
		testutil.WithRisk(db.RiskTierDangerous),
	)

	cmd := newTestCancelCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "cancel", req.ID,
		"-s", sess.ID,
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
	if result["request_id"] != req.ID {
		t.Errorf("expected request_id=%s, got %v", req.ID, result["request_id"])
	}
	if result["status"] != "cancelled" {
		t.Errorf("expected status=cancelled, got %v", result["status"])
	}

	// Verify request was actually cancelled in DB
	updated, err := h.DB.GetRequest(req.ID)
	if err != nil {
		t.Fatalf("failed to get request: %v", err)
	}
	if updated.Status != db.StatusCancelled {
		t.Errorf("expected request status=cancelled, got %s", updated.Status)
	}
}

func TestCancelCommand_CannotCancelOthersRequest(t *testing.T) {
	h := testutil.NewHarness(t)
	resetCancelFlags()

	// Create two sessions
	requestorSess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("Requestor"),
	)
	otherSess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("OtherAgent"),
	)

	// Create request from requestor
	req := testutil.MakeRequest(t, h.DB, requestorSess,
		testutil.WithCommand("rm -rf ./build", h.ProjectDir, true),
	)

	// Try to cancel using other session
	cmd := newTestCancelCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "cancel", req.ID,
		"-s", otherSess.ID,
		"-j",
	)

	if err == nil {
		t.Fatal("expected error when trying to cancel another's request")
	}
	if !strings.Contains(err.Error(), "not the requestor") && !strings.Contains(err.Error(), "session mismatch") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCancelCommand_CanCancelApproved(t *testing.T) {
	h := testutil.NewHarness(t)
	resetCancelFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
	)
	req := testutil.MakeRequest(t, h.DB, sess)

	// Mark request as approved (but not yet executing)
	h.DB.UpdateRequestStatus(req.ID, db.StatusApproved)

	cmd := newTestCancelCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "cancel", req.ID,
		"-s", sess.ID,
		"-j",
	)

	if err != nil {
		t.Fatalf("unexpected error: %v (should be able to cancel approved request)", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}
	if result["status"] != "cancelled" {
		t.Errorf("expected status=cancelled, got %v", result["status"])
	}
}

func TestCancelCommand_CannotCancelExecuted(t *testing.T) {
	h := testutil.NewHarness(t)
	resetCancelFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
	)
	req := testutil.MakeRequest(t, h.DB, sess)

	// Mark request as approved, then executing, then executed (terminal state)
	h.DB.UpdateRequestStatus(req.ID, db.StatusApproved)
	h.DB.UpdateRequestStatus(req.ID, db.StatusExecuting)
	h.DB.UpdateRequestStatus(req.ID, db.StatusExecuted)

	cmd := newTestCancelCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "cancel", req.ID,
		"-s", sess.ID,
		"-j",
	)

	if err == nil {
		t.Fatal("expected error when trying to cancel executed request")
	}
	if !strings.Contains(err.Error(), "must be pending or approved") && !strings.Contains(err.Error(), "status is executed") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCancelCommand_RequestNotFound(t *testing.T) {
	h := testutil.NewHarness(t)
	resetCancelFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
	)

	cmd := newTestCancelCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "cancel", "nonexistent-request-id",
		"-s", sess.ID,
		"-j",
	)

	if err == nil {
		t.Fatal("expected error for nonexistent request")
	}
}

func TestCancelCommand_Help(t *testing.T) {
	h := testutil.NewHarness(t)
	resetCancelFlags()

	cmd := newTestCancelCmd(h.DBPath)
	stdout, _, err := executeCommand(cmd, "cancel", "--help")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(stdout, "cancel") {
		t.Error("expected help to mention 'cancel'")
	}
	if !strings.Contains(stdout, "--session-id") {
		t.Error("expected help to mention '--session-id' flag")
	}
	if !strings.Contains(stdout, "pending") {
		t.Error("expected help to mention 'pending' requests")
	}
}
