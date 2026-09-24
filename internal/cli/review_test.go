package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/spf13/cobra"
)

// newTestReviewCmd creates a fresh review command tree for testing.
func newTestReviewCmd(dbPath string) *cobra.Command {
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

	// Create fresh review commands
	revCmd := &cobra.Command{
		Use:   "review [request-id]",
		Short: "View request details for review",
		Args:  cobra.MaximumNArgs(1),
		RunE:  reviewCmd.RunE,
	}
	revCmd.PersistentFlags().BoolVarP(&flagReviewAll, "all", "a", false, "show requests from all projects")
	revCmd.PersistentFlags().BoolVar(&flagReviewPool, "review-pool", false, "show requests from configured review pool")

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List pending requests awaiting review",
		RunE:  reviewListCmd.RunE,
	}
	listCmd.Flags().BoolVarP(&flagReviewAll, "all", "a", false, "show requests from all projects")

	showCmd := &cobra.Command{
		Use:   "show <request-id>",
		Short: "Show full details of a request",
		Args:  cobra.ExactArgs(1),
		RunE:  reviewShowCmd.RunE,
	}

	revCmd.AddCommand(listCmd, showCmd)
	root.AddCommand(revCmd)

	return root
}

func resetReviewFlags() {
	flagDB = ""
	flagOutput = "text"
	flagJSON = false
	flagProject = ""
	flagConfig = ""
	flagReviewAll = false
	flagReviewPool = false
	// --review-pool resolves the reviewer from the persistent --session-id
	// before the actor (654c244); never inherit either from an earlier test.
	flagSessionID = ""
	flagActor = ""
}

func TestReviewListCommand_ListsPendingRequests(t *testing.T) {
	h := testutil.NewHarness(t)
	resetReviewFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
	)
	testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("rm -rf ./build", h.ProjectDir, true),
		testutil.WithRisk(db.RiskTierDangerous),
	)
	testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("git push --force", h.ProjectDir, true),
		testutil.WithRisk(db.RiskTierCritical),
	)

	cmd := newTestReviewCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "review", "list", "-C", h.ProjectDir, "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result []map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if len(result) < 2 {
		t.Errorf("expected at least 2 pending requests, got %d", len(result))
	}

	// Verify structure
	if len(result) > 0 {
		req := result[0]
		if req["id"] == nil {
			t.Error("expected id to be set")
		}
		if req["command"] == nil {
			t.Error("expected command to be set")
		}
		if req["risk_tier"] == nil {
			t.Error("expected risk_tier to be set")
		}
	}
}

func TestReviewListCommand_EmptyList(t *testing.T) {
	h := testutil.NewHarness(t)
	resetReviewFlags()

	cmd := newTestReviewCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "review", "list", "-C", h.ProjectDir, "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result []map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if len(result) != 0 {
		t.Errorf("expected 0 requests, got %d", len(result))
	}
}

func TestReviewShowCommand_RequiresRequestID(t *testing.T) {
	h := testutil.NewHarness(t)
	resetReviewFlags()

	cmd := newTestReviewCmd(h.DBPath)
	_, _, err := executeCommand(cmd, "review", "show")

	if err == nil {
		t.Fatal("expected error when request ID is missing")
	}
	if !strings.Contains(err.Error(), "accepts 1 arg") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestReviewShowCommand_ShowsRequestDetails(t *testing.T) {
	h := testutil.NewHarness(t)
	resetReviewFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
		testutil.WithModel("test-model"),
	)
	req := testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("rm -rf ./build", h.ProjectDir, true),
		testutil.WithRisk(db.RiskTierDangerous),
	)

	cmd := newTestReviewCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "review", "show", req.ID, "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["id"] != req.ID {
		t.Errorf("expected id=%s, got %v", req.ID, result["id"])
	}
	if result["status"] != string(db.StatusPending) {
		t.Errorf("expected status=pending, got %v", result["status"])
	}
	if result["requestor_agent"] != "TestAgent" {
		t.Errorf("expected requestor_agent=TestAgent, got %v", result["requestor_agent"])
	}
}

func TestReviewShowCommand_RequestNotFound(t *testing.T) {
	h := testutil.NewHarness(t)
	resetReviewFlags()

	cmd := newTestReviewCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "review", "show", "nonexistent-request-id", "-j")

	if err == nil {
		t.Fatal("expected error for nonexistent request")
	}
}

