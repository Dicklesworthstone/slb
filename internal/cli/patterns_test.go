package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/spf13/cobra"
)

// newTestPatternsCmd creates a fresh patterns command tree for testing.
func newTestPatternsCmd(dbPath string) *cobra.Command {
	root := &cobra.Command{
		Use:           "slb",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVar(&flagDB, "db", dbPath, "database path")
	root.PersistentFlags().StringVarP(&flagOutput, "output", "o", "text", "output format")
	root.PersistentFlags().BoolVarP(&flagJSON, "json", "j", false, "json output")
	root.PersistentFlags().StringVarP(&flagProject, "project", "C", "", "project directory")

	// Create fresh patterns commands
	patCmd := &cobra.Command{
		Use:   "patterns",
		Short: "Manage command classification patterns",
	}
	patCmd.PersistentFlags().StringVarP(&flagPatternTier, "tier", "T", "", "risk tier")
	patCmd.PersistentFlags().StringVarP(&flagPatternReason, "reason", "r", "", "reason")

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List all patterns grouped by tier",
		RunE:  patternsListCmd.RunE,
	}

	testCmd := &cobra.Command{
		Use:   "test <command>",
		Short: "Test which tier a command matches",
		Args:  cobra.ExactArgs(1),
		RunE:  patternsTestCmd.RunE,
	}
	testCmd.Flags().BoolVar(&flagPatternExitCode, "exit-code", false, "return non-zero if approval needed")

	addCmd := &cobra.Command{
		Use:   "add <pattern>",
		Short: "Add a new pattern to a tier",
		Args:  cobra.ExactArgs(1),
		RunE:  patternsAddCmd.RunE,
	}

	removeCmd := &cobra.Command{
		Use:   "remove <pattern>",
		Short: "Remove a pattern (BLOCKED for agents)",
		Args:  cobra.ExactArgs(1),
		RunE:  patternsRemoveCmd.RunE,
	}

	requestRemovalCmd := &cobra.Command{
		Use:   "request-removal <pattern>",
		Short: "Request removal of a pattern",
		Args:  cobra.ExactArgs(1),
		RunE:  patternsRequestRemovalCmd.RunE,
	}

	suggestCmd := &cobra.Command{
		Use:   "suggest <pattern>",
		Short: "Suggest a pattern for human review",
		Args:  cobra.ExactArgs(1),
		RunE:  patternsSuggestCmd.RunE,
	}

	// Also add check alias
	checkCmdTest := &cobra.Command{
		Use:   "check <command>",
		Short: "Alias for 'patterns test'",
		Args:  cobra.ExactArgs(1),
		RunE:  patternsTestCmd.RunE,
	}

	// Export command
	exportCmd := &cobra.Command{
		Use:   "export",
		Short: "Export patterns for external tools",
		RunE:  patternsExportCmd.RunE,
	}
	exportCmd.Flags().StringVarP(&flagPatternFormat, "format", "f", "json", "export format")
	exportCmd.Flags().StringVar(&flagPatternOutputFile, "output", "", "output file")

	// Version command
	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Show pattern version and hash",
		RunE:  patternsVersionCmd.RunE,
	}

	patCmd.AddCommand(listCmd, testCmd, addCmd, removeCmd, requestRemovalCmd, suggestCmd, exportCmd, versionCmd)
	root.AddCommand(patCmd, checkCmdTest)

	return root
}

func resetPatternsFlags() {
	flagDB = ""
	flagOutput = "text"
	flagJSON = false
	flagProject = ""
	flagPatternTier = ""
	flagPatternReason = ""
	flagPatternExitCode = false
	flagPatternFormat = "json"
	flagPatternOutputFile = ""
}

func TestPatternsListCommand_ListsPatterns(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPatternsFlags()

	cmd := newTestPatternsCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "patterns", "list", "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should return JSON object with tier keys
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	// Should have at least one tier
	if len(result) == 0 {
		t.Error("expected patterns result to have at least one tier")
	}
}

func TestPatternsListCommand_FilterByTier(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPatternsFlags()

	cmd := newTestPatternsCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "patterns", "list", "-T", "critical", "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	// Should only have critical tier
	for tier := range result {
		if tier != "critical" {
			t.Errorf("expected only 'critical' tier when filtering, got %s", tier)
		}
	}
}

