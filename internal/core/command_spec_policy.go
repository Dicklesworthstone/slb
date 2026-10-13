package core

import "strings"

// ClassifyCommandSpec classifies the invocation that RunCommand will execute.
// Raw is authoritative only in shell mode. For direct execution Argv is the
// executable specification, not a display string that can describe a different
// operation. Quoting the argv preserves literal arguments through the shell
// normalizer; explicit interpreters such as sh -c are still analyzed there.
func (e *PatternEngine) ClassifyCommandSpec(spec CommandSpec) *MatchResult {
	return e.ClassifyCommand(commandSpecPolicyText(spec.Raw, spec.Argv, spec.Shell), spec.Cwd)
}

// commandSpecPolicyText mirrors RunCommand's three invocation modes without
// changing the stored command, its hash, or the arguments passed to execution.
// The Fields fallback is intentionally NOT shell parsing: that is how legacy
// non-shell specs without Argv execute. This function never
// performs environment expansion or command substitution.
func commandSpecPolicyText(raw string, argv []string, shell bool) string {
	if shell {
		return raw
	}
	if len(argv) == 0 {
		argv = strings.Fields(raw)
	}
	// Quote EVERY word, including executable names, comments, backticks and
	// empty arguments. A display-oriented join can manufacture shell syntax
	// or swallow arguments; this string is input to the shell normalizer.
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}
