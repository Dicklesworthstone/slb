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
	"path/filepath"
	"reflect"
	"strings"
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
Both transports begin with unresolved requests and completions from the last
15 minutes. If a daemon subscription is lost, a watch_degraded event precedes
the current database snapshot. Previously observed active requests remain
tracked through their final state, even after a longer disconnect.
Consumers should replace state by request ID; rapid transitions may coalesce.
Use 'slb events --follow' for durable, resumable lifecycle replay.

Request events retain status, command hash, submitted vote counts, original
timestamps, approval policy and execution assurance. Counts and notifications
are display information, not execution authority. Same-host peer review is
low assurance, not independently authenticated authorization.

Event types:
  request_pending   - New request awaiting approval
  request_approved  - Request was approved
  request_rejected  - Request was rejected
  request_executing - Command execution was claimed
  request_executed  - Command completed successfully
  request_execution_failed - Command execution failed
  request_timed_out - Command execution timed out
  request_timeout   - Request timed out
  request_escalated - Request was escalated
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
	state, err := resolveWatchState()
	if err != nil {
		return err
	}
	if state.remote {
		err := runWatchDaemon(ctx, cmd.OutOrStdout(), state)
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("remote watch ended; local database fallback is disabled: %w", err)
	}

	// Try daemon IPC first
	client := daemon.NewClient(daemon.WithSocketPath(state.socket),
		daemon.WithPIDFile(daemon.PIDFileForCWD(state.project)), daemon.WithLocalOnly())
	if state.useDaemon && client.IsDaemonRunning() {
		err := runWatchDaemon(ctx, cmd.OutOrStdout(), state)
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
	return runWatchPolling(ctx, cmd.OutOrStdout(), state)
}

