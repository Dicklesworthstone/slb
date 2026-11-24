package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/spf13/cobra"
)

// newTestStatusCmd creates a fresh status command for testing.
func newTestStatusCmd(dbPath string) *cobra.Command {
	root := &cobra.Command{
		Use:           "slb",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVar(&flagDB, "db", dbPath, "database path")
	root.PersistentFlags().StringVarP(&flagOutput, "output", "o", "text", "output format")
	root.PersistentFlags().BoolVarP(&flagJSON, "json", "j", false, "json output")
	root.PersistentFlags().StringVarP(&flagProject, "project", "C", "", "project directory")

	root.AddCommand(statusCmd)

	return root
}

func resetStatusFlags() {
	flagDB = ""
	flagOutput = "text"
	flagJSON = false
	flagProject = ""
	flagStatusWait = false
	if f := statusCmd.Flags().Lookup("help"); f != nil {
		_ = f.Value.Set("false")
		f.Changed = false
	}
}

func TestStatusCommand_RequiresRequestID(t *testing.T) {
	h := testutil.NewHarness(t)
	resetStatusFlags()

	cmd := newTestStatusCmd(h.DBPath)
	_, _, err := executeCommand(cmd, "status")

	if err == nil {
		t.Fatal("expected error when request ID is missing")
	}
	if !strings.Contains(err.Error(), "accepts 1 arg") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestStatusCommand_ShowsRequestStatus(t *testing.T) {
	h := testutil.NewHarness(t)
	resetStatusFlags()

	// Create a session and request
	sess := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir))
	req := testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("rm -rf ./build", h.ProjectDir, true),
		testutil.WithRisk(db.RiskTierDangerous),
	)

	cmd := newTestStatusCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "status", req.ID, "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	// Verify required fields
	if result["request_id"] != req.ID {
		t.Errorf("expected request_id=%s, got %v", req.ID, result["request_id"])
	}
	if result["status"] != string(db.StatusPending) {
		t.Errorf("expected status=pending, got %v", result["status"])
	}
	if result["risk_tier"] != string(db.RiskTierDangerous) {
		t.Errorf("expected risk_tier=dangerous, got %v", result["risk_tier"])
	}
	if result["command"] != "rm -rf ./build" {
		t.Errorf("expected command='rm -rf ./build', got %v", result["command"])
	}
	if result["requestor_agent"] != sess.AgentName {
		t.Errorf("expected requestor_agent=%s, got %v", sess.AgentName, result["requestor_agent"])
	}
}

func TestStatusCommand_ShowsReviews(t *testing.T) {
	h := testutil.NewHarness(t)
	resetStatusFlags()

	// Create sessions and request
	requestorSess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("Requestor"),
		testutil.WithModel("model-a"),
	)
	reviewerSess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("Reviewer"),
		testutil.WithModel("model-b"),
	)

	req := testutil.MakeRequest(t, h.DB, requestorSess,
		testutil.WithCommand("git push --force", h.ProjectDir, true),
		testutil.WithRisk(db.RiskTierDangerous),
	)

	// Add a review
	review := &db.Review{
		RequestID:         req.ID,
		ReviewerSessionID: reviewerSess.ID,
		ReviewerAgent:     reviewerSess.AgentName,
		ReviewerModel:     reviewerSess.Model,
		Decision:          db.DecisionApprove,
		Comments:          "Looks safe",
	}
	if err := h.DB.CreateReview(review); err != nil {
		t.Fatalf("failed to create review: %v", err)
	}

	cmd := newTestStatusCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "status", req.ID, "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	// Verify approval count
	if result["approval_count"].(float64) != 1 {
		t.Errorf("expected approval_count=1, got %v", result["approval_count"])
	}

	// Verify reviews array
	reviews, ok := result["reviews"].([]any)
	if !ok {
		t.Fatal("expected reviews to be an array")
	}
	if len(reviews) != 1 {
		t.Errorf("expected 1 review, got %d", len(reviews))
	}

	if len(reviews) > 0 {
		rv := reviews[0].(map[string]any)
		if rv["reviewer"] != "Reviewer" {
			t.Errorf("expected reviewer=Reviewer, got %v", rv["reviewer"])
		}
		if rv["decision"] != "approve" {
			t.Errorf("expected decision=approve, got %v", rv["decision"])
		}
		if rv["comments"] != "Looks safe" {
			t.Errorf("expected comments='Looks safe', got %v", rv["comments"])
		}
	}
}

