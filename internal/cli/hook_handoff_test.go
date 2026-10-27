package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/daemon"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/spf13/cobra"
)

func hookHandoffDatabase(t *testing.T, project string) string {
	t.Helper()
	path := filepath.Join(project, ".slb", "state.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	physical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return physical
}

func TestHookRuntimeExecutionHandoff(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX Bash handoff fixture")
	}
	root := t.TempDir()
	databasePath := hookHandoffDatabase(t, root)
	setup := fmt.Sprintf(`
shutil.which = lambda name: '/opt/slb bin/slb'
os.environ['SLB_SESSION_ID'] = 'slb-session'
def query_slb_daemon(command, session_id, cwd):
    assert session_id == 'slb-session'
    assert cwd == %q
    return {'action': 'execute', 'approval_policy': 'same_host', 'execution_handoff': {
        'request_id': 'req-bound', 'command_hash': 'a' * 64,
        'approval_policy': 'same_host',
        'session_id': session_id, 'database_path': os.path.join(cwd, '.slb', 'state.db')}}
`, root)
	input, err := json.Marshal(map[string]any{
		"session_id": "provider-session", "cwd": root,
		"tool_input": map[string]any{"command": "git reset --hard", "description": "preserve this", "timeout": 90000},
	})
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr := runHookRuntime(t, t.TempDir(), setup, string(input))
	if hookPermission(t, stdout) != "allow" || stderr != "" {
		t.Fatalf("handoff failed: %s %s", stdout, stderr)
	}
	var payload struct {
		Output struct {
			Input   map[string]any `json:"updatedInput"`
			Context string         `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload.Output.Context, db.ApprovalPolicySameHost.Label()) {
		t.Fatalf("handoff omitted same-host policy notice: %q", payload.Output.Context)
	}
	updated := payload.Output.Input
	command, _ := updated["command"].(string)
	if !strings.HasPrefix(command, "exec '/opt/slb bin/slb' execute ") || strings.Contains(command, "git reset") ||
		!strings.Contains(command, "--expected-command-hash "+strings.Repeat("a", 64)) ||
		!strings.Contains(command, "--session-id slb-session") || !strings.Contains(command, "--timeout 90") ||
		!strings.Contains(command, "--log-dir "+filepath.Join(filepath.Dir(databasePath), "logs")) ||
		!strings.Contains(command, databasePath) || !strings.HasSuffix(command, "-- req-bound") {
		t.Fatalf("handoff was not safely bound and quoted: %q", command)
	}
	if updated["description"] != "preserve this" || updated["timeout"] != float64(90000) {
		t.Fatalf("other Bash input fields were lost: %+v", updated)
	}
}

func TestNativeHookHandoffPreservesSelectedConfig(t *testing.T) {
	project := t.TempDir()
	databasePath := hookHandoffDatabase(t, project)
	selected := filepath.Join(t.TempDir(), "selected 'policy; literal.toml")
	if err := os.WriteFile(selected, []byte("[general]\napproval_policy = 'same_host'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	response := &daemon.HookQueryResult{Action: "execute", ConfigPath: selected, ConfigProject: project,
		ExecutionHandoff: &daemon.HookExecutionHandoff{RequestID: "req-config", CommandHash: strings.Repeat("a", 64),
			SessionID: "requester", DatabasePath: databasePath, ConfigPath: selected, ConfigProject: project}}
	for _, invalid := range []string{"", "relative", "missing", "foreign project", "missing scope", "lost handoff config", "classification mismatch"} {
		t.Run(invalid, func(t *testing.T) {
			copy := *response
			handoff := *response.ExecutionHandoff
			copy.ExecutionHandoff = &handoff
			switch invalid {
			case "relative":
				handoff.ConfigPath = "selected.toml"
			case "missing":
				handoff.ConfigPath = filepath.Join(t.TempDir(), "missing.toml")
			case "foreign project":
				handoff.ConfigProject = t.TempDir()
			case "missing scope":
				handoff.ConfigProject = ""
			case "lost handoff config":
				handoff.ConfigPath, handoff.ConfigProject = "", ""
			case "classification mismatch":
				copy.ConfigPath, copy.ConfigProject = "", ""
			}
			var stdout strings.Builder
			cmd := &cobra.Command{}
			cmd.SetOut(&stdout)
			err := emitNativeExecutionHandoff(cmd, &copy, map[string]any{"command": "echo selected-hook"}, "requester", project, "")
			if invalid != "" {
				if err == nil || stdout.Len() != 0 {
					t.Fatalf("invalid selected policy permitted handoff: %s %v", stdout.String(), err)
				}
				return
			}
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
			if !strings.Contains(command, "--config "+hookShellQuote(selected)) || !strings.HasSuffix(command, "-- req-config") {
				t.Fatalf("selected config not literally bound to executor: %q", command)
			}
		})
	}
}

func TestNativeHookHandoffPreservesSelectedConfigAlias(t *testing.T) {
	project := t.TempDir()
	databasePath := hookHandoffDatabase(t, project)
	actual := filepath.Join(project, "actual-policy.toml")
	selected := filepath.Join(project, "selected 'policy; literal.toml")
	if err := os.WriteFile(actual, []byte("[general]\napproval_policy = 'same_host'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(actual, selected); err != nil {
		t.Fatal(err)
	}
	response := &daemon.HookQueryResult{Action: "execute", ConfigPath: actual, ConfigProject: project,
		ExecutionHandoff: &daemon.HookExecutionHandoff{RequestID: "req-config", CommandHash: strings.Repeat("a", 64),
			SessionID: "requester", DatabasePath: databasePath, ConfigPath: actual, ConfigProject: project}}
	var stdout strings.Builder
	cmd := &cobra.Command{}
	cmd.SetOut(&stdout)
	if err := emitNativeExecutionHandoff(cmd, response, map[string]any{"command": "echo selected-hook"}, "requester", project, selected); err != nil {
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
	if !strings.Contains(command, "--config "+hookShellQuote(selected)) || strings.Contains(command, "--config "+hookShellQuote(actual)) {
		t.Fatalf("handoff replaced the hook's selected policy source: %q", command)
	}
	replacement := filepath.Join(project, "independent-policy.toml")
	if err := os.WriteFile(replacement, []byte("[general]\napproval_policy = 'independent'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	newAlias := filepath.Join(project, "replacement-alias.toml")
	if err := os.Symlink(replacement, newAlias); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(newAlias, selected); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := emitNativeExecutionHandoff(cmd, response, map[string]any{"command": "echo selected-hook"}, "requester", project, selected); err == nil || stdout.Len() != 0 {
		t.Fatalf("retargeted selected policy accepted a stale handoff: %q, %v", stdout.String(), err)
	}
}

func TestNativeHookSelectedConfigScopesAndRefreshesFallback(t *testing.T) {
	resetHookFlags()
	t.Cleanup(resetHookFlags)
	t.Setenv("HOME", t.TempDir())
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(project, "separate-project")
	for _, root := range []string{project, nested} {
		database, err := db.OpenAndMigrate(filepath.Join(root, ".slb", "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = database.Close() })
	}
	t.Chdir(project)
	flagConfig = filepath.Join(t.TempDir(), "selected policy.toml")
	selected := "[general]\napproval_policy = 'same_host'\n[patterns.dangerous]\npatterns = ['^echo selected-hook$']\n[integrations]\nhook_query_timeout_ms = 900\n"
	if err := os.WriteFile(flagConfig, []byte(selected), 0600); err != nil {
		t.Fatal(err)
	}
	for _, pinned := range []string{"", project} {
		flagHookPolicyProject = pinned
		result, err := classifyNativeHookFallback("echo selected-hook", project)
		if err != nil || result.Action != "block" || result.Tier != "dangerous" || result.ApprovalPolicy != db.ApprovalPolicySameHost {
			t.Fatalf("explicit policy ignored (scope=%q): %+v %v", pinned, result, err)
		}
		if got := hookQueryTimeout(project); got != 900*time.Millisecond {
			t.Fatalf("selected query timeout = %s", got)
		}
		result, err = classifyNativeHookFallback("echo selected-hook", nested)
		if err != nil || result.Action != "ask" || result.ApprovalPolicy != db.ApprovalPolicyIndependent {
			t.Fatalf("parent selected policy leaked into nested project: %+v %v", result, err)
		}
	}
	if err := os.WriteFile(flagConfig, []byte("[patterns.safe]\npatterns = ['^echo selected-hook$']\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if result, err := classifyNativeHookFallback("echo selected-hook", project); err != nil || result.Action != "allow" {
		t.Fatalf("selected file change was ignored: %+v %v", result, err)
	}
	for _, invalid := range []string{"malformed", "missing"} {
		if invalid == "malformed" {
			if err := os.WriteFile(flagConfig, []byte("[broken TOML"), 0600); err != nil {
				t.Fatal(err)
			}
		} else {
			flagConfig = filepath.Join(t.TempDir(), "missing.toml")
		}
		output := nativeHookOutput(t, map[string]any{"cwd": project, "tool_input": map[string]any{"command": "git stash"}}, t.TempDir())
		if permission, specific := nativeHookPermission(t, output); permission != "deny" || specific["updatedInput"] != nil {
			t.Fatalf("%s selected policy failed open: %+v", invalid, output)
		}
		if result, err := classifyNativeHookFallback("git stash", nested); err != nil || result.Action != "allow" {
			t.Fatalf("unavailable parent config disabled nested project: %+v %v", result, err)
		}
	}
}

func TestNativeHookRequiresSelectedPolicyAcknowledgment(t *testing.T) {
	resetHookFlags()
	t.Cleanup(resetHookFlags)
	project := shortProjectDir(t)
	flagConfig = filepath.Join(project, "selected.toml")
	flagHookPolicyProject = project
	if err := os.WriteFile(flagConfig, []byte("[general]\napproval_policy = 'same_host'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// This old daemon returns allow but does not understand ConfigPath. Its
	// weaker response must not override the explicitly selected local policy.
	fakeHookDaemon(t, project, 0)
	output := nativeHookOutput(t, map[string]any{"cwd": project, "tool_input": map[string]any{"command": "git stash"}}, t.TempDir())
	permission, specific := nativeHookPermission(t, output)
	reason, _ := specific["permissionDecisionReason"].(string)
	if permission != "deny" || specific["updatedInput"] != nil || !strings.Contains(reason, "did not acknowledge") {
		t.Fatalf("old daemon silently ignored selected policy: %+v", output)
	}
}

func TestHookRuntimeHandoffPreservesSelectedConfig(t *testing.T) {
	project := t.TempDir()
	hookHandoffDatabase(t, project)
	selected := filepath.Join(t.TempDir(), "selected 'policy; literal.toml")
	if err := os.WriteFile(selected, []byte("[general]\napproval_policy = 'same_host'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	setup := fmt.Sprintf(`
shutil.which = lambda name: '/opt/slb bin/slb'
os.environ['SLB_SESSION_ID'] = 'requester'
response = {'action': 'execute', 'config_path': %q, 'config_project': %q,
    'execution_handoff': {'request_id': 'req-config', 'command_hash': 'a' * 64,
    'session_id': 'requester', 'database_path': os.path.join(%q, '.slb', 'state.db'),
    'config_path': %q, 'config_project': %q}}
query_slb_daemon = lambda *args: response
`, selected, project, project, selected, project)
	input, _ := json.Marshal(map[string]any{"cwd": project, "tool_input": map[string]any{"command": "echo selected-hook"}})
	for _, mutation := range []string{"", "response['execution_handoff'].pop('config_path')", "response['execution_handoff']['config_project'] = '/different-project'", "response['execution_handoff']['config_path'] = 'relative.toml'", "response.pop('config_path')"} {
		stdout, stderr := runHookRuntime(t, t.TempDir(), setup+mutation, string(input))
		if mutation != "" {
			if hookPermission(t, stdout) != "deny" || strings.Contains(stdout, "updatedInput") {
				t.Fatalf("invalid selected policy permitted Python handoff: %s %s", stdout, stderr)
			}
			continue
		}
		var payload struct {
			Output struct {
				Input map[string]any `json:"updatedInput"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
			t.Fatal(err)
		}
		command, _ := payload.Output.Input["command"].(string)
		if hookPermission(t, stdout) != "allow" || !strings.Contains(command, "--config '") || !strings.Contains(command, "policy; literal.toml'") {
			t.Fatalf("Python handoff lost literal selected file: %s %s", stdout, stderr)
		}
	}
}