func TestPatternsListCommand_InvalidTier(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPatternsFlags()

	cmd := newTestPatternsCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "patterns", "list", "-T", "invalid-Tier", "-j")

	if err == nil {
		t.Fatal("expected error for invalid tier")
	}
	if !strings.Contains(err.Error(), "invalid tier") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPatternsTestCommand_RequiresCommand(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPatternsFlags()

	cmd := newTestPatternsCmd(h.DBPath)
	_, _, err := executeCommand(cmd, "patterns", "test")

	if err == nil {
		t.Fatal("expected error when command is missing")
	}
	if !strings.Contains(err.Error(), "accepts 1 arg") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPatternsTestCommand_ClassifiesCommand(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPatternsFlags()

	cmd := newTestPatternsCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "patterns", "test", "rm -rf /", "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["command"] != "rm -rf /" {
		t.Errorf("expected command='rm -rf /', got %v", result["command"])
	}
	// This command should need approval
	if result["needs_approval"] != true {
		t.Errorf("expected needs_approval=true for 'rm -rf /', got %v", result["needs_approval"])
	}
}

func TestPatternsTestCommand_SafeCommand(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPatternsFlags()

	cmd := newTestPatternsCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "patterns", "test", "echo hello", "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	// Echo may or may not be safe depending on pattern configuration
	// Just verify the output structure has the expected fields
	if result["command"] != "echo hello" {
		t.Errorf("expected command='echo hello', got %v", result["command"])
	}
	if _, ok := result["needs_approval"]; !ok {
		t.Error("expected needs_approval field in result")
	}
	if _, ok := result["is_safe"]; !ok {
		t.Error("expected is_safe field in result")
	}
}

func TestCheckCommand_AliasForTest(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPatternsFlags()

	cmd := newTestPatternsCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "check", "echo hello", "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["command"] != "echo hello" {
		t.Errorf("expected command='echo hello', got %v", result["command"])
	}
}

func TestPatternsAddCommand_RequiresPattern(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPatternsFlags()

	cmd := newTestPatternsCmd(h.DBPath)
	_, _, err := executeCommand(cmd, "patterns", "add")

	if err == nil {
		t.Fatal("expected error when pattern is missing")
	}
	if !strings.Contains(err.Error(), "accepts 1 arg") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPatternsAddCommand_RequiresTier(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPatternsFlags()

	cmd := newTestPatternsCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "patterns", "add", "^my-pattern$", "-j")

	if err == nil {
		t.Fatal("expected error when --Tier is missing")
	}
	if !strings.Contains(err.Error(), "--tier is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPatternsAddCommand_AddsPattern(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPatternsFlags()

	cmd := newTestPatternsCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "patterns", "add", "^test-pattern$",
		"-T", "dangerous",
		"-r", "Test pattern",
		"-j",
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["status"] != "added" {
		t.Errorf("expected status=added, got %v", result["status"])
	}
	if result["pattern"] != "^test-pattern$" {
		t.Errorf("expected pattern='^test-pattern$', got %v", result["pattern"])
	}
	if result["tier"] != "dangerous" {
		t.Errorf("expected tier=dangerous, got %v", result["tier"])
	}
}

// Regression test for issue #2: `slb patterns add` reported
// status=added but never persisted the pattern. Verify both that the
// SQLite custom_patterns table is written AND that a follow-up
// `slb patterns test` invocation (which resets the in-memory engine
// and re-loads from DB) sees the new pattern as the matched tier.
func TestPatternsAddCommand_PersistsToCustomPatternsTable(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPatternsFlags()

	cmd := newTestPatternsCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "patterns", "add",
		`my\s+test\s+pattern`,
		"-T", "dangerous",
		"-r", "regression for issue #2",
		"-j",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v\nstdout: %s", err, stdout)
	}

	var addResult map[string]any
	if err := json.Unmarshal([]byte(stdout), &addResult); err != nil {
		t.Fatalf("failed to parse add JSON: %v\nstdout: %s", err, stdout)
	}
	if addResult["status"] != "added" {
		t.Fatalf("expected status=added, got %v", addResult["status"])
	}
	// New invariant from the fix: the response carries the inserted
	// row id (a positive int64), proving the SQLite INSERT actually
	// wrote a row instead of being a silent no-op.
	if id, ok := addResult["id"].(float64); !ok || id <= 0 {
		t.Errorf("expected positive id from persisted insert, got %v", addResult["id"])
	}

	// Verify directly against SQLite — schema must hold a row with
	// the exact pattern, tier, and source the CLI announced.
	count, err := h.DB.CountCustomPatterns()
	if err != nil {
		t.Fatalf("CountCustomPatterns: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 custom pattern row, got %d", count)
	}
	rows, err := h.DB.ListCustomPatterns()
	if err != nil {
		t.Fatalf("ListCustomPatterns: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListCustomPatterns returned %d rows, want 1", len(rows))
	}
	if rows[0].Pattern != `my\s+test\s+pattern` {
		t.Errorf("persisted pattern mismatch: got %q", rows[0].Pattern)
	}
	if rows[0].Tier != "dangerous" {
		t.Errorf("persisted tier mismatch: got %q", rows[0].Tier)
	}
	if rows[0].Source != "agent" {
		t.Errorf("persisted source mismatch: got %q", rows[0].Source)
	}

	// And the smoke test from the issue body: a fresh CLI process
	// should see the persisted pattern via `patterns test`. Reset
	// the in-memory engine state by recreating the command tree —
	// this mirrors a separate process invocation.
	resetPatternsFlags()
	testCmd := newTestPatternsCmd(h.DBPath)
	testStdout, err := executeCommandCapture(t, testCmd, "patterns", "test",
		"my test pattern",
		"-j",
	)
	if err != nil {
		t.Fatalf("patterns test error: %v\nstdout: %s", err, testStdout)
	}
	var testResult map[string]any
	if err := json.Unmarshal([]byte(testStdout), &testResult); err != nil {
		t.Fatalf("failed to parse test JSON: %v\nstdout: %s", err, testStdout)
	}
	if testResult["tier"] != "dangerous" {
		t.Errorf("expected tier=dangerous after persisted pattern reload, got %v\n  full result: %+v",
			testResult["tier"], testResult)
	}
}