func TestReviewShowCommand_IncludesReviews(t *testing.T) {
	h := testutil.NewHarness(t)
	resetReviewFlags()

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
		testutil.WithCommand("rm -rf ./build", h.ProjectDir, true),
	)

	// Add a review
	review := &db.Review{
		RequestID:         req.ID,
		ReviewerSessionID: reviewerSess.ID,
		ReviewerAgent:     reviewerSess.AgentName,
		ReviewerModel:     reviewerSess.Model,
		Decision:          db.DecisionApprove,
		Comments:          "LGTM",
	}
	if err := h.DB.CreateReview(review); err != nil {
		t.Fatalf("failed to create review: %v", err)
	}

	cmd := newTestReviewCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "review", "show", req.ID, "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	reviews, ok := result["reviews"].([]any)
	if !ok {
		t.Fatal("expected reviews to be an array")
	}
	if len(reviews) != 1 {
		t.Errorf("expected 1 review, got %d", len(reviews))
	}

	if len(reviews) > 0 {
		rv := reviews[0].(map[string]any)
		if rv["reviewer_agent"] != "Reviewer" {
			t.Errorf("expected reviewer_agent=Reviewer, got %v", rv["reviewer_agent"])
		}
		if rv["decision"] != "approve" {
			t.Errorf("expected decision=approve, got %v", rv["decision"])
		}
	}
}

func TestReviewCommand_NoArgs_ShowsList(t *testing.T) {
	h := testutil.NewHarness(t)
	resetReviewFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
	)
	testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("rm -rf ./build", h.ProjectDir, true),
	)

	cmd := newTestReviewCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "review", "-C", h.ProjectDir, "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should return a list of requests
	var result []map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON (should be array): %v\nstdout: %s", err, stdout)
	}

	if len(result) < 1 {
		t.Error("expected at least 1 request in list")
	}
}

func TestReviewCommand_Help(t *testing.T) {
	h := testutil.NewHarness(t)
	resetReviewFlags()

	cmd := newTestReviewCmd(h.DBPath)
	stdout, _, err := executeCommand(cmd, "review", "--help")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(stdout, "review") {
		t.Error("expected help to mention 'review'")
	}
	if !strings.Contains(stdout, "list") {
		t.Error("expected help to mention 'list' subcommand")
	}
	if !strings.Contains(stdout, "show") {
		t.Error("expected help to mention 'show' subcommand")
	}
}

func TestReviewShowCommand_TextOutput(t *testing.T) {
	h := testutil.NewHarness(t)
	resetReviewFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
		testutil.WithModel("test-model"),
	)
	req := testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("rm -rf ./build", h.ProjectDir, true),
		testutil.WithRisk(db.RiskTierDangerous),
	)

	cmd := newTestReviewCmd(h.DBPath)
	// No -j flag, so text output
	stdout, err := executeCommandCapture(t, cmd, "review", "show", req.ID)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Text output should contain the request ID and status
	if !strings.Contains(stdout, req.ID) {
		t.Error("expected text output to contain request ID")
	}
}

func TestReviewShowCommand_WithRejection(t *testing.T) {
	h := testutil.NewHarness(t)
	resetReviewFlags()

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
		testutil.WithCommand("rm -rf /", h.ProjectDir, true),
	)

	// Add a rejection review
	review := &db.Review{
		RequestID:         req.ID,
		ReviewerSessionID: reviewerSess.ID,
		ReviewerAgent:     reviewerSess.AgentName,
		ReviewerModel:     reviewerSess.Model,
		Decision:          db.DecisionReject,
		Comments:          "Too dangerous",
	}
	if err := h.DB.CreateReview(review); err != nil {
		t.Fatalf("failed to create review: %v", err)
	}

	cmd := newTestReviewCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "review", "show", req.ID, "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["current_rejections"].(float64) != 1 {
		t.Errorf("expected current_rejections=1, got %v", result["current_rejections"])
	}
}

func TestReviewListCommand_AllProjects(t *testing.T) {
	h := testutil.NewHarness(t)
	resetReviewFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
	)
	testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("rm -rf ./build", h.ProjectDir, true),
	)

	cmd := newTestReviewCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "review", "list", "--all", "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result []map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	// Should find the request when using --all
	if len(result) < 1 {
		t.Error("expected at least 1 request with --all flag")
	}
}

func TestReviewShowCommand_WithJustificationFields(t *testing.T) {
	h := testutil.NewHarness(t)
	resetReviewFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
		testutil.WithModel("test-model"),
	)
	req := testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("rm -rf ./build", h.ProjectDir, true),
		testutil.WithJustification("Need to clean build", "Removes stale files", "Clean build", "Build dir only"),
		testutil.WithRisk(db.RiskTierDangerous),
	)

	cmd := newTestReviewCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "review", "show", req.ID)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Text output should contain justification fields
	if !strings.Contains(stdout, "Reason:") {
		t.Error("expected text output to contain 'Reason:'")
	}
	if !strings.Contains(stdout, "Expected Effect:") {
		t.Error("expected text output to contain 'Expected Effect:'")
	}
	if !strings.Contains(stdout, "Goal:") {
		t.Error("expected text output to contain 'Goal:'")
	}
}

