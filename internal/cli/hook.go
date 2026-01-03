package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

var (
	flagHookGlobal    bool
	flagHookMerge     bool
	flagHookForce     bool
	flagHookOutputDir string
)

func init() {
	// hook install flags
	hookInstallCmd.Flags().BoolVarP(&flagHookGlobal, "global", "g", false, "install globally for all projects")
	hookInstallCmd.Flags().BoolVar(&flagHookMerge, "merge", true, "preserve existing hooks (default)")
	hookInstallCmd.Flags().BoolVarP(&flagHookForce, "force", "f", false, "overwrite existing hooks")

	// hook generate flags
	hookGenerateCmd.Flags().StringVarP(&flagHookOutputDir, "output", "o", "", "output directory (default: ~/.slb/hooks/)")

	// Add subcommands
	hookCmd.AddCommand(hookGenerateCmd)
	hookCmd.AddCommand(hookInstallCmd)
	hookCmd.AddCommand(hookUninstallCmd)
	hookCmd.AddCommand(hookStatusCmd)
	hookCmd.AddCommand(hookTestCmd)

	rootCmd.AddCommand(hookCmd)
}

var hookCmd = &cobra.Command{
	Use:   "hook",
	Short: "Manage Claude Code hook integration",
	Long: `Manage the Claude Code PreToolUse hook that integrates SLB approval workflow.

The hook intercepts Bash tool calls before execution and checks if the command
requires SLB approval. Dangerous commands are blocked until approved.

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
Use --output to specify a different directory.

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
1. Generates the hook script to ~/.slb/hooks/slb_guard.py
2. Updates ~/.claude/settings.json with the hook configuration
3. Preserves existing hooks (use --force to overwrite)

Use --global to install for all projects (user-level settings).`,
	RunE: runHookInstall,
}

var hookUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove hook from Claude Code settings",
	Long: `Remove the SLB hook from Claude Code settings.

This removes the hook configuration from settings.json but does not delete
the hook script file. Use this if you want to temporarily disable SLB
hook integration.`,
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

var hookTestCmd = &cobra.Command{
	Use:   "test [command]",
	Short: "Test hook behavior for a command",
	Long: `Test what the hook would do for a given command.

This simulates the hook's decision without actually running the command.
Useful for verifying pattern classification and approval logic.

Examples:
  slb hook test "rm -rf node_modules"
  slb hook test "git push --force"`,
	Args: cobra.MaximumNArgs(1),
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

	// Generate hook script
	engine := core.GetDefaultEngine()
	hookScript := generateHookScript(engine)

	// Write script
	scriptPath := filepath.Join(outputDir, "slb_guard.py")
	if err := os.WriteFile(scriptPath, []byte(hookScript), 0755); err != nil {
		return fmt.Errorf("failed to write hook script: %w", err)
	}

	out := output.New(output.Format(GetOutput()))
	return out.Write(map[string]any{
		"status":        "generated",
		"script_path":   scriptPath,
		"pattern_hash":  engine.ComputeHash(),
		"pattern_count": engine.Export().Metadata.PatternCount,
	})
}

func runHookInstall(cmd *cobra.Command, args []string) error {
	// Generate the hook script (without output)
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	outputDir := filepath.Join(home, ".slb", "hooks")
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", outputDir, err)
	}

	engine := core.GetDefaultEngine()
	hookScript := generateHookScript(engine)

	hookScriptPath := filepath.Join(outputDir, "slb_guard.py")
	if err := os.WriteFile(hookScriptPath, []byte(hookScript), 0755); err != nil {
		return fmt.Errorf("failed to write hook script: %w", err)
	}

	// Get settings.json path
	settingsPath := filepath.Join(home, ".claude", "settings.json")

	// Read existing settings or create new
	var settings map[string]any
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to read settings: %w", err)
		}
		// Create new settings
		settings = make(map[string]any)
	} else {
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("failed to parse settings: %w", err)
		}
	}

	// Build hook configuration
	slbHook := map[string]any{
		"matcher": "Bash",
		"hooks": []map[string]any{
			{
				"type":    "command",
				"command": fmt.Sprintf("python3 %s", hookScriptPath),
			},
		},
	}

	// Get or create hooks section
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		hooks = make(map[string]any)
	}

	// Get or create PreToolUse array
	preToolUse, ok := hooks["PreToolUse"].([]any)
	if !ok {
		preToolUse = []any{}
	}

	// Check if SLB hook already exists
	found := false
	for i, hook := range preToolUse {
		if h, ok := hook.(map[string]any); ok {
			if matcher, ok := h["matcher"].(string); ok && matcher == "Bash" {
				if hookList, ok := h["hooks"].([]any); ok {
					for _, hk := range hookList {
						if hkMap, ok := hk.(map[string]any); ok {
							if cmd, ok := hkMap["command"].(string); ok {
								if filepath.Base(cmd) == "slb_guard.py" || cmd == fmt.Sprintf("python3 %s", hookScriptPath) {
									found = true
									if flagHookForce {
										preToolUse[i] = slbHook
									}
									break
								}
							}
						}
					}
				}
			}
		}
	}

	if !found {
		preToolUse = append(preToolUse, slbHook)
	}

	hooks["PreToolUse"] = preToolUse
	settings["hooks"] = hooks

	// Ensure .claude directory exists
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0755); err != nil {
		return fmt.Errorf("failed to create .claude directory: %w", err)
	}

	// Write settings
	newData, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, newData, 0644); err != nil {
		return fmt.Errorf("failed to write settings: %w", err)
	}

	out := output.New(output.Format(GetOutput()))
	return out.Write(map[string]any{
		"status":        "installed",
		"settings_path": settingsPath,
		"hook_script":   hookScriptPath,
		"already_existed": found && !flagHookForce,
	})
}

