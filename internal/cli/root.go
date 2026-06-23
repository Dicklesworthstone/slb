// Package cli implements the Cobra command-line interface for SLB.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/Dicklesworthstone/slb/internal/output"
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
	flagTOON      bool
	flagStats     bool
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
	SilenceUsage: true,
	// SilenceErrors intentionally stays false. main() exits non-zero WITHOUT
	// printing the returned error, so silencing errors here would swallow every
	// command failure into a bare `exit 1` with no output (issue #8 — run /
	// request / approve appeared completely broken when they were merely
	// rejecting bad input like a missing --session-id). With this false, cobra
	// prints a single `Error: <msg>` line to stderr for any RunE error. Handlers
	// that emit their own structured error (writeError) set SilenceErrors=true on
	// their OWN command after printing, so cobra does not reprint — no double
	// message. SilenceUsage stays true so a failure is one line, not a usage dump.
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
	RunE: func(cmd *cobra.Command, args []string) error {
		goVersion := runtime.Version()
		configPath := flagConfig
		if configPath == "" {
			home, _ := os.UserHomeDir()
			configPath = filepath.Join(home, ".slb", "config.toml")
		}
		dbPath := GetDB()
		projectPath, _ := os.Getwd()

		payload := map[string]any{
			"version":      version,
			"commit":       commit,
			"build_date":   date,
			"go_version":   goVersion,
			"config_path":  configPath,
			"db_path":      dbPath,
			"project_path": projectPath,
		}

		switch GetOutput() {
		case "json", "yaml", "toon":
			out := output.New(output.Format(GetOutput()), output.WithStats(GetStats()))
			return out.Write(payload)
		case "text":
			fmt.Printf("slb %s\n", version)
			fmt.Printf("  commit:  %s\n", commit)
			fmt.Printf("  built:   %s\n", date)
			fmt.Printf("  go:      %s\n", goVersion)
			fmt.Printf("  config:  %s\n", configPath)
			fmt.Printf("  db:      %s\n", dbPath)
			fmt.Printf("  project: %s\n", projectPath)
			return nil
		default:
			return fmt.Errorf("unsupported format: %s", GetOutput())
		}
	},
}

// Execute runs the root command.
func Execute() error {
	return rootCmd.Execute()
}

// GetOutput returns the configured output format.
// Precedence: CLI flags > SLB_OUTPUT_FORMAT env > TOON_DEFAULT_FORMAT env > default
func GetOutput() string {
	// CLI flags have highest precedence
	if flagJSON {
		return "json"
	}
	if flagTOON {
		return "toon"
	}
	if flagOutput != "text" {
		return flagOutput
	}

	// Check environment variables
	if envFormat := os.Getenv("SLB_OUTPUT_FORMAT"); envFormat != "" {
		switch envFormat {
		case "json", "yaml", "toon", "text":
			return envFormat
		}
	}
	if envFormat := os.Getenv("TOON_DEFAULT_FORMAT"); envFormat != "" {
		switch envFormat {
		case "json", "yaml", "toon", "text":
			return envFormat
		}
	}

	return flagOutput
}

// GetStats returns whether to show token savings statistics.
func GetStats() bool {
	return flagStats
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
	rootCmd.PersistentFlags().StringVarP(&flagOutput, "output", "o", "text", "output format: text, json, yaml, toon (env: SLB_OUTPUT_FORMAT, TOON_DEFAULT_FORMAT)")
	rootCmd.PersistentFlags().BoolVarP(&flagJSON, "json", "j", false, "shorthand for --output=json")
	rootCmd.PersistentFlags().BoolVarP(&flagTOON, "toon", "t", false, "shorthand for --output=toon")
	rootCmd.PersistentFlags().BoolVar(&flagStats, "stats", false, "show token savings statistics (JSON vs TOON bytes)")
	rootCmd.PersistentFlags().BoolVarP(&flagVerbose, "verbose", "v", false, "verbose output")
	rootCmd.PersistentFlags().StringVar(&flagDB, "db", "", "database path")
	rootCmd.PersistentFlags().StringVar(&flagActor, "actor", "", "actor identifier")
	rootCmd.PersistentFlags().StringVarP(&flagSessionID, "session-id", "s", "", "session ID")
	rootCmd.PersistentFlags().StringVarP(&flagProject, "project", "C", "", "project directory")

	// Add subcommands
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(sessionCmd)
}
