package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/daemon"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

var (
	flagHookGlobal          bool
	flagHookMerge           bool
	flagHookForce           bool
	flagHookOutputDir       string
	flagHookLocalOnly       bool
	flagHookSimulateFailure bool
)

func init() {
	// hook install flags
	hookInstallCmd.Flags().BoolVarP(&flagHookGlobal, "global", "g", false, "install globally for all projects")
	hookInstallCmd.Flags().BoolVar(&flagHookMerge, "merge", true, "preserve existing hooks (default)")
	hookInstallCmd.Flags().BoolVarP(&flagHookForce, "force", "f", false, "reset SLB's own hook entry to its canonical form (other hooks are never touched)")
	hookStatusCmd.Flags().BoolVarP(&flagHookGlobal, "global", "g", false, "inspect user-level settings for all projects")
	hookUninstallCmd.Flags().BoolVarP(&flagHookGlobal, "global", "g", false, "remove the hook from user-level settings for all projects")

	// hook generate flags.
	// Named --output-dir (not --output): the persistent --output/-o is the
	// output FORMAT (text/json/yaml/toon). A local --output here would shadow
	// that persistent flag entirely, making `slb hook generate -o json` write
	// to a directory literally named "json" instead of emitting JSON.
	hookGenerateCmd.Flags().StringVar(&flagHookOutputDir, "output-dir", "", "output directory (default: ~/.slb/hooks/)")
	hookTestCmd.Flags().BoolVar(&flagHookLocalOnly, "local-only", false, "skip daemon and test embedded/local policy behavior")
	hookTestCmd.Flags().BoolVar(&flagHookSimulateFailure, "simulate-failure", false, "simulate daemon failure and exercise fallback behavior")

	// Add subcommands
	hookCmd.AddCommand(hookGenerateCmd)
	hookCmd.AddCommand(hookInstallCmd)
	hookCmd.AddCommand(hookUninstallCmd)
	hookCmd.AddCommand(hookStatusCmd)
	hookCmd.AddCommand(hookHealthCmd)
	hookCmd.AddCommand(hookTestCmd)

	rootCmd.AddCommand(hookCmd)
}

var hookCmd = &cobra.Command{
	Use:   "hook",
	Short: "Manage Claude Code hook integration",
	Long: `Manage the Claude Code PreToolUse hook that integrates SLB approval workflow.

The hook intercepts Bash tool calls before execution and checks if the command
requires SLB approval. Installed hooks use the native slb binary on the hot
path; the generated Python guard remains available as a standalone fallback
artifact. Dangerous commands are blocked until approved.

Quick start:
  slb hook install    # Generate and install hook
  slb hook status     # Check installation status
  slb hook uninstall  # Remove hook`,
}

var hookGenerateCmd = &cobra.Command{
	Use:   "generate",
	Short: "Generate the Python hook script",
	Long: `Generate the SLB guard hook script with embedded patterns.

The generated script will be written to ~/.slb/hooks/slb_guard.py by default.
Use --output-dir to specify a different directory.

The script includes:
- Embedded pattern matching for offline classification
- Unix socket connection to SLB daemon for approval checks
- Fail-closed behavior when SLB is unavailable`,
	RunE: runHookGenerate,
}

var hookInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install hook into Claude Code settings",
	Long: `Generate the hook script and configure Claude Code to use it.

This command:
1. Generates the standalone fallback script at .slb/hooks/slb_guard.py
2. Configures Claude Code to call the native 'slb hook guard' entrypoint

Only SLB's own hook object in the selected project's .claude/settings.json is
edited (selection: --project/-C, SLB_PROJECT, then the working directory). An existing
SLB registration (native or legacy Python) is updated in place; otherwise the
guard is appended to the existing "Bash" PreToolUse entry, or a new one is
added. Other hooks - including ones sharing the "Bash" entry - other entries,
key order and indentation are preserved, and the file is not rewritten when
nothing changes. --force resets SLB's own hook object (e.g. drops a custom
"timeout") but still leaves every other hook alone.

SLB always runs synchronously with onFailure: block. Claude Code 2.1.295 or later
is required to block tool calls if the guard cannot start, fails, or times out.
Initialize a project with 'slb init' before installing its hook. The project
database must remain available so the native guard can load its complete policy.
Use --global to select ~/.claude/settings.json and ~/.slb/hooks/slb_guard.py.`,
	RunE: runHookInstall,
}

var hookUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove hook from Claude Code settings",
	Long: `Remove the SLB hook from Claude Code settings.

This removes only SLB's own hook objects from settings.json; sibling hooks in
the same matcher entry are kept. It does not delete the hook script file. Use
this if you want to temporarily disable SLB hook integration.

The selected project's settings are used by default (--project/-C, SLB_PROJECT,
then the working directory). Use --global for user-level settings.`,
	RunE: runHookUninstall,
}

var hookStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show hook installation status",
	Long: `Show the current status of the SLB hook integration.

Checks:
- Hook script exists and is executable
- Claude Code settings.json is configured
- SLB daemon is running (for real-time checks)
- Pattern version matches embedded version`,
	RunE: runHookStatus,
}

var hookHealthCmd = &cobra.Command{
	Use:   "health",
	Short: "Check real-time hook daemon health and policy parity",
	Long: `Check whether the project hook daemon is reachable within the hook's
latency budget (integrations.hook_query_timeout_ms, default 250ms) and whether
it is enforcing the same effective policy as the local fallback snapshot source.

An unreachable daemon is reported as degraded-but-fallback-capable rather than
as a command error, because the generated hook is designed to operate offline.`,
	RunE: runHookHealth,
}

var hookTestCmd = &cobra.Command{
	Use:   "test <command>",
	Short: "Test hook behavior for a command",
	Long: `Test what the hook would do for a given command.

This simulates the hook's decision without actually running the command.
Useful for verifying pattern classification and approval logic.

Examples:
  slb hook test "rm -rf node_modules"
  slb hook test "git push --force"`,
	Args: cobra.ExactArgs(1),
	RunE: runHookTest,
}

func runHookGenerate(cmd *cobra.Command, args []string) error {
	// Determine output directory
	outputDir := flagHookOutputDir
	if outputDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("failed to get home directory: %w", err)
		}
		outputDir = filepath.Join(home, ".slb", "hooks")
	}

	// Create directory if needed
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", outputDir, err)
	}

	// Never replace an installed guard with stale or incomplete policy.
	if _, err := loadCustomPatternsIntoDefaultEngine(); err != nil {
		return err
	}

	// Generate hook script
	engine := core.GetDefaultEngine()
	cautionAction, approvalPolicy, err := hookPolicySettings()
	if err != nil {
		return err
	}
	hookScript := generateHookScriptWithPolicy(engine, cautionAction, approvalPolicy)

	// Write script
	scriptPath := filepath.Join(outputDir, "slb_guard.py")
	if err := os.WriteFile(scriptPath, []byte(hookScript), 0755); err != nil {
		return fmt.Errorf("failed to write hook script: %w", err)
	}

	out := output.New(output.Format(GetOutput()))
	return out.Write(map[string]any{
		"status":          "generated",
		"script_path":     scriptPath,
		"pattern_hash":    engine.ComputeHash(),
		"pattern_count":   engine.Export().Metadata.PatternCount,
		"approval_policy": approvalPolicy,
		"review_notice":   approvalPolicy.Label(),
	})
}

func runHookInstall(cmd *cobra.Command, args []string) error {
	return installClaudeHook(cmd, flagHookGlobal, flagHookForce || !flagHookMerge)
}

type claudeHookPaths struct {
	scope          string
	project        string
	settingsPath   string
	hookScriptPath string
	databasePath   string
}

func resolveClaudeHookPaths(global bool) (claudeHookPaths, error) {
	var paths claudeHookPaths
	project := flagProject
	if project == "" {
		project = os.Getenv("SLB_PROJECT")
	}
	if project == "" {
		project = "."
	}
	project, err := filepath.Abs(project)
	if err != nil {
		return paths, fmt.Errorf("resolving hook project directory: %w", err)
	}
	if info, err := os.Stat(project); err != nil {
		return paths, fmt.Errorf("reading hook project directory: %w", err)
	} else if !info.IsDir() {
		return paths, fmt.Errorf("hook project %q is not a directory", project)
	}
	paths.project = project
	paths.databasePath = flagDB
	if paths.databasePath == "" {
		paths.databasePath = filepath.Join(project, ".slb", "state.db")
	}
	base := project
	paths.scope = "project"
	if global {
		base, err = os.UserHomeDir()
		if err != nil {
			return paths, fmt.Errorf("failed to get home directory: %w", err)
		}
		paths.scope = "global"
	}
	paths.settingsPath = filepath.Join(base, ".claude", "settings.json")
	paths.hookScriptPath = filepath.Join(base, ".slb", "hooks", "slb_guard.py")
	return paths, nil
}

