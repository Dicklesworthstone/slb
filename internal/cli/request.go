// Package cli implements the request command.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

var (
	flagRequestReason         string
	flagRequestExpectedEffect string
	flagRequestGoal           string
	flagRequestSafety         string
	flagRequestRedact         []string
	flagRequestWait           bool
	flagRequestExecute        bool
	flagRequestTimeout        int
	flagRequestAttachFile     []string
	flagRequestAttachContext  []string
	flagRequestAttachScreen   []string
)

func init() {
	requestCmd.Flags().StringVar(&flagRequestReason, "reason", "", "reason/justification for the command (required for dangerous commands)")
	requestCmd.Flags().StringVar(&flagRequestExpectedEffect, "expected-effect", "", "expected effect of the command")
	requestCmd.Flags().StringVar(&flagRequestGoal, "goal", "", "goal this command helps achieve")
	requestCmd.Flags().StringVar(&flagRequestSafety, "safety", "", "safety argument (why this is safe to run)")
	requestCmd.Flags().StringSliceVar(&flagRequestRedact, "redact", nil, "regex patterns to redact from display")
	requestCmd.Flags().BoolVar(&flagRequestWait, "wait", false, "block until a decision is made")
	requestCmd.Flags().BoolVar(&flagRequestExecute, "execute", false, "execute the command if approved (use 'slb run' for atomic flow)")
	requestCmd.Flags().IntVar(&flagRequestTimeout, "timeout", 300, "timeout in seconds when waiting")
	requestCmd.Flags().StringSliceVar(&flagRequestAttachFile, "attach-file", nil, "attach file content as context")
	requestCmd.Flags().StringSliceVar(&flagRequestAttachContext, "attach-context", nil, "attach context")
	requestCmd.Flags().StringSliceVar(&flagRequestAttachScreen, "attach-screenshot", nil, "attach screenshot/image file")

	rootCmd.AddCommand(requestCmd)
}

