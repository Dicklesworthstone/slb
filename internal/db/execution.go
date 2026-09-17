package db

import (
	"encoding/json"
	"fmt"
	"time"
)

// ClaimRequestExecution atomically claims the reviewed command and records who
// will execute it. A failed write must never authorize starting the process.
// The snapshot predicates also close the preflight-to-execution mutation window.
func (db *DB) ClaimRequestExecution(expected *Request, execution *Execution) error {
	if expected == nil || expected.ID == "" || expected.Status != StatusApproved {
		return fmt.Errorf("%w: an approved request snapshot is required", ErrInvalidTransition)
	}
	if execution == nil || execution.ExecutedAt == nil || execution.ExecutedBySessionID == "" || execution.LogPath == "" {
		return fmt.Errorf("execution identity, timestamp and log path are required")
	}
	if expected.Command.Hash == "" || ComputeCommandHash(expected.Command) != expected.Command.Hash {
		return fmt.Errorf("%w: command snapshot hash mismatch", ErrInvalidTransition)
	}
	argv, err := json.Marshal(expected.Command.Argv)
	if err != nil {
		return fmt.Errorf("encoding execution arguments: %w", err)
	}

	result, err := db.Exec(`
		UPDATE requests SET
			status = 'executing', resolved_at = NULL,
			execution_executed_at = ?, execution_executed_by_session_id = ?,
			execution_executed_by_agent = ?, execution_executed_by_model = ?,
			execution_log_path = ?, execution_exit_code = NULL, execution_duration_ms = NULL
		WHERE id = ? AND status = 'approved'
			AND project_path = ?
			AND command_hash = ? AND command_raw = ? AND command_cwd = ?
			AND command_argv_json = ? AND command_shell = ?
			AND risk_tier = ? AND min_approvals = ? AND require_different_model = ?
			AND (approval_expires_at IS NULL OR julianday(approval_expires_at) > julianday('now'))
			AND EXISTS (
				SELECT 1 FROM sessions WHERE id = ? AND ended_at IS NULL
					AND agent_name = ? AND model = ?
			)
	`, execution.ExecutedAt.UTC().Format(time.RFC3339), execution.ExecutedBySessionID,
		execution.ExecutedByAgent, execution.ExecutedByModel, execution.LogPath,
		expected.ID, expected.ProjectPath, expected.Command.Hash, expected.Command.Raw, expected.Command.Cwd,
		string(argv), boolToInt(expected.Command.Shell), string(expected.RiskTier), expected.MinApprovals,
		boolToInt(expected.RequireDifferentModel), execution.ExecutedBySessionID,
		execution.ExecutedByAgent, execution.ExecutedByModel)
	if err != nil {
		return fmt.Errorf("claiming request execution: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking execution claim: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("%w: request changed, approval expired, session ended, or execution already claimed", ErrInvalidTransition)
	}
	return nil
}

// CompleteRequestExecution persists the terminal state and child outcome in one
// write. The unique log path fences stale executors, even when sessions match.
// A NULL exit code denotes failure before a process produced an exit status.
func (db *DB) CompleteRequestExecution(id string, status RequestStatus, execution *Execution) error {
	if status != StatusExecuted && status != StatusExecutionFailed && status != StatusTimedOut {
		return fmt.Errorf("%w: invalid execution outcome %s", ErrInvalidTransition, status)
	}
	if execution == nil || execution.ExecutedBySessionID == "" || execution.LogPath == "" {
		return fmt.Errorf("execution identity and log path are required")
	}
	if status == StatusExecuted && (execution.ExitCode == nil || *execution.ExitCode != 0) {
		return fmt.Errorf("a successful execution requires exit code zero")
	}
	result, err := db.Exec(`
		UPDATE requests SET status = ?, resolved_at = ?,
			execution_exit_code = ?, execution_duration_ms = ?
		WHERE id = ? AND status = 'executing'
			AND execution_executed_by_session_id = ? AND execution_log_path = ?
	`, string(status), time.Now().UTC().Format(time.RFC3339), execution.ExitCode, execution.DurationMs,
		id, execution.ExecutedBySessionID, execution.LogPath)
	if err != nil {
		return fmt.Errorf("completing request execution: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking execution completion: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("%w: execution claim is no longer owned by this executor", ErrInvalidTransition)
	}
	return nil
}