// installClaudeHook is shared by both documented installation entry points.
// It edits exactly one selected settings file through the lossless editor.
func installClaudeHook(cmd *cobra.Command, global, force bool) error {
	paths, err := resolveClaudeHookPaths(global)
	if err != nil {
		return err
	}
	settings, err := loadClaudeSettings(paths.settingsPath)
	if err != nil {
		return err
	}
	if !global {
		// Creating .slb/hooks without a database would make the project appear
		// initialized while its policy is unavailable. The guard correctly
		// denies all commands in that state; never install a broken setup or
		// silently recreate a database whose custom policy may have been lost.
		projectDB := filepath.Join(paths.project, ".slb", "state.db")
		info, err := os.Stat(projectDB)
		if os.IsNotExist(err) {
			return fmt.Errorf("project policy database %q is unavailable: run slb init in a new project, or restore the existing project database before installing its hook", projectDB)
		}
		if err != nil {
			return fmt.Errorf("reading project policy database: %w", err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("project policy database %q is not a regular file", projectDB)
		}
		if flagDB != "" {
			explicit, err := os.Stat(flagDB)
			if err != nil || !os.SameFile(info, explicit) {
				return fmt.Errorf("project hook must use %q; --db cannot select a different policy database", projectDB)
			}
		}
		paths.databasePath = projectDB
	}

	// Install must embed the complete current policy, just like generate.
	if _, err := loadCustomPatternsForProject(paths.project, paths.databasePath, !global || flagDB != ""); err != nil {
		return err
	}

	engine := core.GetDefaultEngine()
	cautionAction, approvalPolicy, err := hookPolicySettingsForProject(paths.project)
	if err != nil {
		return err
	}
	hookScript := generateHookScriptWithPolicy(engine, cautionAction, approvalPolicy)

	// The installed hot path is native. Keep writing the Python guard as
	// an explicit standalone fallback/debugging artifact, but do not pay a
	// Python startup/import penalty on every Bash tool call.
	guardCommand, err := nativeHookGuardCommand()
	if err != nil {
		return fmt.Errorf("resolving native hook command: %w", err)
	}

	// Legacy Python registrations and native registrations pointing at an
	// older/moved executable are the same managed hook: upgrade them in place.
	// Only SLB's own hook object is edited; sibling hooks sharing its matcher
	// entry, other entries and all other settings are preserved (GitHub #18).
	isSLB := func(command string) bool { return isSLBHookCommand(command, paths.hookScriptPath) }
	change, err := installSLBHook(settings, guardCommand, isSLB, force)
	if err != nil {
		return fmt.Errorf("updating %s: %w", paths.settingsPath, err)
	}
	// Validate the existing settings and complete policy before writing either
	// artifact, so invalid input leaves an earlier installation untouched.
	if err := os.MkdirAll(filepath.Dir(paths.hookScriptPath), 0755); err != nil {
		return fmt.Errorf("failed to create hook directory: %w", err)
	}
	if err := os.WriteFile(paths.hookScriptPath, []byte(hookScript), 0755); err != nil {
		return fmt.Errorf("failed to write hook script: %w", err)
	}
	if change.changed {
		if err := settings.save(); err != nil {
			return err
		}
	}

	result := map[string]any{
		"status":                  "installed",
		"scope":                   paths.scope,
		"settings_path":           paths.settingsPath,
		"settings_changed":        change.changed,
		"hook_script":             paths.hookScriptPath,
		"hook_command":            guardCommand,
		"native_guard":            true,
		"on_failure":              "block",
		"upgraded":                change.upgraded,
		"already_existed":         change.found && !change.upgraded,
		"matcher":                 change.entryMatcher,
		"sibling_hooks_preserved": change.siblingHooks,
		"approval_policy":         approvalPolicy,
		"review_notice":           approvalPolicy.Label(),
	}
	if change.upgraded {
		result["previous_command"] = change.previousCommand
	}
	if change.duplicatesRemoved > 0 {
		result["duplicate_slb_hooks_removed"] = change.duplicatesRemoved
	}
	out := output.New(output.Format(GetOutput()))
	return out.Write(result)
}

func runHookUninstall(cmd *cobra.Command, args []string) error {
	paths, err := resolveClaudeHookPaths(flagHookGlobal)
	if err != nil {
		return err
	}

	settingsPath := paths.settingsPath
	settings, err := loadClaudeSettings(settingsPath)
	if err != nil {
		return err
	}
	out := output.New(output.Format(GetOutput()))
	if !settings.exists {
		return out.Write(map[string]any{
			"status":        "not_installed",
			"scope":         paths.scope,
			"settings_path": settingsPath,
			"message":       "Claude Code settings.json not found",
		})
	}

	// Remove only SLB's own hook objects. Sibling hooks in the same matcher
	// entry stay; an entry is dropped only if SLB's hook was its last one.
	removed, err := uninstallSLBHook(settings, func(command string) bool {
		return isSLBHookCommand(command, paths.hookScriptPath)
	})
	if err != nil {
		return fmt.Errorf("updating %s: %w", settingsPath, err)
	}
	if removed == 0 {
		return out.Write(map[string]any{
			"status":        "not_installed",
			"scope":         paths.scope,
			"settings_path": settingsPath,
			"removed":       false,
			"message":       "No SLB PreToolUse hook configured",
		})
	}
	if err := settings.save(); err != nil {
		return err
	}
	return out.Write(map[string]any{
		"status":        "uninstalled",
		"scope":         paths.scope,
		"removed":       true,
		"removed_count": removed,
		"settings_path": settingsPath,
	})
}

func runHookStatus(cmd *cobra.Command, args []string) error {
	paths, err := resolveClaudeHookPaths(flagHookGlobal)
	if err != nil {
		return err
	}

	hookScriptPath := paths.hookScriptPath
	settingsPath := paths.settingsPath

	// Compare the current persisted policy with the installed guard.
	if _, err := loadCustomPatternsForProject(paths.project, paths.databasePath, flagDB != ""); err != nil {
		return err
	}
	currentHash := core.GetDefaultEngine().ComputeHash()
	currentCautionAction, currentPolicy, err := hookPolicySettingsForProject(paths.project)
	if err != nil {
		return err
	}
	status := map[string]any{
		"scope":                         paths.scope,
		"hook_script_exists":            false,
		"hook_script_path":              hookScriptPath,
		"settings_configured":           false,
		"settings_path":                 settingsPath,
		"failure_policy_configured":     false,
		"current_pattern_hash":          currentHash,
		"installed_pattern_hash":        "",
		"pattern_hash_matches":          false,
		"current_hook_caution_action":   currentCautionAction,
		"installed_hook_caution_action": "",
		"hook_caution_action_matches":   false,
		"daemon_reachable":              false,
		"daemon_status":                 "unreachable",
		"daemon_pattern_hash":           "",
		"daemon_pattern_hash_matches":   false,
		"current_approval_policy":       currentPolicy,
		"current_review_notice":         currentPolicy.Label(),
		"installed_approval_policy":     "",
		"approval_policy_matches":       false,
	}

	if info, err := os.Stat(hookScriptPath); err == nil {
		status["hook_script_exists"] = true
		status["hook_script_executable"] = info.Mode()&0111 != 0
		status["hook_script_size"] = info.Size()
		if data, readErr := os.ReadFile(hookScriptPath); readErr == nil {
			installedHash := embeddedHookPatternHash(data)
			status["installed_pattern_hash"] = installedHash
			status["pattern_hash_matches"] = installedHash != "" && installedHash == currentHash
			installedAction := embeddedHookCautionAction(data)
			status["installed_hook_caution_action"] = installedAction
			status["hook_caution_action_matches"] = installedAction != "" && installedAction == currentCautionAction
			installedPolicy := embeddedHookApprovalPolicy(data)
			status["installed_approval_policy"] = installedPolicy
			status["approval_policy_matches"] = installedPolicy != "" && installedPolicy == currentPolicy
		} else {
			status["hook_script_read_error"] = readErr.Error()
		}
	}

	if settings, err := loadClaudeSettings(settingsPath); err != nil {
		status["settings_error"] = err.Error()
	} else if _, entries, _, err := settings.preToolUse(); err != nil {
		status["settings_error"] = err.Error()
	} else {
		for _, location := range findSLBHooks(entries, func(command string) bool {
			return isSLBHookCommand(command, hookScriptPath)
		}) {
			if !location.coversBash {
				continue
			}
			status["settings_configured"] = true
			status["configured_command"] = location.command
			status["native_guard"] = strings.Contains(location.command, "hook guard")
			status["configured_on_failure"] = location.hook.values["onFailure"]
			synchronous := true
			for _, key := range []string{"async", "asyncRewake"} {
				if value, present := location.hook.get(key); present && value != false {
					synchronous = false
				}
			}
			status["failure_policy_configured"] = location.hook.values["type"] == "command" &&
				synchronous && location.hook.values["onFailure"] == "block"
			break
		}
	}

	{
		cwd := paths.project
		healthCtx, cancel := context.WithTimeout(cmd.Context(), hookQueryTimeout(cwd))
		client := daemon.NewIPCClient(daemon.SocketPathForCWD(cwd))
		health, healthErr := client.HookHealth(healthCtx, cwd)
		cancel()
		_ = client.Close()
		if healthErr != nil {
			status["daemon_error"] = healthErr.Error()
		} else {
			status["daemon_reachable"] = true
			status["daemon_status"] = health.Status
			status["daemon_pattern_hash"] = health.PatternHash
			status["daemon_pattern_hash_matches"] = health.PatternHash != "" && health.PatternHash == currentHash
			status["daemon_hook_caution_action"] = health.HookCautionAction
			status["daemon_hook_caution_action_matches"] = health.HookCautionAction == currentCautionAction
			status["daemon_approval_policy"] = health.ApprovalPolicy
			status["daemon_review_notice"] = health.ReviewNotice
			status["daemon_approval_policy_matches"] = health.ApprovalPolicy != "" && health.ApprovalPolicy == currentPolicy
			status["daemon_pattern_count"] = health.PatternCount
			status["daemon_uptime_seconds"] = health.Uptime
			if health.PolicyError != "" {
				status["daemon_policy_error"] = health.PolicyError
			}
		}
	}

	scriptOK := status["hook_script_exists"].(bool)
	settingsOK := status["settings_configured"].(bool)
	fresh := status["pattern_hash_matches"].(bool) && status["hook_caution_action_matches"].(bool) && status["approval_policy_matches"].(bool)
	daemonReachable := status["daemon_reachable"].(bool)
	daemonHealthy := !daemonReachable ||
		(status["daemon_status"] == "ok" && status["daemon_pattern_hash_matches"] == true &&
			status["daemon_hook_caution_action_matches"] == true && status["daemon_approval_policy_matches"] == true)
	switch {
	case scriptOK && settingsOK && !fresh:
		status["status"] = "stale"
	case scriptOK && settingsOK && (!daemonHealthy || status["failure_policy_configured"] != true):
		status["status"] = "degraded"
	case scriptOK && settingsOK:
		status["status"] = "installed"
	case scriptOK || settingsOK:
		status["status"] = "partial"
	default:
		status["status"] = "not_installed"
	}

	out := output.New(output.Format(GetOutput()))
	return out.Write(status)
}

func embeddedHookPatternHash(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		const prefix = "# SHA256: "
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

func embeddedHookCautionAction(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		const prefix = "HOOK_CAUTION_ACTION = "
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		var action string
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, prefix))), &action); err == nil {
			return action
		}
	}
	return ""
}

