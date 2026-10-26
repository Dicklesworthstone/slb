package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// RequestReviewSnapshot is one consistent view of a request, all its reviews,
// and the selected reviewer. It is display evidence, not execution authority.
// The session's key digest is kept unexported so that a display model cannot
// serialize it; use SessionKeyMatches to authenticate the selected reviewer.
type RequestReviewSnapshot struct {
	Request *Request
	Reviews []*Review
	Session *Session

	sessionKeyDigest string
}

// SessionKeyMatches reports whether supplied is the selected reviewer's key,
// as of the snapshot.
func (s *RequestReviewSnapshot) SessionKeyMatches(supplied string) bool {
	return s != nil && s.Session != nil && SessionKeyMatches(s.sessionKeyDigest, supplied)
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
		result.Session, result.sessionKeyDigest, err = scanSessionWithDigest(tx.QueryRowContext(ctx,
			`SELECT `+sessionColumns+`, session_key_digest FROM sessions WHERE id = ?`, sessionID))
		if err != nil && !errors.Is(err, ErrSessionNotFound) {
			return nil, fmt.Errorf("reading reviewer snapshot: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}
