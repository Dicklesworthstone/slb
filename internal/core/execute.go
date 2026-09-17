// Package core implements command execution with gate conditions.
package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/integrations"
)

// Execution errors.
var (
	ErrRequestNotApproved  = errors.New("request is not approved")
	ErrApprovalExpired     = errors.New("approval has expired")
	ErrCommandHashMismatch = errors.New("command hash does not match")
	ErrTierEscalated       = errors.New("current policy requires higher tier than approved")
	ErrAlreadyExecuted     = errors.New("request has already been executed")
	ErrAlreadyExecuting    = errors.New("request is already being executed")
	ErrExecutionTimeout    = errors.New("command execution timed out")
)

// DefaultExecutionTimeout is the default timeout for command execution.
const DefaultExecutionTimeout = 5 * time.Minute

// ExecuteOptions holds parameters for command execution.
type ExecuteOptions struct {
	// RequestID is the approved request to execute (required).
	RequestID string
	// ExpectedCommandHash binds a handoff to the exact command seen by its
	// caller. Empty means no additional caller-side binding is requested.
	ExpectedCommandHash string
	// SessionID is the executor's session ID (required for tracking).
	SessionID string
	// Timeout is the maximum execution duration (default 5 minutes).
	Timeout time.Duration
	// Background runs the command in background, returning immediately.
	Background bool
	// LogDir is the directory for execution logs (default .slb/logs/).
	LogDir string
	// SuppressOutput prevents streaming command output to stdout (still logged to file).
	// Useful for machine-readable output formats (e.g., --output json).
	SuppressOutput bool

	// CaptureRollback enables rollback state capture for supported destructive commands.
	CaptureRollback bool
	// MaxRollbackSizeMB limits filesystem rollback capture (0 uses config default).
	MaxRollbackSizeMB int
}

// ExecutionResult holds the result of command execution.
type ExecutionResult struct {
	// Request is the executed request.
	Request *db.Request
	// ExitCode is the command's exit code (-1 if no exit status is available).
	ExitCode int
	// LogPath is the path to the execution log.
	LogPath string
	// Duration is the execution duration.
	Duration time.Duration
	// Output is the combined stdout/stderr output.
	Output string
	// TimedOut indicates if the command timed out.
	TimedOut bool
	// Error contains any execution error.
	Error error
}

// Executor handles command execution with validation.
type Executor struct {
	db            *db.DB
	patternEngine *PatternEngine
	notifier      integrations.RequestNotifier
}

// NewExecutor creates a new executor.
func NewExecutor(database *db.DB, patternEngine *PatternEngine) *Executor {
	if patternEngine == nil {
		patternEngine = GetDefaultEngine()
	}
	return &Executor{
		db:            database,
		patternEngine: patternEngine,
		notifier:      integrations.NoopNotifier{},
	}
}

// WithNotifier sets the notifier used for execution events.
func (e *Executor) WithNotifier(n integrations.RequestNotifier) *Executor {
	if n != nil {
		e.notifier = n
	}
	return e
}

