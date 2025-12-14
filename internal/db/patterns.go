// Package db provides pattern change CRUD operations.
package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PatternChange represents a pending pattern modification request.
type PatternChange struct {
	ID         int64     `json:"id"`
	Tier       string    `json:"tier"`
	Pattern    string    `json:"pattern"`
	ChangeType string    `json:"change_type"` // add, remove, suggest
	Reason     string    `json:"reason"`
	Status     string    `json:"status"` // pending, approved, rejected
	CreatedAt  time.Time `json:"created_at"`
}

// PatternChangeStatus constants.
const (
	PatternChangeStatusPending  = "pending"
	PatternChangeStatusApproved = "approved"
	PatternChangeStatusRejected = "rejected"
)

// PatternChangeType constants.
const (
	PatternChangeTypeAdd     = "add"
	PatternChangeTypeRemove  = "remove"
	PatternChangeTypeSuggest = "suggest"
)

// ErrPatternChangeNotFound is returned when a pattern change is not found.
var ErrPatternChangeNotFound = errors.New("pattern change not found")

// CreatePatternChange creates a new pattern change request.
func (db *DB) CreatePatternChange(pc *PatternChange) error {
	if pc.Status == "" {
		pc.Status = PatternChangeStatusPending
	}
	if pc.CreatedAt.IsZero() {
		pc.CreatedAt = time.Now().UTC()
	}

	result, err := db.Exec(`
		INSERT INTO pattern_changes (tier, pattern, change_type, reason, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, pc.Tier, pc.Pattern, pc.ChangeType, pc.Reason, pc.Status, pc.CreatedAt.Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("creating pattern change: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("getting last insert id: %w", err)
	}
	pc.ID = id

	return nil
}

// GetPatternChange retrieves a pattern change by ID.
func (db *DB) GetPatternChange(id int64) (*PatternChange, error) {
	row := db.QueryRow(`
		SELECT id, tier, pattern, change_type, reason, status, created_at
		FROM pattern_changes WHERE id = ?
	`, id)

	return scanPatternChange(row)
}

// ListPendingPatternChanges returns all pending pattern changes.
func (db *DB) ListPendingPatternChanges() ([]*PatternChange, error) {
	return db.ListPatternChangesByStatus(PatternChangeStatusPending)
}

// ListPatternChangesByStatus returns pattern changes with a given status.
func (db *DB) ListPatternChangesByStatus(status string) ([]*PatternChange, error) {
	rows, err := db.Query(`
		SELECT id, tier, pattern, change_type, reason, status, created_at
		FROM pattern_changes WHERE status = ?
		ORDER BY created_at DESC
	`, status)
	if err != nil {
		return nil, fmt.Errorf("querying pattern changes: %w", err)
	}
	defer rows.Close()

	return scanPatternChanges(rows)
}

// ListPatternChangesByType returns pattern changes with a given type.
func (db *DB) ListPatternChangesByType(changeType string) ([]*PatternChange, error) {
	rows, err := db.Query(`
		SELECT id, tier, pattern, change_type, reason, status, created_at
		FROM pattern_changes WHERE change_type = ?
		ORDER BY created_at DESC
	`, changeType)
	if err != nil {
		return nil, fmt.Errorf("querying pattern changes by type: %w", err)
	}
	defer rows.Close()

	return scanPatternChanges(rows)
}

// ListAllPatternChanges returns all pattern changes.
func (db *DB) ListAllPatternChanges() ([]*PatternChange, error) {
	rows, err := db.Query(`
		SELECT id, tier, pattern, change_type, reason, status, created_at
		FROM pattern_changes
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("querying all pattern changes: %w", err)
	}
	defer rows.Close()

	return scanPatternChanges(rows)
}

// UpdatePatternChangeStatus updates the status of a pattern change.
func (db *DB) UpdatePatternChangeStatus(id int64, status string) error {
	result, err := db.Exec(`
		UPDATE pattern_changes SET status = ? WHERE id = ?
	`, status, id)
	if err != nil {
		return fmt.Errorf("updating pattern change status: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("getting rows affected: %w", err)
	}
	if rows == 0 {
		return ErrPatternChangeNotFound
	}

	return nil
}

// ApprovePatternChange approves a pattern change.
func (db *DB) ApprovePatternChange(id int64) error {
	return db.UpdatePatternChangeStatus(id, PatternChangeStatusApproved)
}

// RejectPatternChange rejects a pattern change.
func (db *DB) RejectPatternChange(id int64) error {
	return db.UpdatePatternChangeStatus(id, PatternChangeStatusRejected)
}

// DeletePatternChange deletes a pattern change.
func (db *DB) DeletePatternChange(id int64) error {
	result, err := db.Exec(`DELETE FROM pattern_changes WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting pattern change: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("getting rows affected: %w", err)
	}
	if rows == 0 {
		return ErrPatternChangeNotFound
	}

	return nil
}

// CountPendingPatternChanges counts pending pattern changes.
func (db *DB) CountPendingPatternChanges() (int, error) {
	var count int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM pattern_changes WHERE status = ?
	`, PatternChangeStatusPending).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting pending pattern changes: %w", err)
	}
	return count, nil
}

// scanPatternChange scans a single pattern change row.
func scanPatternChange(row *sql.Row) (*PatternChange, error) {
	pc := &PatternChange{}
	var createdAt string

	err := row.Scan(&pc.ID, &pc.Tier, &pc.Pattern, &pc.ChangeType, &pc.Reason, &pc.Status, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPatternChangeNotFound
		}
		return nil, fmt.Errorf("scanning pattern change: %w", err)
	}

	if createdAt != "" {
		pc.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	}

	return pc, nil
}

// scanPatternChanges scans multiple pattern change rows.
func scanPatternChanges(rows *sql.Rows) ([]*PatternChange, error) {
	var changes []*PatternChange

	for rows.Next() {
		pc := &PatternChange{}
		var createdAt string

		err := rows.Scan(&pc.ID, &pc.Tier, &pc.Pattern, &pc.ChangeType, &pc.Reason, &pc.Status, &createdAt)
		if err != nil {
			return nil, fmt.Errorf("scanning pattern change row: %w", err)
		}

		if createdAt != "" {
			pc.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		}

		changes = append(changes, pc)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating pattern changes: %w", err)
	}

	return changes, nil
}
