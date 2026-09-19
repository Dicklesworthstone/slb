package db

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// Migration represents a single schema migration.
type Migration struct {
	Version int
	Name    string
	Up      string
}

// migrations is the ordered list of schema migrations.
var migrations = []Migration{
	{
		Version: 1,
		Name:    "initial_schema",
		Up: `
-- Sessions: agent sessions with HMAC keys
CREATE TABLE IF NOT EXISTS sessions (
  id TEXT PRIMARY KEY,
  agent_name TEXT NOT NULL,
  program TEXT,
  model TEXT,
  project_path TEXT NOT NULL,
  session_key TEXT NOT NULL,
  started_at TEXT NOT NULL,
  last_active_at TEXT NOT NULL,
  ended_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_sessions_last_active ON sessions(last_active_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_active_agent_project
  ON sessions(agent_name, project_path)
  WHERE ended_at IS NULL;

-- Requests: command approval requests
CREATE TABLE IF NOT EXISTS requests (
  id TEXT PRIMARY KEY,
  project_path TEXT NOT NULL,
  command_raw TEXT NOT NULL,
  command_argv_json TEXT,
  command_cwd TEXT NOT NULL,
  command_shell INTEGER NOT NULL DEFAULT 0,
  command_hash TEXT NOT NULL,
  command_display_redacted TEXT,
  command_contains_sensitive INTEGER NOT NULL DEFAULT 0,
  risk_tier TEXT NOT NULL,
  requestor_session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  requestor_agent TEXT NOT NULL,
  requestor_model TEXT NOT NULL,
  justification_reason TEXT NOT NULL,
  reason TEXT GENERATED ALWAYS AS (justification_reason) STORED,
  justification_expected_effect TEXT,
  justification_goal TEXT,
  justification_safety_argument TEXT,
  dry_run_command TEXT,
  dry_run_output TEXT,
  attachments_json TEXT,
  status TEXT NOT NULL,
  min_approvals INTEGER NOT NULL,
  require_different_model INTEGER NOT NULL DEFAULT 0,
  execution_log_path TEXT,
  execution_exit_code INTEGER,
  execution_duration_ms INTEGER,
  execution_executed_at TEXT,
  execution_executed_by_session_id TEXT,
  execution_executed_by_agent TEXT,
  execution_executed_by_model TEXT,
  rollback_path TEXT,
  rollback_rolled_back_at TEXT,
  created_at TEXT NOT NULL,
  resolved_at TEXT,
  expires_at TEXT,
  approval_expires_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_requests_status ON requests(status);
CREATE INDEX IF NOT EXISTS idx_requests_project ON requests(project_path);
CREATE INDEX IF NOT EXISTS idx_requests_created ON requests(created_at);
CREATE INDEX IF NOT EXISTS idx_requests_hash ON requests(command_hash);

-- Reviews: approvals/rejections
CREATE TABLE IF NOT EXISTS reviews (
  id TEXT PRIMARY KEY,
  request_id TEXT NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
  reviewer_session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  reviewer_agent TEXT NOT NULL,
  reviewer_model TEXT NOT NULL,
  decision TEXT NOT NULL,
  signature TEXT NOT NULL,
  signature_timestamp TEXT NOT NULL,
  responses_json TEXT,
  comments TEXT,
  created_at TEXT NOT NULL,
  UNIQUE(request_id, reviewer_session_id)
);

-- Full-text search for requests (external content table)
CREATE VIRTUAL TABLE IF NOT EXISTS requests_fts USING fts5(
  request_id UNINDEXED,
  command_raw,
  justification,
  requestor_agent,
  status,
  content='requests',
  content_rowid='rowid'
);

CREATE TRIGGER IF NOT EXISTS requests_ai AFTER INSERT ON requests BEGIN
  INSERT INTO requests_fts(rowid, request_id, command_raw, justification, requestor_agent, status)
  VALUES (new.rowid, new.id, new.command_raw,
          COALESCE(new.justification_reason,'') || ' ' || COALESCE(new.justification_expected_effect,'') || ' ' ||
          COALESCE(new.justification_goal,'') || ' ' || COALESCE(new.justification_safety_argument,''),
          new.requestor_agent, new.status);
END;

CREATE TRIGGER IF NOT EXISTS requests_au AFTER UPDATE ON requests BEGIN
  INSERT INTO requests_fts(requests_fts, rowid, request_id, command_raw, justification, requestor_agent, status)
  VALUES ('delete', old.rowid, old.id, old.command_raw,
          COALESCE(old.justification_reason,'') || ' ' || COALESCE(old.justification_expected_effect,'') || ' ' ||
          COALESCE(old.justification_goal,'') || ' ' || COALESCE(old.justification_safety_argument,''),
          old.requestor_agent, old.status);
  INSERT INTO requests_fts(rowid, request_id, command_raw, justification, requestor_agent, status)
  VALUES (new.rowid, new.id, new.command_raw,
          COALESCE(new.justification_reason,'') || ' ' || COALESCE(new.justification_expected_effect,'') || ' ' ||
          COALESCE(new.justification_goal,'') || ' ' || COALESCE(new.justification_safety_argument,''),
          new.requestor_agent, new.status);
END;

CREATE TRIGGER IF NOT EXISTS requests_ad AFTER DELETE ON requests BEGIN
  DELETE FROM requests_fts WHERE rowid = old.rowid;
END;

-- Execution outcomes for analytics/learning
CREATE TABLE IF NOT EXISTS execution_outcomes (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  request_id TEXT NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
  result TEXT,
  notes TEXT,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Pattern change audit trail
CREATE TABLE IF NOT EXISTS pattern_changes (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  tier TEXT NOT NULL,
  pattern TEXT NOT NULL,
  change_type TEXT NOT NULL, -- add/remove/suggest
  reason TEXT,
  status TEXT NOT NULL DEFAULT 'pending',
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_pattern_changes_status ON pattern_changes(status);
CREATE INDEX IF NOT EXISTS idx_pattern_changes_type ON pattern_changes(change_type);

-- Agent-added patterns
CREATE TABLE IF NOT EXISTS custom_patterns (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  tier TEXT NOT NULL,
  pattern TEXT NOT NULL,
  description TEXT,
  source TEXT,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  UNIQUE(tier, pattern)
);
`,
	},
	{
		Version: 2,
		Name:    "sessions_rate_limit_reset_at",
		Up: `
-- Per-session rate limit reset timestamp (human escape hatch).
ALTER TABLE sessions ADD COLUMN rate_limit_reset_at TEXT;
`,
	},
	{
		Version: 3,
		Name:    "execution_outcomes_enhanced",
		Up: `
-- Enhance execution_outcomes for analytics/learning.
ALTER TABLE execution_outcomes ADD COLUMN caused_problems INTEGER NOT NULL DEFAULT 0;
ALTER TABLE execution_outcomes ADD COLUMN problem_description TEXT;
ALTER TABLE execution_outcomes ADD COLUMN human_rating INTEGER;
ALTER TABLE execution_outcomes ADD COLUMN human_notes TEXT;
`,
	},
	{
		Version: 4,
		Name:    "durable_request_events",
		Up:      requestEventsSchema,
	},
}

