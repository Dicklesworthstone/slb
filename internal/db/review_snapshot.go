package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// RequestReviewSnapshot is one consistent view of a request, all its reviews,
// and the selected reviewer. It is display evidence, not execution authority.
// SessionKey is already excluded from Session's JSON representation.
type RequestReviewSnapshot struct {
	Request *Request
	Reviews []*Review
	Session *Session
}

// ReadRequestReviewSnapshot prevents a refresh from displaying a committed
// decision with pre-commit reviews (or vice versa). Errors return no partial
// snapshot. The optional reviewer may be missing/ended without hiding history.
func (db *DB) ReadRequestReviewSnapshot(ctx context.Context, project, id, sessionID string) (*RequestReviewSnapshot, error) {
	if project == "" || id == "" {
		return nil, errors.New("project and request ID are required")
	}
	tx, err := db.conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	target, err := db.GetRequestTx(tx, id)
	if err != nil {
		return nil, err
	}
	if target.ProjectPath != project {
		return nil, errors.New("request belongs to another project")
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, request_id, reviewer_session_id, reviewer_agent, reviewer_model,
		decision, signature, signature_timestamp, responses_json, comments, created_at
		FROM reviews WHERE request_id = ? ORDER BY created_at, rowid`, id)
	if err != nil {
		return nil, fmt.Errorf("reading review snapshot: %w", err)
	}
	reviews, readErr := scanReviewList(rows)
	closeErr := rows.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	result := &RequestReviewSnapshot{Request: target, Reviews: reviews}
	if sessionID != "" {
		result.Session, err = scanSession(tx.QueryRowContext(ctx, `SELECT id, agent_name, program, model, project_path,
			session_key, started_at, last_active_at, ended_at FROM sessions WHERE id = ?`, sessionID))
		if err != nil && !errors.Is(err, ErrSessionNotFound) {
			return nil, fmt.Errorf("reading reviewer snapshot: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}
