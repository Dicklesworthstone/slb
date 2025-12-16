package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/spf13/cobra"
)

// newTestPendingCmd creates a fresh pending command for testing.
func newTestPendingCmd(dbPath string) *cobra.Command {
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

	root.AddCommand(pendingCmd)

	return root
}

func resetPendingFlags() {
	flagDB = ""
	flagOutput = "text"
	flagJSON = false
	flagProject = ""
	flagSessionID = ""
	flagConfig = ""
	flagPendingAllProjects = false
	flagPendingReviewPool = false
}

func TestPendingCommand_ListsPendingRequests(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPendingFlags()

	// Create session and multiple pending requests
	sess := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir))
	testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("rm -rf ./build", h.ProjectDir, true),
		testutil.WithRisk(db.RiskTierDangerous),
	)
	testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("git push --force", h.ProjectDir, true),
		testutil.WithRisk(db.RiskTierDangerous),
	)

	cmd := newTestPendingCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "pending", "-C", h.ProjectDir, "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result []map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if len(result) != 2 {
		t.Errorf("expected 2 pending requests, got %d", len(result))
	}

	// Verify structure of first request
	if len(result) > 0 {
		req := result[0]
		if req["request_id"] == nil {
			t.Error("expected request_id to be set")
		}
		if req["command"] == nil {
			t.Error("expected command to be set")
		}
		if req["risk_tier"] != string(db.RiskTierDangerous) {
			t.Errorf("expected risk_tier=dangerous, got %v", req["risk_tier"])
		}
	}
}

func TestPendingCommand_EmptyList(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPendingFlags()

	cmd := newTestPendingCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "pending", "-C", h.ProjectDir, "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result []map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if len(result) != 0 {
		t.Errorf("expected 0 pending requests, got %d", len(result))
	}
}

func TestPendingCommand_OnlyShowsPending(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPendingFlags()

	// Create session and requests with different statuses
	sess := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir))

	// Create a pending request
	pendingReq := testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("rm -rf ./build", h.ProjectDir, true),
	)

	// Create another request and approve it
	approvedReq := testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("git push", h.ProjectDir, true),
	)
	h.DB.UpdateRequestStatus(approvedReq.ID, db.StatusApproved)

	cmd := newTestPendingCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "pending", "-C", h.ProjectDir, "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result []map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	// Should only show the pending request
	if len(result) != 1 {
		t.Errorf("expected 1 pending request, got %d", len(result))
	}

	if len(result) > 0 && result[0]["request_id"] != pendingReq.ID {
		t.Errorf("expected pending request %s, got %v", pendingReq.ID, result[0]["request_id"])
	}
}

func TestPendingCommand_ReviewPoolFlag(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPendingFlags()

	// Create two sessions - one for requestor, one for reviewer
	requestorSess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("Requestor"),
	)
	reviewerSess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("Reviewer"),
	)

	// Create request from requestor
	testutil.MakeRequest(t, h.DB, requestorSess,
		testutil.WithCommand("rm -rf ./build", h.ProjectDir, true),
	)

	cmd := newTestPendingCmd(h.DBPath)

	// Without review-pool, show all pending
	stdout, err := executeCommandCapture(t, cmd, "pending", "-C", h.ProjectDir, "-j")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result []map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}
	if len(result) != 1 {
		t.Errorf("expected 1 pending request without filter, got %d", len(result))
	}

	// With review-pool and session-id, exclude own requests
	resetPendingFlags()
	cmd2 := newTestPendingCmd(h.DBPath)
	stdout2, err := executeCommandCapture(t, cmd2, "pending",
		"-C", h.ProjectDir,
		"-s", reviewerSess.ID, // Reviewer's session
		"--review-pool",
		"-j",
	)
	if err != nil {
		t.Fatalf("unexpected error with review-pool: %v", err)
	}

	var result2 []map[string]any
	if err := json.Unmarshal([]byte(stdout2), &result2); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout2)
	}
	// Reviewer should see the request (not their own)
	if len(result2) != 1 {
		t.Errorf("expected 1 pending request for reviewer, got %d", len(result2))
	}
}

func TestPendingCommand_Help(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPendingFlags()

	cmd := newTestPendingCmd(h.DBPath)
	stdout, _, err := executeCommand(cmd, "pending", "--help")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(stdout, "pending") {
		t.Error("expected help to mention 'pending'")
	}
	if !strings.Contains(stdout, "--all-projects") {
		t.Error("expected help to mention '--all-projects' flag")
	}
	if !strings.Contains(stdout, "--review-pool") {
		t.Error("expected help to mention '--review-pool' flag")
	}
}

func TestDedupeStrings(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty", []string{}, []string{}},
		{"no dupes", []string{"a", "b", "c"}, []string{"a", "b", "c"}},
		{"with dupes", []string{"a", "b", "a", "c", "b"}, []string{"a", "b", "c"}},
		{"empty strings", []string{"a", "", "b", ""}, []string{"a", "b"}},
		{"all same", []string{"x", "x", "x"}, []string{"x"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dedupeStrings(tt.in)
			if len(got) != len(tt.want) {
				t.Errorf("dedupeStrings(%v) = %v, want %v", tt.in, got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("dedupeStrings(%v) = %v, want %v", tt.in, got, tt.want)
					break
				}
			}
		})
	}
}
