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

// splitPipesShellAware splits a segment on unquoted pipes, using the same
// quoting rules as splitCompoundShellAware.
//
// Splitting with a regex instead broke every command carrying a `|` inside
// quotes — `jq -r '.[] | .a'`, `gh api ... --jq '.[] | .number'`, `awk '/a|b/'`,
// a commit message with a pipe — into fragments with unbalanced quotes. The
// tokenizer then failed on those fragments, and a parse failure is upgraded to
// CAUTION, which the hook turns into a human approval prompt. Ordinary
// read-only commands were stopping sessions for someone to approve (GitHub
// #14). `;`, `&&`, `||` and `&` were already quote-aware; only the pipe was not.
func splitPipesShellAware(seg string) []string {
	var parts []string
	var current strings.Builder
	inSingleQuote := false
	inDoubleQuote := false
	escaped := false
	parenDepth := 0
	runes := []rune(seg)

	for i := 0; i < len(runes); i++ {
		r := runes[i]

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

		if !inSingleQuote && !inDoubleQuote {
			if r == '(' {
				parenDepth++
			} else if r == ')' && parenDepth > 0 {
				parenDepth--
			}
		}
		if r == '|' && !inSingleQuote && !inDoubleQuote && parenDepth == 0 {
			// `||` is a compound separator, already split upstream; leave any
			// that reaches here intact rather than treating it as two pipes.
			if i+1 < len(runes) && runes[i+1] == '|' {
				current.WriteRune(r)
				current.WriteRune(runes[i+1])
				i++
				continue
			}
			parts = append(parts, strings.TrimSpace(current.String()))
			current.Reset()
			continue
		}

		current.WriteRune(r)
	}

	parts = append(parts, strings.TrimSpace(current.String()))
	return parts
}

