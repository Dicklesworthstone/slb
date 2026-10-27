package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Dicklesworthstone/slb/internal/audit"
	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/daemon"
	"github.com/Dicklesworthstone/slb/internal/db"
	shellwords "github.com/mattn/go-shellwords"
	"github.com/spf13/cobra"
)

const maxHookGuardInputBytes = 1 << 20

var flagHookPolicyProject string

var hookRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
var hookCommandHashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var hookPythonExecutablePattern = regexp.MustCompile(`^python(?:[0-9]+(?:\.[0-9]+)*)?$`)

func init() {
	hookGuardCmd.Flags().StringVar(&flagHookPolicyProject, "policy-project", "", "physical project owning an installed explicit hook policy")
	hookCmd.AddCommand(hookGuardCmd)
}

// hookGuardCmd is the low-latency native Claude Code PreToolUse entrypoint.
// It intentionally bypasses normal CLI formatting: stdout is the hook protocol.
var hookGuardCmd = &cobra.Command{
	Use:    "guard",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE:   runHookGuard,
}

func runHookGuard(cmd *cobra.Command, _ []string) error {
	input, err := readHookGuardInput(cmd.InOrStdin())
	if err != nil {
		return emitHookDecision(cmd, "ask", "SLB: invalid hook input; unable to inspect the command.")
	}
	toolInput, ok := input["tool_input"].(map[string]any)
	if !ok {
		return emitHookDecision(cmd, "ask", "SLB: invalid hook input; unable to inspect the command.")
	}
	command, ok := toolInput["command"].(string)
	if !ok {
		return emitHookDecision(cmd, "ask", "SLB: missing or invalid command; unable to inspect it.")
	}
	if command == "" {
		return emitHookDecision(cmd, "allow", "")
	}

	cwd, err := hookGuardCWD(input)
	if err != nil {
		return emitHookDecision(cmd, "ask", "SLB: invalid working directory; unable to inspect the command.")
	}
	sessionID := strings.TrimSpace(os.Getenv("SLB_SESSION_ID"))
	if sessionID == "" {
		if providerSession, ok := input["session_id"].(string); ok {
			sessionID = providerSession
		}
	}
	policyOptions, err := nativeHookPolicyOptions(cwd)
	if err == nil && policyOptions.ConfigPath != "" {
		// An online daemon must not hide a missing or invalid explicitly
		// selected file. The same file will be checked again by the executor.
		_, err = config.Load(policyOptions)
	}
	if err != nil {
		return decideAndAuditNativeHook(cmd, command, sessionID, cwd, "block",
			"SLB: selected hook policy could not be loaded; command blocked until policy is repaired.",
			"unknown", 0, "policy_load_error", "hook_native_policy", false, "")
	}

	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	queryTimeout := hookQueryTimeout(cwd)
	queryCtx, cancel := context.WithTimeout(parent, queryTimeout)
	client := daemon.NewIPCClient(daemon.SocketPathForCWD(cwd))
	live, liveErr := client.HookQuery(queryCtx, daemon.HookQueryParams{
		Command: command, SessionID: sessionID, CWD: cwd, ExecutionHandoff: true,
		ConfigPath: policyOptions.ConfigPath,
	})
	cancel()
	_ = client.Close()
	if liveErr == nil {
		if live == nil || (policyOptions.ConfigPath != "" && !nativeHookPolicyAcknowledged(live, cwd, policyOptions.ConfigPath)) {
			recorded := live != nil && live.AuditRecorded && live.Action == "block" && live.MatchedPattern == "policy_config_mismatch"
			return decideAndAuditNativeHook(cmd, command, sessionID, cwd, "block",
				"SLB: daemon did not acknowledge the hook's selected policy; restart it with the same --config before retrying.",
				"unknown", 0, "policy_config_mismatch", "hook_native_daemon", recorded, "")
		}
		if live.Action == "execute" {
			if err := emitNativeExecutionHandoff(cmd, live, toolInput, sessionID, cwd, policyOptions.ConfigPath); err == nil {
				return nil
			}
			return decideAndAuditNativeHook(cmd, command, sessionID, cwd, "block",
				"SLB: execution handoff unavailable or invalid; use slb execute explicitly.",
				live.Tier, live.MinApprovals, live.MatchedPattern, "hook_native_daemon", live.AuditRecorded, live.ApprovalPolicy)
		}
		return decideAndAuditNativeHook(cmd, command, sessionID, cwd, live.Action, live.Message,
			live.Tier, live.MinApprovals, live.MatchedPattern, "hook_native_daemon", live.AuditRecorded, live.ApprovalPolicy)
	}

	// The local fallback stays fail-closed whatever the cause. A deadline on a
	// daemon that is running is recorded distinctly from an unreachable
	// daemon, so a slow daemon can be told apart from a missing one.
	source := "hook_native_offline"
	timedOut := errors.Is(liveErr, context.DeadlineExceeded)
	if timedOut {
		source = "hook_native_timeout"
	}
	fallback, err := classifyNativeHookFallback(command, cwd)
	if err != nil {
		return decideAndAuditNativeHook(cmd, command, sessionID, cwd, "block",
			"SLB: classification failed; command blocked until policy is available.",
			"unknown", 0, "policy_load_error", source, false, "")
	}
	message := fallback.Message
	if timedOut && fallback.Action != "allow" {
		message = fmt.Sprintf("SLB: daemon did not answer within %s (integrations.hook_query_timeout_ms); used local policy. %s",
			queryTimeout, message)
	}
	return decideAndAuditNativeHook(cmd, command, sessionID, cwd, fallback.Action, message,
		fallback.Tier, fallback.MinApprovals, fallback.MatchedPattern, source, false, fallback.ApprovalPolicy)
}

