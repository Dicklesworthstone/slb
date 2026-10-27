# CLI Testing Patterns Guide

This document describes testing patterns, utilities, and conventions used in the SLB CLI package.

## Overview

### Purpose
- Ensure CLI commands work correctly with database state
- Verify flag parsing and validation
- Test JSON and text output formats
- Validate error handling and edge cases

### Coverage Target
- **Target**: 90% statement coverage for business logic
- **Rationale**: CLI commands are user-facing and should be thoroughly tested

### Test File Naming
- One test file per command: `<command>_test.go`
- Example: `review_test.go`, `approve_test.go`, `session_test.go`

## Test Infrastructure

### Harness Setup (`testutil.NewHarness`)

The Harness provides a complete test environment with temp directories and database:

```go
func TestMyCommand(t *testing.T) {
    h := testutil.NewHarness(t)

    // h.ProjectDir - temp project directory
    // h.SLBDir     - path to .slb directory
    // h.DBPath     - path to state.db
    // h.DB         - database connection

    // No cleanup needed - registered via t.Cleanup
}
```

### Fixture Creation

Use `testutil.MakeSession` and `testutil.MakeRequest` to create test data:

```go
// Create session with options
sess := testutil.MakeSession(t, h.DB,
    testutil.WithProject(h.ProjectDir),
    testutil.WithAgent("TestAgent"),
    testutil.WithModel("test-model"),
)

// Create request linked to session
req := testutil.MakeRequest(t, h.DB, sess,
    testutil.WithCommand("rm -rf ./build", h.ProjectDir, true),
    testutil.WithRisk(db.RiskTierDangerous),
    testutil.WithJustification("reason", "effect", "goal", "safety"),
)
```

#### Session Options
- `WithProject(path)` - Set project path
- `WithAgent(name)` - Set agent name
- `WithModel(model)` - Set model name
- `WithProgram(program)` - Set program name

#### Request Options
- `WithCommand(raw, cwd, shell)` - Set command spec
- `WithRisk(tier)` - Set risk tier
- `WithJustification(reason, effect, goal, safety)` - Set justification fields
- `WithDryRun(cmd, output)` - Add dry run results
- `WithRequireDifferentModel(bool)` - Set model requirement
- `WithMinApprovals(n)` - Set approval threshold
- `WithExpiresAt(time)` - Set expiry time

### Command Execution Helpers

Two patterns exist for executing commands in tests:

#### 1. `executeCommand` - Buffer-based capture

```go
func executeCommand(root *cobra.Command, args ...string) (stdout, stderr string, err error) {
    stdoutBuf := new(bytes.Buffer)
    stderrBuf := new(bytes.Buffer)
    root.SetOut(stdoutBuf)
    root.SetErr(stderrBuf)
    root.SetArgs(args)
    err = root.Execute()
    return stdoutBuf.String(), stderrBuf.String(), err
}
```

Use when you need both stdout and stderr, or when testing commands that use `cmd.OutOrStdout()`.

#### 2. `executeCommandCapture` - Real stdout capture

```go
func executeCommandCapture(t *testing.T, root *cobra.Command, args ...string) (stdout string, err error) {
    t.Helper()
    root.SetArgs(args)
    stdout = captureStdout(t, func() {
        err = root.Execute()
    })
    return stdout, err
}
```

Use when testing commands that write directly to `os.Stdout`.

### Flag Reset Pattern

CLI tests must reset global flags between tests to avoid state pollution:

```go
func resetMyCommandFlags() {
    flagDB = ""
    flagOutput = "text"
    flagJSON = false
    flagProject = ""
    // ... reset all flags used by command
}

func TestMyCommand(t *testing.T) {
    h := testutil.NewHarness(t)
    resetMyCommandFlags()  // Always call at start of test

    // ... test code
}
```

### Creating Fresh Command Trees

Each test should create a fresh command tree to avoid state pollution:

```go
func newTestMyCmd(dbPath string) *cobra.Command {
    root := &cobra.Command{
        Use:           "slb",
        SilenceUsage:  true,
        SilenceErrors: true,
    }

    // Add flags (pointing to package-level flag variables)
    root.PersistentFlags().StringVar(&flagDB, "db", dbPath, "database path")
    root.PersistentFlags().BoolVarP(&flagJSON, "json", "j", false, "json output")
    // ... add other needed flags

    // Create your command
    myCmd := &cobra.Command{
        Use:  "mycommand",
        RunE: myCommandImpl.RunE,
    }
    root.AddCommand(myCmd)

    return root
}
```

## Testing Patterns

### A. Command Structure Tests

