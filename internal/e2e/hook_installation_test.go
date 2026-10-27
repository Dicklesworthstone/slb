package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

// Use the installed registration and its generated fallback with a real daemon
// and executor. Only the selected file opts in to same-host review; dropping it
// at any process boundary must never silently change the effective policy.
func TestBuiltCLIHookSelectedPolicyIntegration(t *testing.T) {
	binary := buildIdentityBoundaryCLI(t)
	f := newSameHostCLI(t, binary, 1, false)
	project, err := filepath.EvalSymlinks(f.ProjectDir)
	if err != nil {
		t.Fatal(err)
	}
	f.ProjectDir = project
	home, err := os.MkdirTemp("", "slbh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	f.home = home
	const base = `[general]
require_different_model = false
enable_dry_run = false
enable_rollback_capture = false
approval_policy = %q
[integrations]
agent_mail_enabled = false
[notifications]
desktop_enabled = false
`
	const patterns = `[patterns.dangerous]
patterns = ['^echo selected-hook-probe$', '^printf']
min_approvals = 1
`
	f.WriteFile(".slb/config.toml", []byte(fmt.Sprintf(base, "independent")), 0600)
	policyTarget := filepath.Join(f.ProjectDir, "policy source.toml")
	configPath := filepath.Join(f.ProjectDir, "selected 'policy'.toml")
	selected := []byte(fmt.Sprintf(base, "same_host") + patterns)
	writePolicy := func(data []byte) {
		t.Helper()
		if err := os.WriteFile(policyTarget, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writePolicy(selected)
	if err := os.Symlink(policyTarget, configPath); err != nil {
		t.Fatal(err)
	}
	installed := f.json("--config", filepath.Base(configPath), "hook", "install")
	native, ok := installed["hook_command"].(string)
	if !ok || !strings.Contains(native, "--config") {
		t.Fatalf("installed guard lost its explicitly selected policy: %+v", installed)
	}
	requester := f.session("Requester", "model-a")
	reviewer := f.session("Reviewer", "model-b")
	type hookOutput struct {
		HookSpecificOutput struct {
			Decision string         `json:"permissionDecision"`
			Reason   string         `json:"permissionDecisionReason"`
			Context  string         `json:"additionalContext"`
			Updated  map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	runShell := func(command, cwd string, input []byte) (string, string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
		cmd.Dir = cwd
		cmd.Env = append(identityCLIEnvironment(f.home), "SLB_SESSION_ID="+requester.ID,
			"PATH="+filepath.Dir(binary)+string(os.PathListSeparator)+os.Getenv("PATH"))
		cmd.Stdin = bytes.NewReader(input)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}
	guard := func(invocation, command, cwd, want string) hookOutput {
		t.Helper()
		input, err := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": cwd,
			"tool_input": map[string]any{"command": command, "timeout": 10000}})
		if err != nil {
			t.Fatal(err)
		}
		stdout, stderr, err := runShell(invocation, cwd, input)
		var result hookOutput
		if err != nil || json.Unmarshal([]byte(stdout), &result) != nil || result.HookSpecificOutput.Decision != want {
			t.Fatalf("guard should return %s for %q: %v\nstdout=%s\nstderr=%s", want, command, err, stdout, stderr)
		}
		return result
	}
	guards := []string{native}
	if python, err := exec.LookPath("python3"); err == nil {
		quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
		guards = append(guards, quote(python)+" "+quote(installed["hook_script"].(string)))
	} else {
		t.Log("python3 unavailable; native installed guard still covered")
	}

	// An offline invocation must read the selected custom rule. A missing or
	// malformed selected file cannot restore a weaker embedded/default policy.
	for _, invocation := range guards {
		result := guard(invocation, "echo selected-hook-probe", f.ProjectDir, "deny")
		if !strings.Contains(result.HookSpecificOutput.Reason, "same-host") {
			t.Fatalf("offline guard lost low-assurance labeling: %+v", result)
		}
		writePolicy([]byte("[invalid"))
		guard(invocation, "git stash", f.ProjectDir, "deny")
		writePolicy(selected)
		backup := configPath + ".saved"
		if err := os.Rename(configPath, backup); err != nil {
			t.Fatal(err)
		}
		guard(invocation, "git stash", f.ProjectDir, "deny")
		if err := os.Rename(backup, configPath); err != nil {
			t.Fatal(err)
		}
	}

	// A parent installation must not export its selected policy into a
	// separately initialized nested project, even when invoked from there.
	nested := filepath.Join(f.ProjectDir, "nested project")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	f.json("-C", nested, "init")
	f.WriteFile("nested project/.slb/config.toml", []byte(fmt.Sprintf(base, "independent")+
		"[patterns.safe]\npatterns = ['^echo selected-hook-probe$']\n"), 0600)
	for _, invocation := range guards {
		guard(invocation, "echo selected-hook-probe", nested, "allow")
	}

	running := false
	t.Cleanup(func() {
		if running {
			if _, stderr, err := f.run("daemon", "stop"); err != nil {
				t.Errorf("stopping hook test daemon: %v %s", err, stderr)
			}
		}
	})
	// Equivalent aliases may select the same file, but execution must keep
	// the hook's selected filename so a later replacement remains observable.
	f.json("--config", policyTarget, "daemon", "start")
	running = true
	createApproved := func(command string) *db.Request {
		t.Helper()
		view := f.json("--config", configPath, "request", command, "--session-id", requester.ID,
			"--reason", "Verify the installed hook execution handoff", "--expected-effect", "Write an isolated test marker",
			"--safety", "Only a temporary fixture is affected")
		id, ok := view["request_id"].(string)
		if !ok || view["status"] != "pending" {
			t.Fatalf("test command did not enter peer review: %+v", view)
		}
		f.json("--config", configPath, "approve", id, "--session-id", reviewer.ID, "--session-key", reviewer.SessionKey)
		request, err := f.DB.GetRequest(id)
		if err != nil || request.Status != db.StatusApproved {
			t.Fatalf("review did not persist: %+v %v", request, err)
		}
		return request
	}
	request := createApproved("/bin/sh -c \"printf reviewed >> 'hook marker'\"")
	_, reviews, err := f.DB.GetRequestWithReviews(request.ID)
	if err != nil || len(reviews) != 1 || reviews[0].Assurance != db.AssuranceSameHost {
		t.Fatalf("review evidence is missing: %+v %v", reviews, err)
	}
	var handoff string
	for _, invocation := range guards {
		result := guard(invocation, request.Command.Raw, f.ProjectDir, "allow")
		command, ok := result.HookSpecificOutput.Updated["command"].(string)
		if !ok || !strings.Contains(command, "--config") || !strings.Contains(result.HookSpecificOutput.Context, "same-host") {
			t.Fatalf("reviewed command lacks a policy-bound execution handoff: %+v", result)
		}
		handoff = command
	}
	stdout, stderr, err := runShell(handoff, f.ProjectDir, nil)
	var outcome map[string]any
	if err != nil || json.Unmarshal([]byte(stdout), &outcome) != nil || outcome["status"] != "executed" || outcome["exit_code"] != float64(0) || outcome["assurance"] != "same_host" {
		t.Fatalf("exact hook handoff failed: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	completed, afterReviews, err := f.DB.GetRequestWithReviews(request.ID)
	if err != nil || completed.Status != db.StatusExecuted || completed.Execution == nil ||
		completed.Execution.ApprovalPolicy != db.ApprovalPolicySameHost || completed.Execution.Assurance != db.AssuranceSameHost ||
		!reflect.DeepEqual(reviews, afterReviews) {
		t.Fatalf("handoff changed stored review/execution assurance: %+v %+v %v", completed, afterReviews, err)
	}
	if _, _, err := runShell(handoff, f.ProjectDir, nil); err == nil {
		t.Fatal("hook handoff was reusable after execution")
	}
	marker, err := os.ReadFile(filepath.Join(f.ProjectDir, "hook marker"))
	if err != nil || string(marker) != "reviewed" {
		t.Fatalf("reviewed command did not execute exactly once: %q %v", marker, err)
	}

	// A handoff is still only a candidate: opting out after lookup must stop
	// the final executor without modifying the signed historical review. The
	// daemon's target stays same_host; only the hook's selected alias changes.
	changed := createApproved("/bin/sh -c \"printf changed >> 'policy changed marker'\"")
	result := guard(native, changed.Command.Raw, f.ProjectDir, "allow")
	changedHandoff, ok := result.HookSpecificOutput.Updated["command"].(string)
	if !ok {
		t.Fatal("second approved command has no handoff")
	}
	independentPath := filepath.Join(f.ProjectDir, "independent policy.toml")
	if err := os.WriteFile(independentPath, []byte(fmt.Sprintf(base, "independent")+patterns), 0600); err != nil {
		t.Fatal(err)
	}
	retarget := func(target string) {
		t.Helper()
		next := configPath + ".next"
		if err := os.Symlink(target, next); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(next, configPath); err != nil {
			t.Fatal(err)
		}
	}
	retarget(independentPath)
	stdout, stderr, err = runShell(changedHandoff, f.ProjectDir, nil)
	if err == nil || !strings.Contains(stdout+stderr, "current policy") {
		t.Fatalf("executor ignored policy change after lookup: %v\n%s\n%s", err, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(f.ProjectDir, "policy changed marker")); !os.IsNotExist(err) {
		t.Fatalf("refused handoff ran the command: %v", err)
	}
	stored, err := f.DB.GetRequest(changed.ID)
	if err != nil || stored.Status != db.StatusApproved || stored.Execution != nil || stored.ApprovalPolicy != db.ApprovalPolicySameHost {
		t.Fatalf("refused handoff claimed or relabeled approval: %+v %v", stored, err)
	}
	retarget(policyTarget)
	f.json("daemon", "stop")
	running = false
	f.json("daemon", "start")
	running = true
	// The hook asserts its selected file; it cannot silently adopt a daemon
	// started with another policy, nor reconfigure that daemon per query.
	for _, invocation := range guards {
		guard(invocation, "echo selected-hook-probe", f.ProjectDir, "deny")
	}
}

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
