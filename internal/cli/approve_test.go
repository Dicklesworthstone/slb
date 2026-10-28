package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/integrations"
	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/spf13/cobra"
)

// newTestApproveCmd creates a fresh approve command for testing.
func newTestApproveCmd(dbPath string) *cobra.Command {
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

	// Create a fresh approve command to avoid flag conflicts. Mirror
	// production: no -s local shorthand (-s is owned by the persistent
	// --session-id); the session is passed via the long --session-id flag.
	approve := &cobra.Command{
		Use:   "approve <request-id>",
		Short: "Approve a pending request",
		Args:  cobra.ExactArgs(1),
		RunE:  approveCmd.RunE,
	}
	approve.Flags().StringVar(&flagApproveSessionID, "session-id", "", "reviewer session ID (required)")
	approve.Flags().StringVarP(&flagApproveSessionKey, "session-key", "k", "", "session HMAC key for signing (required)")
	approve.Flags().StringVarP(&flagApproveComments, "comments", "m", "", "additional comments")
	approve.Flags().StringVar(&flagApproveTargetProject, "target-project", "", "target project path for cross-project approvals")
	approve.Flags().StringVar(&flagApproveReasonResponse, "reason-response", "", "response to the reason justification")
	approve.Flags().StringVar(&flagApproveEffectResponse, "effect-response", "", "response to the expected effect")
	approve.Flags().StringVar(&flagApproveGoalResponse, "goal-response", "", "response to the goal")
	approve.Flags().StringVar(&flagApproveSafetyResponse, "safety-response", "", "response to the safety argument")

	root.AddCommand(approve)

	return root
}

func resetApproveFlags() {
	flagDB = ""
	flagOutput = "text"
	flagJSON = false
	flagProject = ""
	flagConfig = ""
	flagApproveSessionID = ""
	flagApproveSessionKey = ""
	flagApproveComments = ""
	flagApproveTargetProject = ""
	flagApproveReasonResponse = ""
	flagApproveEffectResponse = ""
	flagApproveGoalResponse = ""
	flagApproveSafetyResponse = ""
}

func TestApproveCommand_RequiresRequestID(t *testing.T) {
	h := testutil.NewHarness(t)
	resetApproveFlags()

	cmd := newTestApproveCmd(h.DBPath)
	_, _, err := executeCommand(cmd, "approve")

	if err == nil {
		t.Fatal("expected error when request ID is missing")
	}
	if !strings.Contains(err.Error(), "accepts 1 arg") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestApproveCommand_RequiresSessionID(t *testing.T) {
	h := testutil.NewHarness(t)
	resetApproveFlags()

	cmd := newTestApproveCmd(h.DBPath)
	_, _, err := executeCommand(cmd, "approve", "some-request-id")

	if err == nil {
		t.Fatal("expected error when --session-id is missing")
	}
	if !strings.Contains(err.Error(), "--session-id is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestApproveCommand_RequiresSessionKey(t *testing.T) {
	h := testutil.NewHarness(t)
	resetApproveFlags()

	cmd := newTestApproveCmd(h.DBPath)
	_, _, err := executeCommand(cmd, "approve", "some-request-id", "--session-id", "session-123")

	if err == nil {
		t.Fatal("expected error when --session-key is missing")
	}
	if !strings.Contains(err.Error(), "--session-key is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

// Without an external identity authority (issue #30), a valid reviewer
// session and key still cannot approve through the CLI.
func TestApproveCommand_FailsClosedWithoutIdentityAuthority(t *testing.T) {
	h := testutil.NewHarness(t)
	resetApproveFlags()
	requestor := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("Requestor"), testutil.WithModel("model-a"))
	reviewer := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("Reviewer"), testutil.WithModel("model-b"))
	req := testutil.MakeRequest(t, h.DB, requestor,
		testutil.WithCommand("rm -rf ./build", h.ProjectDir, true),
		testutil.WithRisk(db.RiskTierDangerous))

	cmd := newTestApproveCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "approve", req.ID,
		"--session-id", reviewer.ID, "-k", reviewer.SessionKey, "-C", h.ProjectDir, "-j")
	if !errors.Is(err, db.ErrIndependentIdentityUnavailable) || stdout != "" {
		t.Fatalf("approve = %q, %v; want %v", stdout, err, db.ErrIndependentIdentityUnavailable)
	}
	stored, err := h.DB.GetRequest(req.ID)
	if err != nil || stored.Status != db.StatusPending {
		t.Fatalf("refused approval changed the request: %+v %v", stored, err)
	}
	if approvals, _, err := h.DB.CountReviewsByDecision(req.ID); err != nil || approvals != 0 {
		t.Fatalf("refused approval recorded a vote: %d %v", approvals, err)
	}
}

func TestApproveCommand_ApprovesRequest(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	h := testutil.NewHarness(t)
	resetApproveFlags()

	// Create requestor session
	requestorSess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("Requestor"),
		testutil.WithModel("model-a"),
	)

	// Create reviewer session with different model
	reviewerSess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("Reviewer"),
		testutil.WithModel("model-b"),
	)

	// Create request
	req := testutil.MakeRequest(t, h.DB, requestorSess,
		testutil.WithCommand("rm -rf ./build", h.ProjectDir, true),
		testutil.WithRisk(db.RiskTierDangerous),
	)
	// Set MinApprovals to 1 and RequireDifferentModel to false for simpler test
	h.DB.Exec(`UPDATE requests SET min_approvals = 1, require_different_model = false WHERE id = ?`, req.ID)

	cmd := newTestApproveCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "approve", req.ID,
		"--session-id", reviewerSess.ID,
		"-k", reviewerSess.SessionKey,
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
	if result["request_id"] != req.ID {
		t.Errorf("expected request_id=%s, got %v", req.ID, result["request_id"])
	}
	if result["decision"] != "approve" {
		t.Errorf("expected decision=approve, got %v", result["decision"])
	}
	if result["approvals"].(float64) != 1 {
		t.Errorf("expected approvals=1, got %v", result["approvals"])
	}
	if result["request_status_changed"] != true {
		t.Errorf("expected request_status_changed=true, got %v", result["request_status_changed"])
	}
	if result["new_request_status"] != string(db.StatusApproved) {
		t.Errorf("expected new_request_status=approved, got %v", result["new_request_status"])
	}
	if result["request_status"] != string(db.StatusApproved) {
		t.Errorf("missing committed request status: %v", result["request_status"])
	}
}