// hookQueryTimeout is the configured daemon-query deadline for cwd's physical project.
// Config errors fall back to the default: the offline path loads the same
// config and fails closed on it.
func hookQueryTimeout(cwd string) time.Duration {
	ms := config.DefaultHookQueryTimeoutMS
	if opts, err := nativeHookPolicyOptions(cwd); err == nil {
		if cfg, err := config.Load(opts); err == nil {
			ms = cfg.Integrations.HookQueryTimeoutMS
		}
	}
	if ms < config.MinHookQueryTimeoutMS || ms > config.MaxHookQueryTimeoutMS {
		ms = config.DefaultHookQueryTimeoutMS
	}
	return time.Duration(ms) * time.Millisecond
}

// Installed overrides belong to one physical project, even when the hook is
// registered globally. A nested initialized project keeps its own policy.
// Direct --config calls use the normal CLI project selection as their scope.
func nativeHookPolicyOptions(cwd string) (config.LoadOptions, error) {
	root, err := daemon.PhysicalProjectRootForCWD(cwd)
	if err != nil {
		return config.LoadOptions{}, err
	}
	opts := config.LoadOptions{ProjectDir: root}
	if flagConfig == "" {
		if flagHookPolicyProject != "" {
			return opts, errors.New("hook policy project requires --config")
		}
		return opts, nil
	}
	owner := flagHookPolicyProject
	if owner == "" {
		owner, err = projectPath()
		if err != nil {
			return opts, err
		}
		owner, err = daemon.PhysicalProjectRootForCWD(owner)
	} else if !filepath.IsAbs(owner) || strings.ContainsRune(owner, 0) {
		return opts, errors.New("hook policy project must be absolute")
	} else {
		owner, err = filepath.EvalSymlinks(owner)
	}
	if err != nil {
		return opts, err
	}
	if root != owner {
		return opts, nil
	}
	selected, err := filepath.Abs(flagConfig)
	if err != nil {
		return opts, err
	}
	if info, err := os.Stat(selected); err != nil {
		return opts, err
	} else if !info.Mode().IsRegular() {
		return opts, errors.New("selected hook policy is not a regular file")
	}
	opts.ConfigPath = selected
	return opts, nil
}

func nativeHookPolicyAcknowledged(response *daemon.HookQueryResult, cwd, expected string) bool {
	actual, err := hookExecutionConfig(cwd, response.ConfigPath, response.ConfigProject)
	if err != nil || actual == "" {
		return false
	}
	left, err := os.Stat(expected)
	if err != nil {
		return false
	}
	right, err := os.Stat(actual)
	return err == nil && os.SameFile(left, right)
}

