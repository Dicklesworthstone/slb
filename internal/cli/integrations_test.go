package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/integrations"
	"github.com/spf13/cobra"
)

func TestClaudeHooksCmd_Flags(t *testing.T) {
	// Test that the command has correct flags
	cmd := claudeHooksCmd

	if cmd.Use != "claude-hooks" {
		t.Errorf("expected Use to be 'claude-hooks', got %s", cmd.Use)
	}

	// Verify flags exist
	flagNames := []string{"install", "preview", "merge", "global"}
	for _, name := range flagNames {
		f := cmd.Flags().Lookup(name)
		if f == nil {
			t.Errorf("expected flag %s to exist", name)
		}
	}

	// Verify default values
	if merge := cmd.Flags().Lookup("merge"); merge != nil {
		if merge.DefValue != "true" {
			t.Errorf("expected merge default to be true, got %s", merge.DefValue)
		}
	}
}

func TestCursorRulesCmd_Flags(t *testing.T) {
	// Test that the command has correct flags
	cmd := cursorRulesCmd

	if cmd.Use != "cursor-rules" {
		t.Errorf("expected Use to be 'cursor-rules', got %s", cmd.Use)
	}

	// Verify flags exist
	flagNames := []string{"install", "preview", "append", "replace"}
	for _, name := range flagNames {
		f := cmd.Flags().Lookup(name)
		if f == nil {
			t.Errorf("expected flag %s to exist", name)
		}
	}

	// Verify default values
	if append := cmd.Flags().Lookup("append"); append != nil {
		if append.DefValue != "true" {
			t.Errorf("expected append default to be true, got %s", append.DefValue)
		}
	}
}

func TestIntegrationsCmd_Subcommands(t *testing.T) {
	// Test that the integrations command has both subcommands
	found := make(map[string]bool)
	for _, cmd := range integrationsCmd.Commands() {
		found[cmd.Name()] = true
	}

	expected := []string{"claude-hooks", "cursor-rules"}
	for _, name := range expected {
		if !found[name] {
			t.Errorf("expected subcommand %s to be registered", name)
		}
	}
}

