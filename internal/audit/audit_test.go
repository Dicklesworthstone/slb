package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDaemonDiagnosticsPersistWithoutChangingOlderAuditEvents(t *testing.T) {
	for _, tc := range []struct {
		name, socketPath, daemonError string
	}{
		{"both fields", "/tmp/custom-daemon.sock", "identity_mismatch"},
		{"socket only", "/tmp/custom-daemon.sock", ""},
		{"category only", "", "discovery_invalid"},
		{"older event", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			event := sampleEvent(time.Now())
			event.Source = "hook_native_offline"
			event.SocketPath, event.DaemonError = tc.socketPath, tc.daemonError
			if err := Record(directory, event); err != nil {
				t.Fatal(err)
			}
			events, err := Query(directory, Filter{})
			if err != nil || len(events) != 1 || events[0].SocketPath != tc.socketPath ||
				events[0].DaemonError != tc.daemonError || events[0].Source != event.Source {
				t.Fatalf("persisted daemon diagnostics changed: %+v, %v", events, err)
			}

			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 1 {
				t.Fatalf("expected one persisted event: %+v, %v", entries, err)
			}
			data, err := os.ReadFile(filepath.Join(directory, entries[0].Name()))
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			for field, want := range map[string]string{"socket_path": tc.socketPath, "daemon_error": tc.daemonError} {
				value, present := fields[field]
				if present != (want != "") || (present && value != want) {
					t.Fatalf("optional %s field changed the stored schema: %s", field, data)
				}
			}
		})
	}
}

func sampleEvent(at time.Time) Event {
	return Event{
		Timestamp: at, CommandRedacted: "git push --force", CommandHash: CommandHash("git push --force", "/project"),
		CWD: "/project", SessionID: "agent-session", Action: "block", Tier: "dangerous",
		MinApprovals: 1, Source: "daemon",
	}
}

func TestObservationMetadataPreservesIndependentAuditRecords(t *testing.T) {
	directory := t.TempDir()
	observation := NewObservationID()
	if !ValidObservationID(observation) {
		t.Fatalf("generated observation ID has invalid format: %q", observation)
	}
	if next := NewObservationID(); !ValidObservationID(next) || next == observation {
		t.Fatal("independent hook invocations did not receive distinct valid IDs")
	}
	for _, source := range []string{"daemon", "hook_native_timeout"} {
		event := sampleEvent(time.Now())
		event.ID, event.ObservationID, event.Source = "caller-selected-record", observation, source
		if err := Record(directory, event); err != nil {
			t.Fatal(err)
		}
	}
	events, err := Query(directory, Filter{})
	if err != nil || len(events) != 2 {
		t.Fatalf("correlated observations lost immutable evidence: %+v, %v", events, err)
	}
	if events[0].ID == events[1].ID {
		t.Fatal("correlation replaced independent event identities")
	}
	for _, event := range events {
		if event.ObservationID != observation || !ValidObservationID(event.ID) || event.ID == observation ||
			event.Version != 1 || event.Action != "block" || event.Tier != "dangerous" {
			t.Fatalf("correlation changed the audit record's identity or decision: %+v", event)
		}
	}
}

