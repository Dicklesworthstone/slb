// Package core implements pattern matching for risk classification.
package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
)

// Pattern represents a risk classification pattern.
type Pattern struct {
	Tier        RiskTier
	Pattern     string
	Compiled    *regexp.Regexp
	Description string
	Source      string // "builtin", "config", "agent", "human", "suggested"
}

// MatchResult contains the result of pattern matching.
type MatchResult struct {
	Tier           RiskTier
	MatchedPattern string
	MinApprovals   int
	NeedsApproval  bool
	IsSafe         bool
	ParseError     bool
	// Opaque marks executed code that could not be determined statically
	// (see NormalizedCommand.Opaque); such commands are at least CAUTION.
	Opaque              bool
	MatchedSegments     []SegmentMatch
	HasUnmatchedSegment bool
}

// SegmentMatch describes a match within a compound command.
type SegmentMatch struct {
	Segment        string
	Tier           RiskTier
	MatchedPattern string
}

// PatternEngine publishes compiled patterns and approval policy together.
type PatternEngine struct {
	mu        sync.RWMutex
	safe      []*Pattern
	critical  []*Pattern
	dangerous []*Pattern
	caution   []*Pattern
	policy    *enginePolicy
}

func NewPatternEngine() *PatternEngine {
	engine := &PatternEngine{}
	engine.LoadDefaultPatterns()
	return engine
}

// LoadDefaultPatterns uses the same defaults as configuration loading.
func (e *PatternEngine) LoadDefaultPatterns() {
	if _, err := e.ReplacePolicy(config.DefaultConfig(), nil); err != nil {
		panic(fmt.Sprintf("invalid builtin policy: %v", err))
	}
}

func compilePatterns(tier RiskTier, patterns []string, source string) []*Pattern {
	result := make([]*Pattern, 0, len(patterns))
	for _, p := range patterns {
		compiled, err := regexp.Compile("(?i)" + p)
		if err != nil {
			if source == "builtin" {
				panic(fmt.Sprintf("invalid builtin pattern %q: %v", p, err))
			}
			continue
		}
		result = append(result, &Pattern{Tier: tier, Pattern: p, Compiled: compiled, Source: source})
	}
	return result
}

// ClassifyCommand determines the risk tier and configured approval count.
func (e *PatternEngine) ClassifyCommand(cmd, cwd string) (result *MatchResult) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	defer func() { e.applyPolicyLocked(result) }()

	normalized := NormalizeCommand(cmd)
	result = &MatchResult{ParseError: normalized.ParseError}
	if normalized.IsCompound && len(normalized.Segments) > 1 {
		return e.finalizeClassification(e.classifyCompoundCommand(normalized, cwd), normalized)
	}

	var checkCmd string
	if normalized.Primary != "" {
		checkCmd = normalized.Primary
	} else if len(normalized.Segments) > 0 {
		checkCmd = normalized.Segments[0]
	} else {
		checkCmd = cmd
	}
	if cwd != "" {
		checkCmd = ResolvePathsInCommand(checkCmd, cwd)
	}
	if match := e.matchPatterns(checkCmd, e.safe); match != nil {
		result.Tier, result.IsSafe, result.MatchedPattern = RiskTier(RiskSafe), true, match.Pattern
		return e.finalizeClassification(result, normalized)
	}
	if match := e.matchPatterns(checkCmd, e.critical); match != nil {
		result.Tier, result.NeedsApproval, result.MatchedPattern = RiskTierCritical, true, match.Pattern
		return e.finalizeClassification(result, normalized)
	}
	if match := e.matchPatterns(checkCmd, e.dangerous); match != nil {
		result.Tier, result.NeedsApproval, result.MatchedPattern = RiskTierDangerous, true, match.Pattern
		return e.finalizeClassification(result, normalized)
	}
	if match := e.matchPatterns(checkCmd, e.caution); match != nil {
		result.Tier, result.NeedsApproval, result.MatchedPattern = RiskTierCaution, true, match.Pattern
		return e.finalizeClassification(result, normalized)
	}
	lowerRaw := strings.ToLower(cmd)
	if strings.Contains(lowerRaw, "delete from") {
		result.Tier = RiskTierDangerous
		result.NeedsApproval = true
		result.MatchedPattern = "fallback_sql_delete_with_where"
		if !strings.Contains(lowerRaw, "where") {
			result.Tier = RiskTierCritical
			result.MatchedPattern = "fallback_sql_delete_no_where"
		}
	}
	return e.finalizeClassification(result, normalized)
}

