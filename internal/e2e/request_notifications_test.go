package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/notifications"
)

// Exercise the real CLI and detached daemon/worker processes. The selected
// file deliberately conflicts with project defaults: one effective policy must
// govern admission, review, completion callbacks and durable delivery alike.
func TestBuiltCLINotificationConfigIntegration(t *testing.T) {
	binary := buildIdentityBoundaryCLI(t)
	f := newSameHostCLI(t, binary, 1, false)
	project, err := filepath.EvalSymlinks(f.ProjectDir)
	if err != nil {
		t.Fatal(err)
	}
	f.ProjectDir = project
	// macOS resolves /var aliases and imposes a short Unix socket path limit.
	// Keep the daemon's private temporary directory short on every platform.
	home, err := os.MkdirTemp("", "slbn-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	f.home = home
	const policy = `[general]
approval_policy = "same_host"
require_different_model = false
enable_dry_run = false
enable_rollback_capture = false
[patterns.dangerous]
min_approvals = 1
[integrations]
agent_mail_enabled = true
[notifications]
desktop_enabled = false
`
	f.WriteFile(".slb/config.toml", []byte(policy+"[notifications.requests]\nenabled = false\n"), 0600)

	marker := filepath.Join(f.home, "legacy-called")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "mcp-agent-mail"),
		[]byte("#!/bin/sh\nprintf 'called\\n' >> \"$TEST_SLB_NOTIFICATION_MARKER\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_SLB_NOTIFICATION_MARKER", marker)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	var mu sync.Mutex
	var received []notifications.RequestNotice
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
		var notice notifications.RequestNotice
		if err != nil || json.Unmarshal(data, &notice) != nil {
			t.Error("invalid request notification")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if notice.ID == "" || notice.ID != r.Header.Get("X-SLB-Event-ID") || notice.Project != f.ProjectDir {
			t.Errorf("notification lost destination/event identity: %+v", notice)
		}
		if strings.Contains(string(data), "private-notification-marker") || strings.Contains(string(data), "private-review-reason") || strings.Contains(string(data), "cursor") {
			t.Error("notification exposed command text, free-form evidence or a journal cursor")
		}
		mu.Lock()
		received = append(received, notice)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	configPath := filepath.Join(f.ProjectDir, "notification policy.toml")
	if err := os.WriteFile(configPath, []byte(policy+fmt.Sprintf(
		"[notifications.requests]\nenabled = true\nwebhook_url = %q\n", server.URL)), 0600); err != nil {
		t.Fatal(err)
	}

	requester := f.session("Requester", "model-a")
	reviewer := f.session("Reviewer", "model-b")
	create := func(command string) *db.Request {
		t.Helper()
		result := f.json("--config", configPath, "request", command, "--session-id", requester.ID,
			"--reason", "private-review-reason", "--expected-effect", "Create an isolated marker",
			"--safety", "Only a temporary test project is affected")
		id, ok := result["request_id"].(string)
		if !ok || result["status"] != "pending" {
			t.Fatalf("request was not queued for review: %+v", result)
		}
		request, err := f.DB.GetRequest(id)
		if err != nil {
			t.Fatal(err)
		}
		return request
	}
	journal := func() db.RequestEventPage {
		t.Helper()
		page, err := f.DB.ReadRequestEvents(context.Background(), f.ProjectDir, "", 100)
		if err != nil || page.HasMore {
			t.Fatalf("reading test journal: %+v %v", page, err)
		}
		return *page
	}
	approved := create("touch private-notification-marker")
	before := journal()
	f.fails("cannot review your own request", "--config", configPath, "approve", approved.ID,
		"--session-id", requester.ID, "--session-key", requester.SessionKey)
	if after := journal(); after.Cursor != before.Cursor {
		t.Fatal("refused self-review committed a journal event")
	}
	f.json("--config", configPath, "approve", approved.ID,
		"--session-id", reviewer.ID, "--session-key", reviewer.SessionKey)
	f.json("--config", configPath, "execute", approved.ID, "--session-id", requester.ID, "--background")
	deadline := time.Now().Add(10 * time.Second)
	for {
		stored, err := f.DB.GetRequest(approved.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Status == db.StatusExecuted {
			if stored.Execution == nil || stored.Execution.Assurance != db.AssuranceSameHost || stored.Execution.ApprovalPolicy != db.ApprovalPolicySameHost {
				t.Fatalf("execution lost low-assurance evidence: %+v", stored.Execution)
			}
			break
		}
		if time.Now().After(deadline) || stored.Status != db.StatusExecuting {
			t.Fatalf("background command did not complete: %+v", stored)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(f.ProjectDir, "private-notification-marker")); err != nil {
		t.Fatalf("reviewed background command did not run: %v", err)
	}
	rejected := create("touch rejected-private-notification-marker")
	f.json("--config", configPath, "reject", rejected.ID, "--session-id", reviewer.ID,
		"--session-key", reviewer.SessionKey, "--reason", "private-review-reason")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("explicit journal mode invoked the legacy transport: %v", err)
	}
	mu.Lock()
	noticesBeforeStart := len(received)
	mu.Unlock()
	if noticesBeforeStart != 0 {
		t.Fatal("request paths performed synchronous journal transport")
	}

	running := false
	t.Cleanup(func() {
		if running {
			_, stderr, err := f.run("daemon", "stop")
			if err != nil {
				t.Errorf("stopping test daemon: %v %s", err, stderr)
			}
		}
	})
	start := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		// Like other commands, -C selects the working directory for a relative
		// config. The selected file must survive background process re-exec.
		cmd := exec.CommandContext(ctx, binary, "--config", filepath.Base(configPath), "-C", f.ProjectDir,
			"daemon", "start", "--json")
		cmd.Dir, cmd.Env = f.home, identityCLIEnvironment(f.home)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("starting daemon with selected config: %v\n%s", err, output)
		}
		running = true
	}
	stop := func() {
		t.Helper()
		f.json("daemon", "stop")
		running = false
	}
	waitForDelivery := func(page db.RequestEventPage) {
		t.Helper()
		last := page.Events[len(page.Events)-1].Sequence
		ledgerPath := filepath.Join(f.ProjectDir, ".slb", "notifications", "request-delivery.json")
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			var ledger struct {
				Destinations map[string]struct{ Sequence int64 }
			}
			data, _ := os.ReadFile(ledgerPath)
			if json.Unmarshal(data, &ledger) == nil {
				for _, destination := range ledger.Destinations {
					if destination.Sequence == last {
						return
					}
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("selected daemon configuration did not deliver through journal sequence %d", last)
	}
	start()
	waitForDelivery(journal())
	stop()
	// Events committed during downtime must be delivered on the next startup,
	// and the already acknowledged prefix must not be sent again.
	create("touch offline-private-notification-marker")
	want := journal()
	start()
	waitForDelivery(want)
	stop()

	mu.Lock()
	got := append([]notifications.RequestNotice(nil), received...)
	mu.Unlock()
	if len(got) != len(want.Events) {
		t.Fatalf("restart lost or duplicated events: got %d, journal has %d", len(got), len(want.Events))
	}
	var sawApproval, sawRejection, sawExecution bool
	for i, notice := range got {
		event := want.Events[i]
		if notice.Sequence != event.Sequence || notice.RequestID != event.RequestID || notice.Event != event.Kind ||
			notice.Status != event.State.Status || notice.ApprovalPolicy != "same_host" || notice.CommandHash != event.State.CommandHash {
			t.Fatalf("notification changed committed event metadata: %+v != %+v", notice, event)
		}
		if notice.ReviewDecision != "" {
			if notice.ReviewAssurance != "same_host" {
				t.Fatalf("notification promoted local review evidence: %+v", notice)
			}
			sawApproval = sawApproval || notice.ReviewDecision == "approve"
			sawRejection = sawRejection || notice.ReviewDecision == "reject"
		}
		if notice.Status == "executed" {
			sawExecution = true
			if notice.ExecutionAssurance != "same_host" || notice.ExitCode == nil || *notice.ExitCode != 0 {
				t.Fatalf("notification lost completion evidence: %+v", notice)
			}
		}
	}
	if !sawApproval || !sawRejection || !sawExecution {
		t.Fatalf("missing lifecycle observations: approved=%t rejected=%t executed=%t", sawApproval, sawRejection, sawExecution)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("journal workflow also invoked the legacy transport: %v", err)
	}
}
