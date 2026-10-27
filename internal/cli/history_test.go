package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	slbgit "github.com/Dicklesworthstone/slb/internal/git"
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
	root.PersistentFlags().StringVar(&flagConfig, "config", "", "selected config")

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
	histCmd.AddCommand(newHistorySyncCmd())

	root.AddCommand(histCmd)

	return root
}

func resetHistoryFlags() {
	flagDB = ""
	flagConfig = ""
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

func TestHistorySyncCommandReplaysScopedMetadata(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	resetHistoryFlags()
	t.Cleanup(resetHistoryFlags)
	t.Setenv("HOME", t.TempDir())
	h := testutil.NewHarness(t)
	h.WriteFile(".slb/config.toml", []byte("[history]\ngit_repo_path = 'audit-history'\nauto_git_commit = false\n"), 0600)
	session := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir))
	first := testutil.MakeRequest(t, h.DB, session, func(r *db.Request) {
		r.ApprovalPolicy = db.ApprovalPolicySameHost
		r.Command.Raw = "echo raw-evidence-canary"
		r.Command.ContainsSensitive = true
		r.Justification.Reason = "private-justification-canary"
	})
	other := testutil.MakeSession(t, h.DB, testutil.WithProject(t.TempDir()))
	foreign := testutil.MakeRequest(t, h.DB, other)
	testutil.MakeRequest(t, h.DB, session)
	page, err := h.DB.ReadRequestEvents(context.Background(), h.ProjectDir, "", 100)
	if err != nil || len(page.Events) != 2 {
		t.Fatalf("reading fixture journal: %+v %v", page, err)
	}
	var previousCommit string
	for i := 0; i < 3; i++ {
		cmd := newTestHistoryCmd(h.DBPath)
		text, err := executeCommandCapture(t, cmd, "history", "sync", "-C", h.ProjectDir, "--limit", "1", "--json")
		if err != nil {
			t.Fatalf("explicit sync failed with auto export disabled: %v (%s)", err, text)
		}
		var result slbgit.HistorySyncResult
		if err := json.Unmarshal([]byte(text), &result); err != nil {
			t.Fatal(err)
		}
		expectedRepository, err := filepath.EvalSymlinks(filepath.Join(h.ProjectDir, "audit-history"))
		if err != nil {
			t.Fatal(err)
		}
		if result.Repository != expectedRepository || result.Ref == "" || result.Commit == "" {
			t.Fatalf("missing export destination: %+v", result)
		}
		if i < 2 {
			event := page.Events[i]
			if result.Events != 1 || result.HasMore != (i == 0) || result.Cursor != event.Cursor || result.LastSequence != event.Sequence {
				t.Fatalf("sync skipped or mis-scoped events: %+v", result)
			}
			show := exec.Command("git", "-C", result.Repository, "show", fmt.Sprintf("%s:events/%020d.json", result.Ref, event.Sequence))
			data, err := show.Output()
			if err != nil {
				t.Fatal(err)
			}
			var exported db.RequestEvent
			if err := json.Unmarshal(data, &exported); err != nil {
				t.Fatal(err)
			}
			if exported.Cursor != event.Cursor || exported.State.ApprovalPolicy != event.State.ApprovalPolicy || exported.RequestID == foreign.ID {
				t.Fatalf("history changed original scope or policy: %+v", exported)
			}
			for _, excluded := range []string{"raw-evidence-canary", "private-justification-canary", session.SessionKey} {
				if excluded != "" && strings.Contains(string(data), excluded) {
					t.Fatal("Git metadata exposed private request evidence")
				}
			}
		} else if result.Events != 0 || result.HasMore || result.Commit != previousCommit {
			t.Fatalf("no-op sync rewrote history: %+v", result)
		}
		previousCommit = result.Commit
	}
	stored, err := h.DB.GetRequest(first.ID)
	if err != nil || stored.Status != db.StatusPending || stored.ApprovalPolicy != db.ApprovalPolicySameHost {
		t.Fatalf("export changed authorization state: %+v %v", stored, err)
	}
}

