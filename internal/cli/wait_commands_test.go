package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
