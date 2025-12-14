// Package cli implements the Cobra command-line interface for SLB.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"
)

// Version information set by goreleaser
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// Global flag values
var (
	flagConfig    string
	flagOutput    string
	flagJSON      bool
	flagVerbose   bool
	flagDB        string
	flagActor     string
	flagSessionID string
	flagProject   string
)

var rootCmd = &cobra.Command{
	Use:   "slb",
	Short: "Simultaneous Launch Button - Two-person rule for dangerous commands",
	Long: `SLB implements a two-person authorization rule for dangerous commands.

Before running commands like 'rm -rf', 'git push --force', or 'kubectl delete',
SLB requires approval from another authorized agent or human reviewer.

Commands are classified by risk level:
  CRITICAL   - Requires 2+ approvals (data destruction, production deploys)
  DANGEROUS  - Requires 1 approval (force pushes, schema changes)
  CAUTION    - Auto-approved after 30s with notification
  SAFE       - Skipped entirely (read-only commands)`,
	SilenceUsage:  true,
	SilenceErrors: true,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if flagProject == "" {
			return nil
		}
		if err := os.Chdir(flagProject); err != nil {
			return fmt.Errorf("changing directory to %s: %w", flagProject, err)
		}
		return nil
	},
	Run: func(cmd *cobra.Command, args []string) {
		// When no subcommand given, show quick reference card
		showQuickReference()
	},
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, args []string) {
		goVersion := runtime.Version()
		configPath := flagConfig
		if configPath == "" {
			home, _ := os.UserHomeDir()
			configPath = home + "/.slb/config.toml"
		}
		dbPath := GetDB()
		projectPath, _ := os.Getwd()

		if flagJSON || flagOutput == "json" {
			fmt.Printf(`{"version":"%s","commit":"%s","build_date":"%s","go_version":"%s","config_path":"%s","db_path":"%s","project_path":"%s"}`+"\n",
				version, commit, date, goVersion, configPath, dbPath, projectPath)
		} else {
			fmt.Printf("slb %s\n", version)
			fmt.Printf("  commit:  %s\n", commit)
			fmt.Printf("  built:   %s\n", date)
			fmt.Printf("  go:      %s\n", goVersion)
			fmt.Printf("  config:  %s\n", configPath)
			fmt.Printf("  db:      %s\n", dbPath)
			fmt.Printf("  project: %s\n", projectPath)
		}
	},
}

// Execute runs the root command.
func Execute() error {
	return rootCmd.Execute()
}

// GetOutput returns the configured output format.
func GetOutput() string {
	if flagJSON {
		return "json"
	}
	return flagOutput
}

// GetDB returns the database path.
func GetDB() string {
	if flagDB != "" {
		return flagDB
	}
	project, err := projectPath()
	if err == nil && project != "" {
		return filepath.Join(project, ".slb", "state.db")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".slb", "history.db")
}

// GetActor returns the actor identifier.
func GetActor() string {
	if flagActor != "" {
		return flagActor
	}
	// Try environment detection
	if actor := os.Getenv("SLB_ACTOR"); actor != "" {
		return actor
	}
	if actor := os.Getenv("AGENT_NAME"); actor != "" {
		return actor
	}
	// Fallback to username@hostname
	user := os.Getenv("USER")
	if user == "" {
		user = "unknown"
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "localhost"
	}
	return user + "@" + host
}

func init() {
	// Global flags with short aliases as specified in plan
	rootCmd.PersistentFlags().StringVarP(&flagConfig, "config", "c", "", "config file path")
	rootCmd.PersistentFlags().StringVarP(&flagOutput, "output", "o", "text", "output format: text, json, yaml")
	rootCmd.PersistentFlags().BoolVarP(&flagJSON, "json", "j", false, "shorthand for --output=json")
	rootCmd.PersistentFlags().BoolVarP(&flagVerbose, "verbose", "v", false, "verbose output")
	rootCmd.PersistentFlags().StringVar(&flagDB, "db", "", "database path")
	rootCmd.PersistentFlags().StringVar(&flagActor, "actor", "", "actor identifier")
	rootCmd.PersistentFlags().StringVarP(&flagSessionID, "session-id", "s", "", "session ID")
	rootCmd.PersistentFlags().StringVarP(&flagProject, "project", "C", "", "project directory")

	// Add subcommands
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(sessionCmd)
}
