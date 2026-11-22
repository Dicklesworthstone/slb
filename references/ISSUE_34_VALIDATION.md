# Issue #34: private-group directory validation

## Status and source

The production fix for [issue #34](https://github.com/Dicklesworthstone/slb/issues/34)
was already on main at
[3bc68010693c52bba9b4e0745cb7f4d66aa3a522](https://github.com/Dicklesworthstone/slb/commit/3bc68010693c52bba9b4e0745cb7f4d66aa3a522).
Its parent is the reported v0.6.1 revision,
`755f005c5f44da8539002569136920b7ea4cdbc0`.

**The production fix builds, passes vet, and passes the focused policy tests.
Live daemon/hook validation remains incomplete.** The attempted native test
failed because the executor denied Unix-domain socket creation. This record
does not claim a successful end-to-end run, a full race-suite pass, or release
qualification.

The initial validation commit,
[345def651b18da6e7556534420efa89917c51ac2](https://github.com/Dicklesworthstone/slb/commit/345def651b18da6e7556534420efa89917c51ac2),
added `TestBuiltCLIPrivateGroupRuntimeIntegration` to
[hook_installation_test.go](../internal/e2e/hook_installation_test.go), alongside
this validation record. That commit changed no production permissions,
discovery policy, or dependencies. The exact test-file blob compiled during the attempted run is
`1a92daa23b1322456264147f8fcfe885b8818f07`; its base blob was
`bcec5bf904a0a492eceb9bd4e4c27eee4e0ee404`.

## What the fix admits and rejects

The Go daemon and standalone Python hook both require local account evidence
before admitting a nonsticky, group-writable Linux ancestor. The directory
must belong to the effective UID/GID and have no world write or extended POSIX
access ACL. Trusted local account files must prove a single-user primary group
whose name matches the username. Relevant NSS databases must use only
`files`. The policy and its administrator-trust limits are documented in
[SECURITY.md](SECURITY.md#local-daemon-discovery-issue-33).

| Condition | Intended result |
| --- | --- |
| Owned `0775` ancestor with a verified private primary group and no access ACL | Admit |
| Another primary or supplementary group member | Reject |
| Remote/additional NSS providers, NSS actions, ambiguous account records, or unsafe account files | Reject the private-group exception |
| Nonsticky world-writable ancestor | Reject |
| Extended access ACL on an otherwise eligible group-writable ancestor | Reject |
| Group-writable `.slb` directory | Reject |
| Public discovery record or permissive socket | Reject |
| Sticky shared ancestor already accepted by the existing policy | Preserve existing behavior |
| Membership or permissions become unsafe after an earlier successful traversal | Revalidate and reject |

This is an ancestor exception. It does not relax the permissions of project
state, discovery records, PID files, sockets, or persistent leases.

## Completed checks

The following checks ran through DSR 0.2.3 with Go 1.26.10 on Linux/amd64.
The production source was exactly `3bc68010693c52bba9b4e0745cb7f4d66aa3a522`.
DSR recorded identical source before and after all three baseline checks.

| Check | Observed result |
| --- | --- |
| `go build ./...` | PASS, 40.810 seconds |
| `go vet ./...` | PASS, 8.549 seconds |
| Focused daemon/Python-hook policy command below | PASS, 12.145 seconds |
| Focused test accounting | 8 top-level passes; 75 passing test/subtest results; 2 skips |
| Standalone build of original v0.6.1 | PASS |
| Standalone build of current production fix | PASS |
| `git diff --check` for the new test addition | PASS |
| Compilation of the new E2E test and its production CLI | PASS; execution then failed at native socket creation |

The elapsed times identify this run; they are not performance benchmarks.
The 75 results include parent tests and subtests and are not 75 independent
top-level tests.

The focused command was:

```sh
go test -count=1 -v \
  -run 'Test(LocalUserPrivateGroup|ReadLocalAccountFile|RuntimeParentsPrivateGroup|RuntimeParentsRejectPrivateGroup|DiscoverRuntimeUserPrivateGroup|HookRuntime(LocalUserPrivateGroup|PrivateGroup))' \
  ./internal/daemon ./internal/cli
```

This covers Go/Python account-parser parity, malformed/remote NSS cases,
untrusted account-file rejection, private/shared/world-writable mode decisions,
ancestor and lexical-symlink traversal, hook refusal before RPC, and membership
refresh. Some existing hook tests deliberately use scripted transport; they
must not be treated as live Unix-socket evidence.

The baseline DSR receipt ID was `20261122T002400-MWxuGfMg`.
The baseline configuration SHA-256 was:

```text
5bd72c8c2ba23bed432a05b57cb33a660fc1f4ecca9fefcbdefd656cbbd6a4a5
```

### The two skips

- `TestDiscoverRuntimeUserPrivateGroupAncestors`: the ordinary executor's
  account files had unmapped owner UID `65534`. The production check correctly
  refused to treat them as trusted root-owned account databases.
- `TestRuntimeParentsRejectPrivateGroupWithAccessACL`: the fixture filesystem
  returned `ENOTSUP` when the test tried to install an actual POSIX access ACL.

Neither skipped case is counted as passed. A proposed rerun using trusted
namespace account files and a private tmpfs was interrupted before execution.

## Native test added in this follow-up

The new test uses private Linux user, mount, and PID namespaces plus a fixture
chroot. It creates real root-owned account files and real `0775` project,
home, and runtime directories through `umask 0002`. System executable/library
mounts are read-only, and PID-namespace teardown contains detached descendants.
The host's account files and mount configuration are not changed.

The fixture runs as **namespace UID/GID 0** with a conventional private group.
It is not evidence from an ordinary nonroot Linux login account.

The test's assertions require:

1. Production CLI initialization and installed native/generated Python hooks.
2. Detached daemon startup, matching daemon status, and real Unix-socket RPC.
3. Online acceptance of an unclassified probe and daemon-origin audit evidence
   for a denied dangerous-command classification.
4. Discovery refusal and offline hook decisions for eight unsafe conditions:
   shared supplementary membership, shared primary membership, additional NSS,
   world-writable project, world-writable runtime directory, writable
   `.slb`, public discovery record, and permissive socket.
5. Recovery to the same daemon PID after each repair.
6. Clean stop and removal of discovery, PID, and socket resources.

These are assertions added to the repository, not a claim that all ran here.
The attempted run completed namespace/chroot setup, real CLI initialization,
and hook installation. It then failed during daemon startup, before the
live-RPC, unsafe-transition, recovery, and shutdown assertions.

Missing Linux namespace prerequisites may skip the test. A skip does not
satisfy native validation. Once its capability probe succeeds, later setup and
lifecycle errors fail the test; the socket denial was not hidden as a pass.

## Observed native-transport blocker

The new test reported:

```text
Error: daemon exited before readiness: exit status 1
```

An independent foreground run exposed the underlying error:

```text
Error: creating ipc server: creating unix socket:
creating Unix socket: listen unix <fixture>/runtime/slb-<hash>.sock:
socket: operation not permitted
```

An independent Python `socket(AF_UNIX, SOCK_STREAM)` probe also returned
`PermissionError(1, 'Operation not permitted')`.

The private namespace did provide root-owned, mode `0644` passwd, group,
and nsswitch fixtures, and current code proceeded past ancestor validation.
The original v0.6.1 binary was rejected earlier by its ancestor check.
However, the overall version-comparison script failed when the current daemon
could not create its socket. **The comparison was not a passing lifecycle
comparison.**

Automatic approval rejected an attempt to run the socket probe outside the
restricted executor because `sandbox_approval` is disabled. The executor
subsequently disconnected and became unavailable. No alternate path was used
to evade that restriction.

## Follow-up validation and endpoint diagnostics

After the executor reconnected, DSR reran the native account-file and discovery
checks inside a Linux user/mount namespace under `umask 0002`, with trusted,
root-owned, files-only account fixtures. The namespace's effective UID and GID
were both `0`; this is not evidence from a nonroot host account.

`TestDiscoverRuntimeUserPrivateGroupAncestors` **passed**, resolving its earlier
unmapped-account-owner skip for this controlled namespace. The focused daemon
run recorded **5 top-level passes, 56 passing parent/subtest results, and one
skip**. These results overlap the earlier tests and must not be added together
as independent coverage. The ACL fixture still returned `ENOTSUP`, including
on the available tmpfs filesystem.

The DSR receipt was `20261122T010231-o0dA1PdX`; source fingerprints were unchanged
throughout the run. The snapshot contained the initial validation commit plus
the endpoint-diagnostic fix below and Git-classifier regression tests. Its UPG
production implementation was unchanged from `3bc6801`.

The command was:

```sh
go test -count=1 -v \
  -run 'Test(LocalUserPrivateGroup|ReadLocalAccountFile|RuntimeParentsPrivateGroup|RuntimeParentsRejectPrivateGroup|DiscoverRuntimeUserPrivateGroup)' \
  ./internal/daemon
```

The review also exposed a real diagnostic mismatch: the native IPC client
rejected unsafe socket paths without wrapping `ErrDiscoveryInvalid`, causing
hooks to report ordinary unavailability rather than invalid discovery. Commit
[71d49fdfeb97ff3d04535022edfc595608b8e938](https://github.com/Dicklesworthstone/slb/commit/71d49fdfeb97ff3d04535022edfc595608b8e938)
corrects that classification while retaining ordinary unavailability for a
genuinely missing endpoint.

Real filesystem regressions put a regular file, directory, symlink, and FIFO
behind a valid discovery record. All four failed the typed-error assertion
before the fix and passed afterward; the missing-endpoint control passed in
both runs. They exercise the actual client before dialing and require no
socket creation. DSR receipts `20261122T005618-ZGtCoUbX` and
`20261122T005744-5F3ZCapL` also record successful whole-module build, vet,
formatting, and diff checks for that fix. The first receipt intentionally has
a failing aggregate because it separately reproduced the then-unfixed Git
classification regression; its IPC check passed.

The bounded linter was killed with exit `137` before reporting findings. UBS
could not complete its Go helper checks and flagged a pre-existing PID fixture
as security-token randomness; that is not a passing scanner result. A fresh
socket capability probe continued to return `EPERM` for `AF_UNIX`, so the live
daemon/hook lifecycle remains unverified.

## Remaining gates and reproduction

| Gate | Status |
| --- | --- |
| Native daemon/hook lifecycle test | Attempted; FAILED at denied socket creation |
| Actual POSIX ACL rejection fixture | SKIPPED; filesystem lacks fixture support |
| Full race suite under `umask 0002` and `0022` | NOT VERIFIED |
| Final `golangci-lint run ./...` | NOT VERIFIED; follow-up run killed with exit 137 |
| UBS | INCOMPLETE; Go helper failures and a pre-existing PID-fixture finding |
| Module formatting gate for the endpoint fix | PASS through DSR |
| Nonroot Linux UPG host validation | NOT PERFORMED |
| macOS/Windows runtime validation | NOT PERFORMED |

Do not close these gaps on the strength of compilation, scripted hook tests,
namespace setup, or an unsupported-environment skip.

Use DSR for follow-up checks; GitHub Actions are not an execution path for this
repository. For example, configure these profiles in the DSR repositories
configuration:

```yaml
tools:
  slb-upg-policy:
    checks:
      - "go build ./..."
      - "go vet ./..."
      - "go test -count=1 -v -run 'Test(LocalUserPrivateGroup|ReadLocalAccountFile|RuntimeParentsPrivateGroup|RuntimeParentsRejectPrivateGroup|DiscoverRuntimeUserPrivateGroup|HookRuntime(LocalUserPrivateGroup|PrivateGroup))' ./internal/daemon ./internal/cli"
  slb-upg-native:
    checks:
      - "go test -race -count=1 -v -run '^TestBuiltCLIPrivateGroupRuntimeIntegration$' ./internal/e2e"
      - "go test -race -count=1 ./..."
      - "golangci-lint run ./..."
      - "go fmt ./..."
      - "git diff --check"
      - "ubs internal/e2e/hook_installation_test.go"
```

Run the policy profile against the selected checkout:

```sh
dsr --json quality --tool slb-upg-policy --work-dir /path/to/slb
```

On a Linux host with a trusted files-only UPG, functioning Unix sockets,
namespace/chroot support, and a filesystem supporting POSIX access ACLs, run
the native profile under both umasks:

```sh
(umask 0002; dsr --json quality --tool slb-upg-native --work-dir /path/to/slb)
(umask 0022; dsr --json quality --tool slb-upg-native --work-dir /path/to/slb)
```

Inspect verbose output and require actual PASS results for the named native,
discovery, and ACL tests. Record a prerequisite skip as blocked validation,
even if the aggregate test command exits zero. The native fixture also needs
its Python interpreter and dependencies reachable through the system paths it
mounts (`/usr`, `/bin`, `/lib`, `/lib64`, and `/opt`).