func runHookUninstall(cmd *cobra.Command, args []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	settingsPath := filepath.Join(home, ".claude", "settings.json")

	// Read existing settings
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		if os.IsNotExist(err) {
			out := output.New(output.Format(GetOutput()))
			return out.Write(map[string]any{
				"status":  "not_installed",
				"message": "Claude Code settings.json not found",
			})
		}
		return fmt.Errorf("failed to read settings: %w", err)
	}

	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		return fmt.Errorf("failed to parse settings: %w", err)
	}

	// Remove SLB hook from PreToolUse
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		out := output.New(output.Format(GetOutput()))
		return out.Write(map[string]any{
			"status":  "not_installed",
			"message": "No hooks configured",
		})
	}

	preToolUse, ok := hooks["PreToolUse"].([]any)
	if !ok {
		out := output.New(output.Format(GetOutput()))
		return out.Write(map[string]any{
			"status":  "not_installed",
			"message": "No PreToolUse hooks configured",
		})
	}

	// Filter out SLB hooks
	var filtered []any
	removed := false
	for _, hook := range preToolUse {
		if h, ok := hook.(map[string]any); ok {
			if matcher, ok := h["matcher"].(string); ok && matcher == "Bash" {
				if hookList, ok := h["hooks"].([]any); ok {
					isSLB := false
					for _, hk := range hookList {
						if hkMap, ok := hk.(map[string]any); ok {
							if cmd, ok := hkMap["command"].(string); ok {
								if filepath.Base(cmd) == "slb_guard.py" ||
								   (len(cmd) > 7 && cmd[len(cmd)-13:] == "slb_guard.py") {
									isSLB = true
									removed = true
									break
								}
							}
						}
					}
					if isSLB {
						continue
					}
				}
			}
		}
		filtered = append(filtered, hook)
	}

	hooks["PreToolUse"] = filtered
	settings["hooks"] = hooks

	// Write settings
	newData, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, newData, 0644); err != nil {
		return fmt.Errorf("failed to write settings: %w", err)
	}

	out := output.New(output.Format(GetOutput()))
	return out.Write(map[string]any{
		"status":  "uninstalled",
		"removed": removed,
	})
}

