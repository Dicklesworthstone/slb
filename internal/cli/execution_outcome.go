package cli

import (
	"errors"
	"fmt"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
)

// executionOutcome describes this invocation, not an optimistic interpretation
// of the requested action. A rejected preflight has no child exit status; an
// outcome persistence failure must retain EXECUTING rather than claim success.
type executionOutcome struct {
	RequestID  string `json:"request_id"`
	Status     string `json:"status"`
	Executed   bool   `json:"executed"`
	ExitCode   int    `json:"exit_code"`
	DurationMs int64  `json:"duration_ms"`
	LogPath    string `json:"log_path"`
	Output     string `json:"output,omitempty"`
	TimedOut   bool   `json:"timed_out,omitempty"`
	Error      string `json:"error,omitempty"`
}

func executionOutputStructured() bool {
	switch GetOutput() {
	case "json", "yaml", "toon":
		return true
	default:
		return false
	}
}

func executionOutcomeFor(requestID string, result *core.ExecutionResult, execErr error) (executionOutcome, error) {
	view := executionOutcome{RequestID: requestID, Status: "not_executed", ExitCode: -1}
	if result == nil {
		if execErr == nil {
			execErr = errors.New("executor returned no execution outcome")
		}
	} else {
		view.Status = "unknown"
		view.ExitCode = result.ExitCode
		view.DurationMs = result.Duration.Milliseconds()
		view.LogPath = result.LogPath
		view.Output = result.Output
		view.TimedOut = result.TimedOut
		if result.Request != nil {
			view.Status = string(result.Request.Status)
			// Core only records an exit status after a child starts. Even -1
			// may be a real (signal-terminated) result; a failed start is nil.
			view.Executed = result.Request.Execution != nil && result.Request.Execution.ExitCode != nil
		}
		if execErr == nil {
			execErr = result.Error
		}
		if execErr == nil {
			switch {
			case result.TimedOut:
				execErr = core.ErrExecutionTimeout
			case view.ExitCode != 0:
				execErr = commandExitError{code: view.ExitCode}
			case view.Status != string(db.StatusExecuted) || !view.Executed:
				execErr = fmt.Errorf("execution did not produce a completed outcome (status %s)", view.Status)
			}
		}
	}
	if execErr != nil {
		view.Error = execErr.Error()
	}
	return view, execErr
}
