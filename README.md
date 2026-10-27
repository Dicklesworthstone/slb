# Simultaneous Launch Button (slb)

[![Go Version](https://img.shields.io/badge/go-1.24.13+-blue.svg)](https://golang.org)
[![License](https://img.shields.io/badge/License-MIT%2BOpenAI%2FAnthropic%20Rider-blue.svg)](LICENSE)
[![Build Status](https://github.com/Dicklesworthstone/slb/workflows/CI/badge.svg)](https://github.com/Dicklesworthstone/slb/actions)

A cross-platform CLI for reviewing potentially destructive commands from AI coding agents before execution.

When an agent wants to run something risky (e.g., `rm -rf`, `git push --force`, `kubectl delete`, `DROP TABLE`), `slb` requires peer review and explicit approval before execution.

**Choose the assurance level explicitly:** The default `independent` approval policy refuses local approvals and execution requiring independently authenticated reviewers. No external identity provider is implemented yet. Cooperating agents on one host can opt into `same_host` peer review, a **mistake-prevention workflow, not a security boundary**. This mode does not authenticate agent or model identities and cannot contain a malicious process sharing the same OS user. Requests, reviews, execution records, and history keep their policy labels; local reviews never become independent authorization. See [the identity boundary and deployment requirements](references/SECURITY.md#reviewer-identity-security-boundary-issue-30).

## Why This Exists

Coding agents can get tunnel vision, hallucinate, or misunderstand context. A second reviewer (ideally with a different model/tooling) catches mistakes before they become irreversible.

`slb` is built for **multi-agent workflows** where many agent terminals run in parallel and a single bad command could destroy work, data, or infrastructure.

## Key Features

- **Risk-Based Classification**: Commands are automatically classified by risk level
- **Client-Side Execution**: Commands run in YOUR shell environment (inheriting AWS credentials, kubeconfig, virtualenvs, etc.)
- **Command Hash Binding**: Approvals bind to the exact command via SHA-256 hash
- **SQLite Source of Truth**: Project state lives in `.slb/state.db`
- **Agent Mail Integration**: Notify reviewers and track audit trails via MCP Agent Mail
- **TUI Dashboard**: Interactive terminal UI for human reviewers

## Risk Tiers

| Tier | Approvals | Auto-approve | Examples |
|------|-----------|--------------|----------|
| **CRITICAL** | 2+ | Never | `rm -rf /`, `DROP DATABASE`, `terraform destroy`, `git push --force` |
| **DANGEROUS** | 1 | Never | `rm -rf ./build`, `git reset --hard`, `kubectl delete`, `DROP TABLE` |
| **CAUTION** | 0 | After 30s | `rm file.txt`, `git branch -d`, `npm uninstall` |
| **SAFE** | 0 | Immediately | `rm *.log`, `git stash`, `kubectl delete pod` |

These counts are review requirements. With the default `independent` policy,
local sessions cannot satisfy a requirement for independent approval. With
`same_host`, they count cooperating local reviewers under the limitations above.

## Quick Start

### Installation

#### Recommended: Homebrew (macOS/Linux)

```bash
brew install dicklesworthstone/tap/slb
```

This method provides:
- Automatic updates via `brew upgrade`
- Dependency management
- Easy uninstall via `brew uninstall`

#### Windows: Scoop

```powershell
scoop bucket add dicklesworthstone https://github.com/Dicklesworthstone/scoop-bucket
scoop install dicklesworthstone/slb
```

#### Alternative: Direct Download

Choose the matching archive from the [latest release](https://github.com/Dicklesworthstone/slb/releases/latest):

- Linux x86_64: `slb_<version>_linux_amd64.tar.gz`
- Linux ARM64: `slb_<version>_linux_arm64.tar.gz`
- macOS Intel: `slb_<version>_darwin_amd64.tar.gz`
- macOS ARM64: `slb_<version>_darwin_arm64.tar.gz`
- Windows x86_64: `slb_<version>_windows_amd64.zip`

Download `checksums.txt` from that same release and verify the archive's SHA-256
before extracting it. The archive contains `slb` on Linux/macOS or `slb.exe` on
Windows. Windows ARM64 archives are not currently published.

#### Alternative: Install Script

```bash
curl -fsSL "https://raw.githubusercontent.com/Dicklesworthstone/slb/main/scripts/install.sh?$(date +%s)" | bash
```

The installer verifies the archive against `checksums.txt` from the selected
release and reports the retained download workspace. Set `INSTALL_DIR` to choose
the destination. For qualification with a known scratch location,
`SLB_RETAIN_WORK_ROOT` may name an absolute path that does not exist yet and whose
parent already exists; it is created privately and retained on success or failure.

#### Build from Source

```bash
git clone https://github.com/Dicklesworthstone/slb.git
cd slb && make build
```

#### Go Install

```bash
go install github.com/Dicklesworthstone/slb/cmd/slb@latest
```

### Initialize a Project

```bash
cd /path/to/your/project
slb init
```

This creates a `.slb/` directory with:
- `state.db` - SQLite database for requests, reviews, and sessions
- `config.toml` - Project-specific configuration
- `pending/` - JSON files for pending requests (for watching/interop)

### Same-Host Peer-Review Workflow

Enable this only when the agents cooperate and you want a second session to
check for mistakes. From the initialized project, opt in before creating requests:

```bash
slb config set general.approval_policy same_host
```

This writes `approval_policy = "same_host"` under `[general]` in
`.slb/config.toml`. The default is `"independent"`. Each request keeps the policy
under which it was created; changing configuration does not convert existing
requests or reviews. Both approving a request and executing it require the current
policy to match. Set it back to `independent` to disable local approval and
execution of outstanding same-host requests.

```bash
# 1. Each agent starts a session and retains its own returned ID and key.
slb session start --agent "Requester" --program "claude-code" --model "opus"
slb session start --agent "Reviewer" --program "codex-cli" --model "codex"

# 2. The requester explains the exact command and its intended effects.
slb request "rm -rf ./build" --session-id <requester-id> \
  --reason "Clean generated artifacts before a fresh compile" \
  --expected-effect "Remove only this project's build directory" \
  --safety "Source files are outside build; all outputs can be regenerated"

# 3. A different agent inspects the command, working directory and explanation.
slb show <request-id>
slb approve <request-id> --session-id <reviewer-id> --session-key <reviewer-key> \
  --comments "Checked the working directory and build contents"

# 4. The requester executes only after the required reviews are recorded.
slb execute <request-id> --session-id <requester-id>
```

The output identifies these as same-host reviews, not independently
authenticated authorization. Quorum, different-model checks when configured,
signatures, expiry, and command binding still apply. The requester cannot review
its own request, including through a restarted session with the same agent name
or the `trusted_self_approve` setting. Names and model labels remain self-reported.
`slb run` provides the same workflow while waiting and executing automatically
after review.

## Commands Reference

### Session Management

```bash
slb session start --agent <name> --program <prog> --model <model>
slb session end --session-id <id>
slb session resume --agent <name> --session-id <id> --session-key <key>  # Resume after crash
slb session list                               # Show active sessions
slb session heartbeat --session-id <id>        # Keep session alive
```

### Request & Run

```bash
# Primary command (atomic: check, request, wait, execute)
slb run "<command>" --reason "..." [--session-id <id>]

# Plumbing commands
slb request "<command>" --reason "..."         # Create request only
slb status <request-id> [--wait]               # Check status
slb pending [--all-projects]                   # List pending requests
slb cancel <request-id>                        # Cancel own request
```

### Review & Approve

```bash
slb review <request-id>                        # Show full details
slb approve <request-id> --session-id <id> --session-key <key>  # Review under the request's policy
slb reject <request-id> --session-id <id> --reason "..."
```

### Execution

```bash
slb execute <request-id>                       # Execute approved request
slb emergency-execute "<cmd>" --reason "..."   # Unreviewed override (logged)
slb rollback <request-id>                      # Rollback if captured
```

### Pattern Management

```bash
slb patterns list [--tier critical|dangerous|caution|safe]
slb patterns test "<command>"                  # Check what tier a command would be
slb patterns add --tier dangerous "<pattern>"  # Agents can add patterns
```

### Daemon & TUI

```bash
slb daemon start [--foreground]                # Start background daemon
slb daemon stop                                # Stop daemon
slb daemon status                              # Check daemon status
slb tui                                        # Launch interactive TUI
slb watch --session-id <id> --json             # Stream events for agents
```

## Configuration

Configuration is hierarchical (lowest to highest priority):
1. Built-in defaults
2. User config (`~/.slb/config.toml`)
3. Project config (`.slb/config.toml`)
4. Environment variables (`SLB_*`)
5. Command-line flags

### Example Configuration

```toml
[general]
approval_policy = "independent"    # Default; "same_host" explicitly opts into local workflow review
min_approvals = 2
request_timeout = 1800              # 30 minutes
approval_ttl_minutes = 30
timeout_action = "escalate"         # or "auto_reject", "auto_approve_warn"

[rate_limits]
max_pending_per_session = 5
max_requests_per_minute = 10

[notifications]
desktop_enabled = true
desktop_delay_seconds = 60

[daemon]
tcp_addr = ""                       # For Docker/remote agents
tcp_require_auth = true
```

## Default Patterns

### CRITICAL (2+ approvals)

| Pattern | Description |
|---------|-------------|
| `rm -rf /...` | Recursive deletion of `/`, system paths, or home roots (`/home`, `/home/user`, `/Users/user`, `/root`, `~`) |
| `DROP DATABASE/SCHEMA` | SQL database destruction |
| `TRUNCATE [TABLE] [ONLY] ...` | SQL data destruction (TABLE is optional) |
| `terraform destroy` | Infrastructure destruction |
| `kubectl delete node/namespace/pv/pvc` | Kubernetes critical resources |
| `git push --force`, `git push origin +main` | Force push (not with-lease), including `+` refspecs |
| `aws terminate-instances` | Cloud resource destruction |
| `dd ... of=/dev/` | Direct disk writes |

### DANGEROUS (1 approval)

| Pattern | Description |
|---------|-------------|
| `rm -rf` | Recursive force delete |
| `git reset --hard` | Discard all changes |
| `git clean -fd` | Remove untracked files |
| `kubectl delete` | Delete Kubernetes resources |
| `curl ... \| bash`, `bash <(curl ...)` | Downloaded script run by a shell |
| `terraform destroy -target` | Targeted destroy |
| `DROP TABLE` | SQL table destruction |
| `chmod -R`, `chown -R` | Recursive permission changes |

For deletion, `-r`, `-R`, and `--recursive` express recursive intent; `-f`
alone remains CAUTION. Project descendants such as `/home/user/project/build`
are DANGEROUS, while home roots and system directories remain CRITICAL.
`build` and `./build` resolve against the same working directory, and any
protected operand makes a recursive deletion CRITICAL.

### CAUTION (auto-approved after 30s)

| Pattern | Description |
|---------|-------------|
| `rm <file>` | Single file deletion |
| `git stash drop` | Discard stashed changes |
| `git branch -d` | Delete local branch |
| `npm/pip uninstall` | Package removal |

### SAFE (skip review)

| Pattern | Description |
|---------|-------------|
| `rm *.log`, `rm *.tmp`, `rm *.bak` | Temporary file cleanup |
| `git stash` | Stash changes (not drop) |
| `kubectl delete pod` | Pod deletion (pods are ephemeral) |
| `npm cache clean` | Cache cleanup |

## IDE Integration

### Claude Code Hooks

Add to your `AGENTS.md`:

```markdown
## SLB Integration

Before running any destructive command, use slb:

\`\`\`bash
# Instead of running directly:
rm -rf ./build

# Use slb:
slb run "rm -rf ./build" --reason "Clean build before fresh compile"
\`\`\`

All DANGEROUS and CRITICAL commands must go through slb review.
```

Generate Claude Code hooks:

```bash
slb integrations claude-hooks > ~/.claude/hooks.json
```

### Cursor Rules

Generate Cursor rules:

```bash
slb integrations cursor-rules > .cursorrules
```

## Shell Completions

```bash
# zsh (~/.zshrc)
eval "$(slb completion zsh)"

# bash (~/.bashrc)
eval "$(slb completion bash)"

# fish (~/.config/fish/config.fish)
slb completion fish | source
```

## Architecture

```
.slb/
├── state.db          # SQLite database (source of truth)
├── config.toml       # Project configuration
├── pending/          # JSON snapshots for watching
│   └── req-<uuid>.json
├── sessions/         # Session files
└── logs/             # Execution logs
    └── req-<uuid>.log
```

**Key Design Decision**: Client-side execution. The daemon checks review evidence and policy; it is not an independently protected authorization service. Commands execute in the calling process's shell environment to inherit:
- AWS_PROFILE, AWS_ACCESS_KEY_ID
- KUBECONFIG
- Activated virtualenvs
- SSH_AUTH_SOCK
- Database connection strings

## Troubleshooting

### "Daemon not running" warning

This is expected - slb works without the daemon (file-based polling). Start the daemon for real-time updates:

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

Approvals have a TTL (30min default, 10min for CRITICAL). Re-request if expired:

```bash
slb run "<command>" --reason "..."  # Creates new request
```

### Command hash mismatch

The command was modified after review. Submit a new request for the modified
command; changing its stored hash does not make the existing signed review valid.

## Safety Note

`slb` adds friction and peer review for dangerous actions. It does NOT replace:
- Least-privilege credentials
- Environment safeguards
- Proper access controls
- Backup strategies

Use slb as **defense in depth**, not your only protection.

## Claude Code Hook Integration

To integrate with Claude Code, `slb` provides a PreToolUse hook that intercepts Bash commands before execution.

### Quick Setup

```bash
# Install hook (generates script and updates Claude Code settings)
slb hook install

# Check installation status
slb hook status

# Test classification without executing
slb hook test "rm -rf ./build"
```

### How It Works

1. **Hook Script**: A Python script at `~/.slb/hooks/slb_guard.py` intercepts Bash tool calls
2. **Pattern Matching**: Commands are classified using embedded patterns (same as the daemon)
3. **Daemon Communication**: For approval checks, the hook connects to the SLB daemon via Unix socket
4. **Fail-Closed**: If SLB is unavailable, dangerous commands are blocked by default

### Hook Commands

```bash
slb hook generate                 # Generate hook script only
slb hook install [--global]       # Install to Claude Code settings
slb hook uninstall                # Remove hook from settings
slb hook status                   # Show installation status
slb hook test "<command>"         # Test command classification
```

The hook returns one of three actions to Claude Code:
- `allow` - Command proceeds without intervention
- `ask` - User is prompted (CAUTION tier)
- `block` - Command is blocked with message to use `slb request`

## Pattern Matching Engine

The pattern matching engine is the core of `slb`'s command classification system.

### Classification Algorithm

1. **Normalization**: Commands are parsed using shell-aware tokenization
   - Strips wrapper prefixes: `sudo`, `doas`, `env`, `time`, `nohup`, etc.
   - Extracts inner commands from `bash -c 'command'` patterns
   - Resolves paths: `./foo` → `/absolute/path/foo`

2. **Compound Command Handling**: Commands with `;`, `&&`, `||`, `|` are split and each segment is classified independently. The **highest risk segment determines the overall tier**.
   ```
   echo "done" && rm -rf /etc    →  CRITICAL (rm -rf /etc wins)
   ls && git status              →  SAFE (no dangerous patterns)
   ```

3. **Shell-Aware Splitting**: Separators inside quotes are preserved:
   ```
   psql -c "DELETE FROM users; DROP TABLE x;"  →  Single segment (SQL)
   echo "foo" && rm -rf /tmp                   →  Two segments
   ```

4. **Pattern Precedence**: Patterns are checked in order: SAFE → CRITICAL → DANGEROUS → CAUTION
   - First match wins within each tier
   - SAFE patterns skip review entirely

5. **Fail-Safe Parse Handling**: If command parsing fails (unbalanced quotes, complex escapes), the tier is **upgraded by one level**:
   - SAFE → CAUTION
   - CAUTION → DANGEROUS
   - DANGEROUS → CRITICAL

### Fallback Detection

For commands that wrap SQL (e.g., `psql -c "..."`, `mysql -e "..."`), pattern matching may not catch embedded statements. The engine includes fallback detection:

```
DELETE FROM ... (no WHERE clause)  →  CRITICAL
DELETE FROM ... WHERE ...          →  DANGEROUS
```

### Runtime Pattern Management

Agents can add patterns at runtime:

```bash
slb patterns add --tier dangerous "docker system prune"
slb patterns list --tier critical
slb patterns test "kubectl delete deployment nginx"
```

Pattern changes are persisted to SQLite and take effect immediately.

## Request Lifecycle

Requests follow a well-defined state machine with strict transition rules.

### State Diagram

```
                    ┌─────────────┐
                    │   PENDING   │
                    └──────┬──────┘
           ┌───────────────┼───────────────┐───────────────┐
           ▼               ▼               ▼               ▼
     ┌──────────┐    ┌──────────┐    ┌──────────┐    ┌──────────┐
     │ APPROVED │    │ REJECTED │    │ CANCELLED│    │ TIMEOUT  │
     └────┬─────┘    └──────────┘    └──────────┘    └────┬─────┘
          │              (terminal)      (terminal)       │
          ▼                                               ▼
     ┌──────────┐                                   ┌──────────┐
     │EXECUTING │                                   │ESCALATED │
     └────┬─────┘                                   └────┬─────┘
          │                                              │
   ┌──────┴──────┬──────────┐                 ┌─────────┴─────────┐
   ▼             ▼          ▼                 ▼                   ▼
┌────────┐  ┌─────────┐  ┌────────┐      ┌──────────┐       ┌──────────┐
│EXECUTED│  │EXEC_FAIL│  │TIMED_OUT│     │ APPROVED │       │ REJECTED │
└────────┘  └─────────┘  └────────┘      └──────────┘       └──────────┘
(terminal)   (terminal)   (terminal)
```

### Terminal States

Once a request reaches a terminal state, no further transitions are allowed:
- **EXECUTED**: Command completed successfully
- **EXECUTION_FAILED**: Command returned non-zero exit code
- **TIMED_OUT**: Command exceeded execution timeout
- **CANCELLED**: Request was cancelled by the requester
- **REJECTED**: Request was rejected by a reviewer

### Approval TTL

Approvals have a time-to-live to prevent stale approvals:
- **Standard requests**: 30 minutes (configurable)
- **CRITICAL requests**: 10 minutes (stricter by default)

If an approval expires before execution, submit a new request for review.

## Execution Verification

Before a reviewed command executes, its policy and supporting evidence must
verify in addition to these execution checks. `same_host` evidence is accepted
only for a same-host request while that policy remains enabled. The default
`independent` policy continues to refuse local approvals, including historical
ones. An `approved` status alone is never an execution permit.

### Gate 1: Status Check
Request must be in APPROVED state.

### Gate 2: Approval Expiry
Approval TTL must not have elapsed.

### Gate 3: Command Hash
SHA-256 hash of the command must match. This ensures the exact approved command is executed, with no modifications allowed after approval.

### Gate 4: Tier Consistency
Risk tier must still match (patterns may have changed since approval).

### Gate 5: First-Executor-Wins
Only one executor can claim the request. Atomic database transition prevents race conditions when multiple agents try to execute.

## Dry Run & Rollback

### Dry Run Pre-flight

For supported commands, `slb` can run a dry-run variant before the real execution:

```bash
# Supported dry-run variants:
terraform plan          # instead of terraform apply
kubectl diff            # instead of kubectl apply
git diff                # show what would change
```

Enable in config:
```toml
[general]
enable_dry_run = true
```

### Rollback State Capture

Before executing, `slb` can capture state for potential rollback:

```toml
[general]
enable_rollback_capture = true
max_rollback_size_mb = 100
```

Captured state includes:
- **Filesystem**: Tar archive of affected paths
- **Git**: HEAD commit, branch, dirty state, untracked files
- **Kubernetes**: YAML manifests of affected resources

Rollback:
```bash
slb rollback <request-id>           # Restore captured state
slb rollback <request-id> --force   # Force overwrite
```

## Daemon Architecture

The daemon provides real-time notifications and execution verification.

### IPC Communication

Primary communication uses Unix domain sockets:
```
/tmp/slb-<hash>.sock
```

The socket and PID file (`/tmp/slb-<hash>.pid`) use the same hash of the discovered project root, allowing independent project daemons to coexist. Both files use the system temporary directory, including `TMPDIR` when set. Use `-C /path/to/project` with `slb daemon start`, `stop`, or `status` to select a project. Selection falls back to `SLB_PROJECT`, then the working directory; all three resolve symlink aliases and discover the nearest ancestor containing `.slb`, so commands also work from nested directories. If no ancestor contains `.slb`, the selected directory is the project root.

Lifecycle commands manage the local project daemon regardless of `SLB_HOST`. Companion `.pid.lock` and `.sock.lock` files remain after shutdown by design; leave them in place so daemon ownership remains coordinated across starts and stops.

When upgrading from a version with the shared per-user PID file, stop any existing daemon through its original foreground session or process manager, then restart it with the new version. The socket naming format and client IPC remain compatible. The new `stop` command requires project-aware shutdown support and deliberately refuses to signal a process from the old shared PID file.

### JSON-RPC Protocol

All daemon communication uses JSON-RPC 2.0:

```json
{"jsonrpc": "2.0", "method": "hook_query", "params": {"command": "rm -rf /"}, "id": 1}
```

Available methods:
- `hook_query` - Classify command and check approvals
- `hook_health` - Health check with pattern hash
- `verify_execution` - Check execution gates
- `subscribe` - Subscribe to request events

### TCP Mode (Docker/Remote)

For agents in containers or remote machines:

```toml
[daemon]
tcp_addr = "0.0.0.0:9876"
tcp_require_auth = true
tcp_allowed_ips = ["192.168.1.0/24"]
```

### Timeout Handling

When a request's approval window expires:

| Action | Behavior |
|--------|----------|
| `escalate` | Transition to ESCALATED, notify humans (default) |
| `auto_reject` | Automatically reject the request |
| `auto_approve_warn` | Auto-approve CAUTION tier with warning notification |

```toml
[general]
timeout_action = "escalate"
```

### Desktop Notifications

Native notifications on macOS (AppleScript), Linux (notify-send), and Windows (PowerShell):

```toml
[notifications]
desktop_enabled = true
desktop_delay_seconds = 60    # Wait before first notification
```

## Advanced Configuration

### Cross-Project Reviews

Allow reviewers from other projects:

```toml
[general]
cross_project_reviews = true
review_pool = ["agent-a", "agent-b", "human-reviewer"]
```

### Trusted Self-Approval

This legacy setting does not enable production self-approval: the default
`independent` policy still needs an external identity authority, and `same_host`
always requires another agent. A trusted name cannot supply a peer review.

```toml
[agents]
trusted_self_approve = ["senior-agent", "lead-developer"]
trusted_self_approve_delay_seconds = 300    # 5 minute delay
```

### Conflict Resolution

When approvals and rejections conflict:

```toml
[general]
conflict_resolution = "any_rejection_blocks"  # Default
# Options: any_rejection_blocks | first_wins | human_breaks_tie
```

### Different Model Requirement

Require reviewers to report a different AI model:

```toml
[general]
require_different_model = true
different_model_timeout = 300    # Escalate to human after 5 min
```

In `same_host` mode this compares session model labels. It helps cooperating
agents choose another model, but does not authenticate which model produced a review.

### Rate Limiting

Prevent request floods:

```toml
[rate_limits]
max_pending_per_session = 5      # Max concurrent pending requests
max_requests_per_minute = 10     # Rate limit per session
rate_limit_action = "reject"     # reject | queue | warn
```

Queue mode waits for capacity before creating a request. Use `--queue-timeout`
to bound that wait separately from `--timeout`, which bounds waiting for a
review decision. `general.request_timeout` is measured in seconds without
rounding to minutes, and the new request receives its full lifetime after
admission preparation.

Admission also resolves the requester's expired pending work without requiring
a daemon. It applies the current timeout policy and records the resulting
timeout or escalation; it never treats expiration as a peer approval. Pending
capacity can then be reused, while the per-minute request count remains intact.
Other requesters' work is unaffected, and invalid policy stops admission.

### Dynamic Quorum

Scale approval requirements based on active reviewers:

```toml
[patterns.critical]
dynamic_quorum = true
dynamic_quorum_floor = 2    # Minimum approvals even with few reviewers
```

### Webhook Notifications

Send events to external systems:

```toml
[notifications]
webhook_url = "https://slack.com/webhook/..."
```

Payload includes request details, classification, and event type.

## Security Design Principles

### Defense in Depth

`slb` implements multiple checks for the selected review policy. In `same_host`
mode they prevent workflow mistakes, not malicious actions by the same OS user:

1. **Pattern-based classification** - First line of defense
2. **Peer review requirement** - Human/agent oversight
3. **Command hash binding** - Tamper detection
4. **Approval TTL** - Prevent stale approvals
5. **Execution verification gates** - Pre-execution checks
6. **Audit logging** - Full traceability

### Cryptographic Guarantees

- **Command binding**: SHA-256 hash computed at request time, verified at execution
- **Review signatures**: HMAC-SHA256 keyed with the digest of the reviewer's session key. Same-host evidence uses a separate signature format that binds the decision to the exact command and review policy. Relabeling the policy or replacing the command invalidates that evidence. A holder of the signing material, including a process that can read the database, can still forge local reviews; a signature does not establish an independent principal.
- **Session keys**: Generated per session and shown once, by `slb session start` (or by `slb session resume` when it creates a session). SQLite stores only a SHA-256 digest of each key, and supplied keys are compared with it in constant time, so a copy or backup of `state.db` does not contain a key slb will accept. Upgrading converts existing keys in place and then rebuilds the file, so no old copy of a key is left in it; agents keep using the keys they hold. Hashing protects copies of the database. It does not separate agents that run as the same OS user: such a process can read another agent's key from its environment, files or terminal, write the database, or run commands directly. See [the identity boundary](references/SECURITY.md#reviewer-identity-security-boundary-issue-30).
- **Session recovery**: Resuming an existing session requires its original `--session-id` and `--session-key`, including with `--force`. Names, programs, and models are not recovery credentials. slb cannot return a lost key, because it does not have it; start a new session instead.

### Fail-Closed Behavior

When components fail:
- Daemon unreachable, or no answer within `integrations.hook_query_timeout_ms`
  (default 250ms) → the hook classifies locally: dangerous/critical commands are
  blocked and commands no pattern covers require confirmation. The audit source
  is `hook_native_timeout` for a deadline and `hook_native_offline` otherwise
- Parse error → Upgrade tier by one level
- Approval expired → Require new approval
- Hash mismatch → Reject execution

### Audit Trail

Every action is logged to SQLite with:
- Timestamp
- Actor (session ID, agent name)
- Action type
- Request/review details
- Approval policy and review assurance (same-host workflow reviews remain labeled)
- Outcome

Query history:
```bash
slb history [--days 7] [--session <id>] [--status executed]
```

## Environment Variables

All config options can be set via environment:

| Variable | Description |
|----------|-------------|
| `SLB_APPROVAL_POLICY` | `independent` (default) or explicitly opt into low-assurance `same_host` review |
| `SLB_MIN_APPROVALS` | Minimum approval count |
| `SLB_REQUEST_TIMEOUT` | Request timeout in seconds |
| `SLB_TIMEOUT_ACTION` | What to do on timeout |
| `SLB_DESKTOP_NOTIFICATIONS` | Enable desktop notifications |
| `SLB_WEBHOOK_URL` | Webhook notification URL |
| `SLB_DAEMON_TCP_ADDR` | TCP listen address |
| `SLB_TRUSTED_SELF_APPROVE` | Comma-separated trusted agents |
| `SLB_HOOK_QUERY_TIMEOUT_MS` | Native hook daemon-query deadline in ms (10-10000, default 250) |

## Agent Event Streaming

The `slb watch` command provides real-time event streaming for agent workflows.

### Event Stream Format

Events are streamed as newline-delimited JSON (NDJSON) for easy programmatic consumption:

```bash
slb watch --session-id <id>
```

Selected fields from the event stream:
```json
{"event":"request_pending","request_id":"abc123","status":"pending","risk_tier":"dangerous","approvals":0,"min_approvals":1,"approval_policy":"same_host","review_notice":"same-host peer review (low assurance; not independently authenticated authorization)"}
{"event":"request_approved","request_id":"abc123","status":"approved","approvals":1,"min_approvals":1,"approval_policy":"same_host"}
{"event":"request_executed","request_id":"abc123","status":"executed","exit_code":0,"approval_policy":"same_host","execution_assurance":"same_host"}
```

Request events also carry the project, redacted command display, command hash,
requester, rejection count, original `created_at` and any `resolved_at` timestamp.
`observed_at` records when the state was observed; it does not replace creation
time. The approval policy and its `review_notice` remain present throughout the
workflow, with stored execution assurance when available. Submitted vote counts
are display information, not verified execution permission. Use `slb execute` to
apply the current policy and verify the evidence.

### Event Types

| Event | Description |
|-------|-------------|
| `request_pending` | Request awaiting review, including changed vote counts |
| `request_approved` | Request was approved |
| `request_rejected` | Request was rejected |
| `request_executing` | Execution was claimed |
| `request_executed` | Command completed successfully |
| `request_execution_failed` | Execution failed |
| `request_timed_out` | Command execution timed out |
| `request_timeout` | Request timed out waiting for approval |
| `request_escalated` | Request was escalated |
| `request_cancelled` | Request was cancelled |

### Transport Modes

**Daemon IPC (preferred)**: Real-time streaming via Unix socket subscription when the daemon is running.

**Polling fallback**: If the daemon is unavailable, the command falls back to database polling at configurable intervals:

```bash
slb watch --poll-interval 5s
```

Both transports start with the selected project's unresolved requests and
completions from the last 15 minutes. A lost daemon connection emits
`watch_degraded` before polling resumes from the current database state.
Previously observed active requests remain tracked through their final state,
including an outcome older than the normal window after a long disconnect.
Old terminal requests then retire from the live watch state. Snapshots larger
than 10,000 requests fail explicitly instead of silently dropping records.

Default local discovery uses the nearest physical project containing `.slb`,
including from a subdirectory or symlink, and pins that project's database for
fallback. An explicit `--db` selects local polling and keeps that database and
the selected project's path spelling throughout the watch. It is never replaced
by a daemon's database. Missing databases produce an error rather than creating
an empty project. When `SLB_HOST` selects a remote daemon, a lost or unavailable
connection returns an error without switching to local records.
`--auto-approve-caution` requires a local database.

Consumers should replace state by request ID and tolerate repeated updates.
Rapid transitions can coalesce: this feed describes current state. For every
journaled lifecycle change and resumable replay, use `slb events --follow` and
save the returned cursors after processing.

### Auto-Approve Mode

For reviewer agents, auto-approve CAUTION tier requests:

```bash
slb watch --session-id <id> --auto-approve-caution
```

## Request Attachments

Requests can include attachments to provide context for reviewers.

### Attachment Types

| Type | Description |
|------|-------------|
| `file` | File contents (base64 encoded) |
| `image` | Screenshots or diagrams (validated dimensions) |
| `command_output` | Output from context-gathering commands |

### Adding Attachments

```bash
# Attach file
slb request "DROP TABLE users" --reason "..." --attach ./schema.sql

# Attach image (screenshot)
slb request "kubectl delete deployment" --reason "..." --attach ./dashboard.png

# Attach command output
slb request "terraform destroy" --reason "..." --attach-cmd "terraform plan -destroy"
```

### Attachment Limits

```toml
[attachments]
max_file_size = 1048576        # 1MB
max_output_size = 102400       # 100KB
max_command_runtime = 10       # seconds
max_image_dimension = 4096     # pixels
```

### Viewing Attachments

```bash
slb show <request-id> --with-attachments
```

## Session Management

Sessions track agent identity and activity for audit and coordination.

### Session Lifecycle

```bash
# Start session (prints session_key once; slb keeps only its digest)
slb session start --agent "GreenLake" --program "claude-code" --model "opus"

# Resume after a crash: present the original session ID and key
slb session resume --agent "GreenLake" --session-id <id> --session-key <key>

# Force resume (ends a session started by another program; still needs the key)
slb session resume --agent "GreenLake" --program "codex-cli" --session-id <id> --session-key <key> --force

# Heartbeat (update last_active for GC)
slb session heartbeat --session-id <id>

# End session gracefully
slb session end --session-id <id>
```

### Session Garbage Collection

Clean up stale sessions from crashed agents:

```bash
# Show what would be cleaned (dry run)
slb session gc --dry-run --threshold 30m

# Clean sessions inactive > 2 hours
slb session gc --threshold 2h --force

# Interactive cleanup (prompts for each)
slb session gc --threshold 1h
```

### Rate Limit Reset

Reset rate limits for a session (admin use):

```bash
slb session reset-limits --session-id <id>
```

## Emergency Override

`emergency-execute` bypasses peer review with acknowledgment and logging. A
same-UID agent can supply that acknowledgment too; it does not authenticate a
human or supply independent authorization. Use the explicit same-host workflow
when a second session should inspect the command before execution.

### Usage

```bash
# Interactive (prompts for confirmation)
slb emergency-execute "rm -rf /tmp/broken" --reason "System emergency: disk full"

# Non-interactive (requires hash acknowledgment)
HASH=$(echo -n "rm -rf /tmp/broken" | sha256sum | cut -d' ' -f1)
slb emergency-execute "rm -rf /tmp/broken" --reason "Emergency" --yes --ack $HASH
```

### Safeguards

1. **Mandatory reason**: Must provide `--reason` explaining the bypass
2. **Hash acknowledgment**: Non-interactive use requires command hash via `--ack`
3. **Extensive logging**: Command, reason, timestamp, and operator identity logged
4. **Rollback capture**: Optional state capture with `--capture-rollback`

### Audit Entry

Emergency executions create a permanent audit record:

```json
{
  "type": "emergency_execute",
  "command": "rm -rf /tmp/broken",
  "command_hash": "abc123...",
  "reason": "System emergency: disk full",
  "operator": "human",
  "timestamp": "2026-01-03T10:30:00Z",
  "exit_code": 0
}
```

## Outcome Tracking

Record execution feedback to improve pattern classification over time.

### Recording Outcomes

After execution, record whether the command caused problems:

```bash
# No problems
slb outcome record <request-id>

# Problems occurred
slb outcome record <request-id> --problems --description "Deleted wrong files"

# With rating and notes
slb outcome record <request-id> --rating 4 --notes "Worked as expected"
```

### Viewing Outcomes

```bash
# List recent outcomes
slb outcome list

# Only problematic executions
slb outcome list --problems-only --limit 50

# Statistics summary
slb outcome stats
```

### Statistics Output

```json
{
  "total_executions": 150,
  "problematic": 3,
  "success_rate": 0.98,
  "by_tier": {
    "critical": {"total": 10, "problems": 1},
    "dangerous": {"total": 50, "problems": 2},
    "caution": {"total": 90, "problems": 0}
  }
}
```

This data enables:
- Identifying patterns that should be upgraded/downgraded
- Detecting agents that frequently cause problems
- Improving justification quality requirements

## TUI Dashboard

The interactive terminal UI gives human reviewers an at-a-glance view of pending requests and agent activity.

### Launching

```bash
slb tui
```

### Layout

```
┌─────────────────────────────────────────────────────────────────────┐
│  SLB Dashboard                                                       │
├─────────────────┬───────────────────────────────────────────────────┤
│  AGENTS         │  PENDING REQUESTS                                  │
│  ───────        │  ────────────────                                  │
│▸ GreenLake      │▸ abc123 CRITICAL rm -rf /etc      BlueLake 2m     │
│  BlueLake       │  def456 DANGEROUS git reset --hard GreenLake 5m   │
│  RedStone       │  ghi789 CAUTION   npm uninstall   RedStone 10m    │
│                 │                                                    │
├─────────────────┴───────────────────────────────────────────────────┤
│  ACTIVITY                                                            │
│  ────────                                                            │
│  10:30:15 GreenLake approved abc123                                  │
│  10:28:42 BlueLake requested def456 (DANGEROUS)                      │
│  10:25:00 RedStone executed xyz999 (exit 0)                          │
└─────────────────────────────────────────────────────────────────────┘
```

### Keyboard Navigation

| Key | Action |
|-----|--------|
| `Tab` | Cycle focus between panels |
| `↑/↓` | Navigate within panel |
| `Enter` | View selected request details |
| `a` | Approve selected request |
| `r` | Reject selected request |
| `p` | Open pattern management |
| `h` | Open history view |
| `q` | Quit |

### Panel Details

**Agents Panel**: Active sessions with last activity time and pending request count.

**Pending Panel**: Requests awaiting approval, sorted by urgency (CRITICAL first).

**Activity Panel**: Real-time feed of approvals, rejections, and executions.

## History & Search

Browse and search the full audit history.

### Full-Text Search

```bash
# Search commands
slb history -q "rm -rf"

# Search with filters
slb history -q "database" --tier critical --status executed
```

### Filtering

```bash
# By status
slb history --status pending|approved|rejected|executed|cancelled

# By tier
slb history --tier critical|dangerous|caution|safe

# By agent
slb history --agent "GreenLake"

# By date
slb history --since 2026-01-01
slb history --since 2026-01-03T10:00:00Z

# Combined
slb history --tier critical --status executed --since 2026-01-01 --limit 100
```

### Detailed View

```bash
# Show full request details
slb show <request-id>

# Include all information
slb show <request-id> --with-reviews --with-execution --with-attachments
```

## Agent Mail Integration

SLB can tell reviewers about requests through MCP Agent Mail. There are two
routes, and there is no CLI command for sending a notification by hand: the
notices follow request lifecycle changes.

### Request lifecycle notifications (daemon, recommended)

Since v0.5.0 the project daemon can deliver committed request, review, status
and execution changes from its request journal to Agent Mail (HTTP MCP) and/or a
JSON webhook. Delivery survives daemon downtime and retries failed sends. It is
opt-in and configured in the project's `.slb/config.toml`:

```toml
[notifications.requests]
enabled = true
agent_mail_url = "http://127.0.0.1:8765/mcp/"
agent_mail_sender = "BlueLake"                     # an existing, registered identity
agent_mail_recipients = ["GreenCastle", "RedStone"] # identities the sender may contact
```

- The Agent Mail project key is the daemon's absolute project path, and the
  thread is `SLB-Requests`.
- SLB does not register identities or change contact policies: the sender and
  recipients must already exist in that project and be allowed to message each
  other.
- `integrations.agent_mail_enabled` does not turn this worker on; only
  `[notifications.requests] enabled` does.
- Restart the project daemon after changing these settings.
- Notices carry request metadata (ID, risk tier, status, review counts, exit
  code), never the command text or justification. Reviewers run
  `slb review <request-id>` for the details.

[docs/REQUEST_NOTIFICATIONS.md](docs/REQUEST_NOTIFICATIONS.md) covers webhooks,
templates, retry and replay semantics. Alerts for commands that a hook blocked
are a separate opt-in route, described in
[docs/BLOCKED_NOTIFICATIONS.md](docs/BLOCKED_NOTIFICATIONS.md).

### Legacy CLI notifications

Without request-journal delivery, SLB sends a best-effort message when a request
is created, approved, rejected or executed:

```toml
[integrations]
agent_mail_enabled = true            # default
agent_mail_thread = "SLB-Reviews"    # default
```

| Event | Importance |
|-------|------------|
| New CRITICAL request | urgent |
| New DANGEROUS request | normal |
| New CAUTION request | low |
| Request approved | normal |
| Request rejected | normal |
| Request executed | low |

These messages are sent with the `mcp-agent-mail` CLI to `SLB-Broadcast` (sender
`SLB-System` unless one is configured), with a two-second timeout. If that CLI is not installed they
are skipped silently, and a failed send never blocks the request. Enabling
`[notifications.requests]` turns this route off, so the journal worker is the
single source of request notices.

## Output Formats

All commands support structured output for programmatic use.

### JSON Mode

```bash
# Global flag
slb --output json pending

# Per-command
slb pending --json
slb history --json
slb session list --json
```

### Output Examples

**Pending requests (JSON)**:
```json
{
  "requests": [
    {
      "id": "abc123",
      "status": "pending",
      "tier": "critical",
      "command": "rm -rf /etc",
      "requestor": "GreenLake",
      "created_at": "2026-01-03T10:00:00Z",
      "approvals": 0,
      "required_approvals": 2
    }
  ],
  "count": 1
}
```

**Session start (JSON)**:
```json
{
  "session_id": "sess_abc123",
  "session_key": "key_xyz789",
  "agent_name": "GreenLake",
  "program": "claude-code",
  "model": "opus",
  "project_path": "/home/user/myproject",
  "started_at": "2026-01-03T10:00:00Z"
}
```

### Exit Codes

| Code | Meaning |
|------|---------|
| 0 | Success |
| 1 | General error |
| 2 | Invalid arguments |
| 3 | Request not found |
| 4 | Permission denied |
| 5 | Timeout |
| 6 | Rate limited |

## Planning & Development

- Design doc: `PLAN_TO_MAKE_SLB.md`
- Agent rules: `AGENTS.md`
- Task tracking: `bd ready` (beads)
- Prioritization: `bv --robot-priority`

## Contributions

> *About Contributions:* Please don't take this the wrong way, but I do not accept outside contributions for any of my projects. I simply don't have the mental bandwidth to review anything, and it's my name on the thing, so I'm responsible for any problems it causes; thus, the risk-reward is highly asymmetric from my perspective. I'd also have to worry about other "stakeholders," which seems unwise for tools I mostly make for myself for free. Feel free to submit issues, and even PRs if you want to illustrate a proposed fix, but know I won't merge them directly. Instead, I'll have Claude or Codex review submissions via `gh` and independently decide whether and how to address them. Bug reports in particular are welcome. Sorry if this offends, but I want to avoid wasted time and hurt feelings. I understand this isn't in sync with the prevailing open-source ethos that seeks community contributions, but it's the only way I can move at this velocity and keep my sanity.

## License

MIT License (with OpenAI/Anthropic Rider) — see [LICENSE](LICENSE) for details.
