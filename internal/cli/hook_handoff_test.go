package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
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
