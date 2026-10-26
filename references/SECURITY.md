# SLB Security Design — Reference

## Reviewer identity security boundary (issue #30)

The local SQLite backend does **not** authenticate independent reviewers.
All approval submissions now fail with `independent reviewer identity unavailable`.
Execution also refuses any request requiring approvals or a different model,
including previously approved requests. Rejections remain available. Explicit
zero-review CAUTION auto-approval remains a separate policy choice.

Agent names, program labels, model labels, extra session IDs, HMAC keys, and
cross-project database records cannot establish separate principals. The
`trusted_self_approve` list does not bypass this restriction. There is no
environment variable, configuration option, or CLI flag to turn these labels
into trusted identity. Peer-approval examples elsewhere in the documentation
describe the intended workflow, which is currently blocked by this restriction.

The quorum, self-review, different-model, signature and expiry checks are kept
for a future authority. Their tests run them through
`db.SimulateIndependentIdentityForTest`, which works only inside binaries built
by `go test` (`testing.Testing()`, fixed at link time). A test builds the real
CLI and confirms that it still refuses fresh and database-forged approvals.

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

## Table of Contents
- [Defense in Depth](#defense-in-depth)
- [Execution Verification Gates](#execution-verification-gates)
- [Cryptographic Guarantees](#cryptographic-guarantees)
- [Fail-Closed Behavior](#fail-closed-behavior)
- [Emergency Override](#emergency-override)
- [Safety Note](#safety-note)

---

## Defense in Depth (6 Layers)

1. **Pattern-based classification** — Commands categorized by risk tier
2. **Peer review requirement** — Another agent must approve
3. **Command hash binding** — SHA-256 prevents modification
4. **Approval TTL** — Approvals expire after 10-30 minutes
5. **Execution verification gates** — 5 checks before execution
6. **Audit logging** — All actions logged with full context

---

## Execution Verification Gates

Before any command executes, five gates must pass:

| Gate | Check |
|------|-------|
| **1. Status** | Request must be in APPROVED state |
| **2. Expiry** | Approval TTL must not have elapsed |
| **3. Hash** | SHA-256 hash of command must match (tamper detection) |
| **4. Tier** | Risk tier must still match (patterns may have changed) |
| **5. First-Executor** | Atomic claim prevents race conditions |

---

## Cryptographic Guarantees

- **Command binding**: SHA-256 hash verified at execution
- **Review signatures**: HMAC-SHA256 keyed with the session key's digest, so execution can re-verify stored votes. Anything that can read the database can compute them; they detect edits that bypass slb, not who reviewed.
- **Session keys**: Generated per session and shown once. SQLite stores only SHA-256 over a domain label and the key (`session_key_digest`); the old `session_key` column is left empty. Supplied keys are compared with the digest in constant time. File permissions do not isolate processes sharing a UID, and key possession is not proof of independent identity.

---

## Fail-Closed Behavior

- **Daemon unreachable** → Block dangerous commands (hook)
- **Parse error** → Upgrade tier by one level
- **Approval expired** → Require new approval
- **Hash mismatch** → Reject execution

---

## Emergency Override

For true emergencies, humans can bypass with extensive logging:

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
