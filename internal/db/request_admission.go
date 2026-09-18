package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ErrRequestAdmissionIdentity means the requester ended or changed identity
// after the caller prepared its request. Retrying requires a fresh session read.
var ErrRequestAdmissionIdentity = errors.New("requester session is inactive or its identity changed")

// RequestAdmissionLimits applies to new peer-review requests, not execution or
// administrative imports. Both limits must be positive; callers resolve defaults.
type RequestAdmissionLimits struct {
	MaxPending   int
	MaxPerMinute int
	WarnOnly     bool
}

// RequestAdmissionStats describes the counters BEFORE the attempted insertion.
// Exceeded remains true for warn-only admissions, even though they succeed.
type RequestAdmissionStats struct {
	Pending  int
	Recent   int
	ResetAt  time.Time
	Exceeded bool
}

// RequestAdmissionLimitError is a rejected admission, not a storage failure.
// The transaction has been rolled back; no request or quota slot was consumed.
type RequestAdmissionLimitError struct{ Stats RequestAdmissionStats }

func (e *RequestAdmissionLimitError) Error() string { return "request admission rate limit exceeded" }

// AdmitRequest checks identity, counters and the reset watermark, then inserts
// the request under ONE SQLite writer reservation. A process-local mutex or a
// read-then-CreateRequest sequence cannot enforce quotas across CLI processes.
//
// Only a successfully committed admission updates r. Notifications must happen
// after this method returns, never inside the transaction. CreateRequest remains
// the low-level import/fixture API; normal submissions must use this gate.
func (db *DB) AdmitRequest(ctx context.Context, r *Request, limits RequestAdmissionLimits) (*RequestAdmissionStats, error) {
	return db.admitRequest(ctx, r, limits, time.Now)
}

