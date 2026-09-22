package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Dicklesworthstone/slb/internal/background"
	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/spf13/cobra"
)

func init() { rootCmd.AddCommand(newBackgroundWorkerCmd()) }

func executionTimeoutDuration(seconds int) (time.Duration, error) {
	if seconds < 0 || int64(seconds) > int64((1<<63-1)/time.Second) {
		return 0, errors.New("execution timeout is out of range")
	}
	if seconds == 0 {
		return core.DefaultExecutionTimeout, nil
	}
	return time.Duration(seconds) * time.Second, nil
}

func launchBackgroundExecution(cmd *cobra.Command, conn *db.DB, request *db.Request, sessionID, expectedHash string, timeout time.Duration, cfg config.Config) error {
	var receipt *background.Receipt
	launch := func() error {
		if expectedHash != "" && expectedHash != request.Command.Hash {
			return core.ErrCommandHashMismatch
		}
		database, err := filepath.Abs(conn.Path())
		if err != nil {
			return err
		}
		logDir, err := filepath.Abs(flagExecuteLogDir)
		if err != nil {
			return err
		}
		configPath := flagConfig
		if configPath != "" {
			configPath, err = filepath.Abs(configPath)
			if err != nil {
				return err
			}
		}
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		receipt, err = background.Launch(ctx, executable, []string{"__execute-background"}, background.Job{
			Version: background.Version, RequestID: request.ID, SessionID: sessionID,
			CommandHash: request.Command.Hash, Database: database, Project: request.ProjectPath,
			Config: configPath, LogDir: logDir, TimeoutSeconds: int64(timeout / time.Second),
			CaptureRollback: cfg.General.EnableRollbackCapture, MaxRollbackSizeMB: cfg.General.MaxRollbackSizeMB,
		})
		return err
	}
	err := launch()
	if err != nil {
		payload := map[string]any{"request_id": request.ID, "status": "not_launched", "error": err.Error()}
		var startup *background.StartError
		if errors.As(err, &startup) {
			payload["status"] = "startup_unconfirmed"
			payload["supervisor_pid"], payload["supervisor_log"] = startup.SupervisorPID, startup.SupervisorLog
		}
		if executionOutputStructured() {
			cmd.SilenceErrors = true
			return errors.Join(err, writeGitHookOutput(cmd, payload))
		}
		return err
	}
	if executionOutputStructured() {
		return writeGitHookOutput(cmd, receipt)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Started request %s in background\nCommand PID: %d\nSupervisor PID: %d\nLog: %s\nSupervisor log: %s\nCheck completion: slb status %s\n",
		receipt.RequestID, receipt.PID, receipt.SupervisorPID, receipt.LogPath, receipt.SupervisorLog, receipt.RequestID)
	return err
}

func newBackgroundWorkerCmd() *cobra.Command {
	return &cobra.Command{
		Use: "__execute-background", Hidden: true, Args: cobra.NoArgs,
		// Inherited CLI flags cannot redirect the protocol job's project. The
		// launcher passes no flags; the private job is validated independently.
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE:              runBackgroundWorker,
	}
}

func runBackgroundWorker(cmd *cobra.Command, _ []string) error {
	ctx, stopSignals := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Also bound abandoned startup when the parent disappears before it can
	// send SIGTERM. Stop this timer before accepting any long-running job.
	startupTimer := time.AfterFunc(background.StartTimeout, cancel)
	defer startupTimer.Stop()
	input, acknowledgment, err := background.OpenWorkerFiles()
	if err != nil {
		return err
	}
	defer acknowledgment.Close()
	job, err := background.ReadJob(input)
	_ = input.Close()
	if err != nil {
		_ = json.NewEncoder(acknowledgment).Encode(background.Receipt{Version: background.Version, Error: "invalid background job"})
		return err
	}
	confirmed := false
	result, runErr := executeBackgroundJob(ctx, job, func(start core.ExecutionStart) error {
		if !startupTimer.Stop() {
			return context.DeadlineExceeded
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		receipt := background.Receipt{Version: background.Version, RequestID: start.RequestID,
			CommandHash: start.CommandHash, Status: "executing", SupervisorPID: os.Getpid(), PID: start.PID, LogPath: start.LogPath}
		// Keep PIDs and startup identity recoverable even if the reply is lost.
		if err := json.NewEncoder(os.Stderr).Encode(receipt); err != nil {
			return err
		}
		if err := os.Stderr.Sync(); err != nil {
			return err
		}
		if err := json.NewEncoder(acknowledgment).Encode(receipt); err != nil {
			return err
		}
		if err := acknowledgment.Close(); err != nil {
			return err
		}
		confirmed = true
		return nil
	})
	if !confirmed {
		message := "command did not start"
		if runErr != nil {
			message = runErr.Error()
			if len(message) > 2048 {
				message = "execution refused; inspect the supervisor log"
			}
		}
		_ = json.NewEncoder(acknowledgment).Encode(background.Receipt{Version: background.Version, RequestID: job.RequestID, Error: message})
	}
	outcome := map[string]any{"event": "background_execution_finished", "request_id": job.RequestID, "startup_confirmed": confirmed}
	if result != nil {
		outcome["exit_code"], outcome["log_path"] = result.ExitCode, result.LogPath
		if result.Request != nil {
			outcome["status"] = result.Request.Status
		}
		if runErr == nil && result.ExitCode != 0 {
			runErr = commandExitError{code: result.ExitCode}
		}
	}
	if runErr != nil {
		outcome["error"] = runErr.Error()
	}
	logErr := json.NewEncoder(os.Stderr).Encode(outcome)
	return errors.Join(runErr, logErr)
}

// executeBackgroundJob owns its database until the command's terminal outcome
// is committed. It never trusts a parent-side CanExecute check as permission.
func executeBackgroundJob(ctx context.Context, job background.Job, onStarted func(core.ExecutionStart) error) (*core.ExecutionResult, error) {
	if err := job.Validate(); err != nil {
		return nil, err
	}
	if onStarted == nil {
		return nil, errors.New("background startup observer is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn, err := db.OpenWithOptions(job.Database, db.OpenOptions{CreateIfNotExists: false, InitSchema: false})
	if err != nil {
		return nil, fmt.Errorf("opening background database: %w", err)
	}
	defer conn.Close()
	request, err := conn.GetRequest(job.RequestID)
	if err != nil {
		return nil, err
	}
	if request.ProjectPath != job.Project || request.Command.Hash != job.CommandHash {
		return nil, core.ErrCommandHashMismatch
	}
	engine, err := core.LoadCommandPolicy(conn, config.LoadOptions{ProjectDir: job.Project, ConfigPath: job.Config})
	if err != nil {
		return nil, err
	}
	executor := core.NewExecutor(conn, engine).WithNotifier(buildAgentMailNotifier(job.Project))
	return executor.ExecuteApprovedRequest(ctx, core.ExecuteOptions{
		RequestID: job.RequestID, SessionID: job.SessionID, ExpectedCommandHash: job.CommandHash,
		Timeout: time.Duration(job.TimeoutSeconds) * time.Second, LogDir: job.LogDir, SuppressOutput: true,
		CaptureRollback: job.CaptureRollback, MaxRollbackSizeMB: job.MaxRollbackSizeMB, OnStarted: onStarted,
	})
}