// splitCompoundShellAware splits a command on compound separators (;, &&, ||, &)
// while respecting shell quoting rules. Separators inside quotes are not split.
func splitCompoundShellAware(cmd string) []string {
	var segments []string
	var current strings.Builder
	inSingleQuote := false
	inDoubleQuote := false
	escaped := false
	parenDepth := 0
	runes := []rune(cmd)

	for i := 0; i < len(runes); i++ {
		r := runes[i]
		// An unquoted backslash-newline is removed, not turned into a
		// separator or a space (r\\\nm is still the executable rm).
		if !escaped && !inSingleQuote && r == '\\' && i+1 < len(runes) && runes[i+1] == '\n' {
			i++
			continue
		}
		// Comments run to the next newline. Quote/parenthesis characters in
		// a comment must not change the state used to find the next command.
		if !escaped && !inSingleQuote && !inDoubleQuote && r == '#' &&
			(i == 0 || strings.ContainsRune(" \t\r\n;|&(", runes[i-1])) {
			for i+1 < len(runes) && runes[i+1] != '\n' {
				i++
			}
			continue
		}

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

		// Track subshell nesting so a separator inside `( ... )` does not cut
		// the parentheses in half. expandSubshellSegments splits the inside
		// afterwards, so nothing is hidden from classification by this.
		if !inSingleQuote && !inDoubleQuote {
			if r == '(' {
				parenDepth++
			} else if r == ')' && parenDepth > 0 {
				parenDepth--
			}
		}

		// Check for compound separators only when outside quotes and parens
		if !inSingleQuote && !inDoubleQuote && parenDepth == 0 {
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

			// Redirection operators >& / <& / &> are not command separators.
			redirectAmp := r == '&' && ((i > 0 && strings.ContainsRune("<>", runes[i-1])) ||
				(i+1 < len(runes) && runes[i+1] == '>'))
			if r == ';' || r == '\n' || r == '\r' || (r == '&' && !redirectAmp) {
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
	return normalizeCommandDepth(cmd, 0)
}

const maxCommandNesting = 32

// normalizeCommandDepth analyzes the entire executable body of shell wrappers,
// not just its first simple command. All parsing is static: no env expansion,
// command substitution, shell startup, or external executable is invoked.
func normalizeCommandDepth(cmd string, depth int) *NormalizedCommand {
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

	if depth >= maxCommandNesting || len(cmd) > 1<<20 {
		result.Primary = cmd
		result.Segments = []string{cmd}
		result.ParseError = true
		return result
	}

	result.HasSubshell = subshellPattern.MatchString(cmd)
	appendInner := func(inner *NormalizedCommand) {
		result.Segments = append(result.Segments, inner.Segments...)
		result.StrippedWrappers = append(result.StrippedWrappers, inner.StrippedWrappers...)
		result.ParseError = result.ParseError || inner.ParseError
		result.HasSubshell = result.HasSubshell || inner.HasSubshell
	}
	for _, seg := range splitCompoundShellAware(cmd) {
		for _, part := range splitPipesShellAware(seg) {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if inner, ok := stripSubshellWrapper(part); ok {
				result.StrippedWrappers = append(result.StrippedWrappers, "(")
				appendInner(normalizeCommandDepth(inner, depth+1))
				continue
			}
			parser := shellwords.NewParser()
			parser.ParseEnv = false
			parser.ParseBacktick = false
			tokens, err := parser.Parse(maskArithmeticExpansions(part))
			if err != nil {
				result.ParseError = true
				tokens = strings.Fields(part)
			}
			tokens, wrappers, valid := unwrapCommandTokens(tokens)
			result.StrippedWrappers = append(result.StrippedWrappers, wrappers...)
			result.ParseError = result.ParseError || !valid
			if len(tokens) == 0 {
				continue
			}
			if script, ok, valid := shellCommandBody(tokens); ok {
				result.StrippedWrappers = append(result.StrippedWrappers, filepath.Base(tokens[0])+" -c")
				appendInner(normalizeCommandDepth(script, depth+1))
				continue
			} else if !valid {
				result.ParseError = true
			}
			tokens[0] = canonicalExecutable(tokens[0])
			result.Segments = append(result.Segments, strings.Join(tokens, " "))
		}
	}
	result.IsCompound = len(result.Segments) > 1

	// Primary command is the first segment after normalization
	if len(result.Segments) > 0 {
		result.Primary = result.Segments[0]
	}

	return result
}

// expandSubshellSegments replaces any segment that is entirely one subshell
// with the segments of the command inside it.
//
// The compound splitter does not split inside parentheses, so `(cd /tmp && ls)`
// arrives here whole. Handing that to the tokenizer as a single command would
// both fail to parse it and, worse, hide the second command from
// classification — the risk of a compound subshell is the risk of the most
// dangerous command in it, so the inner command list has to be split the same
// way the outer one was (GitHub #14).
func expandSubshellSegments(segments []string) []string {
	expanded := make([]string, 0, len(segments))
	for _, seg := range segments {
		inner, ok := stripSubshellWrapper(seg)
		if !ok {
			expanded = append(expanded, seg)
			continue
		}
		// Recurse: a subshell may wrap another subshell.
		expanded = append(expanded, expandSubshellSegments(splitCompoundShellAware(inner))...)
	}
	return expanded
}

// stripSubshellWrapper unwraps a segment that is entirely one subshell,
// returning the inner command. Only a wrapper whose opening parenthesis closes
// at the very end qualifies, so `(a) | (b)` and `(a) && b` are left to the
// splitters that already understand them.
func stripSubshellWrapper(seg string) (string, bool) {
	seg = strings.TrimSpace(seg)
	if len(seg) < 2 || seg[0] != '(' || seg[len(seg)-1] != ')' {
		return "", false
	}
	depth := 0
	inSingleQuote := false
	inDoubleQuote := false
	escaped := false
	runes := []rune(seg)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && !inSingleQuote {
			escaped = true
			continue
		}
		if r == '\'' && !inDoubleQuote {
			inSingleQuote = !inSingleQuote
			continue
		}
		if r == '"' && !inSingleQuote {
			inDoubleQuote = !inDoubleQuote
			continue
		}
		if inSingleQuote || inDoubleQuote {
			continue
		}
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			// Closed before the end: the parentheses wrap only part of the
			// segment, so this is not a whole-segment subshell.
			if depth == 0 && i != len(runes)-1 {
				return "", false
			}
		}
	}
	if depth != 0 {
		return "", false
	}
	inner := strings.TrimSpace(string(runes[1 : len(runes)-1]))
	if inner == "" {
		return "", false
	}
	return inner, true
}

// maskArithmeticExpansions replaces an arithmetic expansion with a literal so
// the tokenizer can read the rest of the command.
//
// go-shellwords cannot parse `$((...))` and fails the whole segment, which the
// parse-failure path then upgrades to CAUTION — so an ordinary counter
// increment became a human approval prompt. Substituting a constant is safe
// because POSIX arithmetic evaluates numbers, not commands: nothing executable
// is hidden by the mask (GitHub #14).
func maskArithmeticExpansions(seg string) string {
	var out strings.Builder
	runes := []rune(seg)
	for i := 0; i < len(runes); i++ {
		if runes[i] == '$' && i+2 < len(runes) && runes[i+1] == '(' && runes[i+2] == '(' {
			depth := 0
			j := i + 1
			for ; j < len(runes); j++ {
				if runes[j] == '(' {
					depth++
				} else if runes[j] == ')' {
					depth--
					if depth == 0 {
						break
					}
				}
			}
			if depth == 0 && j < len(runes) {
				out.WriteString("0")
				i = j
				continue
			}
		}
		out.WriteRune(runes[i])
	}
	return out.String()
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

	// A whole segment wrapped in a subshell is the same command with the same
	// risk, so it must classify the same way. The tokenizer cannot parse bare
	// parentheses, and the resulting parse failure only upgrades an
	// *unclassified* command to CAUTION — so wrapping a CRITICAL command in
	// parentheses quietly lowered its tier (GitHub #14).
	if inner, ok := stripSubshellWrapper(seg); ok {
		innerNorm, wrappers, parseErr := normalizeSegment(inner)
		return innerNorm, append([]string{"("}, wrappers...), parseErr
	}

	parser := shellwords.NewParser()
	tokens, err := parser.Parse(maskArithmeticExpansions(seg))
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
