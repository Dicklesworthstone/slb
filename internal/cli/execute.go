package cli

import (
	"fmt"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
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
	// -s and -t belong to root persistent flags.
	executeCmd.Flags().StringVar(&flagExecuteSessionID, "session-id", "", "executor session ID (required)")
	executeCmd.Flags().IntVar(&flagExecuteTimeout, "timeout", 300, "execution timeout in seconds")
	executeCmd.Flags().BoolVar(&flagExecuteBackground, "background", false, "run in background, return immediately")
	executeCmd.Flags().StringVar(&flagExecuteLogDir, "log-dir", ".slb/logs", "directory for execution logs")
	executeCmd.Flags().String("expected-command-hash", "", "require the command hash observed before handing off execution")
	rootCmd.AddCommand(executeCmd)
}

// commandExitError lets the entry point preserve a child's nonzero exit status
// without bypassing deferred database/log cleanup with os.Exit in a handler.
type commandExitError struct{ code int }

func (e commandExitError) Error() string { return fmt.Sprintf("command exited with code %d", e.code) }
func (e commandExitError) ExitCode() int {
	if e.code < 1 || e.code > 255 {
		return 1
	}
	return e.code
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
  slb execute abc123 --session-id $SESSION_ID
  slb execute abc123 --session-id $SESSION_ID --timeout 600
  slb execute abc123 --session-id $SESSION_ID --expected-command-hash <hash>`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		requestID := args[0]
		if flagExecuteSessionID == "" {
			return fmt.Errorf("--session-id is required")
		}
		expectedHash, err := cmd.Flags().GetString("expected-command-hash")
		if err != nil {
			return err
		}

		dbConn, err := db.OpenAndMigrate(GetDB())
		if err != nil {
			return fmt.Errorf("opening database: %w", err)
		}
		defer dbConn.Close()

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
		if _, err := loadCustomPatternsIntoDefaultEngine(); err != nil {
			return fmt.Errorf("loading custom patterns: %w", err)
		}

		executor := core.NewExecutor(dbConn, nil).WithNotifier(buildAgentMailNotifier(req.ProjectPath))
		format := GetOutput()
		structured := format == "json" || format == "yaml" || format == "toon"
		result, execErr := executor.ExecuteApprovedRequest(cmd.Context(), core.ExecuteOptions{
			RequestID:           requestID,
			ExpectedCommandHash: expectedHash,
			SessionID:           flagExecuteSessionID,
			Timeout:             time.Duration(flagExecuteTimeout) * time.Second,
			Background:          flagExecuteBackground,
			LogDir:              flagExecuteLogDir,
			SuppressOutput:      structured,
			CaptureRollback:     cfg.General.EnableRollbackCapture,
			MaxRollbackSizeMB:   cfg.General.MaxRollbackSizeMB,
		})

		type executeResult struct {
			RequestID  string `json:"request_id"`
			Status     string `json:"status"`
			ExitCode   int    `json:"exit_code"`
			DurationMs int64  `json:"duration_ms"`
			LogPath    string `json:"log_path"`
			Output     string `json:"output,omitempty"`
			TimedOut   bool   `json:"timed_out,omitempty"`
			Error      string `json:"error,omitempty"`
		}
		resp := executeResult{RequestID: requestID, Status: "not_executed", ExitCode: -1}
		if result != nil {
			resp.ExitCode = result.ExitCode
			resp.DurationMs = result.Duration.Milliseconds()
			resp.LogPath = result.LogPath
			resp.Output = result.Output
			resp.TimedOut = result.TimedOut
			if result.Request != nil {
				resp.Status = string(result.Request.Status)
			}
		}
		if execErr == nil && resp.ExitCode != 0 {
			execErr = commandExitError{code: resp.ExitCode}
		}
		if execErr != nil {
			resp.Error = execErr.Error()
		}

		if structured {
			out := output.New(output.Format(format))
			if err := out.Write(resp); err != nil {
				return err
			}
			return execErr
		}
		if execErr != nil {
			fmt.Printf("Execution failed: %s\n", execErr)
		} else {
			fmt.Printf("Executed request %s\n", requestID)
		}
		fmt.Printf("Exit code: %d\n", resp.ExitCode)
		fmt.Printf("Duration: %dms\n", resp.DurationMs)
		if resp.LogPath != "" {
			fmt.Printf("Log: %s\n", resp.LogPath)
		}
		return execErr
	},
}
