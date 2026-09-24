package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func preflightTarget(t *testing.T, project string) string {
	t.Helper()
	if _, err := exec.LookPath("ls"); err != nil {
		t.Skip("requires ls preview tool")
	}
	name := filepath.Join(project, "keep-me")
	if err := os.WriteFile(name, []byte("valuable data"), 0600); err != nil {
		t.Fatal(err)
	}
	return name
}

func preflightTool(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX preview fixture")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ls"), []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func waitForPreflightFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("preview did not reach its synchronization point")
}

func assertNoPreflightRequest(t *testing.T, creator *RequestCreator, session *db.Session, notifier *admissionNotifier) {
	t.Helper()
	count, err := creator.db.CountPendingBySession(session.ID)
	if err != nil || count != 0 || notifier.calls.Load() != 0 {
		t.Fatalf("failed preflight admitted/notified: %d %d %v", count, notifier.calls.Load(), err)
	}
}

func TestRequestPreflightPersistsEvidenceWithoutExecutingOriginal(t *testing.T) {
	creator, session, notifier := creatorAdmissionFixture(t, RateLimitActionReject)
	target := preflightTarget(t, session.ProjectPath)
	opts := creatorAdmissionOptions(session)
	opts.Command, opts.Shell, opts.RequireDryRun = "rm -rf ./keep-me", true, true
	backing := make([]db.Attachment, 1, 2)
	backing[0] = db.Attachment{Type: db.AttachmentTypeContext, Content: "user evidence"}
	opts.Attachments = backing
	result, err := creator.CreateRequestContext(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if result.Preflight == nil || result.Preflight.Status != "succeeded" || result.Request.Status != db.StatusPending || result.Preflight.CommandHash != result.Request.Command.Hash {
		t.Fatalf("wrong preflight: %+v", result)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "valuable data" {
		t.Fatalf("original deletion executed: %q %v", data, err)
	}
	if notifier.calls.Load() != 1 {
		t.Fatal("request did not notify after admission")
	}
	if len(backing) != 1 || backing[:2][1].Content != "" {
		t.Fatal("generated evidence overwrote caller attachment storage")
	}
	var raw, hash, command, output, attachments, status string
	err = creator.db.QueryRow(`SELECT command_raw,command_hash,dry_run_command,dry_run_output,attachments_json,status FROM requests WHERE id=?`, result.Request.ID).Scan(&raw, &hash, &command, &output, &attachments, &status)
	if err != nil {
		t.Fatal(err)
	}
	var stored []db.Attachment
	if err := json.Unmarshal([]byte(attachments), &stored); err != nil {
		t.Fatal(err)
	}
	if raw != opts.Command || hash != result.Preflight.CommandHash || !strings.HasPrefix(command, "ls -la --") || !strings.Contains(output, "SLB preflight: succeeded") || status != "pending" || len(stored) != 2 {
		t.Fatalf("incomplete persisted evidence: %s %s %s %s", raw, command, status, attachments)
	}
	meta := stored[1].Metadata
	if meta["kind"] != "preflight" || meta["status"] != "succeeded" || meta["command_hash"] != hash || meta["completed_at"] == nil {
		t.Fatalf("missing typed evidence: %+v", meta)
	}
	if result.Request.CreatedAt.Before(result.Preflight.CompletedAt) || result.Request.ExpiresAt.Sub(result.Request.CreatedAt) < creator.requestLifetime()-100*time.Millisecond {
		t.Fatal("preflight consumed the review lifetime")
	}
}

func TestRequestPreflightRequiredAndAdvisoryOutcomes(t *testing.T) {
	for _, name := range []string{"failed-advisory", "failed-required", "unsupported-advisory", "unsupported-required", "disabled", "disabled-required", "negative-timeout"} {
		t.Run(name, func(t *testing.T) {
			creator, session, notifier := creatorAdmissionFixture(t, RateLimitActionReject)
			opts := creatorAdmissionOptions(session)
			opts.Command = "rm -rf ./definitely-missing-preview-target"
			opts.Shell = true
			opts.RequireDryRun = strings.HasSuffix(name, "required")
			wantStatus := "failed"
			if strings.HasPrefix(name, "unsupported") {
				opts.Command = "opaque-operation --target project"
				wantStatus = "unsupported"
			}
			if strings.HasPrefix(name, "disabled") {
				creator.config.EnableDryRun = false
				wantStatus = "disabled"
			}
			if name == "negative-timeout" {
				opts.DryRunTimeout = -time.Second
			}
			result, err := creator.CreateRequest(opts)
			if opts.RequireDryRun || name == "negative-timeout" {
				if err == nil || result != nil {
					t.Fatalf("missing required evidence was admitted: %+v %v", result, err)
				}
				if opts.RequireDryRun && !errors.Is(err, ErrPreflightRequired) {
					t.Fatalf("missing typed preflight error: %v", err)
				}
				assertNoPreflightRequest(t, creator, session, notifier)
				return
			}
			if err != nil || result.Preflight.Status != wantStatus || result.Request.Status != db.StatusPending {
				t.Fatalf("advisory outcome: %+v %v", result, err)
			}
			if wantStatus == "failed" && (result.Request.DryRun == nil || !strings.Contains(result.Request.DryRun.Output, "Preview error:")) {
				t.Fatal("failed preview looked successful to reviewers")
			}
		})
	}
}

func TestRequestPreflightRedactsOutputAndDiagnostics(t *testing.T) {
	creator, session, _ := creatorAdmissionFixture(t, RateLimitActionReject)
	preflightTool(t, "printf 'password=topsecret\ncustomer-secret-value\n'; printf 'token=private-token\n' >&2; exit 7")
	opts := creatorAdmissionOptions(session)
	opts.Command = "rm -rf ./customer-secret-value"
	opts.RedactPatterns = []string{`customer-secret-value`}
	result, err := creator.CreateRequest(opts)
	if err != nil {
		t.Fatal(err)
	}
	if result.Preflight.Status != "failed" {
		t.Fatal(result.Preflight)
	}
	data, err := json.Marshal(result.Request)
	if err != nil {
		t.Fatal(err)
	}
	// Raw command is intentionally retained for execution. All NEW preview
	// evidence, including the preview command and attachment, must be redacted.
	preview, _ := json.Marshal(struct {
		DryRun      *db.DryRunResult
		Report      *PreflightReport
		Attachments []db.Attachment
	}{result.Request.DryRun, result.Preflight, result.Request.Attachments})
	for _, secret := range []string{"topsecret", "private-token", "customer-secret-value"} {
		if strings.Contains(string(preview), secret) {
			t.Fatalf("preview leaked %q: %s", secret, preview)
		}
	}
	if !strings.Contains(string(data), "SLB preflight: failed") || !strings.Contains(string(preview), "[REDACTED]") {
		t.Fatal("redaction/evidence missing")
	}
}

func TestRequestPreflightCancellationTimeoutAndNoEarlyVisibility(t *testing.T) {
	for _, mode := range []string{"cancel", "timeout-advisory", "timeout-required", "ended-session", "quota-race"} {
		t.Run(mode, func(t *testing.T) {
			creator, session, notifier := creatorAdmissionFixture(t, RateLimitActionReject)
			started := filepath.Join(t.TempDir(), "started")
			release := filepath.Join(t.TempDir(), "release")
			t.Setenv("PREFLIGHT_STARTED", started)
			t.Setenv("PREFLIGHT_RELEASE", release)
			preflightTool(t, "printf started > \"$PREFLIGHT_STARTED\"; while [ ! -f \"$PREFLIGHT_RELEASE\" ]; do sleep 0.01; done; printf preview")
			opts := creatorAdmissionOptions(session)
			if strings.HasPrefix(mode, "timeout") {
				// Long enough for the preview process to reach its
				// synchronization point (macOS process start alone can exceed
				// 80ms), still far below the 2s the test waits for completion;
				// the release file is never written in timeout modes.
				opts.DryRunTimeout = 750 * time.Millisecond
			}
			opts.RequireDryRun = mode == "timeout-required"
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			done := make(chan queuedRequestResult, 1)
			go func() { r, e := creator.CreateRequestContext(ctx, opts); done <- queuedRequestResult{r, e} }()
			waitForPreflightFile(t, started)
			assertNoPreflightRequest(t, creator, session, notifier)
			switch mode {
			case "cancel":
				cancel()
			case "ended-session":
				if err := creator.db.EndSession(session.ID); err != nil {
					t.Fatal(err)
				}
			case "quota-race":
				seed := &db.Request{ProjectPath: session.ProjectPath, RequestorSessionID: session.ID, RequestorAgent: session.AgentName, RequestorModel: session.Model, Command: db.CommandSpec{Raw: "competing request", Cwd: session.ProjectPath}, RiskTier: db.RiskTierDangerous, MinApprovals: 1, Justification: db.Justification{Reason: "race"}}
				if _, err := creator.db.AdmitRequest(context.Background(), seed, db.RequestAdmissionLimits{MaxPending: 1, MaxPerMinute: 10}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "ended-session" || mode == "quota-race" {
				if err := os.WriteFile(release, []byte("go"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var finished queuedRequestResult
			select {
			case finished = <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("preflight did not finish")
			}
			if mode == "timeout-advisory" {
				if finished.err != nil || finished.result.Preflight.Status != "timed_out" || notifier.calls.Load() != 1 {
					t.Fatalf("timeout evidence lost: %+v", finished)
				}
				return
			}
			if finished.err == nil || finished.result != nil || notifier.calls.Load() != 0 {
				t.Fatalf("invalid preflight committed: %+v", finished)
			}
			if mode == "cancel" && !errors.Is(finished.err, context.Canceled) {
				t.Fatal(finished.err)
			}
			if mode == "timeout-required" && !errors.Is(finished.err, ErrPreflightRequired) {
				t.Fatal(finished.err)
			}
			if mode != "quota-race" {
				assertNoPreflightRequest(t, creator, session, notifier)
			}
		})
	}
}

func TestRequestPreflightQuotaPrecheckDoesNotLaunchTools(t *testing.T) {
	creator, session, notifier := creatorAdmissionFixture(t, RateLimitActionQueue)
	creator.config.EnableDryRun = false
	if _, err := creator.CreateRequest(creatorAdmissionOptions(session)); err != nil {
		t.Fatal(err)
	}
	creator.config.EnableDryRun = true
	marker := filepath.Join(t.TempDir(), "unexpected-tool")
	t.Setenv("PREFLIGHT_STARTED", marker)
	preflightTool(t, "printf started > \"$PREFLIGHT_STARTED\"")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := creator.CreateRequestContext(ctx, creatorAdmissionOptions(session)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("full queue ran a preview: %v", err)
	}
	if notifier.calls.Load() != 1 {
		t.Fatal("queued preview notified")
	}
}

func TestRequestPreflightNeverEvaluatesShellSyntax(t *testing.T) {
	for _, raw := range []string{`rm -rf ./keep-me; touch marker`, `rm -rf "$(touch marker)"`, "rm -rf `touch marker`", `rm -rf ./keep-me > marker`} {
		t.Run(raw, func(t *testing.T) {
			creator, session, _ := creatorAdmissionFixture(t, RateLimitActionReject)
			target := preflightTarget(t, session.ProjectPath)
			opts := creatorAdmissionOptions(session)
			opts.Command = raw
			opts.Shell = true
			result, err := creator.CreateRequest(opts)
			if err != nil || result.Preflight.Status != "unsupported" {
				t.Fatalf("nonliteral command was previewed: %+v %v", result, err)
			}
			if _, err := os.Stat(target); err != nil {
				t.Fatal("original command executed")
			}
			if _, err := os.Stat(filepath.Join(session.ProjectPath, "marker")); !os.IsNotExist(err) {
				t.Fatal("preview evaluated shell syntax")
			}
		})
	}
}

func TestRunDryRunContextPrecancelledAndAuthoritativeArgv(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := RunDryRunContext(ctx, &db.CommandSpec{Raw: "rm -rf target"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	project := t.TempDir()
	target := preflightTarget(t, project)
	preview, err := RunDryRunContext(context.Background(), &db.CommandSpec{Raw: "display does not control preview", Argv: []string{"rm", "--", target}, Cwd: project})
	if err != nil || preview == nil || !strings.Contains(preview.Output, "keep-me") {
		t.Fatalf("argv was not authoritative: %+v %v", preview, err)
	}
}

func TestRequestPreflightSupportedToolTransformations(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX preview fixtures")
	}
	for _, tc := range []struct{ name, command, want string }{
		// "kubectl delete pod" is a builtin SAFE pattern (controllers recreate
		// pods), so it is skipped without a request; use a deployment.
		{"kubectl", "kubectl delete deployment obsolete --dry-run=none", "delete --dry-run=client -o yaml deployment obsolete"},
		{"terraform", "terraform destroy -auto-approve", "plan -destroy -input=false"},
		{"git", "git reset --hard HEAD~1", "diff --no-ext-diff --no-textconv HEAD~1..HEAD"},
		{"helm", "helm uninstall old-release --namespace team", "get manifest old-release --namespace=team"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			creator, session, _ := creatorAdmissionFixture(t, RateLimitActionReject)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, tc.name), []byte("#!/bin/sh\nprintf '%s' \"$*\"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			opts := creatorAdmissionOptions(session)
			opts.Command = tc.command
			opts.Shell = true
			opts.RequireDryRun = true
			result, err := creator.CreateRequest(opts)
			if err != nil {
				t.Fatal(err)
			}
			if result.Request == nil || result.Preflight == nil || result.Request.DryRun == nil {
				t.Fatalf("no request/preflight evidence was produced: %+v", result)
			}
			if result.Preflight.Status != "succeeded" || !strings.HasSuffix(result.Request.DryRun.Output, tc.want) {
				t.Fatalf("incorrect preapproval argv: %+v", result.Request.DryRun)
			}
			if result.Request.Command.Raw != tc.command {
				t.Fatal("preview rewrote the approved command")
			}
		})
	}
}
