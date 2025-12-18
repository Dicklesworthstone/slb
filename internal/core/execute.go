// Package core implements command execution with gate conditions.
package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	// ExitCode is the command's exit code.
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

	// Get the session (for tracking who executed)
	session, err := e.db.GetSession(opts.SessionID)
	if err != nil {
		return nil, fmt.Errorf("getting session: %w", err)
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
	expectedHash := ComputeCommandHash(request.Command)
	if expectedHash != request.Command.Hash {
		return nil, fmt.Errorf("%w: stored=%s computed=%s", ErrCommandHashMismatch, request.Command.Hash, expectedHash)
	}

	// Gate 4: Current pattern policy doesn't require higher tier
	classification := e.patternEngine.ClassifyCommand(request.Command.Raw, request.Command.Cwd)
	if tierHigher(classification.Tier, request.RiskTier) {
		return nil, fmt.Errorf("%w: approved as %s but now classified as %s",
			ErrTierEscalated, request.RiskTier, classification.Tier)
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

	// Gate 5: First executor wins - transition to EXECUTING
	if err := e.db.UpdateRequestStatus(opts.RequestID, db.StatusExecuting); err != nil {
		// If another executor already started, we'll get an error
		if errors.Is(err, db.ErrInvalidTransition) {
			return nil, ErrAlreadyExecuting
		}
		return nil, fmt.Errorf("updating status to executing: %w", err)
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

	// Update execution info
	if err := e.db.UpdateRequestExecution(opts.RequestID, exec); err != nil {
		// Log but don't fail - the command will still execute
		fmt.Fprintf(os.Stderr, "warning: failed to record execution info: %v\n", err)
	}

	// Execute the command
	result := &ExecutionResult{
		Request: request,
		LogPath: logPath,
	}

	execCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	var streamWriter *os.File
	if !opts.SuppressOutput {
		streamWriter = os.Stdout
	}
	cmdResult, err := RunCommand(execCtx, &request.Command, logPath, streamWriter)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			result.TimedOut = true
			result.Error = ErrExecutionTimeout
			_ = e.db.UpdateRequestStatus(opts.RequestID, db.StatusTimedOut)
		} else {
			result.Error = err
			_ = e.db.UpdateRequestStatus(opts.RequestID, db.StatusExecutionFailed)
		}
	} else {
		result.ExitCode = cmdResult.ExitCode
		result.Duration = cmdResult.Duration
		result.Output = cmdResult.Output

		// Determine final status based on exit code
		if cmdResult.ExitCode == 0 {
			_ = e.db.UpdateRequestStatus(opts.RequestID, db.StatusExecuted)
		} else {
			_ = e.db.UpdateRequestStatus(opts.RequestID, db.StatusExecutionFailed)
		}
	}

	// Update execution details
	exitCode := result.ExitCode
	durationMs := result.Duration.Milliseconds()
	exec.ExitCode = &exitCode
	exec.DurationMs = &durationMs
	_ = e.db.UpdateRequestExecution(opts.RequestID, exec)

	// Notify (best effort)
	_ = e.notifier.NotifyRequestExecuted(request, exec, result.ExitCode)

	return result, result.Error
}

// createLogFile creates the log file for command output.
func (e *Executor) createLogFile(logDir, requestID string) (string, error) {
	// Ensure log directory exists
	if err := os.MkdirAll(logDir, 0700); err != nil {
		return "", fmt.Errorf("creating log dir: %w", err)
	}

	// Create timestamped log file
	timestamp := time.Now().Format("20060102-150405")
	logName := fmt.Sprintf("%s_%s.log", timestamp, requestID[:8])
	logPath := filepath.Join(logDir, logName)

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return "", fmt.Errorf("creating log file: %w", err)
	}
	f.Close()

	return logPath, nil
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

	expectedHash := ComputeCommandHash(request.Command)
	if expectedHash != request.Command.Hash {
		return false, "command hash mismatch (command may have been modified)"
	}

	classification := e.patternEngine.ClassifyCommand(request.Command.Raw, request.Command.Cwd)
	if tierHigher(classification.Tier, request.RiskTier) {
		return false, fmt.Sprintf("policy escalation: command now classified as %s", classification.Tier)
	}

	return true, ""
}
