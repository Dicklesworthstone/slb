package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/testutil"
)

// jsonString renders s as a JSON string literal without HTML escaping, the
// way the settings writer does.
func jsonString(t *testing.T, s string) string {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// sharedBashEntrySettings mirrors the GitHub #18 report: a "Bash" entry shared
// by three tools (the SLB guard last), a separate "Bash|PowerShell" entry, and
// keys deliberately not in alphabetical order.
func sharedBashEntrySettings(slbCommand string) string {
	return `{
  "permissions": {
    "allow": [
      "Bash(ls:*)"
    ]
  },
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "python3 /home/u/.claude/hooks/heredoc_guard.py 2>&1 && echo <ok>"
          },
          {
            "type": "command",
            "command": "/home/u/.local/bin/rch",
            "timeout": 30
          },
          {
            "type": "command",
            "command": ` + slbCommand + `
          }
        ]
      },
      {
        "matcher": "Bash|PowerShell",
        "hooks": [
          {
            "type": "command",
            "command": "dcg"
          }
        ]
      }
    ],
    "Stop": []
  },
  "env": {
    "ZETA": "1",
    "ALPHA": "2"
  },
  "model": "opus",
  "cleanupPeriodDays": 1e3
}
`
}

func setupHookHome(t *testing.T, settings string) (home, settingsPath string) {
	t.Helper()
	resetHookFlags()
	home = t.TempDir()
	t.Setenv("HOME", home)
	settingsPath = filepath.Join(home, ".claude", "settings.json")
	if settings != "" {
		if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(settingsPath, []byte(settings), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	return home, settingsPath
}

func runHookCLI(t *testing.T, dbPath string, args ...string) map[string]any {
	t.Helper()
	resetHookFlags()
	stdout, err := executeCommandCapture(t, newTestHookCmd(dbPath), append([]string{"hook"}, append(args, "-j")...)...)
	if err != nil {
		t.Fatalf("slb hook %v: %v", args, err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("parse output of slb hook %v: %v\n%s", args, err, stdout)
	}
	return result
}

func TestHookInstallUpgradesOnlySLBHookInSharedEntry(t *testing.T) {
	h := testutil.NewHarness(t)
	home, settingsPath := setupHookHome(t, "")
	legacy := `"python3 ` + filepath.Join(home, ".slb", "hooks", "slb_guard.py") + `"`
	original := sharedBashEntrySettings(legacy)
	setupSettingsFile(t, settingsPath, original)

	result := runHookCLI(t, h.DBPath, "install", "--global")
	guard, _ := result["hook_command"].(string)
	if result["upgraded"] != true || result["settings_changed"] != true {
		t.Fatalf("legacy guard not reported as upgraded: %+v", result)
	}
	if result["sibling_hooks_preserved"] != float64(2) {
		t.Fatalf("expected the two sibling hooks to be reported preserved: %+v", result)
	}

	got, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	// Byte-for-byte: only SLB's own command changed. Sibling hooks, the other
	// entry, key order, number spelling and '&'/'<'/'>' are all untouched.
	want := sharedBashEntrySettings(jsonString(t, guard))
	if string(got) != want {
		t.Fatalf("settings changed beyond the SLB hook command.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if info, err := os.Stat(settingsPath); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("settings permissions not preserved: %v %v", info.Mode(), err)
	}

	// Second install is a no-op: nothing is rewritten.
	result = runHookCLI(t, h.DBPath, "install")
	if result["already_existed"] != true || result["upgraded"] != false || result["settings_changed"] != false {
		t.Fatalf("second install was not idempotent: %+v", result)
	}
	again, _ := os.ReadFile(settingsPath)
	if !bytes.Equal(again, got) {
		t.Fatalf("idempotent install rewrote settings:\n%s", again)
	}

	// Uninstall removes only the SLB hook object from the shared entry.
	result = runHookCLI(t, h.DBPath, "uninstall")
	if result["status"] != "uninstalled" || result["removed"] != true {
		t.Fatalf("uninstall did not report removal: %+v", result)
	}
	afterUninstall, _ := os.ReadFile(settingsPath)
	wantUninstalled := strings.Replace(want, `,
          {
            "type": "command",
            "command": `+jsonString(t, guard)+`
          }`, "", 1)
	if wantUninstalled == want {
		t.Fatal("test bug: expected-uninstall text was not derived")
	}
	if string(afterUninstall) != wantUninstalled {
		t.Fatalf("uninstall touched more than the SLB hook.\n--- got ---\n%s\n--- want ---\n%s", afterUninstall, wantUninstalled)
	}

	// A second uninstall finds nothing and leaves the file alone.
	result = runHookCLI(t, h.DBPath, "uninstall")
	if result["removed"] != false {
		t.Fatalf("second uninstall claimed a removal: %+v", result)
	}
	final, _ := os.ReadFile(settingsPath)
	if !bytes.Equal(final, afterUninstall) {
		t.Fatalf("no-op uninstall rewrote settings:\n%s", final)
	}
}

func setupSettingsFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestHookInstallAppendsToExistingBashEntry(t *testing.T) {
	h := testutil.NewHarness(t)
	_, settingsPath := setupHookHome(t, `{
	"hooks": {
		"PreToolUse": [
			{
				"matcher": "Read",
				"hooks": [{"type": "command", "command": "echo read"}]
			},
			{
				"matcher": "Bash",
				"hooks": [{"type": "command", "command": "/usr/local/bin/rch"}]
			}
		]
	}
}`)
	result := runHookCLI(t, h.DBPath, "install")
	if result["upgraded"] != false || result["already_existed"] != false || result["sibling_hooks_preserved"] != float64(1) {
		t.Fatalf("unexpected fresh install result: %+v", result)
	}
	guard := result["hook_command"].(string)
	data, _ := os.ReadFile(settingsPath)
	if !strings.Contains(string(data), "\n\t\t\"PreToolUse\"") {
		t.Fatalf("tab indentation of the original file was not preserved:\n%s", data)
	}
	root, err := decodeSettingsJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	hooks, _ := root.get("hooks")
	entries := hooks.(*settingsObject).values["PreToolUse"].([]any)
	if len(entries) != 2 {
		t.Fatalf("install created a new entry instead of joining the Bash entry: %s", data)
	}
	bash := entries[1].(*settingsObject).values["hooks"].([]any)
	if len(bash) != 2 || bash[0].(*settingsObject).values["command"] != "/usr/local/bin/rch" ||
		bash[1].(*settingsObject).values["command"] != guard {
		t.Fatalf("Bash entry should hold rch then the SLB guard: %s", data)
	}
	if !strings.HasPrefix(string(data), "{\n\t\"hooks\"") || strings.HasSuffix(string(data), "\n") {
		t.Fatalf("file shape (indent / no trailing newline) not preserved:\n%q", data)
	}
}

func TestHookUninstallDropsEntryOnlyWhenSLBWasItsLastHook(t *testing.T) {
	h := testutil.NewHarness(t)
	home, settingsPath := setupHookHome(t, "")
	legacy := "python3 " + filepath.Join(home, ".slb", "hooks", "slb_guard.py")
	setupSettingsFile(t, settingsPath, `{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Edit",
        "hooks": [
          {
            "type": "command",
            "command": "fmt"
          }
        ]
      },
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": `+jsonString(t, legacy)+`
          }
        ]
      }
    ]
  }
}
`)
	result := runHookCLI(t, h.DBPath, "uninstall")
	if result["removed"] != true {
		t.Fatalf("legacy SLB hook not removed: %+v", result)
	}
	data, _ := os.ReadFile(settingsPath)
	want := `{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Edit",
        "hooks": [
          {
            "type": "command",
            "command": "fmt"
          }
        ]
      }
    ]
  }
}
`
	if string(data) != want {
		t.Fatalf("unexpected settings after uninstall:\n%s", data)
	}
}

func TestHookInstallCollapsesDuplicateSLBHooksAndKeepsSiblings(t *testing.T) {
	h := testutil.NewHarness(t)
	home, settingsPath := setupHookHome(t, "")
	legacy := "python3 " + filepath.Join(home, ".slb", "hooks", "slb_guard.py")
	setupSettingsFile(t, settingsPath, `{"hooks": {"PreToolUse": [
  {"matcher": "Bash", "hooks": [{"type": "command", "command": "/opt/other-slb/slb hook guard", "timeout": 5}, {"type": "command", "command": "keep-me"}]},
  {"matcher": "Bash", "hooks": [{"type": "command", "command": `+jsonString(t, legacy)+`}]}
]}}`)
	result := runHookCLI(t, h.DBPath, "install")
	if result["duplicate_slb_hooks_removed"] != float64(1) || result["upgraded"] != true {
		t.Fatalf("duplicates not collapsed: %+v", result)
	}
	guard := result["hook_command"].(string)
	data, _ := os.ReadFile(settingsPath)
	root, err := decodeSettingsJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	hooks, _ := root.get("hooks")
	entries := hooks.(*settingsObject).values["PreToolUse"].([]any)
	if len(entries) != 1 {
		t.Fatalf("emptied duplicate entry should be dropped, sibling entry kept: %s", data)
	}
	list := entries[0].(*settingsObject).values["hooks"].([]any)
	first := list[0].(*settingsObject)
	if len(list) != 2 || first.values["command"] != guard || list[1].(*settingsObject).values["command"] != "keep-me" {
		t.Fatalf("unexpected hooks after collapse: %s", data)
	}
	if first.values["timeout"] != json.Number("5") {
		t.Fatalf("extra keys on SLB's own hook should survive a non-force install: %s", data)
	}

	// --force resets SLB's own hook object only.
	resetHookFlags()
	if _, err := executeCommandCapture(t, newTestHookCmd(h.DBPath), "hook", "install", "--force", "-j"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(settingsPath)
	if strings.Contains(string(data), `"timeout"`) || !strings.Contains(string(data), "keep-me") {
		t.Fatalf("--force should reset only SLB's hook object: %s", data)
	}
}

func TestHookInstallRefusesMalformedHooksSection(t *testing.T) {
	h := testutil.NewHarness(t)
	original := `{"hooks": ["not", "an", "object"]}`
	_, settingsPath := setupHookHome(t, original)
	resetHookFlags()
	_, err := executeCommandCapture(t, newTestHookCmd(h.DBPath), "hook", "install", "-j")
	if err == nil || !strings.Contains(err.Error(), "refusing to rewrite") {
		t.Fatalf("expected refusal for a non-object hooks section, got %v", err)
	}
	data, _ := os.ReadFile(settingsPath)
	if string(data) != original {
		t.Fatalf("settings were modified despite the refusal: %s", data)
	}
}

func TestHookInstallWritesThroughSettingsSymlink(t *testing.T) {
	h := testutil.NewHarness(t)
	home, settingsPath := setupHookHome(t, "")
	target := filepath.Join(home, "dotfiles", "claude-settings.json")
	setupSettingsFile(t, target, "{}\n")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, settingsPath); err != nil {
		t.Fatal(err)
	}
	runHookCLI(t, h.DBPath, "install")
	info, err := os.Lstat(settingsPath)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("settings symlink was replaced: %v %v", info, err)
	}
	data, _ := os.ReadFile(target)
	if !strings.Contains(string(data), "hook guard") {
		t.Fatalf("symlink target not updated: %s", data)
	}
}

func TestMatcherCoversBash(t *testing.T) {
	for matcher, want := range map[string]bool{
		"": true, "*": true, "Bash": true, "Bash|PowerShell": true, ".*": true,
		"Read": false, "Bash2": false, "Edit|Write": false, "(": false,
	} {
		if got := matcherCoversBash(matcher); got != want {
			t.Errorf("matcherCoversBash(%q) = %v, want %v", matcher, got, want)
		}
	}
}

func TestSettingsJSONRoundTripPreservesOrderAndLiterals(t *testing.T) {
	original := "{\n  \"z\": 1.50,\n  \"a\": [\n    true,\n    null,\n    \"x && y > z\"\n  ],\n  \"m\": {}\n}"
	root, err := decodeSettingsJSON([]byte(original))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeSettingsJSON(root, settingsIndent([]byte(original)))
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != original {
		t.Fatalf("round trip changed the document:\n%s", encoded)
	}
	if _, err := decodeSettingsJSON([]byte(`{} {}`)); err == nil {
		t.Fatal("trailing JSON must be rejected")
	}
	if _, err := decodeSettingsJSON([]byte(`[]`)); err == nil {
		t.Fatal("non-object settings must be rejected")
	}
}
