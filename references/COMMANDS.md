# SLB Commands — Complete Reference

## Table of Contents
- [Session Management](#session-management)
- [Request & Run](#request--run)
- [Review & Approve](#review--approve)
- [Execution](#execution)
- [Pattern Management](#pattern-management)
- [Daemon & TUI](#daemon--tui)
- [Claude Code Hook](#claude-code-hook)
- [History & Audit](#history--audit)
- [Exit Codes](#exit-codes)

---

## Session Management

```bash
slb session start --agent <name> --program <prog> --model <model>
slb session end --session-id <id>
slb session resume --agent <name> --session-id <id> --session-key <key>  # Resume after crash
slb session resume --agent <name> --session-id <id> --session-key <key> --model <new-model>
slb session resume --agent <name> --session-id <id> --session-key <key> --program <new-prog> --force
slb session list                               # Show active sessions
slb session heartbeat --session-id <id>        # Refresh activity timestamp
slb session gc --threshold 2h                  # Clean stale sessions
```

An active session resumes with the same ID/key when program and model are omitted
or unchanged. A changed model or permitted program change atomically ends the old
session and returns a fresh ID/key; omitted fields keep their current values.
Changing an established program requires `--force`. Always adopt the returned
credentials. The old identity and signing-key digest are retained so existing
reviews keep their original model, signature, and policy.

Rotation refuses sessions that own `pending`, `approved`, or `escalated` requests.
Finish that work with the original session or cancel eligible requests first;
`--force` does not transfer ownership or bypass this check. Commands already
executing can complete under the original identity. Rotating does not allow an
agent to add another quorum vote or self-review.

`session gc` rechecks each candidate's activity against its cutoff before ending
it, so a refreshed session is skipped. Historical session and review records are
retained.

---

## Request & Run

Commands discover the nearest initialized project from `-C` / `--project`,
`SLB_PROJECT`, or the current directory, in that order. Requests from descendants
use the root project's sessions, policy, custom patterns, and default database.
`-C` changes cwd first; `SLB_PROJECT` changes only project selection. The exact
command cwd remains part of the request binding and is used at execution.

An explicit `--db` remains authoritative. Invalid or inaccessible project markers
and missing initialized policy databases cause an error; SLB does not silently
select home history or create a child project. `slb init` operates on its selected
directory directly, so an intentionally initialized nested project remains separate.

```bash
# Primary command (atomic: check, request, wait, execute)
slb run "<command>" --reason "..." --session-id <id>

# Plumbing commands
slb request "<command>" --reason "..."         # Create request only
slb status <request-id> --wait                 # Check/wait for status
slb pending --all-projects                     # List pending and escalated requests
slb cancel <request-id> --session-id <id>       # Withdraw own unexecuted request
```

`pending` and `review list` show requests whose status is `pending` or `escalated`.
Their project and `--review-pool` filters remain in force, and output identifies
the status and approval policy. Eligible peers can complete review after escalation;
the original quorum, model restrictions and command binding still apply.

The requester can cancel `pending`, `approved` or `escalated` work. Cancellation
checks ownership and current state atomically, preserves existing signed reviews,
and records the transition in the request journal. Once execution has claimed the
request, cancellation is refused. Resolving or withdrawing an escalated request
also removes that request's obstacle to session identity rotation.

---

## Review & Approve

Local peer review requires an explicit opt-in before creating the request:

```bash
slb config set general.approval_policy same_host
```

This is a low-assurance mistake-prevention workflow, not independently
authenticated authorization. The default `independent` policy refuses local
approvals. Existing requests keep their policy; create a new request after
changing it. See [Approval Policy](CONFIG.md#approval-policy).

```bash
slb review <request-id>                        # Show full details
slb approve <request-id> --session-id <id> --session-key <key> --comments "..."
slb reject <request-id> --session-id <id> --reason "..."
```

The reviewer must be a different agent/session and supply its session key.
Same-host request, review and execution output retain the policy and assurance
labels. Quorum, model-difference checks, signatures and expiration still apply.

---

## Execution

Default execution logs are stored in the request's project under `.slb/logs/`,
including detached execution. Emergency logs use the selected project's same
directory and retain their separate `emergency_unreviewed` evidence. Explicit
`--log-dir` overrides are resolved from the invoking cwd. Discovering a parent
project does not relocate the command itself.

```bash
slb execute <request-id> --session-id <id>    # Execute after rechecking policy and evidence
slb emergency-execute "<cmd>" --reason "..."   # Unreviewed override (logged)
slb rollback <request-id>                      # Rollback if captured
slb rollback <request-id> --force              # Force overwrite
```

### Emergency execution

`emergency-execute` is an explicit **unreviewed override**. It requires a reason
and either an interactive `EXECUTE` confirmation or `--yes --ack`. Neither
confirmation authenticates a human. It supplies no peer review and cannot
create authorization evidence for the `independent` or `same_host` policies.

```bash
# Review the command, working directory and reason interactively.
slb emergency-execute "printf 'recovery check\n'" --reason "Diagnose recovery environment"

# Non-interactive acknowledgment retains the raw-command SHA-256 format.
COMMAND="printf 'recovery check\n'"
HASH=$(printf '%s' "$COMMAND" | sha256sum | cut -d' ' -f1)
slb emergency-execute "$COMMAND" --reason "Diagnose recovery environment" \
  --yes --ack "$HASH" --timeout 60 --json
```

The raw acknowledgment hash can be provided in full or as its first eight or
more characters. The JSON `hash` retains this raw-command value. The separate
`command_hash` binds the command specification, including its working directory
and shell mode. This binding is recorded for audit; it is not an approval token.

Before launching the command, SLB commits a separate emergency execution intent
and flushes a uniquely created private log. It records an actual child PID after
startup and the observed outcome on completion. Output and log records use
`execution_mode: "emergency"`; all emergency records retain the fixed
`assurance: "emergency_unreviewed"`. The ledger's `configured_approval_policy`
records context only. Output retains `approval_policy` for that context, with
`approval_policy_bypassed: true` and an explicit `BYPASSED` policy label. The
configured policy did not authorize this command.

Structured output includes `emergency_id`, `status`, `executed`, `exit_code`,
`output`, `log_path`, both hashes, the local actor label, reason and observed
timestamps. `attempted_at` aliases the recorded intent time; `executed_at`
aliases an observed, recorded process start and is absent for a failed start.
Each log begins with an `emergency_execution` event and `phase: "attempt"`,
then appends an `emergency_execution_outcome` after the process attempt.
A command that exits nonzero also makes SLB exit nonzero with the
child's exit code. A command that never starts has `executed: false` and
`exit_code: -1`; its ledger has no invented child exit status. `--timeout`
bounds preparation and execution; the default is 300 seconds. Zero, negative
and overflowing values are rejected. Caller cancellation cancels and reaps the child.

`--capture-rollback` requests capture when enabled by configuration. A capture
error stops the override; an unsupported command type warns that no snapshot is
available. A returned `rollback_path` identifies the captured artifact. An
emergency ID is not a reviewed request ID and cannot be passed to request-based
`slb rollback`. Git snapshots support the explicit `rollback --from <path>
--force` recovery mode.

---

## Pattern Management

```bash
slb patterns list --tier critical              # List patterns by tier
slb patterns test "<command>"                  # Check what tier a command gets
slb patterns add --tier dangerous "<pattern>"  # Add runtime pattern
```

---

## Daemon & TUI

```bash
slb daemon start                               # Start background daemon
slb daemon start --foreground                  # Run daemon in this process
slb -C /path/to/project --config /path/to/policy.toml daemon start
slb daemon stop                                # Stop daemon
slb daemon status                              # Check daemon status
slb tui                                        # Launch interactive TUI
slb watch --session-id <id> --json             # Stream events (NDJSON)
slb watch --session-id <id> --auto-approve-caution  # Auto-approve CAUTION tier
```

The daemon retains an explicitly selected configuration when it starts its
background process. As with other commands, relative config paths resolve from
the directory selected by `-C`. Pass the same
`--config` file to requester and reviewer commands when using an explicit policy.
Restart the daemon with that file after changing notification settings; another
command's `--config` does not change an already running daemon.

---

## Claude Code Hook

```bash
slb init                                       # Initialize project policy first
slb hook install                               # Install project PreToolUse hook
slb -C /path/to/project init                    # Initialize a different project
slb -C /path/to/project hook install            # Select the project explicitly
slb -C /path/to/project --config /path/to/policy.toml hook install
slb hook install --global                      # Install in user settings
slb hook status [--global]                      # Inspect the selected scope
slb hook test "<command>"                      # Test classification, no execution
slb hook uninstall [--global]                   # Remove only that registration
slb hook install --force                       # Reset only SLB's hook options

# Generate IDE integrations
slb integrations claude-hooks --preview         # Current PreToolUse settings fragment
slb integrations claude-hooks --install [--global]  # Same scoped hook installer
slb integrations cursor-rules > .cursorrules
```

The default scope is the nearest initialized project discovered from `-C` /
`--project`, `SLB_PROJECT`, or the current directory, in that order. Its registration is
`<project>/.claude/settings.json`; `--global` selects `~/.claude/settings.json`.
The standalone fallback script lives under that scope's `.slb/hooks/` directory.
Status reports the selected scope only. Existing user installations from older
SLB versions can be inspected or removed with `--global`.

Project installation requires a readable initialized `<project>/.slb/state.db`;
run `slb init` in a new project first. A failed prerequisite check changes no
settings or hook artifacts. Global installation does not require project initialization.

Both installation entry points preserve unrelated settings and neighboring
hooks. `--force` and `--merge=false` reset only SLB's own hook object; uninstall
keeps the script and the other scope's registration. Preview changes no files.
The native registration uses `onFailure: "block"`, which requires Claude Code
v2.1.295 or later for hook startup failures and timeouts to block the action.
See [Claude Code's hook failure behavior](https://code.claude.com/docs/en/hooks#block-the-action-when-a-hook-fails).

An explicit `--config` is retained as an absolute path bound to the installation's
physical project. Use the same file when starting the daemon and submitting or
reviewing requests. The guard uses it offline and includes it in the execution
handoff. An unavailable or malformed selected file blocks the action. A running
daemon with a different selected policy must be restarted; queries cannot override
its configuration. A separately initialized nested project keeps its own policy,
including when the registration is global.

The generated Python artifact retains this explicit binding and invokes the native
guard to read current policy; a missing native guard blocks the action. Without an
explicit file, the generated artifact can still classify from its embedded snapshot.
Every reviewed execution remains single-use and rechecks policy, signatures,
command binding, quorum and expiry when it claims the request.

---

## History & Audit

```bash
slb history --tier critical --status executed  # Filter history
slb history -q "rm -rf"                        # Full-text search
slb history sync --limit 100 --timeout 30s --json # Export request-event metadata to local Git
slb show <request-id> --with-reviews           # Detailed view
slb outcome record <request-id> --problems     # Record feedback
slb outcome stats                              # Execution statistics

# Separate unreviewed overrides, scoped to the selected project/database
slb audit emergency --json
slb audit emergency --id <emergency-id> --json
slb audit emergency --status incomplete --json
slb audit emergency --since 2026-10-01 --jsonl
```

`audit emergency` reads the separate emergency ledger, not reviewed request
history or the hook's blocked-command JSONL directory. It supports `--id`,
`--status`, `--actor`, `--query`, `--since`, `--before` and `--limit`. Time filters
use the recorded intent time; `--since` is inclusive and `--before` is exclusive.
Use `--project` and, when needed, `--db` to select the store. The blocked-hook
`audit --directory` option does not select an emergency ledger.

| Emergency status | Meaning |
|------------------|---------|
| `incomplete` | No terminal outcome was recorded. The child may have started, finished, or still be running. |
| `succeeded` | A child start and zero exit were durably recorded. |
| `failed` | Execution or its required startup/evidence recording failed; inspect the error and observed exit. |
| `timed_out` | The execution attempt exceeded its deadline. |
| `cancelled` | The attempt was cancelled. |

An `incomplete` record is **not permission to retry**. A crash can occur between
process start and its startup record, and a terminal write can fail after the
command changes external state. Inspect the command log, recorded PID and actual
system state before deciding what to do. Even a failed or cancelled command can
have performed part of its work. SLB returns an error when terminal persistence
fails while preserving the observed child outcome in that invocation's output.

### Local Git export

`history sync` exports one page from the selected project's immutable request
journal. Configure `history.git_repo_path` first; relative destinations resolve
against the owning project root. The command accepts `--limit` from 1 to 1000
(default 100) and a positive `--timeout` (default `30s`). It honors `--project`,
`--db` and `--config`, requires an existing source database, and works without a
daemon or with `history.auto_git_commit=false`.

Output contains `repository`, `ref`, `commit`, `events`, `last_sequence`, `cursor`
and `has_more`. Repeat while `has_more` is true. Each project's dedicated
`refs/slb/history/<project-hash>` ref contains event JSON and a checkpoint committed
together. No-op retries leave the commit unchanged. A checkpoint from a different
journal, or a corrupt checkpoint, is refused without silently resetting it.

With a configured path and `history.auto_git_commit=true`, the project daemon
exports new events and replays missed events after a restart. Use the same explicit
configuration when starting the daemon; restart it after changing export settings.
The default path is empty, so automatic Git export is off by default. Failed
exports are retried without changing request, review or execution outcomes.

This copies policy/assurance and status/outcome metadata, excluding raw commands,
free-form evidence, attachments, keys, signatures and execution receipts. It does
not include the separate emergency or blocked-hook ledgers. SQLite remains the
authoritative store; Git history grants no execution permission. SLB leaves the
user's HEAD, index, worktree and author configuration unchanged and never pushes
to a remote. See [configuration and recovery details](../README.md#local-git-journal-export).

---

## Request Attachments

```bash
# Attach file
slb request "DROP TABLE users" --session-id "$REQUESTER_ID" \
  --reason "..." --attach-file ./schema.sql

# Attach screenshot
slb request "kubectl delete deployment nginx" --session-id "$REQUESTER_ID" \
  --reason "..." --attach-screenshot ./dashboard.png

# Attach command output only if the selected policy explicitly marks pwd SAFE
slb request "terraform destroy" --session-id "$REQUESTER_ID" \
  --reason "..." --attach-context "pwd"
```

These flags also work with `slb run`. Context commands execute before the main
request is reviewed, so they must independently pass a fresh SAFE check for the
active requester, project, working directory and selected `--config`. SLB checks
the entire context batch and validates files/images before launching anything,
then rechecks policy and session state before each subprocess. Unknown commands,
file-writing redirections and commands needing review are refused. Captured
output records its command hash, directory and SAFE-policy check; it grants no
peer approval for the main command. For context that needs review, run it through
`slb run` first and attach its saved output with `--attach-file`.

---

## Exit Codes

| Code | Meaning |
|------|---------|
| `0` | Success |
| `1` | General error |
| `2` | Invalid arguments |
| `3` | Request not found |
| `4` | Permission denied |
| `5` | Timeout |
| `6` | Rate limited |

Execution commands, including `emergency-execute`, preserve an observed child's
nonzero exit code. A missing/invalid child status or an infrastructure failure
returns a general error; the numeric table above is not a substitute for the
structured execution outcome.

---

## Environment Variables

| Variable | Description |
|----------|-------------|
| `SLB_APPROVAL_POLICY` | `independent` (default) or explicit low-assurance `same_host` review |
| `SLB_MIN_APPROVALS` | Minimum approval count |
| `SLB_REQUEST_TIMEOUT` | Request timeout in seconds |
| `SLB_TIMEOUT_ACTION` | What to do on timeout |
| `SLB_DESKTOP_NOTIFICATIONS` | Enable desktop notifications |
| `SLB_WEBHOOK_URL` | Webhook notification URL |
| `SLB_DAEMON_TCP_ADDR` | TCP listen address |
| `SLB_TRUSTED_SELF_APPROVE` | Comma-separated trusted agents |