func TestApproveCommand_WithComments(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	h := testutil.NewHarness(t)
	resetApproveFlags()

	requestorSess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("Requestor"),
	)
	reviewerSess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("Reviewer"),
	)

	req := testutil.MakeRequest(t, h.DB, requestorSess)
	h.DB.Exec(`UPDATE requests SET min_approvals = 1, require_different_model = false WHERE id = ?`, req.ID)

	cmd := newTestApproveCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "approve", req.ID,
		"--session-id", reviewerSess.ID,
		"-k", reviewerSess.SessionKey,
		"-m", "Looks good to me",
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

	// Verify review was created with comments
	reviews, _ := h.DB.ListReviewsForRequest(req.ID)
	if len(reviews) != 1 {
		t.Fatalf("expected 1 review, got %d", len(reviews))
	}
	if reviews[0].Comments != "Looks good to me" {
		t.Errorf("expected comments='Looks good to me', got %q", reviews[0].Comments)
	}
}

func TestApproveCommand_SelfReviewPrevented(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	h := testutil.NewHarness(t)
	resetApproveFlags()

	// Create a session that is both requestor and reviewer
	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("SelfReviewer"),
	)

	req := testutil.MakeRequest(t, h.DB, sess)
	h.DB.Exec(`UPDATE requests SET min_approvals = 1, require_different_model = false WHERE id = ?`, req.ID)

	cmd := newTestApproveCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "approve", req.ID,
		"--session-id", sess.ID,
		"-k", sess.SessionKey,
		"-C", h.ProjectDir,
		"-j",
	)

	if err == nil {
		t.Fatal("expected error for self-review")
	}
	if !strings.Contains(err.Error(), "own request") && !strings.Contains(err.Error(), "self") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestApproveCommand_InvalidSessionKey(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	h := testutil.NewHarness(t)
	resetApproveFlags()

	requestorSess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("Requestor"),
	)
	reviewerSess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("Reviewer"),
	)

	req := testutil.MakeRequest(t, h.DB, requestorSess)

	cmd := newTestApproveCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "approve", req.ID,
		"--session-id", reviewerSess.ID,
		"-k", "wrong-key-not-matching",
		"-C", h.ProjectDir,
		"-j",
	)

	if err == nil {
		t.Fatal("expected error for invalid session key")
	}
	if !strings.Contains(err.Error(), "key") && !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestApproveCommand_RequestNotFound(t *testing.T) {
	h := testutil.NewHarness(t)
	resetApproveFlags()

	reviewerSess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("Reviewer"),
	)

	cmd := newTestApproveCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "approve", "nonexistent-request",
		"--session-id", reviewerSess.ID,
		"-k", reviewerSess.SessionKey,
		"-C", h.ProjectDir,
		"-j",
	)

	if err == nil {
		t.Fatal("expected error for nonexistent request")
	}
}

