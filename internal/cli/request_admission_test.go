package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/spf13/cobra"
)

func cliAdmissionFixture(t *testing.T, action core.RateLimitAction) (*core.RequestCreator, *db.DB, core.CreateRequestOptions) {
	t.Helper()
	project := t.TempDir()
	database, err := db.OpenAndMigrate(filepath.Join(project, "admission.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	session := &db.Session{AgentName: "cli-admission", ProjectPath: project, Model: "model-a"}
	if err := database.CreateSession(session); err != nil {
		t.Fatal(err)
	}
	cfg := core.DefaultRequestCreatorConfig()
	cfg.AgentMailEnabled = false
	limiter := core.NewRateLimiter(database, core.RateLimitConfig{MaxPendingPerSession: 1, MaxRequestsPerMinute: 10, Action: action})
	creator := core.NewRequestCreator(database, limiter, core.NewPatternEngine(), cfg)
	opts := core.CreateRequestOptions{SessionID: session.ID, ProjectPath: project, Cwd: project, Command: "rm -rf ./build", Justification: core.Justification{Reason: "test only; no execution"}}
	return creator, database, opts
}

func TestCLIAdmissionTimeoutAndCancellation(t *testing.T) {
	for _, name := range []string{"queue-timeout", "parent-cancel", "reject"} {
		t.Run(name, func(t *testing.T) {
			action := core.RateLimitActionQueue
			if name == "reject" {
				action = core.RateLimitActionReject
			}
			creator, database, opts := cliAdmissionFixture(t, action)
			if _, err := creator.CreateRequest(opts); err != nil {
				t.Fatal(err)
			}
			cmd := &cobra.Command{}
			cmd.Flags().Duration("queue-timeout", 60*time.Millisecond, "test")
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			cmd.SetContext(parent)
			finish, err := beginRequestCommand(cmd)
			if err != nil {
				t.Fatal(err)
			}
			defer finish()
			if name == "parent-cancel" {
				cancel()
			}
			result, err := submitRequestWithCapacity(cmd, creator, opts)
			if result != nil || err == nil {
				t.Fatalf("full admission succeeded: %+v %v", result, err)
			}
			code := requestAdmissionFailureResponse(err)["code"]
			expected := map[string]string{"queue-timeout": "queue_timeout", "parent-cancel": "admission_cancelled", "reject": "rate_limit_exceeded"}[name]
			if code != expected {
				t.Fatalf("code=%v want=%s err=%v", code, expected, err)
			}
			count, countErr := database.CountPendingBySession(opts.SessionID)
			if countErr != nil || count != 1 {
				t.Fatalf("unsuccessful CLI submission created a request: %d %v", count, countErr)
			}
			if name == "queue-timeout" && cmd.Context().Err() != nil {
				t.Fatal("admission deadline leaked into the command's approval/execution context")
			}
		})
	}
}

func TestCLIAdmissionKeepsCommandContextAndRestoresIt(t *testing.T) {
	creator, _, opts := cliAdmissionFixture(t, core.RateLimitActionQueue)
	cmd := &cobra.Command{}
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd.SetContext(parent)
	finish, err := beginRequestCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	active := cmd.Context()
	if _, err := submitRequestWithCapacity(cmd, creator, opts); err != nil {
		t.Fatal(err)
	}
	if cmd.Context() != active || active.Err() != nil {
		t.Fatal("successful admission stopped the rest of the command")
	}
	cancel()
	select {
	case <-active.Done():
	case <-time.After(time.Second):
		t.Fatal("parent cancellation did not reach the execution context")
	}
	finish()
	if cmd.Context() != parent {
		t.Fatal("command context was not restored")
	}
}

func TestCLIAdmissionQueueFlags(t *testing.T) {
	for _, cmd := range []*cobra.Command{requestCmd, runCmd} {
		if cmd.Flags().Lookup("queue-timeout") == nil || !strings.Contains(cmd.Long, "No request ID") {
			t.Fatal("missing queue flag or admission semantics in command help")
		}
	}
	cmd := &cobra.Command{}
	if d, err := requestQueueTimeout(cmd); err != nil || d != 0 {
		t.Fatal("bare RunE command is incompatible")
	}
	cmd.Flags().Duration("queue-timeout", -time.Second, "test")
	if finish, err := beginRequestCommand(cmd); err == nil || finish != nil || cmd.Context() != nil {
		t.Fatal("negative timeout was not rejected before side effects")
	}
}

type admissionBrokenWriter struct{}

func (admissionBrokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestCLIAdmissionWarningsNeverHideCommittedRequests(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(fmt.Sprint(broken), func(t *testing.T) {
			creator, database, opts := cliAdmissionFixture(t, core.RateLimitActionWarn)
			if _, err := creator.CreateRequest(opts); err != nil {
				t.Fatal(err)
			}
			cmd := &cobra.Command{}
			var warning bytes.Buffer
			cmd.SetErr(&warning)
			if broken {
				cmd.SetErr(admissionBrokenWriter{})
			}
			result, err := submitRequestWithCapacity(cmd, creator, opts)
			if err != nil || result.Request == nil {
				t.Fatalf("warning hid committed admission: %+v %v", result, err)
			}
			if !broken && !strings.Contains(warning.String(), "Rate limit warning") {
				t.Fatal("warn-only overrun was silent")
			}
			count, err := database.CountPendingBySession(opts.SessionID)
			if err != nil || count != 2 {
				t.Fatalf("warning changed persistence: %d %v", count, err)
			}
		})
	}
}

func TestCLIAdmissionFailurePayloadNeverInventsRequest(t *testing.T) {
	limit := &core.RateLimitError{SessionID: "session", Pending: 5, MaxPending: 5, Recent: 8, MaxPerMinute: 10, ResetAt: time.Now().UTC()}
	for _, tc := range []struct {
		err  error
		code string
	}{
		{errors.New("storage failure"), "admission_failed"},
		{limit, "rate_limit_exceeded"},
		{errors.Join(context.DeadlineExceeded, limit), "queue_timeout"},
		{context.DeadlineExceeded, "admission_timeout"},
		{errors.Join(context.Canceled, limit), "admission_cancelled"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			resp := requestAdmissionFailureResponse(fmt.Errorf("submission: %w", tc.err))
			if resp["code"] != tc.code || resp["created"] != false || resp["executed"] != false || resp["request_id"] != nil || resp["status"] != "request_failed" {
				t.Fatalf("dishonest failure payload: %+v", resp)
			}
			var found *core.RateLimitError
			if errors.As(tc.err, &found) {
				details, ok := resp["rate_limit"].(map[string]any)
				if !ok || details["pending"] != 5 || details["max_per_minute"] != 10 || details["reset_at"] == nil {
					t.Fatalf("quota diagnostics lost: %+v", resp)
				}
			}
		})
	}
}

func TestCLIAdmissionSuccessMetadata(t *testing.T) {
	limit := &core.RateLimitResult{Allowed: true, RemainingPending: 2}
	resp := map[string]any{"request_id": "real-id"}
	addRequestAdmissionMetadata(resp, &core.CreateRequestResult{Queued: true, QueueWait: 1234 * time.Millisecond, RateLimit: limit})
	if resp["queued"] != true || resp["queue_wait_ms"] != int64(1234) || resp["rate_limit"] != limit || resp["request_id"] != "real-id" {
		t.Fatalf("incorrect queue metadata: %+v", resp)
	}
}
