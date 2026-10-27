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
	// No -t shorthand: -t (--toon) is owned by the root persistent flags.
	emergencyCmd.Flags().IntVar(&flagEmergencyTimeout, "timeout", 300, "execution timeout in seconds")
	emergencyCmd.Flags().StringVar(&flagEmergencyLogDir, "log-dir", "", "directory for execution logs (default: <project>/.slb/logs)")

	rootCmd.AddCommand(emergencyCmd)
}

var emergencyCmd = &cobra.Command{
	Use:   "emergency-execute \"<command>\"",
	Short: "Execute a command with an explicitly unreviewed emergency override",
	Long: `Execute a command bypassing the normal approval process.

This is an UNREVIEWED OVERRIDE for emergency situations. It requires:
- A mandatory reason explaining why the bypass is necessary
- Interactive confirmation OR --yes with --ack containing the command hash

Neither confirmation method authenticates a human or supplies peer review.
The override is recorded separately from approvals, with emergency_unreviewed
assurance, a unique private log, the actual process start, and its outcome.
Inspect it with slb audit emergency. A crash can leave an incomplete record;
inspect the log and process state before retrying.

The --ack hash remains SHA-256 of the raw command for compatibility. The audit
record separately binds the exact command, working directory, and shell mode
with command_hash. --timeout bounds preparation and execution and must be positive.

Examples:
  slb emergency-execute "rm -rf /tmp/broken" -r "System emergency"
  slb emergency-execute "kubectl delete pod stuck-pod" -r "Pod restart needed" --yes --ack abc123

To get the command hash for --ack, run:
  echo -n "your command" | sha256sum | cut -d' ' -f1`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		command := args[0]

		if strings.TrimSpace(command) == "" {
			return fmt.Errorf("command is required for emergency execution")
		}
		if strings.TrimSpace(flagEmergencyReason) == "" {
			return fmt.Errorf("--reason is required for emergency execution")
		}
		if flagEmergencyTimeout <= 0 {
			return fmt.Errorf("--timeout must be greater than zero")
		}
		timeout, err := executionTimeoutDuration(flagEmergencyTimeout)
		if err != nil {
			return err
		}
		if err := cmd.Context().Err(); err != nil {
			return err
		}
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("capturing command working directory: %w", err)
		}
		project, err := projectPath()
		if err != nil {
			return err
		}

		// Preserve the documented raw-command acknowledgment, while recording
		// a separate canonical binding that includes cwd and shell mode.
		hash := sha256.Sum256([]byte(command))
		rawCommandHash := hex.EncodeToString(hash[:])
		cmdSpec := db.CommandSpec{Raw: command, Cwd: cwd, Shell: true}
		cmdSpec.Hash = db.ComputeCommandHash(cmdSpec)
		confirmation := db.EmergencyConfirmationInteractive

		if flagEmergencyYes {
			if flagEmergencyAck == "" {
				return fmt.Errorf("--ack is required when using --yes")
			}
			if len(flagEmergencyAck) < 8 {
				return fmt.Errorf("--ack must be at least 8 characters of the command hash")
			}
			if !strings.HasPrefix(rawCommandHash, flagEmergencyAck) {
				return fmt.Errorf("--ack hash does not match command (expected prefix: %s)", rawCommandHash[:8])
			}
			confirmation = db.EmergencyConfirmationHash
		} else {
			if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "=== EMERGENCY EXECUTION ===\n%s\nAcknowledgment does not authenticate a human.\nCommand: %s\nCWD: %s\nReason: %s\nAcknowledgment hash: %s\nCommand binding: %s\nType 'EXECUTE' to confirm: ",
				emergencyReviewNotice, command, cwd, flagEmergencyReason, rawCommandHash, cmdSpec.Hash); err != nil {
				return err
			}
			reader := bufio.NewReader(cmd.InOrStdin())
			input, err := reader.ReadString('\n')
			if err != nil {
				return fmt.Errorf("reading confirmation: %w", err)
			}

			input = strings.TrimSpace(input)
			if input != "EXECUTE" {
				return fmt.Errorf("execution cancelled")
			}
		}

		// Confirmation identifies a local action, never a reviewer or human
		// authority. No request, approval, or synthetic session is created.
		database, err := GetDB()
		if err != nil {
			return err
		}
		dbConn, err := db.OpenAndMigrate(database)
		if err != nil {
			return fmt.Errorf("opening database: %w", err)
		}
		defer dbConn.Close()

		cfg, err := config.Load(config.LoadOptions{
			ProjectDir: project,
			ConfigPath: flagConfig,
		})
		if err != nil {
			return fmt.Errorf("loading config: %w", err)
		}

		ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
		defer cancel()
		id := uuid.NewString()

		var rollbackPath string
		if flagEmergencyCapture {
			if !cfg.General.EnableRollbackCapture {
				return fmt.Errorf("rollback capture is disabled by config (general.enable_rollback_capture=false)")
			}
			rollbackReq := &db.Request{
				ID:          id,
				ProjectPath: project,
				Command:     cmdSpec,
			}
			data, err := core.CaptureRollbackState(ctx, rollbackReq, core.RollbackCaptureOptions{
				MaxSizeBytes: int64(cfg.General.MaxRollbackSizeMB) * 1024 * 1024,
			})
			if err != nil {
				return fmt.Errorf("capturing emergency rollback state: %w", err)
			} else if data == nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "warning: rollback capture unsupported for this command type")
			} else {
				rollbackPath = data.RollbackPath
			}
		}

		if err := ctx.Err(); err != nil {
			return err
		}
		logDir, err := executionLogDirectory(project, flagEmergencyLogDir)
		if err != nil {
			return fmt.Errorf("resolving log directory: %w", err)
		}
		if err := os.MkdirAll(logDir, 0700); err != nil {
			return fmt.Errorf("creating log dir: %w", err)
		}

		// Exclusive creation prevents simultaneous overrides from destroying
		// each other's evidence. The shared runner appends command output.
		logFile, err := os.CreateTemp(logDir, "emergency-*.log")
		if err != nil {
			return fmt.Errorf("creating log file: %w", err)
		}
		if err := logFile.Close(); err != nil {
			return fmt.Errorf("closing new emergency log: %w", err)
		}
		record := &db.EmergencyExecution{
			ID: id, ProjectPath: project, Command: cmdSpec, RawCommandHash: rawCommandHash,
			ConfiguredApprovalPolicy: db.ApprovalPolicy(cfg.General.ApprovalPolicy),
			Actor:                    GetActor(), Reason: flagEmergencyReason, ConfirmationMethod: confirmation,
			LogPath: logFile.Name(), RollbackPath: rollbackPath,
		}
		var stream io.Writer
		if !executionOutputStructured() {
			stream = cmd.OutOrStdout()
		}
		result, runErr := executeEmergencyOverride(ctx, dbConn, record, stream)
		resp, runErr := emergencyOutcomeFor(record, result, runErr)

		out := output.New(output.Format(GetOutput()))
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		if executionOutputStructured() {
			return errors.Join(runErr, out.Write(resp))
		}
		_, writeErr := fmt.Fprintf(cmd.OutOrStdout(), "\n%s\nEmergency execution: %s\nConfigured policy (bypassed): %s\nStatus: %s\nEvidence assurance: %s\nExit code: %d\nDuration: %dms\nLog: %s\n",
			emergencyReviewNotice, resp.EmergencyID, resp.ApprovalPolicyLabel, resp.Status, resp.Assurance, resp.ExitCode, resp.DurationMs, resp.LogPath)
		if resp.RollbackPath != "" {
			_, rollbackErr := fmt.Fprintf(cmd.OutOrStdout(), "Rollback: %s\n", resp.RollbackPath)
			writeErr = errors.Join(writeErr, rollbackErr)
		}
		if runErr != nil {
			_, errorWriteErr := fmt.Fprintf(cmd.ErrOrStderr(), "Emergency execution failed: %v\n", runErr)
			writeErr = errors.Join(writeErr, errorWriteErr)
		}
		return errors.Join(runErr, writeErr)
	},
}

