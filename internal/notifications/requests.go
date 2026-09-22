package notifications

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

const requestDeliveryPageSize = 64
const maxRequestDeliveryState = 4 * 1024 * 1024

// RequestNotice is metadata observed at a committed journal event, not current
// state or execution permission. Deliberately no raw command, argv, free-form
// evidence, display text, credentials or execution receipt can enter a notice.
type RequestNotice struct {
	ID                    string    `json:"id"`
	Sequence              int64     `json:"sequence"`
	Event                 string    `json:"event"`
	Project               string    `json:"project"`
	RequestID             string    `json:"request_id"`
	OccurredAt            time.Time `json:"occurred_at"`
	Status                string    `json:"status"`
	Tier                  string    `json:"tier"`
	CommandHash           string    `json:"command_hash"`
	RequestorSessionID    string    `json:"requestor_session_id"`
	MinApprovals          int       `json:"min_approvals"`
	RequireDifferentModel bool      `json:"require_different_model"`
	Approvals             int       `json:"submitted_approvals"`
	Rejections            int       `json:"submitted_rejections"`
	ReviewDecision        string    `json:"review_decision,omitempty"`
	ExitCode              *int      `json:"exit_code,omitempty"`
	// Cursor is the source's opaque, validated resume position. It is never
	// sent to external destinations and is advanced only after handling.
	Cursor string `json:"-"`
}

// RequestNoticeID is stable across retries but changes for another journal or
// event. Hashing the opaque cursor avoids exposing its internal representation.
func RequestNoticeID(cursor string) string {
	sum := sha256.Sum256([]byte(cursor))
	return hex.EncodeToString(sum[:])
}

// RequestNoticePage must preserve journal order. The source must validate the
// supplied cursor even for empty pages; a restore must not silently skip work.
type RequestNoticePage struct {
	Notices []RequestNotice
	Cursor  string
	HasMore bool
}

type RequestNoticeSource interface {
	ReadNotices(context.Context, string, string, int) (RequestNoticePage, error)
}

type RequestRoute struct {
	// ID identifies the destination AND recipient set, not credentials. It
	// must be a hash or other non-secret identifier, never an endpoint URL.
	ID   string
	Send func(context.Context, RequestNotice) error
}

// RequestDeliveryPolicy sets a first-install lookback and a durable shared
// attempt budget. Lookback only applies when a destination is FIRST added:
// queued events never age out because a destination remained unavailable.
type RequestDeliveryPolicy struct {
	Lookback     time.Duration
	MaxPerMinute int
}

func (p RequestDeliveryPolicy) normalized() (RequestDeliveryPolicy, error) {
	if p.Lookback == 0 {
		p.Lookback = 24 * time.Hour
	}
	if p.MaxPerMinute == 0 {
		p.MaxPerMinute = 60
	}
	if p.Lookback < time.Second || p.Lookback > 30*24*time.Hour || p.MaxPerMinute < 1 || p.MaxPerMinute > 1000 {
		return p, errors.New("invalid request-notification delivery policy")
	}
	return p, nil
}

type requestDestination struct {
	Since    time.Time `json:"since"`
	Cursor   string    `json:"cursor"`
	Sequence int64     `json:"sequence"`
	RetryAt  time.Time `json:"retry_at"`
	Failures int       `json:"failures"`
}

type requestDeliveryState struct {
	Version      int                           `json:"version"`
	Project      string                        `json:"project"`
	Destinations map[string]requestDestination `json:"destinations"`
	Attempts     []time.Time                   `json:"attempts"`
	ObservedAt   time.Time                     `json:"observed_at"`
	NextRoute    int                           `json:"next_route"`
}

type RequestDeliveryReport struct {
	Sent, Failed, Skipped, Deferred int
}

// RequestDispatcher delivers the durable journal independently to each route.
// A failed route does not advance its cursor or block another destination.
// One instance/process owns a ledger. Concurrent calls to that instance are
// serialized; cross-process ownership is the daemon's responsibility. Delivery
// is at-least-once under recovery, not exactly-once across an external service.
type RequestDispatcher struct {
	StatePath string
	Project   string
	Policy    RequestDeliveryPolicy
	Source    RequestNoticeSource
	mu        sync.Mutex
}

