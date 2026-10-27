package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
)

// executionOutcome describes this invocation, not an optimistic interpretation
// of the requested action. A rejected preflight has no child exit status; an
// outcome persistence failure must retain EXECUTING rather than claim success.
type executionOutcome struct {
	approvalPolicyView
	Assurance  db.ReviewAssurance `json:"assurance,omitempty"`
	RequestID  string             `json:"request_id"`
	Status     string             `json:"status"`
	Executed   bool               `json:"executed"`
	ExitCode   int                `json:"exit_code"`
	DurationMs int64              `json:"duration_ms"`
	LogPath    string             `json:"log_path"`
	Output     string             `json:"output,omitempty"`
	TimedOut   bool               `json:"timed_out,omitempty"`
	Error      string             `json:"error,omitempty"`
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
			view.approvalPolicyView = policyView(result.Request.ApprovalPolicy)
			view.Status = string(result.Request.Status)
			// Core only records an exit status after a child starts. Even -1
			// may be a real (signal-terminated) result; a failed start is nil.
			view.Executed = result.Request.Execution != nil && result.Request.Execution.ExitCode != nil
			if result.Request.Execution != nil {
				view.Assurance = result.Request.Execution.Assurance
			}
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

const emergencyReviewNotice = "unreviewed emergency override; no peer approval or independently authenticated authorization"

// emergencyEvidence retains the configured review policy only as bypassed
// context. The fixed unreviewed assurance never satisfies that policy.
type emergencyEvidence struct {
	approvalPolicyView
	ExecutionMode          string             `json:"execution_mode"`
	Assurance              db.ReviewAssurance `json:"assurance"`
	ReviewNotice           string             `json:"review_notice"`
	ApprovalPolicyBypassed bool               `json:"approval_policy_bypassed"`
}

func emergencyEvidenceFor(policy db.ApprovalPolicy) emergencyEvidence {
	view := policyView(policy)
	view.ApprovalPolicyLabel = "BYPASSED (configured policy only): " + view.ApprovalPolicyLabel
	return emergencyEvidence{
		approvalPolicyView: view, ExecutionMode: "emergency", Assurance: db.AssuranceEmergency,
		ReviewNotice: emergencyReviewNotice, ApprovalPolicyBypassed: true,
	}
}

// emergencyExecutionOutcome keeps the override outside either review policy.
// Status reports the durable ledger; Executed and ExitCode report this caller's
// actual child observation, even if recording completion subsequently failed.
type emergencyExecutionOutcome struct {
	emergencyEvidence
	EmergencyID        string     `json:"emergency_id,omitempty"`
	Command            string     `json:"command"`
	Hash               string     `json:"hash"`
	CommandHash        string     `json:"command_hash"`
	ProjectPath        string     `json:"project_path"`
	CWD                string     `json:"cwd"`
	ConfirmationMethod string     `json:"confirmation_method"`
	Status             string     `json:"status"`
	Executed           bool       `json:"executed"`
	ExitCode           int        `json:"exit_code"`
	DurationMs         int64      `json:"duration_ms"`
	LogPath            string     `json:"log_path"`
	RollbackPath       string     `json:"rollback_path,omitempty"`
	Output             string     `json:"output,omitempty"`
	TimedOut           bool       `json:"timed_out,omitempty"`
	Reason             string     `json:"reason"`
	Actor              string     `json:"actor"`
	AttemptedAt        string     `json:"attempted_at,omitempty"`
	ExecutedAt         string     `json:"executed_at,omitempty"`
	RecordedAt         *time.Time `json:"recorded_at,omitempty"`
	StartedAt          *time.Time `json:"started_at,omitempty"`
	CompletedAt        *time.Time `json:"completed_at,omitempty"`
	PID                *int       `json:"pid,omitempty"`
	Error              string     `json:"error,omitempty"`
}

func emergencyCommandError(result *core.CommandResult, runErr error) error {
	if runErr != nil {
		return runErr
	}
	if result == nil {
		return errors.New("emergency executor returned no execution outcome")
	}
	if result.ExitCode != 0 {
		return commandExitError{code: result.ExitCode}
	}
	return nil
}

func emergencyOutcomeFor(record *db.EmergencyExecution, result *core.CommandResult, runErr error) (emergencyExecutionOutcome, error) {
	view := emergencyExecutionOutcome{
		emergencyEvidence: emergencyEvidenceFor(db.ApprovalPolicyIndependent),
		Status:            "not_recorded", ExitCode: -1,
	}
	if record != nil {
		view.emergencyEvidence = emergencyEvidenceFor(record.ConfiguredApprovalPolicy)
		view.EmergencyID = record.ID
		view.Command, view.Hash, view.CommandHash = record.Command.Raw, record.RawCommandHash, record.Command.Hash
		view.ProjectPath, view.CWD = record.ProjectPath, record.Command.Cwd
		view.ConfirmationMethod = record.ConfirmationMethod
		view.LogPath, view.RollbackPath = record.LogPath, record.RollbackPath
		view.Reason, view.Actor = record.Reason, record.Actor
		view.StartedAt, view.CompletedAt, view.PID = record.StartedAt, record.CompletedAt, record.PID
		if !record.RecordedAt.IsZero() {
			view.RecordedAt = &record.RecordedAt
			view.AttemptedAt = record.RecordedAt.Format(time.RFC3339Nano)
			view.Status = string(record.Status)
		}
		view.TimedOut = record.Status == db.EmergencyStatusTimedOut
	}
	if result != nil {
		view.Executed, view.ExitCode = true, result.ExitCode
		view.DurationMs, view.Output = result.Duration.Milliseconds(), result.Output
		if record != nil && record.StartedAt != nil {
			view.ExecutedAt = record.StartedAt.Format(time.RFC3339Nano)
		}
	}
	runErr = emergencyCommandError(result, runErr)
	view.TimedOut = view.TimedOut || errors.Is(runErr, context.DeadlineExceeded)
	if runErr == nil && (record == nil || record.Status != db.EmergencyStatusSucceeded ||
		record.StartedAt == nil || record.PID == nil || record.ExitCode == nil || *record.ExitCode != 0) {
		runErr = fmt.Errorf("emergency execution has no durable successful outcome (status %s)", view.Status)
	}
	if runErr != nil {
		view.Error = runErr.Error()
	}
	return view, runErr
}
