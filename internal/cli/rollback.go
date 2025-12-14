package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

var (
	flagRollbackForce bool
)

func init() {
	rollbackCmd.Flags().BoolVarP(&flagRollbackForce, "force", "f", false, "force rollback even if state may be stale")

	rootCmd.AddCommand(rollbackCmd)
}

var rollbackCmd = &cobra.Command{
	Use:   "rollback <request-id>",
	Short: "Rollback an executed command",
	Long: `Rollback the effects of an executed command using captured state.

Rollback requires that:
1. The request was executed (status: executed or execution_failed)
2. Rollback state was captured before execution (--capture-rollback flag)
3. The captured state is still valid

Note: Not all commands can be rolled back. Rollback is only available when
pre-execution state capture was enabled.

Examples:
  slb rollback abc123
  slb rollback abc123 --force`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		requestID := args[0]

		// Open database
		dbConn, err := db.OpenAndMigrate(GetDB())
		if err != nil {
			return fmt.Errorf("opening database: %w", err)
		}
		defer dbConn.Close()

		// Get the request
		request, err := dbConn.GetRequest(requestID)
		if err != nil {
			return fmt.Errorf("getting request: %w", err)
		}

		// Validate request state
		if request.Status != db.StatusExecuted && request.Status != db.StatusExecutionFailed {
			return fmt.Errorf("cannot rollback: request status is %s (must be executed or execution_failed)", request.Status)
		}

		// Check for rollback data
		if request.Rollback == nil || request.Rollback.Path == "" {
			return fmt.Errorf("no rollback data available for this request (was --capture-rollback used?)")
		}

		// Check if already rolled back
		if request.Rollback.RolledBackAt != nil {
			if !flagRollbackForce {
				return fmt.Errorf("request was already rolled back at %s (use --force to rollback again)",
					request.Rollback.RolledBackAt.Format(time.RFC3339))
			}
		}

		rollbackData, err := core.LoadRollbackData(request.Rollback.Path)
		if err != nil {
			return fmt.Errorf("loading rollback data: %w", err)
		}

		ctx := context.Background()
		if err := core.RestoreRollbackState(ctx, rollbackData, core.RollbackRestoreOptions{Force: flagRollbackForce}); err != nil {
			return fmt.Errorf("restoring rollback state: %w", err)
		}

		// Build output
		type rollbackResult struct {
			RequestID    string `json:"request_id"`
			RollbackPath string `json:"rollback_path"`
			RolledBackAt string `json:"rolled_back_at"`
			Status       string `json:"status"`
			Message      string `json:"message"`
		}

		now := time.Now().UTC()
		if err := dbConn.UpdateRequestRolledBackAt(requestID, now); err != nil {
			return fmt.Errorf("recording rolled_back_at: %w", err)
		}

		resp := rollbackResult{
			RequestID:    requestID,
			RollbackPath: request.Rollback.Path,
			RolledBackAt: now.Format(time.RFC3339),
			Status:       "rolled_back",
			Message:      "Rollback completed using captured state.",
		}

		out := output.New(output.Format(GetOutput()))
		if GetOutput() == "json" {
			return out.Write(resp)
		}

		// Human-readable output
		fmt.Printf("Rollback for request %s\n", requestID)
		fmt.Printf("Rollback data: %s\n", request.Rollback.Path)
		fmt.Println()
		fmt.Println("Rollback completed.")

		return nil
	},
}
