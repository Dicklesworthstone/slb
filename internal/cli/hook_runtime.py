
# === SLB Hook Integration ===
# Appended to the generated classifier and AUDIT_REDACTION_PATTERNS by Go.
import datetime
import hashlib
import json
import os
import re
import socket
import sys
import tempfile
import time
import uuid
from typing import Optional

SLB_TIMEOUT = 0.05


def _project_root_for_socket(start: str) -> str:
    """Use the nearest .slb ancestor, matching the daemon socket discovery."""
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


def get_socket_path() -> str:
    root = _project_root_for_socket(os.getcwd())
    digest = hashlib.sha256(root.encode()).hexdigest()[:12]
    return os.path.join(tempfile.gettempdir(), f"slb-{digest}.sock")


def query_slb_daemon(command: str, session_id: str, cwd: str) -> Optional[dict]:
    """Read one bounded JSON-RPC frame within a single total time budget."""
    socket_path = get_socket_path()
    if not os.path.exists(socket_path):
        return None
    try:
        deadline = time.monotonic() + SLB_TIMEOUT
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as sock:
            sock.settimeout(SLB_TIMEOUT)
            sock.connect(socket_path)
            request = {"method": "hook_query", "params": {
                "command": command, "session_id": session_id, "cwd": cwd,
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


def _emit_decision(action, message: str = "") -> None:
    action = _normalized_action(action)
    permission = "deny" if action == "block" else action
    payload = {"hookSpecificOutput": {
        "hookEventName": "PreToolUse", "permissionDecision": permission,
    }}
    if message and permission != "allow":
        payload["hookSpecificOutput"]["permissionDecisionReason"] = message
    print(json.dumps(payload))


def _record_audit(command, session_id, cwd, action, tier, min_approvals, source, matched_pattern="") -> bool:
    """Publish the same private JSONL schema as internal/audit, without slb or a daemon."""
    action = _normalized_action(action)
    if action == "allow":
        return True
    pending = None
    try:
        display = command
        # Go exports its default redaction rules; there is no separate Python
        # ruleset that can silently drift from the daemon's privacy behavior.
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
        # Never include the command or a potentially secret-bearing exception
        # message, and never contaminate the JSON protocol on stdout.
        print("SLB: audit recording failed (" + type(error).__name__ + "); safety decision unchanged.", file=sys.stderr)
        return False
    finally:
        if pending is not None:
            try:
                os.unlink(pending)
            except OSError:
                pass


def _decide_and_audit(command, session_id, cwd, action, message, tier, min_approvals, source,
                      matched_pattern="", audit_recorded=False):
    action = _normalized_action(action)
    if not isinstance(message, str):
        message = "SLB: invalid response reason; asking for confirmation."
    if action != "allow" and audit_recorded is not True:
        if not _record_audit(command, session_id, cwd, action, tier, min_approvals, source, matched_pattern):
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
    session_id = input_data.get("session_id", "")
    if not isinstance(session_id, str):
        session_id = ""
    cwd = os.getcwd()
    response = query_slb_daemon(command, session_id, cwd)
    if response is not None:
        _decide_and_audit(command, session_id, cwd, response.get("action"), response.get("message", ""),
                          response.get("tier", "unknown"), response.get("min_approvals", 0), "hook_daemon",
                          response.get("matched_pattern", ""), response.get("audit_recorded", False))
        return
    try:
        tier, min_approvals = classify(command)
    except Exception:
        # A classifier failure must not become a silent permit.
        _decide_and_audit(command, session_id, cwd, "ask", "SLB: classification failed; confirmation required.",
                          "unknown", 0, "hook_offline")
        return
    if tier == "critical":
        action = "block"
        message = f"SLB CRITICAL: Requires {min_approvals} approvals. Use 'slb request' to submit."
    elif tier == "dangerous":
        action = "block"
        message = f"SLB DANGEROUS: Requires {min_approvals} approval. Use 'slb request' to submit."
    elif tier == "caution":
        action, message = "ask", "SLB CAUTION: command requires confirmation. Proceed?"
    else:
        action, message = "allow", ""
    _decide_and_audit(command, session_id, cwd, action, message, tier, min_approvals, "hook_offline")


if __name__ == "__main__":
    main()
