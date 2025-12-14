// Package db provides request CRUD operations.
package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrRequestNotFound is returned when a request is not found.
var ErrRequestNotFound = errors.New("request not found")

// ErrInvalidTransition is returned when a state transition is invalid.
var ErrInvalidTransition = errors.New("invalid state transition")

// DefaultRequestTimeout is the default timeout for pending requests.
const DefaultRequestTimeout = 30 * time.Minute

// CreateRequest creates a new request in the database.
// Generates a UUID and computes the command hash.
func (db *DB) CreateRequest(r *Request) error {
	// Generate UUID if not set
	if r.ID == "" {
		r.ID = uuid.New().String()
	}

	// Compute command hash
	if r.Command.Hash == "" {
		r.Command.Hash = ComputeCommandHash(r.Command)
	}

	// Set timestamps
	now := time.Now().UTC()
	r.CreatedAt = now
	if r.Status == "" {
		r.Status = StatusPending
	}
	if r.ExpiresAt == nil {
		expiresAt := now.Add(DefaultRequestTimeout)
		r.ExpiresAt = &expiresAt
	}

	// Serialize complex fields
	argvJSON, _ := json.Marshal(r.Command.Argv)
	attachmentsJSON, _ := json.Marshal(r.Attachments)

	_, err := db.Exec(`
		INSERT INTO requests (
			id, project_path,
			command_raw, command_argv_json, command_cwd, command_shell, command_hash,
			command_display_redacted, command_contains_sensitive,
			risk_tier, requestor_session_id, requestor_agent, requestor_model,
			justification_reason, justification_expected_effect, justification_goal, justification_safety_argument,
			dry_run_command, dry_run_output, attachments_json,
			status, min_approvals, require_different_model,
			created_at, expires_at, approval_expires_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		r.ID, r.ProjectPath,
		r.Command.Raw, string(argvJSON), r.Command.Cwd, boolToInt(r.Command.Shell), r.Command.Hash,
		nullString(r.Command.DisplayRedacted), boolToInt(r.Command.ContainsSensitive),
		string(r.RiskTier), r.RequestorSessionID, r.RequestorAgent, r.RequestorModel,
		r.Justification.Reason, nullString(r.Justification.ExpectedEffect), nullString(r.Justification.Goal), nullString(r.Justification.SafetyArgument),
		nullDryRunCommand(r.DryRun), nullDryRunOutput(r.DryRun), string(attachmentsJSON),
		string(r.Status), r.MinApprovals, boolToInt(r.RequireDifferentModel),
		r.CreatedAt.Format(time.RFC3339), formatTimePtr(r.ExpiresAt), formatTimePtr(r.ApprovalExpiresAt),
	)

	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	return nil
}

// GetRequest retrieves a request by ID.
func (db *DB) GetRequest(id string) (*Request, error) {
	row := db.QueryRow(`
		SELECT id, project_path,
			command_raw, command_argv_json, command_cwd, command_shell, command_hash,
			command_display_redacted, command_contains_sensitive,
			risk_tier, requestor_session_id, requestor_agent, requestor_model,
			justification_reason, justification_expected_effect, justification_goal, justification_safety_argument,
			dry_run_command, dry_run_output, attachments_json,
			status, min_approvals, require_different_model,
			execution_log_path, execution_exit_code, execution_duration_ms,
			execution_executed_at, execution_executed_by_session_id, execution_executed_by_agent, execution_executed_by_model,
			rollback_path, rollback_rolled_back_at,
			created_at, resolved_at, expires_at, approval_expires_at
		FROM requests WHERE id = ?
	`, id)

	return scanRequest(row)
}

// GetRequestWithReviews retrieves a request and its associated reviews.
func (db *DB) GetRequestWithReviews(id string) (*Request, []*Review, error) {
	r, err := db.GetRequest(id)
	if err != nil {
		return nil, nil, err
	}

	rows, err := db.Query(`
		SELECT id, request_id, reviewer_session_id, reviewer_agent, reviewer_model,
			decision, signature, signature_timestamp, responses_json, comments, created_at
		FROM reviews WHERE request_id = ?
		ORDER BY created_at ASC
	`, id)
	if err != nil {
		return nil, nil, fmt.Errorf("querying reviews: %w", err)
	}
	defer rows.Close()

	reviews, err := scanReviewList(rows)
	if err != nil {
		return nil, nil, err
	}

	return r, reviews, nil
}

// ListPendingRequests returns all pending requests for a project.
func (db *DB) ListPendingRequests(projectPath string) ([]*Request, error) {
	return db.ListRequestsByStatus(StatusPending, projectPath)
}

// ListPendingRequestsByProjects returns pending requests for a set of projects.
func (db *DB) ListPendingRequestsByProjects(projectPaths []string) ([]*Request, error) {
	if len(projectPaths) == 0 {
		return []*Request{}, nil
	}
	placeholders := make([]string, 0, len(projectPaths))
	args := make([]any, 0, len(projectPaths)+1)
	for _, p := range projectPaths {
		placeholders = append(placeholders, "?")
		args = append(args, p)
	}
	args = append(args, string(StatusPending))

	query := fmt.Sprintf(`
		SELECT id, project_path,
			command_raw, command_argv_json, command_cwd, command_shell, command_hash,
			command_display_redacted, command_contains_sensitive,
			risk_tier, requestor_session_id, requestor_agent, requestor_model,
			justification_reason, justification_expected_effect, justification_goal, justification_safety_argument,
			dry_run_command, dry_run_output, attachments_json,
			status, min_approvals, require_different_model,
			execution_log_path, execution_exit_code, execution_duration_ms,
			execution_executed_at, execution_executed_by_session_id, execution_executed_by_agent, execution_executed_by_model,
			rollback_path, rollback_rolled_back_at,
			created_at, resolved_at, expires_at, approval_expires_at
		FROM requests
		WHERE project_path IN (%s) AND status = ?
		ORDER BY created_at DESC
	`, strings.Join(placeholders, ","))

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying pending requests by projects: %w", err)
	}
	defer rows.Close()

	return scanRequests(rows)
}

// ListPendingRequestsAllProjects returns all pending requests across all projects.
func (db *DB) ListPendingRequestsAllProjects() ([]*Request, error) {
	rows, err := db.Query(`
		SELECT id, project_path,
			command_raw, command_argv_json, command_cwd, command_shell, command_hash,
			command_display_redacted, command_contains_sensitive,
			risk_tier, requestor_session_id, requestor_agent, requestor_model,
			justification_reason, justification_expected_effect, justification_goal, justification_safety_argument,
			dry_run_command, dry_run_output, attachments_json,
			status, min_approvals, require_different_model,
			execution_log_path, execution_exit_code, execution_duration_ms,
			execution_executed_at, execution_executed_by_session_id, execution_executed_by_agent, execution_executed_by_model,
			rollback_path, rollback_rolled_back_at,
			created_at, resolved_at, expires_at, approval_expires_at
		FROM requests WHERE status = ?
		ORDER BY created_at DESC
	`, string(StatusPending))
	if err != nil {
		return nil, fmt.Errorf("querying pending requests: %w", err)
	}
	defer rows.Close()

	return scanRequests(rows)
}

// ListRequestsByStatus returns requests with a given status for a project.
func (db *DB) ListRequestsByStatus(status RequestStatus, projectPath string) ([]*Request, error) {
	rows, err := db.Query(`
		SELECT id, project_path,
			command_raw, command_argv_json, command_cwd, command_shell, command_hash,
			command_display_redacted, command_contains_sensitive,
			risk_tier, requestor_session_id, requestor_agent, requestor_model,
			justification_reason, justification_expected_effect, justification_goal, justification_safety_argument,
			dry_run_command, dry_run_output, attachments_json,
			status, min_approvals, require_different_model,
			execution_log_path, execution_exit_code, execution_duration_ms,
			execution_executed_at, execution_executed_by_session_id, execution_executed_by_agent, execution_executed_by_model,
			rollback_path, rollback_rolled_back_at,
			created_at, resolved_at, expires_at, approval_expires_at
		FROM requests WHERE status = ? AND project_path = ?
		ORDER BY created_at DESC
	`, string(status), projectPath)
	if err != nil {
		return nil, fmt.Errorf("querying requests by status: %w", err)
	}
	defer rows.Close()

	return scanRequests(rows)
}

// ListAllRequests returns all requests for a project, ordered by creation time descending.
func (db *DB) ListAllRequests(projectPath string) ([]*Request, error) {
	rows, err := db.Query(`
		SELECT id, project_path,
			command_raw, command_argv_json, command_cwd, command_shell, command_hash,
			command_display_redacted, command_contains_sensitive,
			risk_tier, requestor_session_id, requestor_agent, requestor_model,
			justification_reason, justification_expected_effect, justification_goal, justification_safety_argument,
			dry_run_command, dry_run_output, attachments_json,
			status, min_approvals, require_different_model,
			execution_log_path, execution_exit_code, execution_duration_ms,
			execution_executed_at, execution_executed_by_session_id, execution_executed_by_agent, execution_executed_by_model,
			rollback_path, rollback_rolled_back_at,
			created_at, resolved_at, expires_at, approval_expires_at
		FROM requests WHERE project_path = ?
		ORDER BY created_at DESC
	`, projectPath)
	if err != nil {
		return nil, fmt.Errorf("querying all requests: %w", err)
	}
	defer rows.Close()

	return scanRequests(rows)
}

// UpdateRequestStatus updates a request's status using the state machine.
func (db *DB) UpdateRequestStatus(id string, status RequestStatus) error {
	// Get current request
	r, err := db.GetRequest(id)
	if err != nil {
		return err
	}

	// Validate transition using state machine
	if !canTransition(r.Status, status) {
		return fmt.Errorf("%w: from %s to %s", ErrInvalidTransition, r.Status, status)
	}

	// Build update query
	now := time.Now().UTC().Format(time.RFC3339)
	var resolvedAt sql.NullString
	if status.IsTerminal() {
		resolvedAt = sql.NullString{String: now, Valid: true}
	}

	result, err := db.Exec(`
		UPDATE requests SET status = ?, resolved_at = ? WHERE id = ?
	`, string(status), resolvedAt, id)
	if err != nil {
		return fmt.Errorf("updating request status: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("getting rows affected: %w", err)
	}
	if rowsAffected == 0 {
		return ErrRequestNotFound
	}

	return nil
}

// canTransition checks if a state transition is valid.
func canTransition(from, to RequestStatus) bool {
	// Terminal states cannot transition
	if from.IsTerminal() {
		return false
	}

	switch from {
	case StatusPending:
		return to == StatusApproved || to == StatusRejected || to == StatusCancelled || to == StatusTimeout
	case StatusApproved:
		return to == StatusExecuting || to == StatusCancelled
	case StatusExecuting:
		return to == StatusExecuted || to == StatusExecutionFailed || to == StatusTimedOut
	case StatusTimeout:
		return to == StatusEscalated
	default:
		return false
	}
}

// UpdateRequestExecution updates the execution details for a request.
func (db *DB) UpdateRequestExecution(id string, exec *Execution) error {
	_, err := db.Exec(`
		UPDATE requests SET
			execution_log_path = ?,
			execution_exit_code = ?,
			execution_duration_ms = ?,
			execution_executed_at = ?,
			execution_executed_by_session_id = ?,
			execution_executed_by_agent = ?,
			execution_executed_by_model = ?
		WHERE id = ?
	`,
		nullString(exec.LogPath),
		exec.ExitCode,
		exec.DurationMs,
		formatTimePtr(exec.ExecutedAt),
		nullString(exec.ExecutedBySessionID),
		nullString(exec.ExecutedByAgent),
		nullString(exec.ExecutedByModel),
		id,
	)
	if err != nil {
		return fmt.Errorf("updating request execution: %w", err)
	}
	return nil
}

// UpdateRequestRollbackPath records the rollback capture directory path for a request.
func (db *DB) UpdateRequestRollbackPath(id, rollbackPath string) error {
	_, err := db.Exec(`
		UPDATE requests SET rollback_path = ?
		WHERE id = ?
	`, nullString(rollbackPath), id)
	if err != nil {
		return fmt.Errorf("updating request rollback path: %w", err)
	}
	return nil
}

// UpdateRequestRolledBackAt records when a rollback was performed for a request.
func (db *DB) UpdateRequestRolledBackAt(id string, rolledBackAt time.Time) error {
	_, err := db.Exec(`
		UPDATE requests SET rollback_rolled_back_at = ?
		WHERE id = ?
	`, rolledBackAt.UTC().Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("updating request rolled_back_at: %w", err)
	}
	return nil
}

// CountPendingBySession counts pending requests for a session (rate limiting).
func (db *DB) CountPendingBySession(sessionID string) (int, error) {
	var count int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM requests
		WHERE requestor_session_id = ? AND status = ?
	`, sessionID, string(StatusPending)).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting pending requests: %w", err)
	}
	return count, nil
}

