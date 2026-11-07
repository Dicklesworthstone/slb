
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
import stat
import struct
import subprocess
import sys
import tempfile
import time
import unicodedata
import uuid
from typing import Optional

SLB_TIMEOUT = 0.05
MAX_DISCOVERY_BYTES = 8192
MAX_UNIX_SOCKET_BYTES = 103


# Project discovery and execution use the nearest .slb marker, except slb's
# own ~/.slb while it holds no state.db. Approval records preserve literal
# paths; authenticated local discovery uses strict physical project identity.
def _is_user_dir_without_state(marker: str, strict=False) -> bool:
    """Mirror config.IsUserDirWithoutState: slb's own ~/.slb marks no project until it holds state.db.

    The home comes from the variable Go's os.UserHomeDir reads; directories are
    compared physically, so an aliased home matches. Strict callers must refuse
    an unreadable marker or state path rather than change project boundaries."""
    home = os.environ.get("USERPROFILE" if os.name == "nt" else "HOME", "")
    if not home:
        return False
    try:
        user_info = os.stat(os.path.join(home, ".slb"))
    except (OSError, ValueError):
        return False
    try:
        marker_info = os.stat(marker)
    except (OSError, ValueError):
        if strict:
            raise
        return False
    if not os.path.samestat(marker_info, user_info):
        return False
    try:
        os.lstat(os.path.join(marker, "state.db"))
    except FileNotFoundError:
        return True
    except (OSError, ValueError):
        if strict:
            raise
        return False
    return False


def _project_root_for_approval(start: str) -> str:
    """Preserve the path spelling bound to existing approval records."""
    try:
        path = os.path.abspath(start)
    except (OSError, ValueError):
        return start
    while True:
        marker = os.path.join(path, ".slb")
        if os.path.isdir(marker) and not _is_user_dir_without_state(marker):
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


class _DaemonDiscoveryError(ValueError):
    pass


def _physical_project_root(cwd):
    """Mirror native physical discovery without skipping malformed markers.

    Approval records retain their literal paths through the separate approval
    helper. Local socket discovery must stop at an invalid nearest boundary.
    """
    try:
        if (not isinstance(cwd, str) or not os.path.isabs(cwd)
                or "\x00" in cwd or not os.path.isdir(cwd)):
            raise _DaemonDiscoveryError("invalid project working directory")
        root = physical_cwd = os.path.realpath(cwd)
        while True:
            marker = os.path.join(root, ".slb")
            try:
                os.lstat(marker)
            except FileNotFoundError:
                pass
            else:
                if not stat.S_ISDIR(os.stat(marker).st_mode):
                    raise _DaemonDiscoveryError("invalid project state directory")
                if not _is_user_dir_without_state(marker, strict=True):
                    return root
            parent = os.path.dirname(root)
            if parent == root:
                return physical_cwd
            root = parent
    except (OSError, ValueError, RecursionError) as error:
        raise _DaemonDiscoveryError("project identity is unavailable") from error


def _require_unix_discovery_capabilities():
    """Keep ownership and no-follow checks mandatory on every local path."""
    required_flags = ("O_DIRECTORY", "O_NOFOLLOW", "O_NONBLOCK", "O_CLOEXEC")
    if (os.name != "posix" or not callable(getattr(os, "geteuid", None))
            or not hasattr(socket, "AF_UNIX")
            or any(not isinstance(getattr(os, flag, None), int) or getattr(os, flag, 0) == 0
                   for flag in required_flags)
            or os.open not in getattr(os, "supports_dir_fd", ())
            or os.stat not in getattr(os, "supports_dir_fd", ())
            or os.stat not in getattr(os, "supports_follow_symlinks", ())):
        raise _DaemonDiscoveryError("secure Unix daemon discovery is unavailable")


def _unique_discovery_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise _DaemonDiscoveryError("duplicate discovery field")
        result[key] = value
    return result


def _valid_runtime_path(value):
    if (not isinstance(value, str) or not os.path.isabs(value)
            or value != os.path.normpath(value) or (os.name != "nt" and value.startswith("//"))):
        return False
    try:
        value.encode("utf-8")
    except UnicodeError:
        return False
    return not any(unicodedata.category(character) == "Cc" for character in value)


