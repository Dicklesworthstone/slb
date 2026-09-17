package cli

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/output"
)

// queuePatternChange records a proposal, never an active rule. SAFE additions
// must take this path because an allowlist entry can override dangerous rules.
func queuePatternChange(pattern, tierName, changeType, reason string) error {
	if strings.TrimSpace(pattern) == "" {
		return fmt.Errorf("pattern is required")
	}
	tier := parseTier(tierName)
	if tierName != "" && tier == "" {
		return fmt.Errorf("invalid tier: %s", tierName)
	}
	if changeType != db.PatternChangeTypeRemove {
		if tier == "" {
			return fmt.Errorf("--tier is required")
		}
		if _, err := regexp.Compile("(?i)" + pattern); err != nil {
			return fmt.Errorf("invalid pattern: %w", err)
		}
	}

	conn, err := db.OpenAndMigrate(GetDB())
	if err != nil {
		return fmt.Errorf("opening project database for pattern review: %w", err)
	}
	defer conn.Close()

	message := "Pattern change recorded. Awaiting human review in TUI."
	if changeType == db.PatternChangeTypeRemove && tierName == "" {
		rows, err := conn.ListCustomPatterns()
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.Pattern != pattern {
				continue
			}
			candidate := core.RiskTier(strings.ToLower(row.Tier))
			if tier != "" && tier != candidate {
				return fmt.Errorf("pattern exists in multiple tiers; specify --tier")
			}
			tier = candidate
		}
		if tier == "" {
			// Preserve unscoped removal proposals for human review, but never
			// guess their authority. The approval transaction refuses an empty
			// tier; a precise replacement proposal is required before applying.
			message = "Removal proposal recorded without a matching custom rule. Submit a new request with --tier before approval."
		}
	}

	proposal := &db.PatternChange{
		Tier: string(tier), Pattern: pattern, ChangeType: changeType,
		Reason: reason, Status: db.PatternChangeStatusPending,
	}
	if err := conn.CreatePatternChange(proposal); err != nil {
		return fmt.Errorf("persisting pattern proposal: %w", err)
	}
	status := "pending"
	if changeType == db.PatternChangeTypeSuggest {
		status = "suggested"
	}
	out := output.New(output.Format(GetOutput()))
	return out.Write(map[string]any{
		"status": status, "review_status": proposal.Status,
		"id": proposal.ID, "request_id": proposal.ID,
		"pattern": pattern, "tier": proposal.Tier, "reason": reason,
		"message": message,
	})
}
