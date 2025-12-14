package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

var (
	flagReviewAll  bool
	flagReviewPool bool
)

func init() {
	reviewCmd.PersistentFlags().BoolVarP(&flagReviewAll, "all", "a", false, "show requests from all projects")
	reviewCmd.PersistentFlags().BoolVar(&flagReviewPool, "review-pool", false, "show requests from configured review pool (cross-project)")

	reviewCmd.AddCommand(reviewListCmd)
	reviewCmd.AddCommand(reviewShowCmd)

	rootCmd.AddCommand(reviewCmd)
}

var reviewCmd = &cobra.Command{
	Use:   "review [request-id]",
	Short: "View request details for review",
	Long: `View details of a request to help decide whether to approve or reject it.

If a request ID is provided, shows full details including command, justification,
risk tier, and any existing reviews.

Use 'slb review list' to see all pending requests.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			// No ID provided, show list of pending
			return reviewListCmd.RunE(cmd, args)
		}
		// ID provided, show details
		return showRequestDetails(args[0])
	},
}

var reviewListCmd = &cobra.Command{
	Use:   "list",
	Short: "List pending requests awaiting review",
	RunE: func(cmd *cobra.Command, args []string) error {
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

		dbConn, err := db.Open(GetDB())
		if err != nil {
			return fmt.Errorf("opening database: %w", err)
		}
		defer dbConn.Close()

		var requests []*db.Request
		if flagReviewAll {
			requests, err = dbConn.ListPendingRequestsAllProjects()
		} else {
			if flagReviewPool && cfg.General.CrossProjectReviews && len(cfg.General.ReviewPool) > 0 {
				paths := dedupeStrings(append([]string{project}, cfg.General.ReviewPool...))
				requests, err = dbConn.ListPendingRequestsByProjects(paths)
			} else {
				requests, err = dbConn.ListPendingRequests(project)
			}
		}
		if err != nil {
			return fmt.Errorf("listing requests: %w", err)
		}

		if len(requests) == 0 {
			out := output.New(output.Format(GetOutput()))
			if GetOutput() == "json" {
				return out.Write([]any{})
			}
			fmt.Println("No pending requests found.")
			return nil
		}

		// Build output
		type requestSummary struct {
			ID             string `json:"id"`
			Command        string `json:"command"`
			RiskTier       string `json:"risk_tier"`
			RequestorAgent string `json:"requestor_agent"`
			MinApprovals   int    `json:"min_approvals"`
			CreatedAt      string `json:"created_at"`
			ProjectPath    string `json:"project_path,omitempty"`
		}

		summaries := make([]requestSummary, 0, len(requests))
		for _, r := range requests {
			cmd := r.Command.Raw
			if r.Command.ContainsSensitive && r.Command.DisplayRedacted != "" {
				cmd = r.Command.DisplayRedacted
			}
			// Truncate long commands for display
			if len(cmd) > 60 {
				cmd = cmd[:57] + "..."
			}

			summary := requestSummary{
				ID:             r.ID,
				Command:        cmd,
				RiskTier:       string(r.RiskTier),
				RequestorAgent: r.RequestorAgent,
				MinApprovals:   r.MinApprovals,
				CreatedAt:      r.CreatedAt.Format(time.RFC3339),
			}
			if flagReviewAll {
				summary.ProjectPath = r.ProjectPath
			}
			summaries = append(summaries, summary)
		}

		out := output.New(output.Format(GetOutput()))
		return out.Write(summaries)
	},
}

var reviewShowCmd = &cobra.Command{
	Use:   "show <request-id>",
	Short: "Show full details of a request",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return showRequestDetails(args[0])
	},
}

func showRequestDetails(requestID string) error {
	dbConn, err := db.Open(GetDB())
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer dbConn.Close()

	request, reviews, err := dbConn.GetRequestWithReviews(requestID)
	if err != nil {
		return fmt.Errorf("getting request: %w", err)
	}

	// Count approvals and rejections
	var approvals, rejections int
	for _, rev := range reviews {
		switch rev.Decision {
		case db.DecisionApprove:
			approvals++
		case db.DecisionReject:
			rejections++
		}
	}

	// Build output structure
	type reviewView struct {
		ID            string `json:"id"`
		ReviewerAgent string `json:"reviewer_agent"`
		ReviewerModel string `json:"reviewer_model"`
		Decision      string `json:"decision"`
		Comments      string `json:"comments,omitempty"`
		CreatedAt     string `json:"created_at"`
	}

	type requestDetail struct {
		ID                    string       `json:"id"`
		Status                string       `json:"status"`
		RiskTier              string       `json:"risk_tier"`
		Command               string       `json:"command"`
		CommandHash           string       `json:"command_hash"`
		Cwd                   string       `json:"cwd"`
		ProjectPath           string       `json:"project_path"`
		RequestorAgent        string       `json:"requestor_agent"`
		RequestorModel        string       `json:"requestor_model"`
		JustificationReason   string       `json:"justification_reason"`
		JustificationEffect   string       `json:"justification_expected_effect,omitempty"`
		JustificationGoal     string       `json:"justification_goal,omitempty"`
		JustificationSafety   string       `json:"justification_safety_argument,omitempty"`
		MinApprovals          int          `json:"min_approvals"`
		CurrentApprovals      int          `json:"current_approvals"`
		CurrentRejections     int          `json:"current_rejections"`
		RequireDifferentModel bool         `json:"require_different_model"`
		Reviews               []reviewView `json:"reviews,omitempty"`
		DryRunCommand         string       `json:"dry_run_command,omitempty"`
		DryRunOutput          string       `json:"dry_run_output,omitempty"`
		CreatedAt             string       `json:"created_at"`
		ExpiresAt             string       `json:"expires_at,omitempty"`
	}

	// Build command display
	cmd := request.Command.Raw
	if request.Command.ContainsSensitive && request.Command.DisplayRedacted != "" {
		cmd = request.Command.DisplayRedacted
	}

	detail := requestDetail{
		ID:                    request.ID,
		Status:                string(request.Status),
		RiskTier:              string(request.RiskTier),
		Command:               cmd,
		CommandHash:           request.Command.Hash,
		Cwd:                   request.Command.Cwd,
		ProjectPath:           request.ProjectPath,
		RequestorAgent:        request.RequestorAgent,
		RequestorModel:        request.RequestorModel,
		JustificationReason:   request.Justification.Reason,
		JustificationEffect:   request.Justification.ExpectedEffect,
		JustificationGoal:     request.Justification.Goal,
		JustificationSafety:   request.Justification.SafetyArgument,
		MinApprovals:          request.MinApprovals,
		CurrentApprovals:      approvals,
		CurrentRejections:     rejections,
		RequireDifferentModel: request.RequireDifferentModel,
		CreatedAt:             request.CreatedAt.Format(time.RFC3339),
	}

	if request.ExpiresAt != nil {
		detail.ExpiresAt = request.ExpiresAt.Format(time.RFC3339)
	}

	if request.DryRun != nil {
		detail.DryRunCommand = request.DryRun.Command
		detail.DryRunOutput = request.DryRun.Output
	}

	// Add reviews
	for _, rev := range reviews {
		detail.Reviews = append(detail.Reviews, reviewView{
			ID:            rev.ID,
			ReviewerAgent: rev.ReviewerAgent,
			ReviewerModel: rev.ReviewerModel,
			Decision:      string(rev.Decision),
			Comments:      rev.Comments,
			CreatedAt:     rev.CreatedAt.Format(time.RFC3339),
		})
	}

	out := output.New(output.Format(GetOutput()))
	if GetOutput() == "json" {
		return out.Write(detail)
	}

	// Human-readable output
	fmt.Printf("Request: %s\n", detail.ID)
	fmt.Printf("Status:  %s\n", strings.ToUpper(detail.Status))
	fmt.Printf("Risk:    %s\n", strings.ToUpper(detail.RiskTier))
	fmt.Println()
	fmt.Printf("Command: %s\n", detail.Command)
	fmt.Printf("Hash:    %s\n", detail.CommandHash)
	fmt.Printf("CWD:     %s\n", detail.Cwd)
	fmt.Println()
	fmt.Printf("Requestor: %s (%s)\n", detail.RequestorAgent, detail.RequestorModel)
	fmt.Println()
	fmt.Println("Justification:")
	fmt.Printf("  Reason: %s\n", detail.JustificationReason)
	if detail.JustificationEffect != "" {
		fmt.Printf("  Expected Effect: %s\n", detail.JustificationEffect)
	}
	if detail.JustificationGoal != "" {
		fmt.Printf("  Goal: %s\n", detail.JustificationGoal)
	}
	if detail.JustificationSafety != "" {
		fmt.Printf("  Safety Argument: %s\n", detail.JustificationSafety)
	}
	fmt.Println()
	fmt.Printf("Approvals: %d/%d required\n", detail.CurrentApprovals, detail.MinApprovals)
	if detail.CurrentRejections > 0 {
		fmt.Printf("Rejections: %d\n", detail.CurrentRejections)
	}
	if detail.RequireDifferentModel {
		fmt.Println("Note: Requires approval from a different model")
	}

	if detail.DryRunCommand != "" {
		fmt.Println()
		fmt.Println("Dry Run:")
		fmt.Printf("  Command: %s\n", detail.DryRunCommand)
		if detail.DryRunOutput != "" {
			fmt.Println("  Output:")
			for _, line := range strings.Split(detail.DryRunOutput, "\n") {
				fmt.Printf("    %s\n", line)
			}
		}
	}

	if len(detail.Reviews) > 0 {
		fmt.Println()
		fmt.Println("Reviews:")
		for _, rev := range detail.Reviews {
			fmt.Printf("  - %s by %s (%s)\n", strings.ToUpper(rev.Decision), rev.ReviewerAgent, rev.ReviewerModel)
			if rev.Comments != "" {
				fmt.Printf("    Comment: %s\n", rev.Comments)
			}
		}
	}

	fmt.Println()
	fmt.Printf("Created: %s\n", detail.CreatedAt)
	if detail.ExpiresAt != "" {
		fmt.Printf("Expires: %s\n", detail.ExpiresAt)
	}

	return nil
}
