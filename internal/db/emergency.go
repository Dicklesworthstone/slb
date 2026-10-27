package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Emergency executions deliberately bypass review. They have a separate ledger
// and cannot become requests, approvals, or independently authenticated evidence.
const emergencyExecutionSchema = `
CREATE TABLE emergency_executions (
  id TEXT PRIMARY KEY,
  project_path TEXT NOT NULL,
  command_raw TEXT NOT NULL,
  command_argv_json TEXT NOT NULL,
  command_cwd TEXT NOT NULL,
  command_shell INTEGER NOT NULL CHECK(command_shell IN (0, 1)),
  command_hash TEXT NOT NULL,
  command_display_redacted TEXT NOT NULL DEFAULT '',
  command_contains_sensitive INTEGER NOT NULL DEFAULT 0 CHECK(command_contains_sensitive IN (0, 1)),
  raw_command_hash TEXT NOT NULL,
  actor TEXT NOT NULL,
  reason TEXT NOT NULL,
  confirmation_method TEXT NOT NULL CHECK(confirmation_method IN ('interactive', 'hash_ack')),
  configured_approval_policy TEXT NOT NULL DEFAULT 'independent' CHECK(configured_approval_policy IN ('independent', 'same_host')),
  log_path TEXT NOT NULL UNIQUE,
  rollback_path TEXT NOT NULL DEFAULT '',
  assurance TEXT NOT NULL CHECK(assurance = 'emergency_unreviewed'),
  status TEXT NOT NULL CHECK(status IN ('incomplete', 'succeeded', 'failed', 'timed_out', 'cancelled')),
  recorded_at TEXT NOT NULL,
  started_at TEXT,
  completed_at TEXT,
  pid INTEGER,
  exit_code INTEGER CHECK(exit_code IS NULL OR exit_code >= -1),
  duration_ms INTEGER CHECK(duration_ms IS NULL OR duration_ms >= 0),
  error TEXT NOT NULL DEFAULT '',
  CHECK((started_at IS NULL AND pid IS NULL) OR (started_at IS NOT NULL AND pid IS NOT NULL AND pid > 0)),
  CHECK((status = 'incomplete' AND completed_at IS NULL AND exit_code IS NULL AND duration_ms IS NULL AND error = '')
     OR (status <> 'incomplete' AND completed_at IS NOT NULL)),
  CHECK(status <> 'succeeded' OR (started_at IS NOT NULL AND exit_code IS NOT NULL AND exit_code = 0
     AND duration_ms IS NOT NULL AND error = ''))
);
CREATE INDEX idx_emergency_executions_project_time ON emergency_executions(project_path, recorded_at DESC, id DESC);
CREATE INDEX idx_emergency_executions_project_status ON emergency_executions(project_path, status, recorded_at DESC, id DESC);
`

const (
	// AssuranceEmergencyUnreviewed never satisfies any review policy.
	AssuranceEmergencyUnreviewed     ReviewAssurance = AssuranceEmergency
	EmergencyConfirmationInteractive                 = "interactive"
	EmergencyConfirmationHash                        = "hash_ack"
)

// EmergencyStatus describes observed execution progress, never approval status.
type EmergencyStatus string

const (
	// Incomplete means the outcome is unknown. It does not prove that a child
	// started, is still running, or can safely be retried.
	EmergencyStatusIncomplete EmergencyStatus = "incomplete"
	EmergencyStatusSucceeded  EmergencyStatus = "succeeded"
	EmergencyStatusFailed     EmergencyStatus = "failed"
	EmergencyStatusTimedOut   EmergencyStatus = "timed_out"
	EmergencyStatusCancelled  EmergencyStatus = "cancelled"
)

func (s EmergencyStatus) Valid() bool {
	switch s {
	case EmergencyStatusIncomplete, EmergencyStatusSucceeded, EmergencyStatusFailed,
		EmergencyStatusTimedOut, EmergencyStatusCancelled:
		return true
	default:
		return false
	}
}

var (
	ErrEmergencyExecutionNotFound = errors.New("emergency execution not found")
	ErrEmergencyTransition        = errors.New("invalid emergency execution transition")
)

