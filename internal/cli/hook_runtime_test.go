package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/audit"
	"github.com/Dicklesworthstone/slb/internal/daemon"
	"github.com/Dicklesworthstone/slb/internal/db"
)

// These tests exercise the exact embedded Python runtime, independent of the
// pattern engine. Classification is injected to isolate protocol/audit behavior.
func runHookRuntime(t *testing.T, home, setup, input string) (string, string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required for hook runtime integration tests")
	}
	// The generator embeds these constants ahead of the runtime (f37f6ee added
	// HOOK_CAUTION_ACTION; "block" is the generated default). Tests override
	// them in setup when a case depends on a different value.
	preamble := "__name__ = 'slb_runtime_test'\nAUDIT_REDACTION_PATTERNS = [r'(?i)token=[^ ]+']\nHOOK_CAUTION_ACTION = 'block'\nHOOK_APPROVAL_POLICY = 'independent'\n"
	script := preamble + hookRuntime + "\n" + setup + "\nmain()\n"
	command := exec.Command(python, "-c", script)
	command.Dir = t.TempDir()
	command.Env = append(os.Environ(), "HOME="+home, "USERPROFILE="+home, "SLB_SESSION_ID=", "SLB_HOST=", "SLB_SESSION_KEY=")
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

func TestHookRuntimeApprovalPolicyAuditAndOutput(t *testing.T) {
	for _, tc := range []struct {
		name, setup string
		policy      db.ApprovalPolicy
	}{
		{"offline independent", "query_slb_daemon = lambda *args: None\nclassify = lambda command: ('dangerous', 1)", db.ApprovalPolicyIndependent},
		{"offline same host", "HOOK_APPROVAL_POLICY = 'same_host'\nquery_slb_daemon = lambda *args: None\nclassify = lambda command: ('dangerous', 1)", db.ApprovalPolicySameHost},
		{"live same host", "query_slb_daemon = lambda *args: {'action':'block', 'tier':'dangerous', 'approval_policy':'same_host'}", db.ApprovalPolicySameHost},
		{"live overrides snapshot", "HOOK_APPROVAL_POLICY = 'same_host'\nquery_slb_daemon = lambda *args: {'action':'block', 'tier':'dangerous', 'approval_policy':'independent'}", db.ApprovalPolicyIndependent},
		{"live unknown does not inherit snapshot", "HOOK_APPROVAL_POLICY = 'same_host'\nquery_slb_daemon = lambda *args: {'action':'block', 'tier':'dangerous'}", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			notice := ""
			if tc.policy != "" {
				notice = tc.policy.Label()
			}
			stdout, stderr := runHookRuntime(t, home, tc.setup, `{"tool_input":{"command":"disposable-hook-fixture"}}`)
			if hookPermission(t, stdout) != "deny" || stderr != "" || !strings.Contains(stdout, notice) ||
				(tc.policy == "" && strings.Contains(stdout, "same-host peer review")) {
				t.Fatalf("policy notice or fail-closed decision missing: %s %s", stdout, stderr)
			}
			events, err := audit.Query(filepath.Join(home, ".slb", "audit", "blocked"), audit.Filter{})
			if err != nil || len(events) != 1 || events[0].ApprovalPolicy != string(tc.policy) || events[0].ReviewNotice != notice {
				t.Fatalf("hook policy provenance missing: %+v, %v", events, err)
			}
		})
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

func TestHookRuntimeObservationCorrelationAfterLostAcknowledgement(t *testing.T) {
	const command = "git reset --hard HEAD token=command-secret"
	untrustedID := strings.Repeat("a", 32)
	for _, failure := range []string{"closed", "timeout"} {
		t.Run(failure, func(t *testing.T) {
			home, project := t.TempDir(), t.TempDir()
			input, err := json.Marshal(map[string]any{
				"session_id": "agent-session", "cwd": project,
				"observation_id": "external-observation-secret", "_slb_observation_id": untrustedID,
				"tool_input": map[string]any{"command": command},
			})
			if err != nil {
				t.Fatal(err)
			}
			setup := fmt.Sprintf(`
os.environ["SLB_HOST"] = "fixture.invalid:1"
os.environ["SLB_SESSION_KEY"] = "authentication-secret"
failure = %q
connections = []
class LostAcknowledgementSocket:
    def __init__(self):
        self.closed = False
        self.requests = []
        connections.append(self)
    def __enter__(self):
        return self
    def __exit__(self, *args):
        self.closed = True
    def settimeout(self, timeout):
        pass
    def sendall(self, frame):
        request = json.loads(frame)
        self.requests.append(request.get("method", "auth"))
        if "auth" in request:
            assert request["auth"] == "authentication-secret"
            return
        assert request["method"] == "hook_query"
        params = request["params"]
        assert params["execution_handoff"] is True
        observation = params["observation_id"]
        assert _valid_observation_id(observation) and observation != "a" * 32
        # Persist the daemon's observation, then lose its reply. The fallback
        # must retain the sent identifier without replacing the original row.
        assert _record_audit(params["command"], params["session_id"], params["cwd"],
                             "block", "dangerous", 1, "daemon", observation_id=observation)
    def recv(self, limit):
        if failure == "timeout":
            raise TimeoutError("lost acknowledgement secret")
        return b""
_connect_remote_daemon = lambda *args: LostAcknowledgementSocket()
def classify(command):
    assert len(connections) == 1
    assert connections[0].closed and connections[0].requests == ["auth", "hook_query"]
    return "dangerous", 1
`, failure)
			var outputs strings.Builder
			for range 2 {
				stdout, stderr := runHookRuntime(t, home, setup, string(input))
				if hookPermission(t, stdout) != "deny" || stderr != "" || strings.Contains(stdout, "command-secret") ||
					strings.Contains(stdout, "authentication-secret") || strings.Contains(stdout, "lost acknowledgement secret") ||
					strings.Contains(stdout, "external-observation-secret") {
					t.Fatalf("lost acknowledgement changed denial or exposed a secret: %s %s", stdout, stderr)
				}
				outputs.WriteString(stdout)
			}
			events, err := audit.Query(filepath.Join(home, ".slb", "audit", "blocked"), audit.Filter{})
			if err != nil || len(events) != 4 {
				t.Fatalf("lost-ack audit records were omitted or overwritten: %+v, %v", events, err)
			}
			observations := make(map[string]map[string]string)
			for _, event := range events {
				encoded, err := json.Marshal(event)
				if err != nil || !audit.ValidObservationID(event.ObservationID) || event.ObservationID == untrustedID ||
					strings.Contains(outputs.String(), event.ObservationID) || strings.Contains(string(encoded), "command-secret") ||
					strings.Contains(string(encoded), "external-observation-secret") || strings.Contains(string(encoded), "authentication-secret") ||
					event.Action != "block" || event.Tier != "dangerous" || event.CWD != project || event.SessionID != "agent-session" ||
					event.CommandHash != audit.CommandHash(command, project) {
					t.Fatalf("correlated Python audit lost identity, scope, classification, or redaction: %+v, %v", event, err)
				}
				if observations[event.ObservationID] == nil {
					observations[event.ObservationID] = make(map[string]string)
				}
				if observations[event.ObservationID][event.Source] != "" {
					t.Fatalf("repeated a source within one observation: %+v", events)
				}
				observations[event.ObservationID][event.Source] = event.ID
			}
			if len(observations) != 2 {
				t.Fatalf("independent identical invocations reused an ID: %+v", events)
			}
			for _, sources := range observations {
				if len(sources) != 2 || sources["daemon"] == "" || sources["hook_offline"] == "" || sources["daemon"] == sources["hook_offline"] {
					t.Fatalf("one invocation did not retain independent correlated records: %v", sources)
				}
			}
		})
	}
}

func TestHookRuntimeObservationGenerationFailurePreservesDecision(t *testing.T) {
	for _, failure := range []string{"entropy", "invalid value"} {
		for _, tc := range []struct{ tier, permission, action string }{
			{"safe", "allow", ""}, {"unknown", "ask", "ask"}, {"dangerous", "deny", "block"},
		} {
			t.Run(failure+"/"+tc.tier, func(t *testing.T) {
				home := t.TempDir()
				setup := fmt.Sprintf(`
failure = %q
original_uuid4 = uuid.uuid4
attempted = False
def fail_once():
    global attempted
    if not attempted:
        attempted = True
        if failure == "entropy":
            raise OSError("entropy-secret")
        return type("InvalidUUID", (), {"hex": "invalid-observation-secret"})()
    return original_uuid4()
uuid.uuid4 = fail_once
def query_slb_daemon(command, session_id, cwd, diagnostics=None, observation_id=""):
    assert attempted and observation_id == ""
    return None
classify = lambda command: (%q, 1)
`, failure, tc.tier)
				stdout, stderr := runHookRuntime(t, home, setup,
					`{"observation_id":"external-observation-secret","tool_input":{"command":"opaque token=command-secret"}}`)
				if hookPermission(t, stdout) != tc.permission || stderr != "" || strings.Contains(stdout, "entropy-secret") ||
					strings.Contains(stdout, "invalid-observation-secret") || strings.Contains(stdout, "command-secret") {
					t.Fatalf("optional metadata changed authorization or exposed a secret: %s %s", stdout, stderr)
				}
				events, err := audit.Query(filepath.Join(home, ".slb", "audit", "blocked"), audit.Filter{})
				if err != nil {
					t.Fatal(err)
				}
				if tc.permission == "allow" {
					if len(events) != 0 {
						t.Fatalf("observation generation failure created safe-command audit: %+v", events)
					}
					return
				}
				if len(events) != 1 || events[0].ObservationID != "" || events[0].Action != tc.action || events[0].Tier != tc.tier ||
					strings.Contains(events[0].CommandRedacted, "command-secret") {
					t.Fatalf("metadata failure lost the original decision audit: %+v", events)
				}
			})
		}
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
os.chmod(fixture_path, 0o600)
server.listen(1)
_discover_daemon = lambda cwd: (cwd, fixture_path, None)
def respond():
    with server:
        connection, _ = server.accept()
        with connection:
            stream = connection.makefile("rb")
            request = json.loads(stream.readline())
            assert request["method"] == "status"
            connection.sendall(json.dumps({"id": 0, "result": {"project_path": os.getcwd(), "pid": os.getpid(), "state_ready": True}}).encode() + b'\n')
            request = json.loads(stream.readline())
            assert request["method"] == "hook_query"
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

const hookDiscoveryFixture = `
fixture_dir = tempfile.TemporaryDirectory(prefix="slb-disc-")
project = os.path.join(fixture_dir.name, "project")
os.mkdir(project, 0o700)
os.chdir(project)
marker = os.path.join(project, ".slb")
os.mkdir(marker, 0o700)
record_path = os.path.join(marker, "daemon.json")
record = {"version": 1, "project_path": project,
          "socket_path": os.path.join(fixture_dir.name, "daemon.sock"),
          "pid_file": os.path.join(fixture_dir.name, "daemon.pid"), "pid": os.getpid()}
def write_record(data):
    with os.fdopen(os.open(record_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600), "w") as stream:
        stream.write(data)
`

// The optional JSON accelerator has different nesting limits across Python
// versions. Exercise the standard-library fallback with a predictable limit.
const hookPureJSONDecoderFixture = `
sys.setrecursionlimit(1000)
json.scanner.make_scanner = json.scanner.py_make_scanner
json._default_decoder = json.JSONDecoder()
`

func TestHookRuntimeInvalidDiscoveryNeverFallsBack(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix discovery file ownership")
	}
	for _, tc := range []struct{ name, mutate string }{
		{"malformed", `write_record('{"socket_path":"untrusted-discovery-secret"')`},
		{"deeply nested JSON", hookPureJSONDecoderFixture + `write_record('[' * 1100 + '0' + ']' * 1100)`},
		{"oversized", `write_record(" " * 8193)`},
		{"copied from another project", `record["project_path"] += "-other"; write_record(json.dumps(record))`},
		{"unknown field", `record["untrusted-discovery-secret"] = True; write_record(json.dumps(record))`},
		{"duplicate field", `write_record(json.dumps(record)[:-1] + ',"pid":1}')`},
		{"trailing JSON", `write_record(json.dumps(record) + '{}')`},
		{"relative socket", `record["socket_path"] = "untrusted-discovery-secret"; write_record(json.dumps(record))`},
		{"double slash socket", `record["socket_path"] = "/" + record["socket_path"]; write_record(json.dumps(record))`},
		{"long socket", `record["socket_path"] = "/" + "s" * 103; write_record(json.dumps(record))`},
		{"control in path", `record["socket_path"] += "\nuntrusted-discovery-secret"; write_record(json.dumps(record))`},
		{"boolean PID", `record["pid"] = True; write_record(json.dumps(record))`},
		{"PID overflow", `record["pid"] = sys.maxsize + 1; write_record(json.dumps(record))`},
		{"shared record", `write_record(json.dumps(record)); os.chmod(record_path, 0o644)`},
		{"hard link", `write_record(json.dumps(record)); os.link(record_path, record_path + ".link")`},
		{"symlink record", `write_record(json.dumps(record)); os.rename(record_path, record_path + ".target"); os.symlink(record_path + ".target", record_path)`},
		{"writable marker", `write_record(json.dumps(record)); os.chmod(marker, 0o770)`},
		{"symlink marker", `write_record(json.dumps(record)); os.rename(marker, marker + ".target"); os.symlink(marker + ".target", marker)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			// If a regression silently falls through to the legacy path, make
			// that address appear live and fail the test on any connect attempt.
			setup := hookDiscoveryFixture + `
legacy_path = get_socket_path(project)
original_lstat = os.lstat
from types import SimpleNamespace
def guarded_lstat(path, *args, **kwargs):
    if path == legacy_path:
        return SimpleNamespace(st_mode=stat.S_IFSOCK | 0o600, st_uid=os.geteuid())
    return original_lstat(path, *args, **kwargs)
os.lstat = guarded_lstat
def forbidden_socket(*args, **kwargs):
    raise AssertionError("invalid discovery attempted a fallback connection")
socket.socket = forbidden_socket
classify = lambda command: ('unknown', 0)
` + tc.mutate
			stdout, stderr := runHookRuntime(t, home, setup,
				`{"session_id":"provider-secret","tool_input":{"command":"unknown token=command-secret"}}`)
			if hookPermission(t, stdout) != "ask" || stderr != "" ||
				!strings.Contains(stdout, "discovery or identity validation failed") ||
				strings.Contains(stdout, "untrusted-discovery-secret") || strings.Contains(stdout, "command-secret") ||
				strings.Contains(stdout, "provider-secret") {
				t.Fatalf("invalid discovery was not safely diagnosed: %s %s", stdout, stderr)
			}
			events, err := audit.Query(filepath.Join(home, ".slb", "audit", "blocked"), audit.Filter{})
			if err != nil || len(events) != 1 || events[0].DaemonError != "discovery_invalid" || events[0].SocketPath != "" {
				t.Fatalf("invalid discovery audit leaked an untrusted endpoint: %+v, %v", events, err)
			}
		})
	}
}

func TestHookRuntimeMissingSocketDiagnosticPreservesDecisions(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix discovery file ownership")
	}
	for _, tc := range []struct{ tier, permission string }{{"unknown", "ask"}, {"dangerous", "deny"}, {"safe", "allow"}} {
		t.Run(tc.tier, func(t *testing.T) {
			home := t.TempDir()
			setup := hookDiscoveryFixture + "\nwrite_record(json.dumps(record))\nclassify = lambda command: ('" + tc.tier + "', 1)"
			stdout, stderr := runHookRuntime(t, home, setup, `{"tool_input":{"command":"opaque token=command-secret"}}`)
			if hookPermission(t, stdout) != tc.permission || stderr != "" || strings.Contains(stdout, "command-secret") {
				t.Fatalf("missing socket changed policy or leaked command: %s %s", stdout, stderr)
			}
			events, err := audit.Query(filepath.Join(home, ".slb", "audit", "blocked"), audit.Filter{})
			if err != nil {
				t.Fatal(err)
			}
			if tc.permission == "allow" {
				if len(events) != 0 || strings.Contains(stdout, "permissionDecisionReason") {
					t.Fatalf("safe fallback unexpectedly became a prompt: %s %+v", stdout, events)
				}
			} else if len(events) != 1 || events[0].DaemonError != "unavailable" ||
				!strings.HasSuffix(events[0].SocketPath, "/daemon.sock") ||
				!strings.Contains(stdout, "daemon.sock") {
				t.Fatalf("selected missing socket not diagnosed: %s %+v", stdout, events)
			}
		})
	}
}

func TestHookRuntimeMissingUnixCapabilitiesFailsClosed(t *testing.T) {
	for _, capability := range []struct{ module, name string }{
		{"os", "geteuid"}, {"os", "O_DIRECTORY"}, {"os", "O_NOFOLLOW"},
		{"os", "O_NONBLOCK"}, {"os", "O_CLOEXEC"}, {"os", "supports_dir_fd"},
		{"os", "supports_follow_symlinks"}, {"socket", "AF_UNIX"},
	} {
		for _, verdict := range []struct{ tier, permission string }{{"dangerous", "deny"}, {"unknown", "ask"}, {"safe", "allow"}} {
			t.Run(capability.name+"/"+verdict.tier, func(t *testing.T) {
				home := t.TempDir()
				setup := fmt.Sprintf(`
if hasattr(%s, %q):
    delattr(%s, %q)
def forbidden_socket(*args, **kwargs):
    raise AssertionError("missing ownership capabilities must prevent connection")
socket.socket = forbidden_socket
classify = lambda command: (%q, 1)
`, capability.module, capability.name, capability.module, capability.name, verdict.tier)
				stdout, stderr := runHookRuntime(t, home, setup,
					`{"session_id":"provider-secret","tool_input":{"command":"opaque token=command-secret"}}`)
				if hookPermission(t, stdout) != verdict.permission || stderr != "" ||
					strings.Contains(stdout, "provider-secret") || strings.Contains(stdout, "command-secret") {
					t.Fatalf("missing Unix capability escaped safe fallback: %s %s", stdout, stderr)
				}
				events, err := audit.Query(filepath.Join(home, ".slb", "audit", "blocked"), audit.Filter{})
				if err != nil {
					t.Fatal(err)
				}
				if verdict.permission == "allow" {
					if len(events) != 0 || strings.Contains(stdout, "permissionDecisionReason") {
						t.Fatalf("known-safe command became a prompt: %s %+v", stdout, events)
					}
				} else if len(events) != 1 || events[0].DaemonError != "discovery_invalid" || events[0].SocketPath != "" ||
					!strings.Contains(stdout, "No socket was selected") {
					t.Fatalf("missing capability was not safely diagnosed: %s %+v", stdout, events)
				}
			})
		}
	}
}

func TestHookRuntimeStrictProjectMarkerPreventsAncestorDiscovery(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix project marker fixtures")
	}
	for _, scenario := range []string{"file", "dangling symlink", "unreadable marker", "unreadable user state"} {
		t.Run(scenario, func(t *testing.T) {
			home := t.TempDir()
			setup := hookDiscoveryFixture + fmt.Sprintf(`
scenario = %q
child = os.path.join(project, "child")
os.mkdir(child, 0o700)
os.chdir(child)
child_marker = os.path.join(child, ".slb")
if scenario == "file":
    with open(child_marker, "x") as stream:
        stream.write("invalid marker")
elif scenario == "dangling symlink":
    os.symlink("unavailable-state", child_marker)
elif scenario == "unreadable marker":
    real_lstat = os.lstat
    def checked_lstat(path, *args, **kwargs):
        if path == child_marker:
            raise PermissionError("private-marker-secret")
        return real_lstat(path, *args, **kwargs)
    os.lstat = checked_lstat
else:
    os.mkdir(child_marker, 0o700)
    # Select the child as the stateless-user-directory candidate without
    # changing audit HOME. Its inaccessible state must propagate, not make
    # the walk continue into the ancestor project.
    real_stat, real_lstat = os.stat, os.lstat
    def checked_stat(path, *args, **kwargs):
        if path == os.path.join(os.environ["HOME"], ".slb"):
            return real_stat(child_marker)
        return real_stat(path, *args, **kwargs)
    def checked_lstat(path, *args, **kwargs):
        if path == os.path.join(child_marker, "state.db"):
            raise PermissionError("private-state-secret")
        return real_lstat(path, *args, **kwargs)
    os.stat, os.lstat = checked_stat, checked_lstat
    # Audit records use HOME/.slb. Restore the stat adapter after checking
    # discovery below, so the failure path's storage remains independent.
    try:
        _physical_project_root(child)
    except _DaemonDiscoveryError:
        pass
    else:
        raise AssertionError("unreadable user state changed project identity")
    os.stat = real_stat

def forbidden_discovery(root):
    raise AssertionError("invalid nearest marker reached ancestor discovery")
_read_daemon_discovery = forbidden_discovery
def forbidden_socket(*args, **kwargs):
    raise AssertionError("invalid nearest marker reached socket creation")
socket.socket = forbidden_socket
if scenario == "unreadable user state":
    # Retain an inaccessible nearest marker for the complete hook invocation.
    def checked_lstat(path, *args, **kwargs):
        if path == child_marker:
            raise PermissionError("private-marker-secret")
        return real_lstat(path, *args, **kwargs)
    os.lstat = checked_lstat
classify = lambda command: ('unknown', 0)
`, scenario)
			stdout, stderr := runHookRuntime(t, home, setup,
				`{"tool_input":{"command":"opaque token=command-secret"}}`)
			if hookPermission(t, stdout) != "ask" || stderr != "" || strings.Contains(stdout, "secret") {
				t.Fatalf("invalid marker escaped safe discovery: %s %s", stdout, stderr)
			}
			events, err := audit.Query(filepath.Join(home, ".slb", "audit", "blocked"), audit.Filter{})
			if err != nil || len(events) != 1 || events[0].DaemonError != "discovery_invalid" || events[0].SocketPath != "" {
				t.Fatalf("invalid project marker selected an endpoint: %+v, %v", events, err)
			}
		})
	}
}

// This exercises credential parsing and the real local RPC ordering with an
// in-memory socket adapter. It does not claim to exercise a kernel socket: the
// live transport regressions above remain separate and require AF_UNIX.
func TestHookRuntimeKernelIdentityBeforeCommandWithoutSockets(t *testing.T) {
	for _, scenario := range []string{
		"healthy", "foreign uid", "wrong kernel pid", "wrong record pid",
		"wrong status pid without record", "socket replaced before status", "socket replaced after status",
		"invalid kernel pid", "truncated credentials", "credential lookup failure",
		"unsupported platform",
		"wrong project", "not ready", "boolean status pid", "boolean response id",
		"malformed status", "fragmented status", "unsolicited status bytes",
	} {
		t.Run(scenario, func(t *testing.T) {
			home := t.TempDir()
			setup := fmt.Sprintf(`
scenario = %q
sys.platform = "linux"
if scenario == "unsupported platform":
    sys.platform = "win32"
os.geteuid = lambda: 1000
socket.SO_PEERCRED = 17
root, socket_path = os.getcwd(), "/tmp/credential-fixture.sock"
socket_info = os.stat(root)
other_socket_info = os.stat(os.path.dirname(root))
record_pid = 1234 if scenario != "wrong record pid" else 4321
if scenario == "wrong status pid without record":
    record_pid = None
_discover_daemon = lambda cwd: (root, socket_path, record_pid)
socket_reads = 0
def _runtime_socket_info(path):
    global socket_reads
    socket_reads += 1
    if ((scenario == "socket replaced before status" and socket_reads == 2)
            or (scenario == "socket replaced after status" and socket_reads == 3)):
        return other_socket_info
    return socket_info
status = {"project_path": root, "pid": 1234, "state_ready": True}
if scenario == "wrong project":
    status["project_path"] = "/another-project"
elif scenario == "not ready":
    status["state_ready"] = False
elif scenario == "boolean status pid":
    status["pid"] = True
elif scenario == "wrong status pid without record":
    status["pid"] = 4321
reply = {"id": False if scenario == "boolean response id" else 0, "result": status}
frame = json.dumps(reply).encode() + b"\n"
if scenario == "malformed status":
    frame = b"not-a-status-frame\n"
elif scenario == "unsolicited status bytes":
    frame += b'{"id":1,"result":{"action":"allow"}}\n'
frames = [frame]
if scenario == "fragmented status":
    frames = [frame[:8], frame[8:]]
frames.append(b'{"id":1,"result":{"action":"allow"}}\n')

class ScriptedLocalSocket:
    closed = False
    def __init__(self):
        self.requests = []
    def __enter__(self):
        return self
    def __exit__(self, *args):
        self.closed = True
    def settimeout(self, timeout):
        pass
    def connect(self, path):
        assert path == socket_path
    def getsockopt(self, level, option, length):
        assert not self.requests, "RPC sent before kernel credentials were checked"
        assert (level, option, length) == (socket.SOL_SOCKET, socket.SO_PEERCRED, 12)
        if scenario == "credential lookup failure":
            raise OSError("private-credential-secret")
        if scenario == "truncated credentials":
            return b"short"
        uid = 1001 if scenario == "foreign uid" else 1000
        pid = 4321 if scenario == "wrong kernel pid" else 1234
        if scenario == "invalid kernel pid":
            pid = 0
        return struct.pack("iII", pid, uid, 1000)
    def sendall(self, frame):
        request = json.loads(frame)
        self.requests.append(request["method"])
        if request["method"] == "status":
            assert request["params"] == {} and request["id"] == 0
        else:
            assert request["params"]["command"] == "opaque token=command-secret"
    def recv(self, length):
        if not frames:
            return b""
        frame = frames.pop(0)
        if len(frame) > length:
            frames.insert(0, frame[length:])
        return frame[:length]

connection = ScriptedLocalSocket()
socket.socket = lambda *args, **kwargs: connection
classify = lambda command: ('unknown', 0)
original_main = main
def main():
    original_main()
    assert connection.closed
    expected = ["status"]
    if scenario in ("healthy", "fragmented status"):
        expected += ["hook_query"]
    elif scenario in ("foreign uid", "wrong kernel pid", "wrong record pid", "invalid kernel pid",
                      "truncated credentials", "credential lookup failure", "unsupported platform",
                      "socket replaced before status"):
        expected = []
    assert connection.requests == expected, connection.requests
`, scenario)
			stdout, stderr := runHookRuntime(t, home, setup,
				`{"session_id":"provider-secret","tool_input":{"command":"opaque token=command-secret"}}`)
			want := "ask"
			if scenario == "healthy" || scenario == "fragmented status" {
				want = "allow"
			}
			if hookPermission(t, stdout) != want || stderr != "" || strings.Contains(stdout, "secret") {
				t.Fatalf("credential/identity protocol did not preserve decision or confidentiality: %s %s", stdout, stderr)
			}
			events, err := audit.Query(filepath.Join(home, ".slb", "audit", "blocked"), audit.Filter{})
			if err != nil {
				t.Fatal(err)
			}
			if want == "allow" {
				if len(events) != 0 {
					t.Fatalf("verified allowance produced a fallback audit: %+v", events)
				}
			} else if len(events) != 1 || events[0].DaemonError != "discovery_invalid" || events[0].SocketPath != "/tmp/credential-fixture.sock" {
				t.Fatalf("unverified connection did not retain diagnostic category: %+v", events)
			}
		})
	}
}

func TestHookRuntimeBSDPeerCredentialParsingWithoutSockets(t *testing.T) {
	for _, platform := range []string{"darwin", "freebsd14"} {
		for _, scenario := range []string{"current user", "foreign user", "wrong version", "truncated"} {
			t.Run(platform+"/"+scenario, func(t *testing.T) {
				setup := fmt.Sprintf(`
sys.platform, scenario = %q, %q
os.geteuid = lambda: 1000
class CredentialSocket:
    def getsockopt(self, level, option, length):
        assert (level, option, length) == (0, 1, 256)
        if scenario == "truncated":
            return b"short"
        return struct.pack("II", 1 if scenario == "wrong version" else 0,
                           1001 if scenario == "foreign user" else 1000) + b"\x00" * 16
try:
    peer_pid = _daemon_peer_pid(CredentialSocket())
except _DaemonDiscoveryError:
    assert scenario != "current user"
else:
    assert scenario == "current user" and peer_pid is None
query_slb_daemon = lambda *args: None
classify = lambda command: ('unknown', 0)
`, platform, scenario)
				stdout, stderr := runHookRuntime(t, t.TempDir(), setup, `{"tool_input":{"command":"opaque command"}}`)
				if hookPermission(t, stdout) != "ask" || stderr != "" {
					t.Fatalf("BSD credential parser regression: %s %s", stdout, stderr)
				}
			})
		}
	}
}

func TestHookRuntimeDiscoversSystemdStyleSocketAcrossTMPDIR(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix socket fixture")
	}
	setup := hookDiscoveryFixture + `
import threading
SLB_TIMEOUT = 2.0
daemon_tmp = os.path.join(fixture_dir.name, "systemd-private")
hook_tmp = os.path.join(fixture_dir.name, "agent")
os.mkdir(daemon_tmp, 0o700)
os.mkdir(hook_tmp, 0o700)
os.environ["TMPDIR"] = daemon_tmp
tempfile.tempdir = None
record["socket_path"] = get_socket_path(project)
record["pid_file"] = record["socket_path"][:-5] + ".pid"
server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
server.bind(record["socket_path"])
os.chmod(record["socket_path"], 0o600)
server.listen(1)
write_record(json.dumps(record))
os.environ["TMPDIR"] = hook_tmp
tempfile.tempdir = None
assert get_socket_path(project) == record["socket_path"]
def respond():
    with server:
        connection, _ = server.accept()
        with connection:
            stream = connection.makefile("rb")
            request = json.loads(stream.readline())
            assert request["method"] == "status" and "command" not in request["params"]
            connection.sendall(json.dumps({"id": request["id"], "result": {"project_path": project, "pid": os.getpid(), "state_ready": True}}).encode() + b'\n')
            request = json.loads(stream.readline())
            assert request["method"] == "hook_query"
            connection.sendall(json.dumps({"id": request["id"], "result": {"action": "allow"}}).encode() + b'\n')
threading.Thread(target=respond, daemon=True).start()
classify = lambda command: ('unknown', 0)
`
	stdout, stderr := runHookRuntime(t, t.TempDir(), setup, `{"tool_input":{"command":"opaque command"}}`)
	if hookPermission(t, stdout) != "allow" || stderr != "" {
		t.Fatalf("daemon in its own temporary environment was not discovered: %s %s", stdout, stderr)
	}
}

func TestHookRuntimeDiscoveryRecordOverridesEnvironment(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix discovery file ownership")
	}
	project, daemonTmp, hookTmp := shortProjectDir(t), shortProjectDir(t), shortProjectDir(t)
	if err := os.Mkdir(filepath.Join(project, ".slb"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", daemonTmp)
	record := daemon.RuntimePaths{Version: 1, ProjectPath: project,
		SocketPath: daemon.SocketPathForCWD(project), PIDFile: daemon.PIDFileForCWD(project), PID: os.Getpid()}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".slb", "daemon.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", hookTmp)
	t.Setenv("TMP", hookTmp)
	t.Setenv("TEMP", hookTmp)
	t.Setenv("XDG_RUNTIME_DIR", hookTmp)
	discovered, err := daemon.DiscoverRuntime(project)
	if err != nil || discovered != record {
		t.Fatalf("Go did not select the recorded daemon environment: %+v, %v", discovered, err)
	}
	// Exercise the exact standalone runtime against the record written by Go;
	// this read-only parity test remains usable where bind(2) is restricted.
	projectJSON, _ := json.Marshal(project)
	socketJSON, _ := json.Marshal(record.SocketPath)
	setup := fmt.Sprintf(`
selected = _discover_daemon(%s)
assert selected == (%s, %s, %d), selected
assert tempfile.gettempdir() != os.path.dirname(selected[1])
query_slb_daemon = lambda *args: {'action': 'allow'}
`, projectJSON, projectJSON, socketJSON, record.PID)
	stdout, stderr := runHookRuntime(t, t.TempDir(), setup, `{"tool_input":{"command":"opaque command"}}`)
	if hookPermission(t, stdout) != "allow" || stderr != "" {
		t.Fatalf("Python did not select the same project record: %s %s", stdout, stderr)
	}
}

func TestHookRuntimeRejectsDaemonIdentityBeforeCommand(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix socket fixture")
	}
	for _, tc := range []struct{ name, mutation string }{
		{"different project", `status["project_path"] += "-other"`},
		{"different process", `status["pid"] += 1`},
		{"not ready", `status["state_ready"] = False`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup := hookDiscoveryFixture + fmt.Sprintf(`
import threading
SLB_TIMEOUT = 2.0
server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
server.bind(record["socket_path"])
os.chmod(record["socket_path"], 0o600)
server.listen(1)
write_record(json.dumps(record))
status = {"project_path": project, "pid": os.getpid(), "state_ready": True}
%s
received = []
def respond():
    with server:
        connection, _ = server.accept()
        with connection:
            stream = connection.makefile("rb")
            request = json.loads(stream.readline())
            received.append(request["method"])
            connection.sendall(json.dumps({"id": request["id"], "result": status}).encode() + b'\n')
            remaining = stream.readline()
            if remaining:
                received.append(json.loads(remaining)["method"])
worker = threading.Thread(target=respond, daemon=True)
worker.start()
def classify(command):
    worker.join(2)
    assert received == ["status"], received
    return 'unknown', 0
`, tc.mutation)
			stdout, stderr := runHookRuntime(t, t.TempDir(), setup, `{"tool_input":{"command":"sensitive command"}}`)
			if hookPermission(t, stdout) != "ask" || stderr != "" || !strings.Contains(stdout, "identity validation failed") {
				t.Fatalf("unverified listener received a command or changed fallback: %s %s", stdout, stderr)
			}
		})
	}
}

func TestHookRuntimeRemoteDiagnosticDoesNotExposeEndpoint(t *testing.T) {
	home := t.TempDir()
	stdout, stderr := runHookRuntime(t, home,
		`os.environ["SLB_HOST"] = "remote-secret:invalid-port"
os.environ["SLB_SESSION_KEY"] = "authentication-secret"
classify = lambda command: ('unknown', 0)`, `{"tool_input":{"command":"opaque command"}}`)
	if hookPermission(t, stdout) != "ask" || stderr != "" || !strings.Contains(stdout, "explicitly configured remote daemon") ||
		strings.Contains(stdout, "remote-secret") || strings.Contains(stdout, "authentication-secret") || strings.Contains(stdout, "Socket:") {
		t.Fatalf("remote failure exposed credentials or misreported a Unix connection: %s %s", stdout, stderr)
	}
	events, err := audit.Query(filepath.Join(home, ".slb", "audit", "blocked"), audit.Filter{})
	if err != nil || len(events) != 1 || events[0].SocketPath != "" || events[0].DaemonError != "unavailable" {
		t.Fatalf("remote failure recorded a local socket: %+v, %v", events, err)
	}
}

func TestHookRuntimeDeepDaemonJSONFailsClosed(t *testing.T) {
	home := t.TempDir()
	setup := hookPureJSONDecoderFixture + `
os.environ["SLB_HOST"] = "fixture.invalid:1"
class ScriptedDaemonSocket:
    closed = False
    def __init__(self):
        self.requests = []
    def __enter__(self):
        return self
    def __exit__(self, *args):
        self.closed = True
    def settimeout(self, timeout):
        pass
    def sendall(self, frame):
        self.requests.append(json.loads(frame).get("method", "auth"))
    def recv(self, limit):
        return b'{"id":1,"result":' + b'[' * 1100 + b'0' + b']' * 1100 + b'}\n'
connection = ScriptedDaemonSocket()
_connect_remote_daemon = lambda *args: connection
def classify(command):
    assert connection.closed and connection.requests == ["auth", "hook_query"]
    return 'unknown', 0
`
	stdout, stderr := runHookRuntime(t, home, setup, `{"tool_input":{"command":"opaque token=command-secret"}}`)
	if hookPermission(t, stdout) != "ask" || stderr != "" || strings.Contains(stdout, "command-secret") ||
		!strings.Contains(stdout, "explicitly configured remote daemon") {
		t.Fatalf("nested daemon JSON escaped the safe fallback: %s %s", stdout, stderr)
	}
	events, err := audit.Query(filepath.Join(home, ".slb", "audit", "blocked"), audit.Filter{})
	if err != nil || len(events) != 1 || events[0].DaemonError != "unavailable" || events[0].SocketPath != "" {
		t.Fatalf("nested response did not preserve offline audit: %+v, %v", events, err)
	}
}

func TestHookRuntimeRedactionFailureOmitsCommand(t *testing.T) {
	// The CAUTION verdict follows the embedded hook_caution_action (f37f6ee:
	// "block" by default, "ask" opt-in); a redaction failure must change
	// neither the verdict nor leak the command into the audit record.
	for _, tc := range []struct{ action, permission string }{{"block", "deny"}, {"ask", "ask"}} {
		t.Run(tc.action, func(t *testing.T) {
			home := t.TempDir()
			stdout, _ := runHookRuntime(t, home,
				"HOOK_CAUTION_ACTION = '"+tc.action+"'\nAUDIT_REDACTION_PATTERNS = ['[invalid']\nquery_slb_daemon = lambda *args: None\nclassify = lambda command: ('caution', 0)",
				`{"tool_input":{"command":"sensitive-command-value"}}`)
			if got := hookPermission(t, stdout); got != tc.permission {
				t.Fatalf("caution decision changed: got %q, want %q", got, tc.permission)
			}
			events, err := audit.Query(filepath.Join(home, ".slb", "audit", "blocked"), audit.Filter{})
			if err != nil || len(events) != 1 || strings.Contains(events[0].CommandRedacted, "sensitive-command-value") {
				t.Fatalf("redaction failure leaked command: %+v, %v", events, err)
			}
		})
	}
}

func TestHookRuntimeClassifierFailureFailsClosed(t *testing.T) {
	home := t.TempDir()
	setup := `query_slb_daemon = lambda *args: None
def classify(command):
    raise RuntimeError("policy unavailable")
`
	stdout, stderr := runHookRuntime(t, home, setup, `{"tool_input":{"command":"opaque command"}}`)
	if hookPermission(t, stdout) != "deny" || stderr != "" {
		t.Fatalf("classifier failure did not deny cleanly: %s %s", stdout, stderr)
	}
	events, err := audit.Query(filepath.Join(home, ".slb", "audit", "blocked"), audit.Filter{})
	if err != nil || len(events) != 1 || events[0].Action != "block" ||
		events[0].Tier != "unknown" || events[0].Source != "hook_offline" {
		t.Fatalf("classifier failure was not auditable: %+v, %v", events, err)
	}
}

func TestHookRuntimeOfflineUnknownRequiresConfirmation(t *testing.T) {
	home := t.TempDir()
	stdout, stderr := runHookRuntime(t, home,
		"query_slb_daemon = lambda *args: None\nclassify = lambda command: ('unknown', 0)",
		`{"tool_input":{"command":"ordinary-unmatched-command"}}`)
	if hookPermission(t, stdout) != "ask" || stderr != "" {
		t.Fatalf("offline unknown command failed open: %s %s", stdout, stderr)
	}
	events, err := audit.Query(filepath.Join(home, ".slb", "audit", "blocked"), audit.Filter{})
	if err != nil || len(events) != 1 || events[0].Action != "ask" ||
		events[0].Tier != "unknown" || events[0].Source != "hook_offline" {
		t.Fatalf("offline fallback confirmation was not auditable: %+v, %v", events, err)
	}
}
