// Package cli implements the outcome command for recording execution feedback.
package cli

import (
	"fmt"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

var (
	outcomeProblems    bool
	outcomeDescription string
	outcomeRating      int
	outcomeNotes       string
	outcomeLimit       int
)

func init() {
	rootCmd.AddCommand(outcomeCmd)
	outcomeCmd.AddCommand(outcomeRecordCmd)
	outcomeCmd.AddCommand(outcomeListCmd)
	outcomeCmd.AddCommand(outcomeStatsCmd)

	// Flags for outcome record
	outcomeRecordCmd.Flags().BoolVar(&outcomeProblems, "problems", false, "Indicate that the execution caused problems")
	outcomeRecordCmd.Flags().StringVarP(&outcomeDescription, "description", "d", "", "Description of problems encountered")
	outcomeRecordCmd.Flags().IntVarP(&outcomeRating, "rating", "r", 0, "Human rating (1-5 scale, 0 = not rated)")
	outcomeRecordCmd.Flags().StringVarP(&outcomeNotes, "notes", "n", "", "Additional notes")

	// Flags for outcome list
	outcomeListCmd.Flags().IntVar(&outcomeLimit, "limit", 20, "Maximum number of outcomes to list")
	outcomeListCmd.Flags().BoolVar(&outcomeProblems, "problems-only", false, "Only show problematic outcomes")
}

var outcomeCmd = &cobra.Command{
	Use:   "outcome",
	Short: "Record and view execution outcomes",
	Long: `Manage execution outcome feedback for analytics and learning.

After a command is executed, you can record feedback about whether it caused
problems, provide ratings, and add notes. This data is used to improve
pattern classification and identify risky commands.

Examples:
  slb outcome record <request-id>                    # Record outcome interactively
  slb outcome record <request-id> --problems -d "..."# Record problematic outcome
  slb outcome list                                   # List recent outcomes
  slb outcome list --problems-only                   # List only problematic
  slb outcome stats                                  # Show outcome statistics`,
}

var outcomeRecordCmd = &cobra.Command{
	Use:   "record <request-id>",
	Short: "Record feedback for an executed request",
	Long: `Record execution outcome feedback for analytics.

After a command is executed, use this to provide feedback:
- Whether it caused any problems
- Description of issues encountered
- Human rating (1-5 scale)
- Additional notes

This data helps improve pattern classification and identify risky commands.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		requestID := args[0]

		dbConn, err := db.Open(GetDB())
		if err != nil {
			return fmt.Errorf("opening database: %w", err)
		}
		defer dbConn.Close()

		// Verify request exists and is executed
		request, err := dbConn.GetRequest(requestID)
		if err != nil {
			return fmt.Errorf("getting request: %w", err)
		}

		if request.Status != db.StatusExecuted && request.Status != db.StatusExecutionFailed {
			return fmt.Errorf("request has not been executed yet (status: %s)", request.Status)
		}

		// Validate rating if provided
		var ratingPtr *int
		if outcomeRating != 0 {
			if outcomeRating < 1 || outcomeRating > 5 {
				return fmt.Errorf("rating must be between 1 and 5")
			}
			ratingPtr = &outcomeRating
		}

		// Record the outcome
		outcome, err := dbConn.RecordOutcome(
			requestID,
			outcomeProblems,
			outcomeDescription,
			ratingPtr,
			outcomeNotes,
		)
		if err != nil {
			return fmt.Errorf("recording outcome: %w", err)
		}

		out := output.New(output.Format(GetOutput()))
		response := map[string]any{
			"id":                  outcome.ID,
			"request_id":          outcome.RequestID,
			"caused_problems":     outcome.CausedProblems,
			"problem_description": outcome.ProblemDescription,
			"human_rating":        outcome.HumanRating,
			"human_notes":         outcome.HumanNotes,
			"recorded_at":         outcome.CreatedAt.Format(time.RFC3339),
		}
		policy := request.ApprovalPolicy
		if request.Execution != nil {
			policy = request.Execution.ApprovalPolicy
			response["assurance"] = request.Execution.Assurance
		}
		addApprovalPolicyMetadata(response, policy)
		return out.Write(response)
	},
}

var outcomeListCmd = &cobra.Command{
	Use:   "list",
	Short: "List recent execution outcomes",
	Long:  `List execution outcomes, optionally filtering by problems only.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dbConn, err := db.Open(GetDB())
		if err != nil {
			return fmt.Errorf("opening database: %w", err)
		}
		defer dbConn.Close()

		var outcomes []*db.ExecutionOutcome
		if outcomeProblems {
			outcomes, err = dbConn.ListProblematicOutcomes(outcomeLimit)
		} else {
			outcomes, err = dbConn.ListOutcomes(outcomeLimit)
		}
		if err != nil {
			return fmt.Errorf("listing outcomes: %w", err)
		}

		// Convert to output format
		result := make([]map[string]any, len(outcomes))
		for i, o := range outcomes {
			request, err := dbConn.GetRequest(o.RequestID)
			if err != nil {
				return fmt.Errorf("getting outcome request %s: %w", o.RequestID, err)
			}
			item := map[string]any{
				"id":              o.ID,
				"request_id":      o.RequestID,
				"caused_problems": o.CausedProblems,
				"created_at":      o.CreatedAt.Format(time.RFC3339),
			}
			policy := request.ApprovalPolicy
			if request.Execution != nil {
				policy = request.Execution.ApprovalPolicy
				item["assurance"] = request.Execution.Assurance
			}
			addApprovalPolicyMetadata(item, policy)
			if o.ProblemDescription != "" {
				item["problem_description"] = o.ProblemDescription
			}
			if o.HumanRating != nil {
				item["human_rating"] = *o.HumanRating
			}
			if o.HumanNotes != "" {
				item["human_notes"] = o.HumanNotes
			}
			result[i] = item
		}

		out := output.New(output.Format(GetOutput()))
		return out.Write(map[string]any{
			"outcomes": result,
			"count":    len(result),
		})
	},
}

var outcomeStatsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Show outcome statistics",
	Long: `Display aggregate statistics about execution outcomes.

Shows:
- Total outcome count
- Problematic percentage
- Average human rating
- Time-to-approval statistics`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dbConn, err := db.Open(GetDB())
		if err != nil {
			return fmt.Errorf("opening database: %w", err)
		}
		defer dbConn.Close()

		// Get outcome stats
		outcomeStats, err := dbConn.GetOutcomeStats()
		if err != nil {
			return fmt.Errorf("getting outcome stats: %w", err)
		}

		// Get approval time stats
		approvalStats, err := dbConn.GetTimeToApprovalStats()
		if err != nil {
			return fmt.Errorf("getting approval stats: %w", err)
		}

		out := output.New(output.Format(GetOutput()))
		return out.Write(map[string]any{
			"outcomes": map[string]any{
				"total":               outcomeStats.TotalOutcomes,
				"problematic_count":   outcomeStats.ProblematicCount,
				"problematic_percent": outcomeStats.ProblematicPercent,
				"rated_count":         outcomeStats.RatedCount,
				"avg_rating":          outcomeStats.AvgHumanRating,
			},
			"approval_times": map[string]any{
				"sample_size":    approvalStats.SampleSize,
				"avg_minutes":    approvalStats.AvgMinutes,
				"median_minutes": approvalStats.MedianMinutes,
				"min_minutes":    approvalStats.MinMinutes,
				"max_minutes":    approvalStats.MaxMinutes,
			},
		})
	},
}

var outcomeAgentStatsCmd = &cobra.Command{
	Use:   "agent-stats <agent-name>",
	Short: "Show statistics for a specific agent",
	Long:  `Display request and outcome statistics for a specific agent.`,
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		agentName := args[0]

		dbConn, err := db.Open(GetDB())
		if err != nil {
			return fmt.Errorf("opening database: %w", err)
		}
		defer dbConn.Close()

		stats, err := dbConn.GetRequestStatsByAgent(agentName)
		if err != nil {
			return fmt.Errorf("getting agent stats: %w", err)
		}

		out := output.New(output.Format(GetOutput()))
		return out.Write(map[string]any{
			"agent_name":      agentName,
			"total_requests":  stats.TotalRequests,
			"approved_count":  stats.ApprovedCount,
			"rejected_count":  stats.RejectedCount,
			"executed_count":  stats.ExecutedCount,
			"problematic_pct": stats.ProblematicPct,
		})
	},
}

func init() {
	outcomeCmd.AddCommand(outcomeAgentStatsCmd)
}