// EmergencyExecution records an explicit, unreviewed override. Actor is the
// caller-supplied label, not an independently authenticated identity. The
// immutable command, reason, confirmation and log location are stored before
// starting a child. A crash leaves Status incomplete, with no inferred outcome.
type EmergencyExecution struct {
	ID                 string      `json:"id"`
	ProjectPath        string      `json:"project_path"`
	Command            CommandSpec `json:"command"`
	RawCommandHash     string      `json:"raw_command_hash"`
	Actor              string      `json:"actor"`
	Reason             string      `json:"reason"`
	ConfirmationMethod string      `json:"confirmation_method"`
	// ConfiguredApprovalPolicy is context about the policy that was bypassed.
	// It never determines Assurance and is not a claim that review occurred.
	ConfiguredApprovalPolicy ApprovalPolicy  `json:"configured_approval_policy"`
	LogPath                  string          `json:"log_path"`
	RollbackPath             string          `json:"rollback_path,omitempty"`
	Assurance                ReviewAssurance `json:"assurance"`
	Status                   EmergencyStatus `json:"status"`
	RecordedAt               time.Time       `json:"recorded_at"`
	StartedAt                *time.Time      `json:"started_at,omitempty"`
	CompletedAt              *time.Time      `json:"completed_at,omitempty"`
	PID                      *int            `json:"pid,omitempty"`
	ExitCode                 *int            `json:"exit_code,omitempty"`
	DurationMs               *int64          `json:"duration_ms,omitempty"`
	Error                    string          `json:"error,omitempty"`
}

// EmergencyOutcome is the observed result of one attempt. A nil exit code or
// duration means no such result was observed; it must never become a fake zero.
type EmergencyOutcome struct {
	ExitCode   *int
	DurationMs *int64
	Error      string
}

// EmergencyExecutionFilter is always project-scoped and bounded. Since is
// inclusive and Before is exclusive, both applied to the recorded intent time.
type EmergencyExecutionFilter struct {
	ProjectPath string
	ID          string
	Status      EmergencyStatus
	// ConfiguredApprovalPolicy filters bypassed configuration, not evidence.
	ConfiguredApprovalPolicy ApprovalPolicy
	Actor                    string
	Query                    string
	Since                    time.Time
	Before                   time.Time
	Limit                    int
}

// Fixed-width UTC timestamps preserve chronological ordering in SQLite TEXT.
const emergencyTimeLayout = "2006-01-02T15:04:05.000000000Z"

