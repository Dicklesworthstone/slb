// Package cli implements the run command.
package cli

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

var (
	flagRunReason         string
	flagRunExpectedEffect string
	flagRunGoal           string
	flagRunSafety         string
	flagRunTimeout        int
	flagRunYield          bool
	flagRunAttachFile     []string
	flagRunAttachContext  []string
	flagRunAttachScreen   []string
)

func init() {
	runCmd.Flags().StringVar(&flagRunReason, "reason", "", "reason/justification for the command (required for dangerous commands)")
	runCmd.Flags().StringVar(&flagRunExpectedEffect, "expected-effect", "", "expected effect of the command")
	runCmd.Flags().StringVar(&flagRunGoal, "goal", "", "goal this command helps achieve")
	runCmd.Flags().StringVar(&flagRunSafety, "safety", "", "safety argument (why this is safe to run)")
	runCmd.Flags().IntVar(&flagRunTimeout, "timeout", 300, "timeout in seconds to wait for approval")
	runCmd.Flags().BoolVar(&flagRunYield, "yield", false, "yield to background if approval is needed")
	runCmd.Flags().StringSliceVar(&flagRunAttachFile, "attach-file", nil, "attach file content as context")
	runCmd.Flags().StringSliceVar(&flagRunAttachContext, "attach-context", nil, "run command and attach output as context")
	runCmd.Flags().StringSliceVar(&flagRunAttachScreen, "attach-screenshot", nil, "attach screenshot/image file")

	rootCmd.AddCommand(runCmd)
}

var runCmd = &cobra.Command{
	Use:   "run <command>",
	Short: "Run a command with approval if required",
	Long: `Run a command atomically with approval handling.

This is the primary command for executing dangerous commands with SLB.

Flow:
1. Classify the command by risk tier
2. If SAFE: execute immediately
3. If DANGEROUS/CRITICAL: create request, block, wait for approval
4. If approved: execute in caller's shell environment
5. If rejected/timeout: exit 1 with error

The command inherits the caller's environment and working directory.

Examples:
  slb run "rm -rf ./build" --reason "Clean build artifacts"
  slb run "git push --force" --reason "Rewrite history" --safety "Branch is not shared"
  slb run "kubectl delete deployment nginx" --reason "Removing unused deployment"`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		command := args[0]

		if flagSessionID == "" {
			return fmt.Errorf("--session-id is required")
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

		cwd, err := os.Getwd()
		if err != nil {
			cwd = project
		}

		dbConn, err := db.OpenAndMigrate(GetDB())
		if err != nil {
			return fmt.Errorf("opening database: %w", err)
		}
		defer dbConn.Close()

		out := output.New(output.Format(GetOutput()))

		// Collect attachments from flags
		attachments, err := CollectAttachments(cmd.Context(), AttachmentFlags{
			Files:       flagRunAttachFile,
			Contexts:    flagRunAttachContext,
			Screenshots: flagRunAttachScreen,
		})
		if err != nil {
			return writeError(out, "attachment_error", command, err)
		}

		// Step 1: Classify and create request using config-derived limits and notifiers
		rl := core.NewRateLimiter(dbConn, toRateLimitConfig(cfg))
		creator := core.NewRequestCreator(dbConn, rl, nil, toRequestCreatorConfig(cfg))
		result, err := creator.CreateRequest(core.CreateRequestOptions{
			SessionID: flagSessionID,
			Command:   command,
			Cwd:       cwd,
			Shell:     true, // run always uses shell
			Justification: core.Justification{
				Reason:         flagRunReason,
				ExpectedEffect: flagRunExpectedEffect,
				Goal:           flagRunGoal,
				SafetyArgument: flagRunSafety,
			},
			Attachments: attachments,
			ProjectPath: project,
		})
		if err != nil {
			return writeError(out, "request_failed", command, err)
		}

		// Step 2: If SAFE, execute immediately
		if result.Skipped {
			return runSafeCommand(out, command, cwd, project)
		}

		request := result.Request

		// Step 3: If yield mode and not immediately approved, return request info
		if flagRunYield && request.Status == db.StatusPending {
			return out.Write(map[string]any{
				"status":        "pending",
				"request_id":    request.ID,
				"tier":          string(request.RiskTier),
				"min_approvals": request.MinApprovals,
				"message":       "Request created, yielding to background. Check status with: slb status " + request.ID,
			})
		}

		// Step 4: Wait for approval
		deadline := time.Now().Add(time.Duration(flagRunTimeout) * time.Second)
		for time.Now().Before(deadline) {
			request, _, err = dbConn.GetRequestWithReviews(request.ID)
			if err != nil {
				return writeError(out, "poll_failed", command, err)
			}

			// Check for terminal or approved states
			if request.Status == db.StatusApproved {
				break
			}
			if request.Status.IsTerminal() {
				return writeError(out, string(request.Status), command,
					fmt.Errorf("request %s: %s", request.ID, request.Status))
			}

			time.Sleep(500 * time.Millisecond)
		}

		// Check if we timed out waiting
		if request.Status == db.StatusPending {
			// Mark as timeout
			_ = dbConn.UpdateRequestStatus(request.ID, db.StatusTimeout)
			return writeError(out, "timeout", command,
				fmt.Errorf("request %s timed out waiting for approval", request.ID))
		}

		// Step 5: Execute the approved command
		return runApprovedRequest(out, dbConn, cfg, project, request.ID)
	},
}

