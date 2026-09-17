// Package core implements dry-run pre-flight checks for supported commands.
package core

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
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
	if spec == nil {
		return nil, fmt.Errorf("spec is required")
	}
	if strings.TrimSpace(spec.Raw) == "" {
		return nil, fmt.Errorf("command is required")
	}

	tokens, ok := getDryRunTokens(spec.Raw)
	if !ok {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultDryRunTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, tokens[0], tokens[1:]...)
	if spec.Cwd != "" {
		cmd.Dir = spec.Cwd
	}
	cmd.Env = os.Environ()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	out := combineStdoutStderr(stdout.String(), stderr.String())

	res := &db.DryRunResult{
		Command: shellJoin(tokens),
		Output:  out,
	}

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return res, context.DeadlineExceeded
		}
		if ee, ok := err.(*exec.ExitError); ok {
			return res, fmt.Errorf("dry-run exited with code %d", ee.ExitCode())
		}
		return res, fmt.Errorf("dry-run failed: %w", err)
	}

	return res, nil
}

func getDryRunTokens(raw string) ([]string, bool) {
	normalized := NormalizeCommand(raw)
	// A preview must not silently describe only the first command in a
	// pipeline/list, or guess at arguments after a parse failure. Classification
	// can conservatively recover from those inputs; pre-approval execution cannot.
	if normalized.ParseError || normalized.IsCompound || normalized.HasSubshell {
		return nil, false
	}
	cmd := strings.TrimSpace(normalized.Primary)
	if cmd == "" {
		cmd = strings.TrimSpace(raw)
	}

	parser := shellwords.NewParser()
	parser.ParseEnv = false
	parser.ParseBacktick = false
	tokens, err := parser.Parse(cmd)
	if err != nil || len(tokens) == 0 {
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
		return dryRunGit(tokens)
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
	if len(tokens) < 2 || tokens[1] != "destroy" {
		return nil, false
	}
	out := []string{"terraform", "plan", "-destroy"}
	out = append(out, tokens[2:]...)
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
	if len(tokens) < 3 || tokens[1] != "uninstall" {
		return nil, false
	}
	release := tokens[2]
	return []string{"helm", "get", "manifest", release}, true
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
