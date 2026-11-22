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
	if cwd != "" || ExtractCommandName(checkCmd) == "rm" {
		checkCmd = ResolvePathsInCommand(checkCmd, cwd)
	}
	if tier, pattern := e.classifySegment(checkCmd); pattern != "" {
		result.Tier, result.MatchedPattern = tier, pattern
		result.IsSafe = tier == RiskTier(RiskSafe)
		result.NeedsApproval = !result.IsSafe
		return e.finalizeClassification(result, normalized)
	}
	if tier, pattern := sqlDeleteFallback(cmd); pattern != "" {
		result.Tier, result.MatchedPattern = tier, pattern
		result.NeedsApproval = true
	}
	return e.finalizeClassification(result, normalized)
}

// sqlDeleteFallback preserves the existing raw-text DELETE/WHERE fallback when
// no configured pattern matched. Keep the same literal checks for every policy
// view, including input text kept separate from a cloud command's real argv.
func sqlDeleteFallback(command string) (RiskTier, string) {
	lower := strings.ToLower(command)
	if !strings.Contains(lower, "delete from") {
		return "", ""
	}
	if strings.Contains(lower, "where") {
		return RiskTierDangerous, "fallback_sql_delete_with_where"
	}
	return RiskTierCritical, "fallback_sql_delete_no_where"
}

