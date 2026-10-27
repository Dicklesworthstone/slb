package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

var (
	flagExecuteTimeout    int
	flagExecuteBackground bool
	flagExecuteLogDir     string
)

func init() {
	// The executor session comes from the root persistent --session-id/-s.
	// Declaring a local --session-id here would shadow it (cobra skips the
	// persistent flag by name), so none is declared. Timeout has no -t
	// shorthand: -t remains the persistent --toon.
	executeCmd.Flags().IntVar(&flagExecuteTimeout, "timeout", 300, "execution timeout in seconds")
	executeCmd.Flags().BoolVar(&flagExecuteBackground, "background", false, "run in background, return immediately")
	executeCmd.Flags().StringVar(&flagExecuteLogDir, "log-dir", "", "directory for execution logs (default: <project>/.slb/logs)")
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

With --background, a detached supervisor keeps logs and records completion after
this CLI exits. Startup is confirmed only after the approval claim and process
start. stdin is /dev/null; interactive commands require foreground execution.
Use slb status or slb show for the eventual exit status. Startup errors may be
ambiguous: never retry the raw command or reset its approval.

Gate conditions are validated before execution:
- Request must be in APPROVED status
- Approval must not be expired
- Command hash must match (no tampering)
- Current pattern policy must not require higher tier
- Stored review evidence must match the request's approval policy
- Current policy must still explicitly permit same-host review when used

Same-host reviews are low assurance, not independently authenticated security
approvals. Restoring the independent policy disables their execution; changing
configuration never promotes existing evidence into independent authorization.

Examples:
  slb execute abc123 --session-id $SESSION_ID
  slb execute abc123 --session-id $SESSION_ID --timeout 600
  slb execute abc123 --session-id $SESSION_ID --expected-command-hash <hash>`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		requestID := args[0]
		sessionID := flagSessionID // Root persistent --session-id / -s.
		if sessionID == "" {
			sessionID = os.Getenv("SLB_SESSION_ID")
		}
		if sessionID == "" {
			return fmt.Errorf("--session-id is required")
		}
		executionTimeout, err := executionTimeoutDuration(flagExecuteTimeout)
		if err != nil {
			return err
		}
		expectedHash, err := cmd.Flags().GetString("expected-command-hash")
		if err != nil {
			return err
		}

		database, err := GetDB()
		if err != nil {
			return err
		}
		dbConn, err := db.OpenAndMigrate(database)
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
		engine, err := core.LoadCommandPolicy(dbConn, config.LoadOptions{ProjectDir: req.ProjectPath, ConfigPath: flagConfig})
		if err != nil {
			return fmt.Errorf("loading custom patterns: %w", err)
		}
		logDir, err := executionLogDirectory(req.ProjectPath, flagExecuteLogDir)
		if err != nil {
			return err
		}

		executor := core.NewExecutor(dbConn, engine).WithNotifier(buildAgentMailNotifier(req.ProjectPath, cfg))
		format := GetOutput()
		structured := format == "json" || format == "yaml" || format == "toon"
		if flagExecuteBackground {
			return launchBackgroundExecution(cmd, dbConn, req, sessionID, expectedHash, executionTimeout, cfg, logDir)
		}
		if !structured {
			fmt.Printf("Approval policy: %s\n", req.ApprovalPolicy.Label())
		}
		result, execErr := executor.ExecuteApprovedRequest(cmd.Context(), core.ExecuteOptions{
			RequestID:           requestID,
			ExpectedCommandHash: expectedHash,
			SessionID:           sessionID,
			Timeout:             executionTimeout,
			LogDir:              logDir,
			SuppressOutput:      structured,
			CaptureRollback:     cfg.General.EnableRollbackCapture,
			MaxRollbackSizeMB:   cfg.General.MaxRollbackSizeMB,
		})

		type executeResult struct {
			approvalPolicyView
			Assurance  db.ReviewAssurance `json:"assurance,omitempty"`
			RequestID  string             `json:"request_id"`
			Status     string             `json:"status"`
			ExitCode   int                `json:"exit_code"`
			DurationMs int64              `json:"duration_ms"`
			LogPath    string             `json:"log_path"`
			Output     string             `json:"output,omitempty"`
			TimedOut   bool               `json:"timed_out,omitempty"`
			Error      string             `json:"error,omitempty"`
		}
		resp := executeResult{approvalPolicyView: policyView(req.ApprovalPolicy), RequestID: requestID, Status: "not_executed", ExitCode: -1}
		if result != nil {
			resp.ExitCode = result.ExitCode
			resp.DurationMs = result.Duration.Milliseconds()
			resp.LogPath = result.LogPath
			resp.Output = result.Output
			resp.TimedOut = result.TimedOut
			if result.Request != nil {
				resp.approvalPolicyView = policyView(result.Request.ApprovalPolicy)
				resp.Status = string(result.Request.Status)
				if result.Request.Execution != nil {
					resp.Assurance = result.Request.Execution.Assurance
				}
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
		if resp.Assurance != "" {
			fmt.Printf("Evidence assurance: %s\n", resp.Assurance)
		}
		fmt.Printf("Duration: %dms\n", resp.DurationMs)
		if resp.LogPath != "" {
			fmt.Printf("Log: %s\n", resp.LogPath)
		}
		return execErr
	},
}

// Default logs belong to the request's project, not the caller's directory.
// Creating .slb/logs in a descendant would turn it into a shadow project for
// subsequent policy and daemon discovery. Explicit overrides remain relative
// to the invocation directory and are made absolute before detached execution.
func executionLogDirectory(project, override string) (string, error) {
	if override == "" {
		if project == "" {
			return "", fmt.Errorf("execution log project is required")
		}
		override = filepath.Join(project, ".slb", "logs")
	}
	path, err := filepath.Abs(override)
	if err != nil {
		return "", fmt.Errorf("resolving execution log directory: %w", err)
	}
	return path, nil
}
