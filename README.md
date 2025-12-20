# Simultaneous Launch Button (slb)

[![Go Version](https://img.shields.io/badge/go-1.21+-blue.svg)](https://golang.org)
[![License](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)
[![Build Status](https://github.com/Dicklesworthstone/slb/workflows/CI/badge.svg)](https://github.com/Dicklesworthstone/slb/actions)

A cross-platform CLI that implements a **two-person rule** for running potentially destructive commands from AI coding agents.

When an agent wants to run something risky (e.g., `rm -rf`, `git push --force`, `kubectl delete`, `DROP TABLE`), `slb` requires peer review and explicit approval before execution.

## Why This Exists

Coding agents can get tunnel vision, hallucinate, or misunderstand context. A second reviewer (ideally with a different model/tooling) catches mistakes before they become irreversible.

`slb` is built for **multi-agent workflows** where many agent terminals run in parallel and a single bad command could destroy work, data, or infrastructure.

## Key Features

- **Risk-Based Classification**: Commands are automatically classified by risk level
- **Client-Side Execution**: Commands run in YOUR shell environment (inheriting AWS credentials, kubeconfig, virtualenvs, etc.)
- **Command Hash Binding**: Approvals bind to the exact command via SHA-256 hash
- **SQLite Source of Truth**: Project state lives in `.slb/state.db`
- **Agent Mail Integration**: Notify reviewers and track audit trails via MCP Agent Mail
- **TUI Dashboard**: Beautiful terminal UI for human reviewers

## Risk Tiers

| Tier | Approvals | Auto-approve | Examples |
|------|-----------|--------------|----------|
| **CRITICAL** | 2+ | Never | `rm -rf /`, `DROP DATABASE`, `terraform destroy`, `git push --force` |
| **DANGEROUS** | 1 | Never | `rm -rf ./build`, `git reset --hard`, `kubectl delete`, `DROP TABLE` |
| **CAUTION** | 0 | After 30s | `rm file.txt`, `git branch -d`, `npm uninstall` |
| **SAFE** | 0 | Immediately | `rm *.log`, `git stash`, `kubectl delete pod` |

## Quick Start

### Installation

```bash
# One-liner install
curl -fsSL https://raw.githubusercontent.com/Dicklesworthstone/slb/main/scripts/install.sh | bash

# Or with go install
go install github.com/Dicklesworthstone/slb/cmd/slb@latest

# Or build from source
git clone https://github.com/Dicklesworthstone/slb.git
cd slb && make build
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

### Basic Workflow

```bash
# 1. Start a session (as an AI agent)
slb session start --agent "GreenLake" --program "claude-code" --model "opus"
# Returns: session_id and session_key

# 2. Run a dangerous command (blocks until approved)
slb run "rm -rf ./build" --reason "Clean build artifacts before fresh compile" --session-id <id>

# 3. Another agent reviews and approves
slb pending                    # See what's waiting for review
slb review <request-id>        # View full details
slb approve <request-id> --session-id <reviewer-id> --comment "Looks safe"

# 4. Original command executes automatically after approval
```

## Commands Reference

### Session Management

```bash
slb session start --agent <name> --program <prog> --model <model>
slb session end --session-id <id>
slb session resume --agent <name>              # Resume after crash
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
slb approve <request-id> --session-id <id>     # Approve request
slb reject <request-id> --session-id <id> --reason "..."
```

### Execution

```bash
slb execute <request-id>                       # Execute approved request
slb emergency-execute "<cmd>" --reason "..."   # Human override (logged)
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
| `rm -rf /...` | Recursive delete on system paths |
| `DROP DATABASE/SCHEMA` | SQL database destruction |
| `TRUNCATE TABLE` | SQL data destruction |
| `terraform destroy` | Infrastructure destruction |
| `kubectl delete node/namespace/pv/pvc` | Kubernetes critical resources |
| `git push --force` | Force push (not with-lease) |
| `aws terminate-instances` | Cloud resource destruction |
| `dd ... of=/dev/` | Direct disk writes |

### DANGEROUS (1 approval)

| Pattern | Description |
|---------|-------------|
| `rm -rf` | Recursive force delete |
| `git reset --hard` | Discard all changes |
| `git clean -fd` | Remove untracked files |
| `kubectl delete` | Delete Kubernetes resources |
| `terraform destroy -target` | Targeted destroy |
| `DROP TABLE` | SQL table destruction |
| `chmod -R`, `chown -R` | Recursive permission changes |

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

**Key Design Decision**: Client-side execution. The daemon is a NOTARY (verifies approvals) not an executor. Commands execute in the calling process's shell environment to inherit:
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

Resume your existing session instead of starting a new one:

```bash
slb session resume --agent "YourAgent" --create-if-missing
```

### Approval expired

Approvals have a TTL (30min default, 10min for CRITICAL). Re-request if expired:

```bash
slb run "<command>" --reason "..."  # Creates new request
```

### Command hash mismatch

The command was modified after approval. This is a security feature - re-request approval for the modified command.

## Safety Note

`slb` adds friction and peer review for dangerous actions. It does NOT replace:
- Least-privilege credentials
- Environment safeguards
- Proper access controls
- Backup strategies

Use slb as **defense in depth**, not your only protection.

## Planning & Development

- Design doc: `PLAN_TO_MAKE_SLB.md`
- Agent rules: `AGENTS.md`
- Task tracking: `bd ready` (beads)
- Prioritization: `bv --robot-priority`

## Contributions

> *About Contributions:* Please don't take this the wrong way, but I do not accept outside contributions for any of my projects. I simply don't have the mental bandwidth to review anything, and it's my name on the thing, so I'm responsible for any problems it causes; thus, the risk-reward is highly asymmetric from my perspective. I'd also have to worry about other "stakeholders," which seems unwise for tools I mostly make for myself for free. Feel free to submit issues, and even PRs if you want to illustrate a proposed fix, but know I won't merge them directly. Instead, I'll have Claude or Codex review submissions via `gh` and independently decide whether and how to address them. Bug reports in particular are welcome. Sorry if this offends, but I want to avoid wasted time and hurt feelings. I understand this isn't in sync with the prevailing open-source ethos that seeks community contributions, but it's the only way I can move at this velocity and keep my sanity.

## License

MIT License - See [LICENSE](LICENSE) for details.