// classifyCompoundCommand keeps the highest risk segment, never allowing one
// safe segment to hide an unrecognized segment from explicit request creation.
func (e *PatternEngine) classifyCompoundCommand(normalized *NormalizedCommand, cwd string) *MatchResult {
	result := &MatchResult{MatchedSegments: []SegmentMatch{}}
	highestTier := RiskTier("")
	for _, segment := range normalized.Segments {
		if cwd != "" {
			segment = ResolvePathsInCommand(segment, cwd)
		}
		if xargsCmd := ExtractXargsCommand(segment); xargsCmd != "" {
			segment = xargsCmd
		}
		segmentMatch := SegmentMatch{Segment: segment}
		if match := e.matchPatterns(segment, e.safe); match != nil {
			segmentMatch.Tier, segmentMatch.MatchedPattern = RiskTier(RiskSafe), match.Pattern
			if highestTier == "" {
				highestTier = RiskTier(RiskSafe)
			}
		} else if match := e.matchPatterns(segment, e.critical); match != nil {
			segmentMatch.Tier, segmentMatch.MatchedPattern = RiskTierCritical, match.Pattern
			highestTier = RiskTierCritical
		} else if match := e.matchPatterns(segment, e.dangerous); match != nil {
			segmentMatch.Tier, segmentMatch.MatchedPattern = RiskTierDangerous, match.Pattern
			if highestTier != RiskTierCritical {
				highestTier = RiskTierDangerous
			}
		} else if match := e.matchPatterns(segment, e.caution); match != nil {
			segmentMatch.Tier, segmentMatch.MatchedPattern = RiskTierCaution, match.Pattern
			if highestTier == "" || highestTier == RiskTier(RiskSafe) {
				highestTier = RiskTierCaution
			}
		} else {
			lowerRaw := strings.ToLower(segment)
			if strings.Contains(lowerRaw, "delete from") {
				if !strings.Contains(lowerRaw, "where") {
					segmentMatch.Tier, segmentMatch.MatchedPattern = RiskTierCritical, "fallback_sql_delete_no_where"
					highestTier = RiskTierCritical
				} else {
					segmentMatch.Tier, segmentMatch.MatchedPattern = RiskTierDangerous, "fallback_sql_delete_with_where"
					if highestTier != RiskTierCritical {
						highestTier = RiskTierDangerous
					}
				}
			}
		}
		if segmentMatch.MatchedPattern != "" {
			result.MatchedSegments = append(result.MatchedSegments, segmentMatch)
		} else {
			result.HasUnmatchedSegment = true
		}
	}
	result.Tier = highestTier
	switch highestTier {
	case RiskTierCritical, RiskTierDangerous, RiskTierCaution:
		result.NeedsApproval = true
		result.MinApprovals = e.requiredApprovalsLocked(highestTier, -1)
	case RiskTier(RiskSafe):
		result.IsSafe = true
	}
	for _, segment := range result.MatchedSegments {
		if segment.Tier == result.Tier {
			result.MatchedPattern = segment.MatchedPattern
			break
		}
	}
	return result
}

func (e *PatternEngine) matchPatterns(cmd string, patterns []*Pattern) *Pattern {
	for _, p := range patterns {
		if p.Compiled.MatchString(cmd) {
			return p
		}
	}
	return nil
}

