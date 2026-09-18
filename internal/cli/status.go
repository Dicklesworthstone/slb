// Package cli implements the status command.
package cli

import (
	"fmt"
	"time"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

var (
	flagStatusWait bool
)

func init() {
	statusCmd.Flags().BoolVar(&flagStatusWait, "wait", false, "block until a decision is made")

	rootCmd.AddCommand(statusCmd)
}

var statusCmd = &cobra.Command{
	Use:   "status <request-id>",
	Short: "Show status of a request",
	Long: `Show the current status of a command approval request.

Use --wait to block until the request leaves pending (including approval,
rejection, timeout or escalation). Due policy is processed without a daemon.`,
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

		// Approval is a decision, not a lifecycle terminal state. Do not wait
		// for some other client to execute it before returning to this caller.
		if flagStatusWait && request.Status == db.StatusPending {
			if _, err := core.WaitForDecision(cmd.Context(), dbConn, requestID, core.WaitOptions{ConfigPath: flagConfig}); err != nil {
				return fmt.Errorf("waiting for request: %w", err)
			}
			request, reviews, err = dbConn.GetRequestWithReviews(requestID)
			if err != nil {
				return fmt.Errorf("getting resolved request: %w", err)
			}
		}

		// Build response
		type reviewView struct {
			ReviewID  string `json:"review_id"`
			Reviewer  string `json:"reviewer"`
			Model     string `json:"model"`
			Decision  string `json:"decision"`
			Comments  string `json:"comments,omitempty"`
			CreatedAt string `json:"created_at"`
		}

		type statusView struct {
			RequestID             string       `json:"request_id"`
			Command               string       `json:"command"`
			CommandRedacted       string       `json:"command_redacted,omitempty"`
			CommandHash           string       `json:"command_hash"`
			Cwd                   string       `json:"cwd,omitempty"`
			RiskTier              string       `json:"risk_tier"`
			Status                string       `json:"status"`
			MinApprovals          int          `json:"min_approvals"`
			RequireDifferentModel bool         `json:"require_different_model"`
			RequestorAgent        string       `json:"requestor_agent"`
			RequestorModel        string       `json:"requestor_model"`
			ProjectPath           string       `json:"project_path"`
			Reason                string       `json:"reason,omitempty"`
			ExpectedEffect        string       `json:"expected_effect,omitempty"`
			Goal                  string       `json:"goal,omitempty"`
			SafetyArgument        string       `json:"safety_argument,omitempty"`
			CreatedAt             string       `json:"created_at"`
			ResolvedAt            string       `json:"resolved_at,omitempty"`
			ExpiresAt             string       `json:"expires_at,omitempty"`
			ApprovalExpiresAt     string       `json:"approval_expires_at,omitempty"`
			ApprovalCount         int          `json:"approval_count"`
			RejectionCount        int          `json:"rejection_count"`
			Reviews               []reviewView `json:"reviews"`
		}

		view := statusView{
			RequestID:             request.ID,
			Command:               request.Command.Raw,
			CommandHash:           request.Command.Hash,
			Cwd:                   request.Command.Cwd,
			RiskTier:              string(request.RiskTier),
			Status:                string(request.Status),
			MinApprovals:          request.MinApprovals,
			RequireDifferentModel: request.RequireDifferentModel,
			RequestorAgent:        request.RequestorAgent,
			RequestorModel:        request.RequestorModel,
			ProjectPath:           request.ProjectPath,
			Reason:                request.Justification.Reason,
			ExpectedEffect:        request.Justification.ExpectedEffect,
			Goal:                  request.Justification.Goal,
			SafetyArgument:        request.Justification.SafetyArgument,
			CreatedAt:             request.CreatedAt.Format(time.RFC3339),
			Reviews:               make([]reviewView, 0, len(reviews)),
		}

		if request.Command.DisplayRedacted != "" {
			view.CommandRedacted = request.Command.DisplayRedacted
		}
		if request.ResolvedAt != nil {
			view.ResolvedAt = request.ResolvedAt.Format(time.RFC3339)
		}
		if request.ExpiresAt != nil {
			view.ExpiresAt = request.ExpiresAt.Format(time.RFC3339)
		}
		if request.ApprovalExpiresAt != nil {
			view.ApprovalExpiresAt = request.ApprovalExpiresAt.Format(time.RFC3339)
		}

		// Count approvals and rejections, build review list
		for _, r := range reviews {
			if r.Decision == db.DecisionApprove {
				view.ApprovalCount++
			} else if r.Decision == db.DecisionReject {
				view.RejectionCount++
			}

			rv := reviewView{
				ReviewID:  r.ID,
				Reviewer:  r.ReviewerAgent,
				Model:     r.ReviewerModel,
				Decision:  string(r.Decision),
				Comments:  r.Comments,
				CreatedAt: r.CreatedAt.Format(time.RFC3339),
			}
			view.Reviews = append(view.Reviews, rv)
		}

		out := output.New(output.Format(GetOutput()))
		return out.Write(view)
	},
}
