package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	generateCmd.Flags().StringVarP(&flagHookOutputDir, "output", "o", "", "output directory")

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

	testCmd := &cobra.Command{
		Use:   "test [command]",
		Short: "Test hook behavior for a command",
		Args:  cobra.MaximumNArgs(1),
		RunE:  hookTestCmd.RunE,
	}

	hkCmd.AddCommand(generateCmd, installCmd, uninstallCmd, statusCmd, testCmd)
	root.AddCommand(hkCmd)

	return root
}

func resetHookFlags() {
	flagDB = ""
	flagOutput = "text"
	flagJSON = false
	flagProject = ""
	flagHookGlobal = false
	flagHookMerge = true
	flagHookForce = false
	flagHookOutputDir = ""
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
	stdout, err := executeCommandCapture(t, cmd, "hook", "generate", "-o", tmpDir, "-j")

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
	_, err := executeCommandCapture(t, cmd, "hook", "generate", "-o", tmpDir, "-j")
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
	if !strings.Contains(err.Error(), "command argument required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestHookTestCommand_SafeCommand(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHookFlags()

	cmd := newTestHookCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "hook", "test", "ls -la", "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["command"] != "ls -la" {
		t.Errorf("expected command='ls -la', got %v", result["command"])
	}
	if result["action"] != "allow" {
		t.Errorf("expected action='allow' for safe command, got %v", result["action"])
	}
}

func TestHookTestCommand_DangerousCommand(t *testing.T) {
	h := testutil.NewHarness(t)
	resetHookFlags()

	cmd := newTestHookCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "hook", "test", "rm -rf node_modules", "-j")

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
	stdout, err := executeCommandCapture(t, cmd, "hook", "test", "git push --force", "-j")

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
	stdout, err := executeCommandCapture(t, cmd, "hook", "test", "rm -rf /tmp/test", "-j")

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
							if strings.Contains(cmd, "slb_guard.py") {
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
	_, err := executeCommandCapture(t, cmd, "hook", "generate", "-o", tmpDir, "-j")
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
		"#!/usr/bin/env python3",         // Shebang
		"import re",                       // Regex import
		"import sys",                      // Sys import
		"import json",                     // JSON import
		"import socket",                   // Socket for daemon
		"SAFE_PATTERNS",                   // Pattern arrays
		"CAUTION_PATTERNS",
		"DANGEROUS_PATTERNS",
		"CRITICAL_PATTERNS",
		"def classify(command:",           // Classify function
		"def is_blocked(command:",         // Block check function
		"def query_slb_daemon",            // Daemon query function
		"SLB_SOCKET_PATH",                 // Socket path constant
		"def main():",                     // Entry point
		"if __name__ == \"__main__\":",    // Module guard
	}

	for _, essential := range essentials {
		if !strings.Contains(script, essential) {
			t.Errorf("expected script to contain %q", essential)
		}
	}
}
