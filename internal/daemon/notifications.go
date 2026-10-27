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
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/charmbracelet/log"
)

// WebhookTimeout is the maximum time to wait for a webhook request.
const WebhookTimeout = 10 * time.Second

const (
	pendingAlertPassTimeout  = 15 * time.Second
	pendingAlertRetryBase    = 15 * time.Second
	pendingAlertRetryMaximum = 5 * time.Minute
	pendingAlertAttemptLimit = 60
	desktopNotificationLimit = 5 * time.Second
	desktopNotificationBytes = 4096
)

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
	Event          WebhookEvent      `json:"event"`
	RequestID      string            `json:"request_id"`
	Command        string            `json:"command"`
	Tier           string            `json:"tier"`
	Requestor      string            `json:"requestor"`
	Timestamp      string            `json:"timestamp"`
	Project        string            `json:"project,omitempty"`
	ApprovalPolicy db.ApprovalPolicy `json:"approval_policy"`
	ReviewNotice   string            `json:"review_notice"`
}

// WebhookNotifier handles webhook notifications.
type WebhookNotifier interface {
	Send(ctx context.Context, url string, payload WebhookPayload) error
}

type DesktopNotifier interface {
	// Notify must honor cancellation and return only after its work stops.
	Notify(ctx context.Context, title, message string) error
}

type DesktopNotifierFunc func(context.Context, string, string) error

func (f DesktopNotifierFunc) Notify(ctx context.Context, title, message string) error {
	return f(ctx, title, message)
}

type pendingAlertKey struct {
	requestID   string
	destination string
	event       WebhookEvent
}

type pendingAlertDelivery struct {
	sent     bool
	failures int
	retryAt  time.Time
}

