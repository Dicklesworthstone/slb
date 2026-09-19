package db

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

//go:embed request_events.sql
var requestEventsSchema string

const (
	DefaultRequestEventLimit = 100
	MaxRequestEventLimit     = 1000
	maxRequestEventBytes     = 64 * 1024
	maxRequestCursorBytes    = 16 * 1024
)

var (
	ErrRequestEventCursor    = errors.New("invalid or no longer available request event cursor")
	ErrRequestJournalChanged = errors.New("request event cursor belongs to a different database journal")
	ErrRequestEventScope     = errors.New("request event cursor belongs to a different project")
)

// RequestEventState is the metadata observed AT the mutation, not the current
// request. Counts are submitted review rows, not verified execution permission.
// Sensitive/free-form evidence and execution receipts are intentionally absent.
type RequestEventState struct {
	CommandHash           string  `json:"command_hash"`
	Status                string  `json:"status"`
	RiskTier              string  `json:"risk_tier"`
	RequestorSessionID    string  `json:"requestor_session_id"`
	MinApprovals          int     `json:"min_approvals"`
	RequireDifferentModel bool    `json:"require_different_model"`
	Approvals             int     `json:"approvals"`
	Rejections            int     `json:"rejections"`
	CreatedAt             string  `json:"created_at"`
	ExpiresAt             *string `json:"expires_at"`
	ResolvedAt            *string `json:"resolved_at"`
	ApprovalExpiresAt     *string `json:"approval_expires_at"`
	ExecutorSessionID     *string `json:"executor_session_id"`
	ExecutedAt            *string `json:"executed_at"`
	ExitCode              *int    `json:"exit_code"`
	DurationMs            *int64  `json:"duration_ms"`
	RolledBackAt          *string `json:"rolled_back_at"`
}

type RequestEventReview struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Decision  string `json:"decision"`
}

// RequestEvent is a durable lifecycle observation. Baselines capture rows that
// existed when journaling was enabled, and do not reconstruct earlier history.
// Cursor resumes strictly AFTER this event and binds its database, project and
// contents. Treat it as opaque, and persist it only after handling the event.
type RequestEvent struct {
	Version        int                 `json:"version"`
	Sequence       int64               `json:"sequence"`
	Kind           string              `json:"event"`
	ProjectPath    string              `json:"project_path"`
	RequestID      string              `json:"request_id"`
	OccurredAt     string              `json:"occurred_at"`
	State          RequestEventState   `json:"state"`
	Review         *RequestEventReview `json:"review,omitempty"`
	PreviousStatus *string             `json:"previous_status,omitempty"`
	Cursor         string              `json:"cursor"`
}

type RequestEventPage struct {
	Events  []RequestEvent `json:"events"`
	Cursor  string         `json:"cursor"`
	HasMore bool           `json:"has_more"`
}

// The anchor detects changed/deleted resume records, including ordinary backup
// restores followed by different writes at the same sequence. It is a checksum,
// NOT a MAC or a defense against an administrator rewriting the entire journal.
type requestEventCursor struct {
	Version  int    `json:"v"`
	Journal  string `json:"j"`
	Project  string `json:"p"`
	Sequence int64  `json:"s"`
	Anchor   string `json:"h"`
}

type requestEventPayload struct {
	State          RequestEventState   `json:"state"`
	Review         *RequestEventReview `json:"review,omitempty"`
	PreviousStatus *string             `json:"previous_status,omitempty"`
}

const requestEventColumns = `sequence, kind, project_path, request_id, occurred_at, payload`