// CreateEmergencyExecution commits an intent before its caller starts a child.
// It creates neither a session nor a request, and assigns the unreviewed label
// itself. Callers must not execute when this operation fails.
func (db *DB) CreateEmergencyExecution(record *EmergencyExecution) error {
	if record == nil {
		return errors.New("emergency execution is required")
	}
	if !filepath.IsAbs(record.ProjectPath) || !filepath.IsAbs(record.Command.Cwd) {
		return errors.New("emergency execution project and working directory must be absolute")
	}
	if strings.TrimSpace(record.Command.Raw) == "" || strings.ContainsRune(record.Command.Raw, '\x00') {
		return errors.New("emergency command must be nonempty and contain no NUL bytes")
	}
	if strings.TrimSpace(record.Reason) == "" || strings.TrimSpace(record.Actor) == "" || record.LogPath == "" {
		return errors.New("emergency execution requires a reason, actor label and log path")
	}
	if record.ConfirmationMethod != EmergencyConfirmationInteractive && record.ConfirmationMethod != EmergencyConfirmationHash {
		return errors.New("invalid emergency confirmation method")
	}
	if !record.ConfiguredApprovalPolicy.Valid() {
		return errors.New("invalid configured policy for emergency execution")
	}
	if record.Assurance != "" && record.Assurance != AssuranceEmergencyUnreviewed {
		return errors.New("emergency execution cannot contain review assurance")
	}
	if record.Status != "" && record.Status != EmergencyStatusIncomplete {
		return errors.New("emergency execution must begin incomplete")
	}
	if !record.RecordedAt.IsZero() || record.StartedAt != nil || record.CompletedAt != nil ||
		record.PID != nil || record.ExitCode != nil || record.DurationMs != nil || record.Error != "" {
		return errors.New("emergency execution cannot be created with an existing lifecycle outcome")
	}

	value := *record
	value.ConfiguredApprovalPolicy = value.ConfiguredApprovalPolicy.Normalized()
	if value.ID == "" {
		value.ID = uuid.NewString()
	}
	hash := ComputeCommandHash(value.Command)
	if value.Command.Hash != "" && value.Command.Hash != hash {
		return errors.New("emergency command hash does not match its execution context")
	}
	value.Command.Hash = hash
	rawHash := sha256.Sum256([]byte(value.Command.Raw))
	rawDigest := hex.EncodeToString(rawHash[:])
	if value.RawCommandHash != "" && value.RawCommandHash != rawDigest {
		return errors.New("emergency acknowledgment hash does not match its command")
	}
	value.RawCommandHash = rawDigest
	value.Assurance = AssuranceEmergencyUnreviewed
	value.Status = EmergencyStatusIncomplete
	value.RecordedAt = time.Now().UTC()
	argv, err := json.Marshal(value.Command.Argv)
	if err != nil {
		return fmt.Errorf("encoding emergency command arguments: %w", err)
	}
	_, err = db.Exec(`INSERT INTO emergency_executions (
		id, project_path, command_raw, command_argv_json, command_cwd, command_shell, command_hash,
		command_display_redacted, command_contains_sensitive, raw_command_hash,
		actor, reason, confirmation_method, configured_approval_policy, log_path, rollback_path, assurance, status, recorded_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		value.ID, value.ProjectPath, value.Command.Raw, string(argv), value.Command.Cwd,
		boolToInt(value.Command.Shell), value.Command.Hash, value.Command.DisplayRedacted,
		boolToInt(value.Command.ContainsSensitive), value.RawCommandHash,
		value.Actor, value.Reason, value.ConfirmationMethod, value.ConfiguredApprovalPolicy, value.LogPath, value.RollbackPath,
		value.Assurance, value.Status, value.RecordedAt.Format(emergencyTimeLayout))
	if err != nil {
		return fmt.Errorf("recording emergency execution intent: %w", err)
	}
	*record = value
	return nil
}

// MarkEmergencyExecutionStarted records an observed child startup once. A
// caller whose startup record fails must stop and reap its child. A missing
// startup record means startup was not durably recorded, not that it never ran.
func (db *DB) MarkEmergencyExecutionStarted(id string, pid int) error {
	if id == "" || pid <= 0 {
		return errors.New("emergency startup requires an execution ID and positive child PID")
	}
	result, err := db.Exec(`UPDATE emergency_executions SET started_at = ?, pid = ?
		WHERE id = ? AND status = 'incomplete' AND started_at IS NULL AND pid IS NULL
		AND assurance = 'emergency_unreviewed'`, time.Now().UTC().Format(emergencyTimeLayout), pid, id)
	return checkEmergencyTransition(result, err)
}

// CompleteEmergencyExecution consumes one incomplete intent. It cannot reopen
// an execution or modify the recorded command, actor, reason, log, or assurance.
// Failure outcomes may retain an observed exit code after the startup record
// failed; success requires a durably recorded startup and an observed zero exit.
func (db *DB) CompleteEmergencyExecution(id string, status EmergencyStatus, outcome EmergencyOutcome) error {
	if id == "" || !status.Valid() || status == EmergencyStatusIncomplete {
		return errors.New("emergency completion requires an execution ID and terminal status")
	}
	if outcome.ExitCode != nil && *outcome.ExitCode < -1 {
		return errors.New("invalid emergency exit code")
	}
	if outcome.DurationMs != nil && *outcome.DurationMs < 0 {
		return errors.New("emergency execution duration must not be negative")
	}
	if status == EmergencyStatusSucceeded {
		if outcome.ExitCode == nil || *outcome.ExitCode != 0 || outcome.DurationMs == nil || outcome.Error != "" {
			return errors.New("successful emergency execution requires an observed zero exit, duration and no error")
		}
	} else if outcome.Error == "" && (status != EmergencyStatusFailed || outcome.ExitCode == nil || *outcome.ExitCode == 0) {
		return errors.New("failed emergency execution requires an observed failure or error")
	}
	result, err := db.Exec(`UPDATE emergency_executions
		SET status = ?, completed_at = ?, exit_code = ?, duration_ms = ?, error = ?
		WHERE id = ? AND status = 'incomplete' AND assurance = 'emergency_unreviewed'
		AND (? <> 'succeeded' OR (started_at IS NOT NULL AND pid IS NOT NULL))`,
		status, time.Now().UTC().Format(emergencyTimeLayout), outcome.ExitCode, outcome.DurationMs, outcome.Error, id, status)
	return checkEmergencyTransition(result, err)
}

func checkEmergencyTransition(result sql.Result, err error) error {
	if err != nil {
		return fmt.Errorf("recording emergency execution lifecycle: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking emergency execution transition: %w", err)
	}
	if changed != 1 {
		return ErrEmergencyTransition
	}
	return nil
}

const selectEmergencyExecution = `SELECT id, project_path, command_raw, command_argv_json,
	command_cwd, command_shell, command_hash, command_display_redacted, command_contains_sensitive,
	raw_command_hash, actor, reason, confirmation_method, configured_approval_policy, log_path, rollback_path,
	assurance, status, recorded_at, started_at, completed_at, pid, exit_code, duration_ms, error
	FROM emergency_executions`

func (db *DB) GetEmergencyExecution(id string) (*EmergencyExecution, error) {
	return scanEmergencyExecution(db.QueryRow(selectEmergencyExecution+` WHERE id = ?`, id))
}

func (db *DB) ListEmergencyExecutions(filter EmergencyExecutionFilter) ([]*EmergencyExecution, error) {
	if !filepath.IsAbs(filter.ProjectPath) {
		return nil, errors.New("emergency history requires an absolute project path")
	}
	if filter.Status != "" && !filter.Status.Valid() {
		return nil, errors.New("invalid emergency history status")
	}
	if !filter.ConfiguredApprovalPolicy.Valid() {
		return nil, errors.New("invalid configured policy for emergency history")
	}
	if filter.Limit == 0 {
		filter.Limit = 100
	}
	if filter.Limit < 1 || filter.Limit > 10000 {
		return nil, errors.New("emergency history limit must be between 1 and 10000")
	}
	if !filter.Since.IsZero() && !filter.Before.IsZero() && !filter.Since.Before(filter.Before) {
		return nil, errors.New("emergency history --since must precede --before")
	}
	query := selectEmergencyExecution + ` WHERE project_path = ?`
	args := []any{filter.ProjectPath}
	for _, field := range []struct{ column, value string }{
		{"id", filter.ID}, {"status", string(filter.Status)}, {"actor", filter.Actor},
		{"configured_approval_policy", string(filter.ConfiguredApprovalPolicy)},
	} {
		if field.value != "" {
			query += " AND " + field.column + " = ?"
			args = append(args, field.value)
		}
	}
	if filter.Query != "" {
		query += ` AND instr(lower(command_raw || ' ' || reason || ' ' || actor), lower(?)) > 0`
		args = append(args, filter.Query)
	}
	if !filter.Since.IsZero() {
		query += ` AND recorded_at >= ?`
		args = append(args, filter.Since.UTC().Format(emergencyTimeLayout))
	}
	if !filter.Before.IsZero() {
		query += ` AND recorded_at < ?`
		args = append(args, filter.Before.UTC().Format(emergencyTimeLayout))
	}
	query += ` ORDER BY recorded_at DESC, id DESC LIMIT ?`
	args = append(args, filter.Limit)
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing emergency executions: %w", err)
	}
	defer rows.Close()
	records := make([]*EmergencyExecution, 0)
	for rows.Next() {
		record, err := scanEmergencyExecution(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

type emergencyScanner interface {
	Scan(...any) error
}

func scanEmergencyExecution(scanner emergencyScanner) (*EmergencyExecution, error) {
	var record EmergencyExecution
	var argv, recorded string
	var shell, sensitive int
	var started, completed sql.NullString
	var pid, exit, duration sql.NullInt64
	err := scanner.Scan(&record.ID, &record.ProjectPath, &record.Command.Raw, &argv,
		&record.Command.Cwd, &shell, &record.Command.Hash, &record.Command.DisplayRedacted, &sensitive,
		&record.RawCommandHash, &record.Actor, &record.Reason, &record.ConfirmationMethod, &record.ConfiguredApprovalPolicy, &record.LogPath, &record.RollbackPath,
		&record.Assurance, &record.Status, &recorded, &started, &completed, &pid, &exit, &duration, &record.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrEmergencyExecutionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("reading emergency execution: %w", err)
	}
	if record.Assurance != AssuranceEmergencyUnreviewed || !record.Status.Valid() {
		return nil, errors.New("invalid emergency execution category")
	}
	if record.ConfiguredApprovalPolicy == "" || !record.ConfiguredApprovalPolicy.Valid() {
		return nil, errors.New("invalid emergency execution configuration context")
	}
	if err := json.Unmarshal([]byte(argv), &record.Command.Argv); err != nil {
		return nil, fmt.Errorf("reading emergency command arguments: %w", err)
	}
	record.Command.Shell = shell != 0
	record.Command.ContainsSensitive = sensitive != 0
	if record.RecordedAt, err = time.Parse(time.RFC3339Nano, recorded); err != nil {
		return nil, fmt.Errorf("reading emergency recorded time: %w", err)
	}
	for _, field := range []struct {
		source sql.NullString
		target **time.Time
	}{{started, &record.StartedAt}, {completed, &record.CompletedAt}} {
		if field.source.Valid {
			value, err := time.Parse(time.RFC3339Nano, field.source.String)
			if err != nil {
				return nil, fmt.Errorf("reading emergency lifecycle time: %w", err)
			}
			*field.target = &value
		}
	}
	if pid.Valid {
		value := int(pid.Int64)
		record.PID = &value
	}
	if exit.Valid {
		value := int(exit.Int64)
		record.ExitCode = &value
	}
	if duration.Valid {
		record.DurationMs = &duration.Int64
	}
	return &record, nil
}
