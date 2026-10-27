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
- Tightening conflict resolution from `first_wins` to
  `any_rejection_blocks` or `human_breaks_tie` also prevents execution of an
  outstanding approval that contains a rejection. The execution gate rechecks
  the current conflict policy; an earlier approved status cannot waive it.

These checks prevent accidental evidence reuse through normal SLB operations.
They do not make mutable local storage tamper-proof against the same OS user.

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

---

## Emergency Override

For emergencies, `emergency-execute` bypasses peer review with explicit
acknowledgment and logging. The acknowledgment does not authenticate a human;
a same-UID agent can supply it too. It is a separate unreviewed override, not a
replacement for the optional peer-review workflow.

Its structured output and emergency audit records use
`execution_mode = "emergency"` and `assurance = "emergency_unreviewed"`, with a
`review_notice` stating that peer review was bypassed. These labels are the same
under both approval policies: enabling `same_host` cannot turn emergency
execution into reviewed execution.

```bash
# Interactive (prompts for confirmation)
slb emergency-execute "rm -rf /tmp/broken" --reason "System emergency: disk full"

# Non-interactive (requires hash acknowledgment)
HASH=$(echo -n "rm -rf /tmp/broken" | sha256sum | cut -d' ' -f1)
slb emergency-execute "rm -rf /tmp/broken" --reason "Emergency" --yes --ack $HASH
```

**Safeguards**: Mandatory reason, hash acknowledgment, extensive logging, optional rollback capture.

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