// ReadRequestEvents returns a bounded project-scoped page from ONE read
// transaction. Resume validation and the page therefore see the same snapshot.
// No rows are dropped to fit the page: HasMore means call again with Cursor.
// An empty after starts at the journal's beginning (which may include baselines).
func (db *DB) ReadRequestEvents(ctx context.Context, project, after string, limit int) (*RequestEventPage, error) {
	if project == "" {
		return nil, errors.New("request event project is required")
	}
	if limit == 0 {
		limit = DefaultRequestEventLimit
	}
	if limit < 1 || limit > MaxRequestEventLimit {
		return nil, fmt.Errorf("request event limit must be between 1 and %d", MaxRequestEventLimit)
	}
	var cursor requestEventCursor
	if after != "" {
		var err error
		cursor, err = decodeRequestEventCursor(after)
		if err != nil {
			return nil, err
		}
		if cursor.Project != project {
			return nil, ErrRequestEventScope
		}
	}
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	journal, err := readRequestJournalID(ctx, tx)
	if err != nil {
		return nil, err
	}
	if after == "" {
		cursor = requestEventCursor{Version: 1, Journal: journal, Project: project}
	} else {
		if cursor.Journal != journal {
			return nil, ErrRequestJournalChanged
		}
		if cursor.Sequence != 0 {
			_, anchor, err := scanRequestEvent(tx.QueryRowContext(ctx,
				`SELECT `+requestEventColumns+` FROM request_events WHERE sequence = ? AND project_path = ?`,
				cursor.Sequence, project), journal)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, ErrRequestEventCursor
			}
			if err != nil {
				return nil, fmt.Errorf("validating request event cursor: %w", err)
			}
			if anchor != cursor.Anchor {
				return nil, ErrRequestEventCursor
			}
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+requestEventColumns+` FROM request_events
		WHERE project_path = ? AND sequence > ? ORDER BY sequence LIMIT ?`, project, cursor.Sequence, limit+1)
	if err != nil {
		return nil, fmt.Errorf("reading request events: %w", err)
	}
	page := &RequestEventPage{Events: make([]RequestEvent, 0)}
	for rows.Next() {
		if len(page.Events) == limit {
			page.HasMore = true
			break
		}
		event, _, err := scanRequestEvent(rows, journal)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("decoding request event: %w", err)
		}
		page.Events = append(page.Events, event)
		page.Cursor = event.Cursor
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil || closeErr != nil {
		return nil, errors.Join(err, closeErr)
	}
	if page.Cursor == "" {
		page.Cursor, err = encodeRequestEventCursor(cursor)
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return page, nil
}

// RequestEventHead returns the cursor AFTER the most recent event for project.
// It is useful for an explicitly requested tail, never implicit recovery from a
// bad cursor. Taking this snapshot then reading after it has no handoff gap.
func (db *DB) RequestEventHead(ctx context.Context, project string) (string, error) {
	if project == "" {
		return "", errors.New("request event project is required")
	}
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	journal, err := readRequestJournalID(ctx, tx)
	if err != nil {
		return "", err
	}
	event, _, err := scanRequestEvent(tx.QueryRowContext(ctx, `SELECT `+requestEventColumns+`
		FROM request_events WHERE project_path = ? ORDER BY sequence DESC LIMIT 1`, project), journal)
	if errors.Is(err, sql.ErrNoRows) {
		event.Cursor, err = encodeRequestEventCursor(requestEventCursor{Version: 1, Journal: journal, Project: project})
	}
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return event.Cursor, nil
}

func readRequestJournalID(ctx context.Context, tx *sql.Tx) (string, error) {
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT journal_id FROM request_journal_meta WHERE singleton = 1`).Scan(&id); err != nil {
		return "", fmt.Errorf("reading request journal (schema version 4 or later required): %w", err)
	}
	if !requestEventHex(id, 16) {
		return "", errors.New("invalid request journal identity")
	}
	return id, nil
}

type requestEventScanner interface{ Scan(...any) error }

func scanRequestEvent(scanner requestEventScanner, journal string) (RequestEvent, string, error) {
	event := RequestEvent{Version: 1}
	var payload string
	if err := scanner.Scan(&event.Sequence, &event.Kind, &event.ProjectPath, &event.RequestID, &event.OccurredAt, &payload); err != nil {
		return event, "", err
	}
	if event.Sequence <= 0 || event.Kind == "" || len(payload) > maxRequestEventBytes {
		return event, "", errors.New("invalid request journal record")
	}
	if _, err := time.Parse(time.RFC3339Nano, event.OccurredAt); err != nil {
		return event, "", fmt.Errorf("invalid request event timestamp: %w", err)
	}
	var data requestEventPayload
	if err := decodeRequestEventJSON([]byte(payload), &data); err != nil {
		return event, "", err
	}
	event.State, event.Review, event.PreviousStatus = data.State, data.Review, data.PreviousStatus
	// Hash the STORED representation, so future display/serialization changes
	// cannot invalidate a cursor and no plaintext evidence is needed in it.
	binding, err := json.Marshal([]any{event.Version, event.Sequence, event.Kind, event.ProjectPath,
		event.RequestID, event.OccurredAt, payload})
	if err != nil {
		return event, "", err
	}
	sum := sha256.Sum256(binding)
	anchor := hex.EncodeToString(sum[:])
	event.Cursor, err = encodeRequestEventCursor(requestEventCursor{
		Version: 1, Journal: journal, Project: event.ProjectPath, Sequence: event.Sequence, Anchor: anchor,
	})
	return event, anchor, err
}

func encodeRequestEventCursor(cursor requestEventCursor) (string, error) {
	data, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(data)
	if len(encoded) > maxRequestCursorBytes {
		return "", fmt.Errorf("%w: project path exceeds cursor budget", ErrRequestEventCursor)
	}
	return encoded, nil
}

func decodeRequestEventCursor(encoded string) (requestEventCursor, error) {
	var cursor requestEventCursor
	if len(encoded) > maxRequestCursorBytes {
		return cursor, ErrRequestEventCursor
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return cursor, ErrRequestEventCursor
	}
	if err := decodeRequestEventJSON(data, &cursor); err != nil || cursor.Version != 1 || cursor.Project == "" ||
		!requestEventHex(cursor.Journal, 16) || cursor.Sequence < 0 ||
		(cursor.Sequence == 0 && cursor.Anchor != "") || (cursor.Sequence > 0 && !requestEventHex(cursor.Anchor, 32)) {
		return cursor, ErrRequestEventCursor
	}
	return cursor, nil
}

func requestEventHex(value string, bytes int) bool {
	if len(value) != 2*bytes {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func decodeRequestEventJSON(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing data in request journal JSON")
	}
	return nil
}