Test that commands show help and have correct structure:

```go
func TestMyCommand_Help(t *testing.T) {
    h := testutil.NewHarness(t)
    resetMyCommandFlags()

    cmd := newTestMyCmd(h.DBPath)
    stdout, _, err := executeCommand(cmd, "mycommand", "--help")

    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if !strings.Contains(stdout, "mycommand") {
        t.Error("expected help to mention command name")
    }
}
```

### B. Flag Parsing Tests

Test required flags and validation:

```go
func TestMyCommand_RequiresRequestID(t *testing.T) {
    h := testutil.NewHarness(t)
    resetMyCommandFlags()

    cmd := newTestMyCmd(h.DBPath)
    _, _, err := executeCommand(cmd, "mycommand")

    if err == nil {
        t.Fatal("expected error when request ID is missing")
    }
    if !strings.Contains(err.Error(), "accepts 1 arg") {
        t.Errorf("unexpected error: %v", err)
    }
}
```

### C. Database Integration Tests

Test commands that read/write database state:

```go
func TestMyCommand_CreatesRecord(t *testing.T) {
    h := testutil.NewHarness(t)
    resetMyCommandFlags()

    sess := testutil.MakeSession(t, h.DB,
        testutil.WithProject(h.ProjectDir),
    )
    req := testutil.MakeRequest(t, h.DB, sess)

    cmd := newTestMyCmd(h.DBPath)
    _, err := executeCommandCapture(t, cmd, "mycommand", req.ID)

    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }

    // Verify database state
    updated, err := h.DB.GetRequest(req.ID)
    if err != nil {
        t.Fatalf("failed to get request: %v", err)
    }
    if updated.Status != db.StatusApproved {
        t.Errorf("expected status approved, got %s", updated.Status)
    }
}
```

### D. Output Format Tests

#### JSON Output

```go
func TestMyCommand_JSONOutput(t *testing.T) {
    h := testutil.NewHarness(t)
    resetMyCommandFlags()

    // Create test data...

    cmd := newTestMyCmd(h.DBPath)
    stdout, err := executeCommandCapture(t, cmd, "mycommand", "-j")

    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }

    var result map[string]any
    if err := json.Unmarshal([]byte(stdout), &result); err != nil {
        t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
    }

    // Verify JSON structure
    if result["id"] == nil {
        t.Error("expected id field in JSON output")
    }
}
```

#### Text Output

```go
func TestMyCommand_TextOutput(t *testing.T) {
    h := testutil.NewHarness(t)
    resetMyCommandFlags()

    // Create test data...

    cmd := newTestMyCmd(h.DBPath)
    stdout, err := executeCommandCapture(t, cmd, "mycommand", req.ID)

    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }

    // Verify human-readable output
    if !strings.Contains(stdout, "Status:") {
        t.Error("expected text output to contain 'Status:'")
    }
}
```

### E. Error Path Tests

Test error handling for invalid inputs and edge cases:

```go
func TestMyCommand_NotFound(t *testing.T) {
    h := testutil.NewHarness(t)
    resetMyCommandFlags()

    cmd := newTestMyCmd(h.DBPath)
    _, err := executeCommandCapture(t, cmd, "mycommand", "nonexistent-id")

    if err == nil {
        t.Fatal("expected error for nonexistent request")
    }
}

func TestMyCommand_DatabaseError(t *testing.T) {
    h := testutil.NewHarness(t)
    resetMyCommandFlags()

    // Close database to simulate error
    h.DB.Close()

    cmd := newTestMyCmd(h.DBPath)
    _, err := executeCommandCapture(t, cmd, "mycommand", "any-id")

    if err == nil {
        t.Fatal("expected error with closed database")
    }
}
```

### F. Business Logic Tests

Test command-specific logic:

```go
func TestMyCommand_ValidatesPermissions(t *testing.T) {
    h := testutil.NewHarness(t)
    resetMyCommandFlags()

    requestorSess := testutil.MakeSession(t, h.DB,
        testutil.WithProject(h.ProjectDir),
        testutil.WithAgent("Requestor"),
    )
    reviewerSess := testutil.MakeSession(t, h.DB,
        testutil.WithProject(h.ProjectDir),
        testutil.WithAgent("Reviewer"),
    )

    req := testutil.MakeRequest(t, h.DB, requestorSess,
        testutil.WithRequireDifferentModel(true),
    )

    // Test that same-model review is rejected
    cmd := newTestMyCmd(h.DBPath)
    _, err := executeCommandCapture(t, cmd, "mycommand", req.ID,
        "-s", reviewerSess.ID)

    if err == nil {
        t.Fatal("expected error for same-model review")
    }
}
```

