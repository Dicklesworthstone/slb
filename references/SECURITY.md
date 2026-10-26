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
recovered through an agent-name lookup. Database copies and backups remain
sensitive. Ending/recreating a session does not establish a new principal.

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
- **Review signatures**: HMAC using session keys
- **Session keys**: Generated per-session and stored in plaintext in SQLite for HMAC verification. File permissions do not isolate processes sharing a UID. HMAC possession is not proof of independent identity.

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
