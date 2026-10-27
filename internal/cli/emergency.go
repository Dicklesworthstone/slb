package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

const emergencyReviewNotice = "unreviewed emergency override; no peer approval or independently authenticated authorization"

// emergencyEvidence records the configured policy as context, not as an
// authorization satisfied by this bypass. No reviewer evidence is manufactured.
type emergencyEvidence struct {
	approvalPolicyView
	ExecutionMode string             `json:"execution_mode"`
	Assurance     db.ReviewAssurance `json:"assurance"`
	ReviewNotice  string             `json:"review_notice"`
}

func init() {
	emergencyCmd.Flags().StringVarP(&flagEmergencyReason, "reason", "r", "", "reason for emergency execution (required)")
	emergencyCmd.Flags().BoolVarP(&flagEmergencyYes, "yes", "y", false, "skip interactive confirmation")
	emergencyCmd.Flags().StringVar(&flagEmergencyAck, "ack", "", "command hash acknowledgment (required with --yes)")
	emergencyCmd.Flags().BoolVar(&flagEmergencyCapture, "capture-rollback", false, "capture state for rollback")
	// No -t shorthand: -t (--toon) is owned by the root persistent flags.
	emergencyCmd.Flags().IntVar(&flagEmergencyTimeout, "timeout", 300, "execution timeout in seconds")
	emergencyCmd.Flags().StringVar(&flagEmergencyLogDir, "log-dir", ".slb/logs", "directory for execution logs")

	rootCmd.AddCommand(emergencyCmd)
}