func TestInvalidObservationMetadataDoesNotLoseAudit(t *testing.T) {
	for _, tc := range []struct{ name, id string }{
		{"legacy", ""}, {"short", strings.Repeat("a", 31)},
		{"long", strings.Repeat("a", 33)}, {"uppercase", strings.Repeat("A", 32)},
		{"nonhex", strings.Repeat("g", 32)}, {"unicode", strings.Repeat("é", 16)},
		{"control", strings.Repeat("a", 31) + "\n"}, {"oversized", strings.Repeat("a", maxRecordBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if ValidObservationID(tc.id) {
				t.Fatal("invalid or absent correlation metadata accepted")
			}
			directory := t.TempDir()
			event := sampleEvent(time.Now())
			event.ObservationID = tc.id
			if err := Record(directory, event); err != nil {
				t.Fatalf("optional metadata prevented recording the decision: %v", err)
			}
			events, err := Query(directory, Filter{})
			if err != nil || len(events) != 1 || events[0].ObservationID != "" || events[0].CommandHash != event.CommandHash {
				t.Fatalf("invalid metadata damaged audit evidence: %+v, %v", events, err)
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 1 {
				t.Fatalf("audit publication: %v, %v", entries, err)
			}
			data, err := os.ReadFile(filepath.Join(directory, entries[0].Name()))
			if err != nil || strings.Contains(string(data), "observation_id") {
				t.Fatalf("invalid optional metadata was persisted: %v", err)
			}
		})
	}
}

func TestRecordQueryAndRetention(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "audit")
	now := time.Now().UTC()
	for _, at := range []time.Time{now.Add(-2 * time.Hour), now, now.Add(-time.Hour)} {
		if err := Record(directory, sampleEvent(at)); err != nil {
			t.Fatal(err)
		}
	}
	events, err := Query(directory, Filter{Limit: 2})
	if err != nil || len(events) != 2 || !events[0].Timestamp.Equal(now) || !events[1].Timestamp.Equal(now.Add(-time.Hour)) {
		t.Fatalf("incorrect newest-first selection: %+v, %v", events, err)
	}
	for _, filter := range []Filter{
		{SessionID: "someone-else"}, {CWD: "/elsewhere"}, {Action: "ask"}, {Tier: "critical"}, {Query: "nonmatching"},
	} {
		matches, err := Query(directory, filter)
		if err != nil || len(matches) != 0 {
			t.Fatalf("filter %+v unexpectedly matched: %+v, %v", filter, matches, err)
		}
	}
	events, err = Query(directory, Filter{Since: now.Add(-time.Hour), Before: now, Query: "PUSH"})
	if err != nil || len(events) != 1 {
		t.Fatalf("inclusive since/exclusive before or search failed: %+v, %v", events, err)
	}
	if count, err := Prune(directory, now, true); err != nil || count != 2 {
		t.Fatalf("dry run: %d, %v", count, err)
	}
	if events, _ := Query(directory, Filter{}); len(events) != 3 {
		t.Fatal("dry run deleted records")
	}
	if count, err := Prune(directory, now, false); err != nil || count != 2 {
		t.Fatalf("pruning: %d, %v", count, err)
	}
	if events, _ := Query(directory, Filter{}); len(events) != 1 || !events[0].Timestamp.Equal(now) {
		t.Fatalf("retention removed the boundary/newest record: %+v", events)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		info, err := entries[0].Info()
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("audit file permissions are not private: %v, %v", info, err)
		}
	}
}

func TestConcurrentAuditWritersDoNotLoseOrInterleaveRecords(t *testing.T) {
	directory := t.TempDir()
	const writers = 64
	var wg sync.WaitGroup
	errors := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errors <- Record(directory, sampleEvent(time.Now()))
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	events, err := Query(directory, Filter{Limit: writers})
	if err != nil || len(events) != writers {
		t.Fatalf("lost concurrent audit records: %d, %v", len(events), err)
	}
	ids := make(map[string]bool)
	for _, event := range events {
		if ids[event.ID] {
			t.Fatal("duplicate event ID")
		}
		ids[event.ID] = true
	}
}

func TestAuditCorruptionIsVisibleAndPruneDoesNotDeletePartially(t *testing.T) {
	directory := t.TempDir()
	if err := Record(directory, sampleEvent(time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "broken.jsonl"), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Query(directory, Filter{}); err == nil {
		t.Fatal("corruption was silently ignored")
	}
	if count, err := Prune(directory, time.Now(), false); err == nil || count != 0 {
		t.Fatalf("pruned before validating records: %d, %v", count, err)
	}
}

func TestAuditIgnoresPendingFilesAndRejectsInvalidInputs(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, ".pending-incomplete"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if events, err := Query(directory, Filter{}); err != nil || len(events) != 0 {
		t.Fatalf("unpublished event was read: %+v, %v", events, err)
	}
	if events, err := Query(filepath.Join(directory, "missing"), Filter{}); err != nil || len(events) != 0 {
		t.Fatalf("empty history failed: %+v, %v", events, err)
	}
	for _, limit := range []int{-1, 10001} {
		if _, err := Query(directory, Filter{Limit: limit}); err == nil {
			t.Fatal("invalid limit accepted")
		}
	}
	if _, err := Prune(directory, time.Time{}, false); err == nil {
		t.Fatal("implicit retention cutoff accepted")
	}
	event := sampleEvent(time.Now())
	event.Action = "allow"
	if err := Record(directory, event); err == nil {
		t.Fatal("allowed decision logged as blocked")
	}
	event.Action = "block"
	event.CommandRedacted = strings.Repeat("x", maxRecordBytes)
	if err := Record(directory, event); err == nil {
		t.Fatal("oversized record accepted")
	}
	if CommandHash("ab", "c") == CommandHash("a", "bc") {
		t.Fatal("ambiguous command/cwd fingerprint")
	}
}
