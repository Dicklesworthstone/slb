// Package cli implements the show command.
package cli

import (
	"fmt"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

var (
	flagShowWithReviews     bool
	flagShowWithExecution   bool
	flagShowWithAttachments bool
)

func init() {
	showCmd.Flags().BoolVar(&flagShowWithReviews, "with-reviews", true, "include full review details")
	showCmd.Flags().BoolVar(&flagShowWithExecution, "with-execution", true, "include execution details")
	showCmd.Flags().BoolVar(&flagShowWithAttachments, "with-attachments", false, "include attachment content")

	rootCmd.AddCommand(showCmd)
}

var showCmd = &cobra.Command{
	Use:   "show <request-id>",
	Short: "Show detailed information about a request",
	Long: `Show detailed information about a specific command approval request.

This shows the full request details including:
- Command and classification
- Justification
- Reviews and approvals
- Execution results (if executed)
- Attachments (with --with-attachments)`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		requestID := args[0]

		dbConn, err := db.Open(GetDB())
		if err != nil {
			return fmt.Errorf("opening database: %w", err)
		}
		defer dbConn.Close()

		// Get request with reviews
		request, reviews, err := dbConn.GetRequestWithReviews(requestID)
		if err != nil {
			return fmt.Errorf("getting request: %w", err)
		}

		// Build detailed response
		type attachmentView struct {
			Type     string         `json:"type"`
			Content  string         `json:"content,omitempty"`
			Metadata map[string]any `json:"metadata,omitempty"`
		}

		type responsesView struct {
			ReasonResponse string `json:"reason_response,omitempty"`
			EffectResponse string `json:"effect_response,omitempty"`
			GoalResponse   string `json:"goal_response,omitempty"`
			SafetyResponse string `json:"safety_response,omitempty"`
		}

		type reviewView struct {
			approvalPolicyView
			Assurance         db.ReviewAssurance `json:"assurance"`
			CommandHash       string             `json:"command_hash,omitempty"`
			SignatureVersion  int                `json:"signature_version"`
			ReviewID          string             `json:"review_id"`
			ReviewerSessionID string             `json:"reviewer_session_id"`
			ReviewerAgent     string             `json:"reviewer_agent"`
			ReviewerModel     string             `json:"reviewer_model"`
			Decision          string             `json:"decision"`
			Signature         string             `json:"signature,omitempty"`
			SignatureTime     string             `json:"signature_timestamp,omitempty"`
			Responses         *responsesView     `json:"responses,omitempty"`
			Comments          string             `json:"comments,omitempty"`
			CreatedAt         string             `json:"created_at"`
		}

		type executionView struct {
			approvalPolicyView
			Assurance           db.ReviewAssurance `json:"assurance"`
			LogPath             string             `json:"log_path,omitempty"`
			ExitCode            *int               `json:"exit_code,omitempty"`
			DurationMs          *int64             `json:"duration_ms,omitempty"`
			ExecutedAt          string             `json:"executed_at,omitempty"`
			ExecutedBySessionID string             `json:"executed_by_session_id,omitempty"`
			ExecutedByAgent     string             `json:"executed_by_agent,omitempty"`
			ExecutedByModel     string             `json:"executed_by_model,omitempty"`
		}

		type rollbackView struct {
			Path         string `json:"path,omitempty"`
			RolledBackAt string `json:"rolled_back_at,omitempty"`
		}

		type justificationView struct {
			Reason         string `json:"reason,omitempty"`
			ExpectedEffect string `json:"expected_effect,omitempty"`
			Goal           string `json:"goal,omitempty"`
			SafetyArgument string `json:"safety_argument,omitempty"`
		}

		type commandView struct {
			Raw               string   `json:"raw"`
			DisplayRedacted   string   `json:"display_redacted,omitempty"`
			Argv              []string `json:"argv,omitempty"`
			Cwd               string   `json:"cwd,omitempty"`
			Shell             bool     `json:"shell"`
			Hash              string   `json:"hash"`
			ContainsSensitive bool     `json:"contains_sensitive"`
		}

		type dryRunView struct {
			Command string `json:"command,omitempty"`
			Output  string `json:"output,omitempty"`
		}

		type showView struct {
			approvalPolicyView
			RequestID             string            `json:"request_id"`
			ProjectPath           string            `json:"project_path"`
			Command               commandView       `json:"command"`
			RiskTier              string            `json:"risk_tier"`
			Status                string            `json:"status"`
			MinApprovals          int               `json:"min_approvals"`
			RequireDifferentModel bool              `json:"require_different_model"`
			RequestorSessionID    string            `json:"requestor_session_id"`
			RequestorAgent        string            `json:"requestor_agent"`
			RequestorModel        string            `json:"requestor_model"`
			Justification         justificationView `json:"justification"`
			DryRun                *dryRunView       `json:"dry_run,omitempty"`
			Attachments           []attachmentView  `json:"attachments,omitempty"`
			Reviews               []reviewView      `json:"reviews,omitempty"`
			Execution             *executionView    `json:"execution,omitempty"`
			Rollback              *rollbackView     `json:"rollback,omitempty"`
			CreatedAt             string            `json:"created_at"`
			ResolvedAt            string            `json:"resolved_at,omitempty"`
			ExpiresAt             string            `json:"expires_at,omitempty"`
			ApprovalExpiresAt     string            `json:"approval_expires_at,omitempty"`
		}

		view := showView{
			approvalPolicyView:    policyView(request.ApprovalPolicy),
			RequestID:             request.ID,
			ProjectPath:           request.ProjectPath,
			RiskTier:              string(request.RiskTier),
			Status:                string(request.Status),
			MinApprovals:          request.MinApprovals,
			RequireDifferentModel: request.RequireDifferentModel,
			RequestorSessionID:    request.RequestorSessionID,
			RequestorAgent:        request.RequestorAgent,
			RequestorModel:        request.RequestorModel,
			CreatedAt:             request.CreatedAt.Format(time.RFC3339),
			Command: commandView{
				Raw:               request.Command.Raw,
				DisplayRedacted:   request.Command.DisplayRedacted,
				Argv:              request.Command.Argv,
				Cwd:               request.Command.Cwd,
				Shell:             request.Command.Shell,
				Hash:              request.Command.Hash,
				ContainsSensitive: request.Command.ContainsSensitive,
			},
			Justification: justificationView{
				Reason:         request.Justification.Reason,
				ExpectedEffect: request.Justification.ExpectedEffect,
				Goal:           request.Justification.Goal,
				SafetyArgument: request.Justification.SafetyArgument,
			},
		}

		// Timestamps
		if request.ResolvedAt != nil {
			view.ResolvedAt = request.ResolvedAt.Format(time.RFC3339)
		}
		if request.ExpiresAt != nil {
			view.ExpiresAt = request.ExpiresAt.Format(time.RFC3339)
		}
		if request.ApprovalExpiresAt != nil {
			view.ApprovalExpiresAt = request.ApprovalExpiresAt.Format(time.RFC3339)
		}

		// Dry run
		if request.DryRun != nil {
			view.DryRun = &dryRunView{
				Command: request.DryRun.Command,
				Output:  request.DryRun.Output,
			}
		}

		// Reviews
		if flagShowWithReviews && len(reviews) > 0 {
			view.Reviews = make([]reviewView, 0, len(reviews))
			for _, r := range reviews {
				rv := reviewView{
					approvalPolicyView: policyView(r.ApprovalPolicy),
					Assurance:          r.Assurance,
					CommandHash:        r.CommandHash,
					SignatureVersion:   r.SignatureVersion,
					ReviewID:           r.ID,
					ReviewerSessionID:  r.ReviewerSessionID,
					ReviewerAgent:      r.ReviewerAgent,
					ReviewerModel:      r.ReviewerModel,
					Decision:           string(r.Decision),
					Signature:          r.Signature,
					Comments:           r.Comments,
					CreatedAt:          r.CreatedAt.Format(time.RFC3339),
				}
				if !r.SignatureTimestamp.IsZero() {
					rv.SignatureTime = r.SignatureTimestamp.Format(time.RFC3339)
				}
				// Include responses if any field is non-empty
				if r.Responses.ReasonResponse != "" || r.Responses.EffectResponse != "" ||
					r.Responses.GoalResponse != "" || r.Responses.SafetyResponse != "" {
					rv.Responses = &responsesView{
						ReasonResponse: r.Responses.ReasonResponse,
						EffectResponse: r.Responses.EffectResponse,
						GoalResponse:   r.Responses.GoalResponse,
						SafetyResponse: r.Responses.SafetyResponse,
					}
				}
				view.Reviews = append(view.Reviews, rv)
			}
		}

		// Execution
		if flagShowWithExecution && request.Execution != nil {
			view.Execution = &executionView{
				approvalPolicyView:  policyView(request.Execution.ApprovalPolicy),
				Assurance:           request.Execution.Assurance,
				LogPath:             request.Execution.LogPath,
				ExitCode:            request.Execution.ExitCode,
				DurationMs:          request.Execution.DurationMs,
				ExecutedBySessionID: request.Execution.ExecutedBySessionID,
				ExecutedByAgent:     request.Execution.ExecutedByAgent,
				ExecutedByModel:     request.Execution.ExecutedByModel,
			}
			if request.Execution.ExecutedAt != nil {
				view.Execution.ExecutedAt = request.Execution.ExecutedAt.Format(time.RFC3339)
			}
		}

		// Rollback
		if request.Rollback != nil {
			view.Rollback = &rollbackView{
				Path: request.Rollback.Path,
			}
			if request.Rollback.RolledBackAt != nil {
				view.Rollback.RolledBackAt = request.Rollback.RolledBackAt.Format(time.RFC3339)
			}
		}

		// Attachments
		if len(request.Attachments) > 0 {
			view.Attachments = make([]attachmentView, 0, len(request.Attachments))
			for _, a := range request.Attachments {
				av := attachmentView{
					Type:     string(a.Type),
					Metadata: a.Metadata,
				}
				// Only include content if requested
				if flagShowWithAttachments {
					av.Content = a.Content
				}
				view.Attachments = append(view.Attachments, av)
			}
		}

		out := output.New(output.Format(GetOutput()))
		return out.Write(view)
	},
}