// ExecuteApprovedRequest validates and executes an approved request.
// This runs the command in the CALLER'S shell environment (client-side execution).
func (e *Executor) ExecuteApprovedRequest(ctx context.Context, opts ExecuteOptions) (*ExecutionResult, error) {
	// Validate required fields
	if opts.RequestID == "" {
		return nil, errors.New("request_id is required")
	}
	if opts.SessionID == "" {
		return nil, errors.New("session_id is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.Timeout < 0 {
		return nil, errors.New("execution timeout must not be negative")
	}

	// Set defaults
	if opts.Timeout == 0 {
		opts.Timeout = DefaultExecutionTimeout
	}
	if opts.LogDir == "" {
		opts.LogDir = ".slb/logs"
	}

	if opts.MaxRollbackSizeMB <= 0 {
		opts.MaxRollbackSizeMB = 100
	}

	// Get the request
	request, err := e.db.GetRequest(opts.RequestID)
	if err != nil {
		return nil, fmt.Errorf("getting request: %w", err)
	}
	if opts.ExpectedCommandHash != "" && opts.ExpectedCommandHash != request.Command.Hash {
		return nil, fmt.Errorf("%w: command changed since execution was requested", ErrCommandHashMismatch)
	}

	// Get the session (for tracking who executed)
	session, err := e.db.GetSession(opts.SessionID)
	if err != nil {
		return nil, fmt.Errorf("getting session: %w", err)
	}
	if !session.IsActive() {
		return nil, ErrSessionInactive
	}

	// Gate 1: Request must be approved
	if request.Status == db.StatusExecuting {
		return nil, ErrAlreadyExecuting
	}
	if request.Status == db.StatusExecuted || request.Status == db.StatusExecutionFailed {
		return nil, ErrAlreadyExecuted
	}
	if request.Status != db.StatusApproved {
		return nil, fmt.Errorf("%w: status is %s", ErrRequestNotApproved, request.Status)
	}

	// Gate 2: Approval must not be expired
	if request.ApprovalExpiresAt != nil && time.Now().After(*request.ApprovalExpiresAt) {
		return nil, ErrApprovalExpired
	}

	// Gate 3: Command hash must match (prevents mutation)
	expectedHash := db.ComputeCommandHash(request.Command)
	if expectedHash != request.Command.Hash {
		return nil, fmt.Errorf("%w: stored=%s computed=%s", ErrCommandHashMismatch, request.Command.Hash, expectedHash)
	}

	// Gate 4: Current pattern policy doesn't require higher tier
	classification := e.patternEngine.ClassifyCommand(request.Command.Raw, request.Command.Cwd)
	if tierHigher(classification.Tier, request.RiskTier) {
		return nil, fmt.Errorf("%w: approved as %s but now classified as %s",
			ErrTierEscalated, request.RiskTier, classification.Tier)
	}
	// Refuse unusable approvals before creating logs or running capture tools.
	// The final claim repeats this check under its own writer reservation.
	if err := e.verifyApprovalSnapshot(request); err != nil {
		return nil, err
	}

	// Preflight: create log file and capture rollback state before locking EXECUTING.
	logPath, err := e.createLogFile(opts.LogDir, request.ID)
	if err != nil {
		return nil, fmt.Errorf("creating log file: %w", err)
	}

	if opts.CaptureRollback && (request.Rollback == nil || request.Rollback.Path == "") {
		data, err := CaptureRollbackState(ctx, request, RollbackCaptureOptions{
			MaxSizeBytes: int64(opts.MaxRollbackSizeMB) * 1024 * 1024,
		})
		if err != nil {
			return nil, fmt.Errorf("capturing rollback state: %w", err)
		}
		if data != nil && data.RollbackPath != "" {
			request.Rollback = &db.Rollback{Path: data.RollbackPath}
			if err := e.db.UpdateRequestRollbackPath(opts.RequestID, data.RollbackPath); err != nil {
				return nil, fmt.Errorf("recording rollback path: %w", err)
			}
		}
	}

	// Record executor info
	now := time.Now().UTC()
	exec := &db.Execution{
		ExecutedAt:          &now,
		ExecutedBySessionID: opts.SessionID,
		ExecutedByAgent:     session.AgentName,
		ExecutedByModel:     session.Model,
		LogPath:             logPath,
	}

	// Gate 5: claim exactly the snapshot we checked and record execution identity
	// in the same write. Recheck expiry after potentially lengthy rollback capture.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.ApprovalExpiresAt != nil && !time.Now().Before(*request.ApprovalExpiresAt) {
		return nil, ErrApprovalExpired
	}
	if err := e.db.ClaimRequestExecution(request, exec); err != nil {
		if errors.Is(err, db.ErrInvalidTransition) {
			latest, readErr := e.db.GetRequest(request.ID)
			if readErr == nil && latest.Status == db.StatusExecuting {
				return nil, ErrAlreadyExecuting
			}
		}
		return nil, fmt.Errorf("claiming execution: %w", err)
	}
	request.Status = db.StatusExecuting
	request.Execution = exec

	// Execute the command. Do not report success when a process never starts.
	result := &ExecutionResult{
		Request:  request,
		LogPath:  logPath,
		ExitCode: -1,
	}

	execCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	// Keep a nil interface when streaming is disabled. A typed nil *os.File
	// becomes a non-nil io.Writer and would cause output copying to fail.
	var streamWriter io.Writer
	if !opts.SuppressOutput {
		streamWriter = os.Stdout
	}
	cmdResult, err := RunCommand(execCtx, &request.Command, logPath, streamWriter)
	if cmdResult != nil {
		// Cancellation and I/O failures can still have output and a real exit
		// status. Preserve these before handling the error.
		result.ExitCode = cmdResult.ExitCode
		result.Duration = cmdResult.Duration
		result.Output = cmdResult.Output
	}

	finalStatus := db.StatusExecutionFailed
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		result.TimedOut = true
		result.Error = ErrExecutionTimeout
		finalStatus = db.StatusTimedOut
	case err != nil:
		result.Error = err
	case cmdResult != nil && cmdResult.ExitCode == 0:
		finalStatus = db.StatusExecuted
	}
	// Update execution details - only set exit code and duration when we have valid results.
	// When cmdResult is nil (timeout before process started, or other error), leave as NULL.
	if cmdResult != nil {
		exitCode := result.ExitCode
		durationMs := result.Duration.Milliseconds()
		exec.ExitCode = &exitCode
		exec.DurationMs = &durationMs
	}
	if execErr := e.db.CompleteRequestExecution(opts.RequestID, finalStatus, exec); execErr != nil {
		result.Error = errors.Join(result.Error, fmt.Errorf("recording execution outcome: %w", execErr))
		// Do not announce an outcome as durable when its database write failed.
		return result, result.Error
	}
	request.Status = finalStatus
	resolvedAt := time.Now().UTC()
	request.ResolvedAt = &resolvedAt
	request.Execution = exec

	// Notify (best effort), using the current status and execution metadata.
	_ = e.notifier.NotifyRequestExecuted(request, exec, result.ExitCode)

	return result, result.Error
}

