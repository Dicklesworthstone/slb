package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/audit"
)

// These tests exercise the exact embedded Python runtime, independent of the
// pattern engine. Classification is injected to isolate protocol/audit behavior.
func runHookRuntime(t *testing.T, home, setup, input string) (string, string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required for hook runtime integration tests")
	}
	preamble := "__name__ = 'slb_runtime_test'\nAUDIT_REDACTION_PATTERNS = [r'(?i)token=[^ ]+']\n"
	script := preamble + hookRuntime + "\n" + setup + "\nmain()\n"
	command := exec.Command(python, "-c", script)
	command.Dir = t.TempDir()
	command.Env = append(os.Environ(), "HOME="+home, "USERPROFILE="+home)
	command.Stdin = strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("Python runtime failed: %v\n%s", err, stderr.String())
	}
	return stdout.String(), stderr.String()
}

func hookPermission(t *testing.T, stdout string) string {
	t.Helper()
	var result struct {
		Output struct {
			Event      string `json:"hookEventName"`
			Permission string `json:"permissionDecision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil || result.Output.Event != "PreToolUse" {
		t.Fatalf("invalid hook protocol output: %q, %v", stdout, err)
	}
	return result.Output.Permission
}

func TestHookRuntimeOfflineAuditIsReadableByGo(t *testing.T) {
	home := t.TempDir()
	stdout, stderr := runHookRuntime(t, home,
		"query_slb_daemon = lambda *args: None\nclassify = lambda command: ('dangerous', 1)",
		`{"session_id":"agent-session","tool_input":{"command":"git push --force token=supersecret"}}`)
	if hookPermission(t, stdout) != "deny" || stderr != "" {
		t.Fatalf("unexpected decision or audit failure: %s %s", stdout, stderr)
	}
	events, err := audit.Query(filepath.Join(home, ".slb", "audit", "blocked"), audit.Filter{})
	if err != nil || len(events) != 1 {
		t.Fatalf("Go cannot read Python audit: %+v, %v", events, err)
	}
	event := events[0]
	if event.Action != "block" || event.Source != "hook_offline" || event.SessionID != "agent-session" ||
		strings.Contains(event.CommandRedacted, "supersecret") || !strings.Contains(event.CommandRedacted, "[REDACTED]") ||
		event.CommandHash != audit.CommandHash("git push --force token=supersecret", event.CWD) {
		t.Fatalf("audit redaction or cross-language schema/hash mismatch: %+v", event)
	}
}

func TestHookRuntimeAuditFailurePreservesDenial(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".slb"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr := runHookRuntime(t, home,
		"query_slb_daemon = lambda *args: None\nclassify = lambda command: ('critical', 2)",
		`{"tool_input":{"command":"dangerous token=supersecret"}}`)
	if hookPermission(t, stdout) != "deny" || !strings.Contains(stderr, "audit recording failed") || strings.Contains(stdout+stderr, "supersecret") {
		t.Fatalf("audit failure relaxed denial or leaked a secret: %s %s", stdout, stderr)
	}
}

func TestHookRuntimeDaemonAcknowledgementAvoidsDuplicateAudit(t *testing.T) {
	for _, tc := range []struct {
		name        string
		response    string
		permission  string
		auditEvents int
	}{
		{"recorded by daemon", "{'action':'block', 'audit_recorded':True}", "deny", 0},
		{"legacy daemon", "{'action':'block'}", "deny", 1},
		{"missing verdict", "{}", "ask", 1},
		{"invalid verdict", "{'action':42}", "ask", 1},
		{"allowed command", "{'action':'allow'}", "allow", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			stdout, _ := runHookRuntime(t, home, "query_slb_daemon = lambda *args: "+tc.response,
				`{"tool_input":{"command":"example command"}}`)
			if hookPermission(t, stdout) != tc.permission {
				t.Fatalf("wrong verdict: %s", stdout)
			}
			events, err := audit.Query(filepath.Join(home, ".slb", "audit", "blocked"), audit.Filter{})
			if err != nil || len(events) != tc.auditEvents {
				t.Fatalf("incorrect audit acknowledgement handling: %+v, %v", events, err)
			}
		})
	}
}

func TestHookRuntimeMalformedInputsAskInsteadOfAllowing(t *testing.T) {
	for _, input := range []string{"not-json", "[]", "null", `{"tool_input":[]}`, `{"tool_input":{}}`, `{"tool_input":{"command":42}}`} {
		stdout, _ := runHookRuntime(t, t.TempDir(), "", input)
		if hookPermission(t, stdout) != "ask" {
			t.Errorf("malformed input was allowed: %q -> %s", input, stdout)
		}
	}
}

func TestHookRuntimeHandlesFragmentedDaemonFrames(t *testing.T) {
	// A socket test stays entirely inside the Python child and uses AF_UNIX only
	// where available. Unit tests above also cover platforms without Unix IPC.
	if os.PathSeparator == '\\' {
		t.Skip("Unix socket fixture")
	}
	setup := `
import threading
SLB_TIMEOUT = 2.0
fixture_dir = tempfile.TemporaryDirectory(prefix="slb-frame-")
fixture_path = os.path.join(fixture_dir.name, "s")
server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
server.bind(fixture_path)
server.listen(1)
get_socket_path = lambda: fixture_path
def respond():
    with server:
        connection, _ = server.accept()
        with connection:
            connection.recv(4096)
            for fragment in (b'{"id":1,"res', b'ult":{"action":"block",', b'"audit_recorded":true}}\n'):
                connection.sendall(fragment)
                time.sleep(0.005)
threading.Thread(target=respond, daemon=True).start()
`
	stdout, stderr := runHookRuntime(t, t.TempDir(), setup, `{"tool_input":{"command":"example command"}}`)
	if hookPermission(t, stdout) != "deny" || stderr != "" {
		t.Fatalf("fragmented daemon response lost: %s %s", stdout, stderr)
	}
}

func TestHookRuntimeRedactionFailureOmitsCommand(t *testing.T) {
	home := t.TempDir()
	stdout, _ := runHookRuntime(t, home,
		"AUDIT_REDACTION_PATTERNS = ['[invalid']\nquery_slb_daemon = lambda *args: None\nclassify = lambda command: ('caution', 0)",
		`{"tool_input":{"command":"sensitive-command-value"}}`)
	if hookPermission(t, stdout) != "ask" {
		t.Fatal("caution decision changed")
	}
	events, err := audit.Query(filepath.Join(home, ".slb", "audit", "blocked"), audit.Filter{})
	if err != nil || len(events) != 1 || strings.Contains(events[0].CommandRedacted, "sensitive-command-value") {
		t.Fatalf("redaction failure leaked command: %+v, %v", events, err)
	}
}
