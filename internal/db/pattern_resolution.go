package db

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ErrPatternChangeDecided prevents a reviewed change from being replayed or
// rewritten by a concurrent reviewer.
var ErrPatternChangeDecided = errors.New("pattern change is no longer pending")

// resolvePatternChange commits the decision and its policy effect together.
// A failed application leaves the proposal pending; a success is durable and
// becomes visible to fresh CLI and daemon policy snapshots. Only the human
// review surface should call the approval entry point.
func (db *DB) resolvePatternChange(id int64, decision string) error {
	if decision != PatternChangeStatusApproved && decision != PatternChangeStatusRejected {
		return fmt.Errorf("invalid pattern decision %q", decision)
	}
	return db.Transaction(func(tx *sql.Tx) error {
		// Reserve the decision with a write before reading the proposal. This
		// serializes competing reviewers even across independent connections.
		result, err := tx.Exec(`UPDATE pattern_changes SET status = ? WHERE id = ? AND status = ?`,
			decision, id, PatternChangeStatusPending)
		if err != nil {
			return fmt.Errorf("claiming pattern decision: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			var exists int
			if err := tx.QueryRow(`SELECT 1 FROM pattern_changes WHERE id = ?`, id).Scan(&exists); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrPatternChangeNotFound
				}
				return err
			}
			return ErrPatternChangeDecided
		}
		// Malformed legacy proposals must still be rejectable.
		if decision == PatternChangeStatusRejected {
			return nil
		}

		var tier, pattern, changeType, reason string
		if err := tx.QueryRow(`SELECT tier, pattern, change_type, reason FROM pattern_changes WHERE id = ?`, id).
			Scan(&tier, &pattern, &changeType, &reason); err != nil {
			return err
		}
		tier = strings.ToLower(tier)
		if tier == "" || pattern == "" {
			return fmt.Errorf("pattern proposal must identify an exact tier and pattern; submit a new request with --tier")
		}

		switch changeType {
		case PatternChangeTypeRemove:
			// Removal is literal, not regex matching. Even a malformed custom
			// regex can be removed to repair a policy that now fails closed.
			// Already-absent custom rules satisfy a removal, making concurrent
			// proposals idempotent. This never disables a builtin rule.
			_, err := tx.Exec(`DELETE FROM custom_patterns WHERE lower(tier) = ? AND pattern = ?`, tier, pattern)
			if err != nil {
				return fmt.Errorf("removing custom pattern: %w", err)
			}
		case PatternChangeTypeAdd, PatternChangeTypeSuggest:
			switch tier {
			case "safe", "caution", "dangerous", "critical":
			default:
				return fmt.Errorf("invalid proposed pattern tier %q", tier)
			}
			if _, err := regexp.Compile("(?i)" + pattern); err != nil {
				return fmt.Errorf("invalid proposed pattern: %w", err)
			}
			// Canonicalize legacy mixed-case tiers without adding duplicates.
			var count int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM custom_patterns WHERE lower(tier) = ? AND pattern = ?`, tier, pattern).Scan(&count); err != nil {
				return err
			}
			if count == 0 {
				if _, err := tx.Exec(`INSERT INTO custom_patterns (tier, pattern, description, source, created_at) VALUES (?, ?, ?, ?, ?)`,
					tier, pattern, reason, "human", time.Now().UTC().Format(time.RFC3339)); err != nil {
					return fmt.Errorf("applying approved pattern: %w", err)
				}
			}
		default:
			return fmt.Errorf("unsupported pattern change type %q", changeType)
		}
		return nil
	})
}
