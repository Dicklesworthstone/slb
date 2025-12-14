package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var (
	flagEmergencyReason  string
	flagEmergencyYes     bool
	flagEmergencyAck     string
	flagEmergencyCapture bool
	flagEmergencyTimeout int
	flagEmergencyLogDir  string
)

func init() {
	emergencyCmd.Flags().StringVarP(&flagEmergencyReason, "reason", "r", "", "reason for emergency execution (required)")
	emergencyCmd.Flags().BoolVarP(&flagEmergencyYes, "yes", "y", false, "skip interactive confirmation")
	emergencyCmd.Flags().StringVar(&flagEmergencyAck, "ack", "", "command hash acknowledgment (required with --yes)")
	emergencyCmd.Flags().BoolVar(&flagEmergencyCapture, "capture-rollback", false, "capture state for rollback")
	emergencyCmd.Flags().IntVarP(&flagEmergencyTimeout, "timeout", "t", 300, "execution timeout in seconds")
	emergencyCmd.Flags().StringVar(&flagEmergencyLogDir, "log-dir", ".slb/logs", "directory for execution logs")

	rootCmd.AddCommand(emergencyCmd)
}

var emergencyCmd = &cobra.Command{
	Use:   "emergency-execute \"<command>\"",
	Short: "Execute a command without approval (human override)",
	Long: `Execute a command bypassing the normal approval process.

This is a HUMAN OVERRIDE for emergency situations. It requires:
- A mandatory reason explaining why the bypass is necessary
- Interactive confirmation OR --yes with --ack containing the command hash

The command is extensively logged for audit purposes.

Examples:
  slb emergency-execute "rm -rf /tmp/broken" -r "System emergency"
  slb emergency-execute "kubectl delete pod stuck-pod" -r "Pod restart needed" --yes --ack abc123

To get the command hash for --ack, run:
  echo -n "your command" | sha256sum | cut -d' ' -f1`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		command := args[0]

		// Validate required flags
		if flagEmergencyReason == "" {
			return fmt.Errorf("--reason is required for emergency execution")
		}

		// Compute command hash
		hash := sha256.Sum256([]byte(command))
		commandHash := hex.EncodeToString(hash[:])

		// Validate confirmation
		if flagEmergencyYes {
			// Non-interactive mode requires --ack
			if flagEmergencyAck == "" {
				return fmt.Errorf("--ack is required when using --yes")
			}
			// Verify hash (allow first 8+ chars)
			if len(flagEmergencyAck) < 8 {
				return fmt.Errorf("--ack must be at least 8 characters of the command hash")
			}
			if !strings.HasPrefix(commandHash, flagEmergencyAck) {
				return fmt.Errorf("--ack hash does not match command (expected prefix: %s)", commandHash[:8])
			}
		} else {
			// Interactive confirmation
			fmt.Println("=== EMERGENCY EXECUTION ===")
			fmt.Printf("Command: %s\n", command)
			fmt.Printf("Reason:  %s\n", flagEmergencyReason)
			fmt.Printf("Hash:    %s\n", commandHash)
			fmt.Println()
			fmt.Println("This bypasses the approval process. Are you sure?")
			fmt.Print("Type 'EXECUTE' to confirm: ")

			reader := bufio.NewReader(os.Stdin)
			input, err := reader.ReadString('\n')
			if err != nil {
				return fmt.Errorf("reading confirmation: %w", err)
			}

			input = strings.TrimSpace(input)
			if input != "EXECUTE" {
				return fmt.Errorf("execution cancelled")
			}
		}

		// Open database for logging
		dbConn, err := db.OpenAndMigrate(GetDB())
		if err != nil {
			return fmt.Errorf("opening database: %w", err)
		}
		defer dbConn.Close()

		// Get current working directory
		cwd, err := os.Getwd()
		if err != nil {
			cwd = "."
		}

		project, err := projectPath()
		if err != nil {
			return err
		}

		cfg, err := config.Load(config.LoadOptions{
			ProjectDir: project,
			ConfigPath: flagConfig,
		})
		if err != nil {
			return fmt.Errorf("loading config: %w", err)
		}

		// Build command spec
		cmdSpec := &db.CommandSpec{
			Raw:   command,
			Cwd:   cwd,
			Shell: true, // Emergency commands always use shell
			Hash:  commandHash,
		}

		var rollbackPath string
		if flagEmergencyCapture {
			if !cfg.General.EnableRollbackCapture {
				return fmt.Errorf("rollback capture is disabled by config (general.enable_rollback_capture=false)")
			}
			rollbackReq := &db.Request{
				ID:          uuid.NewString(),
				ProjectPath: project,
				Command:     *cmdSpec,
			}
			data, err := core.CaptureRollbackState(context.Background(), rollbackReq, core.RollbackCaptureOptions{
				MaxSizeBytes: int64(cfg.General.MaxRollbackSizeMB) * 1024 * 1024,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: rollback capture failed: %v\n", err)
			} else if data == nil {
				fmt.Fprintf(os.Stderr, "warning: rollback capture unsupported for this command type\n")
			} else {
				rollbackPath = data.RollbackPath
			}
		}

		// Create log file
		logDir := flagEmergencyLogDir
		if err := os.MkdirAll(logDir, 0755); err != nil {
			return fmt.Errorf("creating log dir: %w", err)
		}

		timestamp := time.Now().Format("20060102-150405")
		logPath := fmt.Sprintf("%s/emergency_%s.log", logDir, timestamp)

		// Log the emergency execution
		logFile, err := os.Create(logPath)
		if err != nil {
			return fmt.Errorf("creating log file: %w", err)
		}
		defer logFile.Close()

		fmt.Fprintf(logFile, "=== EMERGENCY EXECUTION ===\n")
		fmt.Fprintf(logFile, "Time:    %s\n", time.Now().Format(time.RFC3339))
		fmt.Fprintf(logFile, "Actor:   %s\n", GetActor())
		fmt.Fprintf(logFile, "Command: %s\n", command)
		fmt.Fprintf(logFile, "Hash:    %s\n", commandHash)
		fmt.Fprintf(logFile, "Reason:  %s\n", flagEmergencyReason)
		fmt.Fprintf(logFile, "CWD:     %s\n", cwd)
		fmt.Fprintf(logFile, "============================\n\n")

		// Execute the command
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(flagEmergencyTimeout)*time.Second)
		defer cancel()

		var streamWriter *os.File
		if GetOutput() != "json" {
			streamWriter = os.Stdout
		}
		result, err := core.RunCommand(ctx, cmdSpec, logPath, streamWriter)

		// Build output
		type emergencyResult struct {
			Command    string `json:"command"`
			Hash       string `json:"hash"`
			ExitCode   int    `json:"exit_code"`
			DurationMs int64  `json:"duration_ms"`
			LogPath    string `json:"log_path"`
			RollbackPath string `json:"rollback_path,omitempty"`
			Reason     string `json:"reason"`
			Actor      string `json:"actor"`
			ExecutedAt string `json:"executed_at"`
			Error      string `json:"error,omitempty"`
		}

		resp := emergencyResult{
			Command:    command,
			Hash:       commandHash,
			LogPath:    logPath,
			RollbackPath: rollbackPath,
			Reason:     flagEmergencyReason,
			Actor:      GetActor(),
			ExecutedAt: time.Now().Format(time.RFC3339),
		}

		if result != nil {
			resp.ExitCode = result.ExitCode
			resp.DurationMs = result.Duration.Milliseconds()
		}

		if err != nil {
			resp.Error = err.Error()
		}

		out := output.New(output.Format(GetOutput()))
		if GetOutput() == "json" {
			if writeErr := out.Write(resp); writeErr != nil {
				return writeErr
			}
			return err
		}

		// Human-readable output
		fmt.Println()
		if err != nil {
			fmt.Printf("Emergency execution failed: %s\n", err)
		} else {
			fmt.Printf("Emergency execution completed\n")
			fmt.Printf("Exit code: %d\n", resp.ExitCode)
			fmt.Printf("Duration: %dms\n", resp.DurationMs)
		}
		fmt.Printf("Log: %s\n", resp.LogPath)
		if resp.RollbackPath != "" {
			fmt.Printf("Rollback: %s\n", resp.RollbackPath)
		}
		fmt.Println()
		fmt.Println("Note: This execution was logged for audit purposes.")

		if err != nil {
			return err
		}
		return nil
	},
}
