package core

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

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
	engine, _, err := loadCommandPolicy(reader, opts)
	return engine, err
}

// loadCommandPolicy returns the configuration used to compile this exact
// engine, so timers cannot mix approval rules and deadlines from two loads.
func loadCommandPolicy(reader PolicyReader, opts config.LoadOptions) (*PatternEngine, config.Config, error) {
	if opts.ConfigPath != "" {
		if info, err := os.Stat(opts.ConfigPath); err != nil {
			return nil, config.Config{}, fmt.Errorf("reading explicit policy config: %w", err)
		} else if !info.Mode().IsRegular() {
			return nil, config.Config{}, errors.New("explicit policy config is not a regular file")
		}
	}
	cfg, err := config.Load(opts)
	if err != nil {
		return nil, cfg, fmt.Errorf("loading command policy: %w", err)
	}
	var patterns []Pattern
	if reader != nil {
		if patterns, err = ReadCustomPatterns(reader); err != nil {
			return nil, cfg, err
		}
	}
	engine, err := CompileCommandPolicy(cfg, patterns, opts.ConfigPath)
	if err != nil {
		return nil, cfg, err
	}
	return engine, cfg, nil
}

// ReadCustomPatterns returns the persisted custom rules in stored order,
// uncompiled. Only Tier, Pattern, Description and Source are set.
func ReadCustomPatterns(reader PolicyReader) ([]Pattern, error) {
	rows, err := reader.Query(`SELECT tier, pattern, COALESCE(description, ''), COALESCE(source, '') FROM custom_patterns ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("reading custom command policy: %w", err)
	}
	defer rows.Close()
	var patterns []Pattern
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
	return patterns, nil
}

// CompileCommandPolicy compiles a loaded configuration together with the
// custom rules from ReadCustomPatterns into a new engine. It never retains
// or modifies custom.
func CompileCommandPolicy(cfg config.Config, custom []Pattern, configPath string) (*PatternEngine, error) {
	engine := &PatternEngine{}
	if _, err := engine.ReplacePolicy(cfg, custom, configPath); err != nil {
		return nil, err
	}
	return engine, nil
}

// CountPolicyReviewers counts distinct active agent labels only in the
// request's project. The requester and duplicate agent sessions do not count.
// These local labels do not establish independent reviewer identities.
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
	// A legacy relative/empty cwd is not bound to the context reviewed at
	// admission. Require a new request; never fill it from this executor.
	if err := validateCommandCWD(request.Command.Cwd); err != nil {
		return err
	}
	engine, cfg, err := loadCommandPolicy(reader, config.LoadOptions{ProjectDir: request.ProjectPath, ConfigPath: configPath})
	if err != nil {
		return err
	}
	if err := checkApprovalPolicy(request, cfg.General.ApprovalPolicy); err != nil {
		return err
	}
	// Blocking a requester revokes permission to execute their outstanding
	// approvals too. Check the same freshly loaded policy as the classifier,
	// before its SAFE shortcut or quorum calculation. The final atomic claim
	// repeats this guard, so a previously successful advisory check is not a
	// permit to ignore a later block. Stored signatures remain unchanged.
	for _, blocked := range cfg.Agents.Blocked {
		if strings.EqualFold(blocked, request.RequestorAgent) {
			return fmt.Errorf("%w: requester %q cannot execute an outstanding approval", ErrAgentBlocked, request.RequestorAgent)
		}
	}
	if err := checkReviewConflictPolicy(reader, request, cfg.General.ConflictResolution); err != nil {
		return err
	}
	available, err := CountPolicyReviewers(reader, request.ProjectPath, request.RequestorSessionID, request.RequestorAgent)
	if err != nil {
		return err
	}
	return engine.checkRequestPolicy(request, available)
}

// A first-wins decision can contain a later rejection while still satisfying
// quorum. Changing to a policy that blocks on that rejection revokes execution,
// even when the command now matches a SAFE rule. Signed review evidence remains
// unchanged and is independently validated by the DB claim.
func checkReviewConflictPolicy(reader PolicyReader, request *db.Request, resolution string) error {
	rows, err := reader.Query(`SELECT decision FROM reviews WHERE request_id = ? ORDER BY rowid`, request.ID)
	if err != nil {
		return fmt.Errorf("reading review conflict evidence: %w", err)
	}
	defer rows.Close()
	var first string
	hasRejection := false
	for rows.Next() {
		var decision string
		if err := rows.Scan(&decision); err != nil {
			return fmt.Errorf("reading review conflict decision: %w", err)
		}
		if first == "" {
			first = decision
		}
		hasRejection = hasRejection || decision == string(db.DecisionReject)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if hasRejection && (resolution != string(ConflictFirstWins) || first != string(db.DecisionApprove)) {
		return fmt.Errorf("%w: current conflict policy %q does not permit these rejection votes", ErrApprovalPolicyChanged, resolution)
	}
	return nil
}

// A change in assurance requires a new request and new reviews. In particular,
// switching back to the default must revoke outstanding same-host workflows,
// even if the command now happens to match a SAFE rule.
func checkApprovalPolicy(request *db.Request, current string) error {
	stored := request.ApprovalPolicy.Normalized()
	if !stored.Valid() || stored != db.ApprovalPolicy(current) {
		return fmt.Errorf("%w: request approval policy is %q, current policy is %q; submit a new request", ErrApprovalPolicyChanged, stored, current)
	}
	return nil
}

func (e *PatternEngine) checkRequestPolicy(request *db.Request, available int) error {
	classification := e.ClassifyCommandSpec(request.Command)
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