def _validate_runtime_parents(directory):
    remaining = [512]
    def ancestors(path):
        path = os.path.normpath(path)
        while True:
            remaining[0] -= 1
            if remaining[0] < 0:
                raise _DaemonDiscoveryError("too many runtime ancestors")
            info = os.lstat(path)
            if info.st_uid not in (0, os.geteuid()):
                raise _DaemonDiscoveryError("unsafe daemon runtime directory")
            if stat.S_ISLNK(info.st_mode):
                target = os.readlink(path)
                if not os.path.isabs(target):
                    target = os.path.join(os.path.dirname(path), target)
                ancestors(target)
            elif (not stat.S_ISDIR(info.st_mode)
                    or (info.st_mode & 0o022 and not info.st_mode & stat.S_ISVTX)):
                raise _DaemonDiscoveryError("unsafe daemon runtime directory")
            parent = os.path.dirname(path)
            if parent == path:
                return
            path = parent
    ancestors(directory)


def _read_daemon_discovery(root):
    """Read only this physical project's private, bounded discovery record.

    A missing record permits the deterministic legacy address. Any malformed
    or unsafe record fails closed; it must never select another daemon.
    """
    _require_unix_discovery_capabilities()
    marker = os.path.join(root, ".slb")
    directory_fd = None
    record_fd = None
    try:
        try:
            directory = os.lstat(marker)
        except FileNotFoundError:
            return None
        if (not stat.S_ISDIR(directory.st_mode) or directory.st_uid != os.geteuid()
                or directory.st_mode & 0o022):
            raise _DaemonDiscoveryError("unsafe project state directory")
        _validate_runtime_parents(root)
        directory_fd = os.open(marker, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC)
        if not os.path.samestat(directory, os.fstat(directory_fd)):
            raise _DaemonDiscoveryError("project state directory changed")
        try:
            record_info = os.stat("daemon.json", dir_fd=directory_fd, follow_symlinks=False)
        except FileNotFoundError:
            return None
        if (not stat.S_ISREG(record_info.st_mode) or record_info.st_uid != os.geteuid()
                or record_info.st_mode & 0o077 or record_info.st_nlink != 1
                or record_info.st_size > MAX_DISCOVERY_BYTES):
            raise _DaemonDiscoveryError("unsafe discovery file")
        record_fd = os.open("daemon.json", os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC,
                            dir_fd=directory_fd)
        if not os.path.samestat(record_info, os.fstat(record_fd)):
            raise _DaemonDiscoveryError("discovery file changed")
        with os.fdopen(record_fd, "rb") as stream:
            record_fd = None
            encoded = stream.read(MAX_DISCOVERY_BYTES + 1)
        if len(encoded) > MAX_DISCOVERY_BYTES:
            raise _DaemonDiscoveryError("discovery file too large")
        record = json.loads(encoded.decode("utf-8"), object_pairs_hook=_unique_discovery_object)
        if (not isinstance(record, dict)
                or set(record) != {"version", "project_path", "socket_path", "pid_file", "pid"}
                or type(record["version"]) is not int or record["version"] != 1
                or record["project_path"] != root
                or not _valid_runtime_path(record["socket_path"])
                or not _valid_runtime_path(record["pid_file"])
                or record["pid_file"] == record["socket_path"]
                or len(os.fsencode(record["socket_path"])) > MAX_UNIX_SOCKET_BYTES
                or type(record["pid"]) is not int or not 0 < record["pid"] <= sys.maxsize):
            raise _DaemonDiscoveryError("invalid discovery record")
        current = os.stat("daemon.json", dir_fd=directory_fd, follow_symlinks=False)
        if (not os.path.samestat(record_info, current)
                or not os.path.samestat(directory, os.lstat(marker))):
            raise _DaemonDiscoveryError("discovery file changed while reading")
        return record
    except (OSError, ValueError, UnicodeError, NotImplementedError, RecursionError) as error:
        raise _DaemonDiscoveryError("daemon discovery is invalid") from error
    finally:
        if record_fd is not None:
            os.close(record_fd)
        if directory_fd is not None:
            os.close(directory_fd)


def _discover_daemon(cwd):
    root = _physical_project_root(cwd)
    record = _read_daemon_discovery(root)
    if record is not None:
        return root, record["socket_path"], record["pid"]
    digest = hashlib.sha256(root.encode()).hexdigest()[:12]
    return root, os.path.join(tempfile.gettempdir(), f"slb-{digest}.sock"), None