func (db *DB) admitRequest(ctx context.Context, r *Request, limits RequestAdmissionLimits, clock func() time.Time) (*RequestAdmissionStats, error) {
	if r == nil || r.RequestorSessionID == "" {
		return nil, errors.New("request and requester session are required")
	}
	if limits.MaxPending <= 0 || limits.MaxPerMinute <= 0 {
		return nil, errors.New("request admission limits must be positive")
	}
	if r.Status != "" && r.Status != StatusPending {
		return nil, errors.New("only pending requests may be admitted")
	}
	candidate := *r
	computedHash := ComputeCommandHash(candidate.Command)
	if candidate.Command.Hash != "" && candidate.Command.Hash != computedHash {
		return nil, errors.New("request command hash does not match its contents")
	}
	candidate.Command.Hash = computedHash
	candidate.Status = StatusPending
	argv, err := json.Marshal(candidate.Command.Argv)
	if err != nil {
		return nil, fmt.Errorf("encoding command arguments: %w", err)
	}
	attachments, err := json.Marshal(candidate.Attachments)
	if err != nil {
		return nil, fmt.Errorf("encoding request attachments: %w", err)
	}

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning request admission: %w", err)
	}
	// Also release the reservation on cancellation or panic.
	defer func() { _ = tx.Rollback() }()

	// Reserve the writer BEFORE taking a read snapshot. In WAL mode, promoting
	// a stale read transaction after another writer commits can otherwise fail
	// with SQLITE_BUSY_SNAPSHOT instead of observing the newly consumed quota.
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET last_active_at = last_active_at WHERE id = ?`, candidate.RequestorSessionID); err != nil {
		return nil, fmt.Errorf("reserving request admission: %w", err)
	}
	var project, agent, model string
	var endedAt, resetAt sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT project_path, agent_name, COALESCE(model, ''), ended_at, rate_limit_reset_at
		FROM sessions WHERE id = ?
	`, candidate.RequestorSessionID).Scan(&project, &agent, &model, &endedAt, &resetAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("reading admission session: %w", err)
	}
	if endedAt.Valid || project != candidate.ProjectPath || agent != candidate.RequestorAgent || model != candidate.RequestorModel {
		return nil, ErrRequestAdmissionIdentity
	}

	// Sample time after obtaining the lock: a caller may have waited for a
	// competing transaction. Expired rows must not consume a full new minute.
	now := clock().UTC()
	// Legacy requests are stored with second precision. Include the entire
	// boundary second so truncation cannot release a burst up to one second
	// early. This matches the existing advisory limiter's conservative window.
	windowStart := now.Truncate(time.Second).Add(-time.Minute)
	var resetWatermark any
	if resetAt.Valid && resetAt.String != "" {
		reset, err := time.Parse(time.RFC3339Nano, resetAt.String)
		if err != nil {
			return nil, fmt.Errorf("parsing admission reset watermark: %w", err)
		}
		if reset.After(now) {
			return nil, errors.New("session rate limit reset watermark is in the future")
		}
		resetWatermark = reset.UTC().Format(time.RFC3339Nano)
	}
	stats := &RequestAdmissionStats{}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM requests WHERE requestor_session_id = ? AND status = ?
	`, candidate.RequestorSessionID, string(StatusPending)).Scan(&stats.Pending); err != nil {
		return nil, fmt.Errorf("counting pending admissions: %w", err)
	}
	// julianday handles legacy second-precision values, fractional seconds and
	// UTC offsets consistently. A reset is inclusive, so it cannot erase a new
	// request created at the same stored timestamp as the reset itself.
	var oldest sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*), MIN(created_at) FROM requests
		WHERE requestor_session_id = ? AND julianday(created_at) >= julianday(?)
		  AND (? IS NULL OR julianday(created_at) >= julianday(?))
	`, candidate.RequestorSessionID, windowStart.Format(time.RFC3339Nano), resetWatermark, resetWatermark).Scan(&stats.Recent, &oldest); err != nil {
		return nil, fmt.Errorf("counting recent admissions: %w", err)
	}
	if oldest.Valid {
		createdAt, err := time.Parse(time.RFC3339Nano, oldest.String)
		if err != nil {
			return nil, fmt.Errorf("parsing oldest admission: %w", err)
		}
		stats.ResetAt = createdAt.UTC().Truncate(time.Second).Add(time.Minute + time.Second)
	}
	stats.Exceeded = stats.Pending >= limits.MaxPending || stats.Recent >= limits.MaxPerMinute
	if stats.Exceeded && !limits.WarnOnly {
		return stats, &RequestAdmissionLimitError{Stats: *stats}
	}

	if candidate.ID == "" {
		candidate.ID = uuid.New().String()
	}
	candidate.CreatedAt = now
	if candidate.ExpiresAt == nil {
		expires := now.Add(DefaultRequestTimeout)
		candidate.ExpiresAt = &expires
	}
	if !candidate.ExpiresAt.After(now) {
		return nil, errors.New("request expired before admission")
	}
	_, err = tx.ExecContext(ctx, `
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
		candidate.ID, candidate.ProjectPath,
		candidate.Command.Raw, string(argv), candidate.Command.Cwd, boolToInt(candidate.Command.Shell), candidate.Command.Hash,
		nullString(candidate.Command.DisplayRedacted), boolToInt(candidate.Command.ContainsSensitive),
		string(candidate.RiskTier), candidate.RequestorSessionID, candidate.RequestorAgent, candidate.RequestorModel,
		candidate.Justification.Reason, nullString(candidate.Justification.ExpectedEffect), nullString(candidate.Justification.Goal), nullString(candidate.Justification.SafetyArgument),
		nullDryRunCommand(candidate.DryRun), nullDryRunOutput(candidate.DryRun), string(attachments),
		string(candidate.Status), candidate.MinApprovals, boolToInt(candidate.RequireDifferentModel),
		candidate.CreatedAt.Format(time.RFC3339), formatTimePtr(candidate.ExpiresAt), formatTimePtr(candidate.ApprovalExpiresAt),
	)
	if err != nil {
		return nil, fmt.Errorf("inserting admitted request: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing request admission: %w", err)
	}
	*r = candidate
	return stats, nil
}
