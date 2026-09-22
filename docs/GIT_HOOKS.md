# Native Git approval hooks

SLB's native `pre-commit` and `pre-push` hooks extend review to Git invocations
from terminals, scripts and Git clients, not only Claude Code tool calls.
They inspect Git's actual staged tree or proposed ref updates rather than
trying to recover the original command line.

## Setup

Build/install the updated SLB binary and ensure Git can find `slb` on `PATH`.
Initialize SLB in each worktree whose operations will need review:

```sh
slb init
slb session start --agent GitAuthor --program terminal --model human --json
export SLB_SESSION_ID="<session_id from the response>"
slb git-hooks install
slb git-hooks status --json
```

An existing active project session can also be selected with `AGENT_NAME`.
An actor name does not create a session or grant approval. Explicit
`SLB_SESSION_ID` / `--session-id` selection takes precedence.

The installer uses Git's effective hooks directory, including linked worktrees
and `core.hooksPath`. Hooks can be shared across worktrees, but approvals and
sessions remain bound to each worktree's own `.slb/state.db` and absolute root.
The native path rejects a `--db` override pointing to another database.

Select a subset with `--hooks=pre-commit` or `--hooks=pre-push`. Installation is
idempotent for unmodified SLB-managed scripts. Existing foreign, modified or
symlink hooks are preserved and cause an explanatory error; there is no
force-overwrite option. Manual composition with other pre-push handlers must
provide the complete original stdin to every handler, not consume it once and
leave subsequent handlers an empty stream.

## What requires approval

`pre-commit` checks the **index**, not unstaged working-tree changes. Staged
file deletions and file-type replacements, such as replacing a file with a
symlink, require at least the configured **dangerous** review policy. Ordinary
renames are recognized. Initial commits and ordinary additions/modifications
are not flagged by these native structural checks.

`pre-push` consumes Git's complete ref-update protocol and requires at least
the configured **critical** review policy for ref deletions, non-fast-forward
updates, replaced tags/non-branch refs, unprovable ancestry, or changed protected
refs. Missing old objects are handled conservatively. A force flag alone is not
an effect: a genuine fast-forward to an unprotected branch remains allowed.
New unprotected branches and tags are allowed when they do not rewrite refs.

`main` is always protected by the native hook. Add more protected refs with:

```sh
git config --add slb.protectedBranch release
git config --add slb.protectedBranch refs/heads/production
```

Values are exact branch names or full refs, not glob patterns. Invalid values
block the check rather than silently dropping protection.

Configured tier quorums, dynamic quorum and different-model requirements apply,
with at least one independent approval required. Broad command SAFE allowlists
cannot exempt the native structural findings. These checks are not a recreation
of the original Git command-line flags or a semantic review of file contents.

## Request, review, retry Git

A risky Git attempt with an active SLB session creates a pending request and
blocks. Its expected-effect field contains the assessed tree/ref updates and a
snapshot fingerprint. Supply an operator rationale through `SLB_GIT_REASON`:

```sh
export SLB_GIT_REASON="Retiring an obsolete tracked artifact after its replacement was reviewed"
git commit -m "Retire obsolete artifact"
# Git is blocked; SLB prints the request ID and review instructions.
```

Review that request with `slb review <request-id>` and approve it from independent
reviewer sessions using the normal `slb approve` workflow. Then **retry the
original Git operation with the original requester session**.

Do **not** use `slb execute` on the `slb git-hooks authorize ...` token displayed
in the request. That token describes a hook authorization, not a command that
executes Git. The token's CLI handler deliberately refuses execution. Native
hooks consume approved tokens themselves and never recursively invoke a commit
or push.

A pending retry reuses the existing request. Rejected, expired and ambiguous
executing requests do not become permission. Explicit resubmission is available
through `SLB_GIT_NEW_REQUEST=1` on one Git invocation, with a fresh rationale and
normal quota/review requirements. Do not leave that environment variable enabled
on the approval-consuming retry: it explicitly asks for another request.

Approval consumption checks signed review evidence, expiry, exact command
specification, requester session, repository, current policy and the Git
snapshot. The database claim is single-use and atomic; concurrent consumers
cannot both claim one approval. A changed index, HEAD, target location or set of
ref updates cannot borrow an earlier snapshot's approval. Remote locations are
hashed in stored assessments so URL credentials are not displayed.

## Inspect without consuming approval

```sh
slb git-check --operation=commit --json
slb git-check --operation=commit --exit-code
slb git-check --operation=commit --request --reason "Explain this staged change" --json
```

The first command emits an assessment. `--exit-code` makes a review-required
assessment return nonzero. `--request` admits or shows a request but never
consumes approval. Add `--new-request` with `--request` to explicitly resubmit
after rejection or expiry.

Push diagnostics use `--operation=push --remote <name> --location <location>`
and read the same four-field, newline-delimited stdin that Git supplies to
`pre-push`: local ref, local object ID, remote ref, remote object ID. Both SHA-1
and SHA-256 object IDs are accepted; malformed, duplicate-destination or oversized
input fails closed. Normally, let the installed pre-push hook obtain this input
from Git rather than constructing it manually.

## Failure behavior, audit and boundaries

Missing SLB blocks the installed hook. Assessment errors, malformed input,
invalid policy, missing project state for a risky operation, unusable sessions,
invalid reviews, stale snapshots and failed claim/completion writes all block.
Structurally safe operations do not require an initialized SLB database. The
native flow uses the local database and does not require a running daemon;
configured Agent Mail notifications remain best-effort.

An execution record for a `git-hooks authorize` token records **hook
authorization only**, not success of the eventual Git operation. Its JSON log
has `event=git_hook_authorization` and `git_outcome=not_observed`; the prepared
log by itself is not a permit. Only durable successful claim/completion causes
the hook to allow Git. If Git later fails, the approval is still consumed.

Git hooks are client-side guardrails, not an access-control boundary. They can
be bypassed or modified by the local user. A pre-commit hook does **not** intercept
`checkout`, `reset`, `clean`, arbitrary content reversions or every way of
rewriting history. `pre-rebase` is not implemented. These hooks do not lock the
index against all external writers after returning; avoid concurrent index
mutation during a commit. Retain agent-tool interception and server-side branch
protection as separate layers where appropriate.

## Uninstall

```sh
slb git-hooks uninstall
```

Only exact SLB-managed scripts are uninstalled. They are renamed to unique
`.slb-disabled-*` backups in the hooks directory; backup paths are returned in
the result. Foreign or modified hooks are left untouched.
