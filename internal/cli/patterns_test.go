package cli

import (
	"encoding/json"
	"strings"
	"testing"

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
	patCmd.PersistentFlags().StringVarP(&flagPatternTier, "tier", "t", "", "risk tier")
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

	patCmd.AddCommand(listCmd, testCmd, addCmd, removeCmd, requestRemovalCmd, suggestCmd)
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
	stdout, err := executeCommandCapture(t, cmd, "patterns", "list", "-t", "critical", "-j")

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
	_, err := executeCommandCapture(t, cmd, "patterns", "list", "-t", "invalid-tier", "-j")

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
		t.Fatal("expected error when --tier is missing")
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
		"-t", "dangerous",
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
		t.Fatal("expected error when --tier is missing")
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
		"-t", "caution",
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
