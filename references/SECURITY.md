# SLB Security Design — Reference

## Reviewer identity security boundary (issue #30)

The local SQLite backend does **not** authenticate independent reviewers.
The default `general.approval_policy = "independent"` refuses local approval
submissions with `independent reviewer identity unavailable`. Execution also
refuses requests under that policy requiring approvals or a different model,
including previously approved requests. Rejections remain available. Explicit
zero-review CAUTION auto-approval remains a separate policy choice.

Agent names, program labels, model labels, extra session IDs, HMAC keys, and
cross-project database records cannot establish separate principals. The
`trusted_self_approve` list does not bypass this restriction. There is no
environment variable, configuration option, or CLI flag to turn these labels
into independently authenticated identity. The separate `same_host` policy
described below enables local workflow reviews with explicitly lower assurance;
it does not change the meaning of `independent` authorization.

The quorum, self-review, different-model, signature and expiry checks apply to
same-host reviews. Tests of a future independent authority can still use
`db.SimulateIndependentIdentityForTest`, which works only inside binaries built
by `go test` (`testing.Testing()`, fixed at link time). Integration tests build
the real CLI and verify both intentional same-host review and continued refusal
of local evidence under the default policy. The test hook cannot enable
independent approvals in that CLI.

An external authenticated reviewer service is **not implemented** in this
backend. Restoring independent approvals requires a separately protected
authority that authenticates requester and reviewer principals, attests model
identities, and binds every decision to the exact project, request, command,
policy and expiry. Quorum must count distinct principals rather than names or
sessions; self-review must compare those principals. Requester-controlled
configuration must not be able to replace the authority's trust anchor.

The protected authority must also own the execution boundary and effective
policy. Processes sharing a UID can read/write the database, steal credentials,
replace the binary or hooks, alter policy, or run commands directly. Local
checks cannot constrain a malicious process that has those permissions. Use
separate OS accounts with protected resources, or a separately privileged
broker/remote executor, before treating agents as mutually untrusted. Merely
putting each agent in another directory or assigning another name is insufficient.

Existing-session recovery now requires the original session ID and key before
heartbeat, model changes, or forced replacement. This preserves credential-based
recovery where the credentials are protected; it provides no isolation from
another process that can read those credentials. Lost credentials cannot be
recovered through an agent-name lookup. Ending/recreating a session does not
establish a new principal.

Session keys are stored only as digests (schema versions 5 and 6), so the
database, its copies and its backups no longer contain a key that slb accepts.
The upgrade converts plaintext keys in place, re-signs the votes whose
signatures still verify, zeroes the freed space (`secure_delete`), rebuilds the
file with `VACUUM` (rewriting rows does not reach old bytes on pages SQLite
leaves alone, such as a table's root page once it holds more than one page of
rows) and truncates the write-ahead log. Copies made before the upgrade, and
filesystem blocks the old file used, still hold the old keys: end those
sessions and start new ones if such a copy may have leaked. An older slb binary
that writes the database later stores a plaintext key again; a current binary
converts it on its next open and rebuilds the file if it can.
None of this changes the same-UID limits above. A process running as the same
user can read another agent's key wherever that agent keeps it (environment,
files, terminal scrollback), and can write the database or run commands
directly, so hashing is not isolation between agents.

## Optional same-host peer review (issue #31)

Several Claude Code or Codex sessions running as one Linux user can use SLB to
catch accidental mistakes: one explains a command, another inspects the exact
command and expected effects, and the requester executes after review. Enable
this workflow explicitly, before submitting the request:

```bash
slb config set general.approval_policy same_host
```

Equivalent project configuration:

```toml
[general]
approval_policy = "same_host"
```

The only policies are `independent` (the default) and `same_host`. An unknown
value is a configuration error. The CLI labels same-host operations as
**same-host peer review (low assurance; not independently authenticated
authorization)**. Session IDs, agent names and different-model checks remain
coordination labels. Cooperating reviewers must still use another active
session and a different agent name, provide its session key, meet quorum,
provide a different model label when required, and review before expiry.
`trusted_self_approve` never permits self-review in this mode.

When different-model review is required, both requester and reviewer must supply
nonblank model labels. Surrounding whitespace and letter case do not make the
same label different. Submission, reviewer availability and execution-time
verification use this comparison; the original labels remain in signed evidence.
Missing historical labels cannot satisfy the requirement, and these labels still
do not authenticate which model actually performed the review.

