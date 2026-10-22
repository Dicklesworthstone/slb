package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/daemon"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/spf13/cobra"
)

// newTestHookCmd creates a fresh hook command tree for testing.
func newTestHookCmd(dbPath string) *cobra.Command {
	root := &cobra.Command{
		Use:           "slb",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVar(&flagDB, "db", dbPath, "database path")
	root.PersistentFlags().StringVarP(&flagOutput, "output", "o", "text", "output format")
	root.PersistentFlags().BoolVarP(&flagJSON, "json", "j", false, "json output")
	root.PersistentFlags().StringVarP(&flagProject, "project", "C", "", "project directory")
	root.PersistentFlags().StringVarP(&flagSessionID, "session-id", "s", "", "session ID")

	// Create fresh hook commands
	hkCmd := &cobra.Command{
		Use:   "hook",
		Short: "Manage Claude Code hook integration",
	}

	generateCmd := &cobra.Command{
		Use:   "generate",
		Short: "Generate the Python hook script",
		RunE:  hookGenerateCmd.RunE,
	}
	// Mirror production: --output-dir (no -o shorthand); -o/--output is the
	// persistent output FORMAT flag.
	generateCmd.Flags().StringVar(&flagHookOutputDir, "output-dir", "", "output directory")

	installCmd := &cobra.Command{
		Use:   "install",
		Short: "Install hook into Claude Code settings",
		RunE:  hookInstallCmd.RunE,
	}
	installCmd.Flags().BoolVarP(&flagHookGlobal, "global", "g", false, "install globally")
	installCmd.Flags().BoolVar(&flagHookMerge, "merge", true, "preserve existing hooks")
	installCmd.Flags().BoolVarP(&flagHookForce, "force", "f", false, "overwrite existing hooks")

	uninstallCmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove hook from Claude Code settings",
		RunE:  hookUninstallCmd.RunE,
	}

	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show hook installation status",
		RunE:  hookStatusCmd.RunE,
	}

	healthCmd := &cobra.Command{
		Use:   "health",
		Short: "Check hook daemon health",
		RunE:  hookHealthCmd.RunE,
	}

	testCmd := &cobra.Command{
		Use:   "test <command>",
		Short: "Test hook behavior for a command",
		Args:  cobra.ExactArgs(1),
		RunE:  hookTestCmd.RunE,
	}
	testCmd.Flags().BoolVar(&flagHookLocalOnly, "local-only", false, "test local policy")
	testCmd.Flags().BoolVar(&flagHookSimulateFailure, "simulate-failure", false, "simulate daemon failure")

	hkCmd.AddCommand(generateCmd, installCmd, uninstallCmd, statusCmd, healthCmd, testCmd)
	root.AddCommand(hkCmd)

	return root
}

func resetHookFlags() {
	flagDB = ""
	flagOutput = "text"
	flagJSON = false
	flagProject = ""
	flagSessionID = ""
	flagHookGlobal = false
	flagHookMerge = true
	flagHookForce = false
	flagHookOutputDir = ""
	flagHookLocalOnly = false
	flagHookSimulateFailure = false
}

func TestHookCommand_Help(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHookFlags()

	cmd := newTestHookCmd(h.DBPath)
	stdout, _, err := executeCommand(cmd, "hook", "--help")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(stdout, "hook") {
		t.Error("expected help to mention 'hook'")
	}
	if !strings.Contains(stdout, "generate") {
		t.Error("expected help to mention 'generate' subcommand")
	}
	if !strings.Contains(stdout, "install") {
		t.Error("expected help to mention 'install' subcommand")
	}
	if !strings.Contains(stdout, "status") {
		t.Error("expected help to mention 'status' subcommand")
	}
}

func TestHookGenerateCommand_GeneratesScript(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHookFlags()

	// Use temp directory for output
	tmpDir := t.TempDir()

	cmd := newTestHookCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "hook", "generate", "--output-dir", tmpDir, "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["status"] != "generated" {
		t.Errorf("expected status='generated', got %v", result["status"])
	}

	// Verify script file was created
	scriptPath := filepath.Join(tmpDir, "slb_guard.py")
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		t.Fatal("expected hook script to be created")
	}

	// Verify script content
	content, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("failed to read script: %v", err)
	}

	if !strings.Contains(string(content), "import re") {
		t.Error("expected script to contain 'import re'")
	}
	if !strings.Contains(string(content), "SAFE_PATTERNS") {
		t.Error("expected script to contain 'SAFE_PATTERNS'")
	}
	if !strings.Contains(string(content), "def main():") {
		t.Error("expected script to contain 'def main()'")
	}
	if !strings.Contains(string(content), "query_slb_daemon") {
		t.Error("expected script to contain 'query_slb_daemon' function")
	}

	// Check hash is included
	if _, ok := result["pattern_hash"]; !ok {
		t.Error("expected 'pattern_hash' in result")
	}
	if _, ok := result["pattern_count"]; !ok {
		t.Error("expected 'pattern_count' in result")
	}
}

func TestHookGenerateCommand_ScriptIsExecutable(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHookFlags()

	tmpDir := t.TempDir()

	cmd := newTestHookCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "hook", "generate", "--output-dir", tmpDir, "-j")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify script is executable
	scriptPath := filepath.Join(tmpDir, "slb_guard.py")
	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatalf("failed to stat script: %v", err)
	}

	// Check for execute permission
	if info.Mode()&0111 == 0 {
		t.Error("expected script to have execute permission")
	}
}

func TestHookTestCommand_RequiresCommand(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHookFlags()

	cmd := newTestHookCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "hook", "test", "-j")

	if err == nil {
		t.Fatal("expected error when command is missing")
	}
	if !strings.Contains(err.Error(), "accepts 1 arg") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestHookTestCommand_SafeCommand(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHookFlags()

	cmd := newTestHookCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "hook", "test", "git stash", "--local-only", "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["command"] != "git stash" {
		t.Errorf("expected command='git stash', got %v", result["command"])
	}
	if result["action"] != "allow" {
		t.Errorf("expected action='allow' for safe command, got %v", result["action"])
	}
}

func TestHookTestCommand_DangerousCommand(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHookFlags()

	cmd := newTestHookCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "hook", "test", "rm -rf node_modules", "--local-only", "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["action"] != "block" {
		t.Errorf("expected action='block' for dangerous command, got %v", result["action"])
	}
	if result["needs_approval"] != true {
		t.Errorf("expected needs_approval=true for dangerous command, got %v", result["needs_approval"])
	}
}