func TestHookRuntimeSelectedPolicyUsesNativeGuard(t *testing.T) {
	setup := `
HOOK_POLICY_CONFIG = '/selected path/policy.toml'
HOOK_POLICY_PROJECT = '/selected-project'
shutil.which = lambda name: '/opt/slb bin/slb'
def unexpected(*args):
    raise AssertionError('stale embedded policy or independent query was used')
query_slb_daemon = unexpected
classify = unexpected
def native(argv, **kwargs):
    assert argv == ['/opt/slb bin/slb', 'hook', 'guard', '--config', HOOK_POLICY_CONFIG, '--policy-project', HOOK_POLICY_PROJECT]
    assert json.loads(kwargs['input'])['tool_input']['command'] == 'echo selected-hook'
    return subprocess.CompletedProcess(argv, 0, json.dumps({'hookSpecificOutput': {
        'hookEventName': 'PreToolUse', 'permissionDecision': 'deny', 'permissionDecisionReason': 'current selected policy'}}), '')
subprocess.run = native
`
	stdout, stderr := runHookRuntime(t, t.TempDir(), setup, `{"cwd":"/selected-project","tool_input":{"command":"echo selected-hook"}}`)
	if hookPermission(t, stdout) != "deny" || !strings.Contains(stdout, "current selected policy") || stderr != "" {
		t.Fatalf("selected Python guard did not use current native policy: %s %s", stdout, stderr)
	}
	for _, failure := range []string{"shutil.which = lambda name: None", "subprocess.run = lambda *args, **kwargs: subprocess.CompletedProcess([], 1, '', '')", "subprocess.run = lambda *args, **kwargs: subprocess.CompletedProcess([], 0, 'invalid JSON', '')"} {
		stdout, stderr = runHookRuntime(t, t.TempDir(), setup+failure, `{"cwd":"/selected-project","tool_input":{"command":"echo selected-hook"}}`)
		if hookPermission(t, stdout) != "deny" || strings.Contains(stdout, "updatedInput") || stderr != "" {
			t.Fatalf("failed native guard revived embedded policy: %s %s", stdout, stderr)
		}
	}
}

