package cli

import (
	"os"
	"strings"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/spf13/cobra"
)

var completionCmd = &cobra.Command{
	Use:       "completion [bash|zsh|fish|powershell]",
	Short:     "Generate shell completion scripts",
	Args:      cobra.ExactValidArgs(1),
	ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
	RunE: func(cmd *cobra.Command, args []string) error {
		switch args[0] {
		case "bash":
			return rootCmd.GenBashCompletion(os.Stdout)
		case "zsh":
			return rootCmd.GenZshCompletion(os.Stdout)
		case "fish":
			return rootCmd.GenFishCompletion(os.Stdout, true)
		case "powershell":
			return rootCmd.GenPowerShellCompletionWithDesc(os.Stdout)
		default:
			return nil
		}
	},
}

func init() {
	rootCmd.AddCommand(completionCmd)

	// Best-effort dynamic completion for session IDs.
	_ = rootCmd.RegisterFlagCompletionFunc("session-id", completeSessionIDs)
}

func completeSessionIDs(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	opts := db.OpenOptions{
		CreateIfNotExists: false,
		InitSchema:        false,
		ReadOnly:          true,
	}

	path, err := GetDB()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	database, err := db.OpenWithOptions(path, opts)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	defer database.Close()

	sessions, err := database.ListAllActiveSessions()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	out := make([]string, 0, len(sessions))
	for _, s := range sessions {
		if s == nil || s.ID == "" {
			continue
		}
		if toComplete != "" && !strings.HasPrefix(s.ID, toComplete) {
			continue
		}

		desc := s.AgentName
		if s.Program != "" {
			desc += " (" + s.Program + ")"
		}
		if s.Model != "" {
			desc += " " + s.Model
		}
		out = append(out, s.ID+"\t"+desc)
	}

	return out, cobra.ShellCompDirectiveNoFileComp
}