func TestReviewShowCommand_WithMultipleReviews(t *testing.T) {
	h := testutil.NewHarness(t)
	resetReviewFlags()

	requestorSess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("Requestor"),
		testutil.WithModel("model-a"),
	)
	reviewer1Sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("Reviewer1"),
		testutil.WithModel("model-b"),
	)
	reviewer2Sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("Reviewer2"),
		testutil.WithModel("model-c"),
	)

	req := testutil.MakeRequest(t, h.DB, requestorSess,
		testutil.WithCommand("rm -rf ./build", h.ProjectDir, true),
	)

	// Add two reviews
	review1 := &db.Review{
		RequestID:         req.ID,
		ReviewerSessionID: reviewer1Sess.ID,
		ReviewerAgent:     reviewer1Sess.AgentName,
		ReviewerModel:     reviewer1Sess.Model,
		Decision:          db.DecisionApprove,
		Comments:          "Approved by reviewer 1",
	}
	if err := h.DB.CreateReview(review1); err != nil {
		t.Fatalf("failed to create review1: %v", err)
	}

	review2 := &db.Review{
		RequestID:         req.ID,
		ReviewerSessionID: reviewer2Sess.ID,
		ReviewerAgent:     reviewer2Sess.AgentName,
		ReviewerModel:     reviewer2Sess.Model,
		Decision:          db.DecisionApprove,
		Comments:          "Approved by reviewer 2",
	}
	if err := h.DB.CreateReview(review2); err != nil {
		t.Fatalf("failed to create review2: %v", err)
	}

	cmd := newTestReviewCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "review", "show", req.ID, "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	reviews, ok := result["reviews"].([]any)
	if !ok {
		t.Fatal("expected reviews to be an array")
	}
	if len(reviews) != 2 {
		t.Errorf("expected 2 reviews, got %d", len(reviews))
	}
	if result["current_approvals"].(float64) != 2 {
		t.Errorf("expected current_approvals=2, got %v", result["current_approvals"])
	}
}

// TestReviewShowCommand_TextOutputWithReviews tests text output when reviews exist.
func TestReviewShowCommand_TextOutputWithReviews(t *testing.T) {
	h := testutil.NewHarness(t)
	resetReviewFlags()

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
		testutil.WithCommand("rm -rf ./build", h.ProjectDir, true),
	)

	// Add a review with comment
	review := &db.Review{
		RequestID:         req.ID,
		ReviewerSessionID: reviewerSess.ID,
		ReviewerAgent:     reviewerSess.AgentName,
		ReviewerModel:     reviewerSess.Model,
		Decision:          db.DecisionApprove,
		Comments:          "Looks good to me",
	}
	if err := h.DB.CreateReview(review); err != nil {
		t.Fatalf("failed to create review: %v", err)
	}

	cmd := newTestReviewCmd(h.DBPath)
	// Text output (no -j flag)
	stdout, err := executeCommandCapture(t, cmd, "review", "show", req.ID)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Text output should contain reviews section
	if !strings.Contains(stdout, "Reviews:") {
		t.Error("expected text output to contain 'Reviews:'")
	}
	if !strings.Contains(stdout, "APPROVE") {
		t.Error("expected text output to contain 'APPROVE'")
	}
	if !strings.Contains(stdout, "Looks good to me") {
		t.Error("expected text output to contain review comment")
	}
}

// TestReviewShowCommand_TextOutputWithRejection tests text output with rejection count.
func TestReviewShowCommand_TextOutputWithRejection(t *testing.T) {
	h := testutil.NewHarness(t)
	resetReviewFlags()

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
		testutil.WithCommand("rm -rf /", h.ProjectDir, true),
	)

	// Add a rejection review
	review := &db.Review{
		RequestID:         req.ID,
		ReviewerSessionID: reviewerSess.ID,
		ReviewerAgent:     reviewerSess.AgentName,
		ReviewerModel:     reviewerSess.Model,
		Decision:          db.DecisionReject,
		Comments:          "Too dangerous",
	}
	if err := h.DB.CreateReview(review); err != nil {
		t.Fatalf("failed to create review: %v", err)
	}

	cmd := newTestReviewCmd(h.DBPath)
	// Text output (no -j flag)
	stdout, err := executeCommandCapture(t, cmd, "review", "show", req.ID)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Text output should contain rejections count
	if !strings.Contains(stdout, "Rejections:") {
		t.Error("expected text output to contain 'Rejections:'")
	}
}