func embeddedHookApprovalPolicy(data []byte) db.ApprovalPolicy {
	for _, line := range strings.Split(string(data), "\n") {
		const prefix = "HOOK_APPROVAL_POLICY = "
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		var policy db.ApprovalPolicy
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, prefix))), &policy); err == nil && policy != "" && policy.Valid() {
			return policy
		}
	}
	return ""
}

func runHookHealth(cmd *cobra.Command, args []string) error {
	if _, err := loadCustomPatternsIntoDefaultEngine(); err != nil {
		return err
	}
	project, err := projectPath()
	if err != nil {
		return err
	}
	cwd, err := filepath.Abs(project)
	if err != nil {
		return fmt.Errorf("resolving project path: %w", err)
	}
	currentHash := core.GetDefaultEngine().ComputeHash()
	currentCautionAction, currentPolicy, err := hookPolicySettings()
	if err != nil {
		return err
	}
	result := map[string]any{
		"status":                      "unreachable",
		"healthy":                     false,
		"daemon_reachable":            false,
		"fallback_available":          true,
		"current_pattern_hash":        currentHash,
		"current_hook_caution_action": currentCautionAction,
		"socket_path":                 daemon.SocketPathForCWD(cwd),
		"cwd":                         cwd,
		"current_approval_policy":     currentPolicy,
		"current_review_notice":       currentPolicy.Label(),
	}

	healthCtx, cancel := context.WithTimeout(cmd.Context(), hookQueryTimeout(cwd))
	client := daemon.NewIPCClient(daemon.SocketPathForCWD(cwd))
	health, healthErr := client.HookHealth(healthCtx, cwd)
	cancel()
	_ = client.Close()
	if healthErr != nil {
		result["error"] = healthErr.Error()
		return output.New(output.Format(GetOutput())).Write(result)
	}

	hashMatches := health.PatternHash != "" && health.PatternHash == currentHash
	actionMatches := health.HookCautionAction == currentCautionAction
	policyMatches := health.ApprovalPolicy != "" && health.ApprovalPolicy == currentPolicy
	result["status"] = health.Status
	result["daemon_reachable"] = true
	result["healthy"] = health.Status == "ok" && hashMatches && actionMatches && policyMatches
	result["daemon_pattern_hash"] = health.PatternHash
	result["pattern_hash_matches"] = hashMatches
	result["daemon_hook_caution_action"] = health.HookCautionAction
	result["hook_caution_action_matches"] = actionMatches
	result["daemon_approval_policy"] = health.ApprovalPolicy
	result["daemon_review_notice"] = health.ReviewNotice
	result["approval_policy_matches"] = policyMatches
	result["pattern_count"] = health.PatternCount
	result["uptime_seconds"] = health.Uptime
	result["server_time"] = health.ServerTime
	if health.PolicyError != "" {
		result["policy_error"] = health.PolicyError
	}
	return output.New(output.Format(GetOutput())).Write(result)
}