// Re-adding the same pattern should be idempotent and report
// status=already_exists rather than blowing up on the
// UNIQUE(tier, pattern) constraint.
func TestPatternsAddCommand_IdempotentOnDuplicate(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPatternsFlags()

	cmd1 := newTestPatternsCmd(h.DBPath)
	if _, err := executeCommandCapture(t, cmd1, "patterns", "add",
		`^foo$`, "-T", "dangerous", "-r", "first", "-j",
	); err != nil {
		t.Fatalf("first add: %v", err)
	}

	resetPatternsFlags()
	cmd2 := newTestPatternsCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd2, "patterns", "add",
		`^foo$`, "-T", "dangerous", "-r", "duplicate", "-j",
	)
	if err != nil {
		t.Fatalf("duplicate add returned error instead of already_exists: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("parse JSON: %v\nstdout: %s", err, stdout)
	}
	if result["status"] != "already_exists" {
		t.Errorf("expected status=already_exists on duplicate, got %v", result["status"])
	}

	count, err := h.DB.CountCustomPatterns()
	if err != nil {
		t.Fatalf("CountCustomPatterns: %v", err)
	}
	if count != 1 {
		t.Errorf("duplicate add wrote a second row (count=%d)", count)
	}
}

// loadCustomPatternsIntoDefaultEngine must be idempotent: calling
// it twice in the same process must not double-count persisted
// patterns in the in-memory engine. Without dedup, a long-running
// process or a test that creates multiple cobra command trees
// would accumulate duplicate engine entries, drifting away from
// the SQLite source of truth.
func TestLoadCustomPatternsIntoDefaultEngine_Idempotent(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPatternsFlags()

	cmd := newTestPatternsCmd(h.DBPath)
	if _, err := executeCommandCapture(t, cmd, "patterns", "add",
		`^uniq-load-idempotent-pattern$`,
		"-T", "dangerous", "-r", "loader idempotency", "-j",
	); err != nil {
		t.Fatalf("add: %v", err)
	}

	countMatching := func() int {
		engine := core.GetDefaultEngine()
		n := 0
		for _, p := range engine.ListPatterns(core.RiskTierDangerous) {
			if p.Pattern == `^uniq-load-idempotent-pattern$` {
				n++
			}
		}
		return n
	}

	// Snapshot whatever the engine has after the explicit add (it
	// already includes one in-memory copy from the patternsAddCmd
	// engine.AddPattern call). The loader must not increase this.
	before := countMatching()
	if before == 0 {
		t.Fatalf("precondition: explicit add did not register the pattern in the engine")
	}

	// Force flagDB to point at the harness DB so loader picks the
	// right path (matches what newTestPatternsCmd does for cobra).
	prevDB := flagDB
	flagDB = h.DBPath
	t.Cleanup(func() { flagDB = prevDB })

	// First load: may add nothing if the explicit add already
	// populated the engine (current behavior); main thing is no
	// duplication.
	if _, err := loadCustomPatternsIntoDefaultEngine(); err != nil {
		t.Fatalf("first loader call: %v", err)
	}
	if got := countMatching(); got != before {
		t.Errorf("first loader call duplicated: count went from %d to %d", before, got)
	}

	// Second load: must remain idempotent.
	if _, err := loadCustomPatternsIntoDefaultEngine(); err != nil {
		t.Fatalf("second loader call: %v", err)
	}
	if got := countMatching(); got != before {
		t.Errorf("second loader call duplicated: count went from %d to %d", before, got)
	}
}