## Advanced Testing Utilities

### Context Cancellation Testing (`testutil.RunWithCancel`)

Test functions that should respect context cancellation:

```go
func TestMyFunc_RespectsCancel(t *testing.T) {
    result := testutil.RunWithCancel(func(ctx context.Context) error {
        return myLongRunningFunc(ctx)
    }, 50*time.Millisecond, 1*time.Second)

    if !result.WasCancelled {
        t.Error("function did not respect cancellation")
    }
}
```

### Timeout Testing (`testutil.RunWithTimeout`)

Test functions with timeout:

```go
func TestMyFunc_CompletesInTime(t *testing.T) {
    result := testutil.RunWithTimeout(func(ctx context.Context) error {
        return myFunc(ctx)
    }, 100*time.Millisecond)

    if !result.Completed {
        t.Error("function timed out")
    }
}
```

### Condition Polling (`testutil.WaitForCondition`)

Wait for async state changes:

```go
func TestMyAsync_EventuallyReady(t *testing.T) {
    // Start async operation...

    ok := testutil.WaitForCondition(func() bool {
        return server.IsReady()
    }, 10*time.Millisecond, 1*time.Second)

    if !ok {
        t.Error("server did not become ready")
    }
}
```

### Command Execution Mocking (`testutil.MockExecutor`)

Mock external command execution:

```go
func TestMyFunc_ExecutesCommand(t *testing.T) {
    mock := testutil.NewMockExecutor([]byte("success"), nil)

    err := myFunc(mock)

    if err != nil {
        t.Errorf("unexpected error: %v", err)
    }
    if !mock.WasCalledWith("git", "status") {
        t.Error("expected git status to be called")
    }
}
```

For sequential command responses:

```go
func TestMyFunc_RetryLogic(t *testing.T) {
    mock := testutil.NewCommandSequenceMock(
        testutil.SequenceStep{Error: errors.New("temporary failure")},
        testutil.SequenceStep{Output: []byte("success")},
    )

    err := myFuncWithRetry(mock)

    if err != nil {
        t.Error("expected retry to succeed")
    }
}
```

## Coverage Summary

**Current Coverage: 61.9%** (as of Dec 2025)

### Coverage by Category

| Category | Coverage | Notes |
|----------|----------|-------|
| Business Logic | 100% | All pure functions fully tested |
| Command Handlers | ~85% | Most commands well covered |
| System-Level | 0% | See acceptance matrix below |

### Pure Functions at 100% Coverage
- `shouldAutoApproveCaution` - Risk tier evaluation logic
- `evaluateRequestForPolling` - Request filtering logic
- `statusToEventType` - Status mapping
- `ListRequestHistory` - Project-scoped history filters applied before limiting
- `parseTier` - Risk tier parsing
- `outputPatterns` - Pattern formatting
- All `init` functions

## Exit Code Testing Pattern (P3.3 Decision)

### The Challenge

Functions that call `os.Exit()` present a testing challenge because `os.Exit()` terminates the process, including the test runner:

```go
func Execute() error {
    if err := rootCmd.Execute(); err != nil {
        os.Exit(1)  // This kills the test runner!
    }
}
```

### Evaluated Options

**Option A: Build Tag Approach**
```go
//go:build !test
var osExit = os.Exit

//go:build test
var osExit = func(code int) { capturedExitCode = code }
```
- Pros: Clean separation
- Cons: Build complexity, easy to forget tags

**Option B: Dependency Injection**
```go
func ExecuteWithExit(exit func(int)) { ... }
func Execute() { ExecuteWithExit(os.Exit) }
```
- Pros: Explicit, testable
- Cons: API change, extra indirection

**Option C: Accept 0% Coverage (CHOSEN)**
- Pros: No code changes, simpler maintenance
- Cons: Reduced coverage metric

### Decision: Option C

**Rationale:**
1. **Trivial code**: `Execute()` is 3 lines; the complexity is in `rootCmd.Execute()` which is already tested
2. **Integration tested**: E2E tests verify exit codes by running the actual binary
3. **Cost vs benefit**: Testing infrastructure cost is high for minimal bug-catching benefit
4. **Coverage target is 90%, not 100%**: Accepting a few system-level functions at 0% is appropriate

**Mitigation Strategy: Pure Function Extraction**

For complex functions with `os.Exit()`, extract the business logic into testable pure functions:

```go
// System-level (0% coverage, acceptable)
func runCommand() {
    if !shouldProceed(req) {
        os.Exit(1)
    }
    // ... execution
}

// Pure function (100% coverage)
func shouldProceed(req *Request) bool {
    // All business logic here, fully testable
}
```

