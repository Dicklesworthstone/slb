// Package core implements pattern matching for risk classification.
package core

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// Pattern represents a risk classification pattern.
type Pattern struct {
	// Tier is the risk tier this pattern matches.
	Tier RiskTier
	// Pattern is the regex pattern string.
	Pattern string
	// Compiled is the compiled regex.
	Compiled *regexp.Regexp
	// Description describes why this pattern is risky.
	Description string
	// Source indicates where this pattern came from.
	Source string // "builtin", "agent", "human", "suggested"
}

// MatchResult contains the result of pattern matching.
type MatchResult struct {
	// Tier is the matched risk tier.
	Tier RiskTier
	// MatchedPattern is the pattern that matched.
	MatchedPattern string
	// MinApprovals is the minimum approvals required.
	MinApprovals int
	// NeedsApproval indicates if this command needs approval.
	NeedsApproval bool
	// IsSafe indicates if this command is safe (skip review).
	IsSafe bool
	// ParseError indicates normalization/tokenization issues (conservative upgrade applied).
	ParseError bool
	// Segments lists matched segments for compound commands.
	MatchedSegments []SegmentMatch
}

// SegmentMatch describes a match within a compound command.
type SegmentMatch struct {
	Segment        string
	Tier           RiskTier
	MatchedPattern string
}

// PatternEngine handles pattern matching for risk classification.
type PatternEngine struct {
	mu sync.RWMutex
	// Patterns by tier (safe checked first, then critical, dangerous, caution)
	safe      []*Pattern
	critical  []*Pattern
	dangerous []*Pattern
	caution   []*Pattern
}

// NewPatternEngine creates a new pattern engine with default patterns.
func NewPatternEngine() *PatternEngine {
	engine := &PatternEngine{}
	engine.LoadDefaultPatterns()
	return engine
}

// LoadDefaultPatterns loads the default dangerous patterns.
func (e *PatternEngine) LoadDefaultPatterns() {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Safe patterns (skip review entirely)
	e.safe = compilePatterns(RiskTier(RiskSafe), []string{
		`^rm\s+.*\.log$`,
		`^rm\s+.*\.tmp$`,
		`^rm\s+.*\.bak$`,
		`^git\s+stash\s*$`,
		`^kubectl\s+delete\s+pod\s`,
		`^npm\s+cache\s+clean`,
	}, "builtin")

	// Critical patterns (2+ approvals)
	e.critical = compilePatterns(RiskTierCritical, []string{
		// rm -rf on system paths (not /tmp, not relative paths)
		`^rm\s+(-[rf]+\s+)+/(boot|dev|etc|home|lib|lib64|media|mnt|opt|proc|root|run|sbin|srv|sys|usr|var)`,
		`^rm\s+(-[rf]+\s+)+/($|\s)`, // rm -rf / (root)
		`^rm\s+(-[rf]+\s+)+/\*`,     // rm -rf /* (root wildcard)
		`^rm\s+(-[rf]+\s+)+~`,       // rm -rf ~
		// SQL data destruction
		`DROP\s+DATABASE`,
		`DROP\s+SCHEMA`,
		`TRUNCATE\s+TABLE`,
		`DELETE\s+FROM\s+[\w.` + "`" + `"\[\]]+\s*(;|$|--|/\*)`,
		// Infrastructure destruction - terraform destroy without -target is critical
		`^terraform\s+destroy\s*$`,             // terraform destroy with no args
		`^terraform\s+destroy\s+-auto-approve`, // terraform destroy -auto-approve
		`^terraform\s+destroy\s+[^-]`,          // terraform destroy <resource> (no flag)
		`^kubectl\s+delete\s+(node|nodes|namespace|namespaces|pv|persistentvolume|pvc|persistentvolumeclaim)\b`,
		`^helm\s+uninstall.*--all`,
		`^docker\s+system\s+prune\s+-a`,
		// Git force push - both --force and -f (but not --force-with-lease)
		`^git\s+push\s+.*--force($|\s)`,
		`^git\s+push\s+.*-f($|\s)`,
		// Cloud resource destruction
		`^aws\s+.*terminate-instances`,
		`^gcloud.*delete.*--quiet`,
		// Disk/filesystem destruction
		`\bdd\b.*of=/dev/`, // dd writing to device
		`^mkfs`,            // mkfs.* commands
		`^fdisk`,           // partition manipulation
		`^parted`,          // partition manipulation
		// System file permission changes
		`^chmod\s+.*/(etc|usr|var|boot|bin|sbin)`,
		`^chown\s+.*/(etc|usr|var|boot|bin|sbin)`,
	}, "builtin")

	// Dangerous patterns (1 approval)
	e.dangerous = compilePatterns(RiskTierDangerous, []string{
		`^rm\s+-rf`,
		`^rm\s+-r`,
		`^git\s+reset\s+--hard`,
		`^git\s+clean\s+-fd`,
		`^git\s+push.*--force-with-lease`,
		`^kubectl\s+delete`,
		`^helm\s+uninstall`,
		`^docker\s+rm`,
		`^docker\s+rmi`,
		`^terraform\s+destroy.*-target`,
		`^terraform\s+state\s+rm`,
		`DROP\s+TABLE`,
		`DELETE\s+FROM.*WHERE`,
		`^chmod\s+-R`,
		`^chown\s+-R`,
	}, "builtin")

	// Caution patterns (auto-approve after delay)
	e.caution = compilePatterns(RiskTierCaution, []string{
		`^rm\s+[^-]`,
		`^rm$`, // bare rm (used in xargs pipelines like: find | xargs rm)
		`^git\s+stash\s+drop`,
		`^git\s+branch\s+-[dD]`,
		`^npm\s+uninstall`,
		`^pip\s+uninstall`,
		`^cargo\s+remove`,
	}, "builtin")
}

