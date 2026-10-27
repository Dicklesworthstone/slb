
# === SLB Hook Integration ===
# Appended to the generated classifier and AUDIT_REDACTION_PATTERNS by Go.
import datetime
import hashlib
import json
import os
import re
import shlex
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import uuid
from typing import Optional

SLB_TIMEOUT = 0.05


def _project_root_for_approval(start: str) -> str:
    """Preserve the path spelling bound to existing approval records."""
    try:
        path = os.path.abspath(start)
    except (OSError, ValueError):
        return start
    while True:
        if os.path.isdir(os.path.join(path, ".slb")):
            return path
        parent = os.path.dirname(path)
        if parent == path:
            return os.path.abspath(start)
        path = parent


def _project_root_for_socket(start: str) -> str:
    """Use physical project identity, matching daemon PID/socket discovery."""
    try:
        path = os.path.abspath(start)
        # Go's EvalSymlinks leaves missing paths unchanged. Avoid Python's
        # partial resolution of a missing path through an existing alias.
        if os.path.exists(path):
            path = os.path.realpath(path)
    except (OSError, ValueError):
        return _project_root_for_approval(start)
    return _project_root_for_approval(path)


def get_socket_path(cwd=None) -> str:
    root = _project_root_for_socket(cwd if cwd is not None else os.getcwd())
    digest = hashlib.sha256(root.encode()).hexdigest()[:12]
    return os.path.join(tempfile.gettempdir(), f"slb-{digest}.sock")


def query_slb_daemon(command: str, session_id: str, cwd: str) -> Optional[dict]:
    """Read one bounded JSON-RPC frame within a single total time budget."""
    socket_path = get_socket_path(cwd)
    if not os.path.exists(socket_path):
        return None
    try:
        deadline = time.monotonic() + SLB_TIMEOUT
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as sock:
            sock.settimeout(SLB_TIMEOUT)
            sock.connect(socket_path)
            request = {"method": "hook_query", "params": {
                "command": command, "session_id": session_id, "cwd": cwd,
                "execution_handoff": True,
            }, "id": 1}
            sock.sendall(json.dumps(request).encode() + b"\n")
            response = bytearray()
            while b"\n" not in response:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    return None
                sock.settimeout(remaining)
                chunk = sock.recv(4096)
                if not chunk:
                    return None
                response.extend(chunk)
                if len(response) > 65536:
                    return None
            data = json.loads(response.split(b"\n", 1)[0].decode())
            if isinstance(data, dict) and data.get("id") == 1 and isinstance(data.get("result"), dict):
                return data["result"]
    except (OSError, ValueError, UnicodeError):
        pass
    return None


def _normalized_action(action) -> str:
    if not isinstance(action, str):
        return "ask"
    action = action.strip().lower()
    if action == "allow":
        return "allow"
    if action in ("block", "deny"):
        return "block"
    return "ask"


def _approval_policy_notice(policy):
    if policy == "same_host":
        return "same-host peer review (low assurance; not independently authenticated authorization)"
    if policy == "independent":
        return "independently authenticated authorization required"
    return ""


def _emit_decision(action, message: str = "") -> None:
    action = _normalized_action(action)
    permission = "deny" if action == "block" else action
    payload = {"hookSpecificOutput": {
        "hookEventName": "PreToolUse", "permissionDecision": permission,
    }}
    if message and permission != "allow":
        payload["hookSpecificOutput"]["permissionDecisionReason"] = message
    print(json.dumps(payload))


