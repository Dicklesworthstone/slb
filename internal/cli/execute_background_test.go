package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/background"
	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/spf13/cobra"
)

// Invoke the real worker handler in a separate test process. This exercises
// actual SQLite, signed approval checks, process execution and completion.
func TestExecuteBackgroundWorkerProcess(t *testing.T) {
	if os.Getenv("SLB_TEST_EXECUTE_BACKGROUND_WORKER") != "1" {
		return
	}
	command := newBackgroundWorkerCmd()
	command.SetArgs([]string{})
	if err := command.ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func backgroundExecutionFixture(t *testing.T, script string) (*db.DB, background.Job) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("detached POSIX execution test")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("SLB_TEST_EXECUTE_BACKGROUND_WORKER", "1")
	project := t.TempDir()
	conn, err := db.OpenAndMigrate(filepath.Join(project, ".slb", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := os.WriteFile(filepath.Join(project, ".slb", "config.toml"), []byte("[integrations]\nagent_mail_enabled = false\n[general]\nenable_rollback_capture = false\n"), 0600); err != nil {
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
		RequestorSessionID: requester.ID, RequestorAgent: requester.AgentName, RequestorModel: requester.Model,
		Status: db.StatusApproved, RiskTier: db.RiskTierCritical, MinApprovals: 2, ApprovalExpiresAt: &expires,
		Justification: db.Justification{Reason: "background execution regression in a temporary project"}}
	if err := conn.CreateRequest(request); err != nil {
		t.Fatal(err)
	}
	for _, reviewer := range reviewers {
		now := time.Now().UTC()
		review := &db.Review{RequestID: request.ID, ReviewerSessionID: reviewer.ID, ReviewerAgent: reviewer.AgentName, ReviewerModel: reviewer.Model,
			Decision: db.DecisionApprove, SignatureTimestamp: now, Signature: db.ComputeReviewSignature(reviewer.SessionKey, request.ID, db.DecisionApprove, now)}
		if err := conn.CreateReview(review); err != nil {
			t.Fatal(err)
		}
	}
	return conn, background.Job{Version: background.Version, RequestID: request.ID, SessionID: requester.ID, CommandHash: request.Command.Hash,
		Database: conn.Path(), Project: project, LogDir: filepath.Join(project, ".slb", "logs"), TimeoutSeconds: 10}
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
	// Use the production flag object, not an independently redeclared copy.
	flag := executeCmd.Flags().Lookup("session-id")
	if flag == nil || flag.Shorthand != "s" {
		t.Fatal("execute shadows the root session flag without preserving -s")
	}
	previous, changed := flag.Value.String(), flag.Changed
	t.Cleanup(func() {
		if err := flag.Value.Set(previous); err != nil {
			t.Error(err)
		}
		flag.Changed = changed
	})
	var parentSession string
	var toon bool
	root := &cobra.Command{Use: "slb", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().StringVarP(&parentSession, "session-id", "s", "", "session")
	root.PersistentFlags().BoolVarP(&toon, "toon", "t", false, "structured output")
	child := &cobra.Command{Use: "execute", Run: func(*cobra.Command, []string) {}}
	child.Flags().AddFlag(flag)
	root.AddCommand(child)
	root.SetArgs([]string{"execute", "-s", "worker-session", "-t"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if flagExecuteSessionID != "worker-session" || parentSession != "" || !toon {
		t.Fatal("session shorthand or TOON selection was lost")
	}
}
