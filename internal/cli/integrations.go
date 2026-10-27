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
	Short: "Preview or install the native SLB guard in Claude Code settings",
	Long: `Generate or install Claude Code hooks that intercept dangerous commands.

The preview is a settings.json fragment containing a Bash PreToolUse command
hook. The installed hook uses the same native guard and settings-preserving
installer as 'slb hook install'. Existing unrelated settings and hooks are kept.

Installation defaults to the selected project's .claude/settings.json.
Use --global for ~/.claude/settings.json. --merge=false resets only SLB's own
hook entry; it never replaces the settings file or other tools' hooks.

Claude Code 2.1.295 or later is required for onFailure: block, which prevents a
missing or failed guard from silently allowing the tool call.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		install, _ := cmd.Flags().GetBool("install")
		preview, _ := cmd.Flags().GetBool("preview")
		merge, _ := cmd.Flags().GetBool("merge")
		global, _ := cmd.Flags().GetBool("global")
		if install && preview {
			return fmt.Errorf("--install and --preview cannot be used together")
		}

		// Default behavior: preview if neither explicitly chosen.
		if !install && !preview {
			preview = true
		}

		if preview {
			guardCommand, err := nativeHookGuardCommand()
			if err != nil {
				return fmt.Errorf("resolving native hook command: %w", err)
			}
			hooks := integrations.DefaultClaudeHooks(guardCommand)
			data, err := integrations.MarshalClaudeHooks(hooks)
			if err != nil {
				return fmt.Errorf("marshaling hooks: %w", err)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
			return err
		}

		if !install {
			return nil
		}

		return installClaudeHook(cmd, global, !merge)
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
	claudeHooksCmd.Flags().Bool("install", false, "Install the native guard in the selected Claude Code settings.json")
	claudeHooksCmd.Flags().Bool("preview", false, "Print what would be written (default)")
	claudeHooksCmd.Flags().Bool("merge", true, "Keep SLB hook options; false resets only SLB's hook (other settings are preserved)")
	claudeHooksCmd.Flags().Bool("global", false, "Install in user settings for all projects")
}
