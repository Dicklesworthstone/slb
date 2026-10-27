// Package cli implements the Cobra command-line interface for SLB.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/Dicklesworthstone/slb/internal/daemon"
	"github.com/Dicklesworthstone/slb/internal/db"
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
	Short: "Simultaneous Launch Button - Review workflow for dangerous commands",
	Long: `SLB coordinates review of dangerous commands.

Before running commands like 'rm -rf', 'git push --force', or 'kubectl delete',
the default independent policy requires independently authenticated approval.
The local backend has no external identity authority and refuses these approvals.

Explicitly setting general.approval_policy = "same_host" enables cooperating
local sessions to peer-review commands for mistake prevention. Same-host review
is low assurance, not independently authenticated authorization or protection
against a malicious process sharing the operating-system account.

Commands are classified by risk level:
  CRITICAL   - Requires 2+ reviews under the selected policy
  DANGEROUS  - Requires 1 review under the selected policy
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
		return enterProjectDir()
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
		dbPath, err := GetDB()
		if err != nil {
			return err
		}
		projectPath, err := projectPath()
		if err != nil {
			return err
		}

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

// enterProjectDir changes into -C/--project and keeps the flag absolute.
// projectPath and GetDB read flagProject after the change, so a relative
// value would resolve a second time from inside the project: -C sub opened
// sub/sub/.slb/state.db and recorded "sub" as the session's project.
func enterProjectDir() error {
	if flagProject == "" {
		return nil
	}
	abs, err := filepath.Abs(flagProject)
	if err != nil {
		return fmt.Errorf("resolving project directory %s: %w", flagProject, err)
	}
	if err := os.Chdir(abs); err != nil {
		return fmt.Errorf("changing directory to %s: %w", flagProject, err)
	}
	flagProject = abs
	return nil
}

// selectedProjectDir selects the discovery starting directory without changing
// command cwd. Only enterProjectDir applies -C's explicit directory change.
func selectedProjectDir() (string, error) {
	selected := flagProject
	if selected == "" {
		selected = os.Getenv("SLB_PROJECT")
	}
	if selected == "" {
		return os.Getwd()
	}
	abs, err := filepath.Abs(selected)
	if err != nil {
		return "", fmt.Errorf("resolving selected project directory: %w", err)
	}
	return abs, nil
}

// resolveProjectRoot first discovers physical policy/state, then preserves a
// literal root alias only when it names that same project. A symlink into a
// different project's descendant must not borrow its lexical parent's policy.
func resolveProjectRoot(start string) (string, error) {
	physical, err := daemon.PhysicalProjectRootForCWD(start)
	if err != nil {
		return "", err
	}
	for candidate := filepath.Clean(start); ; candidate = filepath.Dir(candidate) {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			return "", fmt.Errorf("resolving project identity: %w", err)
		}
		if resolved == physical {
			return candidate, nil
		}
		if filepath.Dir(candidate) == candidate {
			return physical, nil
		}
	}
}

// GetDB returns the explicitly selected database or the discovered project's
// database. Resolution failures must never select a different history store.
// Once a project marker exists, missing state may mean lost custom policy;
// only an explicit init may recreate it.
func GetDB() (string, error) {
	if flagDB != "" {
		return flagDB, nil
	}
	project, err := projectPath()
	if err != nil {
		return "", fmt.Errorf("resolving project database: %w", err)
	}
	return db.ProjectDatabasePath(project)
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