// runWatchDaemon streams events via daemon IPC subscription.
func runWatchDaemon(ctx context.Context, out io.Writer, state *watchState) error {
	ipcClient := daemon.NewIPCClient(state.socket)
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
			if err := state.emit(enc, *watchEvent); err != nil {
				return err
			}
			state.retireOldCompletions(time.Now().Add(-watchRecentCompletionWindow))

			// Auto-approve CAUTION tier if enabled
			if flagWatchAutoApproveCaution && watchEvent.Event == "request_pending" && watchEvent.RiskTier == "caution" {
				if err := autoApproveCaution(ctx, state.database, watchEvent.RequestID); err != nil {
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

// runWatchPolling reads only the pinned local database. It must not initialize
// an empty database when the selected project or an explicit filename is absent.
func runWatchPolling(ctx context.Context, out io.Writer, state *watchState) error {
	if flagWatchPollInterval <= 0 {
		return fmt.Errorf("--poll-interval must be positive")
	}
	dbConn, err := db.OpenWithOptions(state.database, db.OpenOptions{InitSchema: true})
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer dbConn.Close()

	enc := json.NewEncoder(out)
	ticker := time.NewTicker(flagWatchPollInterval)
	defer ticker.Stop()

	// Initial poll
	if err := pollRequests(ctx, dbConn, enc, state); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := pollRequests(ctx, dbConn, enc, state); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
}

const watchRecentCompletionWindow = 15 * time.Minute

// watchState survives the daemon-to-polling handoff. Complete display states
// detect submitted votes even when status stays pending. Only active IDs extend
// the bounded completion snapshot; no stream event grants execution authority.
type watchState struct {
	project, database, socket string
	remote, useDaemon         bool
	requests                  map[string]daemon.RequestStreamEvent
}

func newWatchState(project string) *watchState {
	return &watchState{project: project, database: filepath.Join(project, ".slb", "state.db"),
		socket: daemon.SocketPathForCWD(project), useDaemon: true,
		requests: make(map[string]daemon.RequestStreamEvent)}
}

// Select one source before connecting. Default local watch uses the same
// physical project root/database as the daemon, including from descendants or
// aliases. An explicit database keeps the selected project spelling and cannot
// be substituted by a daemon. Remote streams never fall back to local state.
func resolveWatchState() (*watchState, error) {
	project, err := projectPath()
	if err != nil {
		return nil, err
	}
	project, err = filepath.Abs(project)
	if err != nil {
		return nil, fmt.Errorf("resolving watch project: %w", err)
	}
	if flagDB != "" {
		state := newWatchState(project)
		state.database, err = filepath.Abs(flagDB)
		if err != nil {
			return nil, fmt.Errorf("resolving watch database: %w", err)
		}
		state.useDaemon = false
		return state, nil
	}
	if strings.TrimSpace(os.Getenv("SLB_HOST")) != "" {
		if flagWatchAutoApproveCaution {
			return nil, errors.New("--auto-approve-caution requires a local database; remote watch cannot make local policy decisions")
		}
		state := newWatchState(project)
		state.remote = true
		return state, nil
	}
	project, err = daemon.PhysicalProjectRootForCWD(project)
	if err != nil {
		return nil, err
	}
	return newWatchState(project), nil
}

func (s *watchState) emit(enc *json.Encoder, event daemon.RequestStreamEvent) error {
	s.retireOldCompletions(time.Now().Add(-watchRecentCompletionWindow))
	observedAt := event.ObservedAt
	event.ObservedAt = "" // Observation time is not a change to request state.
	if previous, exists := s.requests[event.RequestID]; event.RequestID != "" && exists && reflect.DeepEqual(previous, event) {
		return nil
	}
	if event.RequestID != "" && len(s.requests) >= 10000 {
		if _, exists := s.requests[event.RequestID]; !exists {
			return errors.New("watch state exceeds 10000 requests; use slb events for lifecycle replay")
		}
	}
	view := event
	view.ObservedAt = observedAt
	if view.ObservedAt == "" {
		view.ObservedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if err := enc.Encode(view); err != nil {
		return fmt.Errorf("encoding event: %w", err)
	}
	if event.RequestID != "" {
		s.requests[event.RequestID] = event
	}
	return nil
}

func (s *watchState) retireOldCompletions(since time.Time) {
	for id, event := range s.requests {
		if !event.Status.IsTerminal() {
			continue
		}
		stamp := event.ResolvedAt
		if stamp == "" {
			stamp = event.CreatedAt
		}
		resolved, err := time.Parse(time.RFC3339, stamp)
		if err != nil || resolved.Before(since) {
			delete(s.requests, id)
		}
	}
}

// pollRequests reads one project snapshot, including old final states only for
// previously observed active IDs. Missing IDs are deleted or no longer in scope;
// completed IDs retire after the same 15-minute window as daemon snapshots.
func pollRequests(ctx context.Context, dbConn *db.DB, enc *json.Encoder, state *watchState) error {
	activeIDs := make([]string, 0, len(state.requests))
	for id, event := range state.requests {
		if !event.Status.IsTerminal() {
			activeIDs = append(activeIDs, id)
		}
	}
	since := time.Now().Add(-watchRecentCompletionWindow)
	snapshot, err := dbConn.ReadProjectWatchState(ctx, state.project, since, activeIDs...)
	if err != nil {
		return fmt.Errorf("reading watch state: %w", err)
	}
	found := make(map[string]bool, len(snapshot.Requests))
	for _, req := range snapshot.Requests {
		found[req.ID] = true
	}
	for id := range state.requests {
		if !found[id] {
			delete(state.requests, id)
		}
	}
	for _, req := range snapshot.Requests {
		if err := processPolledRequest(ctx, req, enc, state); err != nil {
			return err
		}
	}
	state.retireOldCompletions(since)
	return nil
}

func processPolledRequest(ctx context.Context, req db.WatchRequestState, enc *json.Encoder, state *watchState) error {
	if err := state.emit(enc, daemon.RequestStreamEventFromState(req)); err != nil {
		return err
	}
	// Revisit pending candidates on every poll, not only discovery: the
	// configured delay may not have elapsed when the request first appeared.
	if flagWatchAutoApproveCaution && shouldAutoApproveCaution(req.Status, req.RiskTier).ShouldApprove {
		if err := autoApproveCaution(ctx, state.database, req.ID); err != nil {
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
func autoApproveCaution(ctx context.Context, databasePath, requestID string) error {
	dbConn, err := db.OpenWithOptions(databasePath, db.OpenOptions{InitSchema: true})
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
