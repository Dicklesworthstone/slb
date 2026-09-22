# Supervised background execution

`slb execute --background` launches a detached supervisor for an already approved
request. It does not submit a request, bypass review, or move execution into the
SLB daemon. The supervisor owns its database handle until it records completion.

```sh
slb execute "$REQUEST_ID" --session-id "$SLB_SESSION_ID" \
  --background --timeout 1800 --json

slb status "$REQUEST_ID" --json
slb show "$REQUEST_ID" --json
```

The executor session may also be selected with the root `-s` shorthand or
`SLB_SESSION_ID`. Explicit session flags take precedence. The command inherits
the launching process's environment and uses the request's recorded working
directory, argv and shell mode. Relative database, policy-config and log paths
are resolved before the worker is launched. An explicit `--config` is preserved.

## Startup is not completion

A successful launch reports a receipt with `status=executing`, `request_id`,
`command_hash`, `pid` (the actual command), `supervisor_pid`, `log_path`, and
`supervisor_log`. It deliberately has no final `exit_code` or command output.
The CLI's zero exit status means startup was acknowledged, **not** that the
command completed successfully. A fast command may already be terminal when
you read this receipt. Query the request for its current status and execution
metadata; an old receipt is not current authorization or proof of success.

Before acknowledgment, the worker reloads the request, current configuration,
custom patterns, session and signed approval evidence. It uses the same atomic
single-winner claim as foreground execution. The observed command hash is bound
into the handoff even when `--expected-command-hash` is not supplied. Tightened
policy, expired/invalid approval, another winner, or a changed command cannot
borrow the parent's earlier view of the request.

Only a successful process start after the committed claim produces a receipt.
The command PID is synced to its execution log before acknowledgment. The
supervisor also records startup identity in its diagnostic log, so a lost reply
can be investigated. Requests rejected before claiming remain unconsumed. A
process that fails to start after claiming is recorded as `execution_failed`.

The startup budget is one minute, including rollback capture and approval
checks. `--timeout` separately bounds command execution after preflight; its
default is 300 seconds, and zero selects that default. Negative or overflowing
timeouts are rejected. Slow preflight may require foreground execution.

## Process lifetime and output

On supported Unix platforms the worker is started in a new OS session, without
an inherited controlling terminal. Standard input is `/dev/null`, not the
launcher's stdin or startup pipe. Interactive commands must use foreground mode.
The worker and command retain the caller's environment, including credentials;
no credential snapshot is serialized into the startup job.

Command stdout/stderr go to the existing private execution log, not the launch
JSON or original terminal. In-memory output retains the existing bounded
capture behavior. The supervisor's separate `background-supervisor-*.log`
contains startup/completion diagnostics. Both new files are created with mode
0600. Execution logs retain the existing raw-command/output behavior and can
contain secrets; treat them as sensitive files.

After startup acknowledgment the launching CLI may exit and its context may be
canceled without canceling accepted work. The worker enforces the execution
timeout, reaps the command and records `executed`, `execution_failed`, or
`timed_out`, including the available exit code and duration. Existing request
journal consumers observe these committed transitions. A running daemon is not
required. Ordinary process-group cancellation applies to noninteractive
children, but deliberately escaping descendants are not contained.

## Ambiguous startup and crashes

A failed, lost or malformed acknowledgment reports `startup_unconfirmed`, with
the supervisor PID and diagnostic log where available. The launcher asks the
worker to stop and gives it a bounded opportunity to persist its outcome. The
command may already have had effects before the acknowledgment was lost.
**Never fall back to running the raw command or restore the approval.** Inspect
request status and logs first. Reattempting `slb execute` cannot reuse a claimed
approval; the atomic gate remains authoritative.

SIGKILL, machine loss or an unrecoverable database write can leave a request in
`executing`. The implementation does not invent a terminal outcome, automatically
retry side effects, survive reboot, or reset a consumed approval. PIDs in a
receipt are diagnostic observations, not durable process identities; they can
be reused after exit. This is not a persistent job scheduler or a new remote
cancellation API.

Native Git authorization tokens still must be consumed by their native hooks;
background execution does not turn them into runnable Git operations. Platforms
without the supported detached-session/pipe transport, including Windows,
reject `--background` explicitly instead of silently running in foreground.