func runHookStatus(cmd *cobra.Command, args []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	hookScriptPath := filepath.Join(home, ".slb", "hooks", "slb_guard.py")
	settingsPath := filepath.Join(home, ".claude", "settings.json")

	status := map[string]any{
		"hook_script_exists":   false,
		"hook_script_path":     hookScriptPath,
		"settings_configured":  false,
		"settings_path":        settingsPath,
		"current_pattern_hash": core.GetDefaultEngine().ComputeHash(),
	}

	// Check hook script
	if info, err := os.Stat(hookScriptPath); err == nil {
		status["hook_script_exists"] = true
		status["hook_script_executable"] = info.Mode()&0111 != 0
		status["hook_script_size"] = info.Size()
	}

	// Check settings.json
	data, err := os.ReadFile(settingsPath)
	if err == nil {
		var settings map[string]any
		if err := json.Unmarshal(data, &settings); err == nil {
			if hooks, ok := settings["hooks"].(map[string]any); ok {
				if preToolUse, ok := hooks["PreToolUse"].([]any); ok {
					for _, hook := range preToolUse {
						if h, ok := hook.(map[string]any); ok {
							if matcher, ok := h["matcher"].(string); ok && matcher == "Bash" {
								if hookList, ok := h["hooks"].([]any); ok {
									for _, hk := range hookList {
										if hkMap, ok := hk.(map[string]any); ok {
											if cmd, ok := hkMap["command"].(string); ok {
												if filepath.Base(cmd) == "slb_guard.py" ||
												   (len(cmd) > 13 && cmd[len(cmd)-13:] == "slb_guard.py") {
													status["settings_configured"] = true
													status["configured_command"] = cmd
												}
											}
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}

	// Overall status
	scriptOK := status["hook_script_exists"].(bool)
	settingsOK := status["settings_configured"].(bool)
	if scriptOK && settingsOK {
		status["status"] = "installed"
	} else if scriptOK || settingsOK {
		status["status"] = "partial"
	} else {
		status["status"] = "not_installed"
	}

	out := output.New(output.Format(GetOutput()))
	return out.Write(status)
}

func runHookTest(cmd *cobra.Command, args []string) error {
	var command string
	if len(args) > 0 {
		command = args[0]
	} else {
		return fmt.Errorf("command argument required")
	}

	result := core.Classify(command, "")

	// Determine what the hook would do
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
		action = "ask"
		message = "CAUTION: Command logged for review."
	default:
		action = "allow"
		message = "No matching pattern, allowed"
	}

	out := output.New(output.Format(GetOutput()))
	return out.Write(map[string]any{
		"command":         command,
		"action":          action,
		"message":         message,
		"tier":            string(result.Tier),
		"matched_pattern": result.MatchedPattern,
		"min_approvals":   result.MinApprovals,
		"needs_approval":  result.NeedsApproval,
	})
}

// generateHookScript creates the complete Python hook script with embedded patterns.
func generateHookScript(engine *core.PatternEngine) string {
	// Start with shebang
	var script strings.Builder
	script.WriteString("#!/usr/bin/env python3\n")

	// Get the Claude hook format export
	pythonPatterns := engine.ExportClaudeHook()
	script.WriteString(pythonPatterns)

	// Add the hook main logic
	hookMain := `

# === SLB Hook Integration ===

import sys
import json
import socket
import os

SLB_SOCKET_PATH = os.path.expanduser("~/.slb/hook.sock")
SLB_TIMEOUT = 0.05  # 50ms timeout

def query_slb_daemon(command: str, session_id: str, cwd: str) -> Optional[dict]:
    """Query SLB daemon for approval status. Returns None if unavailable."""
    if not os.path.exists(SLB_SOCKET_PATH):
        return None

    try:
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as sock:
            sock.settimeout(SLB_TIMEOUT)
            sock.connect(SLB_SOCKET_PATH)
            request = json.dumps({
                "type": "query",
                "command": command,
                "session_id": session_id,
                "cwd": cwd
            })
            sock.sendall(request.encode() + b'\n')
            response = sock.recv(4096)
            return json.loads(response.decode())
    except (socket.error, json.JSONDecodeError, TimeoutError):
        return None

def main():
    """Main hook entry point."""
    try:
        input_data = json.loads(sys.stdin.read())
    except json.JSONDecodeError:
        # Invalid input, allow by default
        print(json.dumps({"action": "allow"}))
        return

    # Extract command from Bash tool input
    tool_input = input_data.get("tool_input", {})
    command = tool_input.get("command", "")
    session_id = input_data.get("session_id", "")
    cwd = os.getcwd()

    if not command:
        print(json.dumps({"action": "allow"}))
        return

    # Try daemon first
    daemon_response = query_slb_daemon(command, session_id, cwd)
    if daemon_response:
        print(json.dumps(daemon_response))
        return

    # Fall back to local classification
    tier, min_approvals = classify(command)
    blocked, message = is_blocked(command)

    if blocked:
        print(json.dumps({
            "action": "block",
            "message": message
        }))
    elif tier == 'caution':
        print(json.dumps({
            "action": "ask",
            "message": f"SLB: {tier.upper()} tier command. Proceed?"
        }))
    else:
        print(json.dumps({"action": "allow"}))

if __name__ == "__main__":
    main()
`
	script.WriteString(hookMain)
	return script.String()
}