// createLogFile creates the log file for command output.
func (e *Executor) createLogFile(logDir, requestID string) (string, error) {
	// Ensure log directory exists
	if err := os.MkdirAll(logDir, 0700); err != nil {
		return "", fmt.Errorf("creating log dir: %w", err)
	}

	// Create timestamped log file with truncated request ID
	timestamp := time.Now().Format("20060102-150405")
	idSuffix := requestID
	if len(idSuffix) > 8 {
		idSuffix = idSuffix[:8]
	}
	// Unique, exclusively created files prevent a competing executor from
	// truncating the winner's audit log before its claim is rejected.
	f, err := os.CreateTemp(logDir, fmt.Sprintf("%s-*_%s.log", timestamp, idSuffix))
	if err != nil {
		return "", fmt.Errorf("creating log file: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("closing new log file: %w", err)
	}

	return f.Name(), nil
}

// tierHigher returns true if tier1 is higher (more restrictive) than tier2.
func tierHigher(tier1, tier2 db.RiskTier) bool {
	tierOrder := map[db.RiskTier]int{
		db.RiskTierCaution:   1,
		db.RiskTierDangerous: 2,
		db.RiskTierCritical:  3,
	}
	return tierOrder[tier1] > tierOrder[tier2]
}

// CanExecute checks if a request can be executed and returns the reason if not.
func (e *Executor) CanExecute(requestID string) (bool, string) {
	request, err := e.db.GetRequest(requestID)
	if err != nil {
		return false, fmt.Sprintf("request not found: %v", err)
	}

	if request.Status == db.StatusExecuting {
		return false, "request is already being executed"
	}
	if request.Status == db.StatusExecuted || request.Status == db.StatusExecutionFailed {
		return false, "request has already been executed"
	}
	if request.Status != db.StatusApproved {
		return false, fmt.Sprintf("request is not approved (status: %s)", request.Status)
	}
	if request.ApprovalExpiresAt != nil && time.Now().After(*request.ApprovalExpiresAt) {
		return false, "approval has expired"
	}

	expectedHash := db.ComputeCommandHash(request.Command)
	if expectedHash != request.Command.Hash {
		return false, "command hash mismatch (command may have been modified)"
	}

	classification := e.patternEngine.ClassifyCommand(request.Command.Raw, request.Command.Cwd)
	if tierHigher(classification.Tier, request.RiskTier) {
		return false, fmt.Sprintf("policy escalation: command now classified as %s", classification.Tier)
	}
	if err := e.verifyApprovalSnapshot(request); err != nil {
		return false, err.Error()
	}

	return true, ""
}

// verifyApprovalSnapshot aligns advisory/hook preflight with the final claim.
// Never substitute a newer command for the snapshot whose policy was checked.
func (e *Executor) verifyApprovalSnapshot(request *db.Request) error {
	verified, err := e.db.VerifyRequestApproval(request.ID)
	if err != nil {
		return fmt.Errorf("verifying approval: %w", err)
	}
	if verified.Command.Hash != request.Command.Hash || verified.ProjectPath != request.ProjectPath ||
		verified.RiskTier != request.RiskTier || verified.MinApprovals != request.MinApprovals ||
		verified.RequireDifferentModel != request.RequireDifferentModel {
		return fmt.Errorf("%w: request changed during preflight", db.ErrInvalidTransition)
	}
	return nil
}
