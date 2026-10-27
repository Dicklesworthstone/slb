// Package cli implements the cancel command.
package cli

import (
	"fmt"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(cancelCmd)
}

var cancelCmd = &cobra.Command{
	Use:   "cancel <request-id>",
	Short: "Cancel an unexecuted request",
	Long: `Cancel a pending, approved or escalated command request before execution.

You can only cancel requests that you created (matching session ID).
Use --session-id/-s to specify your session if not using environment.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		requestID := args[0]

		if flagSessionID == "" {
			return fmt.Errorf("--session-id is required to cancel a request")
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

		// Ownership and current state are checked under the same writer
		// reservation as cancellation; a concurrent execution claim wins safely.
		request, err := dbConn.CancelRequest(requestID, flagSessionID)
		if err != nil {
			return fmt.Errorf("cancelling request: %w", err)
		}

		out := output.New(output.Format(GetOutput()))
		response := map[string]any{
			"request_id":    requestID,
			"status":        "cancelled",
			"cancelled_at":  request.ResolvedAt.Format(time.RFC3339),
			"review_notice": request.ApprovalPolicy.Label(),
		}
		addApprovalPolicyMetadata(response, request.ApprovalPolicy)
		return out.Write(response)
	},
}
