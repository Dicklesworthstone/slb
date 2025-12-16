package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/spf13/cobra"
)

// newTestHistoryCmd creates a fresh history command for testing.
func newTestHistoryCmd(dbPath string) *cobra.Command {
	root := &cobra.Command{
		Use:           "slb",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVar(&flagDB, "db", dbPath, "database path")
	root.PersistentFlags().StringVarP(&flagOutput, "output", "o", "text", "output format")
	root.PersistentFlags().BoolVarP(&flagJSON, "json", "j", false, "json output")
	root.PersistentFlags().StringVarP(&flagProject, "project", "C", "", "project directory")

	// Create a fresh historyCmd to avoid flag pollution between tests
	histCmd := &cobra.Command{
		Use:   "history",
		Short: "Browse and search request history",
		RunE:  historyCmd.RunE,
	}
	histCmd.Flags().StringVarP(&flagHistoryQuery, "query", "q", "", "full-text search query")
	histCmd.Flags().StringVar(&flagHistoryStatus, "status", "", "filter by status")
	histCmd.Flags().StringVar(&flagHistoryAgent, "agent", "", "filter by agent")
	histCmd.Flags().StringVar(&flagHistoryTier, "tier", "", "filter by risk tier")
	histCmd.Flags().StringVar(&flagHistorySince, "since", "", "filter by date")
	histCmd.Flags().IntVar(&flagHistoryLimit, "limit", 50, "max results")

	root.AddCommand(histCmd)

	return root
}

func resetHistoryFlags() {
	flagDB = ""
	flagOutput = "text"
	flagJSON = false
	flagProject = ""
	flagHistoryQuery = ""
	flagHistoryStatus = ""
	flagHistoryAgent = ""
	flagHistoryTier = ""
	flagHistorySince = ""
	flagHistoryLimit = 50
}

func TestHistoryCommand_ListsRequests(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHistoryFlags()

	// Create session and requests
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

	cmd := newTestHistoryCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "history", "-C", h.ProjectDir, "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result []map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if len(result) < 2 {
		t.Errorf("expected at least 2 requests, got %d", len(result))
	}

	// Verify structure
	if len(result) > 0 {
		req := result[0]
		if req["request_id"] == nil {
			t.Error("expected request_id to be set")
		}
		if req["command"] == nil {
			t.Error("expected command to be set")
		}
		if req["risk_tier"] == nil {
			t.Error("expected risk_tier to be set")
		}
		if req["status"] == nil {
			t.Error("expected status to be set")
		}
	}
}

func TestHistoryCommand_EmptyList(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHistoryFlags()

	cmd := newTestHistoryCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "history", "-C", h.ProjectDir, "-j")

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

func TestHistoryCommand_FilterByStatus(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHistoryFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
	)

	// Create a pending request
	testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("rm -rf ./build", h.ProjectDir, true),
	)

	// Create an approved request
	approvedReq := testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("git push", h.ProjectDir, true),
	)
	h.DB.UpdateRequestStatus(approvedReq.ID, db.StatusApproved)

	cmd := newTestHistoryCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "history",
		"-C", h.ProjectDir,
		"--status", "approved",
		"-j",
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result []map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	// Should only contain approved requests
	for _, req := range result {
		if req["status"] != "approved" {
			t.Errorf("expected status=approved, got %v", req["status"])
		}
	}
}

func TestHistoryCommand_FilterByAgent(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHistoryFlags()

	// Create two agents
	sess1 := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("Agent1"),
	)
	sess2 := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("Agent2"),
	)

	testutil.MakeRequest(t, h.DB, sess1,
		testutil.WithCommand("rm -rf ./build", h.ProjectDir, true),
	)
	testutil.MakeRequest(t, h.DB, sess2,
		testutil.WithCommand("git push", h.ProjectDir, true),
	)

	cmd := newTestHistoryCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "history",
		"-C", h.ProjectDir,
		"--agent", "Agent1",
		"-j",
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result []map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	// Should only contain Agent1's requests
	for _, req := range result {
		if req["requestor_agent"] != "Agent1" {
			t.Errorf("expected requestor_agent=Agent1, got %v", req["requestor_agent"])
		}
	}
}

func TestHistoryCommand_FilterByTier(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHistoryFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
	)

	testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("rm -rf ./build", h.ProjectDir, true),
		testutil.WithRisk(db.RiskTierDangerous),
	)
	testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("git push --force origin main", h.ProjectDir, true),
		testutil.WithRisk(db.RiskTierCritical),
	)

	cmd := newTestHistoryCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "history",
		"-C", h.ProjectDir,
		"--tier", "critical",
		"-j",
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result []map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	// Should only contain critical tier requests
	for _, req := range result {
		if req["risk_tier"] != "critical" {
			t.Errorf("expected risk_tier=critical, got %v", req["risk_tier"])
		}
	}
}

