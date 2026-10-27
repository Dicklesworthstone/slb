package db

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// RequestHistoryFilter scopes a history query before applying its result limit.
// Query uses SQLite FTS5 syntax; an empty query lists every matching status.
type RequestHistoryFilter struct {
	ProjectPath string
	Query       string
	Status      RequestStatus
	Agent       string
	RiskTier    RiskTier
	Since       time.Time
	Limit       int
}

// ListRequestHistory includes completed requests and never falls back to a
// pending-only result when a search fails. All filters and the limit belong to
// one query, so another project's traffic cannot hide matching history.
func (db *DB) ListRequestHistory(filter RequestHistoryFilter) ([]*Request, error) {
	if strings.TrimSpace(filter.ProjectPath) == "" {
		return nil, errors.New("history project is required")
	}
	if filter.Limit == 0 {
		filter.Limit = 50
	}
	if filter.Limit < 1 || filter.Limit > 10000 {
		return nil, errors.New("history limit must be between 1 and 10000")
	}
	if filter.Status != "" && !filter.Status.Valid() {
		return nil, fmt.Errorf("invalid history status %q", filter.Status)
	}
	if filter.RiskTier != "" && !filter.RiskTier.Valid() {
		return nil, fmt.Errorf("invalid history risk tier %q", filter.RiskTier)
	}
	query := `SELECT r.id, r.project_path,
		r.command_raw, r.command_argv_json, r.command_cwd, r.command_shell, r.command_hash,
		r.command_display_redacted, r.command_contains_sensitive,
		r.risk_tier, r.requestor_session_id, r.requestor_agent, r.requestor_model,
		r.justification_reason, r.justification_expected_effect, r.justification_goal, r.justification_safety_argument,
		r.dry_run_command, r.dry_run_output, r.attachments_json,
		r.status, r.min_approvals, r.require_different_model,
		r.execution_log_path, r.execution_exit_code, r.execution_duration_ms,
		r.execution_executed_at, r.execution_executed_by_session_id, r.execution_executed_by_agent, r.execution_executed_by_model,
		r.rollback_path, r.rollback_rolled_back_at,
		r.created_at, r.resolved_at, r.expires_at, r.approval_expires_at,
		r.approval_policy, r.execution_approval_policy, r.execution_assurance
		FROM requests r`
	if filter.Query != "" {
		query += ` JOIN requests_fts ON r.rowid = requests_fts.rowid`
	}
	query += ` WHERE r.project_path = ?`
	args := []any{filter.ProjectPath}
	if filter.Query != "" {
		query += ` AND requests_fts MATCH ?`
		args = append(args, filter.Query)
	}
	for _, condition := range []struct {
		column string
		value  string
	}{
		{"r.status", string(filter.Status)},
		{"r.requestor_agent", filter.Agent},
		{"r.risk_tier", string(filter.RiskTier)},
	} {
		if condition.value != "" {
			query += " AND " + condition.column + " = ?"
			args = append(args, condition.value)
		}
	}
	if !filter.Since.IsZero() {
		query += ` AND julianday(r.created_at) >= julianday(?)`
		args = append(args, filter.Since.UTC().Format(time.RFC3339Nano))
	}
	query += ` ORDER BY r.created_at DESC, r.id DESC LIMIT ?`
	args = append(args, filter.Limit)
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying request history: %w", err)
	}
	defer rows.Close()
	return scanRequests(rows)
}
