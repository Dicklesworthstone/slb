// Package cli implements the pending command.
package cli

import (
	"fmt"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

var (
	flagPendingAllProjects bool
	flagPendingReviewPool  bool
)

func init() {
	pendingCmd.Flags().BoolVar(&flagPendingAllProjects, "all-projects", false, "list pending and escalated requests across all projects")
	pendingCmd.Flags().BoolVar(&flagPendingReviewPool, "review-pool", false, "only show requests you can review (not your own)")

	rootCmd.AddCommand(pendingCmd)
}

var pendingCmd = &cobra.Command{
	Use:   "pending",
	Short: "List pending and escalated requests awaiting review",
	Long: `List command requests that still need review, including escalated requests.

By default, shows pending and escalated requests for the current project.
Escalation preserves the original review requirements and does not permit execution.
The requester may cancel an escalated request that is no longer needed.
Use --all-projects to see reviewable requests across all projects.
Use --review-pool to filter to requests you can review (excludes your own).

When [general.cross_project_reviews] is true, review_pool is an allowlist of
reviewer agent names. --review-pool includes cross-project requests only when
the target project's policy explicitly contains your reviewer identity.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		project, err := projectPath()
		if err != nil {
			return err
		}

		dbPath, err := GetDB()
		if err != nil {
			return err
		}
		dbConn, err := db.Open(dbPath)
		if err != nil {
			return fmt.Errorf("opening database: %w", err)
		}
		defer dbConn.Close()

		var requests []*db.Request

		if flagPendingAllProjects || flagPendingReviewPool {
			requests, err = dbConn.ListReviewableRequests("")
		} else {
			requests, err = dbConn.ListReviewableRequests(project)
		}
		if err != nil {
			return fmt.Errorf("listing pending requests: %w", err)
		}
		if flagPendingReviewPool {
			requests, err = filterRequestsForReviewPool(dbConn, requests, project)
			if err != nil {
				return err
			}
		}

		// Build response
		type pendingView struct {
			approvalPolicyView
			RequestID       string `json:"request_id"`
			Status          string `json:"status"`
			Command         string `json:"command"`
			CommandRedacted string `json:"command_redacted,omitempty"`
			RiskTier        string `json:"risk_tier"`
			MinApprovals    int    `json:"min_approvals"`
			RequestorAgent  string `json:"requestor_agent"`
			RequestorModel  string `json:"requestor_model"`
			ProjectPath     string `json:"project_path"`
			Reason          string `json:"reason,omitempty"`
			CreatedAt       string `json:"created_at"`
			ExpiresAt       string `json:"expires_at,omitempty"`
		}

		resp := make([]pendingView, 0, len(requests))
		for _, r := range requests {
			view := pendingView{
				approvalPolicyView: policyView(r.ApprovalPolicy),
				RequestID:          r.ID,
				Status:             string(r.Status),
				Command:            r.Command.Raw,
				RiskTier:           string(r.RiskTier),
				MinApprovals:       r.MinApprovals,
				RequestorAgent:     r.RequestorAgent,
				RequestorModel:     r.RequestorModel,
				ProjectPath:        r.ProjectPath,
				Reason:             r.Justification.Reason,
				CreatedAt:          r.CreatedAt.Format(time.RFC3339),
			}
			if r.Command.DisplayRedacted != "" {
				view.CommandRedacted = r.Command.DisplayRedacted
			}
			if r.ExpiresAt != nil {
				view.ExpiresAt = r.ExpiresAt.Format(time.RFC3339)
			}
			resp = append(resp, view)
		}

		out := output.New(output.Format(GetOutput()))
		return out.Write(resp)
	},
}

// dedupeStrings returns a copy with duplicates removed, preserving order.
func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