func TestStatusCommand_NotFound(t *testing.T) {
	h := testutil.NewHarness(t)
	resetStatusFlags()

	cmd := newTestStatusCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "status", "nonexistent-request-id", "-j")

	if err == nil {
		t.Fatal("expected error for nonexistent request")
	}
	if !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "getting request") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestStatusCommand_Help(t *testing.T) {
	h := testutil.NewHarness(t)
	resetStatusFlags()

	cmd := newTestStatusCmd(h.DBPath)
	stdout, _, err := executeCommand(cmd, "status", "--help")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(stdout, "status") {
		t.Error("expected help to mention 'status'")
	}
	if !strings.Contains(stdout, "--wait") {
		t.Error("expected help to mention '--wait' flag")
	}
}

func TestStatusCommand_WaitUntilApprovedAndCancel(t *testing.T) {
	h := testutil.NewHarness(t)
	t.Chdir(h.ProjectDir)
	resetStatusFlags()
	t.Cleanup(func() {
		statusCmd.SetContext(nil)
		resetStatusFlags()
	})

	sess := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir), testutil.WithAgent("StatusWaitAgent"))
	req := testutil.MakeRequest(t, h.DB, sess, testutil.WithCommand("chmod -R 777 ./wait-status-target", h.ProjectDir, true))

	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	statusCmd.SetContext(cancelCtx)
	cmdCancel := newTestStatusCmd(h.DBPath)
	cmdCancel.SetArgs([]string{"status", req.ID, "--wait", "-j"})
	var cancelErr error
	_ = captureStdout(t, func() {
		cancelErr = cmdCancel.ExecuteContext(cancelCtx)
	})
	if cancelErr == nil || !strings.Contains(cancelErr.Error(), "waiting for request") {
		t.Fatalf("expected waiting error on cancelled context, got: %v", cancelErr)
	}

	resetStatusFlags()
	go func() {
		time.Sleep(80 * time.Millisecond)
		db2, err := db.Open(h.DBPath)
		if err != nil {
			return
		}
		defer db2.Close()
		_ = db2.UpdateRequestStatus(req.ID, db.StatusApproved)
	}()

	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	statusCmd.SetContext(ctx)
	cmd := newTestStatusCmd(h.DBPath)
	cmd.SetArgs([]string{"status", req.ID, "--wait", "-j"})
	var err error
	out := captureStdout(t, func() {
		err = cmd.ExecuteContext(ctx)
	})
	if err != nil {
		t.Fatalf("status --wait failed: %v\n%s", err, out)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if payload["status"] != "approved" {
		t.Fatalf("status --wait status=%v, want approved", payload["status"])
	}
}

func TestStatusCommand_RejectedWithTimestamps(t *testing.T) {
	h := testutil.NewHarness(t)
	t.Chdir(h.ProjectDir)
	resetStatusFlags()
	t.Cleanup(func() {
		statusCmd.SetContext(nil)
		resetStatusFlags()
	})

	reqSess := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir), testutil.WithAgent("Req"), testutil.WithModel("model-a"))
	revSess := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir), testutil.WithAgent("Rev"), testutil.WithModel("model-b"))
	req := testutil.MakeRequest(t, h.DB, reqSess, testutil.WithCommand("chmod -R 777 ./rej-status", h.ProjectDir, true))
	now := time.Now().UTC()
	exp := now.Add(time.Hour)
	if _, err := h.DB.Exec(`UPDATE requests SET status = 'rejected', resolved_at = ?, approval_expires_at = ? WHERE id = ?`,
		now.Format(time.RFC3339), exp.Format(time.RFC3339), req.ID); err != nil {
		t.Fatal(err)
	}
	rev := &db.Review{
		RequestID: req.ID, ReviewerSessionID: revSess.ID, ReviewerAgent: revSess.AgentName, ReviewerModel: revSess.Model,
		Decision: db.DecisionReject, Comments: "unsafe", SignatureTimestamp: now,
		Signature: db.ComputeReviewSignature(revSess.SessionKey, req.ID, db.DecisionReject, now),
	}
	if err := h.DB.CreateReview(rev); err != nil {
		t.Fatal(err)
	}

	cmd := newTestStatusCmd(h.DBPath)
	out, err := executeCommandCapture(t, cmd, "status", req.ID, "-j")
	if err != nil {
		t.Fatalf("status rejected failed: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid status JSON: %v\n%s", err, out)
	}
	if payload["rejection_count"].(float64) != 1 || payload["resolved_at"] == "" || payload["approval_expires_at"] == "" {
		t.Fatalf("expected rejection_count=1 and timestamps, got: %+v", payload)
	}
}

func TestStatusCommand_InvalidDatabaseError(t *testing.T) {
	resetStatusFlags()
	t.Cleanup(resetStatusFlags)
	dirAsDB := t.TempDir()
	cmd := newTestStatusCmd(dirAsDB)
	if _, _, err := executeCommand(cmd, "status", "req-any", "-j"); err == nil || !strings.Contains(err.Error(), "opening database") {
		t.Fatalf("expected opening database error for directory DB path, got: %v", err)
	}
}
