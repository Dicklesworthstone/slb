package core

import "fmt"

// ReplaceCustomPatterns publishes builtins plus exactly the supplied custom
// rules. It does not merge with the previous snapshot: removed allow rules and
// rules from another project must not survive a reload. Compile and validate
// the entire candidate before taking the publication lock. On error, callers
// must stop the operation; the previous snapshot is left unchanged.
//
// Compiled fields supplied by callers are ignored. The returned count is the
// number of unique custom rules in the new snapshot (excluding builtin copies).
func (e *PatternEngine) ReplaceCustomPatterns(patterns []Pattern) (int, error) {
	candidate := NewPatternEngine()
	seen := make(map[string]bool)
	for tier, list := range candidate.AllPatterns() {
		for _, p := range list {
			seen[tier+"\x00"+p.Pattern] = true
		}
	}

	loaded := 0
	for _, p := range patterns {
		switch p.Tier {
		case RiskTier(RiskSafe), RiskTierCaution, RiskTierDangerous, RiskTierCritical:
		default:
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

	// Keep the engine identity stable for existing users, and let concurrent
	// classifications see either the complete old snapshot or the complete new
	// one, never a mixture or a transient builtins-only policy.
	e.mu.Lock()
	e.safe = candidate.safe
	e.critical = candidate.critical
	e.dangerous = candidate.dangerous
	e.caution = candidate.caution
	e.mu.Unlock()
	return loaded, nil
}
