package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/integrations"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

var (
	flagExecuteSessionID  string
	flagExecuteTimeout    int
	flagExecuteBackground bool
	flagExecuteLogDir     string
)

func init() {
	executeCmd.Flags().StringVarP(&flagExecuteSessionID, "session-id", "s", "", "executor session ID (required)")
	executeCmd.Flags().IntVarP(&flagExecuteTimeout, "timeout", "t", 300, "execution timeout in seconds")
	executeCmd.Flags().BoolVar(&flagExecuteBackground, "background", false, "run in background, return immediately")
	executeCmd.Flags().StringVar(&flagExecuteLogDir, "log-dir", ".slb/logs", "directory for execution logs")
	// Reuse Agent Mail notifier builder from approve/reject
	_ = integrations.NoopNotifier{} // keep import if build tags change

	rootCmd.AddCommand(executeCmd)
}

var executeCmd = &cobra.Command{
	Use:   "execute <request-id>",
	Short: "Execute an approved request",
	Long: `Execute an approved command request.

The command runs in your current shell environment, inheriting all environment
variables (AWS credentials, KUBECONFIG, virtualenv, etc.).

Gate conditions are validated before execution:
- Request must be in APPROVED status
- Approval must not be expired
- Command hash must match (no tampering)
- Current pattern policy must not require higher tier

Examples:
  slb execute abc123 -s $SESSION_ID
  slb execute abc123 -s $SESSION_ID --timeout 600
  slb execute abc123 -s $SESSION_ID --background`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		requestID := args[0]

		// Validate required flags
		if flagExecuteSessionID == "" {
			return fmt.Errorf("--session-id is required")
		}

		// Open database
		dbConn, err := db.OpenAndMigrate(GetDB())
		if err != nil {
			return fmt.Errorf("opening database: %w", err)
		}
		defer dbConn.Close()

		// Load config based on the request's project path (not just CWD).
		req, err := dbConn.GetRequest(requestID)
		if err != nil {
			return fmt.Errorf("getting request: %w", err)
		}
		cfg, err := config.Load(config.LoadOptions{
			ProjectDir: req.ProjectPath,
			ConfigPath: flagConfig,
		})
		if err != nil {
			return fmt.Errorf("loading config: %w", err)
		}

		// Create executor
		executor := core.NewExecutor(dbConn, nil).WithNotifier(buildAgentMailNotifier(req.ProjectPath))

		// Check if we can execute first
		canExec, reason := executor.CanExecute(requestID)
		if !canExec {
			return fmt.Errorf("cannot execute: %s", reason)
		}

		// Build options
		opts := core.ExecuteOptions{
			RequestID:         requestID,
			SessionID:         flagExecuteSessionID,
			Timeout:           time.Duration(flagExecuteTimeout) * time.Second,
			Background:        flagExecuteBackground,
			LogDir:            flagExecuteLogDir,
			SuppressOutput:    GetOutput() == "json",
			CaptureRollback:   cfg.General.EnableRollbackCapture,
			MaxRollbackSizeMB: cfg.General.MaxRollbackSizeMB,
		}

		// Execute
		ctx := context.Background()
		result, err := executor.ExecuteApprovedRequest(ctx, opts)

		// Build output
		type executeResult struct {
			RequestID  string `json:"request_id"`
			ExitCode   int    `json:"exit_code"`
			DurationMs int64  `json:"duration_ms"`
			LogPath    string `json:"log_path"`
			TimedOut   bool   `json:"timed_out,omitempty"`
			Error      string `json:"error,omitempty"`
		}

		resp := executeResult{
			RequestID: requestID,
		}

		if result != nil {
			resp.ExitCode = result.ExitCode
			resp.DurationMs = result.Duration.Milliseconds()
			resp.LogPath = result.LogPath
			resp.TimedOut = result.TimedOut
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
		if err != nil {
			fmt.Printf("Execution failed: %s\n", err)
			if result != nil && result.LogPath != "" {
				fmt.Printf("Log: %s\n", result.LogPath)
			}
			return err
		}

		fmt.Printf("Executed request %s\n", requestID)
		fmt.Printf("Exit code: %d\n", resp.ExitCode)
		fmt.Printf("Duration: %dms\n", resp.DurationMs)
		fmt.Printf("Log: %s\n", resp.LogPath)

		return nil
	},
}