Same-host review catches mistakes only. Any process sharing the OS user can
enable this option, change its claimed agent/model, read signing material,
write the database, replace SLB or its hook, or execute the command directly.
These are outside this workflow's protection. A local HMAC and audit history
are not evidence that an independent person or model approved the action.

### Policy binding and historical evidence

- The request stores its `approval_policy` at creation. Approving the request and
  executing it require the currently configured policy to match it. Changing
  configuration never converts an existing request into another policy.
- Each review stores `approval_policy`, `assurance`, `signature_version`, the
  command hash and a binding to the reviewed request. Version 2 same-host
  signatures use their own domain and cover this binding, so copying a vote to
  another command or relabeling it cannot reuse its signature.
- Execution rechecks signatures, request binding, quorum, different-model
  restrictions and expiry while atomically claiming the request. The execution
  record stores its own policy and assurance. Request/review/execution audit
  events preserve these distinctions; an `approved` status alone is insufficient.
- Historical requests without a policy remain `independent`. Historical local
  reviews remain readable as `legacy_local` evidence using their original
  signature format. They cannot satisfy same-host review and cannot authorize
  execution under the independent policy. Submit a new request for fresh review
  after opting in; old approvals are never silently upgraded or reinterpreted.
- Returning configuration to `independent` prevents further same-host approval
  and execution, including outstanding approved same-host requests. It does not
  turn their reviews into independent authorization.
- Approval submission reloads the current opt-in after acquiring the database
  writer reservation, including for a long-lived review service. An open TUI
  form is bound to the policy it displayed. Rejection remains available after
  opting out. Configuration files are freshly read; they are not part of the
  SQLite transaction and this is not protection against a concurrent malicious
  process changing them.
- Tightening conflict resolution from `first_wins` to
  `any_rejection_blocks` or `human_breaks_tie` also prevents execution of an
  outstanding approval that contains a rejection. The execution gate rechecks
  the current conflict policy; an earlier approved status cannot waive it.
  This also applies when a SAFE rule now matches the command or a native Git
  hook consumes the approval.
- Background preflight refusals retain the request's policy label without
  claiming execution assurance. A refused replay does not borrow assurance or
  success from the previous execution. Outcome reports use the policy and
  assurance captured in the execution receipt. The standalone Python hook
  reports its generated policy snapshot or the daemon's live policy explicitly;
  neither hook response is execution authorization.
- An authenticated completion report can be retried after a lost reply. The
  current session key, owning session, claim receipt and exact terminal outcome
  must still match, including missing versus zero exit codes and durations. An
  identical retry returns the original committed result, timestamp, policy and
  assurance without updating the terminal record or adding another audit event.
  It returns no permission to execute again. A later configuration change cannot
  relabel that historical claim; conflicting reports remain errors. This receipt
  recovery does not reconcile an executor that disappeared before reporting.
- Hooks installed with `--config` retain the selected absolute filename and its
  owning physical project. The daemon must acknowledge the same selected file;
  the query cannot replace the daemon's policy. Offline native classification
  and the final executor reload that filename, preserving the hook's selected
  alias when the daemon uses another equivalent path. Replacing that alias after
  lookup therefore cannot silently preserve an old opt-in. The Python artifact delegates explicit
  policy bindings to the native guard so an old embedded snapshot cannot restore
  a removed opt-in. Missing or invalid selected policy fails closed, and separately
  initialized nested projects do not inherit the parent's explicit override.
- Escalated requests remain visible in reviewer queues and retain their original
  review requirements. The requester may cancel them before execution. Ownership
  and status are checked in the same writer transaction, so cancellation cannot
  replace an execution claim or terminal result. Cancellation retains prior signed
  reviews and records the original policy and review counts in the event journal;
  it supplies no execution assurance.

These checks prevent accidental evidence reuse through normal SLB operations.
They do not make mutable local storage tamper-proof against the same OS user.

### Project scope and delegated review

Ordinary approvals and rejections require the reviewer's recorded project path
to match the request's, even when projects share a database through `--db`.
Cross-project review uses the explicit `--target-project` delegation workflow;
the target opts in with `general.cross_project_reviews` and restricts reviewer
agent names with `general.review_pool`. Delegation records ended, target-scoped
reviewer provenance rather than creating another active local reviewer session.