func compilePatterns(tier RiskTier, patterns []string, source string) []*Pattern {
	result := make([]*Pattern, 0, len(patterns))
	for _, p := range patterns {
		compiled, err := regexp.Compile("(?i)" + p) // Case-insensitive
		if err != nil {
			// Built-in patterns must always be valid.
			if source == "builtin" {
				panic(fmt.Sprintf("invalid builtin pattern %q: %v", p, err))
			}
			continue // Skip invalid non-builtin patterns
		}
		result = append(result, &Pattern{
			Tier:     tier,
			Pattern:  p,
			Compiled: compiled,
			Source:   source,
		})
	}
	return result
}

// ClassifyCommand determines the risk tier for a command.
func (e *PatternEngine) ClassifyCommand(cmd, cwd string) *MatchResult {
	e.mu.RLock()
	defer e.mu.RUnlock()

	// Normalize the command
	normalized := NormalizeCommand(cmd)

	// Initialize result
	result := &MatchResult{
		NeedsApproval: false,
		IsSafe:        false,
		MinApprovals:  0,
		ParseError:    normalized.ParseError,
	}

	// For compound commands, check each segment
	if normalized.IsCompound && len(normalized.Segments) > 1 {
		return e.applyParseUpgrade(e.classifyCompoundCommand(normalized, cwd), normalized.ParseError)
	}

	// Get the command to check - use normalized primary if available
	var checkCmd string
	if normalized.Primary != "" {
		checkCmd = normalized.Primary
	} else if len(normalized.Segments) > 0 {
		checkCmd = normalized.Segments[0]
	} else {
		checkCmd = cmd
	}

	// Resolve paths if cwd provided
	if cwd != "" {
		checkCmd = ResolvePathsInCommand(checkCmd, cwd)
	}

	// Check against patterns in order of precedence
	// 1. Safe patterns → skip review entirely
	if match := e.matchPatterns(checkCmd, e.safe); match != nil {
		result.Tier = RiskTier(RiskSafe) // Special tier
		result.IsSafe = true
		result.MatchedPattern = match.Pattern
		return e.applyParseUpgrade(result, normalized.ParseError)
	}

	// 2. Critical patterns → 2+ approvals
	if match := e.matchPatterns(checkCmd, e.critical); match != nil {
		result.Tier = RiskTierCritical
		result.MatchedPattern = match.Pattern
		result.MinApprovals = tierApprovals(RiskTierCritical)
		result.NeedsApproval = true
		return e.applyParseUpgrade(result, normalized.ParseError)
	}

	// 3. Dangerous patterns → 1 approval
	if match := e.matchPatterns(checkCmd, e.dangerous); match != nil {
		result.Tier = RiskTierDangerous
		result.MatchedPattern = match.Pattern
		result.MinApprovals = tierApprovals(RiskTierDangerous)
		result.NeedsApproval = true
		return e.applyParseUpgrade(result, normalized.ParseError)
	}

	// 4. Caution patterns → auto-approve with notification
	if match := e.matchPatterns(checkCmd, e.caution); match != nil {
		result.Tier = RiskTierCaution
		result.MatchedPattern = match.Pattern
		result.MinApprovals = 0
		result.NeedsApproval = true // Still tracked, but auto-approved
		return e.applyParseUpgrade(result, normalized.ParseError)
	}

	// Fallback SQL detection on raw command (handles wrappers like psql -c "<SQL>")
	lowerRaw := strings.ToLower(cmd)
	if strings.Contains(lowerRaw, "delete from") {
		if !strings.Contains(lowerRaw, "where") {
			result.Tier = RiskTierCritical
			result.MinApprovals = tierApprovals(RiskTierCritical)
			result.NeedsApproval = true
			result.MatchedPattern = "fallback_sql_delete_no_where"
			return e.applyParseUpgrade(result, normalized.ParseError)
		}
		result.Tier = RiskTierDangerous
		result.MinApprovals = tierApprovals(RiskTierDangerous)
		result.NeedsApproval = true
		result.MatchedPattern = "fallback_sql_delete_with_where"
		return e.applyParseUpgrade(result, normalized.ParseError)
	}

	// No match → allowed without review
	return e.applyParseUpgrade(result, normalized.ParseError)
}

