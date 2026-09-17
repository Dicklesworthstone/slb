package db

import (
	"crypto/hmac"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrExecutionAuthentication means an RPC caller did not prove ownership of
// the named session in the request's project. Never include keys in errors.
var ErrExecutionAuthentication = errors.New("execution session authentication failed")

// ClaimRequestExecution atomically claims the reviewed command and records who
// will execute it. A failed write must never authorize starting the process.
// This local API assumes its caller already has trusted database access.
func (db *DB) ClaimRequestExecution(expected *Request, execution *Execution) error {
	return db.claimRequestExecution(expected, execution, nil)
}

// ClaimRequestExecutionAuthenticated is the RPC boundary. Authentication is
// repeated under the same writer reservation as proof verification and claim,
// so ending a session or rotating its key cannot race a preflight key check.
func (db *DB) ClaimRequestExecutionAuthenticated(expected *Request, execution *Execution, sessionKey string) error {
	return db.claimRequestExecution(expected, execution, &sessionKey)
}

func (db *DB) claimRequestExecution(expected *Request, execution *Execution, sessionKey *string) error {
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

	return db.Transaction(func(tx *sql.Tx) error {
		// Reserve the writer before reading reviews so a concurrent vote/key
		// change cannot race the authorization-to-execution transition.
		if _, err := tx.Exec(`UPDATE requests SET id = id WHERE id = ?`, expected.ID); err != nil {
			return fmt.Errorf("locking execution claim: %w", err)
		}
		current, err := db.GetRequestTx(tx, expected.ID)
		if err != nil {
			return err
		}
		if sessionKey != nil {
			if err := authenticateExecutionTx(tx, current.ProjectPath, execution.ExecutedBySessionID, *sessionKey, true); err != nil {
				return err
			}
		}
		if err := verifyApprovalTx(tx, current, time.Now().UTC()); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidTransition, err)
		}
		result, err := tx.Exec(`
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
	})
}

func authenticateExecutionTx(tx *sql.Tx, project, sessionID, supplied string, requireActive bool) error {
	var key, sessionProject string
	var ended sql.NullString
	err := tx.QueryRow(`SELECT session_key, project_path, ended_at FROM sessions WHERE id = ?`, sessionID).Scan(&key, &sessionProject, &ended)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrExecutionAuthentication
	}
	if err != nil {
		return fmt.Errorf("reading executor identity: %w", err)
	}
	if sessionProject != project || (requireActive && ended.Valid) || !ExecutionSessionKeyMatches(key, supplied) {
		return ErrExecutionAuthentication
	}
	return nil
}

// ExecutionSessionKeyMatches rejects malformed keys and compares decoded keys
// in constant time. Exported for the daemon's advisory preflight check only;
// the authenticated claim repeats it transactionally.
func ExecutionSessionKeyMatches(stored, supplied string) bool {
	a, err := hex.DecodeString(stored)
	if err != nil || len(a) != 32 {
		return false
	}
	b, err := hex.DecodeString(supplied)
	return err == nil && len(b) == 32 && hmac.Equal(a, b)
}

// CompleteRequestExecution persists the terminal state and child outcome in one
// write. The unique log path fences stale executors, even when sessions match.
// A NULL exit code denotes failure before a process produced an exit status.
func (db *DB) CompleteRequestExecution(id string, status RequestStatus, execution *Execution) error {
	return db.completeRequestExecution(id, status, execution, nil)
}

// CompleteRequestExecutionAuthenticated records an RPC outcome for exactly the
// owned claim. Ended sessions may report an already-started execution, but key
// rotation revokes that authority. Completion never grants a fresh execution.
func (db *DB) CompleteRequestExecutionAuthenticated(id string, status RequestStatus, execution *Execution, sessionKey string) error {
	return db.completeRequestExecution(id, status, execution, &sessionKey)
}

func (db *DB) completeRequestExecution(id string, status RequestStatus, execution *Execution, sessionKey *string) error {
	if status != StatusExecuted && status != StatusExecutionFailed && status != StatusTimedOut {
		return fmt.Errorf("%w: invalid execution outcome %s", ErrInvalidTransition, status)
	}
	if execution == nil || execution.ExecutedBySessionID == "" || execution.LogPath == "" {
		return fmt.Errorf("execution identity and log path are required")
	}
	if status == StatusExecuted && (execution.ExitCode == nil || *execution.ExitCode != 0) {
		return fmt.Errorf("a successful execution requires exit code zero")
	}
	if execution.DurationMs != nil && *execution.DurationMs < 0 {
		return fmt.Errorf("execution duration must not be negative")
	}
	return db.Transaction(func(tx *sql.Tx) error {
		if sessionKey != nil {
			if _, err := tx.Exec(`UPDATE requests SET id = id WHERE id = ?`, id); err != nil {
				return fmt.Errorf("locking execution outcome: %w", err)
			}
			current, err := db.GetRequestTx(tx, id)
			if err != nil {
				return err
			}
			if err := authenticateExecutionTx(tx, current.ProjectPath, execution.ExecutedBySessionID, *sessionKey, false); err != nil {
				return err
			}
		}
		result, err := tx.Exec(`
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
	})
}