func runHookTest(cmd *cobra.Command, args []string) error {
	if flagHookLocalOnly && flagHookSimulateFailure {
		return fmt.Errorf("--local-only and --simulate-failure are mutually exclusive")
	}
	command := args[0]
	if _, err := loadCustomPatternsIntoDefaultEngine(); err != nil {
		return err
	}
	project, err := projectPath()
	if err != nil {
		return err
	}
	cwd, err := filepath.Abs(project)
	if err != nil {
		return fmt.Errorf("resolving project path: %w", err)
	}

	var daemonErr error
	if !flagHookLocalOnly && !flagHookSimulateFailure {
		queryCtx, cancel := context.WithTimeout(cmd.Context(), hookQueryTimeout(cwd))
		client := daemon.NewIPCClient(daemon.SocketPathForCWD(cwd))
		live, queryErr := client.HookQuery(queryCtx, daemon.HookQueryParams{
			Command: command, SessionID: flagSessionID, CWD: cwd, ExecutionHandoff: true,
		})
		cancel()
		_ = client.Close()
		if queryErr == nil {
			return output.New(output.Format(GetOutput())).Write(map[string]any{
				"command": command, "action": live.Action, "message": live.Message,
				"tier": live.Tier, "matched_pattern": live.MatchedPattern,
				"min_approvals":  live.MinApprovals,
				"needs_approval": live.Action == "block" || live.Action == "execute",
				"request_id":     live.RequestID, "source": "daemon", "fallback": false,
				"approval_policy": live.ApprovalPolicy, "review_notice": live.ReviewNotice,
			})
		}
		daemonErr = queryErr
	} else if flagHookSimulateFailure {
		daemonErr = fmt.Errorf("simulated daemon failure")
	}

	cautionAction, approvalPolicy, cfgErr := hookPolicySettings()
	if cfgErr != nil {
		return cfgErr
	}
	result := localHookTestResultWithCautionAction(command, cwd, cautionAction)
	result["approval_policy"], result["review_notice"] = approvalPolicy, approvalPolicy.Label()
	if result["action"] != "allow" {
		result["message"] = fmt.Sprintf("%s Review policy: %s.", result["message"], approvalPolicy.Label())
	}
	result["source"] = "local"
	result["fallback"] = !flagHookLocalOnly
	result["local_only"] = flagHookLocalOnly
	if flagHookSimulateFailure {
		result["simulated_failure"] = true
	}
	if daemonErr != nil {
		result["daemon_error"] = daemonErr.Error()
	}
	return output.New(output.Format(GetOutput())).Write(result)
}

