// Package audit stores hook decisions as private, immutable JSONL records.
// One record per file permits atomic publication by independent hook processes
// without an append lock, and lets retention run without rewriting live logs.
package audit

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const maxRecordBytes = 64 * 1024

// Event describes a denied or confirmation-required command, not its execution.
// CommandRedacted must be redacted by the caller before it reaches this store.
type Event struct {
	Version         int       `json:"version"`
	ID              string    `json:"id"`
	Timestamp       time.Time `json:"timestamp"`
	CommandRedacted string    `json:"command_redacted"`
	CommandHash     string    `json:"command_hash"`
	CWD             string    `json:"cwd"`
	SessionID       string    `json:"session_id,omitempty"`
	Action          string    `json:"action"`
	Tier            string    `json:"tier"`
	MatchedPattern  string    `json:"matched_pattern,omitempty"`
	MinApprovals    int       `json:"min_approvals"`
	Source          string    `json:"source"`
}

// Filter selects audit records. Limit defaults to 100 and cannot exceed 10000.
type Filter struct {
	Since     time.Time
	Before    time.Time
	SessionID string
	CWD       string
	Action    string
	Tier      string
	Query     string
	Limit     int
}

// DefaultDirectory is shared by the daemon and generated offline hook.
func DefaultDirectory() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".slb", "audit", "blocked"), nil
}

// CommandHash fingerprints the exact raw command and working directory without
// persisting plaintext secrets. It is an audit correlation key, not an approval.
func CommandHash(command, cwd string) string {
	sum := sha256.Sum256([]byte(command + "\x00" + cwd))
	return hex.EncodeToString(sum[:])
}

// Record syncs and atomically publishes an event before returning success. It does not change
// the hook decision when storage fails; callers must expose that failure.
func Record(directory string, event Event) error {
	if directory == "" {
		return errors.New("audit directory is required")
	}
	if event.Action != "block" && event.Action != "ask" {
		return errors.New("only block and ask decisions belong in the blocked audit")
	}
	event.Version = 1
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	event.Timestamp = event.Timestamp.UTC()
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return fmt.Errorf("generating audit ID: %w", err)
	}
	event.ID = hex.EncodeToString(id[:])
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encoding audit event: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxRecordBytes {
		return errors.New("audit record exceeds 64 KiB")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return fmt.Errorf("creating audit directory: %w", err)
	}
	file, err := os.CreateTemp(directory, ".pending-*")
	if err != nil {
		return fmt.Errorf("creating audit record: %w", err)
	}
	pending := file.Name()
	defer func() {
		_ = file.Close()
		_ = os.Remove(pending)
	}()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("writing audit record: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("syncing audit record: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing audit record: %w", err)
	}
	name := event.Timestamp.Format("20060102T150405.000000000Z") + "_" + event.ID + ".jsonl"
	if err := os.Rename(pending, filepath.Join(directory, name)); err != nil {
		return fmt.Errorf("publishing audit record: %w", err)
	}
	return nil
}

// Query returns newest matching records. Corrupt published records are errors,
// never silently omitted. Unpublished temporary files and symlinks are ignored.
func Query(directory string, filter Filter) ([]Event, error) {
	if filter.Limit == 0 {
		filter.Limit = 100
	}
	if filter.Limit < 1 || filter.Limit > 10000 {
		return nil, errors.New("audit limit must be between 1 and 10000")
	}
	if !filter.Before.IsZero() && !filter.Since.IsZero() && !filter.Since.Before(filter.Before) {
		return nil, errors.New("audit since must be earlier than before")
	}
	result := make([]Event, 0)
	query := strings.ToLower(filter.Query)
	err := walk(directory, func(_ string, event Event) error {
		if (!filter.Since.IsZero() && event.Timestamp.Before(filter.Since)) ||
			(!filter.Before.IsZero() && !event.Timestamp.Before(filter.Before)) ||
			(filter.SessionID != "" && event.SessionID != filter.SessionID) ||
			(filter.CWD != "" && event.CWD != filter.CWD) ||
			(filter.Action != "" && event.Action != filter.Action) ||
			(filter.Tier != "" && event.Tier != filter.Tier) ||
			(query != "" && !strings.Contains(strings.ToLower(event.CommandRedacted), query) && !strings.Contains(strings.ToLower(event.MatchedPattern), query)) {
			return nil
		}
		// Keep only the requested newest records, regardless of directory order.
		index := sort.Search(len(result), func(i int) bool {
			return event.Timestamp.After(result[i].Timestamp) ||
				(event.Timestamp.Equal(result[i].Timestamp) && event.ID > result[i].ID)
		})
		if index < filter.Limit {
			result = append(result, Event{})
			copy(result[index+1:], result[index:])
			result[index] = event
			if len(result) > filter.Limit {
				result = result[:filter.Limit]
			}
		}
		return nil
	})
	return result, err
}

// Prune removes only published, valid records strictly before the cutoff.
// A dry run reports the same selection without deleting anything. Concurrent
// writers publish different immutable files and cannot lose records to rotation.
func Prune(directory string, before time.Time, dryRun bool) (int, error) {
	if before.IsZero() {
		return 0, errors.New("an explicit audit retention cutoff is required")
	}
	var paths []string
	if err := walk(directory, func(path string, event Event) error {
		if event.Timestamp.Before(before) {
			paths = append(paths, path)
		}
		return nil
	}); err != nil {
		return 0, err
	}
	if dryRun {
		return len(paths), nil
	}
	removed := 0
	for _, path := range paths {
		if err := os.Remove(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return removed, fmt.Errorf("pruning audit record: %w", err)
		}
		removed++
	}
	return removed, nil
}

func walk(directory string, visit func(string, Event) error) error {
	return walkContext(context.Background(), directory, visit)
}

// Visit streams validated records without a newest-N limit. Consumers can
// scope before aggregating, so unrelated project traffic cannot hide events.
// Ordering is unspecified. Returning an error stops the scan immediately.
func Visit(ctx context.Context, directory string, visit func(Event) error) error {
	if visit == nil {
		return errors.New("audit visitor is required")
	}
	return walkContext(ctx, directory, func(_ string, event Event) error { return visit(event) })
}

func walkContext(ctx context.Context, directory string, visit func(string, Event) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if directory == "" {
		return errors.New("audit directory is required")
	}
	dir, err := os.Open(directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("opening audit directory: %w", err)
	}
	defer dir.Close()
	for {
		entries, readErr := dir.ReadDir(128)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".jsonl") || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			path := filepath.Join(directory, entry.Name())
			event, err := readEvent(path)
			if os.IsNotExist(err) {
				continue // Concurrent retention already removed this record.
			}
			if err != nil {
				return fmt.Errorf("reading audit record %s: %w", entry.Name(), err)
			}
			if err := visit(path, event); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("listing audit records: %w", readErr)
		}
	}
}

func readEvent(path string) (Event, error) {
	var event Event
	file, err := os.Open(path)
	if err != nil {
		return event, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxRecordBytes+1))
	if err != nil {
		return event, err
	}
	if len(data) > maxRecordBytes {
		return event, errors.New("record exceeds 64 KiB")
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return event, err
	}
	if event.Version != 1 || event.ID == "" || event.Timestamp.IsZero() || (event.Action != "block" && event.Action != "ask") {
		return event, errors.New("invalid audit event schema")
	}
	return event, nil
}