func TestHookTestCommand_CriticalCommand(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHookFlags()

	cmd := newTestHookCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "hook", "test", "git push --force", "--local-only", "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["action"] != "block" {
		t.Errorf("expected action='block' for critical command, got %v", result["action"])
	}
	if result["tier"] != "critical" {
		t.Errorf("expected tier='critical' for force push, got %v", result["tier"])
	}
}

func TestHookTestCommand_OutputFields(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHookFlags()

	cmd := newTestHookCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "hook", "test", "rm -rf /tmp/test", "--local-only", "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	// Check all expected fields
	expectedFields := []string{"command", "action", "message", "tier", "min_approvals", "needs_approval"}
	for _, field := range expectedFields {
		if _, ok := result[field]; !ok {
			t.Errorf("expected field %q in result", field)
		}
	}
}

func TestHookStatusCommand_NotInstalled(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHookFlags()

	// Create a temp home to ensure nothing is installed
	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	defer os.Unsetenv("HOME")

	cmd := newTestHookCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "hook", "status", "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["status"] != "not_installed" {
		t.Errorf("expected status='not_installed', got %v", result["status"])
	}
	if result["hook_script_exists"] != false {
		t.Errorf("expected hook_script_exists=false, got %v", result["hook_script_exists"])
	}
	if result["settings_configured"] != false {
		t.Errorf("expected settings_configured=false, got %v", result["settings_configured"])
	}
}

func TestHookStatusCommand_PartialInstall(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHookFlags()

	// Create temp home and generate script only
	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	defer os.Unsetenv("HOME")

	// Generate script
	resetHookFlags()
	genCmd := newTestHookCmd(h.DBPath)
	_, err := executeCommandCapture(t, genCmd, "hook", "generate", "-j")
	if err != nil {
		t.Fatalf("failed to generate hook: %v", err)
	}

	// Check status
	resetHookFlags()
	statusCmd := newTestHookCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, statusCmd, "hook", "status", "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["status"] != "partial" {
		t.Errorf("expected status='partial', got %v", result["status"])
	}
	if result["hook_script_exists"] != true {
		t.Errorf("expected hook_script_exists=true, got %v", result["hook_script_exists"])
	}
	if result["settings_configured"] != false {
		t.Errorf("expected settings_configured=false, got %v", result["settings_configured"])
	}
}

func TestHookStatusCommand_FullyInstalled(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHookFlags()

	// Create temp home
	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	defer os.Unsetenv("HOME")

	// Install hook
	installCmd := newTestHookCmd(h.DBPath)
	_, err := executeCommandCapture(t, installCmd, "hook", "install", "-j")
	if err != nil {
		t.Fatalf("failed to install hook: %v", err)
	}

	// Check status
	resetHookFlags()
	statusCmd := newTestHookCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, statusCmd, "hook", "status", "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["status"] != "installed" {
		t.Errorf("expected status='installed', got %v", result["status"])
	}
	if result["hook_script_exists"] != true {
		t.Errorf("expected hook_script_exists=true, got %v", result["hook_script_exists"])
	}
	if result["settings_configured"] != true {
		t.Errorf("expected settings_configured=true, got %v", result["settings_configured"])
	}
}

func TestHookInstallCommand_CreatesSettingsFile(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHookFlags()

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	defer os.Unsetenv("HOME")

	cmd := newTestHookCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "hook", "install", "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["status"] != "installed" {
		t.Errorf("expected status='installed', got %v", result["status"])
	}

	// Verify settings file exists and has correct content
	settingsPath := filepath.Join(tmpHome, ".claude", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("failed to read settings file: %v", err)
	}

	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("failed to parse settings: %v", err)
	}

	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		t.Fatal("expected 'hooks' in settings")
	}

	preToolUse, ok := hooks["PreToolUse"].([]any)
	if !ok {
		t.Fatal("expected 'PreToolUse' array in hooks")
	}

	if len(preToolUse) == 0 {
		t.Error("expected at least one PreToolUse hook")
	}
}

func TestHookInstallCommand_PreservesExistingHooks(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHookFlags()

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	defer os.Unsetenv("HOME")

	// Create existing settings with another hook
	claudeDir := filepath.Join(tmpHome, ".claude")
	os.MkdirAll(claudeDir, 0755)

	existingSettings := map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []any{
				map[string]any{
					"matcher": "Read",
					"hooks": []any{
						map[string]any{
							"type":    "command",
							"command": "echo 'reading file'",
						},
					},
				},
			},
		},
	}
	data, _ := json.MarshalIndent(existingSettings, "", "  ")
	os.WriteFile(filepath.Join(claudeDir, "settings.json"), data, 0644)

	// Install SLB hook
	cmd := newTestHookCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "hook", "install", "-j")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify both hooks exist
	settingsPath := filepath.Join(tmpHome, ".claude", "settings.json")
	newData, _ := os.ReadFile(settingsPath)

	var settings map[string]any
	json.Unmarshal(newData, &settings)

	hooks := settings["hooks"].(map[string]any)
	preToolUse := hooks["PreToolUse"].([]any)

	if len(preToolUse) != 2 {
		t.Errorf("expected 2 PreToolUse hooks (existing + SLB), got %d", len(preToolUse))
	}
}

func TestHookUninstallCommand_RemovesHook(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHookFlags()

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	defer os.Unsetenv("HOME")

	// First install
	installCmd := newTestHookCmd(h.DBPath)
	_, err := executeCommandCapture(t, installCmd, "hook", "install", "-j")
	if err != nil {
		t.Fatalf("failed to install: %v", err)
	}

	// Then uninstall
	resetHookFlags()
	uninstallCmd := newTestHookCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, uninstallCmd, "hook", "uninstall", "-j")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["status"] != "uninstalled" {
		t.Errorf("expected status='uninstalled', got %v", result["status"])
	}
	if result["removed"] != true {
		t.Errorf("expected removed=true, got %v", result["removed"])
	}

	// Verify settings no longer has SLB hook
	settingsPath := filepath.Join(tmpHome, ".claude", "settings.json")
	data, _ := os.ReadFile(settingsPath)

	var settings map[string]any
	json.Unmarshal(data, &settings)

	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		return // No hooks section at all - that's fine
	}

	preToolUse, ok := hooks["PreToolUse"].([]any)
	if !ok || preToolUse == nil {
		return // No PreToolUse hooks - that's expected after uninstall
	}

	for _, hook := range preToolUse {
		h, ok := hook.(map[string]any)
		if !ok {
			continue
		}
		if hookList, ok := h["hooks"].([]any); ok {
			for _, hk := range hookList {
				if hkMap, ok := hk.(map[string]any); ok {
					if cmd, ok := hkMap["command"].(string); ok {
						if strings.Contains(cmd, "slb_guard.py") {
							t.Error("SLB hook should have been removed")
						}
					}
				}
			}
		}
	}
}

