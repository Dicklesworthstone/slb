// Package cli implements the watch command for monitoring pending requests.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Dicklesworthstone/slb/internal/daemon"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/spf13/cobra"
)

var (
	flagWatchSessionID          string
	flagWatchAutoApproveCaution bool
	flagWatchPollInterval       time.Duration
)

func init() {
	watchCmd.Flags().StringVarP(&flagWatchSessionID, "session-id", "s", "", "session ID for auto-approve attribution")
	watchCmd.Flags().BoolVar(&flagWatchAutoApproveCaution, "auto-approve-caution", false, "automatically approve CAUTION tier requests")
	watchCmd.Flags().DurationVar(&flagWatchPollInterval, "poll-interval", 2*time.Second, "polling interval when daemon not available")

	rootCmd.AddCommand(watchCmd)
}

var watchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Watch for pending requests (for reviewing agents)",
	Long: `Stream pending request events in NDJSON format for programmatic consumption.

This command is designed for AI agents that review and approve requests.
Events are streamed as newline-delimited JSON objects.

If the daemon is running, events are received in real-time via IPC subscription.
If the daemon is not running, the command falls back to polling the database.

Event types:
  request_pending   - New request awaiting approval
  request_approved  - Request was approved
  request_rejected  - Request was rejected
  request_executed  - Approved request was executed
  request_timeout   - Request timed out
  request_cancelled - Request was cancelled

Use --auto-approve-caution to automatically approve CAUTION tier requests.`,
	RunE: runWatch,
}

func runWatch(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle SIGINT/SIGTERM for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	// Try daemon IPC first
	client := daemon.NewClient()
	if client.IsDaemonRunning() {
		return runWatchDaemon(ctx, client)
	}

	// Fall back to polling
	daemon.ShowDegradedWarningQuiet()
	return runWatchPolling(ctx)
}

// runWatchDaemon streams events via daemon IPC subscription.
func runWatchDaemon(ctx context.Context, client *daemon.Client) error {
	ipcClient := daemon.NewIPCClient(daemon.DefaultSocketPath())
	defer ipcClient.Close()

	events, err := ipcClient.Subscribe(ctx)
	if err != nil {
		return fmt.Errorf("subscribing to events: %w", err)
	}

	enc := json.NewEncoder(os.Stdout)

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-events:
			if !ok {
				return nil
			}

			watchEvent := daemon.ToRequestStreamEvent(event)
			if err := enc.Encode(watchEvent); err != nil {
				return fmt.Errorf("encoding event: %w", err)
			}

			// Auto-approve CAUTION tier if enabled
			if flagWatchAutoApproveCaution && watchEvent.Event == "request_pending" && watchEvent.RiskTier == "caution" {
				if err := autoApproveCaution(ctx, watchEvent.RequestID); err != nil {
					// Log error but continue watching
					errEvent := map[string]any{
						"event":      "auto_approve_error",
						"request_id": watchEvent.RequestID,
						"error":      err.Error(),
					}
					enc.Encode(errEvent)
				}
			}
		}
	}
}

// runWatchPolling polls the database for pending requests.
func runWatchPolling(ctx context.Context) error {
	dbConn, err := db.Open(GetDB())
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer dbConn.Close()

	enc := json.NewEncoder(os.Stdout)
	seen := make(map[string]db.RequestStatus)
	ticker := time.NewTicker(flagWatchPollInterval)
	defer ticker.Stop()

	// Initial poll
	if err := pollRequests(ctx, dbConn, enc, seen); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := pollRequests(ctx, dbConn, enc, seen); err != nil {
				return err
			}
		}
	}
}

// pollRequests checks for new or changed requests and emits events.
func pollRequests(ctx context.Context, dbConn *db.DB, enc *json.Encoder, seen map[string]db.RequestStatus) error {
	// Get all pending requests for all projects
	requests, err := dbConn.ListPendingRequestsAllProjects()
	if err != nil {
		return fmt.Errorf("listing requests: %w", err)
	}

	for _, req := range requests {
		prevStatus, exists := seen[req.ID]

		if !exists {
			// New request
			event := daemon.RequestStreamEvent{
				Event:     "request_pending",
				RequestID: req.ID,
				RiskTier:  string(req.RiskTier),
				Command:   req.Command.DisplayRedacted,
				Requestor: req.RequestorAgent,
				CreatedAt: req.CreatedAt.Format(time.RFC3339),
			}
			if req.Command.DisplayRedacted == "" {
				event.Command = req.Command.Raw
			}
			if err := enc.Encode(event); err != nil {
				return fmt.Errorf("encoding event: %w", err)
			}

			// Auto-approve CAUTION tier if enabled
			if flagWatchAutoApproveCaution && req.RiskTier == db.RiskTierCaution {
				if err := autoApproveCaution(ctx, req.ID); err != nil {
					errEvent := map[string]any{
						"event":      "auto_approve_error",
						"request_id": req.ID,
						"error":      err.Error(),
					}
					enc.Encode(errEvent)
				}
			}
		} else if prevStatus != req.Status {
			// Status changed
			var eventType string
			switch req.Status {
			case db.StatusApproved:
				eventType = "request_approved"
			case db.StatusRejected:
				eventType = "request_rejected"
			case db.StatusExecuted, db.StatusExecutionFailed:
				eventType = "request_executed"
			case db.StatusTimeout:
				eventType = "request_timeout"
			case db.StatusCancelled:
				eventType = "request_cancelled"
			default:
				continue
			}

			event := daemon.RequestStreamEvent{
				Event:     eventType,
				RequestID: req.ID,
			}
			if err := enc.Encode(event); err != nil {
				return fmt.Errorf("encoding event: %w", err)
			}
		}

		seen[req.ID] = req.Status
	}

	return nil
}

// autoApproveCaution automatically approves a CAUTION tier request.
func autoApproveCaution(ctx context.Context, requestID string) error {
	dbConn, err := db.Open(GetDB())
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer dbConn.Close()

	// Get request to verify it's still pending and CAUTION
	request, err := dbConn.GetRequest(requestID)
	if err != nil {
		return fmt.Errorf("getting request: %w", err)
	}

	if request.Status != db.StatusPending {
		return nil // Already resolved
	}

	if request.RiskTier != db.RiskTierCaution {
		return fmt.Errorf("request is not CAUTION tier")
	}

	// Determine reviewer identity
	agent := "auto-reviewer"
	model := "auto"
	session := flagWatchSessionID
	if session == "" {
		session = "auto-approve"
	}

	// Submit approval
	review := &db.Review{
		RequestID:         requestID,
		ReviewerSessionID: session,
		ReviewerAgent:     agent,
		ReviewerModel:     model,
		Decision:          db.DecisionApprove,
		Comments:          "Auto-approved CAUTION tier request",
		CreatedAt:         time.Now(),
	}

	if err := dbConn.CreateReview(review); err != nil {
		return fmt.Errorf("creating review: %w", err)
	}

	// Check if approval threshold met and update status
	reviews, err := dbConn.ListReviewsForRequest(requestID)
	if err != nil {
		return fmt.Errorf("getting reviews: %w", err)
	}

	approvals := 0
	for _, r := range reviews {
		if r.Decision == db.DecisionApprove {
			approvals++
		}
	}

	if approvals >= request.MinApprovals {
		if err := dbConn.UpdateRequestStatus(requestID, db.StatusApproved); err != nil {
			return fmt.Errorf("approving request: %w", err)
		}
	}

	return nil
}
