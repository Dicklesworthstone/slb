# Changelog

All notable changes to [SLB (Simultaneous Launch Button)](https://github.com/Dicklesworthstone/slb) are documented in this file.

SLB is a cross-platform CLI that implements a **two-person rule** for running potentially destructive commands from AI coding agents. It provides risk-based command classification, peer review enforcement, and full audit logging for multi-agent workflows.

> Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). This project uses [Semantic Versioning](https://semver.org/).

---

## [v0.6.0] -- 2026-11-03

Compare: [`v0.5.2...v0.6.0`](https://github.com/Dicklesworthstone/slb/compare/v0.5.2...v0.6.0)

Minor release. Local peer approval now fails closed unless a project opts into same-host review ([#30](https://github.com/Dicklesworthstone/slb/issues/30), [#31](https://github.com/Dicklesworthstone/slb/issues/31)), session keys are stored only as digests, commands in subdirectories use their owning project, and the hook now blocks file-writing redirections and `rm -f` by default (they are CAUTION). Several defaults changed; read **Behavior changes** before upgrading.

### Behavior changes

- **Local approvals are refused by default** ([#30](https://github.com/Dicklesworthstone/slb/issues/30)): under the default `general.approval_policy = "independent"`, every approval entry point (CLI, TUI, daemon RPC, cross-project delegation) fails with `independent reviewer identity unavailable`, and execution of any request that needs approvals or a different model is refused, including requests approved before the upgrade. Session keys, agent names and model labels cannot show that a reviewer is a separate principal, and no external identity provider exists yet. SAFE commands, zero-review CAUTION auto-approval, rejections and `slb emergency-execute` still work; DANGEROUS and CRITICAL commands cannot pass review until a project opts in as below. `trusted_self_approve` no longer lets an agent approve its own request under either policy ([`e67c6e8`](https://github.com/Dicklesworthstone/slb/commit/e67c6e8acc02615297fa99bb72edfe5de3ef1f4f), [`a19f277`](https://github.com/Dicklesworthstone/slb/commit/a19f2776eab1f7caf1d0103b0dce155b4c1904fc))
- **Cooperating local agents opt in with `same_host`** ([#31](https://github.com/Dicklesworthstone/slb/issues/31)): `slb config set general.approval_policy same_host` restores local peer review as a mistake-prevention workflow, not a security boundary. Each request keeps the policy it was created under, and approving or executing it requires the current setting to match. Approvals recorded before the upgrade are kept as `legacy_local` evidence and authorize nothing under either policy: submit a new request after opting in. Switching back to `independent` stops outstanding same-host requests from executing. CLI and TUI output, hook responses, receipts, logs, notifications, watch events and history label same-host reviews as low assurance ([`a19f277`](https://github.com/Dicklesworthstone/slb/commit/a19f2776eab1f7caf1d0103b0dce155b4c1904fc), [`2ed2667`](https://github.com/Dicklesworthstone/slb/commit/2ed2667bc2e1ac031924a2c56c025aec4b4e9461), [`52302b7`](https://github.com/Dicklesworthstone/slb/commit/52302b7afad5c7de073093ebf6520887f6d3482d), [`b66790e`](https://github.com/Dicklesworthstone/slb/commit/b66790e93cd16817b9e8e37bbaae8fe847472355))
- **An approval can be withdrawn until execution is claimed**: another eligible reviewer may reject an approved request until its execution or authorization claim commits. Under the default `any_rejection_blocks` the request becomes `rejected`; `human_breaks_tie` escalates it and `first_wins` keeps the earlier approval. Execution applies the current conflict policy and the current approval TTL counted from the original approval time, so shortening the TTL revokes outstanding approvals. Adding a requester to `agents.blocked` revokes its outstanding approvals, and turning off `cross_project_reviews` or removing a reviewer from the target's `review_pool` blocks that reviewer's delegated approvals. `slb approve` and `slb reject` report the resulting `request_status` ([`59e3c58`](https://github.com/Dicklesworthstone/slb/commit/59e3c58857914360a2aa7b59c097c25d071c09dd), [`2093a2f`](https://github.com/Dicklesworthstone/slb/commit/2093a2f314ce618f90f5b07d1cfebd5a722ed406), [`990fb1e`](https://github.com/Dicklesworthstone/slb/commit/990fb1e707f7298019aa7e8ed745f16df8f9f1b0), [`14a4b32`](https://github.com/Dicklesworthstone/slb/commit/14a4b321d2c8719d3bb2ab8a389e25ecfd1900d5))
- **Session resume needs the original credentials** ([#30](https://github.com/Dicklesworthstone/slb/issues/30)): resuming an existing session requires `--session-id` and `--session-key`, also with `--force`; name, program and model are not credentials. slb cannot print a lost key: end the session and start a new one. Resuming with a changed `--model`, or a changed `--program` with `--force`, ends the old session and returns a new session ID and key that must be used from then on; this is refused while the session owns a pending, approved or escalated request ([`e0d30f1`](https://github.com/Dicklesworthstone/slb/commit/e0d30f14444da55f301d88fa55aaeeeab4163279), [`d191b38`](https://github.com/Dicklesworthstone/slb/commit/d191b380c282848924c6992db2ef8b355e6e890c), [`45d2340`](https://github.com/Dicklesworthstone/slb/commit/45d23402d1c747a9c7b5f0c6b43c59914a85b240))
- **Session keys are stored only as digests** ([#30](https://github.com/Dicklesworthstone/slb/issues/30)): the first open by this version migrates `state.db` from schema version 4 to 8. Each stored key is replaced by its SHA-256 digest, votes whose signatures still verify are re-signed, and the file is rebuilt with `VACUUM` so no old key bytes remain in it. Agents keep using the keys they hold. An older slb binary cannot authenticate the converted sessions. Copies or backups made before the upgrade still contain plaintext keys: end and restart those sessions if such a copy may have leaked ([`a007af3`](https://github.com/Dicklesworthstone/slb/commit/a007af30e35418cbf2d43d61bae008023b2ded42), [`ad8ecb5`](https://github.com/Dicklesworthstone/slb/commit/ad8ecb50a6d4c97f779aed8ddb2175847fc8052a), [`2952c4c`](https://github.com/Dicklesworthstone/slb/commit/2952c4c694ebbfd5b9c1e06e9a4bd670b0727a73))
- **Commands in subdirectories use the owning project**: CLI commands and hooks run anywhere below an initialized project use that project's state database, configuration, sessions and custom patterns. Discovery starts at `-C`/`--project`, then `SLB_PROJECT` (now honored by every command), then the current directory, and takes the nearest enclosing `.slb`, which must hold `state.db`. The command keeps its own working directory in the request binding, and default execution logs go to the owning project's `.slb/logs/`. A nested project initialized with `slb init` keeps its own `.slb`. A `.slb` without a state database is refused with an error, and the hook denies commands there, instead of the database being silently created; restore `state.db` or run `slb init` in that directory. An explicit `--db` still selects the database, and a directory with no `.slb` above it starts its own database as before. slb's own user directory `~/.slb` (user config, daemon log, hooks, audit records) is not a project unless it holds `state.db`; the generated fallback hook script `slb_guard.py` follows the same rule. In v0.5.2 CLI commands used the current directory's database, so a command run from `src/` created `src/.slb/state.db`; a `.slb` left in a subdirectory that way now marks a separate nested project, and if `~/.slb/state.db` exists, uninitialized directories under your home now use it; if a subdirectory should belong to its parent project, move its `.slb` out of the way ([`98b7cf7`](https://github.com/Dicklesworthstone/slb/commit/98b7cf7b2f8ba0b4e13f93e02da708c740c54986), [`8efb516`](https://github.com/Dicklesworthstone/slb/commit/8efb516398b760b51f54fda92d21b6b3ba36036f), [`e72ca5b`](https://github.com/Dicklesworthstone/slb/commit/e72ca5b2621cb83bb6118aeebd618f6a2058106a))
- **Stricter classification**:
  - Shell redirections that create, truncate or append to a file are at least CAUTION (`file_write_redirection`), even when the program is SAFE or unclassified: `echo hi > out.txt`, `git stash > notes.sql`, `: > f`, `cmd 2> err.log`, also inside `sh -c`, `eval`, pipelines and subshells. Writes to `/dev/null`, descriptor duplication or closing, and input redirection are unchanged ([`7f38f8f`](https://github.com/Dicklesworthstone/slb/commit/7f38f8f70501cbcaa9bb046bef0b76aa882d3b6b))
  - An `rm` that no SAFE rule matches is at least CAUTION, including `rm -f notes.txt` and `rm --force notes.txt`, which had no tier. The SAFE rules for `*.log`, `*.tmp` and `*.bak` targets are unchanged ([`48ecc45`](https://github.com/Dicklesworthstone/slb/commit/48ecc450940b753f531c949dd6c9d526c5342606))
  - A downloaded script run by a shell is DANGEROUS (was CAUTION `unresolved_execution`): `curl URL | bash`, `wget -qO- URL | sudo sh -s`, `bash <(curl URL)`, `sh -c "$(curl URL)"`, `source <(curl URL)` ([`723993b`](https://github.com/Dicklesworthstone/slb/commit/723993bb38913981668c3ee866af1329d4ca4243))

  With the default `integrations.hook_caution_action = "block"`, the hook now blocks file-writing redirections and `rm -f` and points to `slb request`. Set it to `"ask"` to be prompted instead.
- **Project cleanup deletions are less strict** ([#27](https://github.com/Dicklesworthstone/slb/issues/27)): CRITICAL deletion rules now require recursive intent and protect `/`, system directories and home roots rather than everything under `/home`. `rm -rf /home/<user>/project`, `rm -rf /Users/<user>/build` and `rm -rf ~/build` are DANGEROUS (were CRITICAL), and a non-recursive `rm -f /etc/<file>` or `rm -f ~/notes.txt` is CAUTION (was CRITICAL). `rm -rf ~`, `rm -rf /home/<user>`, `/Users/<user>`, `/root` and home-wide globs stay CRITICAL ([`48ecc45`](https://github.com/Dicklesworthstone/slb/commit/48ecc450940b753f531c949dd6c9d526c5342606))
- **Context attachments run only SAFE commands**: `--attach-context` on `slb request` and `slb run` runs a context command only when it classifies entirely as SAFE under current policy for an active requester session in the project; otherwise the whole batch is refused before any context command starts. Even `pwd` needs a matching SAFE rule. For anything else, run the command through `slb run` first and attach its saved output with `--attach-file` ([`479dcf2`](https://github.com/Dicklesworthstone/slb/commit/479dcf2cc393ded360ee12e5a0e76b155d1e1922))
- **Hook installation is per project by default**: `slb hook install`, `status` and `uninstall` act on the selected project's `.claude/settings.json` (earlier versions always wrote `~/.claude/settings.json`); `--global` selects user settings. Project installation requires an initialized project, so run `slb init` first. An existing user-level installation is left in place: inspect it with `slb hook status --global` and remove it with `slb hook uninstall --global` if you move to project hooks. `slb integrations claude-hooks` now uses the same installer: by default it prints a `PreToolUse` settings fragment, and `--install` installs, instead of printing `hooks.json` content. Registrations set `onFailure: "block"`, which needs Claude Code 2.1.295 or later to block a tool call when the hook cannot start, fails or times out ([`491607b`](https://github.com/Dicklesworthstone/slb/commit/491607bc5298adb456b59205055ef0e7a71248c1))
- **`slb watch` and `slb history` output changed**: watch reports a failed execution as `request_execution_failed` and an execution timeout as `request_timed_out` (polling reported both as `request_executed`), adds `request_executing` and `request_escalated`, and carries vote counts, approval policy and assurance. `created_at` is now the request's creation time; the time the state was observed is in `observed_at`. Consumers should key state by request ID. When `SLB_HOST` selects a remote daemon, a lost connection is an error rather than a fallback to local records. `slb history` shows only the selected project's requests even in a shared database, applies filters before `--limit`, and rejects invalid filter values ([`5167e9c`](https://github.com/Dicklesworthstone/slb/commit/5167e9cff9d58cb349a147426c0c5916ec98bbcd), [`52302b7`](https://github.com/Dicklesworthstone/slb/commit/52302b7afad5c7de073093ebf6520887f6d3482d))
- **Daemon lifecycle is per project** ([#29](https://github.com/Dicklesworthstone/slb/issues/29)): `slb daemon stop` does not signal a daemon started by an earlier version from the shared per-user PID file. Stop that daemon through its original session or process manager, then start the new one. `slb daemon start`, `stop` and `status` manage the local project daemon and ignore `SLB_HOST`. Leave the `.pid.lock` and `.sock.lock` files in place; they coordinate ownership across restarts ([`160c016`](https://github.com/Dicklesworthstone/slb/commit/160c0166cef6233171df18aad3884dc6604eb7e9))
- **Kubernetes rollback capture is bound to the target cluster**: rollback capture is on by default, and a failed capture stops execution. Executing `kubectl delete` through slb now needs named targets (`type/name` or `type name`) and a file-backed kubeconfig; if `~/.kube/kuberc` exists, set `KUBECTL_KUBERC=false`. Selectors, `--all`, all-namespaces, file or stdin selections and unsupported connection or authentication overrides are refused instead of backing up a subset or the current context. `slb rollback` creates the captured resources and needs `--force` to apply over existing ones; manifests captured by earlier versions are not restored automatically ([`f65cf97`](https://github.com/Dicklesworthstone/slb/commit/f65cf97de9d2ccc6228b318ab6fb2075f1183fa6))
- **`slb emergency-execute` exits with the command's status**: a nonzero child exit makes slb exit nonzero, and a command that never starts reports `executed: false` and `exit_code: -1`. Output and records are labeled `emergency_unreviewed` ([`0bf65c9`](https://github.com/Dicklesworthstone/slb/commit/0bf65c91635b11c16c26e9f1adb090e864afbfeb), [`23b1c1d`](https://github.com/Dicklesworthstone/slb/commit/23b1c1dc1af42c52095af3db1b649580cf8a35c1))
- **Building from source needs Go 1.26.0 or newer** (`mvdan.cc/sh/v3` v3.14.1 declares `go 1.26.0`). Release binaries are built with Go 1.26.10 ([`4391e6b`](https://github.com/Dicklesworthstone/slb/commit/4391e6b601994eeccaef4a75aac812dd16a9eed9))
- **Custom pattern lists**: a config file that sets `patterns.<tier>.patterns` replaces that built-in list rather than extending it. If yours does, add the new deletion, remote-script and `+refspec` rules from `internal/config/defaults.go` yourself
- **Error messages from rollback, dry-run and the Git history journal now start lowercase (Go style)** ([`2c908f3`](https://github.com/Dicklesworthstone/slb/commit/2c908f3364f198148ed47ccfd98243f34ac6b717))

### Same-host review and delegation

- Same-host reviews use version 2 signatures in their own domain, bound to the command, requester, quorum, model rule and justification. Execution rechecks signatures, expiry, request binding and current policy while claiming the request. The requester cannot review its own request, including through a restarted session with the same agent name ([`a19f277`](https://github.com/Dicklesworthstone/slb/commit/a19f2776eab1f7caf1d0103b0dce155b4c1904fc))
- Review submission reloads the project policy after waiting for the database writer, and an open TUI review form stays bound to the policy it displayed. Current conflict resolution applies before SAFE shortcuts and native Git hook authorization ([`2ed2667`](https://github.com/Dicklesworthstone/slb/commit/2ed2667bc2e1ac031924a2c56c025aec4b4e9461))
- When a different model is required, requester and reviewer need nonblank model labels that differ ignoring case and surrounding whitespace, checked at submission, reviewer availability and execution. An expired request that needs a different model now resolves through the normal expiry policy instead of staying pending ([`dd76b00`](https://github.com/Dicklesworthstone/slb/commit/dd76b00a4c3cf8568b61fd851548f3605a041770), [`8317f78`](https://github.com/Dicklesworthstone/slb/commit/8317f784480ad7732d242e624ff3bfd775685e71))
- Ordinary votes from a session of another project are refused even when projects share a database through `--db`; cross-project review goes through `--target-project`. A delegated source session is reauthenticated after waiting for the target's writer, and delegated provenance is recorded per target project, so one reviewer can review several projects that share a database ([`14a4b32`](https://github.com/Dicklesworthstone/slb/commit/14a4b321d2c8719d3bb2ab8a389e25ecfd1900d5), [`c079ca0`](https://github.com/Dicklesworthstone/slb/commit/c079ca0d12c6a4b225500250a039bac2559b2ea3), [`9b7a5f2`](https://github.com/Dicklesworthstone/slb/commit/9b7a5f2620b8620b48441742039094a35cc61036))
- Escalated requests appear in `slb pending` and `slb review list` with their status, keeping their original quorum and model requirements. The requester can withdraw a pending, approved or escalated request with `slb cancel`; cancellation cannot overwrite an executing or finished request ([`318da12`](https://github.com/Dicklesworthstone/slb/commit/318da12880164ddff725af4bce1d0680db4160ee))
- TUI reviewers can object to an approved request until it is claimed, and review notifications describe the committed request state ([`2093a2f`](https://github.com/Dicklesworthstone/slb/commit/2093a2f314ce618f90f5b07d1cfebd5a722ed406))

### Classification

- `git push origin +main` and `+refs/...` refspecs are CRITICAL like `--force`. `xargs` is unwrapped like other execution wrappers, so `xargs rm -rf` is classified (it had no tier), and a command run by `xargs` is never SAFE. A subshell followed by redirections is classified by its content: `(rm -rf /etc) >/dev/null` is CRITICAL (was a CAUTION parse error) ([`723993b`](https://github.com/Dicklesworthstone/slb/commit/723993bb38913981668c3ee866af1329d4ca4243))
- Deletion rules recognize every spelling of recursive intent: `-r`/`-R` bundled with other flags, `--recursive` and GNU abbreviations from `--r` to `--recursiv`, and options after operands. Every operand is checked, so `rm -rf build /etc` is CRITICAL. Root wipes spelled with `?`, bracket or dot globs and `/./` or `/../` aliases, home-wide globs (`~/*`, `~/**`, `~/.*`) and `$HOME`/`${HOME}` operands stay CRITICAL. A quoted filename containing `--` can no longer hide a later recursive flag. Home roots under `/Users` are protected on macOS as `/home` is on Linux ([#27](https://github.com/Dicklesworthstone/slb/issues/27), [`48ecc45`](https://github.com/Dicklesworthstone/slb/commit/48ecc450940b753f531c949dd6c9d526c5342606), [`6a9e263`](https://github.com/Dicklesworthstone/slb/commit/6a9e263e847f6e5913f7152f2094c0156db2b608), [`d839a29`](https://github.com/Dicklesworthstone/slb/commit/d839a29c6daeab447c2b295ef649c4173dc01dca), [`0293310`](https://github.com/Dicklesworthstone/slb/commit/0293310dff831e201336b7b9f1d9d23aca036ac9), [`48145d0`](https://github.com/Dicklesworthstone/slb/commit/48145d07285f94ce32e2db8c9df1dc8e776caa94), [`add95f3`](https://github.com/Dicklesworthstone/slb/commit/add95f3bdd81ad157f7349a9bab14ece606ce915), [`9c08f3b`](https://github.com/Dicklesworthstone/slb/commit/9c08f3bc381c908824f12965ed7aefc54b864b6a), [`723993b`](https://github.com/Dicklesworthstone/slb/commit/723993bb38913981668c3ee866af1329d4ca4243))
- Shell rebinding ([#23](https://github.com/Dicklesworthstone/slb/issues/23)): `printf '%s\n' "$x"` anywhere in a script no longer turns off literal-variable resolution for the rest of it (later lines became CAUTION `parse_error`). Alias definitions are classified, and an alias use is classified as its body plus the remaining words: `alias p='rm -rf /srv'` followed by `p` had no tier. `hash -p`, `BASH_ALIASES`, `BASH_CMDS`, computed name operands of `printf -v`, `read`, `declare` and similar builtins, and command substitutions in array subscripts are at least CAUTION ([`0887f9f`](https://github.com/Dicklesworthstone/slb/commit/0887f9f688895949569318b28d19263d32473e85), [`9d2b222`](https://github.com/Dicklesworthstone/slb/commit/9d2b222793c3937bff6168421aee7c20060393d6), [`23675c7`](https://github.com/Dicklesworthstone/slb/commit/23675c7067e78ab3ab4891aac066319df5e7053d))

### Hooks

- Hooks resolve symlinked project paths before choosing policy, state database and query deadline, validate execution handoffs against that physical database, and refuse an initialized project whose state is unavailable ([`3a7e89d`](https://github.com/Dicklesworthstone/slb/commit/3a7e89d7cbfa2be11f8d3f10b31a275c390871f1))
- A hook installed with `--config` keeps that absolute file and its owning project through offline classification, daemon queries and execution handoffs; the daemon must be started with the same file, and a missing or invalid selected file blocks the action. Notifications, background execution and the daemon also keep an explicit `--config`, and enabling `[notifications.requests]` now also stops legacy CLI Agent Mail sends from interactive reviews, native Git requests, detached execution and `--config` selections ([`318da12`](https://github.com/Dicklesworthstone/slb/commit/318da12880164ddff725af4bce1d0680db4160ee), [`413b6f7`](https://github.com/Dicklesworthstone/slb/commit/413b6f78938b173d1fa62ffb14b6863a04a93757))
- Upgrade and uninstall recognize SLB's own native and Python guard commands, including Claude Code exec-form `command`/`args` registrations, instead of matching file names, and leave other hooks and command lists untouched ([`20c4311`](https://github.com/Dicklesworthstone/slb/commit/20c43115480475f61ab6aeaf5d6d4cc3877b7f59))
- The standalone Python fallback hook allows a SAFE match automatically only for a literal simple command; compound commands, expansions, redirections, wrappers, interpreters, `eval`, assignments and `xargs` forms now require an explicit decision. In that hook, commands longer than 16,384 characters classify CRITICAL instead of taking up to about 44 seconds ([`75a5af2`](https://github.com/Dicklesworthstone/slb/commit/75a5af2c8b061621525b0534aba85567abbea534), [`723993b`](https://github.com/Dicklesworthstone/slb/commit/723993bb38913981668c3ee866af1329d4ca4243))

### Requests and execution

- Execution policy, pending auto-approval timers and executor prechecks classify what will run (the script for shell commands, the exact argv for direct commands), not the display text. A direct command that does not tokenize completely (unbalanced quotes, empty program, NUL bytes) is rejected at admission instead of approving its parsed prefix. The working directory is captured at admission and bound into the command hash; a relative working directory is rejected, and a stored request without an absolute one must be resubmitted ([`45e46a0`](https://github.com/Dicklesworthstone/slb/commit/45e46a060e409baa847444ce773ab324c4e0c9d0), [`699e961`](https://github.com/Dicklesworthstone/slb/commit/699e96143096cf312c37ce5eb21c1dd4f099c186), [`461116f`](https://github.com/Dicklesworthstone/slb/commit/461116fe994e52ea72b0a890128e36dbcc270b44), [`7d86781`](https://github.com/Dicklesworthstone/slb/commit/7d867815c247cd6319d10cb1342af9e822dd491a))
- A request from a session of another project, or with no project, is refused before a SAFE result or a dry-run preview runs ([`3b7198c`](https://github.com/Dicklesworthstone/slb/commit/3b7198c7e1749f88fbbd8ea9e3a46e2199a60d62))
- `slb run` rechecks the session (active, same project, not blocked) and current policy immediately before launching a SAFE command. SAFE run logs get unique private file names, so concurrent runs no longer truncate each other's logs ([`9f0b59e`](https://github.com/Dicklesworthstone/slb/commit/9f0b59ed321484242ece7d3e2a753df9cf827276), [`d1594f2`](https://github.com/Dicklesworthstone/slb/commit/d1594f2dab719d04f4d72a67836b43902fb9b9b8))
- A claimed execution that fails to start consumes its approval instead of returning the request to `approved`. An executor can retry an identical completion report after a lost reply and gets the original committed result, without a second audit event ([`2908834`](https://github.com/Dicklesworthstone/slb/commit/290883453c851a1b33043c3b6e785ccf25880832), [`dd76b00`](https://github.com/Dicklesworthstone/slb/commit/dd76b00a4c3cf8568b61fd851548f3605a041770))
- Without a daemon, a requester's expired pending requests no longer hold `max_pending_per_session` indefinitely: admission resolves them through the timeout policy first. `general.request_timeout` is honored in seconds (a 2-second timeout used to become 60 seconds). A queued submission cancelled during a database retry keeps its rate-limit error ([`87a112e`](https://github.com/Dicklesworthstone/slb/commit/87a112e2f0eefb72e7abd326bddc78065ca736fa), [`3339da4`](https://github.com/Dicklesworthstone/slb/commit/3339da49d9c36080c883aa3a7b96d9552f679e08))
- `slb -C <relative dir>` resolved the directory twice: run from `parent/`, `slb -C sub session start` opened `parent/sub/sub/.slb/state.db` and recorded `sub` as the project. The flag is now made absolute before slb changes into it ([`0ba1631`](https://github.com/Dicklesworthstone/slb/commit/0ba1631f33d9ad35d7649624b0d66161d0d4cec2))

### Rollback and previews

- Each execution attempt captures fresh rollback state, committed together with the winning execution claim, so a refused or competing attempt cannot replace that snapshot ([`f5160d0`](https://github.com/Dicklesworthstone/slb/commit/f5160d0c4a3049dc60e65375f5f3455fbf4be996))
- Request-based rollback requires the request's project and checks the snapshot's request ID, project, command and working directory; new metadata also binds the full command hash. Filesystem recovery validates the whole archive and every destination path (gzip integrity, unsupported entries, symlinked ancestors) before changing files. `--force` waives none of these checks ([`6b80c66`](https://github.com/Dicklesworthstone/slb/commit/6b80c66d9d95c6f9e7003e9e2df435cb7ed684cf), [`484479b`](https://github.com/Dicklesworthstone/slb/commit/484479b6590aefd4c6c9f13d7a3679efa0bb95d2))
- Kubernetes snapshots are private JSON manifests bound to the context, namespace, API server and kubeconfig fingerprints of the delete; restore validates the complete set and unchanged configuration first. Earlier versions could back up from the current context rather than the one the command named ([`f65cf97`](https://github.com/Dicklesworthstone/slb/commit/f65cf97de9d2ccc6228b318ab6fb2075f1183fa6))
- `git reset` previews show the ref, index and working-tree changes for the requested reset mode instead of a HEAD-to-HEAD diff, and run with external diff, text conversion and fsmonitor programs disabled. Previews of direct commands see the same operands as execution ([`b889118`](https://github.com/Dicklesworthstone/slb/commit/b889118fc88555c968b4dfeb77ec9c4018688996), [`3094b47`](https://github.com/Dicklesworthstone/slb/commit/3094b47f14d2eaf73ec48936595733b81146b52d))

### Emergency execution

- `emergency-execute` records an attempt in a separate emergency ledger before launching, then the observed start and outcome; an attempt whose outcome was never recorded stays `incomplete`. Each run writes its own private log. `slb audit emergency` lists and exports these records for the selected project, with `--id`, `--status`, `--actor`, `--query`, `--since`, `--before` and `--limit` filters ([`23b1c1d`](https://github.com/Dicklesworthstone/slb/commit/23b1c1dc1af42c52095af3db1b649580cf8a35c1), [`0bf65c9`](https://github.com/Dicklesworthstone/slb/commit/0bf65c91635b11c16c26e9f1adb090e864afbfeb))

### Watch, history and notifications

- `slb history sync` exports the request journal's metadata (no command text, evidence, keys or signatures) to a dedicated ref in a local Git repository, one resumable page at a time. With `history.git_repo_path` set and `history.auto_git_commit = true`, the project daemon exports continuously and catches up after downtime; these settings had no effect before. Nothing is pushed to a remote ([`0eab88c`](https://github.com/Dicklesworthstone/slb/commit/0eab88ca1c506587dc5b692a464cb1892db7b995))
- `slb watch` tracks requests it has seen through their final state across daemon disconnects and pins the daemon's project database for its polling fallback ([`5167e9c`](https://github.com/Dicklesworthstone/slb/commit/5167e9cff9d58cb349a147426c0c5916ec98bbcd))
- Pending-request desktop and webhook alerts are tracked per destination and retried with capped backoff; a failing helper can no longer hold up daemon shutdown. `slb --db <path> tui` uses the selected database in every view and refuses a missing one instead of creating it ([`b839cf1`](https://github.com/Dicklesworthstone/slb/commit/b839cf14f924db520fb3f3ef845a3b3298f44639))
- The README no longer documents `slb notify`, which the CLI never had, and describes the two Agent Mail routes that exist ([#28](https://github.com/Dicklesworthstone/slb/issues/28), [`291a236`](https://github.com/Dicklesworthstone/slb/commit/291a236a1e6e765d0b3d24b4b533eede44df53a3))

### Daemon, storage and performance

- Hook queries reuse each project's compiled policy until its config files or custom rules change: about 27 µs and 16 KB per query, down from 2.6 ms and 800 KB (benchmark: quiet project, SAFE command), so garbage-collection pauses no longer push answers past `integrations.hook_query_timeout_ms` ([#24](https://github.com/Dicklesworthstone/slb/issues/24), [`fe8e229`](https://github.com/Dicklesworthstone/slb/commit/fe8e22924d54d3c9956edf7ae6a981414b53fb09))
- Each project's daemon has its own PID file and socket, resolved the same way for `-C`, `SLB_PROJECT`, nested directories and symlinked paths. Concurrent starts and cleanup are fenced, `start` reports success once the daemon is ready, and `stop` refuses another project's daemon ([#29](https://github.com/Dicklesworthstone/slb/issues/29), [`160c016`](https://github.com/Dicklesworthstone/slb/commit/160c0166cef6233171df18aad3884dc6604eb7e9), [`e7fbcf9`](https://github.com/Dicklesworthstone/slb/commit/e7fbcf91d82df50c44380aecfd013e76c40025ba))
- A project path containing `#`, `%` or `?` could open a different SQLite file or change its connection options, defeating read-only opens. Paths are now passed as escaped file URIs, and a missing database is no longer recreated implicitly ([`44d9f1d`](https://github.com/Dicklesworthstone/slb/commit/44d9f1d06f1c510016eb355a3ccbf7fa824002c2))

### Installer and packaging

- The install script resolves one stable release tag, verifies the archive against that release's `checksums.txt` (it needs `sha256sum` or `shasum`), installs `slb.exe` on Windows and checks the installed file by running it. It keeps its download workspace and prints the path; `SLB_RETAIN_WORK_ROOT` selects a fresh location. It sends a User-Agent that names the installer. The README lists the real archive names ([`c45da04`](https://github.com/Dicklesworthstone/slb/commit/c45da04900af1e4f5c9249ef7dacc939caa604d1), [`5b17065`](https://github.com/Dicklesworthstone/slb/commit/5b1706579d17eac8ae390d6e54f53f7a7b131431))
- Release archives include `LICENSE` ([`0da5d70`](https://github.com/Dicklesworthstone/slb/commit/0da5d70991fe1539327bdba3dcc0d872503ea978))

### Fixes

- Database close errors and warning-output write failures are now reported instead of being silently ignored ([`fe9a217`](https://github.com/Dicklesworthstone/slb/commit/fe9a217206afbca04d07e581be980996a17c0b3e))
- `slb events --follow` no longer fails with "transaction has already been committed or rolled back" when interrupted or timed out during a read; it now returns the cancellation or deadline. The daemon likewise stops cleanly when a shutdown signal lands during its startup state read ([`f47402a`](https://github.com/Dicklesworthstone/slb/commit/f47402a29689b20cf59fc4fb2f8903a10d348da3))
- `--output toon` (and `-t`) works with the current toon_rust CLI (crate `tru`, binary `toon`); before, slb looked only for `tru` and silently fell back to JSON. The install hint named an unrelated crate ([`5e74c42`](https://github.com/Dicklesworthstone/slb/commit/5e74c42f984249fa2fb8338715b4f3076d60c798))

### Dependencies

- `mvdan.cc/sh/v3` v3.10.0 → v3.14.1, which declares `go 1.26.0`; the module's `go` line rises from 1.24.13 to 1.26.0. Classifier check ([#26](https://github.com/Dicklesworthstone/slb/issues/26)) over 15,169 test inputs: 9 changed and none became less strict ([`4391e6b`](https://github.com/Dicklesworthstone/slb/commit/4391e6b601994eeccaef4a75aac812dd16a9eed9))
- sh v3.13 and later walk parameter-slice offsets themselves, so slb no longer walks them a second time. `${x:$(...)}` forms classify as before the update; the only remaining differences are 3 English strings beginning with `function`, which no longer parse as shell and get CAUTION instead of no tier ([`2dae1eb`](https://github.com/Dicklesworthstone/slb/commit/2dae1ebdc01c24ca1cfd82497b7a509874902782))
- `github.com/mattn/go-shellwords` v1.0.12 → v1.0.16: escapes inside double quotes are kept in normalized text; 7 corpus inputs changed only in that text, with identical tiers ([`68ee5e9`](https://github.com/Dicklesworthstone/slb/commit/68ee5e9e1f7565e15c48d3d78d6191d68f3b0d2b))
- Updates within current major versions, including `fsnotify` v1.10.1, `go.yaml.in/yaml/v3` v3.0.6, `golang.org/x/sys` v0.49.0, `golang.org/x/term` v0.47.0 and `modernc.org/sqlite` v1.61.1; no corpus input changed ([`468312d`](https://github.com/Dicklesworthstone/slb/commit/468312d1c3030359fd94c75b284b85efc456aa62))
- `charmbracelet/bubbles` and `charmbracelet/log` v1.0.0, with no source changes ([`b28b36a`](https://github.com/Dicklesworthstone/slb/commit/b28b36ad430c851373a7c9ec0e9e37fe1c6bdb45))

---

## [v0.5.2] -- 2026-09-25

Compare: [`v0.5.1...v0.5.2`](https://github.com/Dicklesworthstone/slb/compare/v0.5.1...v0.5.2)

Safety patch release: commands hidden behind variables, stdin feeds and `eval` are classified by what they run. **Classification is stricter**; see below.

### Classification

- A command word built from a provably literal script-local variable is classified as the resolved command: `X=rm; $X -rf /srv` is CRITICAL, as is `X=/srv; rm -rf $X` ([`047b97f`](https://github.com/Dicklesworthstone/slb/commit/047b97ffe083a76dff78c4fbf7b799748ece1452))
- A shell reading its program from stdin gets that program classified when it is statically known, recursively: here-strings, here-docs, pipes from `echo`/`printf`/`cat`/`base64 -d`, including inside loops and subshells (`bash <<< "..."`, `echo "..." | bash`, `base64 -d <<< ... | sh`, `for s in a; do cat <<< "..." | bash; done`) ([`047b97f`](https://github.com/Dicklesworthstone/slb/commit/047b97ffe083a76dff78c4fbf7b799748ece1452))
- A shell `-c` operand that is a single command substitution with a statically known result is classified (`sh -c "$(printf '...')"`); `eval` with literal arguments is classified as the code it runs ([`047b97f`](https://github.com/Dicklesworthstone/slb/commit/047b97ffe083a76dff78c4fbf7b799748ece1452))

### Stricter classification (behavior change)

Code whose content cannot be determined statically is now at least CAUTION (`unresolved_execution`) instead of "no pattern". With the default `hook_caution_action = "block"` the hook blocks these and points to `slb request`; an explicit `slb request` escalates them to DANGEROUS like any unmatched command, and they are never auto-approved. This covers:

- a computed command word that is not a script-local literal: `"$CMD" ...`, `$(which x) ...`, and variables derived from the environment such as `TC=$HOME/.rustup/...; $TC/bin/cargo ...`
- shells or interpreters fed an unknown program on stdin: `curl ... | bash`, `bash < script.sh`, `bash <(curl ...)`, `sh -c "$VAR ..."`, `eval "$X"`
- **every non-shell interpreter reading its program from stdin**, whatever the payload: `python3 - <<'EOF' ... EOF`, `python - <<< "..."`, `echo ... | perl`, `node -`. Shell risk patterns cannot judge those programs. Scripts run from a file (`python3 script.py`) and inline `-c`/`-e` code are unchanged.

Measured on 25,000 recorded agent commands: 1,621 (6.5%) change from no tier to CAUTION, about 1,150 of them stdin interpreter programs (mostly `python3 - <<'PY'` edits) and about 420 `$HOME`-derived command words. Set `integrations.hook_caution_action = "ask"` to be prompted instead of blocked.

---

## [v0.5.1] -- 2026-09-25

Compare: [`v0.5.0...v0.5.1`](https://github.com/Dicklesworthstone/slb/compare/v0.5.0...v0.5.1)

Safety patch release: closes classifier bypasses reported in [#22](https://github.com/Dicklesworthstone/slb/issues/22) (thanks to @JYeswak).

### Classification

- `TRUNCATE` without the optional `TABLE` keyword is CRITICAL when it appears in a SQL context: a SQL client's command line (`psql -c 'TRUNCATE users'`, glued `-c'...'`, `psql.exe`), input fed to a client (here-strings, pipes, heredoc bodies), a bare statement following the TRUNCATE grammar, or a `;`-terminated statement. The coreutil `truncate -s 0 f` and plain searches such as `rg truncate src` stay unclassified. The exported Python hook carries the same rules ([`b1461b8`](https://github.com/Dicklesworthstone/slb/commit/b1461b8a1f38637df119effd8ed39607dffade3c), [`b008758`](https://github.com/Dicklesworthstone/slb/commit/b008758d685082bc71890e00557f819a08a22b58))
- Shell operators inside quoted arguments no longer hide the rest of the command: `psql -c 'SELECT 1; DROP DATABASE prod'` is classified by its full text ([`b008758`](https://github.com/Dicklesworthstone/slb/commit/b008758d685082bc71890e00557f819a08a22b58))
- Redirections no longer hide arguments: `git push >/dev/null --force origin main`, `</dev/null git push --force ...`, `psql 2>&1 -c 'DROP DATABASE prod'`, `>|` and named fds (`{fd}>f`) are classified by the command's real argv ([`b008758`](https://github.com/Dicklesworthstone/slb/commit/b008758d685082bc71890e00557f819a08a22b58), [`5f59f44`](https://github.com/Dicklesworthstone/slb/commit/5f59f44da6e352b2d7623cbfd650143f9a58f44d))
- A here-string's word is no longer spliced into the argument list, so `kubectl delete <<<'pod x' namespace prod` and `rm <<<'-f a.log' -rf /etc` cannot pose as allowlisted commands ([`5f59f44`](https://github.com/Dicklesworthstone/slb/commit/5f59f44da6e352b2d7623cbfd650143f9a58f44d))
- A number glued to `&>`/`&>>` stays an argument (bash's `&>` takes no fd), so `rm -f 1&>/dev/null a.log` is no longer read as the log-only `rm -f a.log` ([`fb5ae4c`](https://github.com/Dicklesworthstone/slb/commit/fb5ae4c0c9c48b5097d28a29f54785d0290c3b15))
- Redirection stripping is linear (a 1 MB command classified in ~4 minutes, now 0.1 s), and the bare TRUNCATE rule no longer backtracks quadratically in the exported Python hook ([`5f59f44`](https://github.com/Dicklesworthstone/slb/commit/5f59f44da6e352b2d7623cbfd650143f9a58f44d))

### Upgrade note

A config file that sets `patterns.critical.patterns` replaces the built-in CRITICAL list rather than extending it. If yours does, add the new TRUNCATE rules from `internal/config/defaults.go` yourself.

---

## [v0.5.0] -- 2026-09-24

Compare: [`v0.4.1...v0.5.0`](https://github.com/Dicklesworthstone/slb/compare/v0.4.1...v0.5.0)

Minor release: several defaults changed (see **Behavior changes**), and approvals, execution and hooks were rebuilt around signed, single-use, atomically verified evidence.

### Behavior changes

- **CAUTION stays inside SLB by default**: the hook now blocks CAUTION commands and points to `slb request` instead of raising a Claude Code permission prompt. The old interactive behavior is `integrations.hook_caution_action = "ask"` ([`f37f6ee`](https://github.com/Dicklesworthstone/slb/commit/f37f6ee08327c052e14c33e65db81d13c44246a2))
- **Native hook replaces the Python guard**: `slb hook install` installs a native PreToolUse guard and migrates legacy Python registrations automatically; the generated Python script remains the offline fallback ([`5a4923e`](https://github.com/Dicklesworthstone/slb/commit/5a4923e19efe3ff750fc428dd3aafc3c88a1bad8), [`0c18ce5`](https://github.com/Dicklesworthstone/slb/commit/0c18ce5974a33e4c3f32f202ae83ab82e0491b38))
- **Automatic CAUTION approval is a policy decision, not a review**: `slb watch --auto-approve-caution` and daemonless waiters approve only due, zero-quorum CAUTION requests (default delay 30s) from an active requester; no reviewer record is fabricated ([`cf22a99`](https://github.com/Dicklesworthstone/slb/commit/cf22a99847857c8d42c595e1833258cfe0ad3b73), [`f8e5c7c`](https://github.com/Dicklesworthstone/slb/commit/f8e5c7c23f10b4b7b6c950179d8b906529e71e03))
- **Status alone is not an approval**: execution re-verifies signed reviewer evidence, quorum, TTL and current policy inside a single-use claim; the executor session must belong to the request's project ([`1ffd2d7`](https://github.com/Dicklesworthstone/slb/commit/1ffd2d7bf0a7f77dd2a36999487afe47acba3741), [`f4fb378`](https://github.com/Dicklesworthstone/slb/commit/f4fb378c5fc1a4b153c304b5ef3050d1da56f374), [`d25815f`](https://github.com/Dicklesworthstone/slb/commit/d25815f8864c4061db4c12c100e157f0ad597198))
- **Unloadable policy blocks**: a broken live policy or offline classifier is a hard denial, not a prompt ([`b084d38`](https://github.com/Dicklesworthstone/slb/commit/b084d388d430d2773ba7bee80fb3e71b5d2f364d), [`7672955`](https://github.com/Dicklesworthstone/slb/commit/76729550cd56d9c5986942b9628ead3933079c8f))
- **Explicit `SLB_HOST` is authoritative**: a failing TCP target no longer falls back to the local Unix daemon ([`f95ab24`](https://github.com/Dicklesworthstone/slb/commit/f95ab24c7590db370d6af59d2fc99caec75f3f7e))
- **Native Git operations need approval**: destructive Git changes and native rebases are assessed against immutable snapshots and require signed single-use authorization via installable Git hooks ([`d67767d`](https://github.com/Dicklesworthstone/slb/commit/d67767d5f81591dcae9ff723e4ea214739277134), [`8c035be`](https://github.com/Dicklesworthstone/slb/commit/8c035be4e97ec024335a79542730b333afe43a73), [`7fb32b7`](https://github.com/Dicklesworthstone/slb/commit/7fb32b73336320595a950cfde9e14aa94927db5a), [`28a731b`](https://github.com/Dicklesworthstone/slb/commit/28a731b283d5adf17c92c7ce4568ee83d1b763c0), [`c7eb3cb`](https://github.com/Dicklesworthstone/slb/commit/c7eb3cb9a5f0ac3e2747b4d3869cf7a8d0f5ea4b))

### Classification

- Ordinary shell syntax no longer becomes an approval prompt ([#14](https://github.com/Dicklesworthstone/slb/issues/14)), and case statements, heredocs and shell control flow are parsed through the AST without weakening checks ([#16](https://github.com/Dicklesworthstone/slb/issues/16), [`f8603e6`](https://github.com/Dicklesworthstone/slb/commit/f8603e63c2ba6fffcc8cb6103f886af319760e48), [`4d726da`](https://github.com/Dicklesworthstone/slb/commit/4d726dac5eefced1620702b8d07dab6582dfbe24), [`c5103df`](https://github.com/Dicklesworthstone/slb/commit/c5103df3f8c4a281ca67c7d9bfe59eb770b5a53d), [`f9e3642`](https://github.com/Dicklesworthstone/slb/commit/f9e36425dd9fc683173c16a1af6d908dd563f3c4))
- Script-local literal variables and data here-strings in control flow are resolved when provably literal, which also exposes danger hidden behind a variable ([#20](https://github.com/Dicklesworthstone/slb/issues/20), [`e11a01a`](https://github.com/Dicklesworthstone/slb/commit/e11a01aa12cb5647230476186bf63bf6d52e193a), [`de2688b`](https://github.com/Dicklesworthstone/slb/commit/de2688b732027f1e9877068a4b6e875fe235439c))
- `((X))` is classified by what POSIX sh runs (the nested subshells `( (X) )`), so a destructive command that is also valid arithmetic can no longer pass unclassified ([`751ee81`](https://github.com/Dicklesworthstone/slb/commit/751ee819a1fb10a155c5346cf4a9ab59e5e63cd5))
- Executable substitutions, complete shell bodies and execution wrappers are inspected before allowing a command ([`13e9841`](https://github.com/Dicklesworthstone/slb/commit/13e98411c679661761095aeac70c615234cddabe), [`fccc8e3`](https://github.com/Dicklesworthstone/slb/commit/fccc8e368aebff5307ccbbf865bdf6a546654154))

### Hooks

- `slb hook install` edits only SLB's own entry in Claude settings and keeps sibling hooks ([#18](https://github.com/Dicklesworthstone/slb/issues/18), [`0351e52`](https://github.com/Dicklesworthstone/slb/commit/0351e524076bb4ba79a406645254f1230c85d41f)); a dangling `settings.json` symlink is preserved ([`b63dad6`](https://github.com/Dicklesworthstone/slb/commit/b63dad6dc7f55111298de51188174dbe6bba8a1e))
- The daemon query deadline is configurable instead of a fixed 50 ms ([#21](https://github.com/Dicklesworthstone/slb/issues/21), [`0351e52`](https://github.com/Dicklesworthstone/slb/commit/0351e524076bb4ba79a406645254f1230c85d41f))
- Approved Bash calls are handed to the atomic executor through single-use handoffs; hooks expose health, degraded mode and stale-snapshot diagnostics, and audit offline decisions ([`cb27c56`](https://github.com/Dicklesworthstone/slb/commit/cb27c56b0279bc58adf5c9acaf4da7a966ab9b4d), [`8dbef1b`](https://github.com/Dicklesworthstone/slb/commit/8dbef1b8f712af7af89e9ceaa5078428a2297ff3), [`351d577`](https://github.com/Dicklesworthstone/slb/commit/351d5777ff4e3507db33aa928142d59b41d4a462), [`258c930`](https://github.com/Dicklesworthstone/slb/commit/258c93044e0950b5af1dc9907c8e756ba7ea69c5), [`33e54f2`](https://github.com/Dicklesworthstone/slb/commit/33e54f2f07d2f15408c55b0b7f28b718f551f65d), [`3340e1d`](https://github.com/Dicklesworthstone/slb/commit/3340e1d936f5214865dcc176c2c927e35a4c7803))

### Requests, review and execution

- Cross-process atomic request admission with bounded, cancellable queueing and truthful quota diagnostics ([`c16e20a`](https://github.com/Dicklesworthstone/slb/commit/c16e20af8d287b10997ddb1307af43bfa49f64b1), [`f95c354`](https://github.com/Dicklesworthstone/slb/commit/f95c35448a56b2ccf9721f1bfe06dd8ea47f43d3), [`9d6d5da`](https://github.com/Dicklesworthstone/slb/commit/9d6d5da35d50d6b8ceedd25ed276b460486a1573))
- Bounded preflight evidence (dry-run previews) captured before review admission and exposed in the CLI ([`9d67508`](https://github.com/Dicklesworthstone/slb/commit/9d675087c919cd83b76086c0b8257b4dea14248d), [`532b65c`](https://github.com/Dicklesworthstone/slb/commit/532b65cbea8c7360edc54a3f1abed1c3d127f117), [`177c6f4`](https://github.com/Dicklesworthstone/slb/commit/177c6f4aae127b54698c4602da276ac3d6c0f0ac), [`91636eb`](https://github.com/Dicklesworthstone/slb/commit/91636eb7370ccd0221ebead10747eafa9ca67493), [`6402944`](https://github.com/Dicklesworthstone/slb/commit/64029441e125099d5f00a752d4efc3dbbe64dd77))
- Reviews are authenticated and resolved in one write transaction; policy decisions and human review proposals apply atomically ([`9da612c`](https://github.com/Dicklesworthstone/slb/commit/9da612c0fd7b6dd1af0f0b0239a7340147870935), [`00ce294`](https://github.com/Dicklesworthstone/slb/commit/00ce294f1b6193ebceb58eb2f459c57b208feeca), [`d6e3ad0`](https://github.com/Dicklesworthstone/slb/commit/d6e3ad080517d26e5ca6336a1afc769306e7e347), [`fb568d0`](https://github.com/Dicklesworthstone/slb/commit/fb568d0bd30d6342cefd1ecc50a87aafa682b35d))
- `slb execute --background` runs approved requests under a detached supervisor that records completion after the CLI exits ([`b7397b0`](https://github.com/Dicklesworthstone/slb/commit/b7397b0ad79d51a54cad87adba466a33ed844da3), [`9039ce8`](https://github.com/Dicklesworthstone/slb/commit/9039ce83aab028a0569fcb7bcc98457c466b9631), [`4b530fd`](https://github.com/Dicklesworthstone/slb/commit/4b530fd07b260db8665acaad6cd4c6a29aba5d36))
- Child exit codes propagate to the CLI; output capture is bounded and timed-out process trees are stopped ([`1e463ec`](https://github.com/Dicklesworthstone/slb/commit/1e463ec411c581e727a713ebaf1ba330e252e517), [`3295054`](https://github.com/Dicklesworthstone/slb/commit/3295054ba3578bfe9aa687057d97789d92e09b7c), [`787b10b`](https://github.com/Dicklesworthstone/slb/commit/787b10bff50aed166f51f985f161d9f4a1048884), [`2a1e618`](https://github.com/Dicklesworthstone/slb/commit/2a1e618b6689a08372c94fa7a05d7ab92d42476f))
- `execute` takes the session from the global `--session-id`/`-s` instead of a shadowing local flag ([`019df09`](https://github.com/Dicklesworthstone/slb/commit/019df09ca393f095c1b81ad6df4737ad82b53913))
- Cross-project delegated reviews; `review_pool` names reviewer agents ([`abb23a7`](https://github.com/Dicklesworthstone/slb/commit/abb23a76d5a392078836b0b5636977170cdd40dd), [`654c244`](https://github.com/Dicklesworthstone/slb/commit/654c244e21ad03c223c6e124bf81d677b8883d8b), [`c4d6262`](https://github.com/Dicklesworthstone/slb/commit/c4d62625ba18ab5c3b55fe2d70e5b12d3009f9eb))

### Daemon, events and notifications

- Durable request-lifecycle journal with resumable cursors: `slb events` replays and follows it, and the daemon delivers committed events after downtime ([`5f791f9`](https://github.com/Dicklesworthstone/slb/commit/5f791f9fb24d0eb63ac957df6fa076e6837cafed), [`a23baa8`](https://github.com/Dicklesworthstone/slb/commit/a23baa8ce833bf0884baa9b45934cec3b929959e), [`b668408`](https://github.com/Dicklesworthstone/slb/commit/b668408807f4ce0e92de39e3d1c75a9ad4b81785))
- Opt-in blocked-command alerts and journal notices via Agent Mail MCP and webhooks, rate-limited with offline catch-up ([`f275b76`](https://github.com/Dicklesworthstone/slb/commit/f275b76f7b3f22fcafaa1db4ff423573ea708be5), [`ceedb81`](https://github.com/Dicklesworthstone/slb/commit/ceedb810474487937ca2c33efef3934908d1b59e), [`503c07c`](https://github.com/Dicklesworthstone/slb/commit/503c07c17736ae10891c56ce0a20019092498910), [`51f1440`](https://github.com/Dicklesworthstone/slb/commit/51f14400e169b278b3ad42c1c03bb9ada215c696), [`3ffc1a9`](https://github.com/Dicklesworthstone/slb/commit/3ffc1a903ee4f3b97432b524e4958a8da0950320))
- Searchable JSONL audit of blocked hook decisions ([`b40731e`](https://github.com/Dicklesworthstone/slb/commit/b40731e5e5527d7b024d290bd8b0147bd2b51ca8))
- Exclusive daemon process ownership and fenced Unix-socket cleanup; loss-explicit IPC subscriptions ([`7e21bc0`](https://github.com/Dicklesworthstone/slb/commit/7e21bc0382932453e5ea6e6861720b7d3d48bb3c), [`ea36df9`](https://github.com/Dicklesworthstone/slb/commit/ea36df9d29aced4f6975b4bd0cc9ae2512c05d67), [`8df9e41`](https://github.com/Dicklesworthstone/slb/commit/8df9e41ee35f384ee1dfb60bc6f3f6f79621603a), [`19d2296`](https://github.com/Dicklesworthstone/slb/commit/19d229611db28849c75dd8ac228f407b9f39c70a))
- A shutdown request that arrives while the daemon is still starting is a clean stop, not a failure ([`ea06553`](https://github.com/Dicklesworthstone/slb/commit/ea06553d831fbda070979c9600634a6a923fb960))

### Rollback

- Git snapshots restore the tracked index/worktree and untracked files after `git clean`, and recover without the request database ([`2f4cd1e`](https://github.com/Dicklesworthstone/slb/commit/2f4cd1e099eb10d4fff64db778866b5964fc922c), [`1d061c7`](https://github.com/Dicklesworthstone/slb/commit/1d061c73ca51ee93a4ac837a1ba17691d772848f), [`d0592ba`](https://github.com/Dicklesworthstone/slb/commit/d0592babed82349a8d90aa08487e82281f847beb), [`9ff4d1f`](https://github.com/Dicklesworthstone/slb/commit/9ff4d1f8f64ec378f640468ca83f14657967fa8e))

### TUI

- Interactive review submissions work and stay policy-bound; escalations remain actionable ([`7f4c923`](https://github.com/Dicklesworthstone/slb/commit/7f4c92369215b4c855da87c29acb9410a3e1c89a), [`81134bb`](https://github.com/Dicklesworthstone/slb/commit/81134bbc86915e1b9661f2788659801f25afe09a))

### Build & Tests

- The full suite is green again on Linux and macOS: stale tests updated to the contracts above, test isolation and macOS socket-path/symlink issues fixed ([`e0d122f`](https://github.com/Dicklesworthstone/slb/commit/e0d122fe1d8e309c009a43d07c3ccf4a7e0c7a10), [`ca84c5d`](https://github.com/Dicklesworthstone/slb/commit/ca84c5dd482c15e2f9c45beab501f0a43b95cdaa))
- Concurrent claims serialize with `BEGIN IMMEDIATE` ([`4a3ec32`](https://github.com/Dicklesworthstone/slb/commit/4a3ec32803c50013cb021c832b4548a733aef8b6))

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