func TestHookUninstallCommand_NotInstalled(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHookFlags()

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	defer os.Unsetenv("HOME")

	cmd := newTestHookCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "hook", "uninstall", "-j")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["status"] != "not_installed" {
		t.Errorf("expected status='not_installed', got %v", result["status"])
	}
}

func TestHookInstallCommand_Idempotent(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHookFlags()

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	defer os.Unsetenv("HOME")

	// Install twice
	cmd1 := newTestHookCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd1, "hook", "install", "-j")
	if err != nil {
		t.Fatalf("first install error: %v", err)
	}

	resetHookFlags()
	cmd2 := newTestHookCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd2, "hook", "install", "-j")
	if err != nil {
		t.Fatalf("second install error: %v", err)
	}

	var result map[string]any
	json.Unmarshal([]byte(stdout), &result)

	// Should still succeed but note it already existed
	if result["already_existed"] != true {
		t.Errorf("expected already_existed=true on second install, got %v", result["already_existed"])
	}

	// Verify only one SLB hook in settings
	settingsPath := filepath.Join(tmpHome, ".claude", "settings.json")
	data, _ := os.ReadFile(settingsPath)

	var settings map[string]any
	json.Unmarshal(data, &settings)

	hooks := settings["hooks"].(map[string]any)
	preToolUse := hooks["PreToolUse"].([]any)

	slbCount := 0
	for _, hook := range preToolUse {
		h := hook.(map[string]any)
		if h["matcher"] == "Bash" {
			if hookList, ok := h["hooks"].([]any); ok {
				for _, hk := range hookList {
					if hkMap, ok := hk.(map[string]any); ok {
						if cmd, ok := hkMap["command"].(string); ok {
							// The installed guard is native ("<slb> hook guard");
							// count any SLB registration, native or legacy.
							if isSLBHookCommand(cmd, filepath.Join(tmpHome, ".slb", "hooks", "slb_guard.py")) {
								slbCount++
							}
						}
					}
				}
			}
		}
	}

	if slbCount != 1 {
		t.Errorf("expected exactly 1 SLB hook after double install, got %d", slbCount)
	}
}

func TestGenerateHookScript_ContainsEssentialComponents(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHookFlags()

	tmpDir := t.TempDir()

	cmd := newTestHookCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "hook", "generate", "--output-dir", tmpDir, "-j")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	scriptPath := filepath.Join(tmpDir, "slb_guard.py")
	content, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("failed to read script: %v", err)
	}

	script := string(content)

	// Essential components
	essentials := []string{
		"#!/usr/bin/env python3", // Shebang
		"import re",              // Regex import
		"import sys",             // Sys import
		"import json",            // JSON import
		"import socket",          // Socket for daemon
		"SAFE_PATTERNS",          // Pattern arrays
		"CAUTION_PATTERNS",
		"DANGEROUS_PATTERNS",
		"CRITICAL_PATTERNS",
		"def classify(command:",        // Classify function
		"def is_blocked(command:",      // Block check function
		"def query_slb_daemon",         // Daemon query function
		"def get_socket_path",          // Socket path function
		"def main():",                  // Entry point
		"if __name__ == \"__main__\":", // Module guard
	}

	for _, essential := range essentials {
		if !strings.Contains(script, essential) {
			t.Errorf("expected script to contain %q", essential)
		}
	}
}