// classifyCompoundCommand keeps the highest risk segment, never allowing one
// safe segment to hide an unrecognized segment from explicit request creation.
func (e *PatternEngine) classifyCompoundCommand(normalized *NormalizedCommand, cwd string) *MatchResult {
	result := &MatchResult{MatchedSegments: []SegmentMatch{}}
	highestTier := RiskTier("")
	for _, segment := range normalized.Segments {
		if cwd != "" || ExtractCommandName(segment) == "rm" {
			segment = ResolvePathsInCommand(segment, cwd)
		}
		segmentMatch := SegmentMatch{Segment: segment}
		segmentMatch.Tier, segmentMatch.MatchedPattern = e.classifySegment(segment)
		if segmentMatch.MatchedPattern == "" {
			segmentMatch.Tier, segmentMatch.MatchedPattern = sqlDeleteFallback(segment)
		}
		switch segmentMatch.Tier {
		case RiskTierCritical:
			highestTier = RiskTierCritical
		case RiskTierDangerous:
			if highestTier != RiskTierCritical {
				highestTier = RiskTierDangerous
			}
		case RiskTierCaution:
			if highestTier == "" || highestTier == RiskTier(RiskSafe) {
				highestTier = RiskTierCaution
			}
		case RiskTier(RiskSafe):
			if highestTier == "" {
				highestTier = RiskTier(RiskSafe)
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

// classifySegment returns the tier and pattern of the first rule matching
// one normalized segment, checking safe, critical, dangerous and caution
// rules in that order. An empty pattern means no rule matched.
//
// The normalizer replaces an xargs invocation by the command it runs plus a
// stand-in for the arguments xargs appends; such a command is never SAFE,
// since those arguments are unknown (`xargs kubectl delete pod` deletes
// whatever pods it reads). A segment that still mentions xargs (behind a
// program the normalizer does not unwrap, such as `stdbuf -oL xargs rm -rf
// /`) is also classified as the text after "xargs", and the riskier result
// wins. For that comparison an unmatched result ranks above SAFE, so neither
// reading's allowlist match may clear the other.
func (e *PatternEngine) classifySegment(segment string) (RiskTier, string) {
	allowSafe := !strings.Contains(segment, xargsArgumentsPlaceholder) && !strings.Contains(segment, findPathPlaceholder)
	tier, pattern := e.matchSegment(segment, allowSafe)
	if xargsCmd := ExtractXargsCommand(segment); xargsCmd != "" {
		if xTier, xPattern := e.matchSegment(xargsCmd, false); xargsRank(xTier, xPattern) > xargsRank(tier, pattern) {
			tier, pattern = xTier, xPattern
		}
	}
	return tier, pattern
}

func xargsRank(tier RiskTier, pattern string) int {
	switch {
	case pattern == "":
		return 1
	case tier == RiskTierCritical:
		return 4
	case tier == RiskTierDangerous:
		return 3
	case tier == RiskTierCaution:
		return 2
	default:
		return 0
	}
}

func (e *PatternEngine) matchSegment(segment string, allowSafe bool) (RiskTier, string) {
	if allowSafe {
		if match := e.matchPatterns(segment, e.safe); match != nil {
			return RiskTier(RiskSafe), match.Pattern
		}
	}
	if match := e.matchPatterns(segment, e.critical); match != nil {
		return RiskTierCritical, match.Pattern
	}
	if match := e.matchPatterns(segment, e.dangerous); match != nil {
		return RiskTierDangerous, match.Pattern
	}
	if match := e.matchPatterns(segment, e.caution); match != nil {
		return RiskTierCaution, match.Pattern
	}
	return "", ""
}

func (e *PatternEngine) matchPatterns(cmd string, patterns []*Pattern) *Pattern {
	for _, p := range patterns {
		if p.Compiled.MatchString(cmd) {
			return p
		}
	}
	return nil
}

// finalizeClassification applies the parse-error upgrade and then the effect
// floors: opaque code and file-writing shell redirections can never borrow
// a SAFE program rule or escape as "no pattern". Stronger risk matches remain.
func (e *PatternEngine) finalizeClassification(res *MatchResult, normalized *NormalizedCommand) *MatchResult {
	// Options can themselves be the subject of a project rule, such as a
	// production repository or cluster. Git's existing option-policy views
	// ignore SAFE rules; preserve that protection. For newly normalized cloud
	// commands, retain the original view's prior SAFE-first verdict so a broad
	// hazard does not reactivate on an already exempted pod deletion. Original
	// SAFE results never lower the exposed operation's risk. classifySegment
	// also prevents unknown find/xargs operands from regaining SAFE here.
	// These views are not extra executed or unmatched commands.
	for _, original := range normalized.OriginalOptionSegments {
		var tier RiskTier
		var pattern string
		if ExtractCommandName(original) == "git" {
			tier, pattern = e.matchSegment(original, false)
		} else {
			tier, pattern = e.classifySegment(original)
			if pattern == "" {
				tier, pattern = sqlDeleteFallback(original)
			}
		}
		if pattern != "" && tierHigher(tier, res.Tier) {
			res.Tier, res.MatchedPattern = tier, pattern
			res.IsSafe, res.NeedsApproval = false, true
		}
	}
	res = e.applyParseUpgrade(res, normalized.ParseError)
	res.Opaque = normalized.Opaque
	if res.Tier != "" && res.Tier != RiskTier(RiskSafe) {
		return res
	}
	switch {
	case normalized.Opaque:
		res.MatchedPattern = "unresolved_execution"
	case normalized.WritesFiles:
		res.MatchedPattern = "file_write_redirection"
	default:
		return res
	}
	res.Tier = RiskTierCaution
	res.MinApprovals = tierApprovals(res.Tier)
	res.NeedsApproval = true
	res.IsSafe = false
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

// ExportClaudeHook retains conservative offline regex classification. Only
// literal simple commands can use its safe rules; the native classifier remains
// authoritative for normalization, project policy and single-use approvals.
func (e *PatternEngine) ExportClaudeHook() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var sb strings.Builder
	sb.WriteString("# Auto-generated by: slb patterns export --format=claude-hook\n")
	sb.WriteString(fmt.Sprintf("# Generated: %s\n# SHA256: %s\n", time.Now().UTC().Format(time.RFC3339), e.computeHashLocked()))
	sb.WriteString("# DO NOT EDIT - regenerate with: slb patterns export --format=claude-hook\n\nimport json\nimport re\nfrom typing import Tuple, Optional\n\n")
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
	// Reuse the native analyzer's execution-command vocabulary without trying
	// to duplicate its shell parser in Python. A custom safe regex matching a
	// wrapper, interpreter or evaluator cannot prove its payload is safe.
	normalizationCommands := map[string]bool{
		".": true, "alias": true, "eval": true, "hash": true, "source": true, "python": true, "find": true,
		"if": true, "then": true, "else": true, "elif": true, "fi": true,
		"for": true, "in": true, "do": true, "done": true, "case": true, "esac": true,
		"while": true, "until": true, "select": true, "function": true, "coproc": true,
	}
	for name := range executionWrappers {
		normalizationCommands[name] = true
	}
	for name := range stdinShells {
		normalizationCommands[name] = true
	}
	for name := range stdinInterpreters {
		normalizationCommands[name] = true
	}
	names := make([]string, 0, len(normalizationCommands))
	for name := range normalizationCommands {
		names = append(names, name)
	}
	sort.Strings(names)
	encodedNames, _ := json.Marshal(names)
	sb.WriteString("NORMALIZATION_COMMANDS = frozenset(" + string(encodedNames) + ")\n\n")
	// Share Git's option arities with the native normalizer. The exported hook
	// only applies them to literal argv, never to quoted or executable shell
	// syntax; an opaque configuration override cannot borrow a safe rule.
	gitFlags, _ := json.Marshal(gitGlobalFlags)
	gitValues, _ := json.Marshal(gitGlobalValueOptions)
	gitInformation, _ := json.Marshal(gitGlobalInformationOptions)
	sb.WriteString(fmt.Sprintf("GIT_GLOBAL_FLAGS = json.loads(%q)\n", string(gitFlags)))
	sb.WriteString(fmt.Sprintf("GIT_GLOBAL_VALUE_OPTIONS = json.loads(%q)\n", string(gitValues)))
	sb.WriteString(fmt.Sprintf("GIT_GLOBAL_INFORMATION_OPTIONS = json.loads(%q)\n\n", string(gitInformation)))
	cloudGlobals, _ := json.Marshal(cloudGlobalOptions)
	deleteOptions, _ := json.Marshal(kubectlDeleteOptions)
	terraformOptions, _ := json.Marshal(terraformExecutionOptions)
	protectedResources, _ := json.Marshal(kubectlProtectedResources)
	unresolvedMarkers, _ := json.Marshal([]string{xargsArgumentsPlaceholder, findPathPlaceholder})
	_, _ = fmt.Fprintf(&sb, "CLOUD_GLOBAL_OPTIONS = json.loads(%q)\n", string(cloudGlobals))
	_, _ = fmt.Fprintf(&sb, "KUBECTL_DELETE_OPTIONS = json.loads(%q)\n", string(deleteOptions))
	_, _ = fmt.Fprintf(&sb, "TERRAFORM_EXECUTION_OPTIONS = json.loads(%q)\n", string(terraformOptions))
	_, _ = fmt.Fprintf(&sb, "KUBECTL_PROTECTED_RESOURCES = json.loads(%q)\n", string(protectedResources))
	sb.WriteString("UNRESOLVED_ARGUMENT_MARKERS = " + string(unresolvedMarkers) + "\n\n")
	sb.WriteString(`# Python's re backtracks, so a rule that is linear in Go can take seconds on a
# long command here. Longer commands are not scanned; they need the strictest
# approval instead of being allowed unclassified.
MAX_CLASSIFIED_LENGTH = 16384


def _literal_simple_command(command: str) -> bool:
    """Recognize a narrow literal argv form, without parsing shell syntax."""
    # Quotes, escapes, operators, redirections, expansions, globs, comments and
    # non-ASCII/control characters all require native analysis or confirmation.
    # A SAFE regex prefix must never hide another command or an expanded target.
    if re.fullmatch(r'[A-Za-z0-9_./:@%+=, \t-]+', command) is None:
        return False
    if any(marker in command for marker in UNRESOLVED_ARGUMENT_MARKERS):
        return False
    words = command.split()
    executable = words[0].rsplit('/', 1)[-1].lower()
    if '=' in words[0] or executable in NORMALIZATION_COMMANDS:
        return False
    if executable.startswith(('python2', 'python3')):
        return False
    # The native analyzer also inspects xargs behind an unrecognized wrapper;
    # its appended arguments are unavailable to this exported policy snapshot.
    return not any(word.rsplit('/', 1)[-1].lower() == 'xargs' for word in words)


def _normalize_literal_git(command: str) -> Tuple[str, bool]:
    """Expose a literal Git subcommand without hiding configuration/code inputs."""
    words = command.split()
    if len(words) < 2 or words[0].rsplit('/', 1)[-1] != 'git':
        return (command, False)
    opaque = False
    i = 1
    while i < len(words):
        option = words[i]
        if not option.startswith('-'):
            return (' '.join(['git'] + words[i:]), opaque)
        if option in GIT_GLOBAL_FLAGS:
            opaque = opaque or GIT_GLOBAL_FLAGS[option]
        elif option in GIT_GLOBAL_INFORMATION_OPTIONS or option.startswith('--list-cmds='):
            # These print information or select the help/version command;
            # following words are not a separately executed Git subcommand.
            return ('git ' + option, opaque)
        elif option.startswith('--exec-path='):
            opaque = True
        else:
            name, equals, value = option.partition('=')
            spec = GIT_GLOBAL_VALUE_OPTIONS.get(name)
            if spec is None or (equals and not spec['equals']):
                return (command, True)
            if not equals:
                i += 1
                if i == len(words):
                    return (command, True)
            opaque = opaque or spec['opaque']
        i += 1
    return (command, True)


def _cloud_bool(value: str) -> Optional[bool]:
    if value in ('1', 't', 'T', 'TRUE', 'true', 'True'):
        return True
    if value in ('0', 'f', 'F', 'FALSE', 'false', 'False'):
        return False
    return None


def _scan_cloud_option(words, index, tables):
    """Consume one known literal option without guessing another word's role."""
    word = words[index]
    name, equals, value = word.partition('=')
    spec = next((table[name] for table in tables if name in table), None)
    attached = False
    if spec is None and len(word) > 2 and word[0] == '-' and word[1] != '-':
        short = next((table[word[:2]] for table in tables if word[:2] in table), None)
        if short is not None and short['attached']:
            name, value, spec, attached, equals = word[:2], word[2:], short, True, ''
    if spec is None or (equals and not spec['equals']):
        return None
    next_index = index + 1
    if spec['value']:
        if not equals and not attached:
            if spec['default']:
                value = spec['default']
            elif spec['separate'] and next_index < len(words):
                value = words[next_index]
                next_index += 1
            else:
                return None
    else:
        if not equals:
            value = 'true'
        if _cloud_bool(value) is None:
            return None
    return (name, value, next_index, spec['opaque'], spec['information'])


def _observe_cloud_option(state, option):
    name, value, _, opaque, information = option
    state['opaque'] = state['opaque'] or opaque
    if information:
        # Cobra/pflag's last Boolean value wins, even after a subcommand.
        state['information'] = name if _cloud_bool(value) else None


def _inspect_cloud_global_suffix(words, options, state):
    index = 2
    while index < len(words):
        word = words[index]
        if word == '--':
            return
        if not word.startswith('-'):
            index += 1
            continue
        option = _scan_cloud_option(words, index, (options,))
        if option is None:
            # Unknown grammar may consume later apparent flags as data.
            state['opaque'], state['uncertain'] = True, True
            return
        _observe_cloud_option(state, option)
        index = option[2]


def _protected_kubectl_resource(resource: str) -> Optional[str]:
    parts = resource.split(',')
    for index, part in enumerate(parts):
        kind, slash, suffix = part.partition('/')
        base = kind.split('.', 1)[0]
        if base in KUBECTL_PROTECTED_RESOURCES:
            parts[index] = KUBECTL_PROTECTED_RESOURCES[base] + kind[len(base):] + slash + suffix
            parts[0], parts[index] = parts[index], parts[0]
            return ','.join(parts)
    return None


def _normalize_literal_kubectl_delete(words, state):
    resources = []
    options_done, index = False, 2
    while index < len(words):
        word = words[index]
        if not options_done and word == '--':
            options_done, index = True, index + 1
            continue
        if not options_done and word.startswith('-'):
            option = _scan_cloud_option(words, index, (KUBECTL_DELETE_OPTIONS, CLOUD_GLOBAL_OPTIONS['kubectl']))
            if option is None:
                state['opaque'], state['uncertain'] = True, True
                return (words, True)
            _observe_cloud_option(state, option)
            index = option[2]
            continue
        resources.append(word)
        index += 1
    for index, resource in enumerate(resources):
        if index > 0 and '/' not in resource:
            continue
        protected = _protected_kubectl_resource(resource)
        if protected is not None:
            resources[index] = protected
            resources[0], resources[index] = resources[index], resources[0]
            break
    return ([words[0], 'delete'] + resources, False)


def _normalize_literal_terraform_execution(words, state):
    destroy, invalid, index = words[1] == 'destroy', False, 2
    targets = []
    while index < len(words):
        word = words[index]
        # Like Go flag parsing, operands and -- stop option parsing.
        if word == '--' or not word.startswith('-'):
            break
        option = _scan_cloud_option(words, index, (TERRAFORM_EXECUTION_OPTIONS,))
        if option is None:
            state['opaque'], state['uncertain'], invalid = True, True, True
            break
        _observe_cloud_option(state, option)
        name, value, index, _, _ = option
        if name == '-target':
            if value:
                targets.append('-target=' + value)
            else:
                state['opaque'], state['uncertain'], invalid = True, True, True
        if name == '-destroy' and words[1] == 'apply':
            destroy = _cloud_bool(value)
    if destroy:
        words = [words[0], 'destroy'] + targets
    return (words, invalid)


def _normalize_literal_cloud(command: str):
    """Classify known cloud argv; never read config or execute a cloud client."""
    words = command.split()
    name = words[0].rsplit('/', 1)[-1]
    options = CLOUD_GLOBAL_OPTIONS.get(name)
    if len(words) < 2 or options is None:
        return (command, False, False)
    state = {'opaque': False, 'information': None, 'uncertain': False}
    index = 1
    while index < len(words):
        if words[index].startswith('-'):
            option = _scan_cloud_option(words, index, (options,))
            if option is None:
                return (command, True, True)
            _observe_cloud_option(state, option)
            index = option[2]
            continue
        words = [name] + words[index:]
        invalid = False
        if name == 'kubectl' and words[1] == 'delete':
            words, invalid = _normalize_literal_kubectl_delete(words, state)
        elif name == 'terraform' and words[1] in ('destroy', 'apply'):
            words, invalid = _normalize_literal_terraform_execution(words, state)
        elif name in ('kubectl', 'helm'):
            _inspect_cloud_global_suffix(words, options, state)
        if not invalid and state['information'] is not None and not state['uncertain']:
            words = [name, state['information']]
        return (' '.join(words), state['opaque'], invalid)
    if state['information'] is not None and not state['uncertain']:
        return (' '.join([name, state['information']]), state['opaque'], False)
    return (command, True, True)


def classify(command: str) -> Tuple[str, int]:
    """Classify against the exported policy; return (tier, min_approvals)."""
    command = command.strip()
    if len(command) > MAX_CLASSIFIED_LENGTH:
        return ('critical', MIN_APPROVALS['critical'])
    literal = bool(command) and _literal_simple_command(command)
    opaque, invalid = False, False
    original_commands = [command]
    if literal:
        words = command.split()
        executable = words[0].rsplit('/', 1)[-1]
        if executable == 'git' or executable in CLOUD_GLOBAL_OPTIONS:
            canonical_original = ' '.join([executable] + words[1:])
            if canonical_original != command:
                original_commands.append(canonical_original)
        command, opaque = _normalize_literal_git(command)
        command, cloud_opaque, invalid = _normalize_literal_cloud(command)
        opaque = opaque or cloud_opaque
    # A custom rule can target global options, such as -C /production.
    # Retain raw spellings too: exported policies previously saw absolute
    # executable paths and literal whitespace before this normalization.
    rank = {'unknown': 0, 'safe': 0, 'caution': 1, 'dangerous': 2, 'critical': 3}
    original_tier = 'unknown'
    original_commands = [view for view in original_commands if view != command]
    for view in original_commands:
        # New cloud views retain prior SAFE-first results; Git's existing
        # option-policy views continue to apply hazardous rules regardless
        # of SAFE matches. No original SAFE result lowers canonical risk.
        cloud_view = view.split()[0].rsplit('/', 1)[-1] in CLOUD_GLOBAL_OPTIONS
        if literal and cloud_view and any(pattern.search(view) for pattern in SAFE_PATTERNS):
            continue
        for tier, patterns in (('critical', CRITICAL_PATTERNS), ('dangerous', DANGEROUS_PATTERNS),
                               ('caution', CAUTION_PATTERNS)):
            if any(pattern.search(view) for pattern in patterns):
                if rank[tier] > rank[original_tier]:
                    original_tier = tier
                break
    matched_tier = 'unknown'
    for tier, patterns in (('safe', SAFE_PATTERNS), ('critical', CRITICAL_PATTERNS),
                           ('dangerous', DANGEROUS_PATTERNS), ('caution', CAUTION_PATTERNS)):
        if tier == 'safe' and not literal:
            continue
        if any(pattern.search(command) for pattern in patterns):
            matched_tier = tier
            break
    # Keep native SAFE-first policy precedence for the normalized command,
    # then apply original-option risk and unknown-code floors independently.
    if rank[original_tier] > rank[matched_tier]:
        matched_tier = original_tier
    if invalid:
        matched_tier = {'unknown': 'caution', 'safe': 'caution', 'caution': 'dangerous',
                        'dangerous': 'critical', 'critical': 'critical'}[matched_tier]
    if opaque and matched_tier in ('safe', 'unknown'):
        matched_tier = 'caution'
    return (matched_tier, MIN_APPROVALS.get(matched_tier, 0))


def needs_approval(command: str) -> bool:
    """Return True when approval or explicit confirmation is needed."""
    tier, _ = classify(command)
    return tier != 'safe'


def is_blocked(command: str) -> Tuple[bool, Optional[str]]:
    """Check whether automatic execution must wait for review/confirmation."""
    tier, approvals = classify(command)
    if tier == 'safe':
        return (False, None)
    if tier == 'critical':
        return (True, f"CRITICAL: Requires {approvals} approvals. Use 'slb request' to submit.")
    if tier == 'dangerous':
        return (True, f"DANGEROUS: Requires {approvals} approval. Use 'slb request' to submit.")
    if tier == 'caution':
        return (True, "CAUTION: Command logged for review. Use 'slb request' to submit.")
    return (True, "OFFLINE: Native SLB analysis or explicit confirmation required; local rules cannot allow this command.")
`)
	return sb.String()
}