// finalizeClassification applies the parse-error upgrade and then the opaque
// floor: code whose content cannot be determined statically is never "no
// pattern" or SAFE.
func (e *PatternEngine) finalizeClassification(res *MatchResult, normalized *NormalizedCommand) *MatchResult {
	res = e.applyParseUpgrade(res, normalized.ParseError)
	res.Opaque = normalized.Opaque
	if !normalized.Opaque || (res.Tier != "" && res.Tier != RiskTier(RiskSafe)) {
		return res
	}
	res.Tier = RiskTierCaution
	res.MinApprovals = tierApprovals(res.Tier)
	res.NeedsApproval = true
	res.IsSafe = false
	res.MatchedPattern = "unresolved_execution"
	return res
}

func (e *PatternEngine) applyParseUpgrade(res *MatchResult, parseErr bool) *MatchResult {
	res.ParseError = parseErr
	if !parseErr {
		return res
	}
	if res.Tier == "" {
		res.Tier = RiskTierCaution
		res.MinApprovals = tierApprovals(res.Tier)
		res.NeedsApproval = true
		res.IsSafe = false
		if res.MatchedPattern == "" {
			res.MatchedPattern = "parse_error"
		}
		return res
	}
	upgraded := upgradeTier(res.Tier)
	if upgraded != res.Tier {
		res.Tier = upgraded
		res.MinApprovals = tierApprovals(res.Tier)
		res.NeedsApproval = res.Tier != RiskTier(RiskSafe)
		res.IsSafe = res.Tier == RiskTier(RiskSafe)
		if res.MatchedPattern == "" {
			res.MatchedPattern = "parse_error"
		}
	}
	return res
}

func tierApprovals(t RiskTier) int {
	switch t {
	case RiskTierCritical:
		return 2
	case RiskTierDangerous:
		return 1
	default:
		return 0
	}
}

func upgradeTier(t RiskTier) RiskTier {
	switch t {
	case RiskTierCritical, RiskTierDangerous:
		return RiskTierCritical
	case RiskTierCaution:
		return RiskTierDangerous
	default:
		return RiskTierCaution
	}
}

func (e *PatternEngine) AddPattern(tier RiskTier, pattern, description, source string) error {
	switch tier {
	case RiskTier(RiskSafe), RiskTierCaution, RiskTierDangerous, RiskTierCritical:
	default:
		return fmt.Errorf("invalid pattern tier %q", tier)
	}
	compiled, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	p := &Pattern{Tier: tier, Pattern: pattern, Compiled: compiled, Description: description, Source: source}
	switch tier {
	case RiskTierCritical:
		e.critical = append(e.critical, p)
	case RiskTierDangerous:
		e.dangerous = append(e.dangerous, p)
	case RiskTierCaution:
		e.caution = append(e.caution, p)
	default:
		e.safe = append(e.safe, p)
	}
	return nil
}

func (e *PatternEngine) RemovePattern(tier RiskTier, pattern string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	var list *[]*Pattern
	switch tier {
	case RiskTierCritical:
		list = &e.critical
	case RiskTierDangerous:
		list = &e.dangerous
	case RiskTierCaution:
		list = &e.caution
	default:
		list = &e.safe
	}
	for i, p := range *list {
		if p.Pattern == pattern {
			*list = append((*list)[:i], (*list)[i+1:]...)
			return true
		}
	}
	return false
}

func (e *PatternEngine) ListPatterns(tier RiskTier) []*Pattern {
	e.mu.RLock()
	defer e.mu.RUnlock()
	switch tier {
	case RiskTierCritical:
		return e.critical
	case RiskTierDangerous:
		return e.dangerous
	case RiskTierCaution:
		return e.caution
	default:
		return e.safe
	}
}

func (e *PatternEngine) AllPatterns() map[string][]*Pattern {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return map[string][]*Pattern{"safe": e.safe, "critical": e.critical, "dangerous": e.dangerous, "caution": e.caution}
}

