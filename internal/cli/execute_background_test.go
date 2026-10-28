package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/background"
	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
)

// Invoke the real worker handler in a separate test process. This exercises
// actual SQLite, signed approval checks, process execution and completion.
func TestExecuteBackgroundWorkerProcess(t *testing.T) {
	if os.Getenv("SLB_TEST_EXECUTE_BACKGROUND_WORKER") != "1" {
		return
	}
	// Inherit the parent test's simulated identity authority, if it has one.
	// Its peer-approved fixtures are refused without it (issue #30).
	if os.Getenv(db.SimulatedIdentityEnvForTest) == "1" {
		db.SimulateIndependentIdentityForTest(t)
	}
	command := newBackgroundWorkerCmd()
	command.SetArgs([]string{})
	if err := command.ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func backgroundExecutionFixture(t *testing.T, script string, policies ...db.ApprovalPolicy) (*db.DB, background.Job) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("detached POSIX execution test")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("SLB_TEST_EXECUTE_BACKGROUND_WORKER", "1")
	policy := db.ApprovalPolicyIndependent
	if len(policies) > 0 {
		policy = policies[0]
	}
	project := t.TempDir()
	conn, err := db.OpenAndMigrate(filepath.Join(project, ".slb", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	configuration := fmt.Sprintf("[integrations]\nagent_mail_enabled = false\n[general]\napproval_policy = %q\nenable_rollback_capture = false\n", policy)
	if err := os.WriteFile(filepath.Join(project, ".slb", "config.toml"), []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	requester := &db.Session{AgentName: "BackgroundRequester", Program: "test", Model: "model-a", ProjectPath: project}
	reviewers := []*db.Session{
		{AgentName: "BackgroundReviewerOne", Program: "test", Model: "model-b", ProjectPath: project},
		{AgentName: "BackgroundReviewerTwo", Program: "test", Model: "model-c", ProjectPath: project},
	}
	for _, session := range append([]*db.Session{requester}, reviewers...) {
		if err := conn.CreateSession(session); err != nil {
			t.Fatal(err)
		}
	}
	expires := time.Now().UTC().Add(5 * time.Minute)
	request := &db.Request{ProjectPath: project, Command: db.CommandSpec{Raw: script, Cwd: project, Shell: true},
		ApprovalPolicy:     policy,
		RequestorSessionID: requester.ID, RequestorAgent: requester.AgentName, RequestorModel: requester.Model,
		Status: db.StatusApproved, RiskTier: db.RiskTierCritical, MinApprovals: 2, ApprovalExpiresAt: &expires,
		Justification: db.Justification{Reason: "background execution regression in a temporary project"}}
	if policy == db.ApprovalPolicySameHost {
		request.Status, request.ApprovalExpiresAt = db.StatusPending, nil
	}
	if err := conn.CreateRequest(request); err != nil {
		t.Fatal(err)
	}
	for _, reviewer := range reviewers {
		if policy == db.ApprovalPolicySameHost {
			if _, err := conn.ApplyReview(&db.Review{RequestID: request.ID, ReviewerSessionID: reviewer.ID, Decision: db.DecisionApprove}, reviewer.SessionKey, db.ReviewPolicy{ApprovalPolicy: policy}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		now := time.Now().UTC()
		review := &db.Review{RequestID: request.ID, ReviewerSessionID: reviewer.ID, ReviewerAgent: reviewer.AgentName, ReviewerModel: reviewer.Model,
			Decision: db.DecisionApprove, SignatureTimestamp: now, Signature: db.ComputeReviewSignature(reviewer.SessionKey, request.ID, db.DecisionApprove, now)}
		if err := conn.CreateReview(review); err != nil {
			t.Fatal(err)
		}
	}
	if policy != db.ApprovalPolicySameHost {
		resolvedAt := time.Now().UTC()
		if _, err := conn.Exec("UPDATE requests SET resolved_at = ? WHERE id = ?", resolvedAt.Format(time.RFC3339), request.ID); err != nil {
			t.Fatal(err)
		}
	}
	return conn, background.Job{Version: background.Version, RequestID: request.ID, SessionID: requester.ID, CommandHash: request.Command.Hash,
		ApprovalPolicy: policy,
		Database:       conn.Path(), Project: project, LogDir: filepath.Join(project, ".slb", "logs"), TimeoutSeconds: 10}
}

func TestExecuteBackgroundSameHostCarriesAssurance(t *testing.T) {
	conn, job := backgroundExecutionFixture(t, "printf 'same-host-background\\n'", db.ApprovalPolicySameHost)
	receipt, err := launchTestBackground(t, job)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ApprovalPolicy != db.ApprovalPolicySameHost || receipt.Assurance != db.AssuranceSameHost ||
		!strings.Contains(receipt.ApprovalPolicyLabel, "not independently authenticated") {
		t.Fatalf("startup receipt lost low-assurance classification: %+v", receipt)
	}
	request := waitBackgroundOutcome(t, conn, job.RequestID)
	if request.Status != db.StatusExecuted || request.Execution == nil || request.Execution.Assurance != db.AssuranceSameHost ||
		request.Execution.ApprovalPolicy != db.ApprovalPolicySameHost {
		t.Fatalf("detached outcome lost low-assurance classification: %+v", request)
	}
	log, err := os.ReadFile(receipt.LogPath)
	if err != nil || !strings.Contains(string(log), `"approval_policy":"same_host"`) ||
		!strings.Contains(string(log), `"assurance":"same_host"`) {
		t.Fatalf("execution log lost low-assurance classification: %s %v", log, err)
	}
}

func TestExecuteBackgroundUsesJobNotificationConfig(t *testing.T) {
	for _, journal := range []bool{true, false} {
		t.Run(fmt.Sprintf("journal=%t", journal), func(t *testing.T) {
			conn, job := backgroundExecutionFixture(t, "printf 'selected background config\\n'", db.ApprovalPolicySameHost)
			marker := installAgentMailCapture(t)
			t.Setenv("SLB_AGENT_MAIL_ENABLED", "true")
			t.Setenv("SLB_AGENT_MAIL_THREAD", "")
			projectConfig := fmt.Sprintf("[general]\napproval_policy = 'same_host'\n[notifications.requests]\nenabled = %t\n", !journal)
			if err := os.WriteFile(filepath.Join(job.Project, ".slb", "config.toml"), []byte(projectConfig), 0600); err != nil {
				t.Fatal(err)
			}
			job.Config = filepath.Join(t.TempDir(), "selected.toml")
			selectedConfig := fmt.Sprintf("[general]\napproval_policy = 'same_host'\n[notifications.requests]\nenabled = %t\n[integrations]\nagent_mail_thread = 'background-selected'\n", journal)
			if err := os.WriteFile(job.Config, []byte(selectedConfig), 0600); err != nil {
				t.Fatal(err)
			}
			oldConfig := flagConfig
			flagConfig = ""
			t.Cleanup(func() { flagConfig = oldConfig })
			result, err := executeBackgroundJob(context.Background(), job, func(core.ExecutionStart) error { return nil })
			if err != nil || result == nil || result.ExitCode != 0 {
				t.Fatalf("background execution failed: %+v (%v)", result, err)
			}
			request, err := conn.GetRequest(job.RequestID)
			if err != nil || request.Status != db.StatusExecuted || request.Execution == nil || request.Execution.Assurance != db.AssuranceSameHost {
				t.Fatalf("background evidence missing: %+v (%v)", request, err)
			}
			data, err := os.ReadFile(marker)
			if journal {
				if !os.IsNotExist(err) {
					t.Fatalf("background job used legacy transport: %s (%v)", data, err)
				}
			} else if err != nil || strings.Count(string(data), "CALL\n") != 1 || !strings.Contains(string(data), "--thread\nbackground-selected\n") {
				t.Fatalf("background job lost selected legacy route: %s (%v)", data, err)
			}
		})
	}
}

func TestExecuteBackgroundInvalidSelectedConfigDoesNotClaim(t *testing.T) {
	conn, job := backgroundExecutionFixture(t, "printf 'must not execute\\n'", db.ApprovalPolicySameHost)
	marker := installAgentMailCapture(t)
	job.Config = filepath.Join(t.TempDir(), "invalid.toml")
	if err := os.WriteFile(job.Config, []byte("[notifications.requests\ninvalid"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := executeBackgroundJob(context.Background(), job, func(core.ExecutionStart) error {
		t.Fatal("invalid selected configuration launched execution")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "loading background config") || result == nil || result.LogPath != "" {
		t.Fatalf("invalid selected configuration was accepted: %+v (%v)", result, err)
	}
	request, err := conn.GetRequest(job.RequestID)
	if err != nil || request.Status != db.StatusApproved || request.Execution != nil {
		t.Fatalf("invalid selected config consumed approval: %+v (%v)", request, err)
	}
	if data, err := os.ReadFile(marker); !os.IsNotExist(err) {
		t.Fatalf("invalid selected config sent a notification: %s (%v)", data, err)
	}
}

func TestExecuteBackgroundSameHostRefusalRetainsStoredPolicy(t *testing.T) {
	conn, job := backgroundExecutionFixture(t, "echo background-should-not-run", db.ApprovalPolicySameHost)
	configuration := "[general]\napproval_policy = 'independent'\n[integrations]\nagent_mail_enabled = false\n"
	if err := os.WriteFile(filepath.Join(job.Project, ".slb", "config.toml"), []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := executeBackgroundJob(context.Background(), job, func(core.ExecutionStart) error {
		t.Fatal("disabled same-host background command started")
		return nil
	})
	if err == nil || result == nil || result.Request == nil || result.ExitCode != -1 ||
		result.Request.ApprovalPolicy != db.ApprovalPolicySameHost || result.Request.Execution != nil {
		t.Fatalf("preflight refusal lost stored policy or claimed execution: %+v, %v", result, err)
	}
	stored, err := conn.GetRequest(job.RequestID)
	if err != nil || stored.Status != db.StatusApproved || stored.Execution != nil || stored.ApprovalPolicy != db.ApprovalPolicySameHost {
		t.Fatalf("background refusal consumed or relabeled approval: %+v, %v", stored, err)
	}
}

func TestExecuteBackgroundSameHostReplayDoesNotReuseExecutionReceipt(t *testing.T) {
	t.Setenv(db.SimulatedIdentityEnvForTest, "")
	conn, job := backgroundExecutionFixture(t, "printf 'same-host-execute-once\\n'", db.ApprovalPolicySameHost)
	if _, err := launchTestBackground(t, job); err != nil {
		t.Fatal(err)
	}
	completed := waitBackgroundOutcome(t, conn, job.RequestID)
	if completed.Status != db.StatusExecuted || completed.Execution == nil {
		t.Fatalf("first execution did not complete: %+v", completed)
	}
	replay, err := launchTestBackground(t, job)
	var refusal *background.StartError
	if replay != nil || !errors.As(err, &refusal) || refusal.SupervisorLog == "" {
		t.Fatalf("replay did not return an inspectable refusal: %+v %v", replay, err)
	}
	data, err := os.ReadFile(refusal.SupervisorLog)
	if err != nil {
		t.Fatal(err)
	}
	var outcome map[string]any
	for _, line := range strings.Split(string(data), "\n") {
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) == nil && event["event"] == "background_execution_finished" {
			outcome = event
		}
	}
	if outcome["status"] != "not_started" || outcome["request_status"] != string(db.StatusExecuted) ||
		outcome["startup_confirmed"] != false || outcome["execution_claimed"] != false || outcome["approval_policy"] != "same_host" {
		t.Fatalf("replay refusal borrowed the previous execution status: %s", data)
	}
	for _, field := range []string{"assurance", "log_path", "exit_code"} {
		if _, present := outcome[field]; present {
			t.Fatalf("refused replay borrowed previous execution field %q: %s", field, data)
		}
	}
	stored, err := conn.GetRequest(job.RequestID)
	if err != nil || stored.Status != completed.Status || !reflect.DeepEqual(stored.Execution, completed.Execution) {
		t.Fatalf("refused replay changed historical execution: %+v %v", stored, err)
	}
}

func launchTestBackground(t *testing.T, job background.Job) (*background.Receipt, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return background.Launch(ctx, os.Args[0], []string{"-test.run=^TestExecuteBackgroundWorkerProcess$"}, job)
}

func waitBackgroundOutcome(t *testing.T, conn *db.DB, id string) *db.Request {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		request, err := conn.GetRequest(id)
		if err != nil {
			t.Fatal(err)
		}
		if request.Status.IsTerminal() {
			return request
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("background worker did not persist a terminal outcome")
	return nil
}

func TestExecuteBackgroundPreservesEnvironmentCWDAndLogs(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	conn, job := backgroundExecutionFixture(t, `printf '%s\n' "$SLB_BG_TEST_VALUE"; pwd; if read ignored; then exit 17; fi; sleep 0.2`)
	t.Setenv("SLB_BG_TEST_VALUE", "background-output-is-not-launch-json")
	receipt, err := launchTestBackground(t, job)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.RequestID != job.RequestID || receipt.CommandHash != job.CommandHash || receipt.PID <= 0 || receipt.SupervisorPID <= 0 {
		t.Fatalf("missing startup identity: %+v", receipt)
	}
	request := waitBackgroundOutcome(t, conn, job.RequestID)
	if request.Status != db.StatusExecuted || request.Execution == nil || request.Execution.ExitCode == nil || *request.Execution.ExitCode != 0 || request.Execution.LogPath != receipt.LogPath {
		t.Fatalf("completion missing: %+v", request)
	}
	data, err := os.ReadFile(receipt.LogPath)
	if err != nil || !strings.Contains(string(data), "background-output-is-not-launch-json") || !strings.Contains(string(data), job.Project) {
		t.Fatalf("environment/cwd/output lost: %s %v", data, err)
	}
	if replay, err := launchTestBackground(t, job); err == nil || replay != nil {
		t.Fatal("background approval replayed")
	}
}

func TestExecuteBackgroundFailureAndTimeoutAreDurable(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	for _, tc := range []struct {
		name, script string
		timeout      int64
		status       db.RequestStatus
		exit         int
	}{
		{"nonzero", "exit 7", 10, db.StatusExecutionFailed, 7},
		{"timeout", "sleep 30", 1, db.StatusTimedOut, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, job := backgroundExecutionFixture(t, tc.script)
			job.TimeoutSeconds = tc.timeout
			if _, err := launchTestBackground(t, job); err != nil {
				t.Fatal(err)
			}
			request := waitBackgroundOutcome(t, conn, job.RequestID)
			if request.Status != tc.status || request.Execution == nil || request.Execution.ExitCode == nil || *request.Execution.ExitCode != tc.exit || request.Execution.DurationMs == nil {
				t.Fatalf("wrong background outcome: %+v", request)
			}
		})
	}
}

func TestExecuteBackgroundRevalidatesBeforeStarting(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	for _, fault := range []string{"signature", "expired", "hash", "session", "project", "policy", "missing-shell"} {
		t.Run(fault, func(t *testing.T) {
			conn, job := backgroundExecutionFixture(t, "echo background-should-not-run")
			switch fault {
			case "signature":
				if _, err := conn.Exec("UPDATE reviews SET signature = 'forged' WHERE request_id = ?", job.RequestID); err != nil {
					t.Fatal(err)
				}
			case "expired":
				if _, err := conn.Exec("UPDATE requests SET approval_expires_at = '2000-01-01T00:00:00Z' WHERE id = ?", job.RequestID); err != nil {
					t.Fatal(err)
				}
			case "hash":
				job.CommandHash = strings.Repeat("b", 64)
			case "session":
				job.SessionID = "unknown-session"
			case "project":
				job.Project = t.TempDir()
			case "policy":
				path := filepath.Join(job.Project, "stricter.toml")
				if err := os.WriteFile(path, []byte("[patterns.critical]\npatterns = ['^echo background-should-not-run$']\nmin_approvals = 3\ndynamic_quorum = false\n[patterns.dangerous]\nmin_approvals = 3\n"), 0600); err != nil {
					t.Fatal(err)
				}
				job.Config = path
			case "missing-shell":
				t.Setenv("SHELL", filepath.Join(t.TempDir(), "missing-shell"))
			}
			receipt, err := launchTestBackground(t, job)
			if receipt != nil || err == nil {
				t.Fatal("unusable background authorization accepted")
			}
			stored, err := conn.GetRequest(job.RequestID)
			if err != nil {
				t.Fatal(err)
			}
			if fault == "missing-shell" {
				if stored.Status != db.StatusExecutionFailed || stored.Execution == nil || stored.Execution.ExitCode != nil {
					t.Fatal("failed process start not durably recorded")
				}
			} else if stored.Status != db.StatusApproved || stored.Execution != nil {
				t.Fatal("rejected startup consumed approval")
			}
		})
	}
}

func TestExecuteBackgroundConcurrentSingleWinner(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	conn, job := backgroundExecutionFixture(t, "sleep 0.2; printf once")
	var wg sync.WaitGroup
	results := make(chan *background.Receipt, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			receipt, _ := background.Launch(ctx, os.Args[0], []string{"-test.run=^TestExecuteBackgroundWorkerProcess$"}, job)
			results <- receipt
		}()
	}
	wg.Wait()
	close(results)
	winners := 0
	for receipt := range results {
		if receipt != nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("got %d background winners", winners)
	}
	request := waitBackgroundOutcome(t, conn, job.RequestID)
	if request.Status != db.StatusExecuted {
		t.Fatalf("winner failed: %+v", request)
	}
}

func TestBackgroundJobRequiresStartupObserverAndTimeoutBounds(t *testing.T) {
	_, job := backgroundExecutionFixture(t, "echo unused")
	if _, err := executeBackgroundJob(context.Background(), job, nil); err == nil {
		t.Fatal("unobserved background execution permitted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := executeBackgroundJob(ctx, job, func(core.ExecutionStart) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if d, err := executionTimeoutDuration(0); err != nil || d != core.DefaultExecutionTimeout {
		t.Fatal(d, err)
	}
	if d, err := executionTimeoutDuration(600); err != nil || d != 10*time.Minute {
		t.Fatal(d, err)
	}
	if _, err := executionTimeoutDuration(-1); err == nil {
		t.Fatal("negative execution timeout accepted")
	}
	if runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64" {
		overflow := int64((1<<63-1)/time.Second) + 1
		if _, err := executionTimeoutDuration(int(overflow)); err == nil {
			t.Fatal("overflowing execution timeout accepted")
		}
	}
}

func TestBackgroundExecutionSessionShorthandDoesNotShadowTOON(t *testing.T) {
	// Use the production command objects, not an independently redeclared copy.
	// execute must not declare a local --session-id: a local flag of that name
	// shadows the root persistent --session-id/-s (cobra skips the persistent
	// flag by name), which is the collision the production-tree guard rejects.
	if local := executeCmd.LocalNonPersistentFlags().Lookup("session-id"); local != nil {
		t.Fatalf("execute declares a local --session-id (shorthand %q) that shadows the root flag", local.Shorthand)
	}
	prevSession, prevTOON := flagSessionID, flagTOON
	t.Cleanup(func() {
		flagSessionID, flagTOON = prevSession, prevTOON
		// Persistent flags are merged into execute's set during parsing, so
		// look them up afterwards to clear the parse state on the shared objects.
		for _, name := range []string{"session-id", "toon"} {
			if f := executeCmd.Flags().Lookup(name); f != nil {
				f.Changed = false
			}
		}
	})
	flagSessionID, flagTOON = "", false
	if err := executeCmd.ParseFlags([]string{"-s", "worker-session", "-t"}); err != nil {
		t.Fatal(err)
	}
	if flagSessionID != "worker-session" || !flagTOON {
		t.Fatalf("session shorthand or TOON selection was lost: session=%q toon=%v", flagSessionID, flagTOON)
	}
	if got := executeCmd.Flags().Lookup("session-id"); got == nil || got.Shorthand != "s" {
		t.Fatal("execute does not accept -s for the root --session-id")
	}
}