func TestClaudeHooksIntegration_Install(t *testing.T) {
	for _, merge := range []string{"true", "false"} {
		t.Run("merge="+merge, func(t *testing.T) {
			project, home := t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("SLB_PROJECT", "")
			initializeClaudeHookTestProject(t, project)
			stdout, err := runClaudeHooksCommand(t, project, "--install", "--merge="+merge, "--json")
			if err != nil {
				t.Fatalf("install: %v\n%s", err, stdout)
			}
			var result map[string]any
			if err := json.Unmarshal([]byte(stdout), &result); err != nil {
				t.Fatalf("install response: %v\n%s", err, stdout)
			}
			path := filepath.Join(project, ".claude", "settings.json")
			if result["settings_path"] != path || result["status"] != "installed" {
				t.Fatalf("alias did not use the shared scoped installer: %#v", result)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			assertClaudeNativeSettings(t, data)
			for _, absent := range []string{filepath.Join(project, ".claude", "hooks.json"), filepath.Join(home, ".claude", "settings.json")} {
				if _, err := os.Stat(absent); !os.IsNotExist(err) {
					t.Fatalf("project install wrote obsolete or global configuration %s: %v", absent, err)
				}
			}
			if _, err := runClaudeHooksCommand(t, project, "--install", "--json"); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(data, after) {
				t.Fatalf("reinstall changed settings: %v\nbefore: %s\nafter: %s", err, data, after)
			}
		})
	}
}

func TestClaudeHooksIntegration_Merge(t *testing.T) {
	for _, tc := range []struct {
		name, existing string
		merge          bool
	}{
		{"missing hooks section", `{"env":{"TEAM":"keep"}}`, true},
		{"sibling hook", `{"env":{"TEAM":"keep"},"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo custom"}]}]}}`, true},
		{"reset only SLB", `{"env":{"TEAM":"keep"},"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"slb hook guard","timeout":50},{"type":"command","command":"echo custom"}]}]}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := t.TempDir()
			t.Setenv("HOME", t.TempDir())
			t.Setenv("SLB_PROJECT", "")
			initializeClaudeHookTestProject(t, project)
			path := filepath.Join(project, ".claude", "settings.json")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.existing), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"--install", "--json"}
			if !tc.merge {
				args = append(args, "--merge=false")
			}
			if stdout, err := runClaudeHooksCommand(t, project, args...); err != nil {
				t.Fatalf("merge: %v\n%s", err, stdout)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			assertClaudeNativeSettings(t, data)
			var settings map[string]any
			if err := json.Unmarshal(data, &settings); err != nil {
				t.Fatal(err)
			}
			if env, ok := settings["env"].(map[string]any); !ok || env["TEAM"] != "keep" {
				t.Fatalf("unrelated settings replaced: %s", data)
			}
			if strings.Contains(tc.existing, "echo custom") && !strings.Contains(string(data), "echo custom") {
				t.Fatalf("unrelated Bash hook replaced: %s", data)
			}
			if !tc.merge && strings.Contains(string(data), `"timeout"`) {
				t.Fatalf("--merge=false did not reset SLB-owned options: %s", data)
			}
		})
	}
}

func TestClaudeHooksIntegration_RefusesUninitializedProjectWithoutWrites(t *testing.T) {
	for _, existingSettings := range []bool{false, true} {
		name := "new project"
		if existingSettings {
			name = "existing Claude settings"
		}
		t.Run(name, func(t *testing.T) {
			project, home := t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("SLB_PROJECT", "")
			settingsPath := filepath.Join(project, ".claude", "settings.json")
			const original = `{"env":{"TEAM":"keep"},"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo custom"}]}]}}`
			if existingSettings {
				if err := os.MkdirAll(filepath.Dir(settingsPath), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(settingsPath, []byte(original), 0600); err != nil {
					t.Fatal(err)
				}
			}
			stdout, err := runClaudeHooksCommand(t, project, "--install", "--merge=false", "--json")
			if err == nil || !strings.Contains(err.Error(), "slb init") || stdout != "" {
				t.Fatalf("uninitialized installation did not refuse with initialization guidance: %v\n%s", err, stdout)
			}
			if _, err := os.Stat(filepath.Join(project, ".slb")); !os.IsNotExist(err) {
				t.Fatalf("refusal created an incomplete SLB project marker: %v", err)
			}
			data, err := os.ReadFile(settingsPath)
			if existingSettings {
				if err != nil || string(data) != original {
					t.Fatalf("refusal changed existing settings: %v\n%s", err, data)
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("refusal created Claude settings: %v", err)
			}
			if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
				t.Fatalf("refusal wrote user-level configuration: %v, %v", entries, err)
			}
		})
	}
}

func initializeClaudeHookTestProject(t *testing.T, project string) {
	t.Helper()
	database, err := db.OpenAndMigrate(filepath.Join(project, ".slb", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCursorRulesIntegration_Generate(t *testing.T) {
	section := integrations.CursorRulesSection()

	// Verify section has markers
	if !strings.Contains(section, "slb:cursor-rules:start") {
		t.Error("expected start marker in section")
	}
	if !strings.Contains(section, "slb:cursor-rules:end") {
		t.Error("expected end marker in section")
	}

	// Verify key content
	expectedContent := []string{
		"slb patterns test",
		"slb request",
		"slb status",
		"slb execute",
		"CRITICAL",
		"DANGEROUS",
		"CAUTION",
	}
	for _, expected := range expectedContent {
		if !strings.Contains(section, expected) {
			t.Errorf("expected section to contain %q", expected)
		}
	}
}

func TestCursorRulesIntegration_Apply(t *testing.T) {
	// Test applying to empty content
	result, changed := integrations.ApplyCursorRules("", integrations.CursorRulesAppend)
	if !changed {
		t.Error("expected change for empty content")
	}
	if !strings.Contains(result, "slb:cursor-rules:start") {
		t.Error("expected section in result")
	}

	// Test appending to existing content
	existing := "# My Rules\nDo not use console.log.\n"
	result, changed = integrations.ApplyCursorRules(existing, integrations.CursorRulesAppend)
	if !changed {
		t.Error("expected change when appending")
	}
	if !strings.Contains(result, "My Rules") {
		t.Error("expected existing content preserved")
	}
	if !strings.Contains(result, "slb:cursor-rules:start") {
		t.Error("expected section appended")
	}

	// Test replace mode
	withSection := result
	result, changed = integrations.ApplyCursorRules(withSection, integrations.CursorRulesReplace)
	// Should still report changed since we're replacing
	if !strings.Contains(result, "slb:cursor-rules:start") {
		t.Error("expected section in replaced result")
	}
}

func TestCursorRulesIntegration_NoDouble(t *testing.T) {
	// Apply once
	section := integrations.CursorRulesSection()

	// Try to append again in append mode - should not change
	result, changed := integrations.ApplyCursorRules(section, integrations.CursorRulesAppend)
	if changed {
		t.Error("expected no change when section already present in append mode")
	}
	if result != section {
		t.Error("expected result to equal original")
	}
}

func TestClaudeHooksPreview(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SLB_PROJECT", "")
	for _, args := range [][]string{nil, {"--preview"}, {"--preview", "--global"}} {
		stdout, err := runClaudeHooksCommand(t, project, args...)
		if err != nil {
			t.Fatalf("preview %v: %v", args, err)
		}
		assertClaudeNativeSettings(t, []byte(stdout))
	}
	for _, dir := range []string{project, home} {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("preview wrote files in %s: %v, %v", dir, entries, err)
		}
	}
}

func TestClaudeHooksIntegration_RejectsConflictingActions(t *testing.T) {
	project := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	if _, err := runClaudeHooksCommand(t, project, "--preview", "--install"); err == nil || !strings.Contains(err.Error(), "cannot be used together") {
		t.Fatalf("ambiguous install/preview was accepted: %v", err)
	}
	entries, err := os.ReadDir(project)
	if err != nil || len(entries) != 0 {
		t.Fatalf("ambiguous command wrote files: %v, %v", entries, err)
	}
}

func TestClaudeHooksIntegration_InvalidSettingsRemainUntouched(t *testing.T) {
	for _, content := range []string{`{"hooks":`, `{"hooks":{"PreToolUse":{}}}`} {
		t.Run(content, func(t *testing.T) {
			project := t.TempDir()
			t.Setenv("HOME", t.TempDir())
			path := filepath.Join(project, ".claude", "settings.json")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := runClaudeHooksCommand(t, project, "--install", "--merge=false", "--json"); err == nil {
				t.Fatal("unsupported settings were accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != content {
				t.Fatalf("invalid settings were overwritten: %v\n%s", err, after)
			}
		})
	}
}

func runClaudeHooksCommand(t *testing.T, project string, args ...string) (string, error) {
	t.Helper()
	root := newTestRootCmd()
	flagTOON = false
	group := &cobra.Command{Use: "integrations"}
	command := &cobra.Command{Use: claudeHooksCmd.Use, Args: claudeHooksCmd.Args, RunE: claudeHooksCmd.RunE}
	command.Flags().Bool("install", false, "install")
	command.Flags().Bool("preview", false, "preview")
	command.Flags().Bool("merge", true, "merge")
	command.Flags().Bool("global", false, "global")
	group.AddCommand(command)
	root.AddCommand(group)
	root.SetArgs(append([]string{"--project", project, "integrations", "claude-hooks"}, args...))
	var runErr error
	stdout := captureStdout(t, func() {
		root.SetOut(os.Stdout)
		runErr = root.Execute()
	})
	return stdout, runErr
}

func assertClaudeNativeSettings(t *testing.T, data []byte) {
	t.Helper()
	var settings struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("invalid settings JSON: %v\n%s", err, data)
	}
	if _, obsolete := settings.Hooks["pre_bash"]; obsolete {
		t.Fatalf("obsolete hook schema emitted: %s", data)
	}
	var entries []struct {
		Matcher string           `json:"matcher"`
		Hooks   []map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal(settings.Hooks["PreToolUse"], &entries); err != nil {
		t.Fatalf("missing current hook event: %v\n%s", err, data)
	}
	expected, err := nativeHookGuardCommand()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		for _, hook := range entry.Hooks {
			if hook["command"] == expected {
				count++
				if entry.Matcher != "Bash" || hook["type"] != "command" || hook["onFailure"] != "block" {
					t.Fatalf("native guard must synchronously inspect Bash and fail closed: %s", data)
				}
			}
		}
	}
	if count != 1 {
		t.Fatalf("expected one registration for the current executable, found %d: %s", count, data)
	}
}
