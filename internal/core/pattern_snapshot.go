package core

import (
	"fmt"
	"strings"

	"github.com/Dicklesworthstone/slb/internal/config"
)

// enginePolicy is immutable after publication alongside the compiled rules.
// A nil policy denotes the builtin policy of a zero-value engine.
type enginePolicy struct {
	tiers                 map[RiskTier]config.PatternTierConfig
	requireDifferentModel bool
}

// ReplaceCustomPatterns publishes defaults plus exactly the supplied custom
// rules. Removed rules and rules from another project cannot survive reload.
func (e *PatternEngine) ReplaceCustomPatterns(patterns []Pattern) (int, error) {
	return e.ReplacePolicy(config.DefaultConfig(), patterns)
}

// ReplacePolicy compiles the effective configuration and persisted custom
// rules before atomically publishing them. Configured arrays replace their
// corresponding default arrays; custom rules are then merged by (tier, regex).
// Invalid policy is an error, never a partially published or weakened policy.
func (e *PatternEngine) ReplacePolicy(cfg config.Config, patterns []Pattern) (int, error) {
	if err := config.Validate(cfg); err != nil {
		return 0, err
	}
	candidate := &PatternEngine{policy: &enginePolicy{
		tiers: map[RiskTier]config.PatternTierConfig{
			RiskTier(RiskSafe): cfg.Patterns.Safe, RiskTierCaution: cfg.Patterns.Caution,
			RiskTierDangerous: cfg.Patterns.Dangerous, RiskTierCritical: cfg.Patterns.Critical,
		},
		requireDifferentModel: cfg.General.RequireDifferentModel,
	}}
	seen := make(map[string]bool)
	defaults := config.DefaultConfig().Patterns
	builtins := make(map[string]bool)
	for tier, settings := range map[RiskTier]config.PatternTierConfig{
		RiskTier(RiskSafe): defaults.Safe, RiskTierCaution: defaults.Caution,
		RiskTierDangerous: defaults.Dangerous, RiskTierCritical: defaults.Critical,
	} {
		for _, pattern := range settings.Patterns {
			builtins[string(tier)+"\x00"+pattern] = true
		}
	}
	for _, tier := range []RiskTier{RiskTier(RiskSafe), RiskTierCaution, RiskTierDangerous, RiskTierCritical} {
		settings := candidate.policy.tiers[tier]
		if (tier == RiskTierDangerous || tier == RiskTierCritical) && settings.MinApprovals < 1 {
			return 0, fmt.Errorf("patterns.%s.min_approvals must be at least one", tier)
		}
		if settings.DynamicQuorum && (settings.DynamicQuorumFloor < 1 || settings.DynamicQuorumFloor > settings.MinApprovals) {
			return 0, fmt.Errorf("patterns.%s.dynamic_quorum_floor must be between one and min_approvals", tier)
		}
		for _, pattern := range settings.Patterns {
			// Activating previously ignored config must not revive the
			// last-token-only rm allow rules in older generated configs.
			if tier == RiskTier(RiskSafe) && (pattern == `^rm\s+.*\.log$` || pattern == `^rm\s+.*\.tmp$` || pattern == `^rm\s+.*\.bak$`) {
				return 0, fmt.Errorf("obsolete unsafe rm allow rule %q: replace it with the current default SAFE patterns", pattern)
			}
			key := string(tier) + "\x00" + pattern
			if seen[key] {
				continue
			}
			source := "config"
			if builtins[key] {
				source = "builtin"
			}
			if err := candidate.AddPattern(tier, pattern, "", source); err != nil {
				return 0, fmt.Errorf("invalid configured %s pattern %q: %w", tier, pattern, err)
			}
			seen[key] = true
		}
		// Do not retain caller-owned slices in immutable policy state.
		settings.Patterns = nil
		candidate.policy.tiers[tier] = settings
	}
	loaded := 0
	for _, p := range patterns {
		p.Tier = RiskTier(strings.ToLower(string(p.Tier)))
		if _, ok := candidate.policy.tiers[p.Tier]; !ok {
			return 0, fmt.Errorf("invalid persisted pattern tier %q", p.Tier)
		}
		key := string(p.Tier) + "\x00" + p.Pattern
		if seen[key] {
			continue
		}
		if err := candidate.AddPattern(p.Tier, p.Pattern, p.Description, p.Source); err != nil {
			return 0, fmt.Errorf("invalid persisted %s pattern %q: %w", p.Tier, p.Pattern, err)
		}
		seen[key] = true
		loaded++
	}
	e.mu.Lock()
	e.safe, e.caution = candidate.safe, candidate.caution
	e.dangerous, e.critical = candidate.dangerous, candidate.critical
	e.policy = candidate.policy
	e.mu.Unlock()
	return loaded, nil
}

// RequiredApprovals returns the current quorum for a risk tier. A negative
// reviewer count is advisory and returns the full configured quorum. Dynamic
// quorum only reduces that count when an actual project-local pool is known.
func (e *PatternEngine) RequiredApprovals(tier RiskTier, availableReviewers int) int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.requiredApprovalsLocked(tier, availableReviewers)
}

func (e *PatternEngine) requiredApprovalsLocked(tier RiskTier, availableReviewers int) int {
	if tier == RiskTier(RiskSafe) || tier == "" {
		return 0
	}
	minimum := tierApprovals(tier)
	if e.policy != nil {
		if settings, ok := e.policy.tiers[tier]; ok {
			minimum = settings.MinApprovals
			if settings.DynamicQuorum && availableReviewers >= 0 {
				minimum = min(minimum, max(settings.DynamicQuorumFloor, availableReviewers))
			}
		}
		// A different-model requirement cannot be satisfied by zero reviews.
		if e.policy.requireDifferentModel {
			minimum = max(1, minimum)
		}
	}
	return minimum
}

// RequiresDifferentModel reports the effective policy, rather than forcing
// every critical request to require a different model regardless of config.
func (e *PatternEngine) RequiresDifferentModel() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.policy != nil && e.policy.requireDifferentModel
}

// applyPolicyLocked runs after classification, including compound commands,
// SQL fallback and parse-error escalation, while the same snapshot is locked.
func (e *PatternEngine) applyPolicyLocked(result *MatchResult) {
	if result != nil && result.NeedsApproval {
		result.MinApprovals = e.requiredApprovalsLocked(result.Tier, -1)
	}
}
