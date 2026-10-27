package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/testutil"
)

func TestApprovalWaitDurationRejectsInvalidAndOverflow(t *testing.T) {
	for _, seconds := range []int{-1, 0, int(^uint(0) >> 1)} {
		if seconds > 0 && int64(seconds) <= int64((1<<63-1)/time.Second) {
			continue // On 32-bit platforms every positive int fits.
		}
		if _, err := approvalWaitDuration(seconds); err == nil {
			t.Fatalf("invalid seconds accepted: %d", seconds)
		}
	}
	if got, err := approvalWaitDuration(7); err != nil || got != 7*time.Second {
		t.Fatalf("valid duration rejected: %v %v", got, err)
	}
}

func TestRequestWaitApprovesWithoutDaemon(t *testing.T) {
	h := testutil.NewHarness(t)
	resetRequestFlags()
	t.Cleanup(resetRequestFlags)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	session := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir), testutil.WithAgent("wait-requester"))
	configPath := filepath.Join(h.ProjectDir, "wait-policy.toml")
	if err := os.WriteFile(configPath, []byte("[general]\nenable_dry_run = false\nrequire_different_model = false\n[patterns.caution]\nauto_approve_delay_seconds = 0\nmin_approvals = 0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := newTestRequestCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "request", "git branch -d obsolete",
		"-C", h.ProjectDir, "-c", configPath, "-s", session.ID, "--reason", "test daemonless wait", "--wait", "--timeout", "2", "-j")
	if err != nil {
		t.Fatalf("daemonless request wait failed: %v\n%s", err, stdout)
	}
	var response map[string]any
	if err := json.Unmarshal([]byte(stdout), &response); err != nil {
		t.Fatal(err)
	}
	id, ok := response["request_id"].(string)
	if !ok || response["status"] != "approved" {
		t.Fatalf("request did not resolve: %+v", response)
	}
	stored, err := h.DB.GetRequest(id)
	if err != nil || stored.Status != db.StatusApproved || stored.ApprovalExpiresAt == nil || stored.Execution != nil {
		t.Fatalf("invalid persisted decision: %+v %v", stored, err)
	}
}

func TestWaitingCommandsRefreshNotificationPolicyBeforeExecution(t *testing.T) {
	for _, name := range []string{"request", "run"} {
		t.Run(name, func(t *testing.T) {
			resetRequestFlags()
			resetRunFlags()
			t.Cleanup(resetRequestFlags)
			t.Cleanup(resetRunFlags)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("USERPROFILE", t.TempDir())
			t.Setenv("SHELL", "/bin/sh")
			t.Setenv("SLB_AGENT_MAIL_ENABLED", "true")
			marker := installAgentMailCapture(t)
			h := testutil.NewHarness(t)
			t.Chdir(h.ProjectDir)
			author := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir), testutil.WithAgent("waiting-author"), testutil.WithModel("author-model"))
			reviewer := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir), testutil.WithAgent("waiting-reviewer"), testutil.WithModel("review-model"))
			selected := filepath.Join(h.ProjectDir, "selected.toml")
			policy := "[general]\napproval_policy = 'same_host'\nenable_dry_run = false\nenable_rollback_capture = false\n[patterns.dangerous]\npatterns = ['^printf slb_notification_wait$']\n[notifications.requests]\nenabled = %t\n"
			if err := os.WriteFile(selected, []byte(fmt.Sprintf(policy, false)), 0600); err != nil {
				t.Fatal(err)
			}
			var requestID string
			reviewDone := make(chan error, 1)
			go func() {
				reviewDone <- func() error {
					deadline := time.Now().Add(5 * time.Second)
					for time.Now().Before(deadline) {
						pending, err := h.DB.ListPendingRequests(h.ProjectDir)
						if err != nil {
							return err
						}
						if len(pending) == 1 {
							requestID = pending[0].ID
							// Publish a complete replacement so the waiter never
							// observes a transient, partially written TOML file.
							if err := os.WriteFile(selected+".next", []byte(fmt.Sprintf(policy, true)), 0600); err != nil {
								return err
							}
							if err := os.Rename(selected+".next", selected); err != nil {
								return err
							}
							_, err = h.DB.ApplyReview(&db.Review{RequestID: requestID, ReviewerSessionID: reviewer.ID, Decision: db.DecisionApprove}, reviewer.SessionKey,
								db.ReviewPolicy{ApprovalPolicy: db.ApprovalPolicySameHost})
							return err
						}
						time.Sleep(10 * time.Millisecond)
					}
					return errors.New("request did not arrive for review")
				}()
			}()
			root := newTestRequestCmd(h.DBPath)
			args := []string{name, "printf slb_notification_wait", "-C", h.ProjectDir, "--config", selected,
				"--session-id", author.ID, "--reason", "exercise notification policy refresh", "--timeout", "5", "--json"}
			if name == "request" {
				args = append(args, "--wait", "--execute")
			} else {
				root = newTestRunCmd(h.DBPath)
			}
			stdout, err := executeCommandCapture(t, root, args...)
			if reviewErr := <-reviewDone; reviewErr != nil {
				t.Fatalf("review failed: %v (%s)", reviewErr, stdout)
			}
			if err != nil {
				t.Fatalf("waiting command failed: %v (%s)", err, stdout)
			}
			request, err := h.DB.GetRequest(requestID)
			if err != nil || request.Status != db.StatusExecuted || request.Execution == nil || request.Execution.Assurance != db.AssuranceSameHost {
				t.Fatalf("waiting command lost execution evidence: %+v (%v)", request, err)
			}
			data, err := os.ReadFile(marker)
			if err != nil || strings.Count(string(data), "CALL\n") != 1 || strings.Contains(string(data), "[SLB] EXECUTED") {
				t.Fatalf("execution used admission-time notification policy: %s (%v)", data, err)
			}
		})
	}
}

func TestRunClientTimeoutLeavesRequestPending(t *testing.T) {
	h := testutil.NewHarness(t)
	resetRunFlags()
	t.Cleanup(resetRunFlags)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	session := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir), testutil.WithAgent("wait-requester"))
	configPath := filepath.Join(h.ProjectDir, "wait-policy.toml")
	if err := os.WriteFile(configPath, []byte("[general]\nenable_dry_run = false\n[patterns.dangerous]\npatterns = ['^printf slb_wait_never_execute$']\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := newTestRunCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "run", "printf slb_wait_never_execute",
		"-C", h.ProjectDir, "-c", configPath, "-s", session.ID, "--reason", "test local deadline", "--timeout", "1", "-j")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected local wait timeout: %v\n%s", err, stdout)
	}
	var count int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM requests WHERE requestor_session_id = ?
		AND status = 'pending' AND resolved_at IS NULL AND approval_expires_at IS NULL`, session.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("local deadline overwrote request: pending=%d err=%v", count, err)
	}
}

func TestRequestExecuteRequiresWaitBeforeCreatingAnything(t *testing.T) {
	h := testutil.NewHarness(t)
	resetRequestFlags()
	t.Cleanup(resetRequestFlags)
	cmd := newTestRequestCmd(h.DBPath)
	if _, _, err := executeCommand(cmd, "request", "git branch -d obsolete", "--execute"); err == nil {
		t.Fatal("execute without wait was silently ignored")
	}
	var count int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("invalid invocation created a request: %d %v", count, err)
	}
}
