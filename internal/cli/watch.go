// Package cli implements the watch command for monitoring pending requests.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/daemon"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/spf13/cobra"
)

var (
	flagWatchSessionID          string
	flagWatchAutoApproveCaution bool
	flagWatchPollInterval       time.Duration
	errWatchDaemonUnavailable   = errors.New("watch daemon subscription unavailable")
)

func init() {
	// -s is owned by the root persistent --session-id; don't reclaim the
	// shorthand here (it collides/shadows the persistent flag). Pass the
	// session via the long --session-id flag.
	watchCmd.Flags().StringVar(&flagWatchSessionID, "session-id", "", "reviewer session ID (automatic decisions do not create reviews)")
	watchCmd.Flags().BoolVar(&flagWatchAutoApproveCaution, "auto-approve-caution", false, "process due zero-review CAUTION approvals under current policy")
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
If a daemon subscription is lost, a watch_degraded event precedes a fresh
pending snapshot from polling. Consumers should deduplicate by request ID.

Event types:
  request_pending   - New request awaiting approval
  request_approved  - Request was approved
  request_rejected  - Request was rejected
  request_executed  - Approved request was executed
  request_timeout   - Request timed out
  request_cancelled - Request was cancelled

Use --auto-approve-caution to process due CAUTION approvals. The configured
delay, quorum, model constraints and current project policy still apply.`,
	RunE: runWatch,
}

func runWatch(cmd *cobra.Command, args []string) error {
	if flagWatchPollInterval <= 0 {
		return fmt.Errorf("--poll-interval must be positive")
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if ctx.Err() != nil {
		return nil
	}

	// Try daemon IPC first
	client := daemon.NewClient()
	if client.IsDaemonRunning() {
		err := runWatchDaemon(ctx, client, cmd.OutOrStdout())
		if ctx.Err() != nil {
			return nil
		}
		if !errors.Is(err, errWatchDaemonUnavailable) {
			return err // Output failures must not trigger a second writer.
		}
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
			"event": "watch_degraded", "mode": "polling", "error": err.Error(),
		}); err != nil {
			return fmt.Errorf("encoding fallback event: %w", err)
		}
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
		return fmt.Errorf("%w: %v", errWatchDaemonUnavailable, err)
	}

	enc := json.NewEncoder(out)

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-events:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				return errWatchDaemonUnavailable
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
					if err := enc.Encode(errEvent); err != nil {
						return fmt.Errorf("encoding auto-approval error: %w", err)
					}
				}
			}
		}
	}
}

// runWatchPolling polls the database for pending requests.
func runWatchPolling(ctx context.Context, out io.Writer) error {
	if flagWatchPollInterval <= 0 {
		return fmt.Errorf("--poll-interval must be positive")
	}
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
	case db.StatusExecuted, db.StatusExecutionFailed, db.StatusTimedOut:
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
	// Revisit pending candidates on every poll, not only discovery: the
	// configured delay may not have elapsed when the request first appeared.
	if flagWatchAutoApproveCaution && shouldAutoApproveCaution(req.Status, req.RiskTier).ShouldApprove {
		if err := autoApproveCaution(ctx, req.ID); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err := enc.Encode(map[string]any{
				"event": "auto_approve_error", "request_id": req.ID, "error": err.Error(),
			}); err != nil {
				return fmt.Errorf("encoding auto-approval error: %w", err)
			}
		}
	}
	return nil
}

// AutoApproveDecision encapsulates the result of the auto-approve decision.
// This is returned by the pure decision function for testability.
type AutoApproveDecision struct {
	ShouldApprove bool
	Reason        string
}

// shouldAutoApproveCaution is only a cheap candidate filter. It does not grant
// approval: the shared transactional resolver rechecks current policy, delay,
// request binding, requester identity and review evidence before any change.
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

// autoApproveCaution advances only eligible, due zero-review CAUTION decisions.
// It never fabricates a reviewer identity, unsigned vote or independent status
// write, and it cannot change the project's unrelated timeout policy.
func autoApproveCaution(ctx context.Context, requestID string) error {
	dbConn, err := db.Open(GetDB())
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer dbConn.Close()

	_, err = core.AdvancePendingRequest(ctx, dbConn, requestID, core.PendingOptions{
		ConfigPath: flagConfig, OnlyAutoApprove: true,
	})
	if err != nil {
		return fmt.Errorf("advancing CAUTION policy: %w", err)
	}

	return nil
}
