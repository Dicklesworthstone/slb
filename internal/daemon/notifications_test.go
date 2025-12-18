package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/db"
)

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
	}, nil, DesktopNotifierFunc(func(title, message string) error {
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
	}, nil, DesktopNotifierFunc(func(title, message string) error {
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
	}, nil, DesktopNotifierFunc(func(title, message string) error {
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