// Regression tests for issues #4 and #5 — the generated hook
// script must (a) preserve regex metacharacters in raw strings,
// (b) use re.search not re.match, (c) emit the Claude Code 2026.04
// JSON shape, and (d) defensively map unknown verdicts to "ask"
// rather than "allow".
func TestHookGenerateCommand_GeneratedScriptShapeIsCorrect(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHookFlags()

	tmpDir := t.TempDir()
	cmd := newTestHookCmd(h.DBPath)
	if _, err := executeCommandCapture(t, cmd, "hook", "generate", "--output-dir", tmpDir, "-j"); err != nil {
		t.Fatalf("hook generate: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(tmpDir, "slb_guard.py"))
	if err != nil {
		t.Fatalf("read generated script: %v", err)
	}
	script := string(body)

	// (a) #4: the recursive-rm builtin must preserve regex whitespace
	// in Python raw strings rather than matching literal backslashes.
	if strings.Contains(script, `r'^rm\\s+`) {
		t.Errorf("regression #4: generated script double-escapes \\s inside raw-string literals; " +
			"this kills every regex metacharacter in the embedded fallback classifier")
	}
	if !strings.Contains(script, `r'^rm\s+(?:-[a-zA-Z]+\s+|--[\w=-]+\s+)*`) {
		t.Errorf("expected recursive-rm raw-string in generated script; not found")
	}

	// (b) #4 follow-on: classify() must use search, not match. Anchored
	// matching loses every mid-command hit. 33e54f2 renamed the loop
	// variable from p to pattern.
	if strings.Contains(script, ".match(command)") {
		t.Errorf("regression #4 follow-on: classify() uses anchored .match; should use unanchored .search")
	}
	if !strings.Contains(script, "if pattern.search(command):") {
		t.Errorf("expected pattern.search in classify(); not found")
	}

	// (c) #5: hookSpecificOutput shape is the only one Claude Code
	// 2026.04 honors.
	if !strings.Contains(script, `"hookSpecificOutput"`) {
		t.Errorf("regression #5: generated script does not emit hookSpecificOutput shape")
	}
	if !strings.Contains(script, `"permissionDecision"`) {
		t.Errorf("regression #5: generated script does not emit permissionDecision field")
	}
	// And the legacy shape MUST NOT be emitted.
	if strings.Contains(script, `"action": "block"`) {
		t.Errorf("regression #5: generated script still emits legacy {action: block} shape")
	}

	// (d) defense in depth: _emit_decision must exist and route
	// unknown actions to ask, not allow. We can't run the Python
	// inline, but we can pin the source-level shape.
	if !strings.Contains(script, "def _emit_decision(") {
		t.Errorf("expected _emit_decision helper in generated script")
	}
	// The defensive default for unknown actions is "ask"; the previous
	// broken implementation defaulted to "allow" (fail-open). Since
	// 33e54f2 the mapping lives in _normalized_action, so exercise the
	// generated functions themselves rather than pinning their source.
	if !strings.Contains(script, "def _normalized_action(") {
		t.Errorf("expected _normalized_action helper in generated script")
	}
	for action, want := range map[string]string{
		"'allow'": "allow", "'block'": "deny", "'deny'": "deny",
		"'weird'": "ask", "None": "ask", "42": "ask", "''": "ask",
	} {
		stdout := runGeneratedHookPython(t, filepath.Join(tmpDir, "slb_guard.py"), "g['_emit_decision']("+action+", 'reason')")
		var result struct {
			Output struct {
				Permission string `json:"permissionDecision"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal([]byte(stdout), &result); err != nil {
			t.Fatalf("_emit_decision(%s) produced invalid JSON %q: %v", action, stdout, err)
		}
		if result.Output.Permission != want {
			t.Errorf("_emit_decision(%s) = %q, want %q (unknown verdicts must fail closed)", action, result.Output.Permission, want)
		}
	}
}

// runGeneratedHookPython loads a generated guard without running its main()
// and evaluates expr with the module globals bound to g, returning stdout.
func runGeneratedHookPython(t *testing.T, scriptPath, expr string, args ...string) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required to exercise the generated hook")
	}
	program := "import runpy, sys\ng = runpy.run_path(sys.argv[1], run_name='slb_guard_test')\n" + expr + "\n"
	command := exec.Command(python, append([]string{"-c", program, scriptPath}, args...)...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("generated hook evaluation failed: %v\n%s", err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

// The generated script must walk up from CWD looking for .slb/
// when computing the daemon socket path (#3). Anchored hashing of
// the immediate CWD diverges from the daemon's hash whenever the
// hook fires from a sub-directory of the project.
func TestHookGenerateCommand_SocketPathWalksUpToProjectRoot(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHookFlags()

	tmpDir := t.TempDir()
	cmd := newTestHookCmd(h.DBPath)
	if _, err := executeCommandCapture(t, cmd, "hook", "generate", "--output-dir", tmpDir, "-j"); err != nil {
		t.Fatalf("hook generate: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(tmpDir, "slb_guard.py"))
	if err != nil {
		t.Fatalf("read generated script: %v", err)
	}
	script := string(body)

	if !strings.Contains(script, "_project_root_for_socket(") {
		t.Fatalf("regression #3: generated script does not include _project_root_for_socket helper")
	}
	// Exercise the walk-up itself (33e54f2 reshaped its source): from a
	// nested directory it must return the nearest ancestor holding .slb/,
	// and without any .slb/ ancestor it must fall back to the start dir.
	root := t.TempDir()
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(filepath.Join(root, ".slb"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	loose := t.TempDir()
	got := runGeneratedHookPython(t, filepath.Join(tmpDir, "slb_guard.py"),
		"print(g['_project_root_for_socket'](sys.argv[2]))\nprint(g['_project_root_for_socket'](sys.argv[3]))", nested, loose)
	lines := strings.Split(got, "\n")
	resolvedRoot, _ := filepath.EvalSymlinks(root)
	if len(lines) != 2 || (lines[0] != root && lines[0] != resolvedRoot) {
		t.Errorf("regression #3: _project_root_for_socket(%q) = %q, want project root %q", nested, got, root)
	}
	// The fallback is the start dir unless the host itself has a .slb
	// directory somewhere above the temp dir; mirror the walk in Go.
	physicalLoose, err := filepath.EvalSymlinks(loose)
	if err != nil {
		t.Fatal(err)
	}
	wantLoose := physicalLoose
	for dir := physicalLoose; ; dir = filepath.Dir(dir) {
		if info, err := os.Stat(filepath.Join(dir, ".slb")); err == nil && info.IsDir() {
			wantLoose = dir
			break
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	if len(lines) == 2 && lines[1] != wantLoose {
		t.Errorf("_project_root_for_socket without .slb in the fixture = %q, want %q", lines[1], wantLoose)
	}
}

func TestHookGenerateCommand_SymlinkSocketParity(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHookFlags()
	t.Cleanup(resetHookFlags)
	scriptDir := t.TempDir()
	cmd := newTestHookCmd(h.DBPath)
	if _, err := executeCommandCapture(t, cmd, "hook", "generate", "--output-dir", scriptDir, "-j"); err != nil {
		t.Fatal(err)
	}
	physical, err := filepath.EvalSymlinks(h.ProjectDir)
	if err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(physical, "src", "pkg")
	nested := filepath.Join(physical, "nested")
	for _, dir := range []string{subdir, filepath.Join(nested, ".slb")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	outside := t.TempDir()
	rootAlias := filepath.Join(outside, "root-alias")
	subdirAlias := filepath.Join(outside, "descendant-alias")
	nestedAlias := filepath.Join(outside, "nested-alias")
	for alias, target := range map[string]string{rootAlias: physical, subdirAlias: subdir, nestedAlias: nested} {
		if err := os.Symlink(target, alias); err != nil {
			if runtime.GOOS == "windows" {
				t.Skipf("creating symlinks requires Windows developer mode or privilege: %v", err)
			}
			t.Fatal(err)
		}
	}
	paths := []string{physical, subdir, rootAlias, subdirAlias, nestedAlias, filepath.Join(rootAlias, "missing"), outside}
	stdout := runGeneratedHookPython(t, filepath.Join(scriptDir, "slb_guard.py"),
		"import json\nprint(json.dumps([g['get_socket_path'](path) for path in sys.argv[2:]]))", paths...)
	var sockets []string
	if err := json.Unmarshal([]byte(stdout), &sockets); err != nil || len(sockets) != len(paths) {
		t.Fatalf("invalid generated socket results: %q, %v", stdout, err)
	}
	for i, path := range paths {
		if want := daemon.SocketPathForCWD(path); sockets[i] != want {
			t.Errorf("Python/Go socket mismatch for %q: got %q, want %q", path, sockets[i], want)
		}
	}
	for _, i := range []int{1, 2, 3} {
		if sockets[i] != sockets[0] {
			t.Errorf("same project alias has a distinct socket: %q vs %q", sockets[i], sockets[0])
		}
	}
	if sockets[4] == sockets[0] {
		t.Fatal("nested project incorrectly shares its parent socket")
	}
}

// Regression for the integration gap caught while reviewing #2/#5:
// `slb hook generate` must merge persisted custom_patterns into
// the engine before emitting the script. Without the loader call,
// the embedded fallback would only ever enforce the 52 builtins,
// even after a user runs `slb patterns add`.
func TestHookGenerateCommand_IncludesPersistedCustomPatterns(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHookFlags()

	// Persist a custom pattern via the patterns add command.
	resetPatternsFlags()
	addCmd := newTestPatternsCmd(h.DBPath)
	uniqPattern := `^uniq-hook-gen-include-marker-pattern-x9q$`
	if _, err := executeCommandCapture(t, addCmd, "patterns", "add",
		uniqPattern,
		"-T", "dangerous", "-r", "regression for hook-generate inclusion", "-j",
	); err != nil {
		t.Fatalf("patterns add: %v", err)
	}

	// Now generate the hook. The generated script must contain
	// the custom pattern alongside the builtins.
	resetHookFlags()
	tmpDir := t.TempDir()
	hookCmd := newTestHookCmd(h.DBPath)
	if _, err := executeCommandCapture(t, hookCmd, "hook", "generate",
		"--output-dir", tmpDir, "-j",
	); err != nil {
		t.Fatalf("hook generate: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(tmpDir, "slb_guard.py"))
	if err != nil {
		t.Fatalf("read generated script: %v", err)
	}

	if !strings.Contains(string(body), uniqPattern) {
		t.Errorf("hook generate did not include the persisted custom pattern in the embedded script.\n"+
			"  expected to find: %s\n"+
			"  This means `slb patterns add` is silent about the hook fallback path:\n"+
			"  the daemon sees the pattern (after a daemon-side fix) but the offline\n"+
			"  fallback never does.", uniqPattern)
	}
}

func TestHookStatusCommand_DetectsStalePatternSnapshot(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHookFlags()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	installCmd := newTestHookCmd(h.DBPath)
	if _, err := executeCommandCapture(t, installCmd, "hook", "install", "-j"); err != nil {
		t.Fatalf("install hook: %v", err)
	}

	scriptPath := filepath.Join(home, ".slb", "hooks", "slb_guard.py")
	body, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(body), "\n")
	replaced := false
	for i, line := range lines {
		if strings.HasPrefix(line, "# SHA256: ") {
			lines[i] = "# SHA256: " + strings.Repeat("0", 64)
			replaced = true
			break
		}
	}
	if !replaced {
		t.Fatal("generated hook did not contain a policy hash")
	}
	if err := os.WriteFile(scriptPath, []byte(strings.Join(lines, "\n")), 0o755); err != nil {
		t.Fatal(err)
	}

	resetHookFlags()
	statusCmd := newTestHookCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, statusCmd, "hook", "status", "-j")
	if err != nil {
		t.Fatalf("hook status: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode status: %v\n%s", err, stdout)
	}
	if result["status"] != "stale" || result["pattern_hash_matches"] != false ||
		result["installed_pattern_hash"] != strings.Repeat("0", 64) {
		t.Fatalf("stale snapshot was not detected: %+v", result)
	}
}

func TestHookTestCommand_UnknownRequiresOfflineConfirmation(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHookFlags()
	cmd := newTestHookCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "hook", "test", "ordinary-unmatched-command", "--local-only", "-j")
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode hook test: %v\n%s", err, stdout)
	}
	if result["action"] != "ask" || result["tier"] != "unknown" {
		t.Fatalf("unknown local command did not require confirmation: %+v", result)
	}
}

func TestHookHealthCommand_UnreachableDaemonReportsFallback(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHookFlags()
	cmd := newTestHookCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "-C", h.ProjectDir, "hook", "health", "-j")
	if err != nil {
		t.Fatalf("hook health: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode health: %v\n%s", err, stdout)
	}
	if result["status"] != "unreachable" || result["daemon_reachable"] != false ||
		result["fallback_available"] != true || result["healthy"] != false {
		t.Fatalf("unexpected offline health result: %+v", result)
	}
	if result["socket_path"] != daemon.SocketPathForCWD(h.ProjectDir) {
		t.Fatalf("health targeted wrong project socket: %+v", result)
	}
}

func TestHookTestCommand_SimulatedFailureUsesFallback(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHookFlags()
	cmd := newTestHookCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "-C", h.ProjectDir, "hook", "test",
		"ordinary-unmatched-command", "--simulate-failure", "-j")
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode test: %v\n%s", err, stdout)
	}
	if result["action"] != "ask" || result["source"] != "local" ||
		result["fallback"] != true || result["simulated_failure"] != true {
		t.Fatalf("simulated failure did not exercise fallback: %+v", result)
	}
}

func TestHookCautionPolicyEmbeddedAndLocal(t *testing.T) {
	engine := core.NewPatternEngine()
	askScript := generateHookScriptWithCautionAction(engine, "ask")
	if !strings.Contains(askScript, `HOOK_CAUTION_ACTION = "ask"`) || embeddedHookCautionAction([]byte(askScript)) != "ask" {
		t.Fatal("generated hook lost explicit CAUTION ask policy")
	}
	blockScript := generateHookScriptWithCautionAction(engine, "invalid")
	if embeddedHookCautionAction([]byte(blockScript)) != "block" {
		t.Fatal("invalid generated CAUTION policy did not fail closed")
	}
	if got := localHookTestResultWithCautionAction("rm build.cache", "", "block"); got["action"] != "block" {
		t.Fatalf("local block policy ignored: %+v", got)
	}
	if got := localHookTestResultWithCautionAction("rm build.cache", "", "ask"); got["action"] != "ask" {
		t.Fatalf("local ask policy ignored: %+v", got)
	}
}

func nativeHookOutput(t *testing.T, payload map[string]any, home string) map[string]any {
	t.Helper()
	t.Setenv("HOME", home)
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader(string(data)))
	var stdout strings.Builder
	cmd.SetOut(&stdout)
	if err := runHookGuard(cmd, nil); err != nil {
		t.Fatalf("native hook guard: %v", err)
	}
	var output map[string]any
	if err := json.Unmarshal([]byte(stdout.String()), &output); err != nil {
		t.Fatalf("decode native hook output: %v\n%s", err, stdout.String())
	}
	return output
}

func nativeHookPermission(t *testing.T, output map[string]any) (string, map[string]any) {
	t.Helper()
	specific, ok := output["hookSpecificOutput"].(map[string]any)
	if !ok {
		t.Fatalf("missing hookSpecificOutput: %+v", output)
	}
	permission, _ := specific["permissionDecision"].(string)
	return permission, specific
}

func TestNativeHookGuardProtocolAndFallback(t *testing.T) {
	for _, tc := range []struct {
		name      string
		command   any
		want      string
		wantAudit bool
	}{
		{"safe", "git stash", "allow", false},
		{"caution stays in SLB", "rm build.cache", "deny", true},
		{"dangerous", "rm -rf node_modules", "deny", true},
		{"invalid command", 42, "ask", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			project := t.TempDir()
			output := nativeHookOutput(t, map[string]any{
				"session_id": "provider-session",
				"cwd":        project,
				"tool_input": map[string]any{"command": tc.command},
			}, home)
			permission, _ := nativeHookPermission(t, output)
			if permission != tc.want {
				t.Fatalf("permission=%q want=%q output=%+v", permission, tc.want, output)
			}
			auditDir := filepath.Join(home, ".slb", "audit", "blocked")
			entries, err := os.ReadDir(auditDir)
			if tc.wantAudit {
				if err != nil || len(entries) != 1 {
					t.Fatalf("expected one native audit record, entries=%d err=%v", len(entries), err)
				}
			} else if err == nil && len(entries) != 0 {
				t.Fatalf("unexpected audit records: %d", len(entries))
			}
		})
	}
}

func TestNativeHookGuardExecutionHandoff(t *testing.T) {
	project := t.TempDir()
	hookHandoffDatabase(t, project)
	hash := strings.Repeat("a", 64)
	response := &daemon.HookQueryResult{
		Action: "execute",
		ExecutionHandoff: &daemon.HookExecutionHandoff{
			RequestID: "req-123", CommandHash: hash, SessionID: "session-1",
			DatabasePath: filepath.Join(project, ".slb", "state.db"),
		},
	}
	cmd := &cobra.Command{}
	var stdout strings.Builder
	cmd.SetOut(&stdout)
	toolInput := map[string]any{
		"command": "rm -rf ./build", "timeout": json.Number("1500"), "description": "preserve me",
	}
	if err := emitNativeExecutionHandoff(cmd, response, toolInput, "session-1", project); err != nil {
		t.Fatal(err)
	}
	var output map[string]any
	if err := json.Unmarshal([]byte(stdout.String()), &output); err != nil {
		t.Fatal(err)
	}
	permission, specific := nativeHookPermission(t, output)
	if permission != "allow" {
		t.Fatalf("handoff permission=%q", permission)
	}
	updated, ok := specific["updatedInput"].(map[string]any)
	if !ok {
		t.Fatalf("missing updated input: %+v", specific)
	}
	command, _ := updated["command"].(string)
	for _, fragment := range []string{"exec ", " execute ", "--expected-command-hash", hash, "--timeout", "1", "req-123"} {
		if !strings.Contains(command, fragment) {
			t.Fatalf("handoff command missing %q: %s", fragment, command)
		}
	}
	if updated["description"] != "preserve me" {
		t.Fatalf("handoff discarded tool input: %+v", updated)
	}

	response.ExecutionHandoff.DatabasePath = filepath.Join(t.TempDir(), "wrong.db")
	if err := emitNativeExecutionHandoff(&cobra.Command{}, response, toolInput, "session-1", project); err == nil {
		t.Fatal("cross-project execution handoff was accepted")
	}
}

func TestNativeHookHandoffUsesPhysicalProjectDatabase(t *testing.T) {
	project, other := t.TempDir(), t.TempDir()
	databasePath := hookHandoffDatabase(t, project)
	otherDatabase := hookHandoffDatabase(t, other)
	nested := filepath.Join(project, "src")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	rootAlias := filepath.Join(t.TempDir(), "root-alias")
	descendantAlias := filepath.Join(other, "descendant-alias")
	for alias, target := range map[string]string{rootAlias: project, descendantAlias: nested} {
		if err := os.Symlink(target, alias); err != nil {
			t.Skipf("directory symlinks unavailable: %v", err)
		}
	}
	linkedProject, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(linkedProject, ".slb"), 0o700); err != nil {
		t.Fatal(err)
	}
	linkedDatabase := filepath.Join(linkedProject, ".slb", "state.db")
	if err := os.Symlink(databasePath, linkedDatabase); err != nil {
		t.Skipf("file symlinks unavailable: %v", err)
	}
	for _, tc := range []struct {
		name, cwd, database, logDirectory string
		valid                             bool
	}{
		{"root alias with alias DB", rootAlias, filepath.Join(rootAlias, ".slb", "state.db"), "", true},
		{"root alias with physical DB", rootAlias, databasePath, "", true},
		{"descendant alias with physical DB", descendantAlias, databasePath, "", true},
		{"file-only DB symlink preserves project logs", linkedProject, linkedDatabase, filepath.Join(linkedProject, ".slb", "logs"), true},
		{"descendant alias with lexical parent DB", descendantAlias, otherDatabase, "", false},
		{"root alias with foreign DB", rootAlias, otherDatabase, "", false},
		{"unavailable cwd", filepath.Join(rootAlias, "missing"), databasePath, "", false},
		{"unavailable DB", rootAlias, filepath.Join(project, ".slb", "missing.db"), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &daemon.HookQueryResult{Action: "execute", ExecutionHandoff: &daemon.HookExecutionHandoff{
				RequestID: "req-alias", CommandHash: strings.Repeat("a", 64), SessionID: "session-1", DatabasePath: tc.database,
			}}
			cmd := &cobra.Command{}
			var stdout strings.Builder
			cmd.SetOut(&stdout)
			err := emitNativeExecutionHandoff(cmd, response, map[string]any{"command": "git reset --hard"}, "session-1", tc.cwd)
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
				var payload struct {
					Output struct {
						Input map[string]any `json:"updatedInput"`
					} `json:"hookSpecificOutput"`
				}
				if err := json.Unmarshal([]byte(stdout.String()), &payload); err != nil {
					t.Fatal(err)
				}
				command, _ := payload.Output.Input["command"].(string)
				if !strings.Contains(command, databasePath) || !strings.Contains(command, strings.Repeat("a", 64)) {
					t.Fatalf("valid alias handoff lost its database or command hash: %s", command)
				}
				logDirectory := tc.logDirectory
				if logDirectory == "" {
					logDirectory = filepath.Join(filepath.Dir(databasePath), "logs")
				}
				if !strings.Contains(command, "--log-dir "+hookShellQuote(logDirectory)) {
					t.Fatalf("handoff moved logs outside the selected project's state: %s", command)
				}
			} else if err == nil || stdout.Len() != 0 {
				t.Fatalf("invalid database handoff was accepted: %s, %v", stdout.String(), err)
			}
		})
	}
}

func TestNativeHookFallbackRejectsUnavailableProjectPolicy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	for _, tc := range []struct {
		name, marker string
	}{
		{"missing database", ".slb"},
		{"database directory", filepath.Join(".slb", "state.db")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := t.TempDir()
			if err := os.MkdirAll(filepath.Join(project, tc.marker), 0o700); err != nil {
				t.Fatal(err)
			}
			if result, err := classifyNativeHookFallback("git stash", project); err == nil {
				t.Fatalf("unavailable project state fell back to safe defaults: %+v", result)
			}
		})
	}
	if result, err := classifyNativeHookFallback("git stash", filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatalf("unavailable cwd fell back to safe defaults: %+v", result)
	}
}

func TestNativeHookFallbackFollowsPhysicalProjectThroughAliases(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	project, other := t.TempDir(), t.TempDir()
	for _, root := range []string{project, other} {
		conn, err := db.OpenAndMigrate(filepath.Join(root, ".slb", "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		if root == project {
			if _, err := conn.InsertCustomPattern("critical", "^alias-db-wipe$", "project A rule", "human"); err != nil {
				conn.Close()
				t.Fatal(err)
			}
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}
	nested := filepath.Join(project, "src")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(project, ".slb", "config.toml")
	if err := os.WriteFile(configPath, []byte("[patterns.critical]\npatterns = ['^alias-config-wipe$']\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, ".slb", "config.toml"),
		[]byte("[patterns.safe]\npatterns = ['^alias-config-wipe$', '^alias-db-wipe$']\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rootAlias := filepath.Join(t.TempDir(), "root-alias")
	externalAlias := filepath.Join(t.TempDir(), "descendant-alias")
	otherAlias := filepath.Join(other, "descendant-alias")
	for alias, target := range map[string]string{rootAlias: project, externalAlias: nested, otherAlias: nested} {
		if err := os.Symlink(target, alias); err != nil {
			t.Skipf("directory symlinks unavailable: %v", err)
		}
	}
	for _, command := range []string{"alias-config-wipe", "alias-db-wipe"} {
		control, err := classifyNativeHookFallback(command, other)
		if err != nil || control.Action != "allow" {
			t.Fatalf("project B control did not use its safe policy: %+v, %v", control, err)
		}
		for _, tc := range []struct{ name, cwd string }{
			{"physical", nested}, {"root alias", rootAlias},
			{"external descendant alias", externalAlias}, {"alias below project B", otherAlias},
		} {
			t.Run(command+"/"+tc.name, func(t *testing.T) {
				result, err := classifyNativeHookFallback(command, tc.cwd)
				if err != nil || result.Action != "block" || result.Tier != "critical" {
					t.Fatalf("project A's offline policy was bypassed through %q: %+v, %v", tc.cwd, result, err)
				}
			})
		}
	}
	if err := os.WriteFile(configPath, []byte("[patterns.critical\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{nested, externalAlias, otherAlias} {
		if result, err := classifyNativeHookFallback("alias-config-wipe", cwd); err == nil {
			t.Errorf("unavailable project A policy did not fail closed through %q: %+v", cwd, result)
		}
	}
}

func TestHookInstallUsesNativeGuard(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHookFlags()
	home := t.TempDir()
	t.Setenv("HOME", home)

	cmd := newTestHookCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "hook", "install", "-j")
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if result["native_guard"] != true {
		t.Fatalf("install did not report native guard: %+v", result)
	}
	configured, _ := result["hook_command"].(string)
	if !strings.Contains(configured, " hook guard") || !isSLBHookCommand(configured, "") {
		t.Fatalf("install did not configure native guard: %q", configured)
	}

	data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "python3") {
		t.Fatalf("installed hot path still invokes Python: %s", data)
	}
	if !strings.Contains(string(data), "hook guard") {
		t.Fatalf("native guard missing from settings: %s", data)
	}
	if _, err := os.Stat(filepath.Join(home, ".slb", "hooks", "slb_guard.py")); err != nil {
		t.Fatalf("standalone fallback guard was not generated: %v", err)
	}
}

func TestNativeHookCommandRecognition(t *testing.T) {
	command, err := nativeHookGuardCommand()
	if err != nil {
		t.Fatal(err)
	}
	if !isSLBHookCommand(command, "") {
		t.Fatalf("current executable guard was not recognized: %q", command)
	}
	if isSLBHookCommand("echo hook guard", "") {
		t.Fatal("unrelated hook guard command was recognized as SLB")
	}
	if !isSLBHookCommand("python3 /tmp/slb_guard.py", "/tmp/slb_guard.py") {
		t.Fatal("legacy Python guard was not recognized")
	}
}

func TestHookInstallAutoUpgradesLegacyPythonGuard(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHookFlags()
	home := t.TempDir()
	t.Setenv("HOME", home)

	legacyScript := filepath.Join(home, ".slb", "hooks", "slb_guard.py")
	if err := os.MkdirAll(filepath.Dir(legacyScript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyScript, []byte("# legacy\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	settingsDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []any{
				map[string]any{
					"matcher": "Bash",
					"hooks": []any{
						map[string]any{"type": "command", "command": "python3 " + legacyScript},
					},
				},
			},
		},
	}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newTestHookCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "hook", "install", "-j")
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if result["upgraded"] != true || result["already_existed"] != false {
		t.Fatalf("legacy registration was not reported upgraded: %+v", result)
	}
	updated, err := os.ReadFile(filepath.Join(settingsDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(updated), "python3") || !strings.Contains(string(updated), "hook guard") {
		t.Fatalf("legacy registration was not migrated to native guard: %s", updated)
	}

	resetHookFlags()
	second := newTestHookCmd(h.DBPath)
	stdout, err = executeCommandCapture(t, second, "hook", "install", "-j")
	if err != nil {
		t.Fatal(err)
	}
	result = map[string]any{}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if result["upgraded"] != false || result["already_existed"] != true {
		t.Fatalf("current native registration was not idempotent: %+v", result)
	}
}

// fakeHookDaemon answers hook_query on the project's socket after delay; a
// negative delay accepts the connection but never answers (a stalled daemon).
func fakeHookDaemon(t *testing.T, project string, delay time.Duration) {
	t.Helper()
	socketPath := daemon.SocketPathForCWD(project)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen %s: %v", socketPath, err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		close(done)
		_ = listener.Close()
		_ = os.Remove(socketPath)
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				scanner := bufio.NewScanner(conn)
				for scanner.Scan() {
					var req struct {
						ID     int64  `json:"id"`
						Method string `json:"method"`
					}
					if json.Unmarshal(scanner.Bytes(), &req) != nil {
						return
					}
					if delay < 0 {
						<-done
						return
					}
					select {
					case <-time.After(delay):
					case <-done:
						return
					}
					reply, _ := json.Marshal(map[string]any{"id": req.ID, "result": map[string]any{
						"action": "allow", "message": "No matching pattern", "tier": "",
					}})
					if _, err := conn.Write(append(reply, '\n')); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
}

func shortProjectDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "slbhq")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// GitHub #21: a healthy daemon that needs more than the old fixed 50ms must
// still answer; an unmatched command must not become a human prompt.
func TestNativeHookGuardWaitsForSlowButHealthyDaemon(t *testing.T) {
	home := t.TempDir()
	project := shortProjectDir(t)
	fakeHookDaemon(t, project, 80*time.Millisecond)
	output := nativeHookOutput(t, map[string]any{
		"session_id": "provider-session", "cwd": project,
		"tool_input": map[string]any{"command": "frobnicate --report"},
	}, home)
	if permission, specific := nativeHookPermission(t, output); permission != "allow" {
		t.Fatalf("slow daemon answer was discarded: permission=%q %+v", permission, specific)
	}
}

// A stalled daemon still falls back to the fail-closed local policy, within
// the configured deadline, and the audit record says it was a deadline.
func TestNativeHookGuardDeadlineFallsBackFailClosedAndIsDiagnosable(t *testing.T) {
	home := t.TempDir()
	project := shortProjectDir(t)
	fakeHookDaemon(t, project, -1)
	t.Setenv("SLB_HOOK_QUERY_TIMEOUT_MS", "40")
	started := time.Now()
	output := nativeHookOutput(t, map[string]any{
		"session_id": "provider-session", "cwd": project,
		"tool_input": map[string]any{"command": "frobnicate --report"},
	}, home)
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("guard did not honor the configured deadline: %s", elapsed)
	}
	permission, specific := nativeHookPermission(t, output)
	if permission != "ask" {
		t.Fatalf("stalled daemon must fall back to confirmation, got %q %+v", permission, specific)
	}
	reason, _ := specific["permissionDecisionReason"].(string)
	if !strings.Contains(reason, "did not answer within 40ms") || !strings.Contains(reason, "hook_query_timeout_ms") {
		t.Fatalf("fallback reason does not explain the deadline: %q", reason)
	}
	entries, err := os.ReadDir(filepath.Join(home, ".slb", "audit", "blocked"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one audit record, entries=%d err=%v", len(entries), err)
	}
	record, err := os.ReadFile(filepath.Join(home, ".slb", "audit", "blocked", entries[0].Name()))
	if err != nil || !strings.Contains(string(record), `"source":"hook_native_timeout"`) {
		t.Fatalf("audit record does not identify a deadline: %s (%v)", record, err)
	}

	// Dangerous commands are still denied on the deadline path.
	output = nativeHookOutput(t, map[string]any{
		"session_id": "provider-session", "cwd": project,
		"tool_input": map[string]any{"command": "rm -rf node_modules"},
	}, home)
	if permission, _ := nativeHookPermission(t, output); permission != "deny" {
		t.Fatalf("dangerous command not denied after deadline: %q", permission)
	}
}

func TestNativeHookGuardUnreachableDaemonIsRecordedOffline(t *testing.T) {
	home := t.TempDir()
	project := shortProjectDir(t)
	output := nativeHookOutput(t, map[string]any{
		"session_id": "provider-session", "cwd": project,
		"tool_input": map[string]any{"command": "frobnicate --report"},
	}, home)
	if permission, _ := nativeHookPermission(t, output); permission != "ask" {
		t.Fatalf("unreachable daemon must fall back to confirmation, got %q", permission)
	}
	entries, err := os.ReadDir(filepath.Join(home, ".slb", "audit", "blocked"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one audit record, entries=%d err=%v", len(entries), err)
	}
	record, _ := os.ReadFile(filepath.Join(home, ".slb", "audit", "blocked", entries[0].Name()))
	if !strings.Contains(string(record), `"source":"hook_native_offline"`) {
		t.Fatalf("unreachable daemon not recorded as offline: %s", record)
	}
}

func TestHookQueryTimeoutConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := t.TempDir()
	if got := hookQueryTimeout(project); got != 250*time.Millisecond {
		t.Fatalf("default hook query timeout = %s, want 250ms", got)
	}
	t.Setenv("SLB_HOOK_QUERY_TIMEOUT_MS", "900")
	if got := hookQueryTimeout(project); got != 900*time.Millisecond {
		t.Fatalf("configured hook query timeout = %s, want 900ms", got)
	}
	t.Setenv("SLB_HOOK_QUERY_TIMEOUT_MS", "5")
	if got := hookQueryTimeout(project); got != 250*time.Millisecond {
		t.Fatalf("out-of-range timeout must use the default, got %s", got)
	}
}

func TestHookQueryTimeoutUsesPhysicalProject(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	project, other := t.TempDir(), t.TempDir()
	for root, timeout := range map[string]string{project: "900", other: "1200"} {
		if err := os.Mkdir(filepath.Join(root, ".slb"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".slb", "config.toml"),
			[]byte("[integrations]\nhook_query_timeout_ms = "+timeout+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	nested := filepath.Join(project, "src")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(other, "descendant-alias")
	if err := os.Symlink(nested, alias); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	if got := hookQueryTimeout(alias); got != 900*time.Millisecond {
		t.Fatalf("query timeout selected the lexical parent's policy: %s", got)
	}
}