func readHookGuardInput(r io.Reader) (map[string]any, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxHookGuardInputBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxHookGuardInputBytes {
		return nil, fmt.Errorf("hook input exceeds %d bytes", maxHookGuardInputBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var input map[string]any
	if err := decoder.Decode(&input); err != nil {
		return nil, err
	}
	if input == nil {
		return nil, fmt.Errorf("hook input must be an object")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("hook input contains trailing JSON")
	}
	return input, nil
}

func hookGuardCWD(input map[string]any) (string, error) {
	if raw, exists := input["cwd"]; exists {
		cwd, ok := raw.(string)
		if !ok || cwd == "" || strings.ContainsRune(cwd, 0) || !filepath.IsAbs(cwd) {
			return "", fmt.Errorf("invalid cwd")
		}
		return filepath.Clean(cwd), nil
	}
	cwd, err := os.Getwd()
	if err != nil || !filepath.IsAbs(cwd) {
		return "", fmt.Errorf("working directory unavailable")
	}
	return filepath.Clean(cwd), nil
}

func classifyNativeHookFallback(command, cwd string) (*daemon.HookQueryResult, error) {
	opts, err := nativeHookPolicyOptions(cwd)
	if err != nil {
		return nil, err
	}
	root := opts.ProjectDir
	cfg, err := config.Load(opts)
	if err != nil {
		return nil, err
	}

	var reader core.PolicyReader
	var connection *db.DB
	stateDB := filepath.Join(root, ".slb", "state.db")
	if info, statErr := os.Stat(stateDB); statErr == nil {
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("project state database is not a regular file")
		}
		connection, err = db.OpenWithOptions(stateDB, db.OpenOptions{
			CreateIfNotExists: false, InitSchema: false, ReadOnly: true,
		})
		if err != nil {
			return nil, err
		}
		defer connection.Close()
		reader = connection
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return nil, statErr
	} else if _, markerErr := os.Stat(filepath.Join(root, ".slb")); markerErr == nil {
		// An initialized project's missing database is unavailable policy, not
		// permission to discard its custom rules and use defaults.
		return nil, statErr
	} else if !os.IsNotExist(markerErr) {
		return nil, markerErr
	}
	engine, err := core.LoadCommandPolicy(reader, opts)
	if err != nil {
		return nil, err
	}
	classified := engine.ClassifyCommand(command, cwd)
	result := &daemon.HookQueryResult{
		Tier: string(classified.Tier), MatchedPattern: classified.MatchedPattern,
		MinApprovals:   classified.MinApprovals,
		ApprovalPolicy: db.ApprovalPolicy(cfg.General.ApprovalPolicy),
		ReviewNotice:   db.ApprovalPolicy(cfg.General.ApprovalPolicy).Label(),
	}
	switch {
	case classified.IsSafe:
		result.Action, result.Message = "allow", "Safe command"
	case classified.Tier == core.RiskTierCritical:
		result.Action = "block"
		result.Message = fmt.Sprintf("SLB CRITICAL: Requires %d approvals. Use 'slb request' to submit.", classified.MinApprovals)
	case classified.Tier == core.RiskTierDangerous:
		result.Action = "block"
		result.Message = fmt.Sprintf("SLB DANGEROUS: Requires %d approval. Use 'slb request' to submit.", classified.MinApprovals)
	case classified.Tier == core.RiskTierCaution:
		if cfg.Integrations.HookCautionAction == "ask" {
			result.Action, result.Message = "ask", "SLB CAUTION: command requires confirmation. Proceed?"
		} else {
			result.Action = "block"
			result.Message = "SLB CAUTION: submit with 'slb request'; configured auto-approval policy applies after admission."
		}
	default:
		result.Action = "ask"
		result.Tier = "unknown"
		result.Message = "SLB OFFLINE: command is not covered by policy; confirmation required while the daemon is unavailable."
	}
	if result.Action != "allow" {
		result.Message += " Review policy: " + result.ReviewNotice + "."
	}
	return result, nil
}

func decideAndAuditNativeHook(cmd *cobra.Command, command, sessionID, cwd, action, message, tier string,
	minApprovals int, matchedPattern, source string, alreadyRecorded bool, policy db.ApprovalPolicy,
) error {
	action = normalizedNativeHookAction(action)
	if action != "allow" && !alreadyRecorded {
		directory, err := audit.DefaultDirectory()
		if err == nil {
			notice := ""
			if policy != "" {
				notice = policy.Label()
			}
			err = audit.Record(directory, audit.Event{
				CommandRedacted: core.ApplyRedaction(command, nil),
				CommandHash:     audit.CommandHash(command, cwd), CWD: cwd, SessionID: sessionID,
				Action: action, Tier: tier, MatchedPattern: matchedPattern,
				MinApprovals: minApprovals, Source: source,
				ApprovalPolicy: string(policy), ReviewNotice: notice,
			})
		}
		if err != nil {
			message += " Audit recording failed; safety decision unchanged."
		}
	}
	return emitHookDecision(cmd, action, message)
}

func normalizedNativeHookAction(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "allow":
		return "allow"
	case "block", "deny":
		return "block"
	default:
		return "ask"
	}
}

