package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// WatchRequestState is a lightweight display projection, not an authorization
// proof. It deliberately excludes argv, attachments, raw secrets and session keys.
type WatchRequestState struct {
	ID, ProjectPath, Command, CommandHash, Requestor string
	Status                                           RequestStatus
	RiskTier                                         RiskTier
	ApprovalPolicy                                   ApprovalPolicy
	ExecutionAssurance                               ReviewAssurance
	MinApprovals, Approvals, Rejections              int
	ExitCode                                         *int
	CreatedAt, ResolvedAt                            string
}

type ProjectWatchState struct {
	Requests       []WatchRequestState
	ActiveSessions int
}

// ReadProjectWatchState reads request and session state from one SQLite snapshot.
// Keep all unresolved requests, plus recently resolved ones, so clients see
// completions without loading an unbounded audit history or attachment bodies.
// Previously observed active IDs may be included to recover their final state
// after a disconnect longer than the recent-completion window. They remain
// project-scoped and do not cause unrelated historical rows to be loaded.
// State notifications may coalesce rapid transitions; the database is the audit
// source of truth. A size limit fails explicitly instead of silently omitting rows.
func (db *DB) ReadProjectWatchState(ctx context.Context, project string, since time.Time, includeIDs ...string) (*ProjectWatchState, error) {
	if project == "" {
		return nil, errors.New("watch project is required")
	}
	if len(includeIDs) > 10000 {
		return nil, errors.New("watch cannot track more than 10000 active requests")
	}
	args := []any{project, since.UTC().Format(time.RFC3339)}
	includeClause := ""
	if len(includeIDs) > 0 {
		includeClause = " OR r.id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(includeIDs)), ",") + ")"
		for _, id := range includeIDs {
			args = append(args, id)
		}
	}
	tx, err := db.conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `
		SELECT r.id, r.project_path,
			CASE WHEN r.command_contains_sensitive != 0 THEN
				COALESCE(NULLIF(r.command_display_redacted, ''), '[REDACTED]')
			ELSE COALESCE(NULLIF(r.command_display_redacted, ''), r.command_raw) END,
			r.command_hash, r.requestor_agent, r.status, r.risk_tier, r.min_approvals,
			(SELECT COUNT(*) FROM reviews v WHERE v.request_id = r.id AND v.decision = 'approve'),
			(SELECT COUNT(*) FROM reviews v WHERE v.request_id = r.id AND v.decision = 'reject'),
			r.execution_exit_code, r.created_at, COALESCE(r.resolved_at, ''),
			r.approval_policy,
			CASE WHEN r.execution_log_path IS NOT NULL OR r.execution_exit_code IS NOT NULL OR r.execution_executed_at IS NOT NULL
				THEN COALESCE(NULLIF(r.execution_assurance, ''), 'legacy_unknown') ELSE '' END
		FROM requests r
		WHERE r.project_path = ? AND (
			r.status NOT IN ('executed', 'execution_failed', 'rejected', 'cancelled', 'timed_out')
			OR julianday(COALESCE(r.resolved_at, r.created_at)) >= julianday(?)
		`+includeClause+`)
		ORDER BY r.created_at, r.id LIMIT 10001
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("reading watch requests: %w", err)
	}
	state := &ProjectWatchState{Requests: make([]WatchRequestState, 0)}
	for rows.Next() {
		var r WatchRequestState
		var exit sql.NullInt64
		if err := rows.Scan(&r.ID, &r.ProjectPath, &r.Command, &r.CommandHash, &r.Requestor,
			&r.Status, &r.RiskTier, &r.MinApprovals, &r.Approvals, &r.Rejections,
			&exit, &r.CreatedAt, &r.ResolvedAt, &r.ApprovalPolicy, &r.ExecutionAssurance); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if exit.Valid {
			code := int(exit.Int64)
			r.ExitCode = &code
		}
		state.Requests = append(state.Requests, r)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil || closeErr != nil {
		return nil, errors.Join(err, closeErr)
	}
	if len(state.Requests) > 10000 {
		return nil, errors.New("watch snapshot exceeds 10000 requests; narrow the project or resolve stale requests")
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE project_path = ? AND ended_at IS NULL`, project).Scan(&state.ActiveSessions); err != nil {
		return nil, fmt.Errorf("reading watch sessions: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return state, nil
}
