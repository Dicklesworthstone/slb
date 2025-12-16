package cli

import (
	"encoding/json"
	"strings"
	"testing"

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
