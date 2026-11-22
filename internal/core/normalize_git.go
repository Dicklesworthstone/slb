package core

import (
	"path/filepath"
	"strings"
)

// These tables describe Git's global options, before its subcommand. The
// exported Python classifier uses the same vocabulary without evaluating shell
// syntax. Values identify options that may select additional executable code.
var gitGlobalFlags = map[string]bool{
	"-p": true, "--paginate": true,
	"-P": false, "--no-pager": false,
	"--bare": false, "--no-replace-objects": false, "--no-lazy-fetch": false,
	"--literal-pathspecs": false, "--no-literal-pathspecs": false,
	"--glob-pathspecs": false, "--noglob-pathspecs": false, "--icase-pathspecs": false,
	"--no-optional-locks": false, "--no-advice": false,
}

type gitGlobalValueOption struct {
	Equals bool `json:"equals"`
	Opaque bool `json:"opaque"`
}

var gitGlobalValueOptions = map[string]gitGlobalValueOption{
	"-C":             {},
	"-c":             {Opaque: true},
	"--config-env":   {Equals: true, Opaque: true},
	"--git-dir":      {Equals: true},
	"--work-tree":    {Equals: true},
	"--namespace":    {Equals: true},
	"--attr-source":  {Equals: true},
	"--shallow-file": {},
}

var gitGlobalInformationOptions = map[string]bool{
	"-h": true, "--help": true, "-v": true, "--version": true,
	"--exec-path": true, "--html-path": true, "--man-path": true, "--info-path": true,
}

// normalizeGitTokens exposes the subcommand behind Git's global options for
// classification only. It never resolves a path, reads configuration, expands
// a shell word, or changes the command that will actually be executed.
//
// Configuration and executable selectors can run additional code, so stripping
// them must not let the remaining subcommand borrow a SAFE classification.
// Unknown options or computed subcommands fail closed instead of guessing which
// subsequent word Git would execute. A computed option value also remains
// opaque, but does not hide a statically visible destructive subcommand.
func normalizeGitTokens(tokens []string) (normalized []string, opaque, valid bool) {
	if len(tokens) < 2 || filepath.Base(tokens[0]) != "git" {
		return tokens, false, true
	}
	for i := 1; i < len(tokens); i++ {
		option := tokens[i]
		if !strings.HasPrefix(option, "-") {
			if option == "" || gitTokenIsDynamic(option) {
				return tokens, true, false
			}
			result := make([]string, 1, len(tokens)-i+1)
			result[0] = tokens[0]
			return append(result, tokens[i:]...), opaque, true
		}
		if code, ok := gitGlobalFlags[option]; ok {
			opaque = opaque || code
			continue
		}
		if gitGlobalInformationOptions[option] || strings.HasPrefix(option, "--list-cmds=") {
			// These print information or select help/version; the following
			// operands are not another Git subcommand to execute.
			return []string{tokens[0], option}, opaque || gitTokenIsDynamic(option), true
		}
		if strings.HasPrefix(option, "--exec-path=") {
			opaque = true
			continue
		}
		name, value, equals := strings.Cut(option, "=")
		spec, ok := gitGlobalValueOptions[name]
		if !ok || (equals && !spec.Equals) {
			return tokens, true, false
		}
		if !equals {
			i++
			if i == len(tokens) {
				return tokens, true, false
			}
			// Git consumes this word even when it begins with '-'. It is a
			// value, not a second option or an executable subcommand.
			value = tokens[i]
		}
		opaque = opaque || spec.Opaque || gitTokenIsDynamic(value)
	}
	// A bare `git` is handled above. Once options have been supplied, the
	// absence of a known subcommand cannot authorize an implicit operation.
	return tokens, true, false
}

// The shell tokenizer deliberately does not evaluate expansion. Quote removal
// loses whether a metacharacter was literal, so ambiguity stays conservative.
func gitTokenIsDynamic(token string) bool {
	return strings.ContainsAny(token, "$`*?{\x00") ||
		(strings.Contains(token, "[") && strings.Contains(token, "]")) ||
		strings.Contains(token, "__slb_substitution__") ||
		strings.Contains(token, xargsArgumentsPlaceholder)
}