func (d *RequestDispatcher) Dispatch(ctx context.Context, now time.Time, routes []RequestRoute) (RequestDeliveryReport, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var report RequestDeliveryReport
	policy, err := d.Policy.normalized()
	if err != nil {
		return report, err
	}
	if d.StatePath == "" || !filepath.IsAbs(d.Project) || d.Source == nil || now.IsZero() || len(routes) > 50 {
		return report, errors.New("invalid request notification dispatcher")
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	ids := make(map[string]bool, len(routes))
	for _, route := range routes {
		if route.ID == "" || len(route.ID) > 256 || route.Send == nil || ids[route.ID] {
			return report, errors.New("invalid or duplicate request notification route")
		}
		ids[route.ID] = true
	}
	if len(routes) == 0 {
		return report, nil // Disabled delivery has no disk or journal side effects.
	}
	state, err := readRequestDeliveryState(d.StatePath, d.Project)
	if err != nil {
		return report, err
	}
	if now.Before(state.ObservedAt) {
		return report, errors.New("notification clock moved backwards; preserving delivery budget")
	}
	state.ObservedAt = now.UTC()
	attempts := state.Attempts[:0]
	for _, at := range state.Attempts {
		if at.After(now.Add(-time.Minute)) {
			attempts = append(attempts, at)
		}
	}
	state.Attempts = attempts
	// Rotate the starting destination, including after timeouts, so a slow or
	// busy first route cannot permanently monopolize the shared pass/budget.
	start := state.NextRoute % len(routes)
	state.NextRoute = (start + 1) % len(routes)
	if err := writeRequestDeliveryState(d.StatePath, state); err != nil {
		return report, err
	}
	var failures []error
	for offset := range routes {
		if err := ctx.Err(); err != nil {
			return report, errors.Join(append(failures, err)...)
		}
		route := routes[(start+offset)%len(routes)]
		entry, found := state.Destinations[route.ID]
		if !found {
			if len(state.Destinations) >= 100 {
				return report, errors.New("request delivery destination capacity exceeded; preserve and inspect the ledger")
			}
			entry.Since = now.Add(-policy.Lookback).UTC()
			state.Destinations[route.ID] = entry
			if err := writeRequestDeliveryState(d.StatePath, state); err != nil {
				return report, err
			}
		}
		if now.Before(entry.RetryAt) {
			report.Deferred++
			continue
		}
		page, err := d.Source.ReadNotices(ctx, d.Project, entry.Cursor, requestDeliveryPageSize)
		if err != nil {
			// Never recover a bad cursor by resetting it or jumping to the head.
			failures = append(failures, fmt.Errorf("request notification journal: %w", err))
			continue
		}
		if err := validateNoticePage(page, d.Project, entry); err != nil {
			failures = append(failures, err)
			continue
		}
		if len(page.Notices) == 0 {
			entry.Cursor = page.Cursor // Also binds an initially empty journal.
			state.Destinations[route.ID] = entry
			if err := writeRequestDeliveryState(d.StatePath, state); err != nil {
				return report, err
			}
			continue
		}
		for _, notice := range page.Notices {
			if err := ctx.Err(); err != nil {
				return report, errors.Join(append(failures, err)...)
			}
			if notice.OccurredAt.Before(entry.Since) {
				report.Skipped++ // Only historical events before this route's cutoff.
			} else {
				if len(state.Attempts) >= policy.MaxPerMinute {
					report.Deferred++
					break
				}
				// Write the attempted-send reservation BEFORE contacting a peer.
				// A crash/ambiguous send is retried without erasing its budget.
				entry.Failures = min(5, entry.Failures+1)
				entry.RetryAt = now.Add(time.Duration(1<<uint(entry.Failures-1)) * 15 * time.Second)
				state.Destinations[route.ID] = entry
				state.Attempts = append(state.Attempts, now.UTC())
				if err := writeRequestDeliveryState(d.StatePath, state); err != nil {
					return report, err
				}
				sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				err := route.Send(sendCtx, notice)
				cancel()
				if err != nil {
					report.Failed++
					// Transport errors can contain credentials; expose only a fixed
					// diagnostic. The failing destination retains this exact event.
					failures = append(failures, errors.New("request notification delivery failed; retry scheduled"))
					break
				}
				report.Sent++
			}
			entry.Cursor, entry.Sequence = notice.Cursor, notice.Sequence
			entry.Failures, entry.RetryAt = 0, time.Time{}
			state.Destinations[route.ID] = entry
			if err := writeRequestDeliveryState(d.StatePath, state); err != nil {
				// The peer might already have received it; do not claim exactly-once.
				return report, err
			}
		}
	}
	return report, errors.Join(append(failures, ctx.Err())...)
}

func validateNoticePage(page RequestNoticePage, project string, entry requestDestination) error {
	if page.Cursor == "" || len(page.Cursor) > 16384 || len(page.Notices) > requestDeliveryPageSize || (page.HasMore && len(page.Notices) == 0) {
		return errors.New("invalid request notification journal page")
	}
	previous := entry.Sequence
	for _, notice := range page.Notices {
		if notice.Project != project || notice.RequestID == "" || notice.Event == "" || notice.OccurredAt.IsZero() ||
			notice.Sequence <= previous || notice.Cursor == "" || len(notice.Cursor) > 16384 || notice.ID != RequestNoticeID(notice.Cursor) {
			return errors.New("invalid or out-of-order request notification event")
		}
		previous = notice.Sequence
	}
	if len(page.Notices) > 0 && page.Cursor != page.Notices[len(page.Notices)-1].Cursor {
		return errors.New("request notification page cursor does not match last event")
	}
	if len(page.Notices) == 0 && entry.Cursor != "" && page.Cursor != entry.Cursor {
		return errors.New("empty request notification page advanced its cursor")
	}
	return nil
}

func readRequestDeliveryState(path, project string) (*requestDeliveryState, error) {
	state := &requestDeliveryState{Version: 1, Project: project, Destinations: make(map[string]requestDestination)}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxRequestDeliveryState || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return nil, errors.New("request delivery ledger must be a private regular file no larger than 4 MiB")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	limited := &io.LimitedReader{R: file, N: maxRequestDeliveryState + 1}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(state); err != nil {
		return nil, errors.New("invalid request delivery ledger JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) || limited.N <= 0 {
		return nil, errors.New("trailing or oversized request delivery ledger")
	}
	if state.Version != 1 || state.Project != project || state.Destinations == nil || len(state.Destinations) > 100 ||
		len(state.Attempts) > 1000 || state.NextRoute < 0 || state.NextRoute >= 50 {
		return nil, errors.New("invalid request delivery ledger scope/schema")
	}
	for id, entry := range state.Destinations {
		if id == "" || len(id) > 256 || entry.Since.IsZero() || entry.Sequence < 0 || len(entry.Cursor) > 16384 ||
			(entry.Sequence > 0 && entry.Cursor == "") || entry.Failures < 0 || entry.Failures > 5 ||
			(entry.Failures == 0) != entry.RetryAt.IsZero() {
			return nil, errors.New("invalid request delivery destination state")
		}
	}
	for _, at := range state.Attempts {
		if at.IsZero() || at.After(state.ObservedAt) {
			return nil, errors.New("invalid request delivery attempt budget")
		}
	}
	return state, nil
}

func writeRequestDeliveryState(path string, state *requestDeliveryState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(data) > maxRequestDeliveryState {
		return errors.New("request delivery ledger exceeds 4 MiB")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".request-delivery-*")
	if err != nil {
		return err
	}
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		dir, err := os.Open(directory)
		if err != nil {
			return err
		}
		defer dir.Close()
		return dir.Sync()
	}
	return nil
}