func TestHookRuntimeInvalidHandoffsNeverPermitRawExecution(t *testing.T) {
	for _, mutation := range []string{
		"response.pop('execution_handoff')",
		"response['execution_handoff']['request_id'] = 'req; touch /tmp/injected'",
		"response['execution_handoff']['command_hash'] = 'wrong'",
		"response['execution_handoff']['session_id'] = 'other-session'",
		"response['execution_handoff']['database_path'] = '/other/.slb/state.db'",
		"shutil.which = lambda name: None",
	} {
		t.Run(mutation, func(t *testing.T) {
			setup := `
shutil.which = lambda name: '/usr/local/bin/slb'
os.environ['SLB_SESSION_ID'] = 'slb-session'
response = {'action': 'execute', 'execution_handoff': {
    'request_id': 'req-bound', 'command_hash': 'a' * 64, 'session_id': 'slb-session',
    'database_path': os.path.join(os.getcwd(), '.slb', 'state.db')}}
os.mkdir('.slb', 0o700)
with open(response['execution_handoff']['database_path'], 'w'):
    pass
` + mutation + "\nquery_slb_daemon = lambda *args: response"
			stdout, _ := runHookRuntime(t, t.TempDir(), setup, `{"tool_input":{"command":"git reset --hard"}}`)
			if hookPermission(t, stdout) != "deny" || strings.Contains(stdout, "updatedInput") {
				t.Fatalf("invalid handoff permitted execution: %s", stdout)
			}
		})
	}
}