func TestHistorySyncCommandRefusesInvalidSetup(t *testing.T) {
	for _, scenario := range []string{"unconfigured", "missing config", "directory config", "bad config", "missing database", "bad limit", "bad timeout", "bad destination"} {
		t.Run(scenario, func(t *testing.T) {
			resetHistoryFlags()
			t.Cleanup(resetHistoryFlags)
			t.Setenv("HOME", t.TempDir())
			h := testutil.NewHarness(t)
			cfg := "[history]\ngit_repo_path = 'audit-history'\n"
			if scenario == "unconfigured" {
				cfg = "[history]\ngit_repo_path = ''\n"
			}
			h.WriteFile(".slb/config.toml", []byte(cfg), 0600)
			args := []string{"history", "sync", "-C", h.ProjectDir, "--json"}
			databasePath := h.DBPath
			switch scenario {
			case "missing config":
				args = append(args, "--config", filepath.Join(h.ProjectDir, "missing.toml"))
			case "directory config":
				args = append(args, "--config", h.ProjectDir)
			case "bad config":
				h.WriteFile("bad.toml", []byte("invalid TOML ["), 0600)
				args = append(args, "--config", filepath.Join(h.ProjectDir, "bad.toml"))
			case "missing database":
				databasePath = filepath.Join(h.ProjectDir, "missing.db")
			case "bad limit":
				args = append(args, "--limit", "0")
			case "bad timeout":
				args = append(args, "--timeout", "0s")
			case "bad destination":
				h.WriteFile("audit-history", []byte("preserve this ordinary file"), 0600)
			}
			cmd := newTestHistoryCmd(databasePath)
			if text, err := executeCommandCapture(t, cmd, args...); err == nil {
				t.Fatalf("invalid history setup succeeded: %s", text)
			}
			if scenario == "missing database" {
				if _, err := os.Stat(databasePath); !os.IsNotExist(err) {
					t.Fatalf("history created a new source database: %v", err)
				}
			}
			if scenario != "bad destination" {
				if _, err := os.Stat(filepath.Join(h.ProjectDir, "audit-history")); !os.IsNotExist(err) {
					t.Fatalf("refused setup wrote a Git destination: %v", err)
				}
			}
		})
	}
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

	// Create multiple requests with unique commands and small delays to avoid ID collisions
	for i := 0; i < 5; i++ {
		testutil.MakeRequest(t, h.DB, sess,
			testutil.WithCommand(fmt.Sprintf("echo unique-limit-test-%d-%d", i, time.Now().UnixNano()), h.ProjectDir, true),
		)
		time.Sleep(time.Millisecond) // Small delay to ensure unique IDs
	}

	cmd := newTestHistoryCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "history",
		"-C", h.ProjectDir,
		"--limit", "2",
		"-j",
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result []map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if len(result) > 2 {
		t.Errorf("expected at most 2 results with limit=2, got %d", len(result))
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

func TestHistoryCommand_CombinedFilters(t *testing.T) {
	h := testutil.NewHarness(t)
	sessions := make(map[string]*db.Session)
	for i, state := range []struct {
		status db.RequestStatus
		agent  string
		tier   db.RiskTier
	}{
		{db.StatusPending, "Agent1", db.RiskTierDangerous},
		{db.StatusApproved, "Agent2", db.RiskTierCritical},
		{db.StatusRejected, "Agent1", db.RiskTierCritical},
	} {
		session := sessions[state.agent]
		if session == nil {
			session = testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir), testutil.WithAgent(state.agent))
			sessions[state.agent] = session
		}
		testutil.MakeRequest(t, h.DB, session, testutil.WithStatus(state.status), testutil.WithRisk(state.tier),
			testutil.WithCommand(fmt.Sprintf("echo history-%d", i), h.ProjectDir, true))
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
			resetHistoryFlags()
			cmd := newTestHistoryCmd(h.DBPath)
			stdout, err := executeCommandCapture(t, cmd, "history", "-C", h.ProjectDir, "-j",
				"--status", tt.status, "--agent", tt.agent, "--tier", tt.tier)
			if err != nil {
				t.Fatal(err)
			}
			var result []map[string]any
			if err := json.Unmarshal([]byte(stdout), &result); err != nil {
				t.Fatal(err)
			}
			if len(result) != tt.expected {
				t.Errorf("expected %d results, got %d", tt.expected, len(result))
			}
		})
	}

	// Reset flags
	resetHistoryFlags()
}

func TestHistoryCommand_SinceDates(t *testing.T) {
	h := testutil.NewHarness(t)
	session := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir))
	old := testutil.MakeRequest(t, h.DB, session)
	recent := testutil.MakeRequest(t, h.DB, session)
	for _, entry := range []struct{ id, at string }{
		{old.ID, "2026-01-01T12:00:00Z"}, {recent.ID, "2026-01-03T12:00:00Z"},
	} {
		if _, err := h.DB.Exec("UPDATE requests SET created_at = ? WHERE id = ?", entry.at, entry.id); err != nil {
			t.Fatal(err)
		}
	}
	for _, since := range []string{"2026-01-03", "2026-01-03T08:00:00-04:00"} {
		t.Run(since, func(t *testing.T) {
			resetHistoryFlags()
			stdout, err := executeCommandCapture(t, newTestHistoryCmd(h.DBPath), "history", "-C", h.ProjectDir, "-j", "--since", since)
			if err != nil {
				t.Fatal(err)
			}
			var result []map[string]any
			if err := json.Unmarshal([]byte(stdout), &result); err != nil {
				t.Fatal(err)
			}
			if len(result) != 1 || result[0]["request_id"] != recent.ID {
				t.Fatalf("unexpected history: %s", stdout)
			}
		})
	}
	resetHistoryFlags()
}

