package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/charmbracelet/log"
)

// WebhookTimeout is the maximum time to wait for a webhook request.
const WebhookTimeout = 10 * time.Second

// WebhookEvent represents the type of webhook event.
type WebhookEvent string

const (
	// WebhookEventCriticalPending is sent when a CRITICAL request is pending.
	WebhookEventCriticalPending WebhookEvent = "critical_request_pending"
	// WebhookEventDangerousPending is sent when a DANGEROUS request is pending.
	WebhookEventDangerousPending WebhookEvent = "dangerous_request_pending"
	// WebhookEventRequestTimeout is sent when a request times out.
	WebhookEventRequestTimeout WebhookEvent = "request_timeout"
	// WebhookEventRequestEscalated is sent when a request is escalated.
	WebhookEventRequestEscalated WebhookEvent = "request_escalated"
)

// WebhookPayload is the JSON payload sent to webhook URLs.
type WebhookPayload struct {
	Event     WebhookEvent `json:"event"`
	RequestID string       `json:"request_id"`
	Command   string       `json:"command"`
	Tier      string       `json:"tier"`
	Requestor string       `json:"requestor"`
	Timestamp string       `json:"timestamp"`
	Project   string       `json:"project,omitempty"`
}

// WebhookNotifier handles webhook notifications.
type WebhookNotifier interface {
	Send(ctx context.Context, url string, payload WebhookPayload) error
}

type DesktopNotifier interface {
	Notify(title, message string) error
}

type DesktopNotifierFunc func(title, message string) error

func (f DesktopNotifierFunc) Notify(title, message string) error {
	return f(title, message)
}

type NotificationManager struct {
	projectPath string
	cfg         config.NotificationsConfig
	logger      *log.Logger
	notifier    DesktopNotifier
	webhook     WebhookNotifier
	now         func() time.Time

	mu       sync.Mutex
	notified map[string]time.Time
}

// DefaultWebhookNotifier is the default implementation of WebhookNotifier.
type DefaultWebhookNotifier struct {
	client *http.Client
}

// NewDefaultWebhookNotifier creates a new default webhook notifier with timeout.
func NewDefaultWebhookNotifier() *DefaultWebhookNotifier {
	return &DefaultWebhookNotifier{
		client: &http.Client{
			Timeout: WebhookTimeout,
		},
	}
}

// Send sends a webhook notification to the specified URL.
func (w *DefaultWebhookNotifier) Send(ctx context.Context, url string, payload WebhookPayload) error {
	if url == "" {
		return nil
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling webhook payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "SLB-Webhook/1.0")

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("sending webhook: %w", err)
	}
	defer resp.Body.Close()

	// Accept 2xx status codes
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}

	return nil
}

func NewNotificationManager(projectPath string, cfg config.NotificationsConfig, logger *log.Logger, notifier DesktopNotifier) *NotificationManager {
	if logger == nil {
		logger = log.Default()
	}
	if notifier == nil {
		notifier = DesktopNotifierFunc(SendDesktopNotification)
	}
	if cfg.DesktopDelaySecs < 0 {
		cfg.DesktopDelaySecs = 0
	}

	// Initialize webhook notifier if URL is configured
	var webhook WebhookNotifier
	if cfg.WebhookURL != "" {
		webhook = NewDefaultWebhookNotifier()
	}

	return &NotificationManager{
		projectPath: projectPath,
		cfg:         cfg,
		logger:      logger,
		notifier:    notifier,
		webhook:     webhook,
		now:         time.Now,
		notified:    make(map[string]time.Time),
	}
}

// WithWebhook sets a custom webhook notifier (for testing).
func (m *NotificationManager) WithWebhook(w WebhookNotifier) *NotificationManager {
	m.webhook = w
	return m
}

func (m *NotificationManager) Run(ctx context.Context, interval time.Duration) {
	if m == nil {
		return
	}
	if interval <= 0 {
		interval = 10 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = m.Check(ctx)
		}
	}
}

