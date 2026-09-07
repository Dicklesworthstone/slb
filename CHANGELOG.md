# Changelog

All notable changes to [SLB (Simultaneous Launch Button)](https://github.com/Dicklesworthstone/slb) are documented in this file.

SLB is a cross-platform CLI that implements a **two-person rule** for running potentially destructive commands from AI coding agents. It provides risk-based command classification, peer review enforcement, and full audit logging for multi-agent workflows.

> Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). This project uses [Semantic Versioning](https://semver.org/).

---

## [v0.4.1] -- 2026-09-07

Compare: [`v0.4.0...v0.4.1`](https://github.com/Dicklesworthstone/slb/compare/v0.4.0...v0.4.1)

### Pattern Matching

- **System-path and flag patterns are anchored to whole tokens** ([#11](https://github.com/Dicklesworthstone/slb/issues/11)): the CRITICAL `chmod`/`chown` rules matched `/(etc|usr|var|boot|bin|sbin)` as a bare substring anywhere in the command, so a project's own `chmod +x /home/u/proj/bin/tool` (and `/opt/app/binary`, via the `bin` prefix) needed two approvals. The system directory now has to be the first component of an absolute path token and end at a path boundary. The sibling audit of the builtin set fixed the same class elsewhere: the `rm` system-path rule (`/home` matched `/homework`, `/opt` matched `/optional`, `/lib` matched `/libs`), `git push -f` (`.*-f(\s|$)` fired on any branch ending in `-f`; the flag must now be its own short-flag token, bundled `-fu`/`-uf` still caught), `gcloud ... delete --quiet` (`delete` matched inside `undelete` and `--filter=name:delete-me`; `-q` accepted), and the SAFE `rm ... .log/.tmp/.bak` rules, which only looked at the last token so `rm -rf / foo.log` was SAFE and skipped review; every target must now end in the extension. A parity test runs the exported Python hook module under `python3` and checks it classifies these cases identically to the Go engine ([`9c3e140`](https://github.com/Dicklesworthstone/slb/commit/9c3e1405592aabe9ffbf912d6632f13ab1df1f63))

### TUI

- **Navigated views no longer wedge at "Loading..."** ([#10](https://github.com/Dicklesworthstone/slb/issues/10)): Bubble Tea delivers `WindowSizeMsg` once at startup, but the detail/dashboard/history/patterns views are created fresh on navigation and gated their first render on it, so pressing Enter on a pending request showed "Loading..." until the terminal was resized. The root model now replays its last known size into any view it creates; the detail view renders directly when no size is known and its divider no longer panics at zero width; the dashboard footer advertises `[enter]` ([`893b8ce`](https://github.com/Dicklesworthstone/slb/commit/893b8ce3006f95be4d8f05792c67c9f11af79cd6))

### Build & Tests

- `TestDefaultSocketPath_FormatStable` compares against `filepath.Clean(os.TempDir())`; on macOS `$TMPDIR` ends in `/` and the test failed on every Mac ([`59f61ee`](https://github.com/Dicklesworthstone/slb/commit/59f61ee64cc5f5837cb5d2a89cc3f72f244f5d2a))
- `spf13/pflag` promoted to a direct module requirement ([`0e68819`](https://github.com/Dicklesworthstone/slb/commit/0e68819af96dff102004f5eadecfa57ea2502218))
- AGENTS.md: require the OpenAI File Downloader user-agent on curl/web fetches ([`e73ce49`](https://github.com/Dicklesworthstone/slb/commit/e73ce498b377c8642af21c02d31e8c3ac64678d1))

---

## [v0.4.0] -- 2026-08-05

Security release. Compare: [`v0.3.1...v0.4.0`](https://github.com/Dicklesworthstone/slb/compare/v0.3.1...v0.4.0)

### Security

- **Unmatched commands can no longer skip the two-person rule** ([#9](https://github.com/Dicklesworthstone/slb/issues/9)): `CreateRequest` had a default-allow branch, so any command matching no pattern (interpreter wrappers such as `uv run python script.py`, `bash -c ...`, `node -e ...`) was "Skipped" and executed immediately with zero approvals. Unmatched commands now **fail closed** and escalate to the dangerous tier ([`004bb40`](https://github.com/Dicklesworthstone/slb/commit/004bb4050dae657e3d433fe71463f20596a1d7b0)). Follow-up from a non-author review: in a compound command (`a && b`, `a; b`, pipelines) a SAFE segment could launder an unmatched sibling past approval, because the first safe segment alone decided the verdict; a segment matching nothing now raises the overall tier ([`2fb23f3`](https://github.com/Dicklesworthstone/slb/commit/2fb23f319f043b3dba492cd0eb523ba0f49dc06d))

### CLI Fixes

- Command errors are printed again instead of collapsing into a bare `exit 1` -- the root command set `SilenceErrors` while `main()` exited without printing, so every failing subcommand (including `--session-id is required`) produced zero output ([#8](https://github.com/Dicklesworthstone/slb/issues/8), [`697fb50`](https://github.com/Dicklesworthstone/slb/commit/697fb503711f1def12cf5b95e912d04e5560ff2f))
- `execute`, `request` and `run` now load persisted custom patterns before classifying; they classified against builtins only and silently ignored every `slb patterns add` ([#7](https://github.com/Dicklesworthstone/slb/issues/7), [`c990c9a`](https://github.com/Dicklesworthstone/slb/commit/c990c9ab9792038eee5f3e4d2f60f7a5f47862e5))
- Subcommand flags that collided with root persistent flags (`-t`, `-s`) caused a pflag panic on `slb execute --help`; shorthands dropped ([#7](https://github.com/Dicklesworthstone/slb/issues/7), [`1e9dac9`](https://github.com/Dicklesworthstone/slb/commit/1e9dac95e330d3a7a750b1d3067a9789adcfc794))
- Nil `Classification` guarded on the skipped-request path ([#7](https://github.com/Dicklesworthstone/slb/issues/7), [`e2baefb`](https://github.com/Dicklesworthstone/slb/commit/e2baefbbd10d8a03dc9b3660fc38bf4a6b1e6bab))

### Custom Patterns and Hook

- `slb patterns add` reported `status:added` but never persisted anything; additions now go to the `custom_patterns` table and are reloaded on read ([#2](https://github.com/Dicklesworthstone/slb/issues/2), [`566daed`](https://github.com/Dicklesworthstone/slb/commit/566daed02f08be9b7332c11db1a85c446c9a6629)); the daemon, `hook generate/install/test/status` and `patterns version` all merge persisted customs too ([`4c7b550`](https://github.com/Dicklesworthstone/slb/commit/4c7b550efcfe389d132cc556ecb766c9e5998115), [`69ee1a5`](https://github.com/Dicklesworthstone/slb/commit/69ee1a597f19a517cd4154d4fd70d0c2b2c4d00b), [`7cceea8`](https://github.com/Dicklesworthstone/slb/commit/7cceea82fac90ef2556dc917b9a432f6e2888bd8))
- Daemon and hook hash the nearest `.slb/` project root, not the CWD, for the socket path, so a hook fired from a sub-directory reaches the daemon ([#3](https://github.com/Dicklesworthstone/slb/issues/3), [`dad649e`](https://github.com/Dicklesworthstone/slb/commit/dad649ef50a6ae91ea8d340cb4c7b1b1184cf6c0))
- Generated `slb_guard.py` emits the Claude Code 2026.04 `hookSpecificOutput.permissionDecision` shape and no longer double-escapes backslashes inside Python raw-string regexes (every `\s`, `\b`, `\w` in the 52 builtins was dead) ([#4](https://github.com/Dicklesworthstone/slb/issues/4), [#5](https://github.com/Dicklesworthstone/slb/issues/5), [`4d815ce`](https://github.com/Dicklesworthstone/slb/commit/4d815ce315d7d77a8b1e989c6fe066136adaa154)); fresh-eyes follow-ups pinned the hook contract in regression tests and made unknown daemon verdicts deny rather than allow ([`3204527`](https://github.com/Dicklesworthstone/slb/commit/3204527e0bd8a7073c4c6fe25b9e99e2c3d713e3), [`64ccd73`](https://github.com/Dicklesworthstone/slb/commit/64ccd739e1c0560a7cdbe8d0ee966bb3f2a27aac))

---

## [v0.3.1] -- 2026-04-24

Compare: [`v0.3.0...v0.3.1`](https://github.com/Dicklesworthstone/slb/compare/v0.3.0...v0.3.1)

- Release workflow: the "Verify Linux binary" step tolerates goreleaser v2 dist layout shifts ([`3e9a549`](https://github.com/Dicklesworthstone/slb/commit/3e9a549a5ed9b37188e0ac3b94d208f3740d5d3e))

---

## [v0.3.0] -- 2026-03-26

Compare: [`v0.2.0...v0.3.0`](https://github.com/Dicklesworthstone/slb/compare/v0.2.0...v0.3.0)

### Toolchain & Skill

- Go toolchain bumped to 1.24.13 ([`b4b90a4`](https://github.com/Dicklesworthstone/slb/commit/b4b90a4fea734920468eb6b48d712149df458bfd))
- Claude Code skill: SKILL.md refreshed and `references/` documentation added ([`ccc708f`](https://github.com/Dicklesworthstone/slb/commit/ccc708fd7d24afa743132fec0e675157488b53a5))

### Pattern Matching

- **Fallback SQL DELETE detection for compound commands**: The pattern engine now detects dangerous `DELETE FROM` statements embedded inside compound commands (e.g., `psql -c "DELETE FROM users; DROP TABLE x;"`), closing a gap where SQL wrapped in shell commands could evade classification ([`c40b097`](https://github.com/Dicklesworthstone/slb/commit/c40b09758b21edd800a3b4ed0082cacf3d08bff9))

### Output Formats

- **TOON format support**: Token-efficient encoding via `tru`/`tr` binary for structured CLI output, reducing token consumption when agents consume `slb` output ([`d2154d9`](https://github.com/Dicklesworthstone/slb/commit/d2154d93694ddf7ac9688d59c2585b5827e3a7fe))
- TOON output simplified and refactored; support for both `tru` and `tr` binary names ([`9c741cc`](https://github.com/Dicklesworthstone/slb/commit/9c741cc567b0e56a90ff38bd05f7c91fc9042546), [`62803d9`](https://github.com/Dicklesworthstone/slb/commit/62803d908ade7de88b87e8a22b2aad33fa99c919), [`de41dbb`](https://github.com/Dicklesworthstone/slb/commit/de41dbb6e1be4b5e037900d64fe397cd1cd3bd62))
- **`--stats` flag** and `SLB_OUTPUT_FORMAT` environment variable for output format control ([`91241f6`](https://github.com/Dicklesworthstone/slb/commit/91241f688913f99a3e67e7012e76021868dd007d))

### CLI Fixes

- `slb check` now outputs human-readable text instead of raw Go map syntax ([`99cda5b`](https://github.com/Dicklesworthstone/slb/commit/99cda5b660a3e44b3bc1d3ba7fde244d7e76da28))
- Status update failures after command execution are now logged instead of silently swallowed ([`2a5110e`](https://github.com/Dicklesworthstone/slb/commit/2a5110e074b96ab4c043b6d524f8efcb8130cc46))
- Tier flag updated from `-t` to `-T` in pattern tests to avoid flag collision ([`f56eeb0`](https://github.com/Dicklesworthstone/slb/commit/f56eeb0885492100f355513cbcc33344e1baeba7))

### Licensing & Branding

- License changed to **MIT with OpenAI/Anthropic Rider** ([`badc986`](https://github.com/Dicklesworthstone/slb/commit/badc9863901f79ddf9a4ee7a03d923646e329173), [`b7becfe`](https://github.com/Dicklesworthstone/slb/commit/b7becfe577b20eb89ab60c1c426cdd5238f2b3c9))
- README updated to reference new license ([`a82e130`](https://github.com/Dicklesworthstone/slb/commit/a82e130131ae01a78e7d9a03c286a38a8dd2128b))
- GitHub social preview image added (1280x640) ([`28c2bf7`](https://github.com/Dicklesworthstone/slb/commit/28c2bf77316658962fd72e097f11d710977a14e1))

### Build & CI

- Resolved errcheck lint errors for unchecked error returns ([`0981efd`](https://github.com/Dicklesworthstone/slb/commit/0981efdbfbc6fee6d41afbd35be98b81db0363a5))
- golangci-lint v2 compatibility fixes and configuration refactored for better code quality checks ([`15bc242`](https://github.com/Dicklesworthstone/slb/commit/15bc24259150db03bef8989b1cd54ddcb1906eb6), [`4eec3b6`](https://github.com/Dicklesworthstone/slb/commit/4eec3b61300ca001d69f434b9eeea9621922fd18), [`8aee4ec`](https://github.com/Dicklesworthstone/slb/commit/8aee4ecdb1b96b2a14e97e5571a650d49ed8b0c0), [`3782619`](https://github.com/Dicklesworthstone/slb/commit/3782619c5531ce41c7ceac572ae1b2d9e99abe3b))
- CI workflow improved with security and reliability enhancements; test reliability fixes ([`4acadd0`](https://github.com/Dicklesworthstone/slb/commit/4acadd084a7a4df054d3a4635ba2a228a222d239), [`a1737b4`](https://github.com/Dicklesworthstone/slb/commit/a1737b4b4a6be377ebc326aad286c47fbc68e4bb))
- Go module dependencies updated to latest stable versions ([`d66801f`](https://github.com/Dicklesworthstone/slb/commit/d66801fb728df138ea702232018635f8a5ce597a))
- ACFS checksum dispatch and notification workflows added ([`f0fe162`](https://github.com/Dicklesworthstone/slb/commit/f0fe16231c568220c8cc74f17cb1da7301914c8f), [`94a35eb`](https://github.com/Dicklesworthstone/slb/commit/94a35eb8927ffe2b92dfc9d4466d316d388799f6))

### Documentation

- README: prioritize Homebrew/Scoop installation methods over direct download ([`0b0307c`](https://github.com/Dicklesworthstone/slb/commit/0b0307c980ee4fe13868e5298b902d5da933df67))
- AGENTS.md updated with latest multi-agent conventions ([`a5a4d59`](https://github.com/Dicklesworthstone/slb/commit/a5a4d590fbccffa9539a81e021daeb4f04345180))

---

## [v0.2.0] -- 2026-01-13

**GitHub Release**: [`v0.2.0`](https://github.com/Dicklesworthstone/slb/releases/tag/v0.2.0) (published 2026-01-14)
Compare: [`v0.1.0...v0.2.0`](https://github.com/Dicklesworthstone/slb/compare/v0.1.0...v0.2.0)

This release adds Claude Code hook integration for automatic command interception, enables Homebrew and Scoop auto-publishing via GoReleaser, and resolves a batch of state machine, pattern matching, and hook bugs discovered during post-v0.1.0 stabilization.

### Claude Code Hook Integration

The major feature of this release: a complete `slb hook` subcommand suite that intercepts Bash tool calls before execution in Claude Code sessions.

- **Hook infrastructure**: `slb hook generate`, `install`, `uninstall`, `status`, `test` commands with a Python guard script (`~/.slb/hooks/slb_guard.py`) that classifies risk and communicates with the SLB daemon via Unix socket ([`39c2f87`](https://github.com/Dicklesworthstone/slb/commit/39c2f87ff26843c2bc72c528cb3614a26efddb3c))
- Returns `allow`, `ask`, or `block` action to Claude Code based on risk tier
- Fail-closed: dangerous commands blocked when SLB daemon is unavailable

### Package Distribution

- **GoReleaser auto-publishing** to Homebrew (`brew install dicklesworthstone/tap/slb`) and Scoop (`scoop install dicklesworthstone/slb`) -- packages now auto-update on every release ([`995dd17`](https://github.com/Dicklesworthstone/slb/commit/995dd17c3fbbe84363306b020cbc9288b869e1ef))
- GoReleaser config updated for v2 format (`folder` -> `directory`); Homebrew skipped temporarily until tap repo was ready ([`87db783`](https://github.com/Dicklesworthstone/slb/commit/87db7832ac0b1f578de7a59efdb60d8d0b3850e0), [`8764edd`](https://github.com/Dicklesworthstone/slb/commit/8764eddb4b87f239441c5769ccb54589404a89c2))
- Claude Code `SKILL.md` added for automatic capability discovery ([`2926cda`](https://github.com/Dicklesworthstone/slb/commit/2926cda2a0eee5b8bcceb0efddbfed070f5cae9a))

### Security

- **Compound command quote bypass vulnerability fixed**: Shell-aware splitting now correctly handles quoted separators, preventing commands like `echo ";" && rm -rf /` from being misclassified as safe ([`dffc948`](https://github.com/Dicklesworthstone/slb/commit/dffc94866e3f0f2e04027fedea3de2167599638f))

### State Machine Fixes

- Transitions now prevent panics on short request IDs ([`ba4510d`](https://github.com/Dicklesworthstone/slb/commit/ba4510d9d089d5c5c74c72c4e8d02c94a911a842))
- Cancel command and `StatusEscalated` state transitions corrected ([`1476f11`](https://github.com/Dicklesworthstone/slb/commit/1476f11d8fb5fb6d146acd3285e5fad09f316356))
- Escalated requests can now be reviewed (previously silently rejected) ([`00b213c`](https://github.com/Dicklesworthstone/slb/commit/00b213cfd1fec5d4d017b559d3bc0272f71515d6))

### Pattern Matching Fixes

- Compound command tier precedence corrected -- highest-risk segment now properly determines overall tier ([`7fd44d9`](https://github.com/Dicklesworthstone/slb/commit/7fd44d9c27bb341cba94f680ad4f51749fcdaaa4))
- `IsSafe` flag initialization corrected for compound command classification ([`036c75a`](https://github.com/Dicklesworthstone/slb/commit/036c75a6ca168dde2d74d2c8d05182c8af4177d6))
- `rm -fr` pattern bug resolved (previously only `rm -rf` was matched) ([`bb9f88a`](https://github.com/Dicklesworthstone/slb/commit/bb9f88a0e2729e59e88ba980c28760c9cc0d79a0))

### Execution Fixes

- `exit_code` and `duration` only set when command result is actually available, preventing nil dereferences ([`1c733e8`](https://github.com/Dicklesworthstone/slb/commit/1c733e899a5691aaa6754bdc37f5c1596874c48f))
- Edge cases in command truncation and event type mapping handled ([`8d48a9e`](https://github.com/Dicklesworthstone/slb/commit/8d48a9e607526240a830bfc36b6f2c8738804349))
- Missing `Execution`/`Rollback` parsing in `scanRequests` database query ([`bc94544`](https://github.com/Dicklesworthstone/slb/commit/bc9454456117d2f3eb4f4ba81c9f5b64ffb8751c))

### Hook Fixes

- Corrected bounds check in `slb_guard.py` substring matching ([`24157ef`](https://github.com/Dicklesworthstone/slb/commit/24157ef14ea75d3b741845c77703d89f5f8acdbc))
- Fixed Python hook daemon communication path ([`7268863`](https://github.com/Dicklesworthstone/slb/commit/726886309201f60a41aeba225f7834a0c1830aa1))
- `hookTestCmd` validation and Python fallback caution handling corrected ([`571971e`](https://github.com/Dicklesworthstone/slb/commit/571971e324df1a99e4239a7f6d7ef7eb8265ceae))
- macOS test compatibility issues resolved ([`bb9f88a`](https://github.com/Dicklesworthstone/slb/commit/bb9f88a0e2729e59e88ba980c28760c9cc0d79a0))

### Documentation

- Comprehensive README written covering all features: request lifecycle, execution verification gates, pattern engine internals, TUI dashboard, agent mail, outcome tracking, session management, emergency overrides ([`f7d32a7`](https://github.com/Dicklesworthstone/slb/commit/f7d32a756fdd37235480441abf5d654bcab7a278), [`786899a`](https://github.com/Dicklesworthstone/slb/commit/786899a6bc492189938375e38bcd763d026576eb))
- AI writing patterns removed from README ([`05b3036`](https://github.com/Dicklesworthstone/slb/commit/05b30366482b4456105c3734b6a6d7a0805ebf88))

---

## [v0.1.0] -- 2025-12-24

**GitHub Release**: [`v0.1.0`](https://github.com/Dicklesworthstone/slb/releases/tag/v0.1.0) (published 2025-12-25)

The initial public release of SLB, built from scratch in ~11 days (2025-12-13 to 2025-12-24) with 264 commits, comprehensive test coverage (80%+ CI threshold), and cross-platform binaries via GoReleaser.

### Command Classification Engine

The core of SLB: a shell-aware pattern matching engine that classifies commands into risk tiers before execution.

- **Four-tier risk classification**: CRITICAL (2+ approvals, never auto-approve), DANGEROUS (1 approval), CAUTION (auto-approve after 30s delay), SAFE (immediate, no review) ([`c4561db`](https://github.com/Dicklesworthstone/slb/commit/c4561db89abebab1f24e7717b2276308e6e67aca))
- **Shell-aware normalization**: Strips wrapper prefixes (`sudo`, `doas`, `env`, `time`, `nohup`), extracts inner commands from `bash -c '...'`, resolves relative paths to absolute ([`c4561db`](https://github.com/Dicklesworthstone/slb/commit/c4561db89abebab1f24e7717b2276308e6e67aca))
- **Compound command splitting**: Commands joined by `&&`, `||`, `;`, `|` are split and classified independently -- the highest-risk segment determines the overall tier ([`c4561db`](https://github.com/Dicklesworthstone/slb/commit/c4561db89abebab1f24e7717b2276308e6e67aca))
- **Fail-safe parse handling**: Unparseable commands (unbalanced quotes, complex escapes) get their tier upgraded one level (SAFE -> CAUTION, CAUTION -> DANGEROUS, etc.) ([`c4561db`](https://github.com/Dicklesworthstone/slb/commit/c4561db89abebab1f24e7717b2276308e6e67aca))
- **Runtime pattern management** via `slb patterns list|test|add` -- agents can add patterns but not remove them ([`a515b09`](https://github.com/Dicklesworthstone/slb/commit/a515b09bf9a0212263550ce0ac7ab5ec8f1cc45e))
- Critical patterns for disk destruction (`dd of=/dev/`) and system file changes added ([`b3dd913`](https://github.com/Dicklesworthstone/slb/commit/b3dd913dc748a2afddb9fdc56a1b3a95f5277202))
- Edge case gaps in risk classification closed ([`1225afb`](https://github.com/Dicklesworthstone/slb/commit/1225afb9e33e3d331e0e2af57a8ea4b21a206215))

### Request Lifecycle & Execution

The complete workflow from requesting approval through execution and rollback.

- **`slb run`**: Atomic check-request-wait-execute pipeline -- the primary command for agents ([`a515b09`](https://github.com/Dicklesworthstone/slb/commit/a515b09bf9a0212263550ce0ac7ab5ec8f1cc45e))
- **Client-side execution**: Commands run in the calling process's shell environment, inheriting AWS credentials, kubeconfig, virtualenvs, SSH agents, database connection strings ([`71d5808`](https://github.com/Dicklesworthstone/slb/commit/71d58088b945f3175560cb2d61f594cefd98415b))
- **Command hash binding**: SHA-256 hash computed at request time, verified before execution -- any modification after approval is rejected ([`c4561db`](https://github.com/Dicklesworthstone/slb/commit/c4561db89abebab1f24e7717b2276308e6e67aca))
- **Five execution verification gates**: status check, approval expiry, command hash match, tier consistency, first-executor-wins atomicity ([`71d5808`](https://github.com/Dicklesworthstone/slb/commit/71d58088b945f3175560cb2d61f594cefd98415b))
- **Dry run pre-flight** for supported commands: `terraform plan`, `kubectl diff`, `git diff` ([`d1e8bde`](https://github.com/Dicklesworthstone/slb/commit/d1e8bde94d8d77bdafb219954149ef0ff114aad1))
- **Rollback state capture**: Filesystem tar archives, git state (HEAD, branch, dirty files), Kubernetes manifests captured before execution for potential rollback via `slb rollback` ([`d1e8bde`](https://github.com/Dicklesworthstone/slb/commit/d1e8bde94d8d77bdafb219954149ef0ff114aad1))
- **Emergency override**: `slb emergency-execute` with mandatory reason, hash acknowledgment, and permanent audit record for true emergencies ([`a515b09`](https://github.com/Dicklesworthstone/slb/commit/a515b09bf9a0212263550ce0ac7ab5ec8f1cc45e))
- Request state machine with well-defined transitions: PENDING -> APPROVED/REJECTED/CANCELLED/TIMEOUT -> EXECUTING -> EXECUTED/EXEC_FAIL/TIMED_OUT ([`c4561db`](https://github.com/Dicklesworthstone/slb/commit/c4561db89abebab1f24e7717b2276308e6e67aca))
- Approval TTL enforcement: 30 minutes standard, 10 minutes for CRITICAL ([`c4561db`](https://github.com/Dicklesworthstone/slb/commit/c4561db89abebab1f24e7717b2276308e6e67aca))

### CLI Commands

The full command-line interface for agents and human reviewers.

- **`slb init`**: Project initialization creating `.slb/` directory with `state.db`, `config.toml`, `pending/`, sessions, and logs ([`feb8fab`](https://github.com/Dicklesworthstone/slb/commit/feb8fabf4d73ef34fa9f95de14c0af9ff0fc476a))
- **Request plumbing**: `slb request`, `slb status [--wait]`, `slb pending [--all-projects]`, `slb cancel` ([`a515b09`](https://github.com/Dicklesworthstone/slb/commit/a515b09bf9a0212263550ce0ac7ab5ec8f1cc45e))
- **Peer review**: `slb review`, `slb approve`, `slb reject` with `--target-project` flag for cross-project reviews ([`d4894a7`](https://github.com/Dicklesworthstone/slb/commit/d4894a776526697dc5be3b45193a24829e4930b3), [`701df4c`](https://github.com/Dicklesworthstone/slb/commit/701df4cd349e803fc119b552bfdc6289bd8737f3))
- **Execution**: `slb execute`, `slb emergency-execute`, `slb rollback` ([`4f1acc0`](https://github.com/Dicklesworthstone/slb/commit/4f1acc024d6ce9a55b3fc55ec7d56d55746facac))
- **Session management**: `slb session start|end|resume|list|heartbeat|gc|reset-limits` ([`a515b09`](https://github.com/Dicklesworthstone/slb/commit/a515b09bf9a0212263550ce0ac7ab5ec8f1cc45e))
- **History & search**: `slb history` with full-text search (`-q`), tier/status/agent/date filtering; `slb show` with `--with-reviews`, `--with-execution`, `--with-attachments` ([`a515b09`](https://github.com/Dicklesworthstone/slb/commit/a515b09bf9a0212263550ce0ac7ab5ec8f1cc45e))
- **Outcome tracking**: `slb outcome record|list|stats` for execution feedback to improve classification over time ([`6c3b272`](https://github.com/Dicklesworthstone/slb/commit/6c3b272ddfbbac468476b62c57f5979bb786766e))
- **Event streaming**: `slb watch` with real-time NDJSON output, polling fallback, and `--auto-approve-caution` for reviewer agents ([`b958a38`](https://github.com/Dicklesworthstone/slb/commit/b958a3855379c055e0805bf2d5bd0fe0d3be078b))
- **Daemon management**: `slb daemon start|stop|status` ([`cf17daa`](https://github.com/Dicklesworthstone/slb/commit/cf17daa54521184e129a0bf92354fc18c0b5c794))
- **IDE integration generators**: `slb integrations claude-hooks` and `slb integrations cursor-rules` ([`7fd6a7d`](https://github.com/Dicklesworthstone/slb/commit/7fd6a7d60bd00837273bd7dec5b0da1aad18d0d6))
- **Shell completions**: `slb completion bash|zsh|fish` ([`a515b09`](https://github.com/Dicklesworthstone/slb/commit/a515b09bf9a0212263550ce0ac7ab5ec8f1cc45e))
- **Request attachments**: `--attach` for files/images, `--attach-cmd` for command output ([`a515b09`](https://github.com/Dicklesworthstone/slb/commit/a515b09bf9a0212263550ce0ac7ab5ec8f1cc45e))
- JSON and YAML output formats (`--output json`, `--output yaml`, `--json`) ([`de1fe83`](https://github.com/Dicklesworthstone/slb/commit/de1fe83325a6ec6f41e7140ab5b55b6c01fa4977))
- Structured exit codes (0=success, 1=error, 2=invalid args, 3=not found, 4=permission denied, 5=timeout, 6=rate limited) ([`a515b09`](https://github.com/Dicklesworthstone/slb/commit/a515b09bf9a0212263550ce0ac7ab5ec8f1cc45e))

### Storage & Database

- **SQLite with WAL mode** and **FTS5 full-text search** for request history queries ([`f5c8e40`](https://github.com/Dicklesworthstone/slb/commit/f5c8e4008128d7fb3f2b93406193e6b790b5fbe9))
- `ListAllRequests`, runtime pattern changes, and enhanced query layer ([`0c7e07d`](https://github.com/Dicklesworthstone/slb/commit/0c7e07d03503dfed779b92fa6b85b73d67828417))
- `isUniqueConstraintError` fixed to not incorrectly match FOREIGN KEY errors ([`5561d52`](https://github.com/Dicklesworthstone/slb/commit/5561d523c64dce5e511d49f6952811559fe4f505))

### Daemon & IPC

The background daemon provides real-time notifications and execution verification.

- **Unix socket IPC server** with JSON-RPC 2.0 protocol: `hook_query`, `hook_health`, `verify_execution`, `subscribe` methods ([`9fe267f`](https://github.com/Dicklesworthstone/slb/commit/9fe267f2b04e3d00507b57d140276323be492e16))
- **TCP transport mode** for Docker containers and remote agents with auth and IP whitelisting ([`007a1fd`](https://github.com/Dicklesworthstone/slb/commit/007a1fd7c647b6cf33be52cd455c03b98d870cf2))
- **Webhook notification system** for external alerting integrations (Slack, etc.) ([`4a635db`](https://github.com/Dicklesworthstone/slb/commit/4a635dbcc02dc5b33be95bb21ccdd27ad2111a16))
- **Desktop notifications** via AppleScript (macOS), notify-send (Linux), PowerShell (Windows) ([`007a1fd`](https://github.com/Dicklesworthstone/slb/commit/007a1fd7c647b6cf33be52cd455c03b98d870cf2))
- **File watcher** monitoring `pending/` directory for new request JSON files ([`007a1fd`](https://github.com/Dicklesworthstone/slb/commit/007a1fd7c647b6cf33be52cd455c03b98d870cf2))
- Timeout handling with configurable actions: `escalate` (default), `auto_reject`, `auto_approve_warn` ([`007a1fd`](https://github.com/Dicklesworthstone/slb/commit/007a1fd7c647b6cf33be52cd455c03b98d870cf2))
- IPC server refuses to delete non-socket files, preventing accidental data loss ([`662ffee`](https://github.com/Dicklesworthstone/slb/commit/662ffeefcd58a6bec4a532045114f81e3d2f0fe4))

### TUI Dashboard

An interactive terminal UI for human reviewers to monitor and act on pending requests.

- **Three-panel layout**: Agents (active sessions), Pending Requests (sorted by urgency), Activity feed (real-time) ([`6eea51f`](https://github.com/Dicklesworthstone/slb/commit/6eea51f9965cd234decd44cc2b9709dcd0015e7c))
- **Interactive reviews**: Approve/reject requests directly from the TUI with keyboard shortcuts ([`88c25c8`](https://github.com/Dicklesworthstone/slb/commit/88c25c8c5ecacbc3131ac4a77886d43b5f7f824c), [`ac3f30e`](https://github.com/Dicklesworthstone/slb/commit/ac3f30ef9a395432f7006f2ff2a85b6c8e8e6ee0))
- **Multi-view navigation**: Pattern management view, history browser with FTS search ([`fcc6b68`](https://github.com/Dicklesworthstone/slb/commit/fcc6b68e669ce462af64c2faa71507a1b4c1a146), [`8f5248d`](https://github.com/Dicklesworthstone/slb/commit/8f5248d05e99483b60cfb6de59b7b2d19636c7c4))
- **Pattern removal review**: Human-in-the-loop view for reviewing agent-proposed pattern removals ([`6bba671`](https://github.com/Dicklesworthstone/slb/commit/6bba67182031a8e10599ed8be1f4dae0005d3662))
- Component library: StatusBadge, AgentCard, Timeline, icons ([`f2dca58`](https://github.com/Dicklesworthstone/slb/commit/f2dca58525da128a24e44eae1694b0c0e4cfded9), [`de22580`](https://github.com/Dicklesworthstone/slb/commit/de225807502fd2b18e6a334b4e2894ccd511aaa2))

### Configuration System

- **Hierarchical TOML configuration** with five priority levels: built-in defaults < user config (`~/.slb/config.toml`) < project config (`.slb/config.toml`) < environment variables (`SLB_*`) < CLI flags ([`58084cb`](https://github.com/Dicklesworthstone/slb/commit/58084cb17b90ad6e7f6a92661eae363be2de0a20))
- Viper config library integration ([`9d1b783`](https://github.com/Dicklesworthstone/slb/commit/9d1b783d15ca95379ece5d46733793c512ec4687))
- **Cross-project reviews** with configurable review pools ([`58084cb`](https://github.com/Dicklesworthstone/slb/commit/58084cb17b90ad6e7f6a92661eae363be2de0a20))
- **Trusted self-approval** with mandatory delay for designated agents ([`58084cb`](https://github.com/Dicklesworthstone/slb/commit/58084cb17b90ad6e7f6a92661eae363be2de0a20))
- **Conflict resolution policies**: `any_rejection_blocks` (default), `first_wins`, `human_breaks_tie` ([`58084cb`](https://github.com/Dicklesworthstone/slb/commit/58084cb17b90ad6e7f6a92661eae363be2de0a20))
- **Different-model requirement** with timeout escalation to human reviewers ([`9d1b783`](https://github.com/Dicklesworthstone/slb/commit/9d1b783d15ca95379ece5d46733793c512ec4687), [`d43682d`](https://github.com/Dicklesworthstone/slb/commit/d43682d58bf99dc23fdc4286ad96d54fe6ea4a86))
- **Rate limiting** with configurable actions: `reject`, `queue`, `warn` ([`58084cb`](https://github.com/Dicklesworthstone/slb/commit/58084cb17b90ad6e7f6a92661eae363be2de0a20))
- **Dynamic quorum** scaling based on active reviewer count ([`58084cb`](https://github.com/Dicklesworthstone/slb/commit/58084cb17b90ad6e7f6a92661eae363be2de0a20))

### Security

- **Session key verification** on review submission -- prevents review forgery via HMAC signatures ([`59e183d`](https://github.com/Dicklesworthstone/slb/commit/59e183daaca07676000a676874e8f63097e0db7e))
- **Strict file permissions**: `.slb/` directory enforced at 0700, `state.db` and config files at 0600 ([`0e69925`](https://github.com/Dicklesworthstone/slb/commit/0e699254b0dfd6f35e4a37af3539c7930b62c7c3))
- **TOCTOU, injection, and regex vulnerability patches** ([`edb3e30`](https://github.com/Dicklesworthstone/slb/commit/edb3e3083858000c105c30f41d49007d0e333991))
- **Path traversal fix** in command normalization ([`01b2719`](https://github.com/Dicklesworthstone/slb/commit/01b2719bf469cc6775e7d47cac89144a45fd91c1))
- **Optimistic locking** on `UpdateRequestStatus` to fix race conditions in concurrent review ([`c6cda51`](https://github.com/Dicklesworthstone/slb/commit/c6cda514e440dfb1a13f7ab74a192ea09f4f8429))
- **Transactional review submission** for concurrency safety ([`20039ad`](https://github.com/Dicklesworthstone/slb/commit/20039ad37c91a9b8403fa08e42615a7f4adb8ac9))
- `shouldAutoApproveCaution` extracted as pure function to eliminate P0 security-critical side effects ([`621a9ce`](https://github.com/Dicklesworthstone/slb/commit/621a9ce6dbef333cf257445654ca7216121161e7))
- State machine hardened with strict transition validation ([`fa3918c`](https://github.com/Dicklesworthstone/slb/commit/fa3918ce8136a7fb895d9d26ef71787cd033e9de))
- Duplicate command hashing logic removed to prevent divergence ([`699c318`](https://github.com/Dicklesworthstone/slb/commit/699c318af3b2c391facd8d5cb6c2dc63e5aad0d7))

### Build & CI

- **Go module foundation** with Makefile build system ([`c2376d6`](https://github.com/Dicklesworthstone/slb/commit/c2376d60cdbee4e5061d9a7d426ed6862b70e5f3))
- **CI/CD pipeline** with security scanning via gosec and staticcheck ([`1a82a81`](https://github.com/Dicklesworthstone/slb/commit/1a82a815204d920f832805a91c43355df30c65eb))
- **GoReleaser** cross-platform binaries: Linux amd64/arm64 (tar.gz, deb, rpm, apk), macOS amd64/arm64 (tar.gz), Windows amd64 (zip), with SBOM generation and cosign signatures ([`cc17518`](https://github.com/Dicklesworthstone/slb/commit/cc17518fe7d699363f4bcb48670ed4a3bbc71127))
- **Codecov integration** with 80% coverage threshold, raised from initial 35% ([`101ef24`](https://github.com/Dicklesworthstone/slb/commit/101ef245812fff528a32b51b63ba88473c21476e), [`450dc6c`](https://github.com/Dicklesworthstone/slb/commit/450dc6cac0a0b2b442bc708cd62e63abe332cfbf))

### Testing

Extensive test coverage achieved across all packages:

- **Core**: 90.2% coverage ([`012584a`](https://github.com/Dicklesworthstone/slb/commit/012584ab6207e9a1bac6b23c254c3295368e5578))
- **CLI**: 87% coverage ([`4cd7c6a`](https://github.com/Dicklesworthstone/slb/commit/4cd7c6a22bba4df91c4b209be79eb04478326e7e))
- **Daemon**: 85% coverage ([`b7c65cb`](https://github.com/Dicklesworthstone/slb/commit/b7c65cbf4610314eabe1f08a429f1d1ac0564926))
- **TUI**: 97.6% coverage ([`6c45bae`](https://github.com/Dicklesworthstone/slb/commit/6c45bae2cd44231dfc1622cd2da61df0528c621e))
- **Watch command**: 90% coverage ([`23264c4`](https://github.com/Dicklesworthstone/slb/commit/23264c45faec129ec0acc8091add269a08865d3c))
- **Database patterns**: 90%+ coverage ([`c470dee`](https://github.com/Dicklesworthstone/slb/commit/c470dee3b0a5cfb18e3992a05b109ad257268bf1))
- **Testutil**: 91% coverage ([`9fcd437`](https://github.com/Dicklesworthstone/slb/commit/9fcd437b9f6ecf87820bf879b27d06e29cf5b3f4))
- **E2E harness**: 84.1% coverage ([`a3f0650`](https://github.com/Dicklesworthstone/slb/commit/a3f06503433b1cbee5ceafef50dea8c8efcb6c75))
- Test infrastructure foundation package (`internal/testutil`) with fixtures and helpers ([`966e90d`](https://github.com/Dicklesworthstone/slb/commit/966e90ddfb25484e4dcc7c0084ea09c5bb184303))
- E2E test suites: multi-agent approval workflow ([`561133f`](https://github.com/Dicklesworthstone/slb/commit/561133f6801b9be4aadd0b9547f2f3fa56972c25)), risk tier classification ([`f0b9eec`](https://github.com/Dicklesworthstone/slb/commit/f0b9eec18e22cbd609f455281645474081ad9f30)), session and timeout management ([`1b5d91c`](https://github.com/Dicklesworthstone/slb/commit/1b5d91cf334b1d033e8f1bdefef6f1d0cba4741e)), git and filesystem rollback ([`87834d1`](https://github.com/Dicklesworthstone/slb/commit/87834d102b812cc029c392b5ff34d1aaf88f00fb))
- Flaky test ID generation fixed ([`e9473e4`](https://github.com/Dicklesworthstone/slb/commit/e9473e43b80bf8de8fdbfcb3e8c08860a0c42f79))
- IPC server start/stop race conditions fixed ([`cb1b7fa`](https://github.com/Dicklesworthstone/slb/commit/cb1b7fafa5fe6ae6e9d2482d754ba5e480cbb598))
- Zombie process prevention in daemon tests ([`ba8cae3`](https://github.com/Dicklesworthstone/slb/commit/ba8cae3035db5dfae237ab337e868f5672c61c32))

### Other

- Git history audit trail and IDE integration packages ([`9921510`](https://github.com/Dicklesworthstone/slb/commit/9921510edf4b24d13a020f779ca5a725e4919e4e))
- Output formatting and utility packages ([`006b24a`](https://github.com/Dicklesworthstone/slb/commit/006b24a9dffecf7fcbeb58d602e203a6cc3da28f))
- Version command refactored to use structured output package ([`59b1b2f`](https://github.com/Dicklesworthstone/slb/commit/59b1b2f9dcdbe1e5882b88c86e345d86e286fc52))
- Context propagation in CLI run commands ([`b612262`](https://github.com/Dicklesworthstone/slb/commit/b612262120d3bbbc9510775592ca2f6d79f10585))
- Contribution policy documented in README ([`50917db`](https://github.com/Dicklesworthstone/slb/commit/50917db47a5a873e5a72d14e8db78b1593ce0c59))
- Comprehensive README documentation ([`7f08706`](https://github.com/Dicklesworthstone/slb/commit/7f0870626455e82a39095731ee1752dded0a643d))

---

## Pre-release Development -- 2025-12-13

The project was conceived, designed, and substantially built on 2025-12-13. The initial planning document (`PLAN_TO_MAKE_SLB.md`) went through rapid iteration to v2.0.0, incorporating atomic `slb run`, client-side execution, command hash binding, dynamic quorum, and improved SQL patterns.

- [`eca7c4a`](https://github.com/Dicklesworthstone/slb/commit/eca7c4a8edf322217f502eb808d3c2bb215a8b8f) -- Initial system documentation and AGENTS.md with multi-agent command authorization guidelines
- [`3cf711b`](https://github.com/Dicklesworthstone/slb/commit/3cf711b462bd40e545295161f0dfffb908cecfec) -- Initial planning transcript documenting key concepts, approval processes, and pattern management design
- [`f205128`](https://github.com/Dicklesworthstone/slb/commit/f2051282cba440d572e57293607183e804cd94bd) -- PLAN_TO_MAKE_SLB.md v2.0.0 with major design revisions

---

<!-- link definitions -->
[Unreleased]: https://github.com/Dicklesworthstone/slb/compare/v0.2.0...main
[v0.2.0]: https://github.com/Dicklesworthstone/slb/compare/v0.1.0...v0.2.0
[v0.1.0]: https://github.com/Dicklesworthstone/slb/releases/tag/v0.1.0
