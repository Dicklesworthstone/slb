package cli

import (
	"errors"
	"fmt"
	"os"
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
	rollbackCmd.Flags().String("from", "", "recover a Git snapshot directory without the request database (requires --force)")
	rollbackCmd.Flags().Bool("list", false, "list Git recovery snapshots without opening the request database")

	rootCmd.AddCommand(rollbackCmd)
}

var rollbackCmd = &cobra.Command{
	Use:   "rollback [request-id]",
	Short: "Rollback an executed command",
	Long: `Rollback the effects of an executed command using captured state.

Request-based rollback requires a completed, failed, or timed-out execution
and state captured before execution (--capture-rollback flag).

Git recovery snapshots survive git clean -fdx inside the repository's Git
directory. Use --list to discover them even when .slb/state.db is missing.
Use --from <snapshot-directory> --force for explicit database-independent
recovery. This mode cannot check request execution status and does not update
the request database; it writes a recovery receipt beside the snapshot instead.

Recovery can overwrite current work. Stop other writers before capturing or
restoring state. Not all commands or Git repository states are recoverable.

Examples:
  slb rollback abc123
  slb rollback abc123 --force
  slb rollback --list --json
  slb rollback --from /repo/.git/slb-rollback/req-abc123-12345 --force --json`,
	Args: func(cmd *cobra.Command, args []string) error {
		from, _ := cmd.Flags().GetString("from")
		list, _ := cmd.Flags().GetBool("list")
		if list {
			if from != "" || len(args) != 0 {
				return fmt.Errorf("--list cannot be combined with --from or a request ID")
			}
			return nil
		}
		if from != "" {
			if len(args) != 0 {
				return fmt.Errorf("--from cannot be combined with a request ID")
			}
			if !flagRollbackForce {
				return fmt.Errorf("--from cannot verify execution status; --force is required")
			}
			return nil
		}
		return cobra.ExactArgs(1)(cmd, args)
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		// These recovery modes deliberately run before opening or migrating a
		// database. A missing/corrupt DB must not prevent disaster recovery.
		from, _ := cmd.Flags().GetString("from")
		list, _ := cmd.Flags().GetBool("list")
		if list || from != "" {
			return runSnapshotRollback(cmd, from, list)
		}
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

		// A timed-out command can have modified files before cancellation too.
		if request.Status != db.StatusExecuted && request.Status != db.StatusExecutionFailed && request.Status != db.StatusTimedOut {
			return fmt.Errorf("cannot rollback: request status is %s (must be executed, execution_failed, or timed_out)", request.Status)
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

		if err := core.RestoreRollbackState(cmd.Context(), rollbackData, core.RollbackRestoreOptions{Force: flagRollbackForce}); err != nil {
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

func runSnapshotRollback(cmd *cobra.Command, from string, list bool) error {
	format := GetOutput()
	switch format {
	case "text", "json", "yaml", "toon":
	default:
		return fmt.Errorf("unsupported output format: %s", format)
	}
	out := output.New(output.Format(format), output.WithStats(GetStats()))
	if list {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		snapshots, err := core.ListGitRollbackSnapshots(cmd.Context(), cwd)
		if err != nil {
			return err
		}
		if format != "text" {
			return out.Write(snapshots)
		}
		if len(snapshots) == 0 {
			cmd.Println("No Git recovery snapshots found.")
		}
		for _, snapshot := range snapshots {
			if snapshot.Error != "" {
				cmd.Printf("%s\n  Unavailable: %s\n", snapshot.Path, snapshot.Error)
			} else {
				cmd.Printf("%s\n  Request: %s  Captured: %s\n", snapshot.Path, snapshot.RequestID, snapshot.CapturedAt.Format(time.RFC3339))
			}
		}
		return nil
	}
	result, restoreErr := core.RestoreGitRollbackSnapshot(cmd.Context(), from, core.RollbackRestoreOptions{Force: flagRollbackForce})
	if result == nil {
		return restoreErr
	}
	// A receipt failure occurs after data restoration. Preserve that outcome
	// in structured output even while returning a nonzero error to the caller.
	if format != "text" {
		return errors.Join(restoreErr, out.Write(result))
	}
	cmd.Printf("Rollback for request %s completed from %s\n", result.RequestID, result.RollbackPath)
	cmd.Printf("Recovery receipt: %s\nRequest database was not updated.\n", result.ReceiptPath)
	return restoreErr
}