func localHookTestResult(command, cwd string) map[string]any {
	return localHookTestResultWithCautionAction(command, cwd, "block")
}

func localHookTestResultWithCautionAction(command, cwd, cautionAction string) map[string]any {
	result := core.Classify(command, cwd)
	var action, message string
	switch {
	case result.IsSafe:
		action = "allow"
		message = "Safe command, no approval needed"
	case result.Tier == core.RiskTierCritical:
		action = "block"
		message = fmt.Sprintf("CRITICAL: Requires %d approvals. Use 'slb request' to submit.", result.MinApprovals)
	case result.Tier == core.RiskTierDangerous:
		action = "block"
		message = fmt.Sprintf("DANGEROUS: Requires %d approval. Use 'slb request' to submit.", result.MinApprovals)
	case result.Tier == core.RiskTierCaution:
		if cautionAction == "ask" {
			action = "ask"
			message = "CAUTION: Command requires confirmation."
		} else {
			action = "block"
			message = "CAUTION: Submit with 'slb request'; configured auto-approval policy applies after admission."
		}
	default:
		action = "ask"
		message = "No matching local pattern; confirmation required while the daemon is unavailable"
	}

	tier := string(result.Tier)
	if tier == "" {
		tier = "unknown"
	}
	return map[string]any{
		"command": command, "action": action, "message": message,
		"tier": tier, "matched_pattern": result.MatchedPattern,
		"min_approvals": result.MinApprovals, "needs_approval": result.NeedsApproval,
	}
}