def _emit_execution_handoff(response, tool_input, session_id, cwd):
    """Replace Bash input with the atomic executor, not a raw-shell permit.

    The daemon only looks up a candidate. Competing retries, expiry, policy
    escalation, and mutation are checked again when slb execute claims it.
    No process is launched by this hook, and a missing executable fails closed.
    """
    handoff = response.get("execution_handoff")
    if not isinstance(handoff, dict):
        raise ValueError("missing handoff")
    request_id = handoff.get("request_id")
    command_hash = handoff.get("command_hash")
    database_path = handoff.get("database_path")
    if not isinstance(request_id, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_-]{0,127}", request_id):
        raise ValueError("invalid request ID")
    if not isinstance(command_hash, str) or not re.fullmatch(r"[a-f0-9]{64}", command_hash):
        raise ValueError("invalid command hash")
    if not session_id or handoff.get("session_id") != session_id or "\x00" in session_id:
        raise ValueError("session mismatch")
    database_path, log_directory = _execution_paths(cwd, database_path)
    config_path = _execution_config(cwd, handoff.get("config_path", ""), handoff.get("config_project", ""))
    if config_path or response.get("config_path") or response.get("config_project"):
        actual = _execution_config(cwd, response.get("config_path", ""), response.get("config_project", ""))
        if not config_path or not actual or not os.path.samefile(config_path, actual):
            raise ValueError("handoff policy differs from daemon classification")
    executable = shutil.which("slb")
    if executable is None:
        raise ValueError("slb executable unavailable")
    argv = [os.path.abspath(executable), "execute", "--db", database_path,
            "--log-dir", log_directory,
            "--session-id", session_id, "--expected-command-hash", command_hash, "--json"]
    if config_path:
        argv += ["--config", config_path]
    timeout = tool_input.get("timeout")
    if type(timeout) is int and timeout > 0:
        argv += ["--timeout", str(max(1, timeout // 1000))]
    argv += ["--", request_id]
    updated = dict(tool_input)
    # exec removes the wrapper shell so cancellation reaches the SLB process.
    updated["command"] = "exec " + " ".join(shlex.quote(arg) for arg in argv)
    context = "SLB is executing the reviewed request once and recording its outcome."
    notice = _approval_policy_notice(handoff.get("approval_policy", response.get("approval_policy")))
    if notice:
        context += " Review policy: " + notice + "."
    print(json.dumps({"hookSpecificOutput": {
        "hookEventName": "PreToolUse", "permissionDecision": "allow",
        "updatedInput": updated,
        "additionalContext": context,
    }}))


def _execution_config(cwd, config_path, config_project):
    if config_path == "":
        if config_project != "":
            raise ValueError("hook policy scope has no config file")
        return ""
    if (not isinstance(config_path, str) or not isinstance(config_project, str)
            or not os.path.isabs(config_path) or not os.path.isabs(config_project)
            or "\x00" in config_path or "\x00" in config_project):
        raise ValueError("hook policy paths must be absolute")
    if (not os.path.isdir(config_project)
            or os.path.realpath(config_project) != _project_root_for_socket(cwd)
            or not os.path.isfile(config_path)):
        raise ValueError("hook policy file or project unavailable")
    return os.path.normpath(config_path)


def _forward_selected_policy(input_data):
    """Use current native policy instead of an old embedded config snapshot.

    The native guard scopes this override to its owner, so a global hook used
    in another or nested initialized project selects that project's own rules.
    """
    config_path = globals().get("HOOK_POLICY_CONFIG", "")
    config_project = globals().get("HOOK_POLICY_PROJECT", "")
    if (not isinstance(config_path, str) or not isinstance(config_project, str)
            or not os.path.isabs(config_path) or not os.path.isabs(config_project)
            or "\x00" in config_path or "\x00" in config_project):
        raise ValueError("invalid installed hook policy binding")
    executable = shutil.which("slb")
    if executable is None:
        raise ValueError("selected policy requires the native slb guard")
    result = subprocess.run([os.path.abspath(executable), "hook", "guard",
                             "--config", config_path, "--policy-project", config_project],
                            input=json.dumps(input_data), text=True, capture_output=True,
                            timeout=15, check=False)
    if result.returncode != 0 or len(result.stdout) > 2 * 1024 * 1024:
        raise ValueError("native selected-policy guard failed")
    payload = json.loads(result.stdout)
    specific = payload.get("hookSpecificOutput") if isinstance(payload, dict) else None
    if (not isinstance(specific, dict) or specific.get("hookEventName") != "PreToolUse"
            or specific.get("permissionDecision") not in ("allow", "deny", "ask")):
        raise ValueError("native selected-policy guard returned invalid output")
    print(json.dumps(payload))


def _execution_paths(cwd, database_path):
    """Validate the physical project DB while preserving stored approval paths."""
    if not isinstance(cwd, str) or not os.path.isabs(cwd) or not os.path.isdir(cwd):
        raise ValueError("invalid project working directory")
    root = physical_cwd = os.path.realpath(cwd)
    while True:
        marker = os.path.join(root, ".slb")
        try:
            os.lstat(marker)
        except FileNotFoundError:
            pass
        else:
            if not os.path.isdir(marker):
                raise ValueError("invalid project state directory")
            break
        parent = os.path.dirname(root)
        if parent == root:
            root = physical_cwd
            break
        root = parent
    state_directory = os.path.realpath(os.path.join(root, ".slb"))
    expected_db = os.path.join(state_directory, "state.db")
    if (not isinstance(database_path, str) or not os.path.isabs(database_path)
            or not os.path.isfile(expected_db) or not os.path.isfile(database_path)):
        raise ValueError("project database unavailable")
    expected_db = os.path.realpath(expected_db)
    if os.path.realpath(database_path) != expected_db:
        raise ValueError("project database mismatch")
    return expected_db, os.path.join(state_directory, "logs")


def _record_audit(command, session_id, cwd, action, tier, min_approvals, source, matched_pattern="", approval_policy="") -> bool:
    """Publish the same private JSONL schema as internal/audit, without slb or a daemon."""
    action = _normalized_action(action)
    if action == "allow":
        return True
    pending = None
    try:
        display = command
        try:
            for pattern in AUDIT_REDACTION_PATTERNS:
                display = re.sub(pattern, "[REDACTED]", display)
        except (NameError, re.error, TypeError):
            display = "[command omitted: redaction unavailable]"
        now = datetime.datetime.now(datetime.timezone.utc)
        event_id = uuid.uuid4().hex
        event = {
            "version": 1, "id": event_id,
            "timestamp": now.isoformat().replace("+00:00", "Z"),
            "command_redacted": display,
            "command_hash": hashlib.sha256((command + "\x00" + cwd).encode()).hexdigest(),
            "cwd": cwd, "session_id": session_id, "action": action,
            "tier": tier if isinstance(tier, str) else "unknown",
            "min_approvals": min_approvals if type(min_approvals) is int else 0,
            "matched_pattern": matched_pattern if isinstance(matched_pattern, str) else "",
            "source": source,
        }
        notice = _approval_policy_notice(approval_policy)
        if notice:
            event["approval_policy"] = approval_policy
            event["review_notice"] = notice
        data = (json.dumps(event, ensure_ascii=False) + "\n").encode()
        if len(data) > 65536:
            raise ValueError("audit record exceeds 64 KiB")
        home = os.path.expanduser("~")
        if not os.path.isabs(home):
            raise ValueError("home directory is unavailable")
        directory = os.path.join(home, ".slb", "audit", "blocked")
        os.makedirs(directory, mode=0o700, exist_ok=True)
        fd, pending = tempfile.mkstemp(prefix=".pending-", dir=directory)
        with os.fdopen(fd, "wb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        name = now.strftime("%Y%m%dT%H%M%S.%f") + "000Z_" + event_id + ".jsonl"
        os.replace(pending, os.path.join(directory, name))
        pending = None
        return True
    except (OSError, ValueError, TypeError) as error:
        print("SLB: audit recording failed (" + type(error).__name__ + "); safety decision unchanged.", file=sys.stderr)
        return False
    finally:
        if pending is not None:
            try:
                os.unlink(pending)
            except OSError:
                pass


def _decide_and_audit(command, session_id, cwd, action, message, tier, min_approvals, source,
                      matched_pattern="", audit_recorded=False, approval_policy=""):
    action = _normalized_action(action)
    if not isinstance(message, str):
        message = "SLB: invalid response reason; asking for confirmation."
    notice = _approval_policy_notice(approval_policy)
    if action != "allow" and notice and notice not in message:
        message += " Review policy: " + notice + "."
    if action != "allow" and audit_recorded is not True:
        if not _record_audit(command, session_id, cwd, action, tier, min_approvals, source, matched_pattern, approval_policy):
            message += " Audit recording failed; safety decision unchanged."
    _emit_decision(action, message)


def main():
    """Inspect the command, record the observed decision, and emit one verdict."""
    try:
        input_data = json.loads(sys.stdin.read())
    except (ValueError, UnicodeError):
        _emit_decision("ask", "SLB: invalid hook input; unable to inspect the command.")
        return
    if not isinstance(input_data, dict) or not isinstance(input_data.get("tool_input"), dict):
        _emit_decision("ask", "SLB: invalid hook input; unable to inspect the command.")
        return
    command = input_data["tool_input"].get("command")
    if not isinstance(command, str):
        _emit_decision("ask", "SLB: missing or invalid command; unable to inspect it.")
        return
    if not command:
        _emit_decision("allow")
        return
    # Provider session IDs are not SLB sessions. An explicit SLB session wins;
    # the fallback supports integrations already supplying an SLB session ID.
    session_id = os.environ.get("SLB_SESSION_ID") or input_data.get("session_id", "")
    if not isinstance(session_id, str):
        session_id = ""
    cwd = input_data.get("cwd", os.getcwd())
    if not isinstance(cwd, str) or not os.path.isabs(cwd) or "\x00" in cwd:
        _emit_decision("ask", "SLB: invalid working directory; unable to inspect the command.")
        return
    if globals().get("HOOK_POLICY_CONFIG", "") or globals().get("HOOK_POLICY_PROJECT", ""):
        try:
            _forward_selected_policy(input_data)
        except (OSError, ValueError, TypeError, subprocess.SubprocessError):
            _decide_and_audit(command, session_id, cwd, "block",
                              "SLB: selected hook policy unavailable; restore its file and native slb guard before retrying.",
                              "unknown", 0, "hook_offline", "policy_load_error")
        return
    response = query_slb_daemon(command, session_id, cwd)
    if response is not None:
        if response.get("action") == "execute":
            try:
                _emit_execution_handoff(response, input_data["tool_input"], session_id, cwd)
            except (OSError, ValueError, TypeError):
                _decide_and_audit(command, session_id, cwd, "block",
                                  "SLB: execution handoff unavailable or invalid; use slb execute explicitly.",
                                  response.get("tier", "unknown"), response.get("min_approvals", 0), "hook_daemon",
                                  approval_policy=response.get("approval_policy", ""))
            return
        _decide_and_audit(command, session_id, cwd, response.get("action"), response.get("message", ""),
                          response.get("tier", "unknown"), response.get("min_approvals", 0), "hook_daemon",
                          response.get("matched_pattern", ""), response.get("audit_recorded", False),
                          response.get("approval_policy", ""))
        return
    try:
        tier, min_approvals = classify(command)
    except Exception:
        _decide_and_audit(command, session_id, cwd, "block",
                          "SLB: classification failed; command blocked until policy is available.",
                          "unknown", 0, "hook_offline", approval_policy=HOOK_APPROVAL_POLICY)
        return
    if tier == "critical":
        action = "block"
        message = f"SLB CRITICAL: Requires {min_approvals} approvals. Use 'slb request' to submit."
    elif tier == "dangerous":
        action = "block"
        message = f"SLB DANGEROUS: Requires {min_approvals} approval. Use 'slb request' to submit."
    elif tier == "caution":
        if HOOK_CAUTION_ACTION == "ask":
            action, message = "ask", "SLB CAUTION: command requires confirmation. Proceed?"
        else:
            action, message = "block", (
                "SLB CAUTION: submit with 'slb request'; configured auto-approval "
                "policy applies after admission."
            )
    elif tier == "safe":
        action, message = "allow", ""
    elif tier == "unknown":
        action, message = "ask", (
            "SLB OFFLINE: command is not covered by the local policy snapshot; "
            "confirmation required while the daemon is unavailable.")
    else:
        action, message = "block", "SLB: invalid local classification; command blocked."
    _decide_and_audit(command, session_id, cwd, action, message, tier, min_approvals, "hook_offline",
                      approval_policy=HOOK_APPROVAL_POLICY)


if __name__ == "__main__":
    main()
