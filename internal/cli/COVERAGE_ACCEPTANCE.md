# CLI Coverage Acceptance Criteria

This document defines the coverage acceptance matrix for `internal/cli`, focusing on
system-level functions whose remaining uncovered branches depend on OS, terminal,
filesystem, signal, or child-process failure modes that are impractical or unsafe to
force inside unit tests.

## Package Coverage Summary

- **Package target**: `>= 90.0%` statement coverage (`go test -coverprofile=... ./internal/cli`)
- **Critical safety target**: `100%` on `shouldAutoApproveCaution` and `autoApproveCaution`
- **Zero-percent functions**: `0` (every exported and internal helper in `internal/cli` has active behavioral coverage)

Run locally:

```bash
go test -coverprofile=cli.cov ./internal/cli
go tool cover -func=cli.cov | tail -n 1
```

## System-Level Function Exceptions (<100% Accepted)

| Function | File | Measured | System Dependency | What Unit Tests Cover | E2E / Live Verification Strategy |
|----------|------|----------|-------------------|-----------------------|----------------------------------|
| `detectWidth` | `help.go` | 83.3% | Real TTY ioctl (`term.GetSize` on `os.Stdout`) | `COLUMNS` env override, valid/invalid values, non-TTY fallback width | Verified in interactive terminal invocations of `slb --help` and bare `slb` |
| `followFileWithInterval` | `daemon.go` | 84.8% | Non-EOF kernel read/seek failures on open FD | Missing file open error, live line appends, partial/split line assembly, writer error propagation, immediate `context.Context` cancellation (`followFile` wrapper at 100%) | Verified via `slb daemon logs --follow` with `SIGINT`/`SIGTERM` cancellation |
| `launchBackgroundExecution` / `runBackgroundWorker` / `executeBackgroundJob` | `execute_background.go` | 80.6%–84.6% | Detached process spawning (`os.Executable()`), `os.Signal` interruption, supervisor IPC startup races | Command hash mismatch (JSON & text), timeout validation, startup failure JSON/text receipts, detached worker execution via `TestExecuteBackgroundWorkerProcess` | Covered end-to-end by `internal/cli/execute_background_test.go` subprocess worker tests and `internal/e2e/multi_agent_approval_test.go` |
| `runWatch` / `runWatchDaemon` / `runWatchPolling` | `watch.go` | 80.0%–88.0% | Live Unix socket daemon drop mid-stream, `SIGINT`/`SIGTERM` delivery, stdout pipe breaks | Pre-cancelled context, unreachable `SLB_HOST` remote failure (no local fallback), invalid `--poll-interval`, live daemon event streaming, polling fallback, caution auto-approval (`autoApproveCaution` at 100%) | Covered by `internal/cli/watch_integration_test.go` with live in-process `daemon.RunDaemon` and `internal/e2e/session_timeout_test.go` |
| `tuiCmd` / `resolveTUIOptions` | `tui.go` | 77.8% | Interactive Bubble Tea alt-screen TTY (`tui.RunWithOptions`) | `--refresh-interval <= 0` rejection, project/DB/theme/mouse option resolution | Interactive TUI rendering and keybindings tested in `internal/tui/...` headless model tests |
| `save` / `settingsWriteTarget` | `claude_settings.go` | 63.9%–81.2% | Atomic rename/fsync/permission errors and symlink race conditions on `~/.claude/settings.json` | Lossless JSON formatting/order preservation, malformed JSON rejection, atomic writes, backup/upgrade preservation, sibling hook preservation | Covered by `internal/e2e/hook_installation_test.go` and `internal/cli/claude_settings_test.go` |
| `runHookGenerate` / `installClaudeHook` / `runHookUninstall` / `runHookHealth` / `runHookTest` | `hook.go` | 78.9%–87.8% | Live daemon IPC transport, filesystem permission failures, `os.UserHomeDir` failures | Live daemon status/health/test parity, `--local-only` & `--simulate-failure` fallback, scope mismatch checks, corrupted DB/config rejection, install/uninstall idempotency | Covered by `internal/e2e/hook_installation_test.go`, `internal/cli/hook_runtime_test.go`, and `internal/cli/hook_handoff_test.go` |
| `processNativeGitHook` / `assessNativeGit` | `git_hooks.go` | 76.5%–87.9% | Git subprocess invocations, stdin >1 MiB / I/O errors, concurrent index mutation | `pre-commit` block + approval consumption, `pre-push` stdin validation & oversized rejection, `pre-rebase` argument validation, snapshot drift detection on `revalidate`, YAML/JSON output | Covered by `internal/cli/git_hooks_test.go` and `internal/e2e/git_rollback_test.go` |
| `patternsRemoveCmd` / `patternsTestCmd` (`--exit-code`) | `patterns.go` | 92.1% (file) | Direct `os.Exit(1)` termination paths in text mode | JSON structured block response, text/compound/parse-error classification, export formats (`json`, `yaml`, `claude-hook`), invalid regex/tier/flag validation | Exit-code behavior covered by `internal/e2e/risk_tier_classification_test.go` and live binary checks |
| `Execute` / `daemonLogPath` | `root.go` / `daemon.go` | 100.0% | Previously accepted at <90% due to `os.Exit` / `os.UserHomeDir`; now directly exercised in unit tests | `Execute()` end-to-end Cobra dispatch (`version --json`) and `daemonLogPath()` with/without `HOME` | Also covered by all subprocess binary invocations in `internal/e2e/...` |

## Acceptance Rules

1. **No coverage theater**: Every exception above represents genuine OS/TTY/process error branches or `os.Exit` paths; business logic, policy enforcement, and fail-closed guards must stay covered by real behavioral assertions.
2. **Pure/testable core extraction**: Whenever a system-level command accumulates non-trivial branching (e.g., `followFileWithInterval`, `shouldAutoApproveCaution`, `resolveTUIOptions`, `assessNativeGit`), extract context-cancellable or pure helpers so unit tests exercise the core behavior deterministically.
3. **E2E backstop**: Changes touching daemon IPC, hook installation, Git hooks, emergency overrides, or rollback recovery must keep `go test -race ./internal/e2e/...` green.