The target's current opt-in and reviewer pool are checked when committing a
delegated approval and again before execution. Disabling cross-project review
or removing a reviewer from the pool blocks outstanding delegated approvals
under the current policy. Restoring permission can allow still-valid, unexpired
evidence again; it never raises that evidence's assurance. Changing a provenance
record's program label or making its reserved delegation session active cannot
turn delegated evidence into local approval.

### Context command evidence

`--attach-context` executes before the main command receives review, so each
context command needs its own fresh SAFE-policy check for the active requester,
project, selected configuration and working directory. The entire context batch
and all file/image attachments are checked before the first subprocess; policy
and session checks repeat before each launch. Admission reloads configuration
after collection. Unknown or review-required contexts must use the ordinary
reviewed execution path first, then attach saved output. A context attachment
records its command hash, directory and SAFE check without granting peer-review
assurance. SAFE configuration is operator policy, not process isolation.

## Table of Contents
- [Reviewer Identity Security Boundary](#reviewer-identity-security-boundary-issue-30)
- [Optional Same-Host Peer Review](#optional-same-host-peer-review-issue-31)
- [Workflow Checks](#workflow-checks)
- [Execution Verification Gates](#execution-verification-gates)
- [Cryptographic Guarantees](#cryptographic-guarantees)
- [Fail-Closed Behavior](#fail-closed-behavior)
- [Emergency Override](#emergency-override)
- [Safety Note](#safety-note)

---

## Workflow Checks

1. **Pattern-based classification** — Commands categorized by risk tier
2. **Review policy** — Default independent authorization or explicit same-host peer review
3. **Command and policy binding** — Execution verifies the signed review still describes the request
4. **Approval TTL** — Approvals expire after 10-30 minutes
5. **Execution verification gates** — Recheck evidence and current policy before claiming execution
6. **Audit logging** — All actions logged with full context

---

## Execution Verification Gates

For execution of a reviewed request, these checks must pass:

| Gate | Check |
|------|-------|
| **Policy** | Stored approval policy must match current configuration; independent approvals require external identity |
| **Status** | Request must be in APPROVED state |
| **Evidence** | Review assurance, signature format, request binding, quorum and model requirements must verify |
| **Expiry** | Approval TTL must not have elapsed |
| **Command** | Raw command, arguments, working directory and shell mode must match the reviewed command hash |
| **Tier** | Current patterns must not require a higher tier or stricter review requirements |
| **First executor** | Atomic claim prevents multiple executions of the request |

---

## Cryptographic Guarantees

- **Command binding**: SHA-256 hash verified at execution
- **Review signatures**: HMAC-SHA256 keyed with the session key's digest, so execution can re-verify stored votes. Same-host signatures bind the exact command and review policy with a distinct format and domain. Anything that can read the database can compute them; they detect accidental mutation and evidence reuse, not who reviewed.
- **Session keys**: Generated per session and shown once. SQLite stores only SHA-256 over a domain label and the key (`session_key_digest`); the old `session_key` column is left empty. Supplied keys are compared with the digest in constant time. File permissions do not isolate processes sharing a UID, and key possession is not proof of independent identity.

---

## Fail-Closed Behavior

- **Daemon unreachable** → Block dangerous commands (hook)
- **Parse error** → Upgrade tier by one level
- **Approval expired** → Submit a new request for review
- **Hash mismatch** → Reject execution
- **Policy or assurance mismatch** → Reject review/execution; submit a new request under the intended policy

The installed native Claude guard loads current project policy and uses the
shell normalizer when the daemon is unavailable. The standalone generated Python
guard instead contains a policy snapshot. It only automatically allows SAFE
matches for literal simple commands: a safe prefix cannot cover shell operators,
expansion, redirection, quoting, or an execution wrapper's hidden payload. Known
risks retain their policy checks; forms the snapshot cannot classify require an
explicit decision. Confirmation is not a reviewed approval. Use the native guard
for full shell analysis and the reviewed execution handoff.

---

## Emergency Override

For emergencies, `emergency-execute` bypasses peer review with explicit
acknowledgment and a separate execution ledger. The acknowledgment does not
authenticate a human; a same-UID agent can supply it too. The actor field is a
local label. This is an **unreviewed override**, with fixed assurance
`emergency_unreviewed`; neither `independent` nor `same_host` reviews authorized
it. Creating an override creates no request, approval vote, or synthetic session.

Structured output and log records retain `execution_mode = "emergency"` and
`assurance = "emergency_unreviewed"` under either configured review policy.
The `review_notice` states that there was no peer approval or independently
authenticated authorization. The ledger stores `configured_approval_policy`
only as historical context. Output preserves that value as `approval_policy`,
accompanied by `approval_policy_bypassed = true` and a `BYPASSED` policy label;
it does not mean the policy was satisfied.

```bash
# Interactive (prompts for confirmation)
slb emergency-execute "rm -rf /tmp/broken" --reason "System emergency: disk full"

# Non-interactive (requires hash acknowledgment)
HASH=$(printf '%s' "rm -rf /tmp/broken" | sha256sum | cut -d' ' -f1)
slb emergency-execute "rm -rf /tmp/broken" --reason "Emergency" --yes --ack "$HASH"

# Inspect the distinct unreviewed evidence
slb audit emergency --id <emergency-id> --json
slb audit emergency --status incomplete --json
```

### Acknowledgment and command binding

For existing workflows, `--ack` continues accepting SHA-256 of the raw command
(a full digest or a matching prefix of at least eight characters). That hash
does not include the working directory. The emergency record separately stores
the canonical `CommandSpec` hash, which includes the execution working directory
and shell mode as well as the command. JSON output retains the raw digest as
`hash` and exposes the canonical binding as `command_hash`.

Both hashes describe the explicit override. Neither establishes reviewer identity
or supplies transferable authorization. An emergency record cannot satisfy review
quorum, and its assurance cannot be promoted by changing configuration. The command
still inherits the caller's environment; a stored command hash does not bind all
external files, credentials, tools, or services that the command may use.

### Intent, process start and terminal evidence

SLB commits the unreviewed intent to the selected project database and flushes a
uniquely created private log before starting the override command. Failure to
record the intent or log preamble prevents launch. The log is created with mode
`0600` and is never selected solely by a timestamp, so concurrent overrides do
not truncate each other's evidence. Its first structured event remains
`emergency_execution` with `phase = "attempt"`; a final
`emergency_execution_outcome` carries the observed outcome and durable ledger
status. The `attempted_at` output alias describes the intent, while `executed_at`
describes a recorded child start and is absent when startup failed.

After the operating system starts the child, SLB records its PID and start time.
If that write fails, the shared process runner cancels and reaps the child and
attempts to record the observed failure. The child may already have changed
external state before its startup write failed. On completion, the ledger records
`succeeded`, `failed`, `timed_out`, or `cancelled`, including the actual exit status
when one was observed. Success requires a recorded startup and zero exit; a
command that never started does not acquire a fabricated zero exit.

A crash or failed terminal write can leave status `incomplete`. This means the
outcome is unknown. It does not prove that the child never started, remains
running, or is safe to retry. The absence of a stored PID likewise does not prove
that no process ran. Inspect the log, process state and actual command effects
before deciding how to recover; SLB never automatically retries an override.
The CLI returns an error for failed persistence and retains any observed child
exit/output in its response without relabeling an incomplete ledger as success.

The ledger is local SQLite evidence with the existing database's durability
settings. It is not a protected external audit service, tamper-proof storage, or
a stronger security boundary. Processes sharing its OS identity can alter it.
Mandatory reasons, acknowledgment, bounded execution, optional rollback capture
and explicit evidence labels support deliberate operation and investigation;
they do not replace peer review or independently authenticated authorization.

---

## Safety Note

SLB adds friction and peer review for dangerous actions. It does NOT replace:
- Least-privilege credentials
- Environment safeguards
- Proper access controls
- Backup strategies

Use SLB as **defense in depth**, not your only protection.

---

## Flywheel Integration

| Tool | Integration |
|------|-------------|
| **Agent Mail** | Notify reviewers via inbox; track audit trails |
| **BV** | Track SLB requests as beads |
| **CASS** | Search past SLB decisions across sessions |
| **DCG** | DCG blocks automatically; SLB adds peer review layer |
| **NTM** | Coordinate review across agent terminals |
