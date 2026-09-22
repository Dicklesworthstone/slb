package notifications

import (
	"context"
	"errors"
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
