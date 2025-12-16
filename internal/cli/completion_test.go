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