// TestReviewShowCommand_TextOutputWithDryRun tests text output with dry run info.
func TestReviewShowCommand_TextOutputWithDryRun(t *testing.T) {
	h := testutil.NewHarness(t)
	resetReviewFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
		testutil.WithModel("test-model"),
	)
	req := testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("rm -rf ./build", h.ProjectDir, true),
		testutil.WithDryRun("rm -rf --dry-run ./build", "Would remove: build/\n  file1.o\n  file2.o"),
	)

	cmd := newTestReviewCmd(h.DBPath)
	// Text output (no -j flag)
	stdout, err := executeCommandCapture(t, cmd, "review", "show", req.ID)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Text output should contain dry run section
	if !strings.Contains(stdout, "Dry Run:") {
		t.Error("expected text output to contain 'Dry Run:'")
	}
	if !strings.Contains(stdout, "Would remove") {
		t.Error("expected text output to contain dry run output")
	}
}

// TestReviewShowCommand_TextOutputWithRequireDifferentModel tests text output with model requirement note.
func TestReviewShowCommand_TextOutputWithRequireDifferentModel(t *testing.T) {
	h := testutil.NewHarness(t)
	resetReviewFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
		testutil.WithModel("test-model"),
	)
	req := testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("rm -rf ./build", h.ProjectDir, true),
		testutil.WithRequireDifferentModel(true),
	)

	cmd := newTestReviewCmd(h.DBPath)
	// Text output (no -j flag)
	stdout, err := executeCommandCapture(t, cmd, "review", "show", req.ID)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Text output should contain the model requirement note
	if !strings.Contains(stdout, "different model") {
		t.Error("expected text output to contain 'different model' note")
	}
}

// TestReviewShowCommand_TextOutputWithExpiresAt tests text output includes expiry.
func TestReviewShowCommand_TextOutputWithExpiresAt(t *testing.T) {
	h := testutil.NewHarness(t)
	resetReviewFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
		testutil.WithModel("test-model"),
	)
	// MakeRequest sets ExpiresAt by default
	req := testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("rm -rf ./build", h.ProjectDir, true),
	)

	cmd := newTestReviewCmd(h.DBPath)
	// Text output (no -j flag)
	stdout, err := executeCommandCapture(t, cmd, "review", "show", req.ID)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Text output should contain expiry
	if !strings.Contains(stdout, "Expires:") {
		t.Error("expected text output to contain 'Expires:'")
	}
}

// TestReviewShowCommand_TextOutputWithSafetyArgument tests text output with safety argument.
func TestReviewShowCommand_TextOutputWithSafetyArgument(t *testing.T) {
	h := testutil.NewHarness(t)
	resetReviewFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
		testutil.WithModel("test-model"),
	)
	req := testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("rm -rf ./build", h.ProjectDir, true),
		testutil.WithJustification("Need to clean", "Removes files", "Clean build", "Only build directory, not source"),
	)

	cmd := newTestReviewCmd(h.DBPath)
	// Text output (no -j flag)
	stdout, err := executeCommandCapture(t, cmd, "review", "show", req.ID)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Text output should contain safety argument
	if !strings.Contains(stdout, "Safety Argument:") {
		t.Error("expected text output to contain 'Safety Argument:'")
	}
}

func TestReviewListCommand_ReviewPoolUsesAgentAllowlist(t *testing.T) {
	h := testutil.NewHarness(t)
	resetReviewFlags()
	t.Setenv("SLB_ACTOR", "Reviewer")

	localRequestor := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir), testutil.WithAgent("LocalRequestor"))
	local := testutil.MakeRequest(t, h.DB, localRequestor)

	allowedProject := t.TempDir()
	if err := os.MkdirAll(filepath.Join(allowedProject, ".slb"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(allowedProject, ".slb", "config.toml"),
		[]byte("[general]\ncross_project_reviews = true\nreview_pool = ['Reviewer']\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	allowedRequestor := testutil.MakeSession(t, h.DB,
		testutil.WithProject(allowedProject), testutil.WithAgent("AllowedRequestor"))
	allowed := testutil.MakeRequest(t, h.DB, allowedRequestor)

	deniedProject := t.TempDir()
	if err := os.MkdirAll(filepath.Join(deniedProject, ".slb"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deniedProject, ".slb", "config.toml"),
		[]byte("[general]\ncross_project_reviews = true\nreview_pool = ['AnotherReviewer']\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deniedRequestor := testutil.MakeSession(t, h.DB,
		testutil.WithProject(deniedProject), testutil.WithAgent("DeniedRequestor"))
	denied := testutil.MakeRequest(t, h.DB, deniedRequestor)

	cmd := newTestReviewCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "review", "list",
		"-C", h.ProjectDir, "--review-pool", "-j")
	if err != nil {
		t.Fatal(err)
	}
	var result []map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, item := range result {
		seen[item["id"].(string)] = true
		if item["project_path"] == nil {
			t.Fatalf("review-pool result omitted target project: %+v", item)
		}
	}
	if !seen[local.ID] || !seen[allowed.ID] || seen[denied.ID] || len(seen) != 2 {
		t.Fatalf("review-pool agent filtering wrong: seen=%v", seen)
	}
}