func TestPatternsRemoveCommand_IsBlocked(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPatternsFlags()

	cmd := newTestPatternsCmd(h.DBPath)
	stdout, _ := executeCommandCapture(t, cmd, "patterns", "remove", "^some-pattern$", "-j")

	// Should return error response in JSON
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["error"] != "pattern_removal_blocked" {
		t.Errorf("expected error=pattern_removal_blocked, got %v", result["error"])
	}
}

func TestPatternsRequestRemovalCommand_RequiresReason(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPatternsFlags()

	cmd := newTestPatternsCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "patterns", "request-removal", "^some-pattern$", "-j")

	if err == nil {
		t.Fatal("expected error when --reason is missing")
	}
	if !strings.Contains(err.Error(), "--reason is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPatternsRequestRemovalCommand_CreatesRequest(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPatternsFlags()

	cmd := newTestPatternsCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "patterns", "request-removal", "^some-pattern$",
		"-r", "No longer needed",
		"-j",
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["status"] != "pending" {
		t.Errorf("expected status=pending, got %v", result["status"])
	}
	if result["pattern"] != "^some-pattern$" {
		t.Errorf("expected pattern='^some-pattern$', got %v", result["pattern"])
	}
}

func TestPatternsSuggestCommand_RequiresTier(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPatternsFlags()

	cmd := newTestPatternsCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "patterns", "suggest", "^suggested-pattern$", "-j")

	if err == nil {
		t.Fatal("expected error when --Tier is missing")
	}
	if !strings.Contains(err.Error(), "--tier is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPatternsSuggestCommand_CreatesSuggestion(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPatternsFlags()

	cmd := newTestPatternsCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "patterns", "suggest", "^suggested-pattern$",
		"-T", "caution",
		"-j",
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	if result["status"] != "suggested" {
		t.Errorf("expected status=suggested, got %v", result["status"])
	}
	if result["tier"] != "caution" {
		t.Errorf("expected tier=caution, got %v", result["tier"])
	}
}

func TestPatternsCommand_Help(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPatternsFlags()

	cmd := newTestPatternsCmd(h.DBPath)
	stdout, _, err := executeCommand(cmd, "patterns", "--help")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(stdout, "patterns") {
		t.Error("expected help to mention 'patterns'")
	}
	if !strings.Contains(stdout, "list") {
		t.Error("expected help to mention 'list' subcommand")
	}
	if !strings.Contains(stdout, "test") {
		t.Error("expected help to mention 'test' subcommand")
	}
	if !strings.Contains(stdout, "add") {
		t.Error("expected help to mention 'add' subcommand")
	}
}