func TestHistoryCommand_RejectsInvalidFilters(t *testing.T) {
	h := testutil.NewHarness(t)
	// Exercise the real search evaluator rather than an empty-table query
	// plan that can return before SQLite evaluates MATCH syntax.
	session := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir))
	testutil.MakeRequest(t, h.DB, session)
	for _, args := range [][]string{
		{"--since", "invalid-date"}, {"--limit", "-1"}, {"--limit", "0"}, {"--limit", "10001"},
		{"--status", "made-up"}, {"--tier", "made-up"}, {"--query", "*"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			resetHistoryFlags()
			if _, err := executeCommandCapture(t, newTestHistoryCmd(h.DBPath),
				append([]string{"history", "-C", h.ProjectDir, "-j"}, args...)...); err == nil {
				t.Fatalf("accepted invalid history filters: %v", args)
			}
		})
	}
	resetHistoryFlags()
}

func TestHistoryCommand_FilterByStatusApproved(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHistoryFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
	)

	// Create an approved request
	req := testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("git push", h.ProjectDir, true),
	)
	h.DB.UpdateRequestStatus(req.ID, db.StatusApproved)

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

	// Should find the approved request
	found := false
	for _, r := range result {
		if r["request_id"] == req.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("approved request disappeared from history: %s", stdout)
	}
}

func TestHistoryCommand_FullTextSearch(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHistoryFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
	)

	// Create a request with a specific command
	testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("docker build -t myimage .", h.ProjectDir, true),
	)

	cmd := newTestHistoryCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "history",
		"-C", h.ProjectDir,
		"-q", "docker",
		"-j",
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result []map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if len(result) != 1 || result[0]["command"] != "docker build -t myimage ." {
		t.Fatalf("search did not return its match: %s", stdout)
	}
}

func TestHistoryCommand_CompletedScopedAndRedacted(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHistoryFlags()
	defer resetHistoryFlags()
	session := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir))
	completed := testutil.MakeRequest(t, h.DB, session,
		testutil.WithCommand("echo historyneedle", h.ProjectDir, true),
		testutil.WithStatus(db.StatusExecuted), func(r *db.Request) { r.ApprovalPolicy = db.ApprovalPolicySameHost })
	if _, err := h.DB.Exec("UPDATE requests SET execution_log_path = ?, execution_exit_code = 0, execution_approval_policy = 'same_host', execution_assurance = 'same_host' WHERE id = ?", "test.log", completed.ID); err != nil {
		t.Fatal(err)
	}
	secret := testutil.MakeRequest(t, h.DB, session,
		testutil.WithCommand("echo TOP-SECRET", h.ProjectDir, true),
		testutil.WithStatus(db.StatusExecutionFailed), func(r *db.Request) { r.Command.ContainsSensitive = true })
	foreign := testutil.MakeSession(t, h.DB, testutil.WithProject(t.TempDir()))
	testutil.MakeRequest(t, h.DB, foreign, testutil.WithCommand("echo historyneedle foreign", foreign.ProjectPath, true), testutil.WithStatus(db.StatusExecuted))
	for _, search := range []string{"", "historyneedle"} {
		t.Run(search, func(t *testing.T) {
			resetHistoryFlags()
			args := []string{"history", "-C", h.ProjectDir, "-j"}
			if search != "" {
				args = append(args, "--query", search)
			}
			stdout, err := executeCommandCapture(t, newTestHistoryCmd(h.DBPath), args...)
			if err != nil {
				t.Fatal(err)
			}
			var result []map[string]any
			if err := json.Unmarshal([]byte(stdout), &result); err != nil {
				t.Fatal(err)
			}
			want := 2
			if search != "" {
				want = 1
			}
			if len(result) != want {
				t.Fatalf("history omitted completed rows or leaked another project: %s", stdout)
			}
			found := false
			for _, row := range result {
				if row["project_path"] != h.ProjectDir {
					t.Fatalf("cross-project row: %v", row)
				}
				if row["request_id"] == completed.ID {
					found = true
					if row["execution_assurance"] != "same_host" || row["approval_policy"] != "same_host" {
						t.Fatalf("lost assurance: %v", row)
					}
				}
				if row["request_id"] == secret.ID && row["command"] != "[REDACTED]" {
					t.Fatalf("secret exposed: %v", row)
				}
			}
			if !found || strings.Contains(stdout, "TOP-SECRET") {
				t.Fatalf("invalid completed history: %s", stdout)
			}
		})
	}
}

func TestHistoryCommand_TextOutput(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHistoryFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
	)
	testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("echo hello", h.ProjectDir, true),
	)

	cmd := newTestHistoryCmd(h.DBPath)
	// No -j flag for text output
	stdout, err := executeCommandCapture(t, cmd, "history", "-C", h.ProjectDir)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Text output should contain request information
	_ = stdout // Just verify no error on text output
}
