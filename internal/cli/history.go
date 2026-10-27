// Package cli implements the history command.
package cli

import (
	"fmt"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

var (
	flagHistoryQuery  string
	flagHistoryStatus string
	flagHistoryAgent  string
	flagHistoryTier   string
	flagHistorySince  string
	flagHistoryLimit  int
)

func init() {
	historyCmd.Flags().StringVarP(&flagHistoryQuery, "query", "q", "", "full-text search query")
	historyCmd.Flags().StringVar(&flagHistoryStatus, "status", "", "filter by status (pending, approved, rejected, executed, etc.)")
	historyCmd.Flags().StringVar(&flagHistoryAgent, "agent", "", "filter by requestor agent name")
	historyCmd.Flags().StringVar(&flagHistoryTier, "tier", "", "filter by risk tier (safe, caution, dangerous, critical)")
	historyCmd.Flags().StringVar(&flagHistorySince, "since", "", "only show requests after this date (RFC3339 or YYYY-MM-DD)")
	historyCmd.Flags().IntVar(&flagHistoryLimit, "limit", 50, "max results to return (1-10000)")

	rootCmd.AddCommand(historyCmd)
}

var historyCmd = &cobra.Command{
	Use:   "history",
	Short: "Browse and search request history",
	Long: `Browse and search command approval request history.

Examples:
  slb history                          # Show recent requests
  slb history -q "rm -rf"              # Search for commands containing "rm -rf"
  slb history --status executed        # Show only executed requests
  slb history --tier critical          # Show only critical tier requests
  slb history --agent "BrownStone"     # Show requests from specific agent
  slb history --since 2025-12-01       # Show requests since date

History includes completed requests in the selected project. Search and all
filters are applied before the result limit. Stored policy and execution
assurance describe the original workflow; history is not an execution permit.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if flagHistoryLimit < 1 || flagHistoryLimit > 10000 {
			return fmt.Errorf("--limit must be between 1 and 10000")
		}
		since, err := parseAuditTime(flagHistorySince)
		if err != nil {
			return fmt.Errorf("invalid --since: %w", err)
		}
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

		requests, err := dbConn.ListRequestHistory(db.RequestHistoryFilter{
			ProjectPath: project, Query: flagHistoryQuery, Status: db.RequestStatus(flagHistoryStatus),
			Agent: flagHistoryAgent, RiskTier: db.RiskTier(flagHistoryTier), Since: since, Limit: flagHistoryLimit,
		})
		if err != nil {
			return err
		}

		// Build response
		type historyView struct {
			approvalPolicyView
			ExecutionAssurance db.ReviewAssurance `json:"execution_assurance,omitempty"`
			RequestID          string             `json:"request_id"`
			Command            string             `json:"command"`
			RiskTier           string             `json:"risk_tier"`
			Status             string             `json:"status"`
			RequestorAgent     string             `json:"requestor_agent"`
			ProjectPath        string             `json:"project_path"`
			CreatedAt          string             `json:"created_at"`
			ResolvedAt         string             `json:"resolved_at,omitempty"`
		}

		resp := make([]historyView, 0, len(requests))
		for _, r := range requests {
			view := historyView{
				approvalPolicyView: policyView(r.ApprovalPolicy),
				RequestID:          r.ID,
				Command:            r.Command.Raw,
				RiskTier:           string(r.RiskTier),
				Status:             string(r.Status),
				RequestorAgent:     r.RequestorAgent,
				ProjectPath:        r.ProjectPath,
				CreatedAt:          r.CreatedAt.Format(time.RFC3339),
			}
			// Use redacted version for display if available
			if r.Command.DisplayRedacted != "" {
				view.Command = r.Command.DisplayRedacted
			} else if r.Command.ContainsSensitive {
				view.Command = "[REDACTED]"
			}
			if r.ResolvedAt != nil {
				view.ResolvedAt = r.ResolvedAt.Format(time.RFC3339)
			}
			if r.Execution != nil {
				view.ExecutionAssurance = r.Execution.Assurance
			}
			resp = append(resp, view)
		}

		out := output.New(output.Format(GetOutput()))
		return out.Write(resp)
	},
}
