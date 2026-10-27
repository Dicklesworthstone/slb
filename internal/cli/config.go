package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

var (
	flagConfigGlobal bool
)

func init() {
	configCmd.PersistentFlags().BoolVar(&flagConfigGlobal, "global", false, "operate on user config (~/.slb/config.toml)")

	configCmd.AddCommand(configGetCmd)
	configCmd.AddCommand(configSetCmd)
	configCmd.AddCommand(configEditCmd)

	rootCmd.AddCommand(configCmd)
}

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Show or modify SLB configuration",
	RunE: func(cmd *cobra.Command, args []string) error {
		project, err := projectPath()
		if err != nil {
			return err
		}
		cfg, err := config.Load(config.LoadOptions{
			ProjectDir:    project,
			ConfigPath:    flagConfig,
			FlagOverrides: map[string]any{}, // placeholder for future CLI flags
		})
		if err != nil {
			return err
		}
		out := output.New(output.Format(GetOutput()))
		return out.Write(cfg)
	},
}

var configGetCmd = &cobra.Command{
	Use:   "get <key>",
	Short: "Get a specific configuration value",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		project, err := projectPath()
		if err != nil {
			return err
		}
		cfg, err := config.Load(config.LoadOptions{
			ProjectDir: project,
			ConfigPath: flagConfig,
		})
		if err != nil {
			return err
		}

		val, ok := config.GetValue(cfg, args[0])
		if !ok {
			return fmt.Errorf("unknown key %q", args[0])
		}
		out := output.New(output.Format(GetOutput()))
		return out.Write(map[string]any{
			"key":   args[0],
			"value": val,
		})
	},
}

var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a configuration value in the project (or --global) config file",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		project, err := projectPath()
		if err != nil {
			return err
		}
		userPath, projectPath := config.ConfigPaths(project, flagConfig)
		target := projectPath
		if flagConfigGlobal {
			target = userPath
		}

		value, err := config.ParseValue(args[0], args[1])
		if err != nil {
			return err
		}
		if err := config.WriteValue(target, args[0], value); err != nil {
			return err
		}

		out := output.New(output.Format(GetOutput()))
		response := map[string]any{
			"path":  target,
			"key":   args[0],
			"value": value,
		}
		if args[0] == "general.approval_policy" {
			addApprovalPolicyMetadata(response, db.ApprovalPolicy(args[1]))
		}
		return out.Write(response)
	},
}

var configEditCmd = &cobra.Command{
	Use:   "edit",
	Short: "Open the config file in $EDITOR (default: vi)",
	RunE: func(cmd *cobra.Command, args []string) error {
		project, err := projectPath()
		if err != nil {
			return err
		}
		userPath, projectPath := config.ConfigPaths(project, flagConfig)
		target := projectPath
		if flagConfigGlobal {
			target = userPath
		}

		// Ensure the file exists with at least defaults for convenience.
		if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
			if err := config.WriteValue(target, "general.min_approvals", config.DefaultConfig().General.MinApprovals); err != nil {
				return err
			}
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat %s: %w", target, err)
		}

		editor := os.Getenv("EDITOR")
		if editor == "" {
			editor = "vi"
		}
		editCmd := exec.Command(editor, target)
		editCmd.Stdin = os.Stdin
		editCmd.Stdout = os.Stdout
		editCmd.Stderr = os.Stderr
		return editCmd.Run()
	},
}
