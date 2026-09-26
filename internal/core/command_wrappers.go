package core

import (
	"path/filepath"
	"strings"
)

// wrapperOptions describes only flags whose consumption is understood. Unknown
// options cause conservative classification, never guessing which following
// token might be an option's value instead of an executable.
type wrapperOptions struct {
	booleans string
	values   string
	long     map[string]bool // true: consumes a value; false: no following value
}

var executionWrappers = map[string]wrapperOptions{
	"sudo": {"AbEHnPS", "CDghpRTu", map[string]bool{
		"askpass": false, "background": false, "bell": false, "non-interactive": false,
		"preserve-env": false, "preserve-groups": false, "set-home": false,
		"user": true, "group": true, "host": true, "prompt": true,
		"chdir": true, "chroot": true, "command-timeout": true, "close-from": true,
	}},
	"doas":    {"Ln", "Cu", nil},
	"env":     {"i0v", "uC", map[string]bool{"ignore-environment": false, "null": false, "debug": false, "unset": true, "chdir": true}},
	"command": {"p", "", nil},
	"builtin": {},
	"nohup":   {},
	"nice":    {"", "n", map[string]bool{"adjustment": true}},
	"ionice":  {"t", "cn", map[string]bool{"ignore": false, "class": true, "classdata": true}},
	"time":    {"apv", "fo", map[string]bool{"append": false, "portability": false, "verbose": false, "format": true, "output": true}},
	"timeout": {"v", "ks", map[string]bool{"foreground": false, "preserve-status": false, "verbose": false, "kill-after": true, "signal": true}},
	"exec":    {"cl", "a", nil},
	"strace":  {"fFvTtCxy", "eosu", nil},
	"ltrace":  {"fCiST", "eos", nil},
	// GNU and BSD xargs. The optional-argument forms -e/-i/-l only take a
	// value glued to the flag; a glued value is not modeled and fails closed.
	"xargs": {"0rtpxoeil", "adEILnPsJRS", map[string]bool{
		"null": false, "no-run-if-empty": false, "verbose": false, "interactive": false,
		"exit": false, "open-tty": false, "show-limits": false, "eof": false, "replace": false,
		"max-lines": false, "arg-file": true, "delimiter": true, "max-args": true,
		"max-procs": true, "max-chars": true, "process-slot-var": true,
	}},
}

// xargsArgumentsPlaceholder stands for the arguments xargs appends to the
// command it runs.
const xargsArgumentsPlaceholder = "__slb_xargs_arguments__"

func unwrapCommandTokens(tokens []string) ([]string, []string, bool) {
	var wrappers []string
	for depth := 0; len(tokens) > 0; depth++ {
		if depth >= maxCommandNesting {
			return tokens, wrappers, false
		}
		if isEnvAssignment(tokens[0]) {
			tokens = tokens[1:]
			continue
		}
		name := filepath.Base(tokens[0])
		spec, wrapped := executionWrappers[name]
		if !wrapped {
			return tokens, wrappers, true
		}
		// These invocations inspect command names; they do not execute them.
		if name == "command" && len(tokens) > 1 && (tokens[1] == "-v" || tokens[1] == "-V") {
			return tokens, wrappers, true
		}
		start, ok := wrapperCommandStart(tokens, spec)
		if !ok {
			return tokens, wrappers, false
		}
		if name == "timeout" {
			// timeout's duration is a positional operand, not its command.
			start++
		}
		if start >= len(tokens) {
			return nil, wrappers, true
		}
		wrappers = append(wrappers, name)
		tokens = tokens[start:]
	}
	return tokens, wrappers, true
}

func wrapperCommandStart(tokens []string, spec wrapperOptions) (int, bool) {
	i := 1
	for i < len(tokens) {
		arg := tokens[i]
		if arg == "--" {
			return i + 1, true
		}
		if arg == "-" && filepath.Base(tokens[0]) == "env" {
			i++
			continue
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			return i, true
		}
		if strings.HasPrefix(arg, "--") {
			name, _, equals := strings.Cut(arg[2:], "=")
			value, known := spec.long[name]
			if !known {
				return i, false
			}
			if value && !equals {
				i++
				if i >= len(tokens) {
					return i, false
				}
			}
		} else {
			for j := 1; j < len(arg); j++ {
				flag := rune(arg[j])
				if strings.ContainsRune(spec.booleans, flag) {
					continue
				}
				if !strings.ContainsRune(spec.values, flag) {
					return i, false
				}
				if j == len(arg)-1 {
					i++
					if i >= len(tokens) {
						return i, false
					}
				}
				break // remaining characters are the attached option value
			}
		}
		i++
	}
	return i, true
}

// shellCommandBody extracts the literal -c operand, including bundled flags
// such as -lc, without conflating the script with its $0/$1/... arguments.
func shellCommandBody(tokens []string) (body string, found, valid bool) {
	if len(tokens) == 0 {
		return "", false, true
	}
	switch filepath.Base(tokens[0]) {
	case "sh", "bash", "dash", "zsh", "ksh":
	default:
		return "", false, true
	}
	for i := 1; i < len(tokens); i++ {
		arg := tokens[i]
		if arg == "--" || !strings.HasPrefix(arg, "-") || arg == "-" {
			return "", false, true
		}
		if arg == "-o" || arg == "-O" || arg == "--rcfile" || arg == "--init-file" {
			i++
			if i >= len(tokens) {
				return "", false, false
			}
			continue
		}
		if strings.HasPrefix(arg, "--") {
			switch arg {
			case "--noprofile", "--norc", "--posix", "--restricted", "--verbose", "--login":
				continue
			default:
				return "", false, false
			}
		}
		if strings.ContainsRune(arg[1:], 'c') {
			if i+1 >= len(tokens) {
				return "", false, false
			}
			return tokens[i+1], true, true
		}
	}
	return "", false, true
}

// Classify known executables the same way when invoked by absolute/relative
// path. The original command and its execution argv are never modified.
func canonicalExecutable(name string) string {
	base := filepath.Base(name)
	switch base {
	case "rm", "git", "kubectl", "terraform", "helm", "docker", "aws", "gcloud",
		"dd", "mkfs", "fdisk", "parted", "chmod", "chown", "npm", "pip", "cargo":
		return base
	}
	if strings.HasPrefix(base, "mkfs.") {
		return base
	}
	return name
}