// Check scans for notable events and sends notifications (desktop and/or webhook).
func (m *NotificationManager) Check(ctx context.Context) error {
	if m == nil {
		return nil
	}

	// Check if there's anything to do
	hasDesktop := m.cfg.DesktopEnabled
	hasWebhook := m.webhook != nil && m.cfg.WebhookURL != ""
	if !hasDesktop && !hasWebhook {
		return nil
	}

	if strings.TrimSpace(m.projectPath) == "" {
		return nil
	}

	dbPath := filepath.Join(m.projectPath, ".slb", "state.db")
	dbConn, err := db.OpenWithOptions(dbPath, db.OpenOptions{
		CreateIfNotExists: false,
		InitSchema:        false,
		ReadOnly:          true,
	})
	if err != nil {
		// Treat missing DB as no-op (daemon should not crash).
		return nil
	}
	defer dbConn.Close()

	now := m.now().UTC()
	delay := time.Duration(m.cfg.DesktopDelaySecs) * time.Second

	pending, err := dbConn.ListPendingRequests(m.projectPath)
	if err != nil {
		return nil
	}

	for _, req := range pending {
		if req == nil {
			continue
		}

		// Only notify for CRITICAL and DANGEROUS tiers
		if req.RiskTier != db.RiskTierCritical && req.RiskTier != db.RiskTierDangerous {
			continue
		}

		// Check if enough time has passed since creation
		if now.Sub(req.CreatedAt) < delay {
			continue
		}

		// Determine notification key based on tier
		var notifyKey string
		var webhookEvent WebhookEvent
		switch req.RiskTier {
		case db.RiskTierCritical:
			notifyKey = "critical_pending:" + req.ID
			webhookEvent = WebhookEventCriticalPending
		case db.RiskTierDangerous:
			notifyKey = "dangerous_pending:" + req.ID
			webhookEvent = WebhookEventDangerousPending
		default:
			continue
		}

		// Skip if already notified
		if !m.markOnce(notifyKey, now) {
			continue
		}

		cmd := req.Command.DisplayRedacted
		if cmd == "" {
			cmd = req.Command.Raw
		}
		cmd = strings.TrimSpace(cmd)
		if len(cmd) > 140 {
			cmd = cmd[:140] + "…"
		}

		// Send desktop notification (CRITICAL only)
		if hasDesktop && req.RiskTier == db.RiskTierCritical {
			title := "SLB: CRITICAL request pending"
			message := fmt.Sprintf("%s\nRequestor: %s\nID: %s", cmd, req.RequestorAgent, shortID(req.ID))

			if err := m.notifier.Notify(title, message); err != nil {
				m.logger.Warn("desktop notification failed", "error", err)
			}
		}

		// Send webhook notification
		if hasWebhook {
			payload := WebhookPayload{
				Event:     webhookEvent,
				RequestID: req.ID,
				Command:   cmd,
				Tier:      string(req.RiskTier),
				Requestor: req.RequestorAgent,
				Timestamp: now.Format(time.RFC3339),
				Project:   m.projectPath,
			}

			// Use a timeout context for webhook calls
			webhookCtx, cancel := context.WithTimeout(ctx, WebhookTimeout)
			if err := m.webhook.Send(webhookCtx, m.cfg.WebhookURL, payload); err != nil {
				m.logger.Warn("webhook notification failed",
					"error", err,
					"request_id", req.ID,
					"event", webhookEvent)
			} else {
				m.logger.Debug("webhook notification sent",
					"request_id", req.ID,
					"event", webhookEvent)
			}
			cancel()
		}
	}

	return nil
}

// SendWebhook sends a webhook notification for a specific event (can be called directly).
func (m *NotificationManager) SendWebhook(ctx context.Context, event WebhookEvent, req *db.Request) error {
	if m == nil || m.webhook == nil || m.cfg.WebhookURL == "" {
		return nil
	}

	cmd := req.Command.DisplayRedacted
	if cmd == "" {
		cmd = req.Command.Raw
	}
	cmd = strings.TrimSpace(cmd)
	if len(cmd) > 140 {
		cmd = cmd[:140] + "…"
	}

	payload := WebhookPayload{
		Event:     event,
		RequestID: req.ID,
		Command:   cmd,
		Tier:      string(req.RiskTier),
		Requestor: req.RequestorAgent,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Project:   m.projectPath,
	}

	webhookCtx, cancel := context.WithTimeout(ctx, WebhookTimeout)
	defer cancel()

	if err := m.webhook.Send(webhookCtx, m.cfg.WebhookURL, payload); err != nil {
		m.logger.Warn("webhook notification failed",
			"error", err,
			"request_id", req.ID,
			"event", event)
		return err
	}

	m.logger.Debug("webhook notification sent",
		"request_id", req.ID,
		"event", event)
	return nil
}

func (m *NotificationManager) markOnce(key string, at time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.notified[key]; ok {
		return false
	}
	m.notified[key] = at
	return true
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// SendDesktopNotification sends a best-effort desktop notification on the current platform.
func SendDesktopNotification(title, message string) error {
	title = strings.TrimSpace(title)
	message = strings.TrimSpace(message)
	if title == "" {
		title = "SLB"
	}
	if message == "" {
		return fmt.Errorf("message is required")
	}

	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("osascript"); err != nil {
			return fmt.Errorf("osascript not found")
		}
		script := fmt.Sprintf(
			`display notification "%s" with title "%s"`,
			escapeAppleScript(message),
			escapeAppleScript(title),
		)
		return runNoOutput("osascript", "-e", script)
	case "linux":
		if _, err := exec.LookPath("notify-send"); err != nil {
			return fmt.Errorf("notify-send not found")
		}
		return runNoOutput("notify-send", title, message)
	case "windows":
		// Graceful fallback: don't hard-fail the daemon on unsupported notification setups.
		return errors.New("desktop notifications not implemented on windows")
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}

func runNoOutput(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s failed: %w (%s)", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func escapeAppleScript(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\r")
	return s
}
