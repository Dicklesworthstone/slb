// Package core implements dry-run pre-flight checks for supported commands.
package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/mattn/go-shellwords"
)

const defaultDryRunTimeout = 30 * time.Second

// GetDryRunCommand returns a shell-safe dry-run variant of cmd when supported.
// The second return value is false when no dry-run variant is available.
func GetDryRunCommand(cmd string) (string, bool) {
	tokens, ok := getDryRunTokens(cmd)
	if !ok {
		return "", false
	}
	return shellJoin(tokens), true
}

// RunDryRun executes a dry-run variant for spec when supported.
// If the command type is unsupported, it returns (nil, nil).
func RunDryRun(spec *db.CommandSpec) (*db.DryRunResult, error) {
	return RunDryRunContext(context.Background(), spec)
}

// RunDryRunContext propagates caller cancellation into the bounded preview.
// External preview tools use the caller's environment and configuration; this
// is advisory evidence, not a sandbox or an authorization to run the command.
func RunDryRunContext(parent context.Context, spec *db.CommandSpec) (*db.DryRunResult, error) {
	if err := parent.Err(); err != nil {
		return nil, err
	}
	if spec == nil {
		return nil, fmt.Errorf("spec is required")
	}
	if strings.TrimSpace(spec.Raw) == "" {
		return nil, fmt.Errorf("command is required")
	}

	tokens, ok := getDryRunTokens(spec.Raw)
	if !spec.Shell && len(spec.Argv) > 0 {
		// Argv is authoritative for non-shell execution. Do not reinterpret a
		// display string and preview different arguments from those approved.
		tokens, ok = transformDryRunTokens(spec.Argv)
	}
	if !ok {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(parent, defaultDryRunTimeout)
	defer cancel()

	out, err := runDryRunProcess(ctx, tokens, spec.Cwd)
	return &db.DryRunResult{
		Command: shellJoin(tokens),
		Output:  out,
	}, err
}

// runDryRunProcess never supplies stdin or invokes a shell to interpret argv.
// It bounds both memory and inherited-pipe waits, and stops ordinary Unix
// descendants on cancellation just like approved command execution does.
func runDryRunProcess(ctx context.Context, tokens []string, cwd string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(tokens) == 0 || tokens[0] == "" {
		return "", fmt.Errorf("dry-run command is required")
	}
	ctx, stopSignals := signal.NotifyContext(ctx, commandSignals()...)
	defer stopSignals()

	cmd := exec.CommandContext(ctx, tokens[0], tokens[1:]...)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	cmd.WaitDelay = time.Second
	stdout := &outputCapture{limit: maxCapturedOutputBytes}
	stderr := &outputCapture{limit: maxCapturedOutputBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	stopGroup := configureCommandCancellation(cmd, false)

	err := cmd.Run()
	var stopErr error
	if err != nil && stopGroup != nil {
		stopErr = stopGroup()
		if errors.Is(stopErr, os.ErrProcessDone) {
			stopErr = nil
		}
	}
	if err != nil {
		var exitErr *exec.ExitError
		switch {
		case ctx.Err() != nil:
			err = ctx.Err()
		case errors.As(err, &exitErr):
			err = fmt.Errorf("dry-run exited with code %d: %w", exitErr.ExitCode(), err)
		default:
			err = fmt.Errorf("dry-run failed: %w", err)
		}
	}
	if stopErr != nil {
		err = errors.Join(err, fmt.Errorf("stopping dry-run process group: %w", stopErr))
	}
	return combineStdoutStderr(stdout.String(), stderr.String()), err
}

func getDryRunTokens(raw string) ([]string, bool) {
	tokens, ok := parsePreviewTokens(raw, 0)
	if !ok {
		return nil, false
	}
	return transformDryRunTokens(tokens)
}

// parsePreviewTokens preserves argv boundaries. Display normalization loses
// quoting and must never be reparsed for pre-approval execution. We support
// only literal commands: no expansion, redirection, pipelines, or partial
// parsing, and no context-changing wrapper options or environment assignments.
func parsePreviewTokens(raw string, depth int) ([]string, bool) {
	if depth > 8 || !hasLiteralPreviewSyntax(raw) {
		return nil, false
	}
	parser := shellwords.NewParser()
	parser.ParseEnv = false
	parser.ParseBacktick = false
	tokens, err := parser.Parse(raw)
	if err != nil || parser.Position >= 0 || len(tokens) == 0 {
		return nil, false
	}
	for len(tokens) > 0 {
		switch tokens[0] {
		case "bash", "sh", "zsh", "ksh", "dash":
			if len(tokens) != 3 || tokens[1] != "-c" {
				return nil, false
			}
			return parsePreviewTokens(tokens[2], depth+1)
		case "sudo", "doas", "nohup", "command", "env":
			// Bare privilege wrappers retain the existing unprivileged preview
			// behavior. Options, assignments, and changed users are not guessed.
			tokens = tokens[1:]
			if len(tokens) == 0 || strings.HasPrefix(tokens[0], "-") || strings.Contains(tokens[0], "=") {
				return nil, false
			}
		default:
			return tokens, true
		}
	}
	return nil, false
}

func hasLiteralPreviewSyntax(raw string) bool {
	var single, double, escaped bool
	wordStart := true
	for _, r := range raw {
		if r == 0 {
			return false
		}
		if escaped {
			// The tokenizer and POSIX shells differ for these double-quoted
			// escapes and line continuations. Decline rather than change argv.
			if r == '\n' || r == '\r' || (double && !strings.ContainsRune("$`\"\\", r)) {
				return false
			}
			escaped = false
			wordStart = false
			continue
		}
		if single {
			wordStart = false
			if r == '\'' {
				single = false
			}
			continue
		}
		if r == '\\' {
			escaped = true
			wordStart = false
			continue
		}
		if r == '"' {
			double = !double
			wordStart = false
			continue
		}
		if r == '\'' && !double {
			single = true
			wordStart = false
			continue
		}
		if r == '$' || r == '`' {
			return false
		}
		if !double && strings.ContainsRune(";|&<>\n\r(){}*?[]", r) {
			return false
		}
		// Tildes and hashes inside a word are literal (HEAD~1, file#name).
		// Only an unquoted word-start tilde/comment changes shell semantics.
		if !double && wordStart && (r == '~' || r == '#') {
			return false
		}
		wordStart = !double && (r == ' ' || r == '\t')
	}
	return !single && !double && !escaped
}

func transformDryRunTokens(tokens []string) ([]string, bool) {
	if len(tokens) == 0 {
		return nil, false
	}
	switch tokens[0] {
	case "kubectl":
		return dryRunKubectl(tokens)
	case "terraform":
		return dryRunTerraform(tokens)
	case "rm":
		return dryRunRM(tokens)
	case "git":
		preview, ok := dryRunGit(tokens)
		if !ok {
			return nil, false
		}
		// Git's normal diff presentation can execute repository-configured
		// external diff/textconv programs. Suppress those at the common
		// preview boundary for both raw commands and authoritative argv.
		return append([]string{"git", "diff", "--no-ext-diff", "--no-textconv"}, preview[2:]...), true
	case "helm":
		return dryRunHelm(tokens)
	default:
		return nil, false
	}
}

func parseShellTokens(cmd string) []string {
	parser := shellwords.NewParser()
	tokens, err := parser.Parse(cmd)
	if err == nil {
		return tokens
	}
	return strings.Fields(cmd)
}

func dryRunKubectl(tokens []string) ([]string, bool) {
	if len(tokens) < 2 || tokens[1] != "delete" {
		return nil, false
	}

	// This command runs BEFORE approval. Never trust a supplied dry-run mode:
	// "none" (or older "false") performs the deletion. Put our flag first so
	// neither a preceding value-taking option nor "--" can swallow it, and
	// remove every later override. Never mutate the requested argv.
	out := []string{tokens[0], "delete", "--dry-run=client"}
	args := make([]string, 0, len(tokens)-2)
	var operands []string
	for i := 2; i < len(tokens); i++ {
		token := tokens[i]
		if token == "--" {
			operands = tokens[i:]
			break
		}
		switch {
		case token == "--raw" || strings.HasPrefix(token, "--raw="):
			// Raw API deletion is not an object-level client-side preview.
			return nil, false
		case token == "--dry-run":
			// Accept explicit separated modes as well as the bare flag, but do
			// not consume a resource type/name as if it were a mode.
			if i+1 < len(tokens) {
				switch tokens[i+1] {
				case "client", "server", "none", "true", "false":
					i++
				}
			}
		case strings.HasPrefix(token, "--dry-run="):
			// Replace all requested modes with the enforced client-side mode.
		case strings.HasPrefix(token, "--dry-run"):
			return nil, false
		default:
			args = append(args, token)
		}
	}
	if !hasFlag(args, "-o") && !hasFlagPrefix(args, "-o=") &&
		!hasFlag(args, "--output") && !hasFlagPrefix(args, "--output=") {
		out = append(out, "-o", "yaml")
	}
	out = append(out, args...)
	out = append(out, operands...)
	return out, true
}

func dryRunTerraform(tokens []string) ([]string, bool) {
	verb := 1
	out := []string{"terraform"}
	if len(tokens) > 1 && strings.HasPrefix(tokens[1], "-chdir=") {
		if tokens[1] == "-chdir=" {
			return nil, false
		}
		out = append(out, tokens[1])
		verb++
	}
	if len(tokens) <= verb || tokens[verb] != "destroy" {
		return nil, false
	}
	out = append(out, "plan", "-destroy", "-input=false")
	for _, arg := range tokens[verb+1:] {
		flag, _, _ := strings.Cut(arg, "=")
		switch flag {
		case "-auto-approve", "-input":
			// Destroy-only approval and interactive input must not break an
			// unattended plan. The preview always disables interactive input.
			continue
		case "-out", "-state-out", "-backup", "-destroy", "-refresh-only":
			// Never generate a plan that writes a requested output file or
			// changes planning mode behind the reviewer's back.
			return nil, false
		default:
			out = append(out, arg)
		}
	}
	return out, true
}

func dryRunRM(tokens []string) ([]string, bool) {
	if len(tokens) < 2 {
		return nil, false
	}
	paths := rmTargets(tokens[1:])
	if len(paths) == 0 {
		return nil, false
	}
	out := []string{"ls", "-la", "--"}
	out = append(out, paths...)
	return out, true
}

func dryRunGit(tokens []string) ([]string, bool) {
	if len(tokens) < 2 || tokens[1] != "reset" {
		return nil, false
	}

	target := ""
	for i := 2; i < len(tokens); i++ {
		if tokens[i] == "--hard" {
			continue
		}
		if strings.HasPrefix(tokens[i], "-") {
			continue
		}
		target = tokens[i]
		break
	}
	if target == "" {
		return nil, false
	}

	return []string{"git", "diff", fmt.Sprintf("%s..HEAD", target)}, true
}

func dryRunHelm(tokens []string) ([]string, bool) {
	var scope []string
	release := ""
	seenUninstall := false
	for i := 1; i < len(tokens); i++ {
		arg := tokens[i]
		if !seenUninstall && arg == "uninstall" {
			seenUninstall = true
			continue
		}
		flag, value, equals := strings.Cut(arg, "=")
		if strings.HasPrefix(arg, "-n") && !strings.HasPrefix(arg, "--") && len(arg) > 2 && !equals {
			flag, value, equals = "-n", arg[2:], true
		}
		switch flag {
		case "-n", "--namespace", "--kube-context", "--kubeconfig", "--kube-apiserver", "--kube-ca-file", "--kube-token", "--kube-as-user", "--kube-as-group", "--kube-tls-server-name":
			if !equals {
				if i+1 >= len(tokens) || strings.HasPrefix(tokens[i+1], "-") {
					return nil, false
				}
				i++
				value = tokens[i]
			}
			if value == "" {
				return nil, false
			}
			scope = append(scope, flag+"="+value)
		case "--kube-insecure-skip-tls-verify", "--debug":
			if equals && value != "true" && value != "false" {
				return nil, false
			}
			scope = append(scope, arg)
		case "--wait", "--no-hooks", "--keep-history", "--ignore-not-found", "--dry-run":
			if !seenUninstall || (equals && value != "true" && value != "false") {
				return nil, false
			}
		case "--timeout", "--cascade", "--description":
			if !seenUninstall {
				return nil, false
			}
			if !equals {
				if i+1 >= len(tokens) || strings.HasPrefix(tokens[i+1], "-") {
					return nil, false
				}
				i++
			}
		default:
			if !seenUninstall || strings.HasPrefix(arg, "-") || release != "" || arg == "" {
				// Do not preview one release out of a multi-release uninstall,
				// or silently discard an unrecognized scoping option.
				return nil, false
			}
			release = arg
		}
	}
	if !seenUninstall || release == "" {
		return nil, false
	}
	return append([]string{"helm", "get", "manifest", release}, scope...), true
}

func rmTargets(args []string) []string {
	var out []string
	seenDashDash := false
	for _, a := range args {
		if a == "--" {
			seenDashDash = true
			continue
		}
		if !seenDashDash && strings.HasPrefix(a, "-") {
			continue
		}
		out = append(out, a)
	}
	return out
}

func hasFlag(tokens []string, flag string) bool {
	for _, t := range tokens {
		if t == flag {
			return true
		}
	}
	return false
}

func hasFlagPrefix(tokens []string, prefix string) bool {
	for _, t := range tokens {
		if strings.HasPrefix(t, prefix) {
			return true
		}
	}
	return false
}

func combineStdoutStderr(stdout, stderr string) string {
	stdout = strings.TrimRight(stdout, "\n")
	stderr = strings.TrimRight(stderr, "\n")

	if stdout == "" && stderr == "" {
		return ""
	}
	if stderr == "" {
		return stdout
	}
	if stdout == "" {
		return stderr
	}
	return stdout + "\n--- stderr ---\n" + stderr
}

func shellJoin(tokens []string) string {
	parts := make([]string, 0, len(tokens))
	for _, t := range tokens {
		parts = append(parts, shellQuote(t))
	}
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\r\n'\"\\$&;|<>*?()[]{}") {
		return s
	}
	// POSIX-ish single-quote escaping: close/open around an escaped quote.
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
