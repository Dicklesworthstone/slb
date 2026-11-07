package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/audit"
)

func recordBlock(t *testing.T, dir, cwd, session, command, tier string, at time.Time) {
	t.Helper()
	if err := audit.Record(dir, audit.Event{Timestamp: at, CWD: cwd, SessionID: session,
		CommandHash: audit.CommandHash(command, cwd), CommandRedacted: "[REDACTED]",
		Action: "block", Tier: tier, Source: "offline"}); err != nil {
		t.Fatal(err)
	}
}

func TestReadBlockedGroupsScopesAndEscalates(t *testing.T) {
	dir, project := t.TempDir(), t.TempDir()
	now := time.Now().UTC()
	sub := filepath.Join(project, "src")
	if err := os.MkdirAll(sub, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		recordBlock(t, dir, sub, "agent-a", "secret-command", "dangerous", now.Add(-time.Duration(i)*time.Second))
	}
	recordBlock(t, dir, sub, "agent-b", "secret-command", "dangerous", now)
	recordBlock(t, dir, sub, "agent-a", "other-command", "critical", now)
	recordBlock(t, dir, project+"-other", "agent-a", "secret-command", "critical", now)
	nested := filepath.Join(project, "nested")
	if err := os.MkdirAll(filepath.Join(nested, ".slb"), 0700); err != nil {
		t.Fatal(err)
	}
	recordBlock(t, dir, nested, "agent-a", "secret-command", "critical", now)
	recordBlock(t, dir, sub, "agent-a", "secret-command", "dangerous", now.Add(-11*time.Minute))
	recordBlock(t, dir, sub, "agent-a", "secret-command", "dangerous", now.Add(time.Second))
	recordBlock(t, dir, sub, "agent-a", "caution-command", "caution", now)
	if err := audit.Record(dir, audit.Event{Timestamp: now, CWD: sub, Action: "ask", Tier: "critical"}); err != nil {
		t.Fatal(err)
	}
	alerts, err := ReadBlocked(context.Background(), dir, project, now, Policy{})
	if err != nil || len(alerts) != 3 {
		t.Fatalf("alerts=%+v err=%v", alerts, err)
	}
	var repeated *Alert
	for i := range alerts {
		if alerts[i].Repeated {
			repeated = &alerts[i]
		}
	}
	if repeated == nil || repeated.Attempts != 3 || repeated.SessionID != "agent-a" || repeated.Importance() != "urgent" {
		t.Fatalf("wrong repeated group: %+v", alerts)
	}
	if repeated.CommandRedacted != "[REDACTED]" || repeated.WindowSeconds != 600 {
		t.Fatal(repeated)
	}
	// A copied immutable record must not inflate the attempt count.
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "duplicate.jsonl"), data, 0600); err != nil {
		t.Fatal(err)
	}
	again, err := ReadBlocked(context.Background(), dir, project, now, Policy{})
	if err != nil || len(again) != 3 {
		t.Fatalf("duplicate changed groups: %+v %v", again, err)
	}
	for i := range alerts {
		if alerts[i] != again[i] {
			t.Fatalf("duplicate counted twice: %+v %+v", alerts, again)
		}
	}
}