func TestPatternsListCommand_TextOutput(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPatternsFlags()

	cmd := newTestPatternsCmd(h.DBPath)
	// No -j flag, so text output
	stdout, err := executeCommandCapture(t, cmd, "patterns", "list")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should return text output with patterns
	// The output might be empty if no patterns, but function should run without error
	_ = stdout // Just verify no error occurs
}

// TestPatternsListCommand_TextOutputWithDescriptions tests text output with pattern descriptions.
func TestPatternsListCommand_TextOutputWithDescriptions(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPatternsFlags()

	// First, add a pattern with a description (reason becomes description)
	cmd := newTestPatternsCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "patterns", "add", "^test-with-desc$",
		"-T", "dangerous",
		"-r", "This is a test description",
		"-j",
	)
	if err != nil {
		t.Fatalf("failed to add pattern: %v", err)
	}

	// Reset flags before next command
	resetPatternsFlags()

	// List patterns in text format (no -j flag)
	cmd = newTestPatternsCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "patterns", "list", "-T", "dangerous")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the pattern and description are in output
	if !strings.Contains(stdout, "^test-with-desc$") {
		t.Error("expected output to contain the pattern")
	}
	// The description should appear as a comment line: "    # This is a test description"
	if !strings.Contains(stdout, "This is a test description") {
		t.Error("expected output to contain the pattern description")
	}
}

func TestParseTier_ValidTiers(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"critical", "critical"},
		{"dangerous", "dangerous"},
		{"caution", "caution"},
		{"safe", "safe"},
		{"CRITICAL", "critical"},
		{"Dangerous", "dangerous"},
		{"invalid", ""},
		{"", ""},
	}

	for _, tt := range tests {
		name := tt.input
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			result := parseTier(tt.input)
			if string(result) != tt.expected {
				t.Errorf("parseTier(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

// Tests for export and version commands

func TestPatternsExportCommand_JSON(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPatternsFlags()

	cmd := newTestPatternsCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "patterns", "export", "--format=json")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should return valid JSON with expected structure
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	// Check required fields
	if _, ok := result["version"]; !ok {
		t.Error("expected 'version' field in export")
	}
	if _, ok := result["sha256"]; !ok {
		t.Error("expected 'sha256' field in export")
	}
	if _, ok := result["tiers"]; !ok {
		t.Error("expected 'tiers' field in export")
	}
	if _, ok := result["metadata"]; !ok {
		t.Error("expected 'metadata' field in export")
	}

	// Check tiers structure
	tiers, ok := result["tiers"].(map[string]any)
	if !ok {
		t.Fatalf("expected tiers to be a map, got %T", result["tiers"])
	}

	expectedTiers := []string{"safe", "caution", "dangerous", "critical"}
	for _, tier := range expectedTiers {
		if _, ok := tiers[tier]; !ok {
			t.Errorf("expected tier %q in export", tier)
		}
	}
}

func TestPatternsExportCommand_ClaudeHook(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPatternsFlags()

	cmd := newTestPatternsCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "patterns", "export", "--format=claude-hook")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should contain Python code markers
	if !strings.Contains(stdout, "import re") {
		t.Error("expected 'import re' in Claude hook export")
	}
	if !strings.Contains(stdout, "SAFE_PATTERNS") {
		t.Error("expected 'SAFE_PATTERNS' in Claude hook export")
	}
	if !strings.Contains(stdout, "DANGEROUS_PATTERNS") {
		t.Error("expected 'DANGEROUS_PATTERNS' in Claude hook export")
	}
	if !strings.Contains(stdout, "CRITICAL_PATTERNS") {
		t.Error("expected 'CRITICAL_PATTERNS' in Claude hook export")
	}
	if !strings.Contains(stdout, "def classify(command:") {
		t.Error("expected 'def classify' function in Claude hook export")
	}
	if !strings.Contains(stdout, "SHA256:") {
		t.Error("expected SHA256 hash in Claude hook export header")
	}
}

func TestPatternsExportCommand_InvalidFormat(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPatternsFlags()

	cmd := newTestPatternsCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "patterns", "export", "--format=invalid")

	if err == nil {
		t.Fatal("expected error for invalid format")
	}
	if !strings.Contains(err.Error(), "unknown format") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPatternsVersionCommand(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPatternsFlags()

	cmd := newTestPatternsCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "patterns", "version", "-j")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("failed to parse JSON: %v\nstdout: %s", err, stdout)
	}

	// Check required fields
	if _, ok := result["version"]; !ok {
		t.Error("expected 'version' field")
	}
	if _, ok := result["sha256"]; !ok {
		t.Error("expected 'sha256' field")
	}
	if _, ok := result["pattern_count"]; !ok {
		t.Error("expected 'pattern_count' field")
	}
	if _, ok := result["tier_counts"]; !ok {
		t.Error("expected 'tier_counts' field")
	}

	// Verify pattern_count is positive
	count, ok := result["pattern_count"].(float64)
	if !ok || count <= 0 {
		t.Errorf("expected positive pattern_count, got %v", result["pattern_count"])
	}

	// Verify sha256 is a valid hex string
	sha256, ok := result["sha256"].(string)
	if !ok || len(sha256) != 64 {
		t.Errorf("expected 64-char sha256 hash, got %q", sha256)
	}
}

