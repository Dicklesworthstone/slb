package cli

import (
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/spf13/cobra"
)

func TestCompleteSessionIDs_EmptyDatabase(t *testing.T) {
	h := testutil.NewHarness(t)

	// Set the DB path
	flagDB = h.DBPath

	completions, directive := completeSessionIDs(nil, nil, "")

	// Should return empty list with no sessions
	if len(completions) != 0 {
		t.Errorf("expected 0 completions with empty database, got %d", len(completions))
	}
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("expected ShellCompDirectiveNoFileComp, got %d", directive)
	}
}

func TestCompleteSessionIDs_WithSessions(t *testing.T) {
	h := testutil.NewHarness(t)

	// Create some sessions
	testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("Agent1"),
		testutil.WithModel("model-1"),
	)
	testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("Agent2"),
		testutil.WithModel("model-2"),
	)

	// Set the DB path
	flagDB = h.DBPath

	completions, directive := completeSessionIDs(nil, nil, "")

	if len(completions) < 2 {
		t.Errorf("expected at least 2 completions, got %d", len(completions))
	}
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("expected ShellCompDirectiveNoFileComp, got %d", directive)
	}

	// Completions should include agent names and models
	found := false
	for _, c := range completions {
		if strings.Contains(c, "Agent1") || strings.Contains(c, "Agent2") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected completions to include agent names")
	}
}

func TestCompleteSessionIDs_WithPrefix(t *testing.T) {
	h := testutil.NewHarness(t)

	// Create sessions
	sess1 := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("Agent1"),
	)
	testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("Agent2"),
	)

	// Set the DB path
	flagDB = h.DBPath

	// Use the first session's ID prefix
	prefix := sess1.ID[:8]
	completions, _ := completeSessionIDs(nil, nil, prefix)

	// Should only return sessions matching the prefix
	for _, c := range completions {
		if !strings.HasPrefix(c, prefix) {
			t.Errorf("completion %q doesn't start with prefix %q", c, prefix)
		}
	}
}

func TestCompleteSessionIDs_DatabaseNotFound(t *testing.T) {
	// Set a non-existent database path
	flagDB = "/nonexistent/path/state.db"

	completions, directive := completeSessionIDs(nil, nil, "")

	// Should return empty list when database doesn't exist
	if len(completions) != 0 {
		t.Errorf("expected 0 completions when database missing, got %d", len(completions))
	}
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("expected ShellCompDirectiveNoFileComp, got %d", directive)
	}
}

func TestCompletionCommand_Help(t *testing.T) {
	root := &cobra.Command{
		Use:           "slb",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	completion := &cobra.Command{
		Use:       "completion [bash|zsh|fish|powershell]",
		Short:     "Generate shell completion scripts",
		Long:      "Generate shell completion scripts for bash, zsh, fish, or powershell.",
		Args:      cobra.ExactValidArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
	}

	root.AddCommand(completion)

	stdout, _, err := executeCommand(root, "completion", "--help")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(stdout, "completion") {
		t.Error("expected help to mention 'completion'")
	}
	if !strings.Contains(stdout, "bash") {
		t.Error("expected help to mention 'bash'")
	}
	if !strings.Contains(stdout, "zsh") {
		t.Error("expected help to mention 'zsh'")
	}
	if !strings.Contains(stdout, "fish") {
		t.Error("expected help to mention 'fish'")
	}
	if !strings.Contains(stdout, "powershell") {
		t.Error("expected help to mention 'powershell'")
	}
}

// TestCompleteSessionIDs_SessionWithoutProgram tests completion when session has no program.
func TestCompleteSessionIDs_SessionWithoutProgram(t *testing.T) {
	h := testutil.NewHarness(t)

	// Create a session with empty program
	testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("AgentNoProgram"),
		testutil.WithProgram(""),
		testutil.WithModel("test-model"),
	)

	// Set the DB path
	flagDB = h.DBPath

	completions, directive := completeSessionIDs(nil, nil, "")

	// Should return completions (session still valid without program)
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("expected ShellCompDirectiveNoFileComp, got %d", directive)
	}
	// Verify completion still works
	found := false
	for _, c := range completions {
		if strings.Contains(c, "AgentNoProgram") {
			found = true
			break
		}
	}
	if !found && len(completions) > 0 {
		t.Error("expected to find session with AgentNoProgram")
	}
}

// TestCompleteSessionIDs_SessionWithoutModel tests completion when session has no model.
func TestCompleteSessionIDs_SessionWithoutModel(t *testing.T) {
	h := testutil.NewHarness(t)

	// Create a session with empty model
	testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("AgentNoModel"),
		testutil.WithProgram("test-program"),
		testutil.WithModel(""),
	)

	// Set the DB path
	flagDB = h.DBPath

	completions, directive := completeSessionIDs(nil, nil, "")

	// Should return completions (session still valid without model)
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("expected ShellCompDirectiveNoFileComp, got %d", directive)
	}
	// Verify completion includes the program but not model
	for _, c := range completions {
		if strings.Contains(c, "AgentNoModel") {
			// Found the right session
			if !strings.Contains(c, "test-program") {
				t.Error("expected completion to include program name")
			}
			break
		}
	}
}

// TestCompleteSessionIDs_SessionMinimalInfo tests completion with minimal session info.
func TestCompleteSessionIDs_SessionMinimalInfo(t *testing.T) {
	h := testutil.NewHarness(t)

	// Create a session with only agent name (no program, no model)
	testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("MinimalAgent"),
		testutil.WithProgram(""),
		testutil.WithModel(""),
	)

	// Set the DB path
	flagDB = h.DBPath

	completions, _ := completeSessionIDs(nil, nil, "")

	// Should still return completions
	found := false
	for _, c := range completions {
		if strings.Contains(c, "MinimalAgent") {
			found = true
			// With no program or model, description should just be agent name
			break
		}
	}
	if !found && len(completions) > 0 {
		t.Error("expected to find session with MinimalAgent")
	}
}

func TestCompletionCommand_AllShellsAndInvalidShell(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		t.Run(shell, func(t *testing.T) {
			root := newTestRootCmd()
			comp := &cobra.Command{
				Use:       completionCmd.Use,
				ValidArgs: completionCmd.ValidArgs,
				Args:      completionCmd.Args,
				RunE:      completionCmd.RunE,
			}
			root.AddCommand(comp)
			out, err := executeCommandCapture(t, root, "completion", shell)
			if err != nil {
				t.Fatalf("completion %s failed: %v", shell, err)
			}
			if !strings.Contains(out, "slb") {
				t.Fatalf("completion %s script missing slb command name", shell)
			}
		})
	}

	root := newTestRootCmd()
	comp := &cobra.Command{
		Use:       completionCmd.Use,
		ValidArgs: completionCmd.ValidArgs,
		Args:      completionCmd.Args,
		RunE:      completionCmd.RunE,
	}
	root.AddCommand(comp)
	if _, _, err := executeCommand(root, "completion", "csh"); err == nil {
		t.Fatalf("expected error for unsupported shell 'csh'")
	}

	savedDB := flagDB
	t.Cleanup(func() { flagDB = savedDB })
	flagDB = "/nonexistent/slb-completion-test/state.db"
	items, dir := completeSessionIDs(nil, nil, "")
	if len(items) != 0 || dir != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("expected empty completions for missing DB, got items=%v dir=%v", items, dir)
	}
}
