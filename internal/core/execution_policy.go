package core

import (
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/db"
)

var ErrApprovalPolicyChanged = errors.New("current policy requires additional approval")

// PolicyReader is implemented by both the project DB and its transaction.
// Reading through the transaction avoids a second connection/deadlock while
// the execution claim holds SQLite's writer reservation.
type PolicyReader interface {
	Query(string, ...any) (*sql.Rows, error)
}

// LoadCommandPolicy reads fresh configuration and persisted custom rules.
// A nil reader is valid only for classification before project initialization.
func LoadCommandPolicy(reader PolicyReader, opts config.LoadOptions) (*PatternEngine, error) {
	if opts.ConfigPath != "" {
		if info, err := os.Stat(opts.ConfigPath); err != nil {
			return nil, fmt.Errorf("reading explicit policy config: %w", err)
		} else if !info.Mode().IsRegular() {
			return nil, errors.New("explicit policy config is not a regular file")
		}
	}
	cfg, err := config.Load(opts)
	if err != nil {
		return nil, fmt.Errorf("loading command policy: %w", err)
	}
	var patterns []Pattern
	if reader != nil {
		rows, err := reader.Query(`SELECT tier, pattern, COALESCE(description, ''), COALESCE(source, '') FROM custom_patterns ORDER BY id`)
		if err != nil {
			return nil, fmt.Errorf("reading custom command policy: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var p Pattern
			if err := rows.Scan(&p.Tier, &p.Pattern, &p.Description, &p.Source); err != nil {
				return nil, fmt.Errorf("reading custom command policy row: %w", err)
			}
			patterns = append(patterns, p)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("reading custom command policy: %w", err)
		}
	}
	engine := &PatternEngine{}
	if _, err := engine.ReplacePolicy(cfg, patterns, opts.ConfigPath); err != nil {
		return nil, err
	}
	return engine, nil
}

// CountPolicyReviewers counts independent, active identities only in the
// request's project. The requester and duplicate agent sessions do not count.
func CountPolicyReviewers(reader PolicyReader, project, sessionID, agent string) (int, error) {
	rows, err := reader.Query(`SELECT COUNT(DISTINCT agent_name) FROM sessions
		WHERE project_path = ? AND ended_at IS NULL AND id <> ? AND agent_name <> ? AND agent_name <> ''`,
		project, sessionID, agent)
	if err != nil {
		return 0, fmt.Errorf("counting project reviewers: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, err
		}
		return 0, errors.New("reviewer count query returned no row")
	}
	var count int
	if err := rows.Scan(&count); err != nil {
		return 0, err
	}
	return count, rows.Err()
}

// CheckExecutionPolicy checks current policy without weakening any requirement
// already stored on the request. It does not consume or replace signed reviews.
func CheckExecutionPolicy(reader PolicyReader, request *db.Request, configPath string) error {
	if request == nil {
		return errors.New("request is required for policy check")
	}
	engine, err := LoadCommandPolicy(reader, config.LoadOptions{ProjectDir: request.ProjectPath, ConfigPath: configPath})
	if err != nil {
		return err
	}
	available, err := CountPolicyReviewers(reader, request.ProjectPath, request.RequestorSessionID, request.RequestorAgent)
	if err != nil {
		return err
	}
	return engine.checkRequestPolicy(request, available)
}

func (e *PatternEngine) checkRequestPolicy(request *db.Request, available int) error {
	classification := e.ClassifyCommand(request.Command.Raw, request.Command.Cwd)
	tier := classification.Tier
	if !classification.NeedsApproval {
		if classification.IsSafe && !classification.HasUnmatchedSegment {
			return nil // Stored review/TTL requirements are still verified by DB.
		}
		// A policy cannot inspect opaque scripts. Retain their reviewed tier
		// and enforce its current quorum; never reinterpret no-match as SAFE.
		// New explicit requests are escalated by RequestCreator.
		tier = request.RiskTier
	}
	if tierHigher(tier, request.RiskTier) {
		return fmt.Errorf("%w: approved as %s, now %s", ErrTierEscalated, request.RiskTier, tier)
	}
	minimum := e.RequiredApprovals(tier, available)
	if request.MinApprovals < minimum {
		return fmt.Errorf("%w: approved for %d reviews, now requires %d; submit a new request", ErrApprovalPolicyChanged, request.MinApprovals, minimum)
	}
	if e.RequiresDifferentModel() && !request.RequireDifferentModel {
		return fmt.Errorf("%w: different-model review is now required; submit a new request", ErrApprovalPolicyChanged)
	}
	return nil
}

// ExecutionPolicyGuard repeats the check after preflight/capture while custom
// rules, sessions and approval evidence are protected by the claim transaction.
// Config files are freshly read here, but are not SQLite-transactional files.
func ExecutionPolicyGuard(configPath string) db.ExecutionPolicyCheck {
	return func(tx *sql.Tx, request *db.Request) error {
		return CheckExecutionPolicy(tx, request, configPath)
	}
}
