package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Exercise the installed command itself, not only the shape of its generated
// configuration. All commands passed to the guard are inspected, never run.
func TestBuiltCLIHookInstallationIntegration(t *testing.T) {
	binary := buildIdentityBoundaryCLI(t)
	home, workspace := t.TempDir(), t.TempDir()
	project := filepath.Join(workspace, "selected project")
	otherProject := filepath.Join(workspace, "other project")
	projectSettings := filepath.Join(project, ".claude", "settings.json")
	globalSettings := filepath.Join(home, ".claude", "settings.json")
	otherSettings := filepath.Join(otherProject, ".claude", "settings.json")
	const sibling = `printf 'unrelated-guard'`
	const existing = `{
  "permissions": {"deny": ["Bash(example-dangerous *)"]},
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [
        {"type": "command", "command": "printf 'unrelated-guard'", "timeout": 7, "async": true},
        {"type": "command", "command": "'/old/location/slb' hook guard", "timeout": 45, "onFailure": "continue", "async": true, "asyncRewake": true}
      ]},
      {"matcher": "Read", "hooks": [{"type": "command", "command": "printf 'read-guard'"}]}
    ],
    "PostToolUse": [{"matcher": "Write", "hooks": [{"type": "command", "command": "printf 'formatter'"}]}]
  },
  "env": {"KEEP_ME": "unchanged"}
}
`
	const unrelated = `{"permissions":{"deny":["Bash(example-dangerous *)"]},"env":{"KEEP_ME":"unchanged"}}
`
	for path, data := range map[string]string{projectSettings: existing, globalSettings: unrelated, otherSettings: unrelated} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}

	run := func(directory string, extraEnv []string, args ...string) (string, string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, binary, args...)
		command.Dir = directory
		command.Env = append(identityCLIEnvironment(home), extraEnv...)
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		return stdout.String(), stderr.String(), err
	}
	jsonCLI := func(directory string, extraEnv []string, args ...string) map[string]any {
		t.Helper()
		stdout, stderr, err := run(directory, extraEnv, append(args, "--json")...)
		if err != nil {
			t.Fatalf("CLI %v failed: %v\nstdout=%s\nstderr=%s", args, err, stdout, stderr)
		}
		var view map[string]any
		if err := json.Unmarshal([]byte(stdout), &view); err != nil {
			t.Fatalf("CLI %v did not return one JSON object: %v\n%s\n%s", args, err, stdout, stderr)
		}
		return view
	}
	read := func(path string) ([]byte, map[string]any) {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var settings map[string]any
		if err := json.Unmarshal(data, &settings); err != nil {
			t.Fatalf("invalid settings at %s: %v", path, err)
		}
		return data, settings
	}
	unchanged := func(path string, want []byte) {
		t.Helper()
		got, _ := read(path)
		if !bytes.Equal(got, want) {
			t.Fatalf("editing another scope changed %s:\n%s", path, got)
		}
	}
	assertScope := func(view map[string]any, scope, path string) {
		t.Helper()
		if view["scope"] != scope || view["settings_path"] != path {
			t.Fatalf("incorrect installation scope: %+v", view)
		}
	}
	managed := func(settings map[string]any, present bool) map[string]any {
		t.Helper()
		var found []map[string]any
		hooks, _ := settings["hooks"].(map[string]any)
		entries, _ := hooks["PreToolUse"].([]any)
		for _, rawEntry := range entries {
			entry, _ := rawEntry.(map[string]any)
			if entry["matcher"] != "Bash" {
				continue
			}
			items, _ := entry["hooks"].([]any)
			for _, rawHook := range items {
				hook, _ := rawHook.(map[string]any)
				command, _ := hook["command"].(string)
				if strings.HasSuffix(command, " hook guard") {
					found = append(found, hook)
				}
			}
		}
		if !present {
			if len(found) != 0 {
				t.Fatalf("uninstall retained a managed hook: %+v", found)
			}
			return nil
		}
		if len(found) != 1 || found[0]["type"] != "command" || found[0]["onFailure"] != "block" {
			t.Fatalf("expected one native fail-closed PreToolUse hook: %+v", settings)
		}
		for _, flag := range []string{"async", "asyncRewake"} {
			if value, present := found[0][flag]; present && value != false {
				t.Fatalf("managed guard can finish after its protected action (%s): %+v", flag, found[0])
			}
		}
		return found[0]
	}
	_, original := read(projectSettings)
	preserveProjectSettings := func(settings map[string]any) {
		t.Helper()
		if !reflect.DeepEqual(settings["permissions"], original["permissions"]) || !reflect.DeepEqual(settings["env"], original["env"]) {
			t.Fatalf("SLB changed unrelated settings: %+v", settings)
		}
		hooks, _ := settings["hooks"].(map[string]any)
		originalHooks := original["hooks"].(map[string]any)
		if !reflect.DeepEqual(hooks["PostToolUse"], originalHooks["PostToolUse"]) {
			t.Fatal("SLB changed a different hook event")
		}
		entries, _ := hooks["PreToolUse"].([]any)
		var siblingFound, readFound bool
		for _, raw := range entries {
			entry, _ := raw.(map[string]any)
			if entry["matcher"] == "Read" {
				readFound = reflect.DeepEqual(entry, originalHooks["PreToolUse"].([]any)[1])
			}
			items, _ := entry["hooks"].([]any)
			for _, item := range items {
				hook, _ := item.(map[string]any)
				if hook["command"] == sibling {
					siblingFound = hook["type"] == "command" && hook["timeout"] == float64(7) && hook["async"] == true && len(hook) == 4
				}
			}
		}
		if !siblingFound || !readFound {
			t.Fatalf("SLB damaged another guard: %+v", settings)
		}
	}

	// The documented integrations entry point must preview the same current
	// registration as the main installer, without creating its artifacts.
	preview := jsonCLI(workspace, nil, "-C", project, "integrations", "claude-hooks", "--preview")
	previewHook := managed(preview, true)
	unchanged(projectSettings, []byte(existing))
	unchanged(globalSettings, []byte(unrelated))
	for _, path := range []string{filepath.Join(project, ".slb", "hooks"), filepath.Join(home, ".slb", "hooks"), filepath.Join(project, ".claude", "hooks.json")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("preview created an installation artifact: %s (%v)", path, err)
		}
	}
	// A fallback script must not manufacture a project marker with no policy
	// database. Both installers require real initialization before any writes.
	for _, args := range [][]string{{"hook", "install"}, {"integrations", "claude-hooks", "--install"}} {
		stdout, stderr, err := run(workspace, nil, append([]string{"-C", project}, args...)...)
		if err == nil || !strings.Contains(stdout+stderr, "slb init") {
			t.Fatalf("uninitialized project installation lacked safe recovery guidance: %v\n%s\n%s", err, stdout, stderr)
		}
		unchanged(projectSettings, []byte(existing))
		unchanged(globalSettings, []byte(unrelated))
		unchanged(otherSettings, []byte(unrelated))
		for _, path := range []string{filepath.Join(project, ".slb"), filepath.Join(home, ".slb", "hooks"), filepath.Join(project, ".claude", "hooks.json")} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("refused installation created an artifact: %s (%v)", path, err)
			}
		}
	}
	initialized := jsonCLI(workspace, nil, "-C", project, "init")
	if initialized["initialized"] != true || initialized["database"] != filepath.Join(project, ".slb", "state.db") {
		t.Fatalf("actual project initialization did not create its policy database: %+v", initialized)
	}
	installed := jsonCLI(workspace, nil, "-C", project, "integrations", "claude-hooks", "--install")
	assertScope(installed, "project", projectSettings)
	if installed["native_guard"] != true || installed["hook_script"] != filepath.Join(project, ".slb", "hooks", "slb_guard.py") {
		t.Fatalf("project installer used the wrong guard artifacts: %+v", installed)
	}
	projectBytes, projectView := read(projectSettings)
	projectHook := managed(projectView, true)
	if projectHook["command"] != previewHook["command"] || projectHook["timeout"] != float64(45) {
		t.Fatalf("install diverged from preview or erased a custom timeout: %+v", projectHook)
	}
	preserveProjectSettings(projectView)
	unchanged(globalSettings, []byte(unrelated))
	unchanged(otherSettings, []byte(unrelated))
	status := jsonCLI(workspace, nil, "-C", project, "hook", "status")
	assertScope(status, "project", projectSettings)
	if status["settings_configured"] != true || status["native_guard"] != true {
		t.Fatalf("selected installation is not discoverable: %+v", status)
	}
	repeated := jsonCLI(workspace, nil, "-C", project, "hook", "install")
	if repeated["settings_changed"] != false {
		t.Fatalf("unchanged installation was rewritten: %+v", repeated)
	}
	unchanged(projectSettings, projectBytes)

	// Run the exact shell command Claude Code receives. The daemon is absent,
	// so a dangerous request must be denied by the native offline classifier.
	for _, probe := range []struct{ command, decision string }{
		{"git stash", "allow"},
		{"git status", "ask"},
		{"rm -rf /", "deny"},
	} {
		input, err := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": project, "tool_input": map[string]any{"command": probe.command}})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		command := exec.CommandContext(ctx, "/bin/sh", "-c", projectHook["command"].(string))
		command.Dir, command.Env, command.Stdin = project, identityCLIEnvironment(home), bytes.NewReader(input)
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err = command.Run()
		cancel()
		var result struct {
			HookSpecificOutput struct {
				HookEventName      string `json:"hookEventName"`
				PermissionDecision string `json:"permissionDecision"`
			} `json:"hookSpecificOutput"`
		}
		if err != nil || json.Unmarshal(stdout.Bytes(), &result) != nil || result.HookSpecificOutput.HookEventName != "PreToolUse" || result.HookSpecificOutput.PermissionDecision != probe.decision {
			t.Fatalf("installed guard did not inspect %q correctly: %v\nstdout=%s\nstderr=%s", probe.command, err, stdout.String(), stderr.String())
		}
	}

	// A reset may replace SLB's object, never its neighboring guards/settings.
	jsonCLI(workspace, nil, "-C", project, "integrations", "claude-hooks", "--install", "--merge=false")
	projectBytes, projectView = read(projectSettings)
	if _, exists := managed(projectView, true)["timeout"]; exists {
		t.Fatal("reset retained the custom field on SLB's own hook")
	}
	preserveProjectSettings(projectView)

	global := jsonCLI(workspace, []string{"SLB_PROJECT=" + otherProject}, "hook", "install", "--global")
	assertScope(global, "global", globalSettings)
	if global["hook_script"] != filepath.Join(home, ".slb", "hooks", "slb_guard.py") {
		t.Fatalf("global installer used a project artifact: %+v", global)
	}
	globalBytes, globalView := read(globalSettings)
	managed(globalView, true)
	unchanged(projectSettings, projectBytes)
	unchanged(otherSettings, []byte(unrelated))
	globalStatus := jsonCLI(workspace, nil, "-C", project, "hook", "status", "--global")
	assertScope(globalStatus, "global", globalSettings)
	if globalStatus["settings_configured"] != true {
		t.Fatalf("global installation is not discoverable: %+v", globalStatus)
	}
	otherStatus := jsonCLI(workspace, []string{"SLB_PROJECT=" + otherProject}, "hook", "status")
	assertScope(otherStatus, "project", otherSettings)
	if otherStatus["settings_configured"] != false {
		t.Fatalf("selected-scope status borrowed a different installation: %+v", otherStatus)
	}

	removed := jsonCLI(workspace, nil, "-C", project, "hook", "uninstall")
	assertScope(removed, "project", projectSettings)
	_, projectView = read(projectSettings)
	managed(projectView, false)
	preserveProjectSettings(projectView)
	unchanged(globalSettings, globalBytes)
	projectBytes, _ = read(projectSettings)
	removed = jsonCLI(workspace, nil, "-C", project, "hook", "uninstall", "--global")
	assertScope(removed, "global", globalSettings)
	_, globalView = read(globalSettings)
	managed(globalView, false)
	if !reflect.DeepEqual(globalView["permissions"], original["permissions"]) || !reflect.DeepEqual(globalView["env"], original["env"]) {
		t.Fatalf("global uninstall changed unrelated settings: %+v", globalView)
	}
	unchanged(projectSettings, projectBytes)
	unchanged(otherSettings, []byte(unrelated))
	if _, err := os.Stat(filepath.Join(project, ".claude", "hooks.json")); !os.IsNotExist(err) {
		t.Fatalf("unified installer created obsolete hook configuration: %v", err)
	}
}