// ApplyMigrations applies any pending migrations in order.
func (db *DB) ApplyMigrations(ctx context.Context) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if err := ensureMigrationsTable(db.conn); err != nil {
		return err
	}

	current, err := currentVersion(db.conn)
	if err != nil {
		return err
	}

	// Connections can migrate concurrently. Never sort a shared global slice.
	ordered := append([]Migration(nil), migrations...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Version < ordered[j].Version })

	for _, m := range ordered {
		if m.Version <= current {
			continue
		}

		tx, err := db.conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", m.Version, err)
		}

		// Reserve the SQLite writer before re-reading the version. A second
		// CLI may have finished this migration while we waited; running its
		// DDL/backfill again would fail or create duplicate baseline events.
		if _, err := tx.ExecContext(ctx, `UPDATE schema_migrations SET version = version WHERE 0`); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("locking migration %d: %w", m.Version, err)
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("rechecking migration %d: %w", m.Version, err)
		}
		if m.Version <= current {
			_ = tx.Rollback()
			continue
		}

		// Special-case migrations that need conditional DDL
		switch m.Version {
		case 2:
			if err := addColumnIfMissing(ctx, tx, "sessions", "rate_limit_reset_at", "TEXT"); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("migration %d (%s) failed: %w", m.Version, m.Name, err)
			}
		case 3:
			// Add enhanced columns to execution_outcomes
			cols := []struct{ name, def string }{
				{"caused_problems", "INTEGER NOT NULL DEFAULT 0"},
				{"problem_description", "TEXT"},
				{"human_rating", "INTEGER"},
				{"human_notes", "TEXT"},
			}
			for _, col := range cols {
				if err := addColumnIfMissing(ctx, tx, "execution_outcomes", col.name, col.def); err != nil {
					_ = tx.Rollback()
					return fmt.Errorf("migration %d (%s) failed: %w", m.Version, m.Name, err)
				}
			}
		default:
			if _, err := tx.ExecContext(ctx, m.Up); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("migration %d (%s) failed: %w", m.Version, m.Name, err)
			}
		}

		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO schema_migrations(version, applied_at) VALUES(?, ?)`, m.Version, time.Now().UTC().Format(time.RFC3339)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %d: %w", m.Version, err)
		}

		if err := tx.Commit(); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("commit migration %d: %w", m.Version, err)
		}
	}

	return nil
}

func ensureMigrationsTable(conn *sql.DB) error {
	_, err := conn.Exec(`
CREATE TABLE IF NOT EXISTS schema_migrations (
  version INTEGER PRIMARY KEY,
  applied_at TEXT NOT NULL
);`)
	return err
}

func currentVersion(conn *sql.DB) (int, error) {
	var v sql.NullInt64
	err := conn.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}

func addColumnIfMissing(ctx context.Context, tx *sql.Tx, table, column, colType string) error {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return fmt.Errorf("pragma table_info: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var colName, ctype string
		var notnull int
		var dfltValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &colName, &ctype, &notnull, &dfltValue, &pk); err != nil {
			return fmt.Errorf("scan pragma table_info: %w", err)
		}
		if colName == column {
			return nil // already exists
		}
	}
	if rows.Err() != nil {
		return fmt.Errorf("iterating table_info: %w", rows.Err())
	}

	_, err = tx.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, colType))
	if err != nil {
		return fmt.Errorf("add column %s.%s: %w", table, column, err)
	}
	return nil
}
