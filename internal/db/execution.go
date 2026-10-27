package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrExecutionAuthentication means an RPC caller did not prove ownership of
// the named session in the request's project. Never include keys in errors.
var ErrExecutionAuthentication = errors.New("execution session authentication failed")

// ExecutionPolicyCheck runs under the execution claim's writer reservation.
// It must read database policy through tx, not through another connection.
type ExecutionPolicyCheck func(tx *sql.Tx, request *Request) error

// ClaimRequestExecution atomically claims the reviewed command and records who
// will execute it. A failed write must never authorize starting the process.
// This local API assumes its caller already has trusted database access.
// expected.Rollback must be a fresh capture for this attempt, or nil when this
// attempt did not capture one. Its path is committed with the winning claim.
func (db *DB) ClaimRequestExecution(expected *Request, execution *Execution, checks ...ExecutionPolicyCheck) error {
	return db.claimRequestExecution(expected, execution, nil, checks)
}

// ClaimRequestExecutionAuthenticated repeats authentication, signed evidence
// verification and current policy checks under the same writer reservation.
func (db *DB) ClaimRequestExecutionAuthenticated(expected *Request, execution *Execution, sessionKey string, checks ...ExecutionPolicyCheck) error {
	return db.claimRequestExecution(expected, execution, &sessionKey, checks)
}

func (db *DB) claimRequestExecution(expected *Request, execution *Execution, sessionKey *string, checks []ExecutionPolicyCheck) error {
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
	accepted := *execution
	rollbackPath := ""
	if expected.Rollback != nil {
		rollbackPath = expected.Rollback.Path
	}

	err = db.Transaction(func(tx *sql.Tx) error {
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
		accepted.ApprovalPolicy = current.ApprovalPolicy.Normalized()
		switch {
		case current.MinApprovals == 0 && !current.RequireDifferentModel:
			accepted.Assurance = AssurancePolicyAuto
		case current.ApprovalPolicy == ApprovalPolicySameHost:
			accepted.Assurance = AssuranceSameHost
		default:
			// Only a verified independent authority (currently test-only) can
			// reach this branch. A caller-supplied execution label is ignored.
			accepted.Assurance = AssuranceIndependent
		}
		for _, check := range checks {
			if check == nil {
				return errors.New("execution policy check must not be nil")
			}
			if err := check(tx, current); err != nil {
				return fmt.Errorf("checking execution policy: %w", err)
			}
		}
		result, err := tx.Exec(`
			UPDATE requests SET
				status = 'executing', resolved_at = NULL,
				execution_executed_at = ?, execution_executed_by_session_id = ?,
				execution_executed_by_agent = ?, execution_executed_by_model = ?,
				execution_log_path = ?, execution_exit_code = NULL, execution_duration_ms = NULL,
				execution_approval_policy = ?, execution_assurance = ?,
				rollback_path = ?, rollback_rolled_back_at = NULL
			WHERE id = ? AND status = 'approved'
				AND project_path = ?
				AND command_hash = ? AND command_raw = ? AND command_cwd = ?
				AND command_argv_json = ? AND command_shell = ?
				AND risk_tier = ? AND min_approvals = ? AND require_different_model = ?
				AND approval_policy = ?
				AND (approval_expires_at IS NULL OR julianday(approval_expires_at) > julianday('now'))
				AND EXISTS (
					SELECT 1 FROM sessions WHERE id = ? AND ended_at IS NULL
						AND agent_name = ? AND model = ? AND project_path = requests.project_path
				)
		`, execution.ExecutedAt.UTC().Format(time.RFC3339), execution.ExecutedBySessionID,
			execution.ExecutedByAgent, execution.ExecutedByModel, execution.LogPath,
			string(accepted.ApprovalPolicy), string(accepted.Assurance), nullString(rollbackPath),
			expected.ID, expected.ProjectPath, expected.Command.Hash, expected.Command.Raw, expected.Command.Cwd,
			string(argv), boolToInt(expected.Command.Shell), string(expected.RiskTier), expected.MinApprovals,
			boolToInt(expected.RequireDifferentModel), string(expected.ApprovalPolicy.Normalized()), execution.ExecutedBySessionID,
			execution.ExecutedByAgent, execution.ExecutedByModel)
		if err != nil {
			return fmt.Errorf("claiming request execution: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("checking execution claim: %w", err)
		}
		if count != 1 {
			return fmt.Errorf("%w: request changed, approval expired, session ended, project differs, or execution already claimed", ErrInvalidTransition)
		}
		return nil
	})
	if err != nil {
		return err
	}
	*execution = accepted
	return nil
}

func authenticateExecutionTx(tx *sql.Tx, project, sessionID, supplied string, requireActive bool) error {
	var sessionProject string
	var digest, ended sql.NullString
	err := tx.QueryRow(`SELECT session_key_digest, project_path, ended_at FROM sessions WHERE id = ?`, sessionID).Scan(&digest, &sessionProject, &ended)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrExecutionAuthentication
	}
	if err != nil {
		return fmt.Errorf("reading executor identity: %w", sessionDigestReadError(err))
	}
	if sessionProject != project || (requireActive && ended.Valid) || !SessionKeyMatches(digest.String, supplied) {
		return ErrExecutionAuthentication
	}
	return nil
}