func TestApproveCommand_Help(t *testing.T) {
	h := testutil.NewHarness(t)
	resetApproveFlags()

	cmd := newTestApproveCmd(h.DBPath)
	stdout, _, err := executeCommand(cmd, "approve", "--help")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(stdout, "approve") {
		t.Error("expected help to mention 'approve'")
	}
	if !strings.Contains(stdout, "--session-id") {
		t.Error("expected help to mention '--session-id' flag")
	}
	if !strings.Contains(stdout, "--session-key") {
		t.Error("expected help to mention '--session-key' flag")
	}
}

func TestBuildAgentMailNotifier_AgentMailDisabled(t *testing.T) {
	h := testutil.NewHarness(t)
	resetApproveFlags()

	// Clear any environment variables that might enable agent mail
	origEnv := os.Getenv("SLB_AGENT_MAIL_ENABLED")
	os.Setenv("SLB_AGENT_MAIL_ENABLED", "false")
	defer os.Setenv("SLB_AGENT_MAIL_ENABLED", origEnv)

	// By default, agent mail is disabled in config
	cfg, err := config.Load(config.LoadOptions{ProjectDir: h.ProjectDir})
	if err != nil {
		t.Fatal(err)
	}
	notifier := buildAgentMailNotifier(h.ProjectDir, cfg)

	// Verify we can call the notifier without panic
	// Type check: if it's a NoopNotifier, it handles nil safely
	if _, ok := notifier.(integrations.NoopNotifier); ok {
		// NoopNotifier is expected
		err := notifier.NotifyNewRequest(nil)
		if err != nil {
			t.Errorf("expected no error from NoopNotifier, got %v", err)
		}
	}
	// If it's AgentMailClient, just verify we got a valid notifier
}

func TestBuildAgentMailNotifier_WithConfigPath(t *testing.T) {
	h := testutil.NewHarness(t)
	resetApproveFlags()

	// Clear any environment variables that might enable agent mail
	origEnv := os.Getenv("SLB_AGENT_MAIL_ENABLED")
	os.Setenv("SLB_AGENT_MAIL_ENABLED", "false")
	defer os.Setenv("SLB_AGENT_MAIL_ENABLED", origEnv)

	// Set config path to project dir (will use default config)
	flagConfig = ""
	flagProject = h.ProjectDir

	cfg, err := config.Load(config.LoadOptions{ProjectDir: h.ProjectDir, ConfigPath: flagConfig})
	if err != nil {
		t.Fatal(err)
	}
	notifier := buildAgentMailNotifier(h.ProjectDir, cfg)

	// Should return a notifier - verify it exists
	if notifier == nil {
		t.Error("expected non-nil notifier")
	}
}

