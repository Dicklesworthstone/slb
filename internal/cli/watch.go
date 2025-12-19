// Package cli implements the watch command for monitoring pending requests.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	ctx, cancel := context.WithCancel(cmd.Context())
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
		return runWatchDaemon(ctx, client, cmd.OutOrStdout())
	}

	// Fall back to polling
	daemon.ShowDegradedWarningQuiet()
	return runWatchPolling(ctx, cmd.OutOrStdout())
}

// runWatchDaemon streams events via daemon IPC subscription.
func runWatchDaemon(ctx context.Context, client *daemon.Client, out io.Writer) error {
	ipcClient := daemon.NewIPCClient(daemon.DefaultSocketPath())
	defer ipcClient.Close()

	events, err := ipcClient.Subscribe(ctx)
	if err != nil {
		return fmt.Errorf("subscribing to events: %w", err)
	}

	enc := json.NewEncoder(out)

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
func runWatchPolling(ctx context.Context, out io.Writer) error {
	dbConn, err := db.Open(GetDB())
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer dbConn.Close()

	enc := json.NewEncoder(out)
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

// PollAction represents the action to take for a polled request.
type PollAction string

const (
	// PollActionEmitNew indicates a new pending request should be emitted.
	PollActionEmitNew PollAction = "emit_new"
	// PollActionEmitStatusChange indicates a status change event should be emitted.
	PollActionEmitStatusChange PollAction = "emit_status_change"
	// PollActionSkip indicates no event should be emitted.
	PollActionSkip PollAction = "skip"
)

// RequestPollResult encapsulates the decision about what to do with a polled request.
// This is returned by the pure evaluation function for testability.
type RequestPollResult struct {
	Action    PollAction
	EventType string // Only set when Action is EmitStatusChange
	Reason    string
}

// evaluateRequestForPolling is a pure function that determines what action to take
// when a request is polled. This function should maintain 100% test coverage as it
// contains the core polling business logic.
//
// Decision rules:
//   - New request (not in seen map): emit "request_pending" event
//   - Status changed: emit appropriate status change event
//   - Status unchanged: skip (no event)
func evaluateRequestForPolling(
	requestID string,
	currentStatus db.RequestStatus,
	seen map[string]db.RequestStatus,
) RequestPollResult {
	prevStatus, exists := seen[requestID]

	if !exists {
		// New request - emit pending event
		return RequestPollResult{
			Action:    PollActionEmitNew,
			EventType: "request_pending",
			Reason:    "new request discovered",
		}
	}

	if prevStatus == currentStatus {
		// No change - skip
		return RequestPollResult{
			Action: PollActionSkip,
			Reason: "status unchanged",
		}
	}

	// Status changed - determine event type
	eventType := statusToEventType(currentStatus)
	if eventType == "" {
		return RequestPollResult{
			Action: PollActionSkip,
			Reason: "unknown status transition: " + string(currentStatus),
		}
	}

	return RequestPollResult{
		Action:    PollActionEmitStatusChange,
		EventType: eventType,
		Reason:    "status changed from " + string(prevStatus) + " to " + string(currentStatus),
	}
}

// statusToEventType maps a request status to its corresponding event type string.
// Returns empty string for unknown/unhandled statuses.
func statusToEventType(status db.RequestStatus) string {
	switch status {
	case db.StatusApproved:
		return "request_approved"
	case db.StatusRejected:
		return "request_rejected"
	case db.StatusExecuted, db.StatusExecutionFailed:
		return "request_executed"
	case db.StatusTimeout:
		return "request_timeout"
	case db.StatusCancelled:
		return "request_cancelled"
	default:
		return ""
	}
}

// pollRequests checks for new or changed requests and emits events.
// It handles requests that move out of pending status by checking tracked IDs.
func pollRequests(ctx context.Context, dbConn *db.DB, enc *json.Encoder, seen map[string]db.RequestStatus) error {
	// Get all pending requests for all projects
	requests, err := dbConn.ListPendingRequestsAllProjects()
	if err != nil {
		return fmt.Errorf("listing requests: %w", err)
	}

	// Track which IDs were found in the pending list
	foundPending := make(map[string]bool)

	// Process current pending requests
	for _, req := range requests {
		foundPending[req.ID] = true
		if err := processPolledRequest(ctx, req, enc, seen); err != nil {
			return err
		}
	}

	// Check requests we were tracking that are no longer pending
	// (e.g., they became approved, rejected, executed)
	for id := range seen {
		if foundPending[id] {
			continue
		}

		// Fetch the latest state of the missing request
		req, err := dbConn.GetRequest(id)
		if err != nil {
			// If error (e.g. deleted), we stop tracking it implicit via not processing
			// But 'seen' still has it. Ideally we should remove it?
			// For simplicity, we just skip.
			continue
		}

		if err := processPolledRequest(ctx, req, enc, seen); err != nil {
			return err
		}
	}

	return nil
}

func processPolledRequest(ctx context.Context, req *db.Request, enc *json.Encoder, seen map[string]db.RequestStatus) error {
	// Use pure function for decision logic
	result := evaluateRequestForPolling(req.ID, req.Status, seen)

	switch result.Action {
	case PollActionEmitNew:
		// New request - build and emit pending event
		event := daemon.RequestStreamEvent{
			Event:     result.EventType,
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

	case PollActionEmitStatusChange:
		// Status changed - emit status change event
		event := daemon.RequestStreamEvent{
			Event:     result.EventType,
			RequestID: req.ID,
		}
		if err := enc.Encode(event); err != nil {
			return fmt.Errorf("encoding event: %w", err)
		}

	case PollActionSkip:
		// No action needed
	}

	seen[req.ID] = req.Status
	return nil
}

// AutoApproveDecision encapsulates the result of the auto-approve decision.
// This is returned by the pure decision function for testability.
type AutoApproveDecision struct {
	ShouldApprove bool
	Reason        string
}

// shouldAutoApproveCaution is a SAFETY-CRITICAL pure function that determines
// whether a request should be auto-approved. This function MUST maintain 100%
// test coverage as it guards against unauthorized command execution.
//
// Decision rules:
//   - Auto-approve must be enabled (checked at call site)
//   - Request must still be in pending status
//   - Request must be CAUTION tier (not DANGEROUS or CRITICAL)
//
// This function is intentionally side-effect free for reliable testing.
func shouldAutoApproveCaution(
	requestStatus db.RequestStatus,
	requestRiskTier db.RiskTier,
) AutoApproveDecision {
	// Guard 1: Request must still be pending
	if requestStatus != db.StatusPending {
		return AutoApproveDecision{
			ShouldApprove: false,
			Reason:        "request not pending (status: " + string(requestStatus) + ")",
		}
	}

	// Guard 2: Only CAUTION tier can be auto-approved
	// CRITICAL and DANGEROUS tiers MUST require explicit human approval
	if requestRiskTier != db.RiskTierCaution {
		return AutoApproveDecision{
			ShouldApprove: false,
			Reason:        "not caution tier (tier: " + string(requestRiskTier) + ")",
		}
	}

	return AutoApproveDecision{
		ShouldApprove: true,
		Reason:        "caution tier request eligible for auto-approval",
	}
}

// autoApproveCaution automatically approves a CAUTION tier request.
// This is the side-effectful wrapper that calls the pure decision function.
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

	// Use pure decision function for safety-critical logic
	decision := shouldAutoApproveCaution(request.Status, request.RiskTier)
	if !decision.ShouldApprove {
		if request.Status != db.StatusPending {
			return nil // Already resolved - not an error
		}
		return fmt.Errorf("auto-approve denied: %s", decision.Reason)
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
