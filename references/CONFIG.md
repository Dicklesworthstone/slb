# SLB Configuration — Reference

## Table of Contents
- [Configuration Hierarchy](#configuration-hierarchy)
- [Example Configuration](#example-configuration)
- [Approval Policy](#approval-policy)
- [Advanced Configuration](#advanced-configuration)
- [Daemon Architecture](#daemon-architecture)
- [Dry Run & Rollback](#dry-run--rollback)
- [Troubleshooting](#troubleshooting)

---

## Configuration Hierarchy

Configuration is hierarchical (lowest to highest priority):
1. Built-in defaults
2. User config (`~/.slb/config.toml`)
3. Project config (`.slb/config.toml`)
4. Environment variables (`SLB_*`)
5. Command-line flags

---

## Example Configuration

```toml
[general]
approval_policy = "independent"    # Default; see the explicit same_host option below
min_approvals = 2
request_timeout = 1800              # 30 minutes
approval_ttl_minutes = 30
timeout_action = "escalate"         # or "auto_reject", "auto_approve_warn"
require_different_model = true      # Reviewer must use different AI model

[rate_limits]
max_pending_per_session = 5
max_requests_per_minute = 10

[notifications]
desktop_enabled = true
webhook_url = "https://slack.com/webhook/..."

[daemon]
tcp_addr = ""                       # For Docker/remote agents
tcp_require_auth = true

[agents]
trusted_self_approve = ["senior-agent"]
trusted_self_approve_delay_seconds = 300
```

---

## Approval Policy

`general.approval_policy` accepts two values:

| Value | Behavior |
|-------|----------|
| `independent` (default) | Requires independently authenticated reviewer identities. No external authority is implemented, so local approvals and execution requiring them are refused. |
| `same_host` | Allows cooperating local sessions to perform low-assurance peer review. It catches mistakes but does not protect against malicious processes sharing the OS user. |

Opt in for new requests in the current project:

```bash
slb config set general.approval_policy same_host
```

Or edit the existing `[general]` section in `.slb/config.toml`:

```toml
[general]
approval_policy = "same_host"
```

Requests keep the policy chosen at creation. Approving a request and executing it
require the current configuration to match that stored policy. Switching to
`same_host` does not revive historical local approvals or convert pending
independent requests; submit a new request. Switching back to `independent`
disables execution of outstanding same-host requests and never promotes their
reviews to independently authenticated authorization.

Quorum, command binding, signatures, expiration and the optional different-model
check remain active. Different-model checks compare self-reported labels.
Self-review is rejected in `same_host`, including agents listed in
`trusted_self_approve`. CLI output, stored evidence and execution history label
the low-assurance policy. See [the security reference](SECURITY.md#optional-same-host-peer-review-issue-31)
for the threat model and historical-evidence handling.

---

## Advanced Configuration

### Cross-Project Reviews

Target-project policy controls the review. Cross-project delegation does not
create an independent identity boundary; same-host reviews retain their
low-assurance label across projects.

```toml
[general]
cross_project_reviews = true
review_pool = ["agent-a", "agent-b", "human-reviewer"]
```

### Conflict Resolution

```toml
[general]
conflict_resolution = "any_rejection_blocks"  # Default
# Options: any_rejection_blocks | first_wins | human_breaks_tie
```

### Dynamic Quorum

```toml
[patterns.critical]
dynamic_quorum = true
dynamic_quorum_floor = 2    # Minimum approvals even with few reviewers
```

---

## Daemon Architecture

### IPC Communication

Unix domain sockets (project-specific):
```
/tmp/slb-<hash>.sock
```

### JSON-RPC Protocol

All daemon communication uses JSON-RPC 2.0:

```json
{"jsonrpc": "2.0", "method": "hook_query", "params": {"command": "rm -rf /"}, "id": 1}
```

**Available methods**: `hook_query`, `hook_health`, `verify_execution`, `subscribe`

### TCP Mode (Docker/Remote)

```toml
[daemon]
tcp_addr = "0.0.0.0:9876"
tcp_require_auth = true
tcp_allowed_ips = ["192.168.1.0/24"]
```

### Timeout Handling

| Action | Behavior |
|--------|----------|
| `escalate` | Transition to ESCALATED, notify humans (default) |
| `auto_reject` | Automatically reject the request |
| `auto_approve_warn` | Auto-approve CAUTION tier with warning |

---

## Dry Run & Rollback

### Dry Run Pre-flight

For supported commands, SLB can run a dry-run variant first:

| Command | Dry-run variant |
|---------|-----------------|
| `terraform apply` | `terraform plan` |
| `kubectl apply` | `kubectl diff` |
| `git push` | `git diff` (show what would change) |

```toml
[general]
enable_dry_run = true
```

### Rollback State Capture

Before executing, SLB can capture state:

```toml
[general]
enable_rollback_capture = true
max_rollback_size_mb = 100
```

**Captured state includes:**
- **Filesystem**: Tar archive of affected paths
- **Git**: HEAD commit, branch, dirty state, untracked files
- **Kubernetes**: YAML manifests of affected resources

---

## Troubleshooting

### "Daemon not running" warning

SLB works without daemon (file-based polling). Start for real-time:

```bash
slb daemon start
```

### "Active session already exists"

Resume it with the session ID and key that `slb session start` printed:

```bash
slb session resume --agent "YourAgent" --session-id <id> --session-key <key>
```

slb stores only a digest of the key and cannot print it again. If the key is
lost, end that session and start a new one:

```bash
slb session end --session-id <id>
slb session start --agent "YourAgent" --program <prog> --model <model>
```

### Approval expired

Re-request:
```bash
slb run "<command>" --reason "..."
```

### "Independent reviewer identity unavailable"

This is expected with the default `independent` policy: local session credentials
cannot attest a separate principal. If the intended use is cooperating local
agents checking each other's work, explicitly select `same_host` and submit a
new request. For mutually untrusted agents, use a separately protected identity
authority and execution boundary; this backend does not implement one.

### Approval policy changed

The request's stored policy differs from current configuration. Review evidence
is not converted when configuration changes. Submit a new request under the
intended policy, or restore the request's policy only if its assurance is still
the intended one.

### Command hash mismatch

Command was modified after approval. Re-request for the modified command.