// executeEmergencyOverride writes a separate unreviewed intent before entering
// the ordinary process runner. A failed intent or preamble never starts a child;
// a failed startup observer can only cancel a child that has already started.
func executeEmergencyOverride(ctx context.Context, conn *db.DB, record *db.EmergencyExecution, stream io.Writer) (*core.CommandResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := conn.CreateEmergencyExecution(record); err != nil {
		return nil, fmt.Errorf("recording emergency execution intent: %w", err)
	}
	var result *core.CommandResult
	var startupErr error
	runErr := writeEmergencyEvidence(record)
	evidenceRecorded := runErr == nil
	if runErr == nil {
		result, runErr = core.RunCommandWithStartObserver(ctx, &record.Command, record.LogPath, stream, func(pid int) error {
			if err := conn.MarkEmergencyExecutionStarted(record.ID, pid); err != nil {
				startupErr = fmt.Errorf("recording emergency process start: %w", err)
				return startupErr
			}
			return nil
		})
	}
	runErr = emergencyCommandError(result, runErr)
	status := db.EmergencyStatusSucceeded
	switch {
	case startupErr != nil:
		// The runner cancels the child when its startup cannot be persisted.
		// Record the infrastructure failure, not an external cancellation.
		status = db.EmergencyStatusFailed
	case errors.Is(runErr, context.DeadlineExceeded):
		status = db.EmergencyStatusTimedOut
	case errors.Is(runErr, context.Canceled):
		status = db.EmergencyStatusCancelled
	case runErr != nil:
		status = db.EmergencyStatusFailed
	}
	outcome := db.EmergencyOutcome{}
	if result != nil {
		code, duration := result.ExitCode, result.Duration.Milliseconds()
		outcome.ExitCode, outcome.DurationMs = &code, &duration
	}
	if runErr != nil {
		outcome.Error = runErr.Error()
	}
	if err := conn.CompleteEmergencyExecution(record.ID, status, outcome); err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("recording emergency execution outcome: %w", err))
	}
	// Preserve incomplete when a final write failed. The observed child result
	// is still returned separately, including any actual nonzero exit status.
	stored, err := conn.GetEmergencyExecution(record.ID)
	if err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("reading emergency execution outcome: %w", err))
	} else {
		*record = *stored
	}
	// Preserve the upstream standalone log contract as well as the ledger.
	// Its status comes from durable readback; a failed completion remains
	// incomplete even when the observed child happened to exit successfully.
	if evidenceRecorded {
		view, outcomeErr := emergencyOutcomeFor(record, result, runErr)
		runErr = outcomeErr
		if err := writeEmergencyOutcome(record.LogPath, view); err != nil {
			runErr = errors.Join(runErr, err)
		}
	}
	return result, runErr
}