This pattern is used throughout the CLI package (see `shouldAutoApproveCaution`, `evaluateRequestForPolling`, etc.).

## Coverage Acceptance Matrix

Some functions are intentionally excluded from unit test coverage due to their nature:

| Function | Coverage | Rationale |
|----------|----------|-----------|
| `Execute` | 0% | Main entry point, calls `os.Exit`. Integration tested only. |
| `runWatch` | 0% | Long-running daemon, signal handling. Integration tested. |
| `runWatchDaemon` | 0% | Spawns daemon process. Integration tested. |
| `runWatchPolling` | 0% | Infinite loop with DB polling. Integration tested. |
| `autoApproveCaution` | 0% | Complex global state (DB, session flags). Core logic in `shouldAutoApproveCaution` is 100%. |
| `followFile` | 0% | Signal handling, os.Stdin interaction. |
| `runSafeCommand` | 0% | Executes shell commands with exec.Command. |
| `runApprovedRequest` | 0% | Command execution with side effects. |
| `writeError` | 0% | Simple error output helper. |

### Pure Function Extraction Strategy

For complex system-level functions, extract testable business logic:

```go
// System-level function (hard to test)
func autoApproveCaution(ctx context.Context, req *db.Request) error {
    database, _ := db.Open(GetDB())
    sess, _ := database.GetSession(flagWatchSessionID)

    if !shouldAutoApproveCaution(req, sess, database) {
        return nil
    }
    // ... side effects
}

// Pure function (easy to test, 100% coverage)
func shouldAutoApproveCaution(req *db.Request, sess *db.Session, database *db.DB) bool {
    if req.RiskTier != db.RiskTierCaution {
        return false
    }
    // ... business logic
}
```

## Common Pitfalls

### 1. Flag State Pollution

**Problem**: Flags retain values between tests.

**Solution**: Always reset flags at the start of each test:

```go
func resetFlags() {
    flagDB = ""
    flagJSON = false
    // ... all flags
}

func TestA(t *testing.T) {
    resetFlags()
    // ...
}
```

### 2. Working Directory Assumptions

**Problem**: Tests assume specific working directory.

**Solution**: Use `h.ProjectDir` for all paths:

```go
// Bad
os.Chdir("/tmp/myproject")

// Good
cmd := newTestCmd(h.DBPath)
executeCommand(cmd, "mycommand", "-C", h.ProjectDir)
```

### 3. Environment Variable Leakage

**Problem**: Tests modify environment variables without cleanup.

**Solution**: Save and restore:

```go
func TestWithEnv(t *testing.T) {
    origVal := os.Getenv("MY_VAR")
    defer os.Setenv("MY_VAR", origVal)

    os.Setenv("MY_VAR", "test-value")
    // ... test
}
```

### 4. Test Isolation Failures

**Problem**: Tests share database state.

**Solution**: Always use `testutil.NewHarness(t)` for fresh database:

```go
// Each test gets its own temp database
func TestA(t *testing.T) {
    h := testutil.NewHarness(t)  // Fresh DB
    // ...
}

func TestB(t *testing.T) {
    h := testutil.NewHarness(t)  // Different fresh DB
    // ...
}
```

### 5. Missing `t.Helper()` Calls

**Problem**: Test failures report wrong line numbers.

**Solution**: Add `t.Helper()` to test helper functions:

```go
func assertNoError(t *testing.T, err error) {
    t.Helper()  // Report caller's line number
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
}
```

## Running Tests

```bash
# Run all CLI tests
go test ./internal/cli/...

# Run with coverage
go test -coverprofile=coverage.out ./internal/cli/...

# View coverage report
go tool cover -func=coverage.out

# Generate HTML coverage report
go tool cover -html=coverage.out -o coverage.html

# Run specific test
go test ./internal/cli/... -run TestReviewCommand

# Run with verbose output
go test -v ./internal/cli/...

# Run with race detection
go test -race ./internal/cli/...
```

## Test Organization Example

```
internal/cli/
├── approve.go
├── approve_test.go      # Tests for approve command
├── review.go
├── review_test.go       # Tests for review command
├── root.go
├── root_test.go         # Tests for root command and helpers
├── session.go
├── session_test.go      # Tests for session command
├── watch.go
├── watch_test.go        # Tests for watch command
└── TESTING.md           # This file
```

Each `*_test.go` file should contain:
1. `newTest<Cmd>Cmd(dbPath)` - Fresh command tree factory
2. `reset<Cmd>Flags()` - Flag reset function
3. Test functions organized by behavior
