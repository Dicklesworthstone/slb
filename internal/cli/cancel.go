// Package cli implements the cancel command.
package cli

import (
	"fmt"
	"time"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(cancelCmd)
}

var cancelCmd = &cobra.Command{
	Use:   "cancel <request-id>",
	Short: "Cancel a pending request",
	Long: `Cancel a pending command approval request.

You can only cancel requests that you created (matching session ID).
Use --session-id/-s to specify your session if not using environment.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		requestID := args[0]

		if flagSessionID == "" {
			return fmt.Errorf("--session-id is required to cancel a request")
		}

		dbConn, err := db.Open(GetDB())
		if err != nil {
			return fmt.Errorf("opening database: %w", err)
		}
		defer dbConn.Close()

		// Get the request first to verify ownership
		request, err := dbConn.GetRequest(requestID)
		if err != nil {
			return fmt.Errorf("getting request: %w", err)
		}

		// Verify the requestor matches
		if request.RequestorSessionID != flagSessionID {
			return fmt.Errorf("cannot cancel request: you are not the requestor (session mismatch)")
		}

		// Verify the request can be cancelled (pending or approved, but not yet executing)
		if !core.CanCancel(request.Status) {
			return fmt.Errorf("cannot cancel request: status is %s (must be pending or approved)", request.Status)
		}

		// Cancel the request
		if err := dbConn.UpdateRequestStatus(requestID, db.StatusCancelled); err != nil {
			return fmt.Errorf("cancelling request: %w", err)
		}

		out := output.New(output.Format(GetOutput()))
		return out.Write(map[string]any{
			"request_id":   requestID,
			"status":       "cancelled",
			"cancelled_at": time.Now().UTC().Format(time.RFC3339),
		})
	},
}
