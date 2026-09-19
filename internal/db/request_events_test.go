package db

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type journalSQLWriter interface {
	Exec(string, ...any) (sql.Result, error)
}

func journalExec(t *testing.T, writer journalSQLWriter, query string, args ...any) {
	t.Helper()
	if _, err := writer.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func journalTestDB(t *testing.T) *DB {
	t.Helper()
	database, err := OpenAndMigrate(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func journalTestSession(t *testing.T, writer journalSQLWriter, id, project string) {
	t.Helper()
	journalExec(t, writer, `INSERT INTO sessions(id, agent_name, model, project_path, session_key, started_at, last_active_at)
		VALUES (?, ?, 'model', ?, 'PRIVATE-KEY', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, id, id, project)
}

func journalTestRequest(t *testing.T, writer journalSQLWriter, id, project, session string) {
	t.Helper()
	journalExec(t, writer, `INSERT INTO requests(id, project_path, command_raw, command_cwd, command_hash,
		risk_tier, requestor_session_id, requestor_agent, requestor_model, justification_reason,
		status, min_approvals, created_at) VALUES (?, ?, 'echo artifact', '/private/cwd', 'digest',
		'dangerous', ?, 'agent', 'model', 'test only', 'pending', 1, '2026-01-01T00:00:00Z')`, id, project, session)
}

func journalTestReview(t *testing.T, writer journalSQLWriter, id, request, session string) {
	t.Helper()
	journalExec(t, writer, `INSERT INTO reviews(id, request_id, reviewer_session_id, reviewer_agent, reviewer_model,
		decision, signature, signature_timestamp, comments, created_at)
		VALUES (?, ?, ?, 'reviewer', 'other-model', 'approve', 'proof', '2026-01-01T00:00:00Z', 'private comment', '2026-01-01T00:00:00Z')`, id, request, session)
}

func journalPage(t *testing.T, database *DB, project, cursor string, limit int) *RequestEventPage {
	t.Helper()
	page, err := database.ReadRequestEvents(context.Background(), project, cursor, limit)
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func TestRequestJournalAtomicLifecycle(t *testing.T) {
	database := journalTestDB(t)
	journalTestSession(t, database, "author", "/project")
	journalTestSession(t, database, "reviewer", "/project")
	journalTestRequest(t, database, "request", "/project", "author")
	first := journalPage(t, database, "/project", "", 1)
	if len(first.Events) != 1 || first.Events[0].Kind != "request_created" || first.HasMore {
		t.Fatalf("bad creation: %+v", first)
	}

	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	journalTestReview(t, tx, "review", "request", "reviewer")
	journalExec(t, tx, `UPDATE requests SET status='approved', approval_expires_at='2026-01-02T00:00:00Z' WHERE id='request'`)
	journalExec(t, tx, `UPDATE requests SET status='executing', execution_log_path='SECRET-RECEIPT', execution_executed_by_session_id='author' WHERE id='request'`)
	journalExec(t, tx, `UPDATE requests SET status='execution_failed', execution_exit_code=7, execution_duration_ms=12 WHERE id='request'`)
	// The reader opens another pooled connection and must not see uncommitted events.
	if page := journalPage(t, database, "/project", first.Cursor, 10); len(page.Events) != 0 {
		t.Fatal("uncommitted events visible")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	page := journalPage(t, database, "/project", first.Cursor, 2)
	if len(page.Events) != 2 || !page.HasMore {
		t.Fatalf("pagination lost events: %+v", page)
	}
	if page.Events[0].Kind != "review_added" || page.Events[0].Review.ID != "review" || page.Events[0].State.Approvals != 1 || page.Events[0].State.Status != "pending" {
		t.Fatal("review observation was replaced by later request state")
	}
	if page.Events[1].State.Status != "approved" || page.Events[1].PreviousStatus == nil || *page.Events[1].PreviousStatus != "pending" {
		t.Fatal("approval transition missing")
	}
	last := journalPage(t, database, "/project", page.Cursor, 2)
	if len(last.Events) != 2 || last.HasMore || last.Events[0].State.Status != "executing" || last.Events[1].State.ExitCode == nil || *last.Events[1].State.ExitCode != 7 {
		t.Fatalf("execution history lost: %+v", last)
	}
	if again := journalPage(t, database, "/project", last.Cursor, 2); len(again.Events) != 0 || again.Cursor != last.Cursor {
		t.Fatal("resume replayed acknowledged events")
	}
	// Consumers can checkpoint each individual event, not just the page boundary.
	partial := journalPage(t, database, "/project", page.Events[0].Cursor, 10)
	if len(partial.Events) != 3 || partial.Events[0].Sequence != page.Events[1].Sequence {
		t.Fatal("partial-page resume skipped work")
	}
}

func TestRequestJournalRollbackAndMandatoryRecording(t *testing.T) {
	database := journalTestDB(t)
	journalTestSession(t, database, "author", "/project")
	journalTestRequest(t, database, "request", "/project", "author")
	before := journalPage(t, database, "/project", "", 10)
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	journalExec(t, tx, `UPDATE requests SET status='approved' WHERE id='request'`)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if page := journalPage(t, database, "/project", before.Cursor, 10); len(page.Events) != 0 {
		t.Fatal("rollback left a phantom event")
	}
	journalExec(t, database, `CREATE TRIGGER injected_journal_failure BEFORE INSERT ON request_events BEGIN SELECT RAISE(ABORT, 'journal unavailable'); END`)
	if _, err := database.Exec(`UPDATE requests SET status='approved' WHERE id='request'`); err == nil {
		t.Fatal("state committed without its journal event")
	}
	var status string
	if err := database.QueryRow(`SELECT status FROM requests WHERE id='request'`).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("failed journal did not roll back mutation: %s %v", status, err)
	}
}

func TestRequestJournalNoOpReservationsAndEvidenceChanges(t *testing.T) {
	database := journalTestDB(t)
	journalTestSession(t, database, "author", "/project")
	journalTestRequest(t, database, "request", "/project", "author")
	cursor := journalPage(t, database, "/project", "", 10).Cursor
	journalExec(t, database, `UPDATE requests SET id=id, status=status, command_hash=command_hash WHERE id='request'`)
	if page := journalPage(t, database, "/project", cursor, 10); len(page.Events) != 0 {
		t.Fatal("no-op writer reservation flooded journal")
	}
	for _, assignment := range []string{
		`min_approvals=2`, `require_different_model=1`, `command_raw='changed but hash not updated'`,
		`command_argv_json='["different"]'`, `command_cwd='/new/cwd'`, `command_shell=1`,
		`attachments_json='[{"content":"SECRET"}]'`, `dry_run_output='SECRET'`, `justification_reason='changed'`,
		`expires_at='2026-01-02T00:00:00Z'`, `execution_log_path='SECRET-RECEIPT'`,
		`rollback_path='/private/snapshot'`, `rollback_rolled_back_at='2026-01-02T00:00:00Z'`,
	} {
		journalExec(t, database, `UPDATE requests SET `+assignment+` WHERE id='request'`)
		page := journalPage(t, database, "/project", cursor, 10)
		if len(page.Events) != 1 || page.Events[0].Kind != "request_updated" {
			t.Fatalf("missing mutation %s: %+v", assignment, page)
		}
		cursor = page.Cursor
	}
}

func TestRequestJournalExcludesPrivateEvidence(t *testing.T) {
	database := journalTestDB(t)
	journalTestSession(t, database, "author", "/project")
	journalTestSession(t, database, "reviewer", "/project")
	journalTestRequest(t, database, "request", "/project", "author")
	journalExec(t, database, `UPDATE requests SET command_raw='SECRET-COMMAND', command_argv_json='["SECRET-ARGV"]',
		command_cwd='SECRET-CWD', command_display_redacted='SECRET-BAD-REDACTION', command_contains_sensitive=1,
		requestor_agent='SECRET-AGENT', requestor_model='SECRET-MODEL', justification_reason='SECRET-REASON',
		dry_run_command='SECRET-PREVIEW', dry_run_output='SECRET-OUTPUT', attachments_json='["SECRET-ATTACHMENT"]',
		execution_log_path='SECRET-RECEIPT', rollback_path='SECRET-BACKUP' WHERE id='request'`)
	journalTestReview(t, database, "review", "request", "reviewer")
	journalExec(t, database, `UPDATE reviews SET comments='SECRET-COMMENTS', responses_json='{"reason_response":"SECRET"}', signature='SECRET-SIGNATURE' WHERE id='review'`)
	page := journalPage(t, database, "/project", "", 100)
	data, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "SECRET") || strings.Contains(string(data), "PRIVATE-KEY") || strings.Contains(string(data), "/private/cwd") {
		t.Fatalf("private material leaked: %s", data)
	}
	rows, err := database.Query(`SELECT payload FROM request_events`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(payload, "SECRET") || strings.Contains(payload, "private comment") {
			t.Fatal("raw journal retained private evidence")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestRequestJournalRetainsDeletionAndReviewRemoval(t *testing.T) {
	database := journalTestDB(t)
	journalTestSession(t, database, "author", "/project")
	journalTestSession(t, database, "reviewer", "/project")
	journalTestRequest(t, database, "request", "/project", "author")
	journalTestReview(t, database, "review", "request", "reviewer")
	cursor := journalPage(t, database, "/project", "", 10).Cursor
	journalExec(t, database, `DELETE FROM reviews WHERE id='review'`)
	page := journalPage(t, database, "/project", cursor, 10)
	if len(page.Events) != 1 || page.Events[0].Kind != "review_removed" || page.Events[0].State.Approvals != 0 {
		t.Fatal("removed vote was not journaled")
	}
	journalTestReview(t, database, "review-again", "request", "reviewer")
	cursor = journalPage(t, database, "/project", "", 100).Cursor
	// Cascading deletion of a session must not cascade into the event journal.
	journalExec(t, database, `DELETE FROM sessions WHERE id='author'`)
	page = journalPage(t, database, "/project", cursor, 100)
	found := false
	for _, event := range page.Events {
		if event.Kind == "request_deleted" {
			found = event.State.Approvals == 1
		}
	}
	if !found {
		t.Fatal("deletion lost the last request state")
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM requests_fts WHERE requests_fts MATCH 'artifact'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("FTS deletion failed: %d %v", count, err)
	}
	if len(journalPage(t, database, "/project", "", 100).Events) < 5 {
		t.Fatal("deletion erased journal history")
	}
}

func TestRequestJournalProjectMoveAndIsolation(t *testing.T) {
	database := journalTestDB(t)
	journalTestSession(t, database, "author", "/one")
	journalTestRequest(t, database, "request", "/one", "author")
	old := journalPage(t, database, "/one", "", 100)
	journalExec(t, database, `UPDATE requests SET project_path='/two' WHERE id='request'`)
	moved := journalPage(t, database, "/one", old.Cursor, 100)
	if len(moved.Events) != 1 || moved.Events[0].Kind != "request_removed" || moved.Events[0].ProjectPath != "/one" {
		t.Fatal("old project missed its removal")
	}
	newProject := journalPage(t, database, "/two", "", 100)
	if len(newProject.Events) != 1 || newProject.Events[0].Kind != "request_updated" {
		t.Fatal("new project missed its addition")
	}
	if _, err := database.ReadRequestEvents(context.Background(), "/two", old.Cursor, 100); !errors.Is(err, ErrRequestEventScope) {
		t.Fatalf("cross-project cursor accepted: %v", err)
	}
}

func TestRequestJournalCursorBinding(t *testing.T) {
	database := journalTestDB(t)
	journalTestSession(t, database, "author", "/project")
	journalTestRequest(t, database, "request", "/project", "author")
	cursor := journalPage(t, database, "/project", "", 100).Cursor
	other := journalTestDB(t)
	if _, err := other.ReadRequestEvents(context.Background(), "/project", cursor, 100); !errors.Is(err, ErrRequestJournalChanged) {
		t.Fatalf("database replacement not detected: %v", err)
	}
	for _, query := range []string{
		`UPDATE request_events SET kind='changed'`, `DELETE FROM request_events`,
		`UPDATE request_journal_meta SET journal_id=journal_id`, `DELETE FROM request_journal_meta`,
	} {
		if _, err := database.Exec(query); err == nil {
			t.Fatalf("append-only guard missing: %s", query)
		}
	}
	// Simulate an administrator/restore changing a resume record. This checksum
	// detects mismatch; it does not make a writable SQLite file tamper-proof.
	journalExec(t, database, `DROP TRIGGER request_events_no_update`)
	journalExec(t, database, `UPDATE request_events SET payload=json_set(payload, '$.state.status', 'cancelled') WHERE sequence=1`)
	if _, err := database.ReadRequestEvents(context.Background(), "/project", cursor, 100); !errors.Is(err, ErrRequestEventCursor) {
		t.Fatalf("changed cursor anchor accepted: %v", err)
	}
	decoded, err := decodeRequestEventCursor(cursor)
	if err != nil {
		t.Fatal(err)
	}
	decoded.Sequence = 99999
	missing, err := encodeRequestEventCursor(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ReadRequestEvents(context.Background(), "/project", missing, 100); !errors.Is(err, ErrRequestEventCursor) {
		t.Fatal("nonexistent sequence accepted")
	}
}

func TestRequestJournalCursorValidation(t *testing.T) {
	valid := requestEventCursor{Version: 1, Journal: strings.Repeat("a", 32), Project: "/project"}
	for _, mutate := range []func(*requestEventCursor){
		func(c *requestEventCursor) { c.Version = 2 }, func(c *requestEventCursor) { c.Project = "" },
		func(c *requestEventCursor) { c.Journal = "wrong" }, func(c *requestEventCursor) { c.Sequence = -1 },
		func(c *requestEventCursor) { c.Anchor = strings.Repeat("a", 64) },
		func(c *requestEventCursor) { c.Sequence = 1 },
	} {
		cursor := valid
		mutate(&cursor)
		encoded, err := encodeRequestEventCursor(cursor)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeRequestEventCursor(encoded); !errors.Is(err, ErrRequestEventCursor) {
			t.Fatalf("invalid cursor accepted: %+v", cursor)
		}
	}
	encoded, err := encodeRequestEventCursor(valid)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeRequestEventCursor(encoded)
	if err != nil || decoded != valid {
		t.Fatal("cursor round trip failed")
	}
	for _, value := range []string{"!", strings.Repeat("a", maxRequestCursorBytes+1),
		base64.RawURLEncoding.EncodeToString([]byte(`null`)),
		base64.RawURLEncoding.EncodeToString([]byte(`{} {}`)),
		base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"j":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","p":"/project","s":0,"h":"","unknown":true}`)),
	} {
		if _, err := decodeRequestEventCursor(value); !errors.Is(err, ErrRequestEventCursor) {
			t.Fatalf("malformed cursor accepted: %q", value)
		}
	}
}

func TestRequestJournalTailHasNoHandoffGap(t *testing.T) {
	database := journalTestDB(t)
	cursor, err := database.RequestEventHead(context.Background(), "/project")
	if err != nil || cursor == "" {
		t.Fatalf("empty head: %s %v", cursor, err)
	}
	journalTestSession(t, database, "author", "/project")
	journalTestRequest(t, database, "first", "/project", "author")
	if page := journalPage(t, database, "/project", cursor, 100); len(page.Events) != 1 {
		t.Fatal("first event lost after empty head")
	}
	cursor, err = database.RequestEventHead(context.Background(), "/project")
	if err != nil {
		t.Fatal(err)
	}
	journalTestRequest(t, database, "second", "/project", "author")
	page := journalPage(t, database, "/project", cursor, 100)
	if len(page.Events) != 1 || page.Events[0].RequestID != "second" {
		t.Fatal("tail lost or replayed an event")
	}
}

func TestRequestJournalReadValidationAndCancellation(t *testing.T) {
	database := journalTestDB(t)
	for _, limit := range []int{-1, MaxRequestEventLimit + 1} {
		if _, err := database.ReadRequestEvents(context.Background(), "/project", "", limit); err == nil {
			t.Fatal("invalid limit accepted")
		}
	}
	if _, err := database.ReadRequestEvents(context.Background(), "", "", 1); err == nil {
		t.Fatal("unscoped read accepted")
	}
	if _, err := database.RequestEventHead(context.Background(), ""); err == nil {
		t.Fatal("unscoped tail accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := database.ReadRequestEvents(ctx, "/project", "", 100); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
	readonly, err := OpenWithOptions(database.Path(), OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer readonly.Close()
	if page := journalPage(t, readonly, "/project", "", 0); len(page.Events) != 0 || page.Cursor == "" {
		t.Fatal("readonly empty page failed")
	}
}

func TestRequestJournalConcurrentWritersAndPagination(t *testing.T) {
	database := journalTestDB(t)
	journalTestSession(t, database, "author", "/project")
	const clients = 12
	connections := make([]*DB, clients)
	for i := range connections {
		c, err := OpenWithOptions(database.Path(), OpenOptions{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		connections[i] = c
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, connection := range connections {
		wg.Add(1)
		go func(i int, connection *DB) {
			defer wg.Done()
			<-start
			tx, err := connection.Begin()
			if err != nil {
				t.Error(err)
				return
			}
			defer tx.Rollback()
			// Match admission/review/execution: reserve the writer before FTS
			// preparation or any read can establish a deferred snapshot.
			journalExec(t, tx, `UPDATE sessions SET id=id WHERE id='author'`)
			id := fmt.Sprintf("request-%d", i)
			journalTestRequest(t, tx, id, "/project", "author")
			journalExec(t, tx, `UPDATE requests SET status='approved' WHERE id=?`, id)
			journalExec(t, tx, `UPDATE requests SET status='executing' WHERE id=?`, id)
			journalExec(t, tx, `UPDATE requests SET status='executed', execution_exit_code=0 WHERE id=?`, id)
			if err := tx.Commit(); err != nil {
				t.Error(err)
			}
		}(i, connection)
	}
	close(start)
	wg.Wait()
	var events []RequestEvent
	cursor := ""
	for {
		page := journalPage(t, database, "/project", cursor, 5)
		events = append(events, page.Events...)
		cursor = page.Cursor
		if !page.HasMore {
			break
		}
	}
	if len(events) != 4*clients {
		t.Fatalf("lost committed mutations: %d", len(events))
	}
	for i, event := range events {
		if i > 0 && event.Sequence <= events[i-1].Sequence {
			t.Fatal("event sequence is not monotonic")
		}
		if i%4 != 0 && event.RequestID != events[i-1].RequestID {
			t.Fatal("another writer interleaved within a transaction")
		}
	}
}

func TestRequestJournalMigrationUpgradeAndConcurrentOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3.db")
	legacy, err := OpenWithOptions(path, OpenOptions{CreateIfNotExists: true})
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if err := ensureMigrationsTable(legacy.conn); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations {
		if migration.Version >= 4 {
			continue
		}
		journalExec(t, legacy, migration.Up)
		journalExec(t, legacy, `INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`, migration.Version, time.Now().UTC().Format(time.RFC3339))
	}
	journalTestSession(t, legacy, "author", "/project")
	for i := 0; i < 15; i++ {
		journalTestRequest(t, legacy, fmt.Sprint(i), "/project", "author")
	}
	const clients = 8
	connections := make([]*DB, clients)
	for i := range connections {
		c, err := OpenWithOptions(path, OpenOptions{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		connections[i] = c
	}
	start := make(chan struct{})
	results := make(chan error, clients)
	for _, c := range connections {
		go func(c *DB) { <-start; results <- c.ApplyMigrations(context.Background()) }(c)
	}
	close(start)
	for i := 0; i < clients; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	page := journalPage(t, legacy, "/project", "", 100)
	if len(page.Events) != 15 {
		t.Fatalf("baseline duplicated or omitted: %d", len(page.Events))
	}
	for _, event := range page.Events {
		if event.Kind != "request_baseline" {
			t.Fatal("upgrade fabricated pre-migration history")
		}
	}
	if err := legacy.ValidateSchema(); err != nil {
		t.Fatal(err)
	}
	before := page.Cursor
	if err := legacy.ApplyMigrations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if again := journalPage(t, legacy, "/project", before, 100); len(again.Events) != 0 {
		t.Fatal("reopen re-seeded baseline")
	}
}

// Old binaries need not know about the journal: their autocommit inserts still
// produce an event via the migrated database's triggers.
func TestRequestJournalLegacyAutocommitWriters(t *testing.T) {
	database := journalTestDB(t)
	journalTestSession(t, database, "author", "/project")
	const clients = 8
	var wg sync.WaitGroup
	for i := 0; i < clients; i++ {
		c, err := OpenWithOptions(database.Path(), OpenOptions{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			journalTestRequest(t, c, fmt.Sprint(i), "/project", "author")
		}(i)
	}
	wg.Wait()
	if len(journalPage(t, database, "/project", "", 100).Events) != clients {
		t.Fatal("legacy writer failed to publish")
	}
}