func emitHookDecision(cmd *cobra.Command, action, message string) error {
	action = normalizedNativeHookAction(action)
	permission := action
	if action == "block" {
		permission = "deny"
	}
	specific := map[string]any{
		"hookEventName": "PreToolUse", "permissionDecision": permission,
	}
	if message != "" && permission != "allow" {
		specific["permissionDecisionReason"] = message
	}
	return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"hookSpecificOutput": specific})
}

func emitNativeExecutionHandoff(cmd *cobra.Command, response *daemon.HookQueryResult, toolInput map[string]any,
	sessionID, cwd, selectedConfig string,
) error {
	handoff := response.ExecutionHandoff
	if handoff == nil || !hookRequestIDPattern.MatchString(handoff.RequestID) ||
		!hookCommandHashPattern.MatchString(handoff.CommandHash) || sessionID == "" ||
		handoff.SessionID != sessionID {
		return fmt.Errorf("invalid execution handoff")
	}
	databasePath, logDirectory, err := hookExecutionPaths(cwd, handoff.DatabasePath)
	if err != nil {
		return err
	}
	configPath, err := hookExecutionConfig(cwd, handoff.ConfigPath, handoff.ConfigProject)
	if err != nil {
		return err
	}
	if (configPath != "" || response.ConfigPath != "" || response.ConfigProject != "") && !nativeHookPolicyAcknowledged(response, cwd, configPath) {
		return errors.New("execution handoff policy differs from daemon classification")
	}
	if selectedConfig != "" {
		selectedConfig, err = hookExecutionConfig(cwd, selectedConfig, handoff.ConfigProject)
		if err != nil || !nativeHookPolicyAcknowledged(response, cwd, selectedConfig) {
			return errors.New("execution handoff policy differs from selected hook policy")
		}
		// Preserve the hook's selected filename, even when the daemon names the
		// same file through another alias. The executor must reload this source
		// so replacing or retargeting it can still revoke the reviewed command.
		configPath = selectedConfig
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return err
	}
	args := []string{
		executable, "execute", "--db", databasePath,
		"--log-dir", logDirectory,
		"--session-id", sessionID, "--expected-command-hash", handoff.CommandHash, "--json",
	}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	if rawTimeout, ok := toolInput["timeout"]; ok {
		if timeout, ok := hookJSONPositiveInt(rawTimeout); ok {
			seconds := timeout / 1000
			if seconds < 1 {
				seconds = 1
			}
			args = append(args, "--timeout", fmt.Sprintf("%d", seconds))
		}
	}
	args = append(args, "--", handoff.RequestID)

	updated := make(map[string]any, len(toolInput))
	for key, value := range toolInput {
		updated[key] = value
	}
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = hookShellQuote(arg)
	}
	updated["command"] = "exec " + strings.Join(quoted, " ")
	return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName": "PreToolUse", "permissionDecision": "allow",
			"updatedInput":      updated,
			"additionalContext": "SLB is executing the reviewed request once and recording its outcome. Review policy: " + handoff.ApprovalPolicy.Label() + ".",
		},
	})
}

func hookExecutionConfig(cwd, configPath, configProject string) (string, error) {
	if configPath == "" {
		if configProject != "" {
			return "", errors.New("hook policy scope has no config path")
		}
		return "", nil
	}
	if !filepath.IsAbs(configPath) || !filepath.IsAbs(configProject) || strings.ContainsRune(configPath+configProject, 0) {
		return "", errors.New("hook policy paths must be absolute")
	}
	root, err := daemon.PhysicalProjectRootForCWD(cwd)
	if err != nil {
		return "", err
	}
	owner, err := filepath.EvalSymlinks(configProject)
	if err != nil || owner != root {
		return "", errors.New("hook policy belongs to a different project")
	}
	if info, err := os.Stat(configPath); err != nil {
		return "", err
	} else if !info.Mode().IsRegular() {
		return "", errors.New("hook policy config is not a regular file")
	}
	return filepath.Clean(configPath), nil
}