// classifyCompoundCommand handles compound commands.
// The highest risk segment determines the overall tier.
func (e *PatternEngine) classifyCompoundCommand(normalized *NormalizedCommand, cwd string) *MatchResult {
	result := &MatchResult{
		NeedsApproval:   false,
		IsSafe:          true, // Assume safe until proven otherwise
		MinApprovals:    0,
		MatchedSegments: []SegmentMatch{},
	}

	highestTier := RiskTier("")

	for _, segment := range normalized.Segments {
		// Resolve paths for this segment
		if cwd != "" {
			segment = ResolvePathsInCommand(segment, cwd)
		}

		// Check for xargs with a command - extract and classify the inner command
		if xargsCmd := ExtractXargsCommand(segment); xargsCmd != "" {
			// Classify the command that xargs will execute
			segment = xargsCmd
		}

		segmentMatch := SegmentMatch{Segment: segment}

		// Check tiers in the same precedence order as single-command classification:
		// SAFE → CRITICAL → DANGEROUS → CAUTION.
		if match := e.matchPatterns(segment, e.safe); match != nil {
			segmentMatch.Tier = RiskTier(RiskSafe)
			segmentMatch.MatchedPattern = match.Pattern
			if highestTier == "" {
				highestTier = RiskTier(RiskSafe)
			}
		} else if match := e.matchPatterns(segment, e.critical); match != nil {
			segmentMatch.Tier = RiskTierCritical
			segmentMatch.MatchedPattern = match.Pattern
			highestTier = RiskTierCritical
		} else if match := e.matchPatterns(segment, e.dangerous); match != nil {
			segmentMatch.Tier = RiskTierDangerous
			segmentMatch.MatchedPattern = match.Pattern
			if highestTier != RiskTierCritical {
				highestTier = RiskTierDangerous
			}
		} else if match := e.matchPatterns(segment, e.caution); match != nil {
			segmentMatch.Tier = RiskTierCaution
			segmentMatch.MatchedPattern = match.Pattern
			// Caution is higher risk than Safe (and no-match), so upgrade
			if highestTier == "" || highestTier == RiskTier(RiskSafe) {
				highestTier = RiskTierCaution
			}
		}

		if segmentMatch.MatchedPattern != "" {
			result.MatchedSegments = append(result.MatchedSegments, segmentMatch)
		}
	}

	// Set overall result based on highest tier
	switch highestTier {
	case RiskTierCritical:
		result.Tier = RiskTierCritical
		result.MinApprovals = 2
		result.NeedsApproval = true
		result.IsSafe = false
	case RiskTierDangerous:
		result.Tier = RiskTierDangerous
		result.MinApprovals = 1
		result.NeedsApproval = true
		result.IsSafe = false
	case RiskTierCaution:
		result.Tier = RiskTierCaution
		result.MinApprovals = tierApprovals(RiskTierCaution)
		result.NeedsApproval = true
		result.IsSafe = false
	case RiskTier(RiskSafe):
		result.Tier = RiskTier(RiskSafe)
		result.MinApprovals = 0
		result.NeedsApproval = false
		result.IsSafe = true
	}

	// Get the first matching pattern for the result
	for _, seg := range result.MatchedSegments {
		if seg.Tier == result.Tier {
			result.MatchedPattern = seg.MatchedPattern
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

// applyParseUpgrade enforces conservative behavior when normalization fails.
// It upgrades the tier by one step (safe→caution→dangerous→critical) or sets
// a default caution tier if no tier was determined.
func (e *PatternEngine) applyParseUpgrade(res *MatchResult, parseErr bool) *MatchResult {
	res.ParseError = parseErr
	if !parseErr {
		return res
	}

	// If no tier determined, default to caution with approval
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
	case RiskTierCritical:
		return RiskTierCritical
	case RiskTierDangerous:
		return RiskTierCritical
	case RiskTierCaution:
		return RiskTierDangerous
	case RiskTier(RiskSafe):
		return RiskTierCaution
	default:
		return RiskTierCaution
	}
}

// AddPattern adds a new pattern to the engine.
func (e *PatternEngine) AddPattern(tier RiskTier, pattern, description, source string) error {
	compiled, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	p := &Pattern{
		Tier:        tier,
		Pattern:     pattern,
		Compiled:    compiled,
		Description: description,
		Source:      source,
	}

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

// RemovePattern removes a pattern from the engine.
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

// ListPatterns returns all patterns for a tier.
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

// AllPatterns returns all patterns grouped by tier.
func (e *PatternEngine) AllPatterns() map[string][]*Pattern {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return map[string][]*Pattern{
		"safe":      e.safe,
		"critical":  e.critical,
		"dangerous": e.dangerous,
		"caution":   e.caution,
	}
}

// Global pattern engine instance
var defaultEngine = NewPatternEngine()

// GetDefaultEngine returns the global pattern engine.
func GetDefaultEngine() *PatternEngine {
	return defaultEngine
}

// Classify is a convenience function using the default engine.
func Classify(cmd, cwd string) *MatchResult {
	return defaultEngine.ClassifyCommand(cmd, cwd)
}

// TestPattern tests if a command matches any dangerous pattern.
// Returns true if the command needs approval.
func TestPattern(cmd string) bool {
	result := defaultEngine.ClassifyCommand(cmd, "")
	return result.NeedsApproval
}

// MatchesPattern checks if a command matches a specific pattern.
func MatchesPattern(cmd, pattern string) bool {
	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		return false
	}
	return re.MatchString(strings.TrimSpace(cmd))
}