func writeEmergencyEvidence(record *db.EmergencyExecution) error {
	file, err := os.OpenFile(record.LogPath, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("opening emergency evidence log: %w", err)
	}
	evidence := emergencyEvidenceFor(record.ConfiguredApprovalPolicy)
	err = json.NewEncoder(file).Encode(map[string]any{
		"event": "emergency_execution", "phase": "attempt", "emergency_id": record.ID,
		"execution_mode": evidence.ExecutionMode, "assurance": evidence.Assurance,
		"approval_policy": evidence.ApprovalPolicy, "approval_policy_label": evidence.ApprovalPolicyLabel,
		"approval_policy_bypassed": true,
		"review_notice":            emergencyReviewNotice, "command": record.Command.Raw,
		"command_hash": record.Command.Hash, "raw_command_hash": record.RawCommandHash, "hash": record.RawCommandHash,
		"cwd": record.Command.Cwd, "project": record.ProjectPath, "actor": record.Actor,
		"reason": record.Reason, "confirmation_method": record.ConfirmationMethod,
		"recorded_at": record.RecordedAt, "timestamp": record.RecordedAt, "rollback_path": record.RollbackPath,
	})
	if err == nil {
		err = file.Sync()
	}
	if err = errors.Join(err, file.Close()); err != nil {
		return fmt.Errorf("recording emergency evidence: %w", err)
	}
	return nil
}

func writeEmergencyOutcome(path string, outcome emergencyExecutionOutcome) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("opening emergency outcome log: %w", err)
	}
	err = json.NewEncoder(file).Encode(struct {
		Event string `json:"event"`
		emergencyExecutionOutcome
	}{"emergency_execution_outcome", outcome})
	if err == nil {
		err = file.Sync()
	}
	if err = errors.Join(err, file.Close()); err != nil {
		return fmt.Errorf("recording emergency execution outcome log: %w", err)
	}
	return nil
}