func TestReadBlockedFailureAndCancellation(t *testing.T) {
	dir, project := t.TempDir(), t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadBlocked(ctx, dir, project, time.Now(), Policy{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := ReadBlocked(context.Background(), dir, "relative", time.Now(), Policy{}); err == nil {
		t.Fatal("relative scope accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, "corrupt.jsonl"), []byte("bad JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	if alerts, err := ReadBlocked(context.Background(), dir, project, time.Now(), Policy{}); err == nil || alerts != nil {
		t.Fatal("corrupt audit became partial result")
	}
}

func observedBlock(cwd, id, source string, at time.Time) audit.Event {
	return audit.Event{Version: 1, ID: id, ObservationID: strings.Repeat("a", 32), Timestamp: at,
		CWD: cwd, SessionID: "session", CommandHash: audit.CommandHash("command", cwd),
		CommandRedacted: "[REDACTED]", Action: "block", Tier: "dangerous", Source: source}
}

// Write the reader's input directly, including malformed external metadata
// that audit.Record would remove from newly published records.
func writeObservedBlock(t *testing.T, dir string, event audit.Event) []byte {
	t.Helper()
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(dir, event.ID+".jsonl"), data, 0600); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestReadBlockedCorrelatesDaemonAndHookObservations(t *testing.T) {
	for _, source := range []string{"hook_native_offline", "hook_native_timeout", "hook_native_daemon", "hook_offline", "hook_daemon"} {
		for _, daemonFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/daemon_first=%v", source, daemonFirst), func(t *testing.T) {
				dir, project := t.TempDir(), t.TempDir()
				now := time.Now().UTC()
				first := observedBlock(project, "first", "daemon", now.Add(-2*time.Second))
				second := observedBlock(project, "second", source, now.Add(-time.Second))
				second.Tier = "critical"
				second.CommandRedacted = "other redacted rendering"
				if !daemonFirst {
					first.Source, second.Source = source, "daemon"
					first.Tier, second.Tier = "critical", "dangerous"
				}
				// Publication order must not choose the representative or tier.
				secondData := writeObservedBlock(t, dir, second)
				firstData := writeObservedBlock(t, dir, first)
				alerts, err := ReadBlocked(context.Background(), dir, project, now, Policy{})
				if err != nil || len(alerts) != 1 {
					t.Fatalf("alerts=%+v error=%v", alerts, err)
				}
				a := alerts[0]
				if a.Attempts != 1 || a.Repeated || a.Tier != "critical" || a.Importance() != "urgent" ||
					a.EventID != first.ID || !a.FirstAt.Equal(first.Timestamp) || !a.LastAt.Equal(first.Timestamp) ||
					a.CommandRedacted != first.CommandRedacted {
					t.Fatalf("wrong correlated alert: %+v", a)
				}
				for id, before := range map[string][]byte{first.ID: firstData, second.ID: secondData} {
					after, err := os.ReadFile(filepath.Join(dir, id+".jsonl"))
					if err != nil || string(after) != string(before) {
						t.Fatalf("correlation altered immutable audit %s: %v", id, err)
					}
				}
				events, err := audit.Query(dir, audit.Filter{})
				if err != nil || len(events) != 2 {
					t.Fatalf("audit history lost observations: %+v %v", events, err)
				}
			})
		}
	}
}

func TestReadBlockedObservationReuseCountsConservatively(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sources  []string
		attempts int
		last     int
	}{
		{"same_daemon", []string{"daemon", "daemon", "daemon"}, 3, 2},
		{"same_hook", []string{"hook_offline", "hook_offline", "hook_offline"}, 3, 2},
		{"different_hooks", []string{"hook_native_timeout", "hook_offline"}, 2, 1},
		{"unknown_source", []string{"daemon", "offline"}, 2, 1},
		{"source_case", []string{"Daemon", "hook_offline"}, 2, 1},
		{"policy_source", []string{"daemon", "hook_native_policy"}, 2, 1},
		{"two_daemons_then_hook", []string{"daemon", "daemon", "hook_offline"}, 2, 1},
		{"two_hooks_then_daemon", []string{"hook_offline", "hook_offline", "daemon"}, 2, 1},
		{"pair_then_daemon", []string{"daemon", "hook_offline", "daemon"}, 2, 2},
		{"pair_then_hook", []string{"daemon", "hook_offline", "hook_offline"}, 2, 2},
		{"pair_then_other_hook", []string{"daemon", "hook_native_timeout", "hook_offline"}, 2, 2},
		{"two_pairs_same_id", []string{"daemon", "hook_offline", "daemon", "hook_offline"}, 3, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, project := t.TempDir(), t.TempDir()
			now := time.Now().UTC()
			for i, source := range tc.sources {
				writeObservedBlock(t, dir, observedBlock(project, fmt.Sprint(i), source, now.Add(time.Duration(i-10)*time.Second)))
			}
			alerts, err := ReadBlocked(context.Background(), dir, project, now, Policy{})
			if err != nil || len(alerts) != 1 || alerts[0].Attempts != tc.attempts ||
				alerts[0].Repeated != (tc.attempts >= 3) || alerts[0].EventID != fmt.Sprint(tc.last) {
				t.Fatalf("ambiguous observations miscounted: %+v %v", alerts, err)
			}
		})
	}
}