// CompleteRequestExecution persists the terminal state and child outcome in one
// write. The unique log path fences stale executors, even when sessions match.
// A NULL exit code denotes failure before a process produced an exit status.
func (db *DB) CompleteRequestExecution(id string, status RequestStatus, execution *Execution) error {
	_, err := db.completeRequestExecution(id, status, execution, nil)
	return err
}

// CompleteRequestExecutionAuthenticated records an RPC outcome for exactly the
// owned claim. Ended sessions may report an already-started execution, but key
// rotation revokes that authority. Completion never grants a fresh execution.
// An identical retry recovers the original committed result after reply loss;
// it does not rewrite the outcome, its timestamps, or the audit journal.
func (db *DB) CompleteRequestExecutionAuthenticated(id string, status RequestStatus, execution *Execution, sessionKey string) (*Request, error) {
	return db.completeRequestExecution(id, status, execution, &sessionKey)
}

func (db *DB) completeRequestExecution(id string, status RequestStatus, execution *Execution, sessionKey *string) (*Request, error) {
	if status != StatusExecuted && status != StatusExecutionFailed && status != StatusTimedOut {
		return nil, fmt.Errorf("%w: invalid execution outcome %s", ErrInvalidTransition, status)
	}
	if execution == nil || execution.ExecutedBySessionID == "" || execution.LogPath == "" {
		return nil, fmt.Errorf("execution identity and log path are required")
	}
	if status == StatusExecuted && (execution.ExitCode == nil || *execution.ExitCode != 0) {
		return nil, fmt.Errorf("a successful execution requires exit code zero")
	}
	if execution.DurationMs != nil && *execution.DurationMs < 0 {
		return nil, fmt.Errorf("execution duration must not be negative")
	}
	var completed *Request
	err := db.Transaction(func(tx *sql.Tx) error {
		if sessionKey != nil {
			// Writable DB transactions begin with BEGIN IMMEDIATE. Authenticate
			// and compare under that reservation without even a no-op UPDATE:
			// terminal immutability triggers must not fire for a read-only retry.
			current, err := db.GetRequestTx(tx, id)
			if err != nil {
				return err
			}
			if err := authenticateExecutionTx(tx, current.ProjectPath, execution.ExecutedBySessionID, *sessionKey, false); err != nil {
				return err
			}
			if current.Status != StatusExecuting {
				if !matchesCompletedExecution(current, status, execution) {
					return fmt.Errorf("%w: completion differs from the recorded execution", ErrInvalidTransition)
				}
				completed = current
				return nil
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
		if sessionKey != nil {
			// Read the canonical persisted timestamps and claim assurance while
			// this transaction still owns the outcome. Never reconstruct a
			// successful receipt from the caller's report or a later read.
			completed, err = db.GetRequestTx(tx, id)
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return completed, nil
}

func matchesCompletedExecution(request *Request, status RequestStatus, report *Execution) bool {
	stored := request.Execution
	if request.Status != status || request.ResolvedAt == nil || stored == nil ||
		stored.ExecutedBySessionID != report.ExecutedBySessionID || stored.LogPath != report.LogPath ||
		!sameCompletionValue(stored.ExitCode, report.ExitCode) || !sameCompletionValue(stored.DurationMs, report.DurationMs) {
		return false
	}
	// RPC reports carry the claim fence and outcome only. Local callers may
	// also assert immutable claim metadata; if supplied, it must agree with
	// the original record and cannot upgrade its evidence classification.
	if (report.ApprovalPolicy != "" && report.ApprovalPolicy.Normalized() != stored.ApprovalPolicy.Normalized()) ||
		(report.Assurance != "" && report.Assurance != stored.Assurance) ||
		(report.ExecutedByAgent != "" && report.ExecutedByAgent != stored.ExecutedByAgent) ||
		(report.ExecutedByModel != "" && report.ExecutedByModel != stored.ExecutedByModel) {
		return false
	}
	// Claims persist timestamps at whole-second precision. Compare the same
	// representation when a caller retained its original subsecond timestamp.
	return report.ExecutedAt == nil || (stored.ExecutedAt != nil &&
		report.ExecutedAt.UTC().Format(time.RFC3339) == stored.ExecutedAt.UTC().Format(time.RFC3339))
}

func sameCompletionValue[T comparable](stored, reported *T) bool {
	if stored == nil || reported == nil {
		return stored == reported
	}
	return *stored == *reported
}