type NotificationManager struct {
	projectPath string
	cfg         config.NotificationsConfig
	logger      *log.Logger
	notifier    DesktopNotifier
	webhook     WebhookNotifier
	now         func() time.Time

	// Serialize scans and delivery without making a cancelled caller wait for
	// another pass. State is bounded by current pending requests and one minute
	// of attempted sends; it never changes request or review records.
	gate       chan struct{}
	deliveries map[pendingAlertKey]pendingAlertDelivery
	attempts   []time.Time
	next       int
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
		gate:        make(chan struct{}, 1),
		deliveries:  make(map[pendingAlertKey]pendingAlertDelivery),
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
	select {
	case m.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-m.gate }()
	ctx, cancel := context.WithTimeout(ctx, pendingAlertPassTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
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

	var jobs []pendingAlertKey
	needed := make(map[pendingAlertKey]bool)
	for _, req := range pending {
		if req == nil || now.Sub(req.CreatedAt) < delay {
			continue
		}
		event := WebhookEventDangerousPending
		if req.RiskTier == db.RiskTierCritical {
			event = WebhookEventCriticalPending
		} else if req.RiskTier != db.RiskTierDangerous {
			continue
		}
		for _, destination := range []string{"desktop", "webhook"} {
			if destination == "desktop" && (!hasDesktop || req.RiskTier != db.RiskTierCritical) || destination == "webhook" && !hasWebhook {
				continue
			}
			key := pendingAlertKey{requestID: req.ID, destination: destination, event: event}
			jobs = append(jobs, key)
			needed[key] = true
		}
	}
	for key := range m.deliveries {
		if !needed[key] {
			delete(m.deliveries, key)
		}
	}
	if len(jobs) == 0 {
		return nil
	}
	start := m.next % len(jobs)
	for offset := range jobs {
		if err := ctx.Err(); err != nil {
			return err
		}
		index := (start + offset) % len(jobs)
		key := jobs[index]
		delivery := m.deliveries[key]
		now = m.now().UTC()
		if delivery.sent || now.Before(delivery.retryAt) {
			continue
		}
		attempts := m.attempts[:0]
		for _, at := range m.attempts {
			if at.After(now.Add(-time.Minute)) {
				attempts = append(attempts, at)
			}
		}
		m.attempts = attempts
		if len(m.attempts) >= pendingAlertAttemptLimit {
			m.next = index
			break
		}
		// Re-read before EACH destination. A decision committed while the
		// other destination was slow must end pending-alert retries.
		req, err := dbConn.GetRequest(key.requestID)
		if err != nil {
			continue
		}
		if req.Status != db.StatusPending || req.ProjectPath != m.projectPath ||
			key.event == WebhookEventCriticalPending && req.RiskTier != db.RiskTierCritical ||
			key.event == WebhookEventDangerousPending && req.RiskTier != db.RiskTierDangerous {
			delete(m.deliveries, key)
			continue
		}
		m.next = (index + 1) % len(jobs)
		m.attempts = append(m.attempts, now)
		delivery.failures = min(delivery.failures+1, 6)
		delivery.retryAt = now.Add(min(pendingAlertRetryMaximum, pendingAlertRetryBase*time.Duration(1<<uint(delivery.failures-1))))
		m.deliveries[key] = delivery
		attempt, stop := context.WithTimeout(ctx, WebhookTimeout)
		if key.destination == "desktop" {
			command := req.Command.DisplayRedacted
			if command == "" {
				command = req.Command.Raw
			}
			command = strings.TrimSpace(command)
			if len(command) > 140 {
				command = command[:140] + "…"
			}
			message := fmt.Sprintf("%s\nRequestor: %s\nID: %s\n%s", command, req.RequestorAgent, shortID(req.ID), req.ApprovalPolicy.Label())
			err = m.notifier.Notify(attempt, "SLB: CRITICAL request pending", message)
			if err != nil && ctx.Err() == nil {
				m.logger.Warn("desktop notification failed; retry scheduled", "error", err, "request_id", req.ID)
			}
		} else {
			err = m.SendWebhook(attempt, key.event, req)
		}
		stop()
		if err == nil {
			m.deliveries[key] = pendingAlertDelivery{sent: true}
		}
		if err := ctx.Err(); err != nil {
			return err
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
		Event:          event,
		RequestID:      req.ID,
		Command:        cmd,
		Tier:           string(req.RiskTier),
		Requestor:      req.RequestorAgent,
		Timestamp:      time.Now().UTC().Format(time.RFC3339),
		Project:        m.projectPath,
		ApprovalPolicy: req.ApprovalPolicy.Normalized(), ReviewNotice: req.ApprovalPolicy.Label(),
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

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// SendDesktopNotification sends a best-effort desktop notification on the current platform.
func SendDesktopNotification(ctx context.Context, title, message string) error {
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
		return runNoOutput(ctx, "osascript", "-e", script)
	case "linux":
		if _, err := exec.LookPath("notify-send"); err != nil {
			return fmt.Errorf("notify-send not found")
		}
		return runNoOutput(ctx, "notify-send", title, message)
	case "windows":
		// Graceful fallback: don't hard-fail the daemon on unsupported notification setups.
		return errors.New("desktop notifications not implemented on windows")
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}

func runNoOutput(parent context.Context, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(parent, desktopNotificationLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = os.Environ()
	output := &notificationOutput{}
	cmd.Stdout, cmd.Stderr = output, output
	cmd.WaitDelay = time.Second
	stopGroup := core.ConfigureNoninteractiveCommandCancellation(cmd)
	err := cmd.Run()
	if err != nil {
		if stopGroup != nil {
			if stopErr := stopGroup(); stopErr != nil && !errors.Is(stopErr, os.ErrProcessDone) {
				err = errors.Join(err, stopErr)
			}
		}
		if ctx.Err() != nil {
			err = errors.Join(err, ctx.Err())
		}
		return fmt.Errorf("%s failed: %w (%s)", name, err, strings.TrimSpace(output.String()))
	}
	return nil
}

type notificationOutput struct {
	buffer    bytes.Buffer
	truncated bool
}

func (o *notificationOutput) Write(data []byte) (int, error) {
	n := min(len(data), desktopNotificationBytes-o.buffer.Len())
	_, _ = o.buffer.Write(data[:n])
	o.truncated = o.truncated || n < len(data)
	return len(data), nil
}

func (o *notificationOutput) String() string {
	if o.truncated {
		return o.buffer.String() + " [output truncated]"
	}
	return o.buffer.String()
}

func escapeAppleScript(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\r")
	return s
}