var emergencyCmd = &cobra.Command{
	Use:   "emergency-execute \"<command>\"",
	Short: "Execute a command without approval (unreviewed emergency override)",
	Long: `Execute a command bypassing the normal approval process.

This is an UNREVIEWED OVERRIDE for emergency situations. It requires:
- A mandatory reason explaining why the bypass is necessary
- Interactive confirmation OR --yes with --ack containing the command hash

Acknowledgment does not authenticate a human or an independent reviewer.
The execution is explicitly logged as emergency_unreviewed, even when the
configured approval policy is same_host or independent.

Examples:
  slb emergency-execute "rm -rf /tmp/broken" -r "System emergency"
  slb emergency-execute "kubectl delete pod stuck-pod" -r "Pod restart needed" --yes --ack abc123

To get the command hash for --ack, run:
  echo -n "your command" | sha256sum | cut -d' ' -f1`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		command := args[0]

		// Validate required flags
		if strings.TrimSpace(flagEmergencyReason) == "" {
			return fmt.Errorf("--reason is required for emergency execution")
		}
		if flagEmergencyTimeout <= 0 {
			return fmt.Errorf("--timeout must be greater than zero")
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
			var prompt io.Writer = os.Stdout
			if executionOutputStructured() {
				prompt = os.Stderr
			}
			if _, err := fmt.Fprintf(prompt, "=== EMERGENCY EXECUTION ===\nCommand: %s\nReason:  %s\nHash:    %s\n\nReview: %s\nAcknowledgment does not authenticate a human. Are you sure?\nType 'EXECUTE' to confirm: ",
				command, flagEmergencyReason, commandHash, emergencyReviewNotice); err != nil {
				return fmt.Errorf("writing emergency confirmation: %w", err)
			}

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
		evidence := emergencyEvidence{
			approvalPolicyView: policyView(db.ApprovalPolicy(cfg.General.ApprovalPolicy)),
			ExecutionMode:      "emergency",
			Assurance:          db.AssuranceEmergency,
			ReviewNotice:       emergencyReviewNotice,
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
		if err := os.MkdirAll(logDir, 0700); err != nil {
			return fmt.Errorf("creating log dir: %w", err)
		}

		// Every invocation owns a new log, including concurrent executions in
		// the same second. Never truncate an earlier emergency's audit evidence.
		logFile, err := os.CreateTemp(logDir, "emergency_"+time.Now().UTC().Format("20060102-150405")+"_*.log")
		if err != nil {
			return fmt.Errorf("creating log file: %w", err)
		}
		logPath := logFile.Name()
		attemptedAt := time.Now().UTC()
		// Persist the bypass classification before starting any command. JSON
		// escaping keeps newlines in a command/reason inside this one record.
		err = json.NewEncoder(logFile).Encode(struct {
			emergencyEvidence
			Event   string    `json:"event"`
			Phase   string    `json:"phase"`
			Time    time.Time `json:"timestamp"`
			Actor   string    `json:"actor"`
			Command string    `json:"command"`
			Hash    string    `json:"hash"`
			Reason  string    `json:"reason"`
			CWD     string    `json:"cwd"`
		}{evidence, "emergency_execution", "attempt", attemptedAt, GetActor(), command, commandHash, flagEmergencyReason, cwd})
		if err == nil {
			err = logFile.Sync()
		}
		if err = errors.Join(err, logFile.Close()); err != nil {
			return fmt.Errorf("recording emergency execution evidence: %w", err)
		}

		// Execute the command
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(flagEmergencyTimeout)*time.Second)
		defer cancel()

		var streamWriter io.Writer
		if !executionOutputStructured() {
			streamWriter = os.Stdout
		}
		startedAt := time.Now().UTC()
		result, err := core.RunCommand(ctx, cmdSpec, logPath, streamWriter)

		// Build output
		type emergencyResult struct {
			emergencyEvidence
			Command      string `json:"command"`
			Hash         string `json:"hash"`
			ExitCode     int    `json:"exit_code"`
			DurationMs   int64  `json:"duration_ms"`
			LogPath      string `json:"log_path"`
			RollbackPath string `json:"rollback_path,omitempty"`
			Reason       string `json:"reason"`
			Actor        string `json:"actor"`
			AttemptedAt  string `json:"attempted_at"`
			Executed     bool   `json:"executed"`
			ExecutedAt   string `json:"executed_at,omitempty"`
			Error        string `json:"error,omitempty"`
		}

		resp := emergencyResult{
			emergencyEvidence: evidence,
			Command:           command,
			Hash:              commandHash,
			LogPath:           logPath,
			RollbackPath:      rollbackPath,
			Reason:            flagEmergencyReason,
			Actor:             GetActor(),
			AttemptedAt:       attemptedAt.Format(time.RFC3339),
			ExitCode:          -1,
		}

		if result != nil {
			resp.Executed = true
			resp.ExecutedAt = startedAt.Format(time.RFC3339)
			resp.ExitCode = result.ExitCode
			resp.DurationMs = result.Duration.Milliseconds()
			if err == nil && result.ExitCode != 0 {
				err = commandExitError{code: result.ExitCode}
			}
		}

		if err != nil {
			resp.Error = err.Error()
		}
		// Keep the actual outcome with the attempt, including a failed process
		// start. The initial evidence alone never claims a command was started.
		outcomeLog, logErr := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0600)
		if logErr == nil {
			logErr = json.NewEncoder(outcomeLog).Encode(struct {
				Event string `json:"event"`
				emergencyResult
			}{"emergency_execution_outcome", resp})
			if logErr == nil {
				logErr = outcomeLog.Sync()
			}
			logErr = errors.Join(logErr, outcomeLog.Close())
		}
		if logErr != nil {
			err = errors.Join(err, fmt.Errorf("recording emergency execution outcome: %w", logErr))
			resp.Error = err.Error()
		}

		out := output.New(output.Format(GetOutput()))
		if executionOutputStructured() {
			if writeErr := out.Write(resp); writeErr != nil {
				return writeErr
			}
			return err
		}

		// Human-readable output
		fmt.Println()
		fmt.Printf("Review: %s\n", evidence.ReviewNotice)
		fmt.Printf("Assurance: %s\n", evidence.Assurance)
		fmt.Printf("Configured policy (bypassed): %s\n", evidence.ApprovalPolicyLabel)
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