func hookExecutionPaths(cwd, databasePath string) (string, string, error) {
	root, err := daemon.PhysicalProjectRootForCWD(cwd)
	if err != nil {
		return "", "", err
	}
	if !filepath.IsAbs(databasePath) {
		return "", "", fmt.Errorf("execution handoff database path must be absolute")
	}
	stateDirectory, err := filepath.EvalSymlinks(filepath.Join(root, ".slb"))
	if err != nil {
		return "", "", fmt.Errorf("resolving project state directory: %w", err)
	}
	expected, err := filepath.EvalSymlinks(filepath.Join(stateDirectory, "state.db"))
	if err != nil {
		return "", "", fmt.Errorf("resolving project database: %w", err)
	}
	actual, err := filepath.EvalSymlinks(databasePath)
	if err != nil || actual != expected {
		return "", "", fmt.Errorf("execution handoff project mismatch")
	}
	info, err := os.Stat(expected)
	if err != nil {
		return "", "", err
	}
	if !info.Mode().IsRegular() {
		return "", "", fmt.Errorf("execution handoff database is not a regular file")
	}
	// Use the validated physical path for execution rather than following the
	// supplied alias again. Logs remain in the project's state directory even
	// when only state.db is a symlink. The request retains its literal identity.
	return expected, filepath.Join(stateDirectory, "logs"), nil
}

func hookJSONPositiveInt(value any) (int, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := number.Int64()
	if err != nil || parsed <= 0 || parsed > int64(^uint(0)>>1) {
		return 0, false
	}
	return int(parsed), true
}

func nativeHookGuardCommand(configPath, configProject string) (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return "", err
	}
	command := hookShellQuote(executable) + " hook guard"
	if configPath != "" {
		if !filepath.IsAbs(configPath) || !filepath.IsAbs(configProject) || strings.ContainsRune(configPath+configProject, 0) {
			return "", errors.New("installed hook policy paths must be absolute")
		}
		command += " --config " + hookShellQuote(configPath) + " --policy-project " + hookShellQuote(configProject)
	} else if configProject != "" {
		return "", errors.New("installed hook policy project requires a config file")
	}
	return command, nil
}

func isSLBHookCommand(command, generatedScript string) bool {
	// A composite hook may also run another tool. Do not take ownership of
	// multiline commands or the first command of a pipeline/command list.
	if strings.ContainsAny(command, "\r\n") {
		return false
	}
	parser := shellwords.NewParser()
	parser.ParseEnv = false
	parser.ParseBacktick = false
	tokens, err := parser.Parse(command)
	if err != nil || len(tokens) == 0 || parser.Position >= 0 {
		return false
	}
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(tokens[0])), ".exe")
	if len(tokens) >= 3 && tokens[1] == "hook" && tokens[2] == "guard" {
		if base == "slb" {
			return true
		}
		if current, currentErr := os.Executable(); currentErr == nil {
			if currentAbs, absErr := filepath.Abs(current); absErr == nil {
				if tokenAbs, tokenErr := filepath.Abs(tokens[0]); tokenErr == nil && filepath.Clean(tokenAbs) == filepath.Clean(currentAbs) {
					return true
				}
			}
		}
	}
	if generatedScript == "" {
		return false
	}
	scriptName := filepath.Base(generatedScript)
	if filepath.Base(tokens[0]) == scriptName {
		return true
	}
	if !hookPythonExecutablePattern.MatchString(base) {
		return false
	}
	for i := 1; i < len(tokens); i++ {
		switch tokens[i] {
		case "--":
			return i+1 < len(tokens) && filepath.Base(tokens[i+1]) == scriptName
		case "-B", "-E", "-I", "-O", "-OO", "-q", "-s", "-S", "-u", "-v":
			// These interpreter options do not consume another argument.
			continue
		default:
			// Do not mistake -c code, -m modules, other scripts' arguments,
			// or option operands for our generated script.
			return !strings.HasPrefix(tokens[i], "-") && filepath.Base(tokens[i]) == scriptName
		}
	}
	return false
}

func hookShellQuote(value string) string {
	if value == "" {
		return "''"
	}
	if !strings.ContainsAny(value, " \t\r\n'\"\\$&;|<>*?()[]{}") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