var requestCmd = &cobra.Command{
	Use:   "request <command>",
	Short: "Create a command approval request",
	Long: `Create a new command approval request (plumbing command).

This is the lower-level command for creating approval requests.
For atomic command execution, use 'slb run' instead.

The command is classified by risk tier:
  CRITICAL   - Requires 2+ approvals
  DANGEROUS  - Requires 1 approval
  CAUTION    - Auto-approved after the configured delay, if policy permits
  SAFE       - Skipped (no request created)

Use --wait to block until approval/rejection.
Use --execute with --wait to execute after approval.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		command := args[0]
		waitTimeout, err := approvalWaitDuration(flagRequestTimeout)
		if err != nil {
			return err
		}
		if flagRequestExecute && !flagRequestWait {
			return fmt.Errorf("--execute requires --wait; use 'slb run' for atomic execution")
		}

		if flagSessionID == "" {
			return fmt.Errorf("--session-id is required to create a request")
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

		// Collect attachments from flags
		attachments, err := CollectAttachments(cmd.Context(), AttachmentFlags{
			Files:       flagRequestAttachFile,
			Contexts:    flagRequestAttachContext,
			Screenshots: flagRequestAttachScreen,
		})
		if err != nil {
			return fmt.Errorf("collecting attachments: %w", err)
		}

		// Merge the project's custom patterns into the default engine before
		// classifying. CreateRequest classifies the command against the default
		// engine to pick its risk tier; without this, custom patterns added via
		// `slb patterns add` are ignored here and commands are classified
		// against builtins only.
		if _, err := loadCustomPatternsIntoDefaultEngine(); err != nil {
			return fmt.Errorf("loading custom patterns: %w", err)
		}

		// Create the request using the core logic (config-driven rate limits + integrations).
		rl := core.NewRateLimiter(dbConn, toRateLimitConfig(cfg))
		creator := core.NewRequestCreator(dbConn, rl, nil, toRequestCreatorConfig(cfg))
		result, err := creator.CreateRequest(core.CreateRequestOptions{
			SessionID: flagSessionID,
			Command:   command,
			Cwd:       cwd,
			Justification: core.Justification{
				Reason:         flagRequestReason,
				ExpectedEffect: flagRequestExpectedEffect,
				Goal:           flagRequestGoal,
				SafetyArgument: flagRequestSafety,
			},
			Attachments:    attachments,
			RedactPatterns: flagRequestRedact,
			ProjectPath:    project,
		})
		if err != nil {
			return fmt.Errorf("creating request: %w", err)
		}

		out := output.New(output.Format(GetOutput()))

		// If skipped (safe command), return immediately
		if result.Skipped {
			return out.Write(skippedRequestResponse(result, command))
		}

		request := result.Request

		// Build response
		resp := map[string]any{
			"request_id":    request.ID,
			"status":        string(request.Status),
			"tier":          string(request.RiskTier),
			"command":       request.Command.Raw,
			"command_hash":  request.Command.Hash,
			"min_approvals": request.MinApprovals,
			"created_at":    request.CreatedAt.Format(time.RFC3339),
		}

		if request.Command.DisplayRedacted != "" {
			resp["command_redacted"] = request.Command.DisplayRedacted
		}
		if request.ExpiresAt != nil {
			resp["expires_at"] = request.ExpiresAt.Format(time.RFC3339)
		}

		// If not waiting, return now
		if !flagRequestWait {
			return out.Write(resp)
		}

		// Share daemonless policy advancement with run/status. The request's
		// own expiry is independent of this client's wait timeout.
		waited, waitErr := core.WaitForDecision(cmd.Context(), dbConn, request.ID, core.WaitOptions{
			ConfigPath: flagConfig, Timeout: waitTimeout,
		})
		if waited != nil {
			request = waited
		}

		// Update response with final status
		resp["status"] = string(request.Status)
		if request.ResolvedAt != nil {
			resp["resolved_at"] = request.ResolvedAt.Format(time.RFC3339)
		}
		if waitErr != nil {
			resp["wait_timed_out"] = errors.Is(waitErr, context.DeadlineExceeded)
			resp["error"] = waitErr.Error()
			if err := out.Write(resp); err != nil {
				return err
			}
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			return fmt.Errorf("waiting for request %s: %w", request.ID, waitErr)
		}

		// Execute if approved and --execute was specified
		if flagRequestExecute && request.Status == db.StatusApproved {
			executor := core.NewExecutor(dbConn, nil).WithNotifier(buildAgentMailNotifier(project))
			execResult, execErr := executor.ExecuteApprovedRequest(cmd.Context(), core.ExecuteOptions{
				RequestID:         request.ID,
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

			resp["executed"] = true
			resp["exit_code"] = exitCode
			resp["duration_ms"] = durationMs
			resp["log_path"] = logPath

			if execErr != nil {
				resp["execution_error"] = execErr.Error()
			}

			// Refresh status after execution.
			if updated, err := dbConn.GetRequest(request.ID); err == nil && updated != nil {
				resp["status"] = string(updated.Status)
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
				return fmt.Errorf("executing request: %w", execErr)
			}
			if exitCode != 0 {
				os.Exit(exitCode)
			}
		}

		return out.Write(resp)
	},
}

// skippedRequestResponse builds the JSON payload for a request that was skipped
// (a safe/unmatched command, so no request row was created).
//
// result.Classification is a pointer and, while the current core always sets it
// on the skipped paths, nothing in the type system guarantees that — a future
// or alternate skip path (e.g. an early skip before classification) could leave
// it nil. The old code dereferenced result.Classification.Tier unconditionally,
// a latent nil-pointer panic. Guard it: the "tier" field is included only when
// a classification is present (omitted otherwise rather than emitting a bogus
// zero value).
func skippedRequestResponse(result *core.CreateRequestResult, command string) map[string]any {
	resp := map[string]any{
		"status":  "skipped",
		"reason":  result.SkipReason,
		"command": command,
	}
	if result.Classification != nil {
		resp["tier"] = result.Classification.Tier
	}
	return resp
}