func TestReadBlockedInvalidOrDifferentObservationIDsRemainIndependent(t *testing.T) {
	for _, tc := range []struct{ name, first, second string }{
		{"missing", "", ""},
		{"short", strings.Repeat("a", 31), strings.Repeat("a", 31)},
		{"long", strings.Repeat("a", 33), strings.Repeat("a", 33)},
		{"uppercase", strings.Repeat("A", 32), strings.Repeat("A", 32)},
		{"non_hex", strings.Repeat("g", 32), strings.Repeat("g", 32)},
		{"space", strings.Repeat("a", 31) + " ", strings.Repeat("a", 31) + " "},
		{"different", strings.Repeat("a", 32), strings.Repeat("b", 32)},
		{"one_missing", strings.Repeat("a", 32), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, project := t.TempDir(), t.TempDir()
			now := time.Now().UTC()
			first := observedBlock(project, "first", "daemon", now.Add(-time.Second))
			second := observedBlock(project, "second", "hook_offline", now)
			first.ObservationID, second.ObservationID = tc.first, tc.second
			writeObservedBlock(t, dir, first)
			writeObservedBlock(t, dir, second)
			alerts, err := ReadBlocked(context.Background(), dir, project, now, Policy{})
			if err != nil || len(alerts) != 1 || alerts[0].Attempts != 2 || alerts[0].EventID != second.ID {
				t.Fatalf("independent observations lost: %+v %v", alerts, err)
			}
		})
	}
}

func TestReadBlockedObservationDoesNotCrossContext(t *testing.T) {
	for _, changed := range []string{"cwd", "session", "command_hash", "project", "nested_project"} {
		t.Run(changed, func(t *testing.T) {
			dir, project := t.TempDir(), t.TempDir()
			now := time.Now().UTC()
			first := observedBlock(project, "first", "daemon", now.Add(-time.Second))
			second := observedBlock(project, "second", "hook_offline", now)
			groups := 2
			switch changed {
			case "cwd":
				second.CWD = filepath.Join(project, "src")
			case "session":
				second.SessionID = "another-session"
			case "command_hash":
				second.CommandHash = audit.CommandHash("another-command", project)
			case "project":
				second.CWD = t.TempDir()
				groups = 1
			case "nested_project":
				second.CWD = filepath.Join(project, "nested")
				if err := os.MkdirAll(filepath.Join(second.CWD, ".slb"), 0700); err != nil {
					t.Fatal(err)
				}
				groups = 1
			}
			writeObservedBlock(t, dir, first)
			writeObservedBlock(t, dir, second)
			alerts, err := ReadBlocked(context.Background(), dir, project, now, Policy{})
			if err != nil || len(alerts) != groups {
				t.Fatalf("contexts combined: %+v %v", alerts, err)
			}
			for _, a := range alerts {
				if a.Attempts != 1 {
					t.Fatalf("context has another attempt: %+v", a)
				}
			}
			if groups == 1 {
				other, err := ReadBlocked(context.Background(), dir, second.CWD, now, Policy{})
				if err != nil || len(other) != 1 || other[0].Attempts != 1 || other[0].EventID != second.ID {
					t.Fatalf("other project lost its attempt: %+v %v", other, err)
				}
			}
		})
	}
}

func TestReadBlockedNonBlockObservationCannotEraseBlock(t *testing.T) {
	for _, ignored := range []string{"daemon_ask", "hook_ask", "daemon_caution", "hook_unknown", "future_daemon"} {
		t.Run(ignored, func(t *testing.T) {
			dir, project := t.TempDir(), t.TempDir()
			now := time.Now().UTC()
			first := observedBlock(project, "first", "daemon", now.Add(-time.Second))
			second := observedBlock(project, "second", "hook_offline", now)
			wantID := second.ID
			switch ignored {
			case "daemon_ask":
				first.Action = "ask"
			case "hook_ask":
				second.Action, wantID = "ask", first.ID
			case "daemon_caution":
				first.Tier = "caution"
			case "hook_unknown":
				second.Tier, wantID = "unknown", first.ID
			case "future_daemon":
				first.Timestamp = now.Add(time.Second)
			}
			writeObservedBlock(t, dir, first)
			writeObservedBlock(t, dir, second)
			alerts, err := ReadBlocked(context.Background(), dir, project, now, Policy{})
			if err != nil || len(alerts) != 1 || alerts[0].Attempts != 1 || alerts[0].EventID != wantID {
				t.Fatalf("block suppressed by unrelated decision: %+v %v", alerts, err)
			}
		})
	}
}

func TestReadBlockedCorrelationWindowBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name          string
		first, second time.Duration
		attempts      int
	}{
		{"earliest_at_cutoff", -10 * time.Second, -8 * time.Second, 1},
		{"earliest_before_cutoff", -12 * time.Second, -8 * time.Second, 0},
		{"lookback_boundary", -20 * time.Second, -10 * time.Second, 0},
		{"outside_lookback", -20*time.Second - time.Nanosecond, -10 * time.Second, 1},
		{"counterpart_too_far", -16 * time.Second, -5 * time.Second, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, project := t.TempDir(), t.TempDir()
			now := time.Now().UTC()
			first := observedBlock(project, "first", "daemon", now.Add(tc.first))
			second := observedBlock(project, "second", "hook_offline", now.Add(tc.second))
			writeObservedBlock(t, dir, first)
			writeObservedBlock(t, dir, second)
			alerts, err := ReadBlocked(context.Background(), dir, project, now, Policy{Window: 10 * time.Second})
			if err != nil || len(alerts) != tc.attempts {
				t.Fatalf("wrong correlation boundary: %+v %v", alerts, err)
			}
			if tc.attempts != 0 && alerts[0].Attempts != 1 {
				t.Fatalf("wrong surviving attempts: %+v", alerts)
			}
		})
	}
}

func TestReadBlockedCorrelationKeepsPhysicalEventBound(t *testing.T) {
	dir, project := t.TempDir(), t.TempDir()
	now := time.Now().UTC()
	for i := 0; i < maxEvents; i++ {
		source := "daemon"
		if i%2 != 0 {
			source = "hook_offline"
		}
		event := observedBlock(project, fmt.Sprintf("%032x", i+1), source, now)
		event.ObservationID = fmt.Sprintf("%032x", i/2+1)
		writeObservedBlock(t, dir, event)
	}
	policy := Policy{Window: 10 * time.Second}
	alerts, err := ReadBlocked(context.Background(), dir, project, now, policy)
	if err != nil || len(alerts) != 1 || alerts[0].Attempts != maxEvents/2 || !alerts[0].Repeated {
		t.Fatalf("exact physical bound rejected valid pairs: %+v %v", alerts, err)
	}
	writeObservedBlock(t, dir, observedBlock(project, "overflow", "daemon", now))
	for _, at := range []time.Time{now, now.Add(policy.Window + time.Nanosecond)} {
		alerts, err := ReadBlocked(context.Background(), dir, project, at, policy)
		if err == nil || alerts != nil || !strings.Contains(err.Error(), "correlation lookback") {
			t.Fatalf("physical overflow became partial or understated count: %+v %v", alerts, err)
		}
	}
	// Neither expired correlation candidates nor unrelated projects consume
	// another scan's bounded capacity.
	alerts, err = ReadBlocked(context.Background(), dir, project, now.Add(2*policy.Window+time.Nanosecond), policy)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("expired lookback consumed capacity: %+v %v", alerts, err)
	}
	alerts, err = ReadBlocked(context.Background(), dir, t.TempDir(), now, policy)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("other project consumed capacity: %+v %v", alerts, err)
	}
}

func TestReadBlockedCorrelationBoundsRetainedText(t *testing.T) {
	for _, field := range []string{"display", "unused_audit_detail"} {
		t.Run(field, func(t *testing.T) {
			dir, project := t.TempDir(), t.TempDir()
			now := time.Now().UTC()
			large := strings.Repeat("x", 60*1024)
			for i := 0; i < 160; i++ {
				event := observedBlock(project, fmt.Sprint(i), "daemon", now)
				if field == "display" {
					event.CommandRedacted = large
				} else {
					event.MatchedPattern = large
				}
				writeObservedBlock(t, dir, event)
			}
			alerts, err := ReadBlocked(context.Background(), dir, project, now, Policy{})
			if field == "display" {
				if err == nil || alerts != nil || !strings.Contains(err.Error(), "8 MiB of retained event text") {
					t.Fatalf("retained text overflow became partial alerts: %+v %v", alerts, err)
				}
			} else if err != nil || len(alerts) != 1 || alerts[0].Attempts != 160 {
				t.Fatalf("unused audit details consumed aggregation memory budget: %+v %v", alerts, err)
			}
		})
	}
}

