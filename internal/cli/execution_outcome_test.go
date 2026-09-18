package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

func TestExecutionOutcomeNeverFabricatesSuccess(t *testing.T) {
	denied := errors.New("approval expired")
	storeErr := errors.New("recording execution outcome failed")
	child := func(status db.RequestStatus, code int, started bool) *core.ExecutionResult {
		request := &db.Request{Status: status}
		if started {
			request.Execution = &db.Execution{ExitCode: &code}
		}
		return &core.ExecutionResult{Request: request, ExitCode: code, Output: "child output", LogPath: "execution.log", Duration: time.Second}
	}
	timedOut := child(db.StatusTimedOut, -1, true)
	timedOut.TimedOut = true
	embeddedError := child(db.StatusExecuting, 0, true)
	embeddedError.Error = storeErr
	for _, tc := range []struct {
		name      string
		result    *core.ExecutionResult
		err       error
		status    string
		code      int
		executed  bool
		wantError bool
	}{
		{"preflight-denied", nil, denied, "not_executed", -1, false, true},
		{"missing-result", nil, nil, "not_executed", -1, false, true},
		{"success", child(db.StatusExecuted, 0, true), nil, "executed", 0, true, false},
		{"child-failure", child(db.StatusExecutionFailed, 7, true), nil, "execution_failed", 7, true, true},
		{"signal", child(db.StatusExecutionFailed, -1, true), nil, "execution_failed", -1, true, true},
		{"failed-start", child(db.StatusExecutionFailed, -1, false), denied, "execution_failed", -1, false, true},
		{"cancelled", child(db.StatusExecutionFailed, -1, true), context.Canceled, "execution_failed", -1, true, true},
		{"timeout", timedOut, nil, "timed_out", -1, true, true},
		{"outcome-not-durable", child(db.StatusExecuting, 0, true), storeErr, "executing", 0, true, true},
		{"embedded-error", embeddedError, nil, "executing", 0, true, true},
		{"missing-request", &core.ExecutionResult{}, nil, "unknown", 0, false, true},
		{"missing-child-evidence", child(db.StatusExecuted, 0, false), nil, "executed", 0, false, true},
		{"not-completed", child(db.StatusExecuting, 0, true), nil, "executing", 0, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view, err := executionOutcomeFor("request-id", tc.result, tc.err)
			if view.RequestID != "request-id" || view.Status != tc.status || view.ExitCode != tc.code || view.Executed != tc.executed || (err != nil) != tc.wantError {
				t.Fatalf("incorrect outcome: %+v err=%v", view, err)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Fatalf("underlying failure lost: %v", err)
			}
			if (view.Error != "") != tc.wantError {
				t.Fatalf("JSON omitted failure: %+v", view)
			}
			if tc.result != nil && view.Output != tc.result.Output {
				t.Fatal("captured output lost")
			}
			if tc.name == "child-failure" {
				var exitErr interface{ ExitCode() int }
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
					t.Fatalf("child exit code not preserved through error: %v", err)
				}
			}
		})
	}
}

func TestRunSafeJSONPreservesOutputAndFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix shell regression")
	}
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is unavailable")
	}
	oldOutput, oldJSON := flagOutput, flagJSON
	flagOutput, flagJSON = "json", true
	t.Cleanup(func() { flagOutput, flagJSON = oldOutput, oldJSON })
	for _, tc := range []struct {
		name, command, status string
		missingShell          bool
		exit                  int
	}{
		{"success", "printf machine-child-output", "executed", false, 0},
		{"child-failure", "printf machine-child-output; exit 7", "execution_failed", false, 7},
		{"failed-start", "printf machine-child-output", "not_executed", true, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := t.TempDir()
			t.Setenv("SHELL", shell)
			if tc.missingShell {
				t.Setenv("SHELL", filepath.Join(project, "missing-shell"))
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := &cobra.Command{}
			cmd.SetContext(ctx)
			stdout, code, runErr := captureSafeExecution(t, func() (int, error) {
				return runSafeCommand(cmd, output.New(output.Format(GetOutput())), tc.command, project, project)
			})
			if code != tc.exit || (runErr != nil) != (tc.exit != 0) {
				t.Fatalf("incorrect execution return: code=%d err=%v stdout=%s", code, runErr, stdout)
			}
			var view map[string]any
			if err := json.Unmarshal([]byte(stdout), &view); err != nil {
				t.Fatalf("child output contaminated JSON: %v\n%s", err, stdout)
			}
			if view["status"] != tc.status || view["exit_code"] != float64(tc.exit) || view["executed"] != !tc.missingShell {
				t.Fatalf("misleading execution response: %+v", view)
			}
			if !tc.missingShell && view["output"] != "machine-child-output" {
				t.Fatalf("captured child output missing: %+v", view)
			}
			if tc.exit == 7 {
				var exitErr interface{ ExitCode() int }
				if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 7 {
					t.Fatalf("child exit status lost: %v", runErr)
				}
			}
		})
	}
}

// Output is deliberately tiny, so a synchronous pipe capture cannot fill its
// buffer. The caller returns normally even on failure, exercising defer-safe
// handler behavior rather than terminating the test process with os.Exit.
func captureSafeExecution(t *testing.T, run func() (int, error)) (string, int, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	previous := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = previous }()
	code, runErr := run()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout = previous
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(data), code, runErr
}