def get_socket_path(cwd=None) -> str:
    return _discover_daemon(cwd if cwd is not None else os.getcwd())[1]


def _runtime_socket_info(socket_path):
    _require_unix_discovery_capabilities()
    if not _valid_runtime_path(socket_path) or len(os.fsencode(socket_path)) > MAX_UNIX_SOCKET_BYTES:
        raise _DaemonDiscoveryError("invalid socket path")
    info = os.lstat(socket_path)
    if (not stat.S_ISSOCK(info.st_mode) or info.st_uid != os.geteuid()
            or info.st_mode & 0o077):
        raise _DaemonDiscoveryError("unsafe daemon socket")
    _validate_runtime_parents(os.path.dirname(socket_path))
    return info


def _daemon_peer_pid(sock):
    """Authenticate the connected OS user, with a kernel PID where available.

    Filesystem ownership and daemon-reported identity do not replace kernel
    credentials for the actual connection. This remains a same-user boundary.
    """
    try:
        uid = os.geteuid()
        if sys.platform.startswith("linux"):
            credentials = sock.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12)
            pid, peer_uid, _ = struct.unpack("iII", credentials)
            if peer_uid != uid or pid <= 0:
                raise _DaemonDiscoveryError("daemon peer identity mismatch")
            return pid
        if sys.platform == "darwin" or sys.platform.startswith("freebsd"):
            # SOL_LOCAL / LOCAL_PEERCRED on Darwin and FreeBSD. xucred begins
            # with uint32 version and UID; trailing fields are platform-specific.
            credentials = sock.getsockopt(0, 1, 256)
            if len(credentials) < 8:
                raise _DaemonDiscoveryError("incomplete daemon credentials")
            version, peer_uid = struct.unpack("II", credentials[:8])
            if version != 0 or peer_uid != uid:
                raise _DaemonDiscoveryError("daemon peer identity mismatch")
            return None  # These credentials do not supply a portable peer PID.
        raise _DaemonDiscoveryError("native socket authentication required")
    except (OSError, ValueError, AttributeError, NotImplementedError, struct.error) as error:
        raise _DaemonDiscoveryError("daemon peer credentials are unavailable or invalid") from error


def _daemon_rpc(sock, deadline, method, params, request_id, limit=65536):
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise TimeoutError("daemon deadline expired")
    sock.settimeout(remaining)
    sock.sendall(json.dumps({"method": method, "params": params, "id": request_id}).encode() + b"\n")
    response = bytearray()
    while b"\n" not in response:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("daemon deadline expired")
        sock.settimeout(remaining)
        chunk = sock.recv(4096)
        if not chunk:
            raise OSError("daemon closed connection")
        response.extend(chunk)
        if len(response) > limit:
            raise ValueError("daemon response too large")
    frame, remainder = response.split(b"\n", 1)
    data = json.loads(frame.decode())
    if (remainder or not isinstance(data, dict) or type(data.get("id")) is not int
            or data["id"] != request_id or data.get("error") is not None
            or not isinstance(data.get("result"), dict)):
        raise ValueError("invalid daemon response")
    return data["result"]


def _connect_remote_daemon(endpoint, timeout):
    host, separator, port = endpoint.rpartition(":")
    if host.startswith("[") and host.endswith("]"):
        host = host[1:-1]
    elif ":" in host:
        raise ValueError("invalid remote daemon endpoint")
    if not separator or not host or not port.isascii() or not port.isdigit() or not 0 < int(port) < 65536:
        raise ValueError("invalid remote daemon endpoint")
    return socket.create_connection((host, int(port)), timeout=timeout)