func TestDispatcherLateObservationCounterpartDoesNotRemind(t *testing.T) {
	for _, tc := range []struct {
		name        string
		delay       time.Duration
		critical    bool
		wantEventID string
	}{
		{"later_timestamp", 2 * time.Second, false, "b"},
		{"earlier_timestamp", -time.Second, false, "a"},
		{"equal_timestamp", 0, false, "a"},
		{"higher_severity", 2 * time.Second, true, "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, project := t.TempDir(), t.TempDir()
			now := time.Now().UTC()
			first := observedBlock(project, "b", "hook_offline", now)
			writeObservedBlock(t, dir, first)
			var sent []Alert
			statePath := filepath.Join(t.TempDir(), "delivery.json")
			s := &Service{Project: project, AuditDirectory: dir, Dispatcher: &Dispatcher{StatePath: statePath},
				Routes: []Route{{ID: "test-route", Send: func(_ context.Context, a Alert) error { sent = append(sent, a); return nil }}}}
			if report, err := s.Check(context.Background(), now); err != nil || report.Sent != 1 {
				t.Fatalf("first observation not delivered: %+v %v", report, err)
			}
			second := observedBlock(project, "a", "daemon", now.Add(tc.delay))
			if tc.critical {
				second.Tier = "critical"
			}
			writeObservedBlock(t, dir, second)
			// Publication after cooldown and a dispatcher restart must not
			// turn the counterpart into a new attempt or reminder.
			s.Dispatcher = &Dispatcher{StatePath: statePath}
			later := now.Add(time.Minute + time.Second)
			alerts, err := ReadBlocked(context.Background(), dir, project, later, Policy{})
			if err != nil || len(alerts) != 1 || alerts[0].Attempts != 1 || alerts[0].EventID != tc.wantEventID {
				t.Fatalf("late counterpart changed attempt identity: %+v %v", alerts, err)
			}
			report, err := s.Check(context.Background(), later)
			if tc.critical {
				if err != nil || report.Sent != 1 || len(sent) != 2 || sent[1].Tier != "critical" || sent[1].Attempts != 1 {
					t.Fatalf("higher severity was hidden: %+v sent=%+v %v", report, sent, err)
				}
			} else if err != nil || report.Suppressed != 1 || report.Sent != 0 || len(sent) != 1 {
				t.Fatalf("late duplicate created reminder: %+v sent=%+v %v", report, sent, err)
			}
			// The later counterpart can remain inside the window after the
			// representative expires; the old attempt must not reappear.
			if report, err := s.Check(context.Background(), now.Add(10*time.Minute+time.Second)); err != nil || report != (Report{}) {
				t.Fatalf("expired attempt revived at cutoff: %+v %v", report, err)
			}
		})
	}
}

func testAlert(now time.Time) Alert {
	return Alert{Key: "group-a", EventID: "event-a", Project: "/project", CWD: "/project/src", CommandRedacted: "[REDACTED]",
		CommandHash: strings.Repeat("a", 64), Tier: "dangerous", Attempts: 1, FirstAt: now, LastAt: now, WindowSeconds: 600}
}

