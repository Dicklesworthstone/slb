package daemon

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/charmbracelet/log"
)

const stateReconcileInterval = 2 * time.Second

// requestStateEvents creates small display-only notifications. Review counts
// describe submitted votes, NOT signature verification or execution permission.
func requestStateEvents(state *db.ProjectWatchState) ([]Event, int32) {
	events := make([]Event, 0, len(state.Requests))
	var pending int32
	for _, r := range state.Requests {
		if r.Status == db.StatusPending {
			pending++
		}
		command := core.ApplyRedaction(r.Command, nil)
		if len(command) > 2048 {
			command = command[:2048] + "..."
		}
		payload := map[string]any{
			"request_id": r.ID, "project_path": r.ProjectPath, "status": string(r.Status),
			"risk_tier": string(r.RiskTier), "command": command, "command_hash": r.CommandHash,
			"requestor": r.Requestor, "min_approvals": r.MinApprovals,
			"approvals": r.Approvals, "rejections": r.Rejections, "created_at": r.CreatedAt,
		}
		if r.ResolvedAt != "" {
			payload["resolved_at"] = r.ResolvedAt
		}
		if r.ExitCode != nil {
			payload["exit_code"] = *r.ExitCode
		}
		events = append(events, Event{Type: "request_" + string(r.Status), Payload: payload, Time: time.Now().Unix()})
	}
	return events, pending
}

// publishRequestState atomically replaces the bootstrap snapshot and queues
// changed states. Subscription registration takes this same lock: it receives
// either the old snapshot plus the change or the new snapshot, never a gap.
func (s *IPCServer) publishRequestState(state *db.ProjectWatchState) {
	events, pending := requestStateEvents(state)
	s.subscribersMu.Lock()
	defer s.subscribersMu.Unlock()
	next := make(map[string]Event, len(events))
	for _, event := range events {
		id := event.Payload.(map[string]any)["request_id"].(string)
		next[id] = event
		previous, exists := s.requestSnapshot[id]
		if !exists || event.Type != previous.Type || !reflect.DeepEqual(event.Payload, previous.Payload) {
			s.broadcastLocked(event)
		}
	}
	if s.stateReady && s.sessionCount != state.ActiveSessions {
		s.broadcastLocked(Event{Type: "sessions_changed", Payload: map[string]any{"active_sessions": state.ActiveSessions}, Time: time.Now().Unix()})
	}
	s.requestSnapshot = next
	s.pendingCount.Store(pending)
	s.sessionCount = state.ActiveSessions
	s.stateManaged, s.stateReady, s.stateError = true, true, ""
}

// Snapshot errors invalidate streaming rather than leaving a healthy-looking
// connection silently serving stale approvals. Existing clients fall back to DB
// polling; new subscribers receive an explicit error until reconciliation works.
func (s *IPCServer) invalidateRequestState(err error) {
	s.subscribersMu.Lock()
	defer s.subscribersMu.Unlock()
	s.stateManaged, s.stateReady, s.stateError = true, false, err.Error()
	for id, sub := range s.subscribers {
		sub.stop()
		delete(s.subscribers, id)
	}
}

func (s *IPCServer) snapshotEventsLocked() []Event {
	ids := make([]string, 0, len(s.requestSnapshot))
	for id := range s.requestSnapshot {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	events := make([]Event, 0, len(ids))
	for _, id := range ids {
		events = append(events, s.requestSnapshot[id])
	}
	return events
}

// reconcileRequestState runs at startup, on filesystem hints and periodically.
// Watch events are hints only: dropped/coalesced events never become lost state.
func reconcileRequestState(ctx context.Context, database *db.DB, project string, servers []*IPCServer) error {
	state, err := database.ReadProjectWatchState(ctx, project, time.Now().Add(-15*time.Minute))
	if err != nil {
		for _, server := range servers {
			server.invalidateRequestState(err)
		}
		return err
	}
	for _, server := range servers {
		server.publishRequestState(state)
	}
	return nil
}

func runRequestStateMonitor(ctx context.Context, database *db.DB, project string, servers []*IPCServer, watcher *Watcher, logger *log.Logger) {
	var events <-chan WatchEvent
	var watcherErrors <-chan error
	if watcher != nil {
		events, watcherErrors = watcher.Events(), watcher.Errors()
	}
	ticker := time.NewTicker(stateReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-events:
			if !ok {
				events = nil
				logger.Warn("state watcher closed; using periodic reconciliation")
			}
		case err, ok := <-watcherErrors:
			if !ok {
				watcherErrors = nil
			} else {
				logger.Warn("state watcher failed; reconciling from database", "error", err)
			}
		case <-ticker.C:
		}
		if ctx.Err() != nil {
			return
		}
		if err := reconcileRequestState(ctx, database, project, servers); err != nil {
			logger.Error("state reconciliation failed", "error", fmt.Sprint(err))
		}
	}
}
