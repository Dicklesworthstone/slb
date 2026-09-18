package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PendingDecision describes a timer-driven transition. A nil decision leaves
// the request pending. Automatic approval is exclusively a zero-review CAUTION
// operation; this API cannot manufacture reviews or waive recorded constraints.
type PendingDecision struct {
	Status       RequestStatus
	ApprovalTTL  time.Duration
	AllowExpired bool // Only for an explicitly configured auto_approve_warn policy.
}

// PendingResolution reports only a committed change. Competing timers and
// reviewers may resolve a request first; their existing outcome is returned.
type PendingResolution struct {
	Request *Request
	Changed bool
}

// PendingDecider must read policy through tx, and must not send notifications
// or execute commands. It runs after SQLite reserves the writer.
type PendingDecider func(tx *sql.Tx, request *Request, now time.Time) (*PendingDecision, error)

// ResolvePendingRequest serializes timer decisions with reviews and execution.
// The decision and approval deadline are one write: a crash cannot strand a
// request between TIMEOUT and ESCALATED or publish an approval without its TTL.
func (db *DB) ResolvePendingRequest(ctx context.Context, id string, decide PendingDecider) (*PendingResolution, error) {
	if id == "" || decide == nil {
		return nil, errors.New("request ID and pending decision policy are required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE requests SET id = id WHERE id = ?`, id); err != nil {
		return nil, fmt.Errorf("reserving pending decision: %w", err)
	}
	request, err := db.GetRequestTx(tx, id)
	if err != nil {
		return nil, err
	}
	result := &PendingResolution{Request: request}
	if request.Status != StatusPending {
		return result, nil // Roll back the no-op reservation; never rewrite a decision.
	}
	decision, err := decide(tx, request, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if decision == nil {
		return result, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	expired := request.ExpiresAt != nil && !now.Before(*request.ExpiresAt)
	var approvalExpiresAt *time.Time
	switch decision.Status {
	case StatusApproved:
		if request.RiskTier != RiskTierCaution || request.MinApprovals != 0 || request.RequireDifferentModel ||
			request.Command.Hash == "" || ComputeCommandHash(request.Command) != request.Command.Hash ||
			request.CreatedAt.IsZero() || request.CreatedAt.After(now) || decision.ApprovalTTL <= 0 {
			return nil, fmt.Errorf("%w: request is not eligible for automatic approval", ErrInvalidTransition)
		}
		if expired && !decision.AllowExpired {
			return result, nil // Expiry won during policy evaluation; reevaluate next time.
		}
		var reviews, active int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM reviews WHERE request_id = ?`, id).Scan(&reviews); err != nil {
			return nil, err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE id = ? AND project_path = ?
			AND agent_name = ? AND model = ? AND ended_at IS NULL`, request.RequestorSessionID,
			request.ProjectPath, request.RequestorAgent, request.RequestorModel).Scan(&active); err != nil {
			return nil, err
		}
		if reviews != 0 || active != 1 {
			return result, nil // A review or requester revocation always wins over a timer.
		}
		deadline := now.Add(decision.ApprovalTTL)
		approvalExpiresAt = &deadline
	case StatusTimeout, StatusEscalated:
		if !expired || decision.ApprovalTTL != 0 || decision.AllowExpired {
			return nil, fmt.Errorf("%w: timeout decision requires an expired pending request", ErrInvalidTransition)
		}
	default:
		return nil, fmt.Errorf("%w: unsupported pending decision %s", ErrInvalidTransition, decision.Status)
	}
	updated, err := tx.ExecContext(ctx, `UPDATE requests SET status = ?, resolved_at = ?, approval_expires_at = ?
		WHERE id = ? AND status = 'pending'`, string(decision.Status), now.Format(time.RFC3339), formatTimePtr(approvalExpiresAt), id)
	if err != nil {
		return nil, fmt.Errorf("recording pending decision: %w", err)
	}
	count, err := updated.RowsAffected()
	if err != nil {
		return nil, err
	}
	if count != 1 {
		return nil, fmt.Errorf("%w: pending decision lost ownership", ErrInvalidTransition)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing pending decision: %w", err)
	}
	request.Status = decision.Status
	request.ResolvedAt = &now
	request.ApprovalExpiresAt = approvalExpiresAt
	result.Changed = true
	return result, nil
}