func TestDispatcherPersistsSuppressionAndEscalation(t *testing.T) {
	now := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "private", "delivery.json")
	d := &Dispatcher{StatePath: path}
	calls := 0
	routes := []Route{{ID: "mail:destination-hash", Send: func(context.Context, Alert) error { calls++; return nil }}}
	a := testAlert(now)
	if report, err := d.Dispatch(context.Background(), now, []Alert{a}, routes); err != nil || report.Sent != 1 {
		t.Fatalf("%+v %v", report, err)
	}
	// New dispatcher = restart: successful delivery must not replay.
	d = &Dispatcher{StatePath: path}
	if report, err := d.Dispatch(context.Background(), now.Add(time.Second), []Alert{a}, routes); err != nil || report.Suppressed != 1 {
		t.Fatalf("%+v %v", report, err)
	}
	a.EventID, a.LastAt, a.Attempts = "event-b", now.Add(2*time.Second), 2
	if report, err := d.Dispatch(context.Background(), a.LastAt, []Alert{a}, routes); err != nil || report.Suppressed != 1 {
		t.Fatalf("cooldown: %+v %v", report, err)
	}
	a.EventID, a.LastAt, a.Attempts, a.Repeated = "event-c", now.Add(3*time.Second), 3, true
	if report, err := d.Dispatch(context.Background(), a.LastAt, []Alert{a}, routes); err != nil || report.Sent != 1 {
		t.Fatalf("escalation: %+v %v", report, err)
	}
	if report, err := d.Dispatch(context.Background(), a.LastAt.Add(time.Second), []Alert{a}, routes); err != nil || report.Suppressed != 1 {
		t.Fatalf("replayed escalation: %+v %v", report, err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("ledger permissions: %v %v", info, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(data), "REDACTED") || strings.Contains(string(data), "/project") {
		t.Fatalf("ledger persisted display data: %s %v", data, err)
	}
}

func TestDispatcherRetriesFailuresIndependentlyAndBoundsAttempts(t *testing.T) {
	now := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "delivery.json")
	d := &Dispatcher{StatePath: path, Policy: Policy{MaxPerMinute: 3}}
	mailCalls, webhookCalls := 0, 0
	routes := []Route{
		{ID: "mail", Send: func(context.Context, Alert) error {
			mailCalls++
			if mailCalls == 1 {
				return errors.New("unavailable")
			}
			return nil
		}},
		{ID: "webhook", Send: func(context.Context, Alert) error { webhookCalls++; return nil }},
	}
	a := testAlert(now)
	if report, err := d.Dispatch(context.Background(), now, []Alert{a}, routes); err == nil || report.Failed != 1 || report.Sent != 1 {
		t.Fatalf("%+v %v", report, err)
	}
	d = &Dispatcher{StatePath: path, Policy: Policy{MaxPerMinute: 3}}
	if report, err := d.Dispatch(context.Background(), now.Add(time.Second), []Alert{a}, routes); err != nil || report.Deferred != 1 || report.Suppressed != 1 {
		t.Fatalf("retry budget lost: %+v %v", report, err)
	}
	if report, err := d.Dispatch(context.Background(), now.Add(15*time.Second), []Alert{a}, routes); err != nil || report.Sent != 1 {
		t.Fatalf("retry: %+v %v", report, err)
	}
	b := a
	b.Key, b.EventID = "group-b", "event-b"
	if report, err := d.Dispatch(context.Background(), now.Add(20*time.Second), []Alert{b}, routes); err != nil || report.Deferred != 2 {
		t.Fatalf("global attempt budget: %+v %v", report, err)
	}
	if mailCalls != 2 || webhookCalls != 1 {
		t.Fatalf("mail=%d webhook=%d", mailCalls, webhookCalls)
	}
	if report, err := d.Dispatch(context.Background(), now.Add(time.Minute), []Alert{b}, routes); err != nil || report.Sent != 2 {
		t.Fatalf("budget reset: %+v %v", report, err)
	}
}

func TestDispatcherCheckpointFailurePreventsSend(t *testing.T) {
	now := time.Now().UTC()
	for _, kind := range []string{"corrupt", "unwritable-parent", "symlink", "version"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			switch kind {
			case "corrupt":
				if err := os.WriteFile(path, []byte("bad"), 0600); err != nil {
					t.Fatal(err)
				}
			case "version":
				if err := os.WriteFile(path, []byte(`{"version":42,"entries":{}}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "unwritable-parent":
				if err := os.WriteFile(path, []byte("not a directory"), 0600); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(path, "state.json")
			case "symlink":
				if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), path); err != nil {
					t.Skip(err)
				}
			}
			calls := 0
			d := &Dispatcher{StatePath: path}
			_, err := d.Dispatch(context.Background(), now, []Alert{testAlert(now)}, []Route{{ID: "route", Send: func(context.Context, Alert) error { calls++; return nil }}})
			if err == nil || calls != 0 {
				t.Fatalf("checkpoint failure sent notification: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestDispatcherConcurrentCallsAndCancellation(t *testing.T) {
	now := time.Now().UTC()
	d := &Dispatcher{StatePath: filepath.Join(t.TempDir(), "state.json")}
	var calls atomic.Int32
	routes := []Route{{ID: "route", Send: func(context.Context, Alert) error { calls.Add(1); return nil }}}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := d.Dispatch(context.Background(), now, []Alert{testAlert(now)}, routes); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("concurrent duplicate", calls.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.Dispatch(ctx, now, []Alert{testAlert(now)}, routes); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestNotificationPolicyBounds(t *testing.T) {
	for _, policy := range []Policy{{Window: -1}, {Window: 25 * time.Hour}, {RepeatThreshold: 1}, {RepeatThreshold: maxEvents + 1}, {Cooldown: -1}, {MaxPerMinute: -1}, {MaxPerMinute: 1001}} {
		if _, err := policy.normalized(); err == nil {
			t.Fatalf("accepted policy %+v", policy)
		}
	}
}