func runSafeCommand(out *output.Writer, command, cwd, project string) error {
	logPath, err := createRunLogFile(project, "safe")
	if err != nil {
		return writeError(out, "log_create_failed", command, err)
	}

	spec := &db.CommandSpec{
		Raw:   command,
		Cwd:   cwd,
		Shell: true,
	}
	spec.Hash = core.ComputeCommandHash(*spec)

	var streamWriter *os.File
	if GetOutput() != "json" {
		streamWriter = os.Stdout
	}

	result, execErr := core.RunCommand(context.Background(), spec, logPath, streamWriter)

	exitCode := 0
	durationMs := int64(0)
	if result != nil {
		exitCode = result.ExitCode
		durationMs = result.Duration.Milliseconds()
	}

	resp := map[string]any{
		"status":           "executed",
		"command":          command,
		"exit_code":        exitCode,
		"duration_ms":      durationMs,
		"log_path":         logPath,
		"tier":             "safe",
		"skipped_approval": true,
	}
	if execErr != nil {
		resp["error"] = execErr.Error()
	}

	if GetOutput() == "json" {
		_ = out.Write(resp)
		if execErr != nil {
			os.Exit(1)
		}
		if exitCode != 0 {
			os.Exit(exitCode)
		}
		return nil
	}

	if execErr != nil {
		fmt.Fprintf(os.Stderr, "[slb] Execution failed: %s\n", execErr.Error())
		os.Exit(1)
	}
	if exitCode != 0 {
		fmt.Fprintf(os.Stderr, "\n[slb] Command exited with code %d\n", exitCode)
		os.Exit(exitCode)
	}
	return nil
}

func runApprovedRequest(out *output.Writer, dbConn *db.DB, cfg config.Config, project, requestID string) error {
	executor := core.NewExecutor(dbConn, nil).WithNotifier(buildAgentMailNotifier(project))

	execResult, execErr := executor.ExecuteApprovedRequest(context.Background(), core.ExecuteOptions{
		RequestID:         requestID,
		SessionID:         flagSessionID,
		LogDir:            ".slb/logs",
		SuppressOutput:    GetOutput() == "json",
		CaptureRollback:   cfg.General.EnableRollbackCapture,
		MaxRollbackSizeMB: cfg.General.MaxRollbackSizeMB,
	})

	exitCode := 0
	durationMs := int64(0)
	logPath := ""
	if execResult != nil {
		exitCode = execResult.ExitCode
		durationMs = execResult.Duration.Milliseconds()
		logPath = execResult.LogPath
	}

	resp := map[string]any{
		"status":      "executed",
		"request_id":  requestID,
		"exit_code":   exitCode,
		"duration_ms": durationMs,
		"log_path":    logPath,
	}
	if execErr != nil {
		resp["error"] = execErr.Error()
	}

	if GetOutput() == "json" {
		_ = out.Write(resp)
		if execErr != nil {
			os.Exit(1)
		}
		if exitCode != 0 {
			os.Exit(exitCode)
		}
		return nil
	}

	if execErr != nil {
		fmt.Fprintf(os.Stderr, "[slb] Execution failed: %s\n", execErr.Error())
		os.Exit(1)
	}
	if exitCode != 0 {
		fmt.Fprintf(os.Stderr, "\n[slb] Command exited with code %d\n", exitCode)
		os.Exit(exitCode)
	}
	return nil
}

func createRunLogFile(project, prefix string) (string, error) {
	if prefix == "" {
		prefix = "run"
	}
	baseDir := ".slb/logs"
	if project != "" {
		baseDir = filepath.Join(project, ".slb", "logs")
	}
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return "", fmt.Errorf("creating log dir: %w", err)
	}

	timestamp := time.Now().Format("20060102-150405")
	logName := fmt.Sprintf("%s_%s.log", timestamp, prefix)
	logPath := filepath.Join(baseDir, logName)

	f, err := os.Create(logPath)
	if err != nil {
		return "", fmt.Errorf("creating log file: %w", err)
	}
	_ = f.Close()
	return logPath, nil
}

// Helpers to adapt config into core types ------------------------------------

func toRateLimitConfig(cfg config.Config) core.RateLimitConfig {
	action := core.RateLimitAction(cfg.RateLimits.RateLimitAction)
	switch action {
	case core.RateLimitActionReject, core.RateLimitActionQueue, core.RateLimitActionWarn:
	default:
		action = core.RateLimitActionReject
	}
	return core.RateLimitConfig{
		MaxPendingPerSession: cfg.RateLimits.MaxPendingPerSession,
		MaxRequestsPerMinute: cfg.RateLimits.MaxRequestsPerMinute,
		Action:               action,
	}
}

func toRequestCreatorConfig(cfg config.Config) *core.RequestCreatorConfig {
	timeoutMinutes := int(math.Ceil(float64(cfg.General.RequestTimeoutSecs) / 60.0))
	if timeoutMinutes <= 0 {
		timeoutMinutes = 30
	}
	return &core.RequestCreatorConfig{
		BlockedAgents:              cfg.Agents.Blocked,
		DynamicQuorumEnabled:       false,
		DynamicQuorumFloor:         1,
		RequestTimeoutMinutes:      timeoutMinutes,
		ApprovalTTLMinutes:         cfg.General.ApprovalTTLMins,
		ApprovalTTLCriticalMinutes: cfg.General.ApprovalTTLCriticalMins,
		AgentMailEnabled:           cfg.Integrations.AgentMailEnabled,
		AgentMailThread:            cfg.Integrations.AgentMailThread,
		AgentMailSender:            "",
	}
}

// writeError outputs an error response.
func writeError(out *output.Writer, status, command string, err error) error {
	resp := map[string]any{
		"status":  status,
		"command": command,
		"error":   err.Error(),
	}

	if GetOutput() == "json" {
		_ = out.Write(resp)
	} else {
		fmt.Fprintf(os.Stderr, "[slb] Error: %s\n", err.Error())
	}

	os.Exit(1)
	return nil // unreachable
}