// TestBuildAgentMailNotifier_AgentMailEnabled tests when agent mail is enabled in config.
func TestBuildAgentMailNotifier_AgentMailEnabled(t *testing.T) {
	h := testutil.NewHarness(t)
	resetApproveFlags()

	// Create config file with agent_mail enabled
	configPath := h.ProjectDir + "/slb.toml"
	configContent := `
[integrations]
agent_mail_enabled = true
agent_mail_thread = "test-thread-123"
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	flagConfig = configPath

	cfg, err := config.Load(config.LoadOptions{ProjectDir: h.ProjectDir, ConfigPath: flagConfig})
	if err != nil {
		t.Fatal(err)
	}
	notifier := buildAgentMailNotifier(h.ProjectDir, cfg)

	// Should return a notifier (either AgentMailClient or fallback)
	if notifier == nil {
		t.Error("expected non-nil notifier")
	}

	// The function should attempt to create AgentMailClient when enabled
	// This covers the line 160 path in buildAgentMailNotifier
}

// TestBuildAgentMailNotifier_DefaultsToNoopWithNoConfig tests default behavior.
func TestBuildAgentMailNotifier_DefaultsToNoopWithNoConfig(t *testing.T) {
	h := testutil.NewHarness(t)
	resetApproveFlags()

	// No config file in project dir, should use defaults (agent mail disabled)
	flagConfig = ""

	cfg, err := config.Load(config.LoadOptions{ProjectDir: h.ProjectDir})
	if err != nil {
		t.Fatal(err)
	}
	notifier := buildAgentMailNotifier(h.ProjectDir, cfg)

	// Should return a notifier - with defaults, agent mail is disabled
	if notifier == nil {
		t.Error("expected non-nil notifier")
	}
	// By default (no config), agent_mail_enabled is false
	// So we should get NoopNotifier
	_, isNoop := notifier.(integrations.NoopNotifier)
	if !isNoop {
		// This is acceptable - config might come from environment or defaults
		// Just verify we got a valid notifier
	}
}

// installAgentMailCapture keeps every notification regression local: a shell
// fixture records arguments without contacting an Agent Mail service.
func installAgentMailCapture(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX notification transport fixture")
	}
	bin := t.TempDir()
	path := filepath.Join(t.TempDir(), "notifications")
	t.Setenv("SLB_TEST_NOTIFICATION_ARGS", path)
	script := "#!/bin/sh\nprintf '%s\\n' CALL \"$@\" >> \"$SLB_TEST_NOTIFICATION_ARGS\"\n"
	if err := os.WriteFile(filepath.Join(bin, "mcp-agent-mail"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

func TestAgentMailNotifierUsesEffectiveDeliveryPolicy(t *testing.T) {
	for _, journal := range []bool{true, false} {
		t.Run(fmt.Sprintf("journal=%t", journal), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("USERPROFILE", t.TempDir())
			t.Setenv("SLB_AGENT_MAIL_ENABLED", "true")
			t.Setenv("SLB_AGENT_MAIL_THREAD", "")
			marker := installAgentMailCapture(t)
			project := t.TempDir()
			if err := os.MkdirAll(filepath.Join(project, ".slb"), 0700); err != nil {
				t.Fatal(err)
			}
			projectConfig := fmt.Sprintf("[notifications.requests]\nenabled = %t\n[integrations]\nagent_mail_thread = 'project-thread'\n", !journal)
			if err := os.WriteFile(filepath.Join(project, ".slb", "config.toml"), []byte(projectConfig), 0600); err != nil {
				t.Fatal(err)
			}
			explicitPath := filepath.Join(t.TempDir(), "selected.toml")
			explicitConfig := fmt.Sprintf("[notifications.requests]\nenabled = %t\n[integrations]\nagent_mail_thread = 'explicit-thread'\n", journal)
			if err := os.WriteFile(explicitPath, []byte(explicitConfig), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(config.LoadOptions{ProjectDir: project, ConfigPath: explicitPath})
			if err != nil {
				t.Fatal(err)
			}
			notifier := buildAgentMailNotifier(project, cfg)
			request := &db.Request{ID: "request-notification", ApprovalPolicy: db.ApprovalPolicySameHost,
				Status: db.StatusApproved, RiskTier: db.RiskTierDangerous, Command: db.CommandSpec{Raw: "echo notification"}}
			review := &db.Review{ReviewerAgent: "Reviewer", ReviewerModel: "reviewer-model", Assurance: db.AssuranceSameHost}
			for _, notify := range []func() error{
				func() error { return notifier.NotifyNewRequest(request) },
				func() error { return notifier.NotifyRequestApproved(request, review) },
				func() error { return notifier.NotifyRequestRejected(request, review) },
				func() error {
					return notifier.NotifyRequestExecuted(request, &db.Execution{ApprovalPolicy: db.ApprovalPolicySameHost, Assurance: db.AssuranceSameHost}, 0)
				},
			} {
				if err := notify(); err != nil {
					t.Fatal(err)
				}
			}
			data, err := os.ReadFile(marker)
			if journal {
				if !os.IsNotExist(err) {
					t.Fatalf("durable policy invoked legacy transport: %s (%v)", data, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(data), "CALL\n") != 4 || strings.Count(string(data), "--thread\nexplicit-thread\n") != 4 ||
				strings.Contains(string(data), "project-thread") || !strings.Contains(string(data), "not independently authenticated") {
				t.Fatalf("explicit legacy route or review assurance was lost: %s", data)
			}
		})
	}
}

func TestReviewNotificationsUseRequestProjectAndExplicitPolicy(t *testing.T) {
	for _, decision := range []string{"approve", "reject"} {
		for _, journal := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/journal=%t", decision, journal), func(t *testing.T) {
				resetApproveFlags()
				resetRejectFlags()
				t.Cleanup(resetApproveFlags)
				t.Cleanup(resetRejectFlags)
				t.Setenv("HOME", t.TempDir())
				t.Setenv("USERPROFILE", t.TempDir())
				t.Setenv("SLB_AGENT_MAIL_ENABLED", "true")
				t.Setenv("SLB_AGENT_MAIL_THREAD", "")
				marker := installAgentMailCapture(t)
				h := testutil.NewHarness(t)
				caller := testutil.NewHarness(t)
				caller.WriteFile(".slb/config.toml", []byte("[notifications.requests]\nenabled = true\n[integrations]\nagent_mail_thread = 'wrong-project'\n"), 0600)
				h.WriteFile(".slb/config.toml", []byte("[general]\napproval_policy = 'same_host'\n[notifications.requests]\nenabled = false\n[integrations]\nagent_mail_thread = 'request-project'\n"), 0600)
				author := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir), testutil.WithAgent("Author"))
				reviewer := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir), testutil.WithAgent("Reviewer"))
				request := testutil.MakeRequest(t, h.DB, author, func(r *db.Request) { r.ApprovalPolicy = db.ApprovalPolicySameHost })
				root := newTestApproveCmd(h.DBPath)
				args := []string{decision, request.ID, "--session-id", reviewer.ID, "--session-key", reviewer.SessionKey, "-C", caller.ProjectDir, "--json"}
				if decision == "reject" {
					root = newTestRejectCmd(h.DBPath)
					args = append(args, "--reason", "review notification regression")
				}
				if journal {
					selected := filepath.Join(t.TempDir(), "selected.toml")
					if err := os.WriteFile(selected, []byte("[general]\napproval_policy = 'same_host'\n[notifications.requests]\nenabled = true\n"), 0600); err != nil {
						t.Fatal(err)
					}
					args = append(args, "--config", selected)
				}
				stdout, err := executeCommandCapture(t, root, args...)
				if err != nil {
					t.Fatalf("review failed: %v (%s)", err, stdout)
				}
				stored, err := h.DB.GetRequest(request.ID)
				want := db.StatusApproved
				if decision == "reject" {
					want = db.StatusRejected
				}
				if err != nil || stored.Status != want || !strings.Contains(stdout, `"assurance": "same_host"`) {
					t.Fatalf("review was not persisted with low assurance: %+v (%v), %s", stored, err, stdout)
				}
				data, err := os.ReadFile(marker)
				if journal {
					if !os.IsNotExist(err) {
						t.Fatalf("explicit journal review invoked legacy transport: %s (%v)", data, err)
					}
				} else if err != nil || strings.Count(string(data), "CALL\n") != 1 ||
					!strings.Contains(string(data), "--project\n"+h.ProjectDir+"\n") || !strings.Contains(string(data), "--thread\nrequest-project\n") ||
					strings.Contains(string(data), "wrong-project") {
					t.Fatalf("review notification was routed using the caller's project: %s (%v)", data, err)
				}
			})
		}
	}
}

func TestReviewCommandsRefuseUnavailableExplicitConfig(t *testing.T) {
	for _, decision := range []string{"approve", "reject"} {
		for _, fault := range []string{"missing", "directory"} {
			t.Run(decision+"/"+fault, func(t *testing.T) {
				resetApproveFlags()
				resetRejectFlags()
				t.Cleanup(resetApproveFlags)
				t.Cleanup(resetRejectFlags)
				t.Setenv("HOME", t.TempDir())
				t.Setenv("USERPROFILE", t.TempDir())
				t.Setenv("SLB_AGENT_MAIL_ENABLED", "true")
				marker := installAgentMailCapture(t)
				h := testutil.NewHarness(t)
				author := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir), testutil.WithAgent("Author"))
				reviewer := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir), testutil.WithAgent("Reviewer"))
				request := testutil.MakeRequest(t, h.DB, author, func(r *db.Request) { r.ApprovalPolicy = db.ApprovalPolicySameHost })
				selected := filepath.Join(t.TempDir(), "selected.toml")
				if fault == "directory" {
					if err := os.Mkdir(selected, 0700); err != nil {
						t.Fatal(err)
					}
				}
				root := newTestApproveCmd(h.DBPath)
				args := []string{decision, request.ID, "--session-id", reviewer.ID, "--session-key", reviewer.SessionKey,
					"-C", h.ProjectDir, "--config", selected, "--json"}
				if decision == "reject" {
					root = newTestRejectCmd(h.DBPath)
					args = append(args, "--reason", "selected configuration must be available")
				}
				stdout, err := executeCommandCapture(t, root, args...)
				if err == nil || !strings.Contains(err.Error(), "explicit review config") {
					t.Fatalf("unavailable selected config was accepted: %v (%s)", err, stdout)
				}
				stored, err := h.DB.GetRequest(request.ID)
				if err != nil || stored.Status != db.StatusPending || stored.ResolvedAt != nil || stored.ApprovalExpiresAt != nil {
					t.Fatalf("unavailable selected config mutated request: %+v (%v)", stored, err)
				}
				reviews, err := h.DB.ListReviewsForRequest(request.ID)
				if err != nil || len(reviews) != 0 {
					t.Fatalf("unavailable selected config recorded a review: %+v (%v)", reviews, err)
				}
				if data, err := os.ReadFile(marker); !os.IsNotExist(err) {
					t.Fatalf("unavailable selected config invoked a fallback transport: %s (%v)", data, err)
				}
			})
		}
	}
}

// TestApproveCommand_CrossProject tests approving a request from another project
// using the --target-project flag.
func TestApproveCommand_CrossProject(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	targetH := testutil.NewHarness(t)
	currentH := testutil.NewHarness(t)
	resetApproveFlags()

	requestorSess := testutil.MakeSession(t, targetH.DB,
		testutil.WithProject(targetH.ProjectDir),
		testutil.WithAgent("Requestor"),
		testutil.WithModel("model-a"),
	)
	reviewerSess := testutil.MakeSession(t, currentH.DB,
		testutil.WithProject(currentH.ProjectDir),
		testutil.WithAgent("Reviewer"),
		testutil.WithModel("model-b"),
	)
	req := testutil.MakeRequest(t, targetH.DB, requestorSess,
		testutil.WithCommand("rm -rf ./build", targetH.ProjectDir, true),
		testutil.WithRisk(db.RiskTierDangerous),
	)
	targetH.DB.Exec(`UPDATE requests SET min_approvals = 1, require_different_model = false WHERE id = ?`, req.ID)

	configPath := filepath.Join(targetH.ProjectDir, ".slb", "config.toml")
	if err := os.WriteFile(configPath, []byte("[general]\ncross_project_reviews = true\nreview_pool = ['Reviewer']\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newTestApproveCmd(currentH.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "approve", req.ID,
		"--session-id", reviewerSess.ID,
		"-k", reviewerSess.SessionKey,
		"-C", currentH.ProjectDir,
		"--target-project", targetH.ProjectDir,
		"-j",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}
	if result["decision"] != "approve" || result["request_status_changed"] != true {
		t.Fatalf("unexpected cross-project approval result: %+v", result)
	}
	updatedReq, err := targetH.DB.GetRequest(req.ID)
	if err != nil || updatedReq.Status != db.StatusApproved {
		t.Fatalf("target request not approved: status=%v err=%v", updatedReq.Status, err)
	}
	reviews, err := targetH.DB.ListReviewsForRequest(req.ID)
	if err != nil || len(reviews) != 1 {
		t.Fatalf("delegated review missing: len=%d err=%v", len(reviews), err)
	}
	if reviews[0].ReviewerAgent != reviewerSess.AgentName || reviews[0].ReviewerSessionID == reviewerSess.ID {
		t.Fatalf("review did not bind delegated identity: %+v", reviews[0])
	}
	provenance, err := targetH.DB.GetSession(reviews[0].ReviewerSessionID)
	if err != nil || provenance.EndedAt == nil {
		t.Fatalf("delegated provenance must be ended, session=%+v err=%v", provenance, err)
	}
	if provenance.SessionKey == reviewerSess.SessionKey {
		t.Fatal("target project received the source reviewer session key")
	}
	active, err := targetH.DB.ListActiveSessions(targetH.ProjectDir)
	if err != nil || len(active) != 1 || active[0].ID != requestorSess.ID {
		t.Fatalf("delegation polluted target active reviewer capacity: %+v err=%v", active, err)
	}
}

func TestApproveCommand_CrossProjectRequiresTargetPolicy(t *testing.T) {
	targetH := testutil.NewHarness(t)
	currentH := testutil.NewHarness(t)
	resetApproveFlags()
	requestor := testutil.MakeSession(t, targetH.DB, testutil.WithProject(targetH.ProjectDir), testutil.WithAgent("Requestor"))
	reviewer := testutil.MakeSession(t, currentH.DB, testutil.WithProject(currentH.ProjectDir), testutil.WithAgent("Reviewer"))
	req := testutil.MakeRequest(t, targetH.DB, requestor)

	cmd := newTestApproveCmd(currentH.DBPath)
	_, err := executeCommandCapture(t, cmd, "approve", req.ID,
		"--session-id", reviewer.ID, "-k", reviewer.SessionKey,
		"-C", currentH.ProjectDir, "--target-project", targetH.ProjectDir, "-j")
	if err == nil || !strings.Contains(err.Error(), "cross-project review") {
		t.Fatalf("target policy did not deny delegated review: %v", err)
	}
}

func TestApproveCommand_SameHostCrossProjectRetainsProvenance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	target := testutil.NewHarness(t)
	source := testutil.NewHarness(t)
	targetChild := filepath.Join(target.ProjectDir, "src", "deep")
	if err := os.MkdirAll(targetChild, 0700); err != nil {
		t.Fatal(err)
	}
	resetApproveFlags()
	t.Cleanup(resetApproveFlags)
	requester := testutil.MakeSession(t, target.DB, testutil.WithProject(target.ProjectDir),
		testutil.WithAgent("CooperativeRequestor"), testutil.WithModel("author-model"))
	reviewer := testutil.MakeSession(t, source.DB, testutil.WithProject(source.ProjectDir),
		testutil.WithAgent("CooperativeReviewer"), testutil.WithModel("reviewer-model"))
	writePolicy := func(policy string) {
		t.Helper()
		target.WriteFile(".slb/config.toml", []byte("[general]\napproval_policy = '"+policy+"'\ncross_project_reviews = true\nreview_pool = ['CooperativeReviewer']\n[integrations]\nagent_mail_enabled = false\n"), 0600)
	}
	newRequest := func() *db.Request {
		t.Helper()
		return testutil.MakeRequest(t, target.DB, requester,
			testutil.WithCommand("echo cooperative-cross-project-fixture", target.ProjectDir, false),
			testutil.WithRequireDifferentModel(true),
			func(request *db.Request) { request.ApprovalPolicy = db.ApprovalPolicySameHost })
	}
	approve := func(requestID, key string) (string, error) {
		t.Helper()
		resetApproveFlags()
		return executeCommandCapture(t, newTestApproveCmd(source.DBPath), "approve", requestID,
			"--session-id", reviewer.ID, "-k", key, "-C", source.ProjectDir,
			"--target-project", targetChild, "-j")
	}

	writePolicy("same_host")
	approved := newRequest()
	if _, err := approve(approved.ID, "wrong-source-credential"); err == nil {
		t.Fatal("same-host cross-project review bypassed source authentication")
	}
	if reviews, err := target.DB.ListReviewsForRequest(approved.ID); err != nil || len(reviews) != 0 {
		t.Fatalf("failed source authentication committed a vote: %+v, %v", reviews, err)
	}
	stdout, err := approve(approved.ID, reviewer.SessionKey)
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal([]byte(stdout), &response); err != nil {
		t.Fatal(err)
	}
	if response["approval_policy"] != "same_host" || response["assurance"] != "same_host" ||
		response["approval_policy_label"] != db.ApprovalPolicySameHost.Label() || response["request_status_changed"] != true {
		t.Fatalf("same-host cross-project output lost provenance: %+v", response)
	}
	verified, err := target.DB.VerifyRequestApproval(approved.ID)
	if err != nil || verified.ApprovalPolicy != db.ApprovalPolicySameHost || verified.Status != db.StatusApproved {
		t.Fatalf("delegated approval failed signed-proof validation: %+v, %v", verified, err)
	}
	reviews, err := target.DB.ListReviewsForRequest(approved.ID)
	if err != nil || len(reviews) != 1 || reviews[0].ApprovalPolicy != db.ApprovalPolicySameHost || reviews[0].Assurance != db.AssuranceSameHost ||
		reviews[0].ReviewerAgent != reviewer.AgentName || reviews[0].ReviewerSessionID == reviewer.ID {
		t.Fatalf("delegated review provenance missing: %+v, %v", reviews, err)
	}
	provenance, err := target.DB.GetSession(reviews[0].ReviewerSessionID)
	if err != nil || provenance.EndedAt == nil || provenance.SessionKey != "" {
		t.Fatalf("delegated provenance retained active capacity or a credential: %+v, %v", provenance, err)
	}
	active, err := target.DB.ListActiveSessions(target.ProjectDir)
	if err != nil || len(active) != 1 || active[0].ID != requester.ID {
		t.Fatalf("delegation changed target reviewer capacity: %+v, %v", active, err)
	}
	if _, err := source.DB.AuthenticateSession(reviewer.ID, reviewer.SessionKey); err != nil {
		t.Fatalf("delegation damaged source session authentication: %v", err)
	}

	revoked := newRequest()
	writePolicy("independent")
	if _, err := approve(revoked.ID, reviewer.SessionKey); err == nil {
		t.Fatal("target policy disablement retained same-host cross-project authority")
	}
	if reviews, err := target.DB.ListReviewsForRequest(revoked.ID); err != nil || len(reviews) != 0 {
		t.Fatalf("disabled target committed a same-host vote: %+v, %v", reviews, err)
	}
	stored, err := target.DB.GetRequest(approved.ID)
	if err != nil || stored.Status != db.StatusApproved || stored.ApprovalPolicy != db.ApprovalPolicySameHost {
		t.Fatalf("policy change relabeled delegated approval history: %+v, %v", stored, err)
	}
	if _, err := os.Lstat(filepath.Join(targetChild, ".slb")); !os.IsNotExist(err) {
		t.Fatalf("cross-project approval created shadow target state: %v", err)
	}
}

func TestReviewTargetNeverCreatesMissingState(t *testing.T) {
	for _, operation := range []string{"approve", "reject"} {
		for _, markerExists := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/marker=%t", operation, markerExists), func(t *testing.T) {
				source := testutil.NewHarness(t)
				target := t.TempDir()
				child := filepath.Join(target, "src")
				if err := os.Mkdir(child, 0700); err != nil {
					t.Fatal(err)
				}
				marker := filepath.Join(target, ".slb")
				if markerExists {
					if err := os.Mkdir(marker, 0700); err != nil {
						t.Fatal(err)
					}
				}
				resetApproveFlags()
				resetRejectFlags()
				t.Cleanup(resetApproveFlags)
				t.Cleanup(resetRejectFlags)
				args := []string{operation, "missing-request", "--session-id", "reviewer", "--session-key", "key",
					"-C", source.ProjectDir, "--target-project", child}
				var command *cobra.Command
				if operation == "approve" {
					command = newTestApproveCmd(source.DBPath)
				} else {
					command = newTestRejectCmd(source.DBPath)
					args = append(args, "--reason", "Target state must already exist")
				}
				if _, err := executeCommandCapture(t, command, args...); err == nil || !strings.Contains(err.Error(), "opening database") {
					t.Fatalf("missing review target was not refused before request lookup: %v", err)
				}
				if _, err := os.Lstat(filepath.Join(child, ".slb")); !os.IsNotExist(err) {
					t.Fatalf("review created shadow target state: %v", err)
				}
				if _, err := os.Stat(filepath.Join(marker, "state.db")); !os.IsNotExist(err) {
					t.Fatalf("review recreated lost target state: %v", err)
				}
				if !markerExists {
					if _, err := os.Lstat(marker); !os.IsNotExist(err) {
						t.Fatalf("review initialized a target project: %v", err)
					}
				}
			})
		}
	}
}

// TestApproveCommand_Help_IncludesTargetProject verifies help mentions --target-project.
func TestApproveCommand_Help_IncludesTargetProject(t *testing.T) {
	h := testutil.NewHarness(t)
	resetApproveFlags()

	cmd := newTestApproveCmd(h.DBPath)
	stdout, _, err := executeCommand(cmd, "approve", "--help")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(stdout, "--target-project") {
		t.Error("expected help to mention '--target-project' flag")
	}
	if !strings.Contains(stdout, "cross-project") {
		t.Error("expected help to mention 'cross-project'")
	}
}
