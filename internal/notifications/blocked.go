// Package notifications derives rate-limited alerts from immutable hook audit
// records. Alerts are advisory and never grant command execution permission.
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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Dicklesworthstone/slb/internal/audit"
)

const maxEvents = 10000
const maxStateBytes = 4 * 1024 * 1024

// Policy bounds repeat detection, reminders and ALL transport attempts,
// including failed sends. Zero values select defaults; invalid values fail.
type Policy struct {
	Window          time.Duration
	RepeatThreshold int
	Cooldown        time.Duration
	MaxPerMinute    int
}

func (p Policy) normalized() (Policy, error) {
	if p.Window == 0 {
		p.Window = 10 * time.Minute
	}
	if p.RepeatThreshold == 0 {
		p.RepeatThreshold = 3
	}
	if p.Cooldown == 0 {
		p.Cooldown = time.Minute
	}
	if p.MaxPerMinute == 0 {
		p.MaxPerMinute = 10
	}
	if p.Window < time.Second || p.Window > 24*time.Hour || p.RepeatThreshold < 2 || p.RepeatThreshold > maxEvents ||
		p.Cooldown < time.Second || p.Cooldown > 24*time.Hour || p.MaxPerMinute < 1 || p.MaxPerMinute > 1000 {
		return p, errors.New("invalid blocked-notification policy")
	}
	return p, nil
}

// Alert contains only stored redacted command text, never raw command argv or
// session keys. SessionID is an audit label, not a verified agent identity.
type Alert struct {
	Key             string    `json:"key"`
	EventID         string    `json:"event_id"`
	Project         string    `json:"project"`
	CWD             string    `json:"cwd"`
	SessionID       string    `json:"session_id,omitempty"`
	CommandRedacted string    `json:"command_redacted"`
	CommandHash     string    `json:"command_hash"`
	Tier            string    `json:"tier"`
	Attempts        int       `json:"attempts"`
	FirstAt         time.Time `json:"first_at"`
	LastAt          time.Time `json:"last_at"`
	WindowSeconds   int64     `json:"window_seconds"`
	Repeated        bool      `json:"repeated"`
}

func (a Alert) Importance() string {
	if a.Repeated || a.Tier == "critical" {
		return "urgent"
	}
	return "normal"
}