func TestHistoryCommand_LimitResults(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHistoryFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
	)

	// Create multiple requests
	for i := 0; i < 10; i++ {
		testutil.MakeRequest(t, h.DB, sess,
			testutil.WithCommand("echo test", h.ProjectDir, true),
		)
	}

	cmd := newTestHistoryCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "history",
		"-C", h.ProjectDir,
		"--limit", "3",
		"-j",
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result []map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if len(result) > 3 {
		t.Errorf("expected at most 3 results with limit=3, got %d", len(result))
	}
}

func TestHistoryCommand_Help(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHistoryFlags()

	cmd := newTestHistoryCmd(h.DBPath)
	stdout, _, err := executeCommand(cmd, "history", "--help")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(stdout, "history") {
		t.Error("expected help to mention 'history'")
	}
	if !strings.Contains(stdout, "--query") {
		t.Error("expected help to mention '--query' flag")
	}
	if !strings.Contains(stdout, "--status") {
		t.Error("expected help to mention '--status' flag")
	}
	if !strings.Contains(stdout, "--agent") {
		t.Error("expected help to mention '--agent' flag")
	}
	if !strings.Contains(stdout, "--limit") {
		t.Error("expected help to mention '--limit' flag")
	}
}

func TestApplyHistoryFilters(t *testing.T) {
	// Test the in-memory filtering function
	requests := []*db.Request{
		{ID: "1", Status: db.StatusPending, RequestorAgent: "Agent1", RiskTier: db.RiskTierDangerous},
		{ID: "2", Status: db.StatusApproved, RequestorAgent: "Agent2", RiskTier: db.RiskTierCritical},
		{ID: "3", Status: db.StatusRejected, RequestorAgent: "Agent1", RiskTier: db.RiskTierCritical},
	}

	tests := []struct {
		name     string
		status   string
		agent    string
		tier     string
		expected int
	}{
		{"no filters", "", "", "", 3},
		{"filter by status", "pending", "", "", 1},
		{"filter by agent", "", "Agent1", "", 2},
		{"filter by tier", "", "", "critical", 2},
		{"multiple filters", "approved", "Agent2", "critical", 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flagHistoryStatus = tt.status
			flagHistoryAgent = tt.agent
			flagHistoryTier = tt.tier
			flagHistorySince = ""

			result := applyHistoryFilters(requests)
			if len(result) != tt.expected {
				t.Errorf("expected %d results, got %d", tt.expected, len(result))
			}
		})
	}

	// Reset flags
	resetHistoryFlags()
}

func TestApplyHistoryFilters_SinceDateRFC3339(t *testing.T) {
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	recent := now.Add(-1 * time.Hour)

	requests := []*db.Request{
		{ID: "1", Status: db.StatusPending, CreatedAt: old},
		{ID: "2", Status: db.StatusPending, CreatedAt: recent},
	}

	// Filter to only recent (since 24 hours ago)
	flagHistoryStatus = ""
	flagHistoryAgent = ""
	flagHistoryTier = ""
	flagHistorySince = now.Add(-24 * time.Hour).Format(time.RFC3339)

	result := applyHistoryFilters(requests)
	if len(result) != 1 {
		t.Errorf("expected 1 result with since filter, got %d", len(result))
	}
	if len(result) > 0 && result[0].ID != "2" {
		t.Errorf("expected request 2, got %s", result[0].ID)
	}

	resetHistoryFlags()
}

func TestApplyHistoryFilters_SinceDateOnly(t *testing.T) {
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	recent := now.Add(-1 * time.Hour)

	requests := []*db.Request{
		{ID: "1", Status: db.StatusPending, CreatedAt: old},
		{ID: "2", Status: db.StatusPending, CreatedAt: recent},
	}

	// Filter using date-only format
	flagHistoryStatus = ""
	flagHistoryAgent = ""
	flagHistoryTier = ""
	flagHistorySince = now.Format("2006-01-02")

	result := applyHistoryFilters(requests)
	// Both should pass since they're on or after today's date
	// Actually, the old one is 2 days ago so it should fail
	if len(result) != 1 {
		t.Errorf("expected 1 result with date-only since filter, got %d", len(result))
	}

	resetHistoryFlags()
}

func TestApplyHistoryFilters_InvalidSinceDate(t *testing.T) {
	requests := []*db.Request{
		{ID: "1", Status: db.StatusPending, CreatedAt: time.Now()},
	}

	// Invalid date should be ignored
	flagHistoryStatus = ""
	flagHistoryAgent = ""
	flagHistoryTier = ""
	flagHistorySince = "invalid-date"

	result := applyHistoryFilters(requests)
	// Should return all since invalid date is ignored
	if len(result) != 1 {
		t.Errorf("expected 1 result with invalid since filter (ignored), got %d", len(result))
	}

	resetHistoryFlags()
}
