package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHookRuntimeExecutionHandoff(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX Bash handoff fixture")
	}
	root := t.TempDir()
	setup := fmt.Sprintf(`
shutil.which = lambda name: '/opt/slb bin/slb'
os.environ['SLB_SESSION_ID'] = 'slb-session'
def query_slb_daemon(command, session_id, cwd):
    assert session_id == 'slb-session'
    assert cwd == %q
    return {'action': 'execute', 'execution_handoff': {
        'request_id': 'req-bound', 'command_hash': 'a' * 64,
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
			Input map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatal(err)
	}
	updated := payload.Output.Input
	command, _ := updated["command"].(string)
	if !strings.HasPrefix(command, "exec '/opt/slb bin/slb' execute ") || strings.Contains(command, "git reset") ||
		!strings.Contains(command, "--expected-command-hash "+strings.Repeat("a", 64)) ||
		!strings.Contains(command, "--session-id slb-session") || !strings.Contains(command, "--timeout 90") ||
		!strings.Contains(command, "--log-dir "+filepath.Join(root, ".slb", "logs")) ||
		!strings.Contains(command, filepath.Join(root, ".slb", "state.db")) || !strings.HasSuffix(command, "-- req-bound") {
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
` + mutation + "\nquery_slb_daemon = lambda *args: response"
			stdout, _ := runHookRuntime(t, t.TempDir(), setup, `{"tool_input":{"command":"git reset --hard"}}`)
			if hookPermission(t, stdout) != "deny" || strings.Contains(stdout, "updatedInput") {
				t.Fatalf("invalid handoff permitted execution: %s", stdout)
			}
		})
	}
}