// ReadBlocked scopes BEFORE aggregation and before applying capacity bounds.
// Offline Python and native/daemon audit records use the same Event schema.
// Missing logs are empty; corruption/overflow is reported, not a partial count.
func ReadBlocked(ctx context.Context, directory, project string, now time.Time, policy Policy) ([]Alert, error) {
	p, err := policy.normalized()
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(project) {
		return nil, errors.New("notification project must be absolute")
	}
	project = filepath.Clean(project)
	if resolved, err := filepath.EvalSymlinks(project); err == nil {
		project = resolved
	}
	groups := make(map[string]*Alert)
	seen := make(map[string]bool)
	cutoff := now.Add(-p.Window)
	err = audit.Visit(ctx, directory, func(event audit.Event) error {
		if event.Action != "block" || (event.Tier != "critical" && event.Tier != "dangerous") ||
			event.Timestamp.Before(cutoff) || event.Timestamp.After(now) {
			return nil
		}
		belongs, err := belongsToProject(event.CWD, project)
		if err != nil || !belongs {
			return err
		}
		if seen[event.ID] {
			return nil
		}
		if len(seen) >= maxEvents {
			return errors.New("blocked alert window exceeds 10000 events; shorten the window or inspect slb audit")
		}
		seen[event.ID] = true
		if event.CommandHash == "" {
			return errors.New("blocked audit event has no command fingerprint")
		}
		identity, _ := json.Marshal([]string{project, event.CWD, event.SessionID, event.CommandHash})
		digest := sha256.Sum256(identity)
		key := hex.EncodeToString(digest[:])
		a := groups[key]
		if a == nil {
			a = &Alert{Key: key, Project: project, CWD: event.CWD, SessionID: event.SessionID,
				CommandHash: event.CommandHash, Tier: event.Tier, FirstAt: event.Timestamp, WindowSeconds: int64(p.Window / time.Second)}
			groups[key] = a
		}
		a.Attempts++
		a.Repeated = a.Attempts >= p.RepeatThreshold
		if event.Tier == "critical" {
			a.Tier = "critical"
		}
		if event.Timestamp.Before(a.FirstAt) {
			a.FirstAt = event.Timestamp
		}
		if event.Timestamp.After(a.LastAt) || (event.Timestamp.Equal(a.LastAt) && event.ID > a.EventID) {
			a.LastAt, a.EventID, a.CommandRedacted = event.Timestamp, event.ID, event.CommandRedacted
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	alerts := make([]Alert, 0, len(groups))
	for _, a := range groups {
		alerts = append(alerts, *a)
	}
	// Urgent groups go first, with oldest first to avoid starvation on floods.
	sort.Slice(alerts, func(i, j int) bool {
		if alerts[i].Importance() != alerts[j].Importance() {
			return alerts[i].Importance() == "urgent"
		}
		if !alerts[i].FirstAt.Equal(alerts[j].FirstAt) {
			return alerts[i].FirstAt.Before(alerts[j].FirstAt)
		}
		return alerts[i].Key < alerts[j].Key
	})
	return alerts, nil
}

func belongsToProject(cwd, project string) (bool, error) {
	if !filepath.IsAbs(cwd) {
		return false, nil
	}
	cwd = filepath.Clean(cwd)
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	rel, err := filepath.Rel(project, cwd)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false, nil
	}
	// A nested initialized project owns its own events, not its parent's.
	for at := cwd; at != project; at = filepath.Dir(at) {
		if _, err := os.Lstat(filepath.Join(at, ".slb")); err == nil {
			return false, nil
		} else if !os.IsNotExist(err) {
			return false, fmt.Errorf("checking audit project scope: %w", err)
		}
	}
	return true, nil
}

// Route.ID must change when its destination changes. Store a destination hash,
// not an endpoint URL (which may contain credentials). Sends must honor ctx.
type Route struct {
	ID   string
	Send func(context.Context, Alert) error
}

type delivery struct {
	EventID  string    `json:"event_id,omitempty"`
	EventAt  time.Time `json:"event_at"`
	SentAt   time.Time `json:"sent_at"`
	SeenAt   time.Time `json:"seen_at"`
	Repeated bool      `json:"repeated"`
	Critical bool      `json:"critical"`
	RetryAt  time.Time `json:"retry_at"`
	Failures int       `json:"failures"`
}

type deliveryState struct {
	Version  int                 `json:"version"`
	Entries  map[string]delivery `json:"entries"`
	Attempts []time.Time         `json:"attempts"`
}

// Report distinguishes attempted deliveries from cooldown/budget suppression.
type Report struct {
	Sent, Failed, Suppressed, Deferred int
}

// Dispatcher persists delivery suppression and retry budgets across restarts.
// One dispatcher owns a project's ledger. It serializes concurrent calls in a
// process; it is NOT a cross-process lock or an exactly-once transport. A crash
// after external delivery but before receipt persistence can cause a duplicate.
type Dispatcher struct {
	StatePath string
	Policy    Policy
	mu        sync.Mutex
}

func (d *Dispatcher) Dispatch(ctx context.Context, now time.Time, alerts []Alert, routes []Route) (Report, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var report Report
	p, err := d.Policy.normalized()
	if err != nil {
		return report, err
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if d.StatePath == "" {
		return report, errors.New("notification state path is required")
	}
	ids := make(map[string]bool)
	for _, route := range routes {
		if route.ID == "" || route.Send == nil || ids[route.ID] {
			return report, errors.New("invalid or duplicate notification route")
		}
		ids[route.ID] = true
	}
	state, err := readState(d.StatePath)
	if err != nil {
		return report, err
	}
	for key, entry := range state.Entries {
		if entry.SeenAt.Before(now.Add(-p.Window)) {
			delete(state.Entries, key)
		}
	}
	attempts := state.Attempts[:0]
	for _, at := range state.Attempts {
		if at.After(now.Add(-time.Minute)) {
			attempts = append(attempts, at)
		}
	}
	state.Attempts = attempts
	for _, alert := range alerts {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if alert.Key == "" || alert.EventID == "" || alert.Attempts < 1 || alert.LastAt.IsZero() {
			return report, errors.New("invalid blocked alert")
		}
		if alert.LastAt.Before(now.Add(-p.Window)) || alert.LastAt.After(now) {
			continue
		}
		for _, route := range routes {
			if err := ctx.Err(); err != nil {
				return report, err
			}
			keyData, _ := json.Marshal([]string{alert.Key, route.ID})
			hash := sha256.Sum256(keyData)
			key := hex.EncodeToString(hash[:])
			entry := state.Entries[key]
			entry.SeenAt = alert.LastAt
			escalation := (alert.Repeated && !entry.Repeated) || (alert.Tier == "critical" && !entry.Critical)
			newEvent := alert.LastAt.After(entry.EventAt) || (alert.LastAt.Equal(entry.EventAt) && alert.EventID != entry.EventID)
			if !entry.SentAt.IsZero() && ((!newEvent && !escalation) || (!escalation && now.Sub(entry.SentAt) < p.Cooldown)) {
				report.Suppressed++
				continue
			}
			if now.Before(entry.RetryAt) || len(state.Attempts) >= p.MaxPerMinute {
				report.Deferred++
				continue
			}
			if len(state.Entries) >= 3*maxEvents {
				return report, errors.New("blocked delivery state capacity exceeded")
			}
			// Reserve retry/budget BEFORE I/O. A failed checkpoint means no send.
			entry.Failures = min(entry.Failures+1, 5)
			entry.RetryAt = now.Add(time.Duration(1<<uint(entry.Failures-1)) * 15 * time.Second)
			state.Entries[key] = entry
			state.Attempts = append(state.Attempts, now)
			if err := writeState(d.StatePath, state); err != nil {
				return report, err
			}
			sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := route.Send(sendCtx, alert)
			cancel()
			if err != nil {
				report.Failed++
				continue // Failure is never acknowledged as successful delivery.
			}
			entry.EventID, entry.EventAt, entry.SentAt = alert.EventID, alert.LastAt, now
			entry.Repeated, entry.Critical = alert.Repeated, alert.Tier == "critical"
			entry.Failures, entry.RetryAt = 0, time.Time{}
			state.Entries[key] = entry
			if err := writeState(d.StatePath, state); err != nil {
				return report, err
			}
			report.Sent++
		}
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	// Also persist pruning on otherwise idle scans; never retain old groups forever.
	if err := writeState(d.StatePath, state); err != nil {
		return report, err
	}
	if report.Failed > 0 {
		return report, errors.New("one or more blocked-alert transports failed; retry scheduled")
	}
	return report, nil
}

func readState(path string) (*deliveryState, error) {
	state := &deliveryState{Version: 1, Entries: make(map[string]delivery)}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxStateBytes || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return nil, errors.New("notification ledger must be a private regular file no larger than 4 MiB")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxStateBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxStateBytes {
		return nil, errors.New("notification ledger exceeds 4 MiB")
	}
	if err := json.Unmarshal(data, state); err != nil {
		return nil, fmt.Errorf("reading notification ledger: %w", err)
	}
	if state.Version != 1 || state.Entries == nil || len(state.Entries) > 3*maxEvents || len(state.Attempts) > 1000 {
		return nil, errors.New("invalid notification ledger schema")
	}
	return state, nil
}

func writeState(path string, state *deliveryState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(data) > maxStateBytes {
		return errors.New("notification ledger exceeds 4 MiB")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".blocked-delivery-*")
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