// `patterns version` must reflect persisted customs in its
// reported sha256 + pattern_count. Without the loader call,
// tooling that uses the hash for "do I need to regenerate the
// hook?" decisions would never observe an `slb patterns add`.
func TestPatternsVersionCommand_ReflectsPersistedCustomPatterns(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPatternsFlags()

	// Baseline version (builtins only).
	baseCmd := newTestPatternsCmd(h.DBPath)
	stdout1, err := executeCommandCapture(t, baseCmd, "patterns", "version", "-j")
	if err != nil {
		t.Fatalf("baseline version: %v", err)
	}
	var base map[string]any
	if err := json.Unmarshal([]byte(stdout1), &base); err != nil {
		t.Fatalf("parse baseline JSON: %v\nstdout: %s", err, stdout1)
	}

	// Persist a custom pattern.
	resetPatternsFlags()
	addCmd := newTestPatternsCmd(h.DBPath)
	if _, err := executeCommandCapture(t, addCmd, "patterns", "add",
		`^uniq-version-includes-marker-q42z$`,
		"-T", "dangerous", "-r", "regression for patterns version inclusion", "-j",
	); err != nil {
		t.Fatalf("patterns add: %v", err)
	}

	// Re-run version. The pattern_count must increase by exactly 1
	// and the sha256 must differ from the baseline.
	resetPatternsFlags()
	postCmd := newTestPatternsCmd(h.DBPath)
	stdout2, err := executeCommandCapture(t, postCmd, "patterns", "version", "-j")
	if err != nil {
		t.Fatalf("post-add version: %v", err)
	}
	var post map[string]any
	if err := json.Unmarshal([]byte(stdout2), &post); err != nil {
		t.Fatalf("parse post-add JSON: %v\nstdout: %s", err, stdout2)
	}

	baseCount, _ := base["pattern_count"].(float64)
	postCount, _ := post["pattern_count"].(float64)
	if postCount != baseCount+1 {
		t.Errorf("pattern_count did not include the persisted custom pattern: baseline=%v post-add=%v",
			baseCount, postCount)
	}
	if base["sha256"] == post["sha256"] {
		t.Errorf("sha256 should differ when a custom pattern is added; both reported %v",
			base["sha256"])
	}
}

func TestPatternsVersionCommand_DeterministicHash(t *testing.T) {
	h := testutil.NewHarness(t)
	resetPatternsFlags()

	// Run version command twice
	cmd1 := newTestPatternsCmd(h.DBPath)
	stdout1, err := executeCommandCapture(t, cmd1, "patterns", "version", "-j")
	if err != nil {
		t.Fatalf("first run error: %v", err)
	}

	resetPatternsFlags()
	cmd2 := newTestPatternsCmd(h.DBPath)
	stdout2, err := executeCommandCapture(t, cmd2, "patterns", "version", "-j")
	if err != nil {
		t.Fatalf("second run error: %v", err)
	}

	var result1, result2 map[string]any
	json.Unmarshal([]byte(stdout1), &result1)
	json.Unmarshal([]byte(stdout2), &result2)

	// Hash should be identical for same patterns
	if result1["sha256"] != result2["sha256"] {
		t.Errorf("hash not deterministic: %v != %v", result1["sha256"], result2["sha256"])
	}
}
