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
	historyCmd.Flags().IntVar(&flagHistoryLimit, "limit", 50, "max results to return")

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
  slb history --since 2025-12-01       # Show requests since date`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dbConn, err := db.Open(GetDB())
		if err != nil {
			return fmt.Errorf("opening database: %w", err)
		}
		defer dbConn.Close()

		var requests []*db.Request

		// If query is provided, use FTS search
		if flagHistoryQuery != "" {
			requests, err = dbConn.SearchRequests(flagHistoryQuery)
			if err != nil {
				return fmt.Errorf("searching requests: %w", err)
			}
		} else {
			// Use filtered listing
			requests, err = listRequestsWithFilters(dbConn)
			if err != nil {
				return fmt.Errorf("listing requests: %w", err)
			}
		}

		// Apply additional filters
		requests = applyHistoryFilters(requests)

		// Limit results
		if len(requests) > flagHistoryLimit {
			requests = requests[:flagHistoryLimit]
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

// listRequestsWithFilters retrieves requests with basic filtering.
// For now this returns all requests - we could add more DB-level filtering.
func listRequestsWithFilters(dbConn *db.DB) ([]*db.Request, error) {
	project, _ := projectPath()

	// If status filter is set, use status-based listing
	if flagHistoryStatus != "" {
		status := db.RequestStatus(flagHistoryStatus)
		return dbConn.ListRequestsByStatus(status, project)
	}

	// Otherwise get all pending (and use in-memory filtering)
	// Note: This is a simplification - in production we'd want a more flexible DB query
	pending, err := dbConn.ListPendingRequests(project)
	if err != nil {
		return nil, err
	}

	// Also get from FTS with wildcard to get all
	// Note: This is a workaround - ideally we'd have a ListAllRequests function
	all, err := dbConn.SearchRequests("*")
	if err != nil {
		// FTS might not work with wildcard, fall back to pending only
		return pending, nil
	}

	return all, nil
}

// applyHistoryFilters applies in-memory filters to requests.
func applyHistoryFilters(requests []*db.Request) []*db.Request {
	result := make([]*db.Request, 0, len(requests))

	var sinceTime time.Time
	if flagHistorySince != "" {
		// Try parsing as RFC3339 first, then as date only
		var err error
		sinceTime, err = time.Parse(time.RFC3339, flagHistorySince)
		if err != nil {
			sinceTime, err = time.Parse("2006-01-02", flagHistorySince)
			if err != nil {
				// Invalid date, ignore filter
				sinceTime = time.Time{}
			}
		}
	}

	for _, r := range requests {
		// Filter by status
		if flagHistoryStatus != "" && string(r.Status) != flagHistoryStatus {
			continue
		}

		// Filter by agent
		if flagHistoryAgent != "" && r.RequestorAgent != flagHistoryAgent {
			continue
		}

		// Filter by tier
		if flagHistoryTier != "" && string(r.RiskTier) != flagHistoryTier {
			continue
		}

		// Filter by since
		if !sinceTime.IsZero() && r.CreatedAt.Before(sinceTime) {
			continue
		}

		result = append(result, r)
	}

	return result
}
