package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Dicklesworthstone/slb/internal/integrations"
	"github.com/spf13/cobra"
)

var integrationsCmd = &cobra.Command{
	Use:   "integrations",
	Short: "Integration helpers for agent tools",
}

var cursorRulesCmd = &cobra.Command{
	Use:   "cursor-rules",
	Short: "Generate Cursor .cursorrules content for SLB safety policy",
	RunE: func(cmd *cobra.Command, args []string) error {
		install, _ := cmd.Flags().GetBool("install")
		preview, _ := cmd.Flags().GetBool("preview")
		appendMode, _ := cmd.Flags().GetBool("append")
		replaceMode, _ := cmd.Flags().GetBool("replace")

		// Default behavior: preview if neither explicitly chosen.
		if !install && !preview {
			preview = true
		}

		mode := integrations.CursorRulesAppend
		if replaceMode {
			mode = integrations.CursorRulesReplace
		} else if !appendMode {
			// If explicitly disabled, default to replace-like behavior (upsert).
			mode = integrations.CursorRulesReplace
		}

		projectDir := flagProject
		if projectDir == "" {
			if env := os.Getenv("SLB_PROJECT"); env != "" {
				projectDir = env
			} else {
				wd, err := os.Getwd()
				if err != nil {
					return err
				}
				projectDir = wd
			}
		}

		path := filepath.Join(projectDir, ".cursorrules")

		var existing string
		if b, err := os.ReadFile(path); err == nil {
			existing = string(b)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("reading %s: %w", path, err)
		}

		next, _ := integrations.ApplyCursorRules(existing, mode)

		if preview {
			fmt.Print(next)
			return nil
		}

		if !install {
			return nil
		}

		if err := os.WriteFile(path, []byte(next), 0600); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}

		fmt.Fprintf(os.Stderr, "Wrote %s\n", path)
		return nil
	},
}

var claudeHooksCmd = &cobra.Command{
	Use:   "claude-hooks",
	Short: "Generate Claude Code hooks.json for SLB integration",
	Long: `Generate or install Claude Code hooks that intercept dangerous commands.

When installed, Claude Code will check commands with 'slb patterns test' before execution
and prompt for slb approval when the command requires review.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		install, _ := cmd.Flags().GetBool("install")
		preview, _ := cmd.Flags().GetBool("preview")
		merge, _ := cmd.Flags().GetBool("merge")

		// Default behavior: preview if neither explicitly chosen.
		if !install && !preview {
			preview = true
		}

		projectDir := flagProject
		if projectDir == "" {
			if env := os.Getenv("SLB_PROJECT"); env != "" {
				projectDir = env
			} else {
				wd, err := os.Getwd()
				if err != nil {
					return err
				}
				projectDir = wd
			}
		}

		if preview {
			hooks := integrations.DefaultClaudeHooks()
			data, err := integrations.MarshalClaudeHooks(hooks)
			if err != nil {
				return fmt.Errorf("marshaling hooks: %w", err)
			}
			fmt.Println(string(data))
			return nil
		}

		if !install {
			return nil
		}

		path, merged, err := integrations.InstallClaudeHooks(projectDir, merge)
		if err != nil {
			return fmt.Errorf("installing hooks: %w", err)
		}

		if merged {
			fmt.Fprintf(os.Stderr, "Merged SLB hooks into %s\n", path)
		} else {
			fmt.Fprintf(os.Stderr, "Wrote %s\n", path)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(integrationsCmd)

	integrationsCmd.AddCommand(cursorRulesCmd)
	cursorRulesCmd.Flags().Bool("install", false, "Write to .cursorrules in the project directory")
	cursorRulesCmd.Flags().Bool("preview", false, "Print what would be written")
	cursorRulesCmd.Flags().Bool("append", true, "Append section if missing (default)")
	cursorRulesCmd.Flags().Bool("replace", false, "Replace existing slb section")

	integrationsCmd.AddCommand(claudeHooksCmd)
	claudeHooksCmd.Flags().Bool("install", false, "Write to .claude/hooks.json in the project directory")
	claudeHooksCmd.Flags().Bool("preview", false, "Print what would be written (default)")
	claudeHooksCmd.Flags().Bool("merge", true, "Merge with existing hooks.json (default true)")
}

