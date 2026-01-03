// Package core provides command normalization for pattern matching.
package core

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mattn/go-shellwords"
)

// NormalizedCommand represents a parsed and normalized command.
type NormalizedCommand struct {
	// Original is the original command string.
	Original string
	// Primary is the primary command after stripping wrappers.
	Primary string
	// Segments contains individual command segments for compound commands.
	Segments []string
	// IsCompound indicates if this is a compound command.
	IsCompound bool
	// HasSubshell indicates if the command contains subshells.
	HasSubshell bool
	// StrippedWrappers lists the wrappers that were stripped.
	StrippedWrappers []string
	// ParseError indicates if parsing failed (triggers tier upgrade).
	ParseError bool
}

// Command wrapper prefixes to strip
var wrapperPrefixes = []string{
	"sudo",
	"doas",
	"env",
	"command",
	"builtin",
	"time",
	"nice",
	"ionice",
	"nohup",
	"strace",
	"ltrace",
}

// Shell commands that execute other commands with -c flag
var shellExecutors = []string{"bash", "sh", "zsh", "ksh", "dash"}

// Pattern to extract command from shell -c 'command'
var shellCPattern = regexp.MustCompile(`^(bash|sh|zsh|ksh|dash)\s+-c\s+['"](.+)['"]$`)

// Pattern to detect xargs with a command
var xargsPattern = regexp.MustCompile(`xargs\s+(.+)$`)

// Compound command separators
var compoundSeparators = regexp.MustCompile(`\s*(?:;|&&|\|\||&)\s*`)

// Pipe detection
var pipePattern = regexp.MustCompile(`\s*\|\s*`)

// Subshell patterns: $(...) or `...` or (...)
var subshellPattern = regexp.MustCompile("\\$\\([^)]+\\)|`[^`]+`|\\([^)]+\\)")

var envAssignPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// splitCompoundShellAware splits a command on compound separators (;, &&, ||, &)
// while respecting shell quoting rules. Separators inside quotes are not split.
func splitCompoundShellAware(cmd string) []string {
	var segments []string
	var current strings.Builder
	inSingleQuote := false
	inDoubleQuote := false
	escaped := false
	runes := []rune(cmd)

	for i := 0; i < len(runes); i++ {
		r := runes[i]

		// Handle escape sequences
		if escaped {
			current.WriteRune(r)
			escaped = false
			continue
		}

		if r == '\\' && !inSingleQuote {
			current.WriteRune(r)
			escaped = true
			continue
		}

		// Handle quote state changes
		if r == '\'' && !inDoubleQuote {
			inSingleQuote = !inSingleQuote
			current.WriteRune(r)
			continue
		}

		if r == '"' && !inSingleQuote {
			inDoubleQuote = !inDoubleQuote
			current.WriteRune(r)
			continue
		}

		// Check for compound separators only when outside quotes
		if !inSingleQuote && !inDoubleQuote {
			// Check for && or ||
			if i+1 < len(runes) {
				if (r == '&' && runes[i+1] == '&') || (r == '|' && runes[i+1] == '|') {
					seg := strings.TrimSpace(current.String())
					if seg != "" {
						segments = append(segments, seg)
					}
					current.Reset()
					i++ // Skip the second character of && or ||
					continue
				}
			}

			// Check for ; or single &
			if r == ';' || r == '&' {
				seg := strings.TrimSpace(current.String())
				if seg != "" {
					segments = append(segments, seg)
				}
				current.Reset()
				continue
			}
		}

		current.WriteRune(r)
	}

	// Add the last segment
	seg := strings.TrimSpace(current.String())
	if seg != "" {
		segments = append(segments, seg)
	}

	return segments
}

// NormalizeCommand parses and normalizes a command for pattern matching.
func NormalizeCommand(cmd string) *NormalizedCommand {
	result := &NormalizedCommand{
		Original:   cmd,
		Segments:   []string{},
		ParseError: false,
	}

	// Trim whitespace
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return result
	}

	// Check for subshells
	result.HasSubshell = subshellPattern.MatchString(cmd)

	// Split on compound separators using shell-aware parsing.
	// We use proper tokenization to determine if separators are inside quotes.
	segments := splitCompoundShellAware(cmd)
	if len(segments) > 1 {
		result.IsCompound = true
	}

	// Also check for pipes (not technically compound, but multiple commands)
	for _, seg := range segments {
		if pipePattern.MatchString(seg) {
			result.IsCompound = true
			// Split on pipes and add each segment
			pipeParts := pipePattern.Split(seg, -1)
			for _, part := range pipeParts {
				part = strings.TrimSpace(part)
				if part != "" {
					result.Segments = append(result.Segments, part)
				}
			}
		} else {
			seg = strings.TrimSpace(seg)
			if seg != "" {
				result.Segments = append(result.Segments, seg)
			}
		}
	}

	// Normalize each segment (strip wrappers with shell-aware parsing)
	normalizedSegments := make([]string, 0, len(result.Segments))
	for _, seg := range result.Segments {
		normalized, wrappers, parseErr := normalizeSegment(seg)
		if parseErr {
			result.ParseError = true
		}
		if normalized != "" {
			normalizedSegments = append(normalizedSegments, normalized)
		}
		result.StrippedWrappers = append(result.StrippedWrappers, wrappers...)
	}
	result.Segments = normalizedSegments

	// Primary command is the first segment after normalization
	if len(result.Segments) > 0 {
		result.Primary = result.Segments[0]
	}

	return result
}