def query_slb_daemon(command: str, session_id: str, cwd: str, diagnostics=None) -> Optional[dict]:
    """Verify project identity before sending a command on the same connection."""
    if diagnostics is None:
        diagnostics = {}
    diagnostics.clear()
    diagnostics["daemon_error"] = "unavailable"
    try:
        deadline = time.monotonic() + SLB_TIMEOUT
        remote = os.environ.get("SLB_HOST", "").strip()
        if remote:
            diagnostics["remote"] = True
            root, expected_pid = _project_root_for_socket(cwd), None
            connection = _connect_remote_daemon(remote, SLB_TIMEOUT)
        else:
            root, socket_path, expected_pid = _discover_daemon(cwd)
            diagnostics["socket_path"] = socket_path
            info = _runtime_socket_info(socket_path)
            connection = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        with connection as sock:
            sock.settimeout(max(0.001, deadline - time.monotonic()))
            if remote:
                sock.sendall(json.dumps({"auth": os.environ.get("SLB_SESSION_KEY", "").strip()}).encode() + b"\n")
            else:
                sock.connect(socket_path)
                peer_pid = _daemon_peer_pid(sock)
                if peer_pid is not None and expected_pid is not None and peer_pid != expected_pid:
                    raise _DaemonDiscoveryError("daemon peer does not match the discovery record")
                if not os.path.samestat(info, _runtime_socket_info(socket_path)):
                    raise _DaemonDiscoveryError("socket changed during connection")
                try:
                    # The framed reader refuses unsolicited bytes after status;
                    # they cannot become authority for a command sent later.
                    status = _daemon_rpc(sock, deadline, "status", {}, 0, 4096)
                except (ValueError, UnicodeError, RecursionError) as error:
                    raise _DaemonDiscoveryError("invalid daemon identity frame") from error
                if (status.get("project_path") != root or status.get("state_ready") is not True
                        or type(status.get("pid")) is not int or status["pid"] <= 0
                        or (peer_pid is not None and status["pid"] != peer_pid)
                        or (expected_pid is not None and status["pid"] != expected_pid)):
                    raise _DaemonDiscoveryError("daemon identity mismatch")
                if not os.path.samestat(info, _runtime_socket_info(socket_path)):
                    raise _DaemonDiscoveryError("socket changed during connection")
            result = _daemon_rpc(sock, deadline, "hook_query", {
                "command": command, "session_id": session_id, "cwd": cwd,
                "execution_handoff": True,
            }, 1)
            diagnostics.clear()
            return result
    except (_DaemonDiscoveryError, NotImplementedError):
        diagnostics["daemon_error"] = "discovery_invalid"
    except (TimeoutError, socket.timeout):
        diagnostics["daemon_error"] = "timeout"
    except (OSError, ValueError, UnicodeError, RecursionError):
        pass
    return None


def _daemon_diagnostic_message(diagnostics):
    category = diagnostics.get("daemon_error")
    if category == "discovery_invalid":
        message = "SLB OFFLINE: daemon discovery or identity validation failed; used local policy."
    elif category == "timeout":
        message = "SLB OFFLINE: daemon did not answer within the hook deadline; used local policy."
    else:
        message = "SLB OFFLINE: daemon unavailable; used local policy."
    if diagnostics.get("remote"):
        return message + " Transport: explicitly configured remote daemon."
    socket_path = diagnostics.get("socket_path")
    if isinstance(socket_path, str) and socket_path:
        return message + " Socket: " + json.dumps(socket_path, ensure_ascii=True) + "."
    return message + " No socket was selected."


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
            if not _is_user_dir_without_state(marker):
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


def _record_audit(command, session_id, cwd, action, tier, min_approvals, source, matched_pattern="", approval_policy="", diagnostics=None) -> bool:
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
        if diagnostics:
            if isinstance(diagnostics.get("socket_path"), str) and not diagnostics.get("remote"):
                event["socket_path"] = diagnostics["socket_path"]
            if diagnostics.get("daemon_error") in ("unavailable", "timeout", "discovery_invalid"):
                event["daemon_error"] = diagnostics["daemon_error"]
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
                      matched_pattern="", audit_recorded=False, approval_policy="", diagnostics=None):
    action = _normalized_action(action)
    if not isinstance(message, str):
        message = "SLB: invalid response reason; asking for confirmation."
    if action != "allow" and diagnostics:
        message = _daemon_diagnostic_message(diagnostics) + " " + message
    notice = _approval_policy_notice(approval_policy)
    if action != "allow" and notice and notice not in message:
        message += " Review policy: " + notice + "."
    if action != "allow" and audit_recorded is not True:
        if not _record_audit(command, session_id, cwd, action, tier, min_approvals, source, matched_pattern, approval_policy, diagnostics):
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
    diagnostics = {}
    response = query_slb_daemon(command, session_id, cwd, diagnostics)
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
                          "unknown", 0, "hook_offline", approval_policy=HOOK_APPROVAL_POLICY, diagnostics=diagnostics)
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
                      approval_policy=HOOK_APPROVAL_POLICY, diagnostics=diagnostics)


if __name__ == "__main__":
    main()
