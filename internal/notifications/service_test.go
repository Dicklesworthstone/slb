package notifications

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestServiceAuditToWebhookAndRestart(t *testing.T) {
	project, directory := t.TempDir(), t.TempDir()
	var deliveries atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { deliveries.Add(1); w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	cfg := Config{Enabled: true}
	destinations := Destinations{WebhookURL: server.URL}
	now := time.Now().UTC()
	recordBlock(t, directory, project, "offline-session", "rm example", "dangerous", now)
	s, err := NewService(project, directory, cfg, destinations)
	if err != nil {
		t.Fatal(err)
	}
	if report, err := s.Check(context.Background(), now); err != nil || report.Sent != 1 {
		t.Fatalf("%+v %v", report, err)
	}
	s, err = NewService(project, directory, cfg, destinations)
	if err != nil {
		t.Fatal(err)
	}
	if report, err := s.Check(context.Background(), now.Add(time.Second)); err != nil || report.Suppressed != 1 {
		t.Fatalf("restart: %+v %v", report, err)
	}
	recordBlock(t, directory, project, "offline-session", "rm example", "dangerous", now.Add(2*time.Second))
	recordBlock(t, directory, project, "offline-session", "rm example", "dangerous", now.Add(3*time.Second))
	if report, err := s.Check(context.Background(), now.Add(4*time.Second)); err != nil || report.Sent != 1 {
		t.Fatalf("repeat escalation: %+v %v", report, err)
	}
	if deliveries.Load() != 2 {
		t.Fatal("deliveries", deliveries.Load())
	}
}

func TestServiceDisabledAndInvalidConfiguration(t *testing.T) {
	project := t.TempDir()
	s, err := NewService(project, "", Config{BlockedTemplate: "/missing"}, Destinations{WebhookURL: "bad"})
	if err != nil || s != nil {
		t.Fatalf("disabled service accessed dependencies: %v %v", s, err)
	}
	s.Run(context.Background(), 0, nil)
	if _, err := os.Stat(filepath.Join(project, ".slb")); !os.IsNotExist(err) {
		t.Fatal("disabled service created state")
	}
	for _, cfg := range []Config{{Enabled: true, WindowSeconds: -1}, {Enabled: true, WindowSeconds: 86401}, {Enabled: true, RepeatThreshold: 1}, {Enabled: true, MaxPerMinute: 1001}} {
		if _, err := NewService(project, t.TempDir(), cfg, Destinations{WebhookURL: "http://localhost:8765"}); err == nil {
			t.Fatal("invalid policy accepted", cfg)
		}
	}
	if _, err := NewService(project, t.TempDir(), Config{Enabled: true}, Destinations{}); err == nil {
		t.Fatal("enabled without a destination")
	}
}

func TestServiceRunJoinsCancelledTransport(t *testing.T) {
	project, directory := t.TempDir(), t.TempDir()
	s, err := NewService(project, directory, Config{Enabled: true}, Destinations{WebhookURL: "http://localhost:8765"})
	if err != nil {
		t.Fatal(err)
	}
	recordBlock(t, directory, project, "session", "command", "critical", time.Now().UTC())
	entered := make(chan struct{})
	s.Routes = []Route{{ID: "blocking-test", Send: func(ctx context.Context, _ Alert) error { close(entered); <-ctx.Done(); return ctx.Err() }}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx, time.Hour, nil) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("service did not catch up immediately")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("service did not join its transport")
	}
}
