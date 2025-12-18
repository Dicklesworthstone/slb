package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

var (
	flagInitForce bool
)

func init() {
	initCmd.Flags().BoolVarP(&flagInitForce, "force", "f", false, "reinitialize even if .slb/ already exists")
	rootCmd.AddCommand(initCmd)
}

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize SLB in the current project",
	Long: `Initialize the SLB directory structure for a project.

Creates the following structure:
  .slb/
  ├── state.db         # SQLite database (WAL mode)
  ├── config.toml      # Project-specific configuration
  ├── logs/            # Execution output logs
  ├── pending/         # Materialized JSON snapshots
  ├── sessions/        # Active agent sessions
  ├── rollback/        # Captured state for rollback
  └── processed/       # Recently processed requests

Also adds .slb/ to .gitignore if not already present.`,
	RunE: runInit,
}

func runInit(cmd *cobra.Command, args []string) error {
	// Determine project directory
	projectDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	slbDir := filepath.Join(projectDir, ".slb")

	// Check if already initialized
	if info, err := os.Stat(slbDir); err == nil && info.IsDir() {
		if !flagInitForce {
			return fmt.Errorf("already initialized: %s exists (use --force to reinitialize)", slbDir)
		}
		// Force mode: continue but preserve existing data
	}

	// Create directory structure
	dirs := []string{
		slbDir,
		filepath.Join(slbDir, "logs"),
		filepath.Join(slbDir, "pending"),
		filepath.Join(slbDir, "sessions"),
		filepath.Join(slbDir, "rollback"),
		filepath.Join(slbDir, "processed"),
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("creating directory %s: %w", dir, err)
		}
	}

	// Initialize database
	dbPath := filepath.Join(slbDir, "state.db")
	database, err := db.OpenAndMigrate(dbPath)
	if err != nil {
		return fmt.Errorf("initializing database: %w", err)
	}
	database.Close()

	// Create default config.toml
	configPath := filepath.Join(slbDir, "config.toml")
	if err := writeDefaultConfig(configPath, flagInitForce); err != nil {
		return fmt.Errorf("creating config: %w", err)
	}

	// Add to .gitignore
	gitignorePath := filepath.Join(projectDir, ".gitignore")
	if err := addToGitignore(gitignorePath); err != nil {
		// Non-fatal: just warn
		fmt.Fprintf(os.Stderr, "Warning: could not update .gitignore: %v\n", err)
	}

	// Output result
	result := map[string]any{
		"initialized": true,
		"path":        slbDir,
		"database":    dbPath,
		"config":      configPath,
		"directories": []string{"logs", "pending", "sessions", "rollback", "processed"},
	}

	switch GetOutput() {
	case "json", "yaml":
		out := output.New(output.Format(GetOutput()))
		return out.Write(result)
	case "text":
		fmt.Printf("Initialized SLB in %s\n", slbDir)
		fmt.Println()
		fmt.Println("Created:")
		fmt.Printf("  %s/state.db      - SQLite database\n", ".slb")
		fmt.Printf("  %s/config.toml   - Configuration file\n", ".slb")
		fmt.Printf("  %s/logs/         - Execution logs\n", ".slb")
		fmt.Printf("  %s/pending/      - Pending request snapshots\n", ".slb")
		fmt.Printf("  %s/sessions/     - Active sessions\n", ".slb")
		fmt.Printf("  %s/rollback/     - Rollback capture data\n", ".slb")
		fmt.Printf("  %s/processed/    - Processed requests\n", ".slb")
		fmt.Println()
		fmt.Println("Next steps:")
		fmt.Println("  1. Review .slb/config.toml and customize as needed")
		fmt.Println("  2. Start a session: slb session start --agent <name>")
		fmt.Println("  3. Submit a request: slb request --command 'rm -rf ./build'")
		return nil
	default:
		return fmt.Errorf("unsupported format: %s", GetOutput())
	}
}

// writeDefaultConfig writes a default config.toml with comments.
func writeDefaultConfig(path string, force bool) error {
	// Check if config already exists
	if _, err := os.Stat(path); err == nil && !force {
		// Config exists, don't overwrite
		return nil
	}

	cfg := config.DefaultConfig()

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	// Write header comment
	header := `# SLB Configuration
# See https://github.com/Dicklesworthstone/slb for documentation.
#
# Precedence: defaults < user (~/.slb/config.toml) < project (.slb/config.toml) < env (SLB_*) < flags

`
	if _, err := f.WriteString(header); err != nil {
		return err
	}

	enc := toml.NewEncoder(f)
	enc.Indent = "  "
	return enc.Encode(cfg)
}

// addToGitignore ensures .slb/ is in .gitignore.
func addToGitignore(path string) error {
	const slbEntry = ".slb/"

	// Check if .gitignore exists and already contains .slb/
	if f, err := os.Open(path); err == nil {
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == slbEntry || line == ".slb" {
				// Already present
				return nil
			}
		}
		if err := scanner.Err(); err != nil {
			return err
		}
	}

	// Append to .gitignore
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	// Check if file is empty or ends with newline
	info, err := f.Stat()
	if err != nil {
		return err
	}

	content := ""
	if info.Size() > 0 {
		// Read last byte to check for newline
		var buf [1]byte
		if _, err := f.ReadAt(buf[:], info.Size()-1); err == nil && buf[0] != '\n' {
			content = "\n"
		}
	}
	content += "\n# SLB state (don't commit pending requests)\n" + slbEntry + "\n"

	_, err = f.WriteString(content)
	return err
}