func TestHookRuntimeSymlinkHandoffPreservesApprovalIdentity(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX Bash handoff fixture")
	}
	project := t.TempDir()
	databasePath := hookHandoffDatabase(t, project)
	other := t.TempDir()
	otherDatabase := hookHandoffDatabase(t, other)
	nested := filepath.Join(project, "src")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "project-alias")
	if err := os.Symlink(project, alias); err != nil {
		t.Fatal(err)
	}
	descendantAlias := filepath.Join(other, "descendant-alias")
	if err := os.Symlink(nested, descendantAlias); err != nil {
		t.Fatal(err)
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
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, cwd, database, permission, logDirectory string
	}{
		{"root alias with alias DB", alias, filepath.Join(alias, ".slb", "state.db"), "allow", ""},
		{"root alias with physical DB", alias, databasePath, "allow", ""},
		{"descendant alias with physical DB", descendantAlias, databasePath, "allow", ""},
		{"file-only DB symlink preserves project logs", linkedProject, linkedDatabase, "allow", filepath.Join(linkedProject, ".slb", "logs")},
		{"descendant alias with lexical parent DB", descendantAlias, otherDatabase, "deny", ""},
		{"root alias with foreign DB", alias, otherDatabase, "deny", ""},
		{"unavailable cwd", filepath.Join(alias, "missing"), databasePath, "deny", ""},
		{"unavailable DB", alias, filepath.Join(project, ".slb", "missing.db"), "deny", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup := fmt.Sprintf(`
shutil.which = lambda name: '/usr/local/bin/slb'
os.environ['SLB_SESSION_ID'] = 'slb-session'
def query_slb_daemon(command, session_id, cwd):
    assert cwd == %q
    assert command == 'git reset --hard'
    assert session_id == 'slb-session'
    return {'action': 'execute', 'execution_handoff': {
        'request_id': 'req-alias', 'command_hash': 'a' * 64,
        'session_id': 'slb-session', 'database_path': %q}}
`, tc.cwd, tc.database)
			input, err := json.Marshal(map[string]any{
				"cwd": tc.cwd, "tool_input": map[string]any{"command": "git reset --hard"},
			})
			if err != nil {
				t.Fatal(err)
			}
			stdout, stderr := runHookRuntime(t, t.TempDir(), setup, string(input))
			if hookPermission(t, stdout) != tc.permission || stderr != "" {
				t.Fatalf("incorrect handoff decision: %s %s", stdout, stderr)
			}
			if tc.permission == "allow" {
				if !strings.Contains(stdout, databasePath) || !strings.Contains(stdout, strings.Repeat("a", 64)) {
					t.Fatalf("handoff lost the physical database or literal command hash: %s", stdout)
				}
				logDirectory := tc.logDirectory
				if logDirectory == "" {
					logDirectory = filepath.Join(filepath.Dir(databasePath), "logs")
				}
				if !strings.Contains(stdout, "--log-dir "+logDirectory) {
					t.Fatalf("handoff moved logs outside the selected project's state: %s", stdout)
				}
			} else if strings.Contains(stdout, "updatedInput") {
				t.Fatalf("invalid database handoff produced an executable command: %s", stdout)
			}
		})
	}
}