// normalizeSegment strips wrappers using a shell-aware tokenizer.
func normalizeSegment(seg string) (string, []string, bool) {
	// First check for shell -c 'command' pattern and extract inner command
	if match := shellCPattern.FindStringSubmatch(seg); match != nil {
		innerCmd := match[2]
		// Recursively normalize the inner command
		inner, wrappers, parseErr := normalizeSegment(innerCmd)
		wrappers = append([]string{match[1] + " -c"}, wrappers...)
		return inner, wrappers, parseErr
	}

	parser := shellwords.NewParser()
	tokens, err := parser.Parse(seg)
	parseErr := err != nil
	if parseErr {
		// Fallback to simple split to avoid losing data
		tokens = strings.Fields(seg)
	}

	stripped := []string{}

	i := 0
	for i < len(tokens) {
		tok := tokens[i]

		// env with assignments
		if tok == "env" {
			stripped = append(stripped, "env")
			i++
			for i < len(tokens) && isEnvAssignment(tokens[i]) {
				i++
			}
			continue
		}

		if isWrapper(tok) {
			stripped = append(stripped, tok)
			i++
			continue
		}
		break
	}

	if i >= len(tokens) {
		return "", stripped, parseErr
	}

	normalized := strings.TrimSpace(strings.Join(tokens[i:], " "))
	return normalized, stripped, parseErr
}

// ExtractXargsCommand extracts the command from an xargs invocation.
// Returns the command that xargs will execute, or empty string if not xargs.
func ExtractXargsCommand(seg string) string {
	if match := xargsPattern.FindStringSubmatch(seg); match != nil {
		return strings.TrimSpace(match[1])
	}
	return ""
}

func isWrapper(tok string) bool {
	for _, w := range wrapperPrefixes {
		if tok == w {
			return true
		}
	}
	return false
}

func isEnvAssignment(tok string) bool {
	return envAssignPattern.MatchString(tok)
}

// ResolvePathsInCommand expands relative paths to absolute paths using tokenization.
// It handles home directory expansion (~), absolute paths, and relative paths
// containing separators (./, ../, foo/bar).
func ResolvePathsInCommand(cmd, cwd string) string {
	// Parse into tokens to safely handle arguments
	parser := shellwords.NewParser()
	parser.ParseEnv = false
	parser.ParseBacktick = false
	tokens, err := parser.Parse(cmd)
	if err != nil {
		// Fallback to simple fields if parsing fails
		tokens = strings.Fields(cmd)
	}

	home, _ := os.UserHomeDir()

	for i, tok := range tokens {
		// Handle flag=value case (e.g., --output=/tmp/foo)
		if strings.HasPrefix(tok, "-") {
			if idx := strings.Index(tok, "="); idx != -1 {
				key := tok[:idx+1]
				val := tok[idx+1:]
				tokens[i] = key + cleanPathToken(val, cwd, home)
			}
			continue
		}

		tokens[i] = cleanPathToken(tok, cwd, home)
	}

	return strings.Join(tokens, " ")
}

// cleanPathToken cleans a single token if it looks like a path.
func cleanPathToken(tok, cwd, home string) string {
	// Expand ~
	if home != "" {
		if tok == "~" {
			tok = home
		} else if strings.HasPrefix(tok, "~/") {
			tok = filepath.Join(home, tok[2:])
		}
	}

	// If absolute, clean and return
	if filepath.IsAbs(tok) {
		return filepath.Clean(tok)
	}

	// If relative path (contains separator or is . / ..), resolve against CWD
	if strings.Contains(tok, "/") || tok == "." || tok == ".." {
		if cwd != "" {
			return filepath.Clean(filepath.Join(cwd, tok))
		}
		return filepath.Clean(tok)
	}

	// Otherwise treat as plain string (command name, flag, simple argument)
	return tok
}

// ExtractCommandName extracts just the command name (first word).
func ExtractCommandName(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return ""
	}
	// Return just the base command name, without path
	return filepath.Base(fields[0])
}