var defaultEngine = NewPatternEngine()

func GetDefaultEngine() *PatternEngine      { return defaultEngine }
func Classify(cmd, cwd string) *MatchResult { return defaultEngine.ClassifyCommand(cmd, cwd) }
func TestPattern(cmd string) bool           { return defaultEngine.ClassifyCommand(cmd, "").NeedsApproval }

func MatchesPattern(cmd, pattern string) bool {
	re, err := regexp.Compile("(?i)" + pattern)
	return err == nil && re.MatchString(strings.TrimSpace(cmd))
}

type PatternExport struct {
	Version     string                `json:"version"`
	GeneratedAt time.Time             `json:"generated_at"`
	SHA256      string                `json:"sha256"`
	Tiers       map[string]TierExport `json:"tiers"`
	Metadata    PatternExportMetadata `json:"metadata"`
}

type TierExport struct {
	Description  string           `json:"description"`
	MinApprovals int              `json:"min_approvals"`
	Patterns     []PatternDetails `json:"patterns"`
}

type PatternDetails struct {
	Pattern     string `json:"pattern"`
	Description string `json:"description,omitempty"`
	Source      string `json:"source"`
}

type PatternExportMetadata struct {
	PatternCount int            `json:"pattern_count"`
	TierCounts   map[string]int `json:"tier_counts"`
}

func (e *PatternEngine) Export() *PatternExport {
	e.mu.RLock()
	defer e.mu.RUnlock()
	export := &PatternExport{Version: "1.0.0", GeneratedAt: time.Now().UTC(),
		Tiers: make(map[string]TierExport), Metadata: PatternExportMetadata{TierCounts: make(map[string]int)}}
	tiers := []struct {
		name        string
		patterns    []*Pattern
		description string
	}{
		{"safe", e.safe, "Commands that skip review entirely - known safe operations"},
		{"caution", e.caution, "Commands requiring attention but auto-approvable"},
		{"dangerous", e.dangerous, "Commands requiring human/agent approval"},
		{"critical", e.critical, "Commands requiring independent approvals - highest risk"},
	}
	for _, tier := range tiers {
		patterns := make([]PatternDetails, 0, len(tier.patterns))
		for _, p := range tier.patterns {
			patterns = append(patterns, PatternDetails{Pattern: p.Pattern, Description: p.Description, Source: p.Source})
		}
		sort.Slice(patterns, func(i, j int) bool { return patterns[i].Pattern < patterns[j].Pattern })
		export.Tiers[tier.name] = TierExport{Description: tier.description,
			MinApprovals: e.requiredApprovalsLocked(RiskTier(tier.name), -1), Patterns: patterns}
		export.Metadata.TierCounts[tier.name] = len(patterns)
		export.Metadata.PatternCount += len(patterns)
	}
	export.SHA256 = e.computeHashLocked()
	return export
}

func (e *PatternEngine) ComputeHash() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.computeHashLocked()
}