// CountRequestsSince counts requests created at or after the given time for a session.
// This is intended for per-minute rate limiting.
//
// NOTE: created_at is stored as RFC3339 text, so we compare against RFC3339 strings.
func (db *DB) CountRequestsSince(sessionID string, since time.Time) (int, error) {
	var count int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM requests
		WHERE requestor_session_id = ? AND created_at >= ?
	`, sessionID, since.UTC().Format(time.RFC3339)).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting requests since: %w", err)
	}
	return count, nil
}

// OldestRequestCreatedAtSince returns the oldest created_at timestamp (if any) for requests
// at or after the given time for a session.
func (db *DB) OldestRequestCreatedAtSince(sessionID string, since time.Time) (*time.Time, error) {
	var oldest sql.NullString
	err := db.QueryRow(`
		SELECT MIN(created_at) FROM requests
		WHERE requestor_session_id = ? AND created_at >= ?
	`, sessionID, since.UTC().Format(time.RFC3339)).Scan(&oldest)
	if err != nil {
		return nil, fmt.Errorf("querying oldest request created_at: %w", err)
	}
	if !oldest.Valid || oldest.String == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, oldest.String)
	if err != nil {
		return nil, fmt.Errorf("parsing oldest request created_at: %w", err)
	}
	t = t.UTC()
	return &t, nil
}

// CountRecentRequestsBySession counts requests created in the last N seconds for a session.
// Used for rate limiting (e.g., max requests per minute).
func (db *DB) CountRecentRequestsBySession(sessionID string, windowSeconds int) (int, error) {
	since := time.Now().UTC().Add(-time.Duration(windowSeconds) * time.Second)
	return db.CountRequestsSince(sessionID, since)
}

// SearchRequests performs a full-text search on requests.
func (db *DB) SearchRequests(query string) ([]*Request, error) {
	rows, err := db.Query(`
		SELECT r.id, r.project_path,
			r.command_raw, r.command_argv_json, r.command_cwd, r.command_shell, r.command_hash,
			r.command_display_redacted, r.command_contains_sensitive,
			r.risk_tier, r.requestor_session_id, r.requestor_agent, r.requestor_model,
			r.justification_reason, r.justification_expected_effect, r.justification_goal, r.justification_safety_argument,
			r.dry_run_command, r.dry_run_output, r.attachments_json,
			r.status, r.min_approvals, r.require_different_model,
			r.execution_log_path, r.execution_exit_code, r.execution_duration_ms,
			r.execution_executed_at, r.execution_executed_by_session_id, r.execution_executed_by_agent, r.execution_executed_by_model,
			r.rollback_path, r.rollback_rolled_back_at,
			r.created_at, r.resolved_at, r.expires_at, r.approval_expires_at
		FROM requests r
		JOIN requests_fts fts ON r.rowid = fts.rowid
		WHERE requests_fts MATCH ?
		ORDER BY r.created_at DESC
		LIMIT 100
	`, query)
	if err != nil {
		return nil, fmt.Errorf("searching requests: %w", err)
	}
	defer rows.Close()

	return scanRequests(rows)
}

// FindExpiredRequests finds pending requests that have expired.
func (db *DB) FindExpiredRequests() ([]*Request, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := db.Query(`
		SELECT id, project_path,
			command_raw, command_argv_json, command_cwd, command_shell, command_hash,
			command_display_redacted, command_contains_sensitive,
			risk_tier, requestor_session_id, requestor_agent, requestor_model,
			justification_reason, justification_expected_effect, justification_goal, justification_safety_argument,
			dry_run_command, dry_run_output, attachments_json,
			status, min_approvals, require_different_model,
			execution_log_path, execution_exit_code, execution_duration_ms,
			execution_executed_at, execution_executed_by_session_id, execution_executed_by_agent, execution_executed_by_model,
			rollback_path, rollback_rolled_back_at,
			created_at, resolved_at, expires_at, approval_expires_at
		FROM requests
		WHERE status = ? AND expires_at IS NOT NULL AND expires_at < ?
		ORDER BY expires_at ASC
	`, string(StatusPending), now)
	if err != nil {
		return nil, fmt.Errorf("finding expired requests: %w", err)
	}
	defer rows.Close()

	return scanRequests(rows)
}

// ComputeCommandHash computes the hash for a command spec.
// Hash = sha256(raw + "\n" + cwd + "\n" + json(argv) + "\n" + shell_bool)
func ComputeCommandHash(cmd CommandSpec) string {
	argvJSON, _ := json.Marshal(cmd.Argv)
	shellStr := "false"
	if cmd.Shell {
		shellStr = "true"
	}
	data := cmd.Raw + "\n" + cmd.Cwd + "\n" + string(argvJSON) + "\n" + shellStr
	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:])
}

// scanRequest scans a single request row.
func scanRequest(row *sql.Row) (*Request, error) {
	r := &Request{}
	var (
		argvJSON, attachmentsJSON                           sql.NullString
		cmdDisplayRedacted                                  sql.NullString
		justExpEffect, justGoal, justSafety                 sql.NullString
		dryRunCmd, dryRunOutput                             sql.NullString
		execLogPath, execExitCode, execDurationMs           sql.NullString
		execAt, execBySessionID, execByAgent, execByModel   sql.NullString
		rollbackPath, rollbackAt                            sql.NullString
		createdAt, resolvedAt, expiresAt, approvalExpiresAt sql.NullString
		riskTier, status                                    string
		minApprovals                                        int
		requireDiffModel, cmdShell, containsSensitive       int
	)

	err := row.Scan(
		&r.ID, &r.ProjectPath,
		&r.Command.Raw, &argvJSON, &r.Command.Cwd, &cmdShell, &r.Command.Hash,
		&cmdDisplayRedacted, &containsSensitive,
		&riskTier, &r.RequestorSessionID, &r.RequestorAgent, &r.RequestorModel,
		&r.Justification.Reason, &justExpEffect, &justGoal, &justSafety,
		&dryRunCmd, &dryRunOutput, &attachmentsJSON,
		&status, &minApprovals, &requireDiffModel,
		&execLogPath, &execExitCode, &execDurationMs,
		&execAt, &execBySessionID, &execByAgent, &execByModel,
		&rollbackPath, &rollbackAt,
		&createdAt, &resolvedAt, &expiresAt, &approvalExpiresAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRequestNotFound
		}
		return nil, fmt.Errorf("scanning request: %w", err)
	}

	// Parse complex fields
	r.Command.Shell = cmdShell == 1
	r.Command.ContainsSensitive = containsSensitive == 1
	r.RequireDifferentModel = requireDiffModel == 1
	r.RiskTier = RiskTier(riskTier)
	r.Status = RequestStatus(status)
	r.MinApprovals = minApprovals

	if cmdDisplayRedacted.Valid {
		r.Command.DisplayRedacted = cmdDisplayRedacted.String
	}
	if argvJSON.Valid {
		json.Unmarshal([]byte(argvJSON.String), &r.Command.Argv)
	}
	if attachmentsJSON.Valid && attachmentsJSON.String != "null" {
		json.Unmarshal([]byte(attachmentsJSON.String), &r.Attachments)
	}
	if justExpEffect.Valid {
		r.Justification.ExpectedEffect = justExpEffect.String
	}
	if justGoal.Valid {
		r.Justification.Goal = justGoal.String
	}
	if justSafety.Valid {
		r.Justification.SafetyArgument = justSafety.String
	}
	if dryRunCmd.Valid || dryRunOutput.Valid {
		r.DryRun = &DryRunResult{
			Command: dryRunCmd.String,
			Output:  dryRunOutput.String,
		}
	}

	// Execution info
	if execLogPath.Valid || execExitCode.Valid || execAt.Valid {
		r.Execution = &Execution{
			LogPath: execLogPath.String,
		}
		if execExitCode.Valid {
			var exitCode int
			fmt.Sscanf(execExitCode.String, "%d", &exitCode)
			r.Execution.ExitCode = &exitCode
		}
		if execDurationMs.Valid {
			var durationMs int64
			fmt.Sscanf(execDurationMs.String, "%d", &durationMs)
			r.Execution.DurationMs = &durationMs
		}
		if execAt.Valid {
			t, _ := time.Parse(time.RFC3339, execAt.String)
			r.Execution.ExecutedAt = &t
		}
		if execBySessionID.Valid {
			r.Execution.ExecutedBySessionID = execBySessionID.String
		}
		if execByAgent.Valid {
			r.Execution.ExecutedByAgent = execByAgent.String
		}
		if execByModel.Valid {
			r.Execution.ExecutedByModel = execByModel.String
		}
	}

	// Rollback info
	if rollbackPath.Valid || rollbackAt.Valid {
		r.Rollback = &Rollback{
			Path: rollbackPath.String,
		}
		if rollbackAt.Valid {
			t, _ := time.Parse(time.RFC3339, rollbackAt.String)
			r.Rollback.RolledBackAt = &t
		}
	}

	// Timestamps
	if createdAt.Valid {
		r.CreatedAt, _ = time.Parse(time.RFC3339, createdAt.String)
	}
	if resolvedAt.Valid {
		t, _ := time.Parse(time.RFC3339, resolvedAt.String)
		r.ResolvedAt = &t
	}
	if expiresAt.Valid {
		t, _ := time.Parse(time.RFC3339, expiresAt.String)
		r.ExpiresAt = &t
	}
	if approvalExpiresAt.Valid {
		t, _ := time.Parse(time.RFC3339, approvalExpiresAt.String)
		r.ApprovalExpiresAt = &t
	}

	return r, nil
}

// scanRequests scans multiple request rows.
func scanRequests(rows *sql.Rows) ([]*Request, error) {
	var requests []*Request
	for rows.Next() {
		r := &Request{}
		var (
			argvJSON, attachmentsJSON                           sql.NullString
			cmdDisplayRedacted                                  sql.NullString
			justExpEffect, justGoal, justSafety                 sql.NullString
			dryRunCmd, dryRunOutput                             sql.NullString
			execLogPath, execExitCode, execDurationMs           sql.NullString
			execAt, execBySessionID, execByAgent, execByModel   sql.NullString
			rollbackPath, rollbackAt                            sql.NullString
			createdAt, resolvedAt, expiresAt, approvalExpiresAt sql.NullString
			riskTier, status                                    string
			minApprovals                                        int
			requireDiffModel, cmdShell, containsSensitive       int
		)

		err := rows.Scan(
			&r.ID, &r.ProjectPath,
			&r.Command.Raw, &argvJSON, &r.Command.Cwd, &cmdShell, &r.Command.Hash,
			&cmdDisplayRedacted, &containsSensitive,
			&riskTier, &r.RequestorSessionID, &r.RequestorAgent, &r.RequestorModel,
			&r.Justification.Reason, &justExpEffect, &justGoal, &justSafety,
			&dryRunCmd, &dryRunOutput, &attachmentsJSON,
			&status, &minApprovals, &requireDiffModel,
			&execLogPath, &execExitCode, &execDurationMs,
			&execAt, &execBySessionID, &execByAgent, &execByModel,
			&rollbackPath, &rollbackAt,
			&createdAt, &resolvedAt, &expiresAt, &approvalExpiresAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scanning request row: %w", err)
		}

		// Parse complex fields (same as scanRequest)
		r.Command.Shell = cmdShell == 1
		r.Command.ContainsSensitive = containsSensitive == 1
		r.RequireDifferentModel = requireDiffModel == 1
		r.RiskTier = RiskTier(riskTier)
		r.Status = RequestStatus(status)
		r.MinApprovals = minApprovals

		if cmdDisplayRedacted.Valid {
			r.Command.DisplayRedacted = cmdDisplayRedacted.String
		}
		if argvJSON.Valid {
			json.Unmarshal([]byte(argvJSON.String), &r.Command.Argv)
		}
		if attachmentsJSON.Valid && attachmentsJSON.String != "null" {
			json.Unmarshal([]byte(attachmentsJSON.String), &r.Attachments)
		}
		if justExpEffect.Valid {
			r.Justification.ExpectedEffect = justExpEffect.String
		}
		if justGoal.Valid {
			r.Justification.Goal = justGoal.String
		}
		if justSafety.Valid {
			r.Justification.SafetyArgument = justSafety.String
		}
		if dryRunCmd.Valid || dryRunOutput.Valid {
			r.DryRun = &DryRunResult{
				Command: dryRunCmd.String,
				Output:  dryRunOutput.String,
			}
		}

		// Timestamps
		if createdAt.Valid {
			r.CreatedAt, _ = time.Parse(time.RFC3339, createdAt.String)
		}
		if resolvedAt.Valid {
			t, _ := time.Parse(time.RFC3339, resolvedAt.String)
			r.ResolvedAt = &t
		}
		if expiresAt.Valid {
			t, _ := time.Parse(time.RFC3339, expiresAt.String)
			r.ExpiresAt = &t
		}
		if approvalExpiresAt.Valid {
			t, _ := time.Parse(time.RFC3339, approvalExpiresAt.String)
			r.ApprovalExpiresAt = &t
		}

		requests = append(requests, r)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating requests: %w", err)
	}

	return requests, nil
}

// Helper functions

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func nullDryRunCommand(dr *DryRunResult) sql.NullString {
	if dr == nil {
		return sql.NullString{}
	}
	return nullString(dr.Command)
}

func nullDryRunOutput(dr *DryRunResult) sql.NullString {
	if dr == nil {
		return sql.NullString{}
	}
	return nullString(dr.Output)
}
