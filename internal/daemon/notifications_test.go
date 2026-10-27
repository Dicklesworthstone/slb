package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/charmbracelet/log"
)

type pendingWebhookFunc func(context.Context, string, WebhookPayload) error

func (f pendingWebhookFunc) Send(ctx context.Context, endpoint string, payload WebhookPayload) error {
	return f(ctx, endpoint, payload)
}

func pendingNotificationFixture(t *testing.T) (*db.DB, *db.Request) {
	t.Helper()
	project := t.TempDir()
	database, err := db.OpenAndMigrate(filepath.Join(project, ".slb", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	session := &db.Session{ProjectPath: project, AgentName: "requester", Program: "test", Model: "model"}
	if err := database.CreateSession(session); err != nil {
		t.Fatal(err)
	}
	request := &db.Request{ProjectPath: project, Status: db.StatusPending,
		Command:            db.CommandSpec{Raw: "echo pending", Cwd: project, Shell: true},
		RequestorSessionID: session.ID, RequestorAgent: session.AgentName, RequestorModel: session.Model,
		RiskTier: db.RiskTierCritical, MinApprovals: 2, ApprovalPolicy: db.ApprovalPolicySameHost,
		Justification: db.Justification{Reason: "notification test"}, CreatedAt: time.Now().UTC().Add(-time.Minute)}
	if err := database.CreateRequest(request); err != nil {
		t.Fatal(err)
	}
	return database, request
}

func TestPendingNotificationsRetryDestinationsIndependently(t *testing.T) {
	for _, failed := range []string{"desktop", "webhook", "both"} {
		t.Run(failed, func(t *testing.T) {
			database, request := pendingNotificationFixture(t)
			calls := map[string]int{}
			attempt := func(destination string) error {
				calls[destination]++
				if calls[destination] == 1 && (failed == destination || failed == "both") {
					return errors.New("temporary destination failure")
				}
				return nil
			}
			manager := NewNotificationManager(request.ProjectPath, config.NotificationsConfig{
				DesktopEnabled: true, WebhookURL: "http://unused.invalid"}, log.New(io.Discard),
				DesktopNotifierFunc(func(_ context.Context, title, message string) error {
					if !strings.Contains(message, db.ApprovalPolicySameHost.Label()) {
						t.Fatal("desktop alert omitted same-host assurance")
					}
					return attempt("desktop")
				})).WithWebhook(pendingWebhookFunc(func(_ context.Context, _ string, payload WebhookPayload) error {
				if payload.RequestID != request.ID || payload.ApprovalPolicy != db.ApprovalPolicySameHost || payload.ReviewNotice != db.ApprovalPolicySameHost.Label() {
					t.Fatalf("webhook changed review provenance: %+v", payload)
				}
				return attempt("webhook")
			}))
			now := time.Now().UTC()
			manager.now = func() time.Time { return now }
			check := func() {
				t.Helper()
				if err := manager.Check(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			check()
			check()
			now = now.Add(14 * time.Second)
			check()
			if calls["desktop"] != 1 || calls["webhook"] != 1 {
				t.Fatalf("a failed send retried without backoff: %v", calls)
			}
			now = now.Add(time.Second)
			check()
			now = now.Add(time.Hour)
			check()
			for _, destination := range []string{"desktop", "webhook"} {
				want := 1
				if failed == destination || failed == "both" {
					want = 2
				}
				if calls[destination] != want {
					t.Fatalf("destination %s attempts=%d, want %d", destination, calls[destination], want)
				}
			}
			stored, err := database.GetRequest(request.ID)
			if err != nil || stored.Status != db.StatusPending || stored.Command.Hash != request.Command.Hash || stored.ApprovalPolicy != db.ApprovalPolicySameHost {
				t.Fatalf("notification changed request authority: %+v %v", stored, err)
			}
			var reviews int
			if err := database.QueryRow("SELECT COUNT(*) FROM reviews WHERE request_id = ?", request.ID).Scan(&reviews); err != nil || reviews != 0 {
				t.Fatalf("notification manufactured review evidence: %d %v", reviews, err)
			}
		})
	}
}

func TestPendingNotificationsBackoffCapsAtFiveMinutes(t *testing.T) {
	_, request := pendingNotificationFixture(t)
	calls := 0
	manager := NewNotificationManager(request.ProjectPath, config.NotificationsConfig{DesktopEnabled: true}, log.New(io.Discard),
		DesktopNotifierFunc(func(context.Context, string, string) error {
			calls++
			return errors.New("offline")
		}))
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	if err := manager.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i, delay := range []time.Duration{15, 30, 60, 120, 240, 300, 300} {
		now = now.Add(delay*time.Second - time.Nanosecond)
		if err := manager.Check(context.Background()); err != nil || calls != i+1 {
			t.Fatalf("retry occurred early: attempts=%d err=%v", calls, err)
		}
		now = now.Add(time.Nanosecond)
		if err := manager.Check(context.Background()); err != nil || calls != i+2 {
			t.Fatalf("retry did not recover after bounded backoff: attempts=%d err=%v", calls, err)
		}
	}
}

func TestPendingNotificationsShareAttemptBudgetAndContinue(t *testing.T) {
	database, request := pendingNotificationFixture(t)
	ids := map[string]bool{request.ID: true}
	for range 60 {
		extra := *request
		extra.ID = ""
		if err := database.CreateRequest(&extra); err != nil {
			t.Fatal(err)
		}
		ids[extra.ID] = true
	}
	calls := 0
	seen := make(map[string]int)
	manager := NewNotificationManager(request.ProjectPath, config.NotificationsConfig{WebhookURL: "http://unused.invalid"}, log.New(io.Discard), nil).
		WithWebhook(pendingWebhookFunc(func(_ context.Context, _ string, payload WebhookPayload) error {
			calls++
			seen[payload.RequestID]++
			return errors.New("offline")
		}))
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	if err := manager.Check(context.Background()); err != nil || calls != 60 {
		t.Fatalf("initial attempt budget: calls=%d err=%v", calls, err)
	}
	missing := ""
	for id := range ids {
		if seen[id] == 0 {
			missing = id
		}
	}
	if missing == "" {
		t.Fatal("fixture did not exceed the shared attempt budget")
	}
	now = now.Add(59 * time.Second)
	if err := manager.Check(context.Background()); err != nil || calls != 60 {
		t.Fatalf("failures bypassed the rolling attempt budget: calls=%d err=%v", calls, err)
	}
	now = now.Add(time.Second)
	if err := manager.Check(context.Background()); err != nil || calls != 120 || seen[missing] != 1 {
		t.Fatalf("deferred request starved or budget exceeded: calls=%d deferred=%d err=%v", calls, seen[missing], err)
	}
}

func TestPendingNotificationsStopAfterConcurrentDecision(t *testing.T) {
	database, request := pendingNotificationFixture(t)
	desktopCalls, webhookCalls := 0, 0
	manager := NewNotificationManager(request.ProjectPath, config.NotificationsConfig{
		DesktopEnabled: true, WebhookURL: "http://unused.invalid"}, log.New(io.Discard),
		DesktopNotifierFunc(func(context.Context, string, string) error {
			desktopCalls++
			if _, err := database.CancelRequest(request.ID, request.RequestorSessionID); err != nil {
				t.Fatal(err)
			}
			return errors.New("desktop unavailable")
		})).WithWebhook(pendingWebhookFunc(func(context.Context, string, WebhookPayload) error {
		webhookCalls++
		return nil
	}))
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	if err := manager.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if err := manager.Check(context.Background()); err != nil || desktopCalls != 1 || webhookCalls != 0 {
		t.Fatalf("resolved request still generated pending alerts: desktop=%d webhook=%d err=%v", desktopCalls, webhookCalls, err)
	}
}

func TestPendingNotificationsCancelInFlightAndQueuedChecks(t *testing.T) {
	_, request := pendingNotificationFixture(t)
	entered := make(chan struct{})
	manager := NewNotificationManager(request.ProjectPath, config.NotificationsConfig{DesktopEnabled: true}, log.New(io.Discard),
		DesktopNotifierFunc(func(ctx context.Context, _, _ string) error {
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- manager.Check(ctx) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("desktop delivery did not start")
	}
	queued, stop := context.WithCancel(context.Background())
	stop()
	if err := manager.Check(queued); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled queued check did not stop: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("in-flight delivery lost cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight delivery prevented shutdown")
	}
}

func TestNotificationManagerStopsHangingDesktopProcessGroup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux process-state inspection")
	}
	_, request := pendingNotificationFixture(t)
	directory := t.TempDir()
	pidsPath := filepath.Join(directory, "pids")
	t.Setenv("SLB_NOTIFICATION_TEST_PIDS", pidsPath)
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	script := "#!/bin/sh\nsleep 10 &\nchild=$!\nprintf '%s %s\\n' \"$$\" \"$child\" > \"$SLB_NOTIFICATION_TEST_PIDS\"\nwait\n"
	if err := os.WriteFile(filepath.Join(directory, "notify-send"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	manager := NewNotificationManager(request.ProjectPath, config.NotificationsConfig{DesktopEnabled: true}, log.New(io.Discard), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { manager.Run(ctx, time.Millisecond); close(done) }()
	var pids []string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(pidsPath)
		pids = strings.Fields(string(data))
		if len(pids) == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(pids) != 2 {
		t.Fatal("fake desktop helper did not start its ordinary child")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("hanging desktop helper prevented worker shutdown")
	}
	for _, text := range pids {
		pid, err := strconv.Atoi(text)
		if err != nil || pid <= 1 {
			t.Fatalf("invalid helper PID %q", text)
		}
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		// A killed orphan may await the host's reaper; it cannot keep running.
		end := strings.LastIndexByte(string(data), ')')
		state := strings.Fields(string(data)[end+1:])
		if end < 0 || len(state) == 0 || state[0] != "Z" {
			t.Fatalf("ordinary desktop descendant survived cancellation: %s", data)
		}
	}
}

func TestNotificationManagerCriticalPendingDebounced(t *testing.T) {
	project := t.TempDir()

	dbConn, err := db.OpenProjectDB(project)
	if err != nil {
		t.Fatalf("open project db: %v", err)
	}
	t.Cleanup(func() { _ = dbConn.Close() })

	// Insert session to satisfy FK constraints on requests.
	if err := dbConn.CreateSession(&db.Session{
		ID:          "s1",
		AgentName:   "AgentA",
		Program:     "test",
		Model:       "model",
		ProjectPath: project,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	req := &db.Request{
		ProjectPath: project,
		Command: db.CommandSpec{
			Raw: "rm -rf ./build",
			Cwd: project,
		},
		RiskTier:              db.RiskTierCritical,
		RequestorSessionID:    "s1",
		RequestorAgent:        "AgentA",
		RequestorModel:        "model",
		Justification:         db.Justification{Reason: "cleanup"},
		MinApprovals:          2,
		RequireDifferentModel: false,
	}
	if err := dbConn.CreateRequest(req); err != nil {
		t.Fatalf("create request: %v", err)
	}

	calls := 0
	manager := NewNotificationManager(project, config.NotificationsConfig{
		DesktopEnabled:   true,
		DesktopDelaySecs: 0,
	}, nil, DesktopNotifierFunc(func(_ context.Context, title, message string) error {
		calls++
		return nil
	}))

	if err := manager.Check(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}

	if err := manager.Check(context.Background()); err != nil {
		t.Fatalf("check2: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected debounced call count 1, got %d", calls)
	}
}

func TestNotificationManagerDisabled(t *testing.T) {
	project := t.TempDir()
	manager := NewNotificationManager(project, config.NotificationsConfig{
		DesktopEnabled:   false,
		DesktopDelaySecs: 0,
	}, nil, DesktopNotifierFunc(func(_ context.Context, title, message string) error {
		t.Fatalf("should not be called")
		return nil
	}))

	if err := manager.Check(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}
}

// ============== DefaultWebhookNotifier Tests ==============

func TestNewDefaultWebhookNotifier(t *testing.T) {
	notifier := NewDefaultWebhookNotifier()
	if notifier == nil {
		t.Fatal("expected non-nil notifier")
	}
	if notifier.client == nil {
		t.Fatal("expected non-nil http client")
	}
	if notifier.client.Timeout != WebhookTimeout {
		t.Errorf("expected timeout %v, got %v", WebhookTimeout, notifier.client.Timeout)
	}
}

func TestDefaultWebhookNotifierSendEmptyURL(t *testing.T) {
	notifier := NewDefaultWebhookNotifier()
	err := notifier.Send(context.Background(), "", WebhookPayload{
		Event:     WebhookEventCriticalPending,
		RequestID: "test-123",
	})
	if err != nil {
		t.Errorf("empty URL should return nil, got %v", err)
	}
}

func TestDefaultWebhookNotifierSendSuccess(t *testing.T) {
	var receivedPayload WebhookPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected Content-Type application/json, got %s", r.Header.Get("Content-Type"))
		}
		if r.Header.Get("User-Agent") != "SLB-Webhook/1.0" {
			t.Errorf("expected User-Agent SLB-Webhook/1.0, got %s", r.Header.Get("User-Agent"))
		}
		if err := json.NewDecoder(r.Body).Decode(&receivedPayload); err != nil {
			t.Errorf("failed to decode payload: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	notifier := NewDefaultWebhookNotifier()
	payload := WebhookPayload{
		Event:     WebhookEventCriticalPending,
		RequestID: "test-123",
		Command:   "rm -rf /",
		Tier:      "CRITICAL",
		Requestor: "TestAgent",
		Timestamp: time.Now().Format(time.RFC3339),
	}

	err := notifier.Send(context.Background(), server.URL, payload)
	if err != nil {
		t.Errorf("expected success, got %v", err)
	}

	if receivedPayload.RequestID != "test-123" {
		t.Errorf("expected request ID test-123, got %s", receivedPayload.RequestID)
	}
	if receivedPayload.Event != WebhookEventCriticalPending {
		t.Errorf("expected event critical_request_pending, got %s", receivedPayload.Event)
	}
}

func TestDefaultWebhookNotifierSendErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	notifier := NewDefaultWebhookNotifier()
	err := notifier.Send(context.Background(), server.URL, WebhookPayload{
		Event:     WebhookEventCriticalPending,
		RequestID: "test-123",
	})

	if err == nil {
		t.Error("expected error for 500 status")
	}
}

func TestDefaultWebhookNotifierSendNetworkError(t *testing.T) {
	notifier := NewDefaultWebhookNotifier()
	// Use an invalid URL that will fail to connect
	err := notifier.Send(context.Background(), "http://localhost:99999/webhook", WebhookPayload{
		Event:     WebhookEventCriticalPending,
		RequestID: "test-123",
	})

	if err == nil {
		t.Error("expected error for network failure")
	}
}

// ============== WithWebhook Tests ==============

func TestWithWebhook(t *testing.T) {
	project := t.TempDir()
	manager := NewNotificationManager(project, config.NotificationsConfig{
		DesktopEnabled: false,
	}, nil, nil)

	// Initially no webhook
	if manager.webhook != nil {
		t.Error("expected no webhook initially when URL is empty")
	}

	// Set custom webhook
	customWebhook := NewDefaultWebhookNotifier()
	result := manager.WithWebhook(customWebhook)

	if result != manager {
		t.Error("WithWebhook should return the same manager")
	}
	if manager.webhook != customWebhook {
		t.Error("webhook should be set to custom webhook")
	}
}

// ============== SendWebhook Tests ==============

func TestSendWebhookNilManager(t *testing.T) {
	var m *NotificationManager
	err := m.SendWebhook(context.Background(), WebhookEventCriticalPending, &db.Request{})
	if err != nil {
		t.Errorf("nil manager should return nil, got %v", err)
	}
}

func TestSendWebhookNoWebhook(t *testing.T) {
	project := t.TempDir()
	manager := NewNotificationManager(project, config.NotificationsConfig{
		DesktopEnabled: false,
	}, nil, nil)

	err := manager.SendWebhook(context.Background(), WebhookEventCriticalPending, &db.Request{
		ID: "test-123",
	})
	if err != nil {
		t.Errorf("no webhook should return nil, got %v", err)
	}
}

func TestSendWebhookSuccess(t *testing.T) {
	var receivedPayload WebhookPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&receivedPayload); err != nil {
			t.Errorf("failed to decode payload: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	project := t.TempDir()
	manager := NewNotificationManager(project, config.NotificationsConfig{
		DesktopEnabled: false,
		WebhookURL:     server.URL,
	}, nil, nil)

	req := &db.Request{
		ID:             "req-123",
		Command:        db.CommandSpec{Raw: "rm -rf ./build"},
		RiskTier:       db.RiskTierCritical,
		RequestorAgent: "TestAgent",
	}

	err := manager.SendWebhook(context.Background(), WebhookEventRequestTimeout, req)
	if err != nil {
		t.Errorf("expected success, got %v", err)
	}

	if receivedPayload.Event != WebhookEventRequestTimeout {
		t.Errorf("expected event request_timeout, got %s", receivedPayload.Event)
	}
	if receivedPayload.RequestID != "req-123" {
		t.Errorf("expected request ID req-123, got %s", receivedPayload.RequestID)
	}
}

func TestSendWebhookWithLongCommand(t *testing.T) {
	var receivedPayload WebhookPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&receivedPayload); err != nil {
			t.Errorf("failed to decode payload: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	project := t.TempDir()
	manager := NewNotificationManager(project, config.NotificationsConfig{
		DesktopEnabled: false,
		WebhookURL:     server.URL,
	}, nil, nil)

	// Create a command longer than 140 characters
	longCmd := "rm -rf " + string(make([]byte, 200))
	req := &db.Request{
		ID:             "req-123",
		Command:        db.CommandSpec{Raw: longCmd},
		RiskTier:       db.RiskTierCritical,
		RequestorAgent: "TestAgent",
	}

	err := manager.SendWebhook(context.Background(), WebhookEventCriticalPending, req)
	if err != nil {
		t.Errorf("expected success, got %v", err)
	}

	// Command should be truncated to 140 chars + ellipsis
	if len(receivedPayload.Command) > 145 {
		t.Errorf("expected command to be truncated, got length %d", len(receivedPayload.Command))
	}
}

func TestSendWebhookWithDisplayRedacted(t *testing.T) {
	var receivedPayload WebhookPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&receivedPayload); err != nil {
			t.Errorf("failed to decode payload: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	project := t.TempDir()
	manager := NewNotificationManager(project, config.NotificationsConfig{
		DesktopEnabled: false,
		WebhookURL:     server.URL,
	}, nil, nil)

	req := &db.Request{
		ID:             "req-123",
		Command:        db.CommandSpec{Raw: "secret command", DisplayRedacted: "redacted command"},
		RiskTier:       db.RiskTierDangerous,
		RequestorAgent: "TestAgent",
	}

	err := manager.SendWebhook(context.Background(), WebhookEventDangerousPending, req)
	if err != nil {
		t.Errorf("expected success, got %v", err)
	}

	// Should use DisplayRedacted if available
	if receivedPayload.Command != "redacted command" {
		t.Errorf("expected redacted command, got %s", receivedPayload.Command)
	}
}

func TestSendWebhookError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	project := t.TempDir()
	manager := NewNotificationManager(project, config.NotificationsConfig{
		DesktopEnabled: false,
		WebhookURL:     server.URL,
	}, nil, nil)

	req := &db.Request{
		ID:             "req-123",
		Command:        db.CommandSpec{Raw: "test command"},
		RiskTier:       db.RiskTierCritical,
		RequestorAgent: "TestAgent",
	}

	err := manager.SendWebhook(context.Background(), WebhookEventRequestEscalated, req)
	if err == nil {
		t.Error("expected error for webhook failure")
	}
}

// ============== shortID Tests ==============

func TestShortID(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", ""},
		{"abc", "abc"},
		{"12345678", "12345678"},
		{"123456789", "12345678"},
		{"abcdefghijklmnop", "abcdefgh"},
	}

	for _, tc := range tests {
		result := shortID(tc.input)
		if result != tc.expected {
			t.Errorf("shortID(%q) = %q, want %q", tc.input, result, tc.expected)
		}
	}
}

// ============== Check with Webhook Tests ==============

func TestNotificationManagerCheckWithWebhook(t *testing.T) {
	project := t.TempDir()

	dbConn, err := db.OpenProjectDB(project)
	if err != nil {
		t.Fatalf("open project db: %v", err)
	}
	t.Cleanup(func() { _ = dbConn.Close() })

	// Insert session
	if err := dbConn.CreateSession(&db.Session{
		ID:          "s1",
		AgentName:   "AgentA",
		Program:     "test",
		Model:       "model",
		ProjectPath: project,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Create a DANGEROUS request (webhooks should fire but not desktop)
	req := &db.Request{
		ProjectPath: project,
		Command: db.CommandSpec{
			Raw: "rm -rf ./build",
			Cwd: project,
		},
		RiskTier:              db.RiskTierDangerous,
		RequestorSessionID:    "s1",
		RequestorAgent:        "AgentA",
		RequestorModel:        "model",
		Justification:         db.Justification{Reason: "cleanup"},
		MinApprovals:          1,
		RequireDifferentModel: false,
	}
	if err := dbConn.CreateRequest(req); err != nil {
		t.Fatalf("create request: %v", err)
	}

	webhookCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webhookCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	desktopCalls := 0
	manager := NewNotificationManager(project, config.NotificationsConfig{
		DesktopEnabled:   true,
		DesktopDelaySecs: 0,
		WebhookURL:       server.URL,
	}, nil, DesktopNotifierFunc(func(_ context.Context, title, message string) error {
		desktopCalls++
		return nil
	}))

	if err := manager.Check(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}

	// Webhook should be called for DANGEROUS
	if webhookCalls != 1 {
		t.Errorf("expected 1 webhook call, got %d", webhookCalls)
	}

	// Desktop should NOT be called for DANGEROUS (only CRITICAL)
	if desktopCalls != 0 {
		t.Errorf("expected 0 desktop calls for DANGEROUS, got %d", desktopCalls)
	}
}

// ============== Run Tests ==============

func TestNotificationManagerRunNil(t *testing.T) {
	var m *NotificationManager
	// Should not panic
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	m.Run(ctx, 10*time.Millisecond)
}

func TestNotificationManagerRunCancellation(t *testing.T) {
	project := t.TempDir()
	manager := NewNotificationManager(project, config.NotificationsConfig{
		DesktopEnabled: false,
	}, nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		manager.Run(ctx, 10*time.Millisecond)
		close(done)
	}()

	select {
	case <-done:
		// Good - Run exited
	case <-time.After(500 * time.Millisecond):
		t.Error("Run did not exit after context cancellation")
	}
}

func TestNotificationManagerRunDefaultInterval(t *testing.T) {
	project := t.TempDir()
	manager := NewNotificationManager(project, config.NotificationsConfig{
		DesktopEnabled: false,
	}, nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// Should use default interval when 0 is passed
	manager.Run(ctx, 0)
}

// ============== Check Edge Cases ==============

func TestNotificationManagerCheckNil(t *testing.T) {
	var m *NotificationManager
	err := m.Check(context.Background())
	if err != nil {
		t.Errorf("nil manager Check should return nil, got %v", err)
	}
}

func TestNotificationManagerCheckEmptyProjectPath(t *testing.T) {
	manager := NewNotificationManager("", config.NotificationsConfig{
		DesktopEnabled: true,
	}, nil, nil)

	err := manager.Check(context.Background())
	if err != nil {
		t.Errorf("empty project path Check should return nil, got %v", err)
	}
}

func TestNotificationManagerCheckNonExistentDB(t *testing.T) {
	manager := NewNotificationManager("/nonexistent/path", config.NotificationsConfig{
		DesktopEnabled: true,
	}, nil, nil)

	err := manager.Check(context.Background())
	if err != nil {
		t.Errorf("nonexistent DB Check should return nil, got %v", err)
	}
}

func TestNotificationManagerCheckWithNegativeDelay(t *testing.T) {
	project := t.TempDir()
	manager := NewNotificationManager(project, config.NotificationsConfig{
		DesktopEnabled:   true,
		DesktopDelaySecs: -5, // Should be normalized to 0
	}, nil, nil)

	// Should not panic
	_ = manager.Check(context.Background())
}