// generateHookScript creates the complete Python hook script with embedded patterns.
func generateHookScript(engine *core.PatternEngine) string {
	return generateHookScriptWithPolicy(engine, "block", db.ApprovalPolicyIndependent)
}

func generateHookScriptWithPolicy(engine *core.PatternEngine, cautionAction string, policy db.ApprovalPolicy) string {
	if cautionAction != "ask" {
		cautionAction = "block"
	}
	var script strings.Builder
	script.WriteString("#!/usr/bin/env python3\n")
	script.WriteString(engine.ExportClaudeHook())

	// A string slice always marshals successfully. JSON string escapes are also
	// valid Python syntax, so both runtimes use the same default redaction rules.
	redactions, _ := json.Marshal(core.RedactionPatterns())
	script.WriteString("\nAUDIT_REDACTION_PATTERNS = ")
	script.Write(redactions)
	script.WriteString("\nHOOK_CAUTION_ACTION = ")
	actionJSON, _ := json.Marshal(cautionAction)
	script.Write(actionJSON)
	script.WriteString("\nHOOK_APPROVAL_POLICY = ")
	policyJSON, _ := json.Marshal(policy.Normalized())
	script.Write(policyJSON)
	script.WriteString("\n")
	script.WriteString(hookRuntime)
	return script.String()
}

func hookPolicySettings() (string, db.ApprovalPolicy, error) {
	project, err := projectPath()
	if err != nil {
		return "", "", err
	}
	return hookPolicySettingsForProject(project)
}

func hookPolicySettingsForProject(project string) (string, db.ApprovalPolicy, error) {
	cfg, err := config.Load(config.LoadOptions{ProjectDir: project, ConfigPath: flagConfig})
	if err != nil {
		return "", "", fmt.Errorf("loading hook policy: %w", err)
	}
	return cfg.Integrations.HookCautionAction, db.ApprovalPolicy(cfg.General.ApprovalPolicy), nil
}