func (e *PatternEngine) computeHashLocked() string {
	var entries []string
	tiers := []struct {
		name     string
		patterns []*Pattern
	}{
		{"safe", e.safe}, {"caution", e.caution}, {"dangerous", e.dangerous}, {"critical", e.critical},
	}
	for _, tier := range tiers {
		for _, p := range tier.patterns {
			entries = append(entries, fmt.Sprintf("%s:%s", tier.name, p.Pattern))
		}
		if e.policy != nil {
			p := e.policy.tiers[RiskTier(tier.name)]
			entries = append(entries, fmt.Sprintf("policy:%s:%d:%t:%d:%d", tier.name,
				p.MinApprovals, p.DynamicQuorum, p.DynamicQuorumFloor, p.AutoApproveDelaySeconds))
		}
	}
	if e.policy != nil {
		entries = append(entries, fmt.Sprintf("different_model:%t", e.policy.requireDifferentModel))
	}
	sort.Strings(entries)
	h := sha256.New()
	for _, entry := range entries {
		h.Write([]byte(entry))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (e *PatternEngine) ExportJSON() (string, error) {
	data, err := json.MarshalIndent(e.Export(), "", "  ")
	return string(data), err
}

// ExportClaudeHook retains offline regex classification. The daemon remains
// authoritative for normalization, project policy and single-use approvals.
func (e *PatternEngine) ExportClaudeHook() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var sb strings.Builder
	sb.WriteString("# Auto-generated by: slb patterns export --format=claude-hook\n")
	sb.WriteString(fmt.Sprintf("# Generated: %s\n# SHA256: %s\n", time.Now().UTC().Format(time.RFC3339), e.computeHashLocked()))
	sb.WriteString("# DO NOT EDIT - regenerate with: slb patterns export --format=claude-hook\n\nimport re\nfrom typing import Tuple, Optional\n\n")
	tiers := []struct {
		name     string
		patterns []*Pattern
		varName  string
	}{
		{"safe", e.safe, "SAFE_PATTERNS"}, {"caution", e.caution, "CAUTION_PATTERNS"},
		{"dangerous", e.dangerous, "DANGEROUS_PATTERNS"}, {"critical", e.critical, "CRITICAL_PATTERNS"},
	}
	for _, tier := range tiers {
		sb.WriteString(fmt.Sprintf("# %s tier: %d patterns\n%s = [\n", strings.ToUpper(tier.name), len(tier.patterns), tier.varName))
		patterns := append([]*Pattern(nil), tier.patterns...)
		sort.Slice(patterns, func(i, j int) bool { return patterns[i].Pattern < patterns[j].Pattern })
		for _, p := range patterns {
			if strings.ContainsAny(p.Pattern, "'\n\r") {
				// JSON strings are valid Python literals after ASCII-escaping
				// and do not turn an apostrophe/newline into generated code.
				data, _ := json.Marshal(p.Pattern)
				sb.WriteString(fmt.Sprintf("    re.compile(%s, re.IGNORECASE),\n", data))
			} else {
				sb.WriteString(fmt.Sprintf("    re.compile(r'%s', re.IGNORECASE),\n", p.Pattern))
			}
		}
		sb.WriteString("]\n\n")
	}
	sb.WriteString("MIN_APPROVALS = {\n")
	for _, tier := range tiers {
		sb.WriteString(fmt.Sprintf("    '%s': %d,\n", tier.name, e.requiredApprovalsLocked(RiskTier(tier.name), -1)))
	}
	sb.WriteString("}\n\n")
	sb.WriteString(`def classify(command: str) -> Tuple[str, int]:
    """Classify against the exported policy; return (tier, min_approvals)."""
    command = command.strip()
    for tier, patterns in (('safe', SAFE_PATTERNS), ('critical', CRITICAL_PATTERNS),
                           ('dangerous', DANGEROUS_PATTERNS), ('caution', CAUTION_PATTERNS)):
        for pattern in patterns:
            if pattern.search(command):
                return (tier, MIN_APPROVALS[tier])
    return ('unknown', 0)


def needs_approval(command: str) -> bool:
    """Return True if the command needs SLB approval."""
    tier, _ = classify(command)
    return tier in ('dangerous', 'critical', 'caution')


def is_blocked(command: str) -> Tuple[bool, Optional[str]]:
    """Check whether the command should be blocked pending approval."""
    tier, approvals = classify(command)
    if tier == 'safe' or tier == 'unknown':
        return (False, None)
    if tier == 'critical':
        return (True, f"CRITICAL: Requires {approvals} approvals. Use 'slb request' to submit.")
    if tier == 'dangerous':
        return (True, f"DANGEROUS: Requires {approvals} approval. Use 'slb request' to submit.")
    if tier == 'caution':
        return (True, "CAUTION: Command logged for review. Use 'slb request' to submit.")
    return (False, None)
`)
	return sb.String()
}
