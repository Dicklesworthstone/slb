// Package daemon provides pending-request timer processing.
package daemon

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/charmbracelet/log"
)

type TimeoutAction string

const (
	TimeoutActionEscalate        TimeoutAction = "escalate"
	TimeoutActionAutoReject      TimeoutAction = "auto_reject"
	TimeoutActionAutoApproveWarn TimeoutAction = "auto_approve_warn"
	DefaultCheckInterval                       = 10 * time.Second
)

type TimeoutHandlerConfig struct {
	CheckInterval time.Duration
	Action        TimeoutAction
	DesktopNotify bool
	Logger        *log.Logger
}

func DefaultTimeoutConfig() TimeoutHandlerConfig {
	return TimeoutHandlerConfig{CheckInterval: DefaultCheckInterval, Action: TimeoutActionEscalate, DesktopNotify: true}
}

func TimeoutConfigFromConfig(cfg config.Config) TimeoutHandlerConfig {
	action := TimeoutAction(cfg.General.TimeoutAction)
	switch action {
	case TimeoutActionEscalate, TimeoutActionAutoReject, TimeoutActionAutoApproveWarn:
	default:
		action = TimeoutActionEscalate
	}
	return TimeoutHandlerConfig{CheckInterval: DefaultCheckInterval, Action: action, DesktopNotify: cfg.Notifications.DesktopEnabled}
}

// TimeoutHandler processes both delayed CAUTION approvals and expired
// requests. Timer writes use the same transactional path as CLI waiters.
type TimeoutHandler struct {
	db     *db.DB
	config TimeoutHandlerConfig
	logger *log.Logger

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	done    chan struct{}
}

func NewTimeoutHandler(database *db.DB, cfg TimeoutHandlerConfig) *TimeoutHandler {
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}
	if cfg.CheckInterval <= 0 {
		cfg.CheckInterval = DefaultCheckInterval
	}
	return &TimeoutHandler{db: database, config: cfg, logger: logger}
}

func (h *TimeoutHandler) Start(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running {
		return fmt.Errorf("timeout handler already running")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	h.running, h.cancel, h.done = true, cancel, done
	go h.run(ctx, done)
	h.logger.Info("pending request timer started", "interval", h.config.CheckInterval)
	return nil
}

// Stop waits for the captured generation to finish before returning. A stale
// loop must not keep writing after shutdown or interfere with a restarted loop.
func (h *TimeoutHandler) Stop() {
	h.mu.Lock()
	cancel, done := h.cancel, h.done
	h.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

func (h *TimeoutHandler) IsRunning() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.running
}

func (h *TimeoutHandler) run(ctx context.Context, done chan struct{}) {
	defer func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.done == done {
			h.running, h.cancel = false, nil
		}
		close(done)
	}()
	ticker := time.NewTicker(h.config.CheckInterval)
	defer ticker.Stop()
	h.checkPending(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.checkPending(ctx)
		}
	}
}

func (h *TimeoutHandler) checkAndHandleExpired() { h.checkPending(context.Background()) }

func (h *TimeoutHandler) checkPending(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	pending, err := h.db.ListPendingRequestsAllProjects()
	if err != nil {
		h.logger.Error("failed to find pending requests", "error", err)
		return
	}
	for _, request := range pending {
		if ctx.Err() != nil {
			return
		}
		if err := h.advance(ctx, request.ID, h.config.Action, false); err != nil {
			h.logger.Error("failed to process pending request", "request_id", request.ID, "error", err)
		}
	}
}

// HandleExpiredRequest treats the supplied request as an ID, not authoritative
// state. A concurrent review, execution or deadline extension wins in SQLite.
func (h *TimeoutHandler) HandleExpiredRequest(request *db.Request) error {
	if request == nil {
		return fmt.Errorf("request is required")
	}
	return h.advance(context.Background(), request.ID, h.config.Action, true)
}

func (h *TimeoutHandler) handleEscalate(request *db.Request) error {
	return h.advance(context.Background(), request.ID, TimeoutActionEscalate, true)
}

func (h *TimeoutHandler) handleAutoReject(request *db.Request) error {
	return h.advance(context.Background(), request.ID, TimeoutActionAutoReject, true)
}

func (h *TimeoutHandler) handleAutoApproveWarn(request *db.Request) error {
	return h.advance(context.Background(), request.ID, TimeoutActionAutoApproveWarn, true)
}

func (h *TimeoutHandler) advance(ctx context.Context, id string, action TimeoutAction, expiredOnly bool) error {
	result, err := core.AdvancePendingRequest(ctx, h.db, id, core.PendingOptions{
		TimeoutAction: string(action), ExpiredOnly: expiredOnly,
	})
	if err != nil {
		return err
	}
	if !result.Changed {
		return nil
	}
	request := result.Request
	h.logger.Info("pending request resolved", "request_id", request.ID, "status", request.Status,
		"tier", request.RiskTier, "command", truncateString(core.ApplyRedaction(request.Command.Raw, nil), 80))
	if h.config.DesktopNotify {
		switch request.Status {
		case db.StatusEscalated:
			h.sendDesktopNotification(request)
		case db.StatusApproved:
			h.sendAutoApproveWarning(request)
		}
	}
	return nil
}

func (h *TimeoutHandler) sendDesktopNotification(request *db.Request) {
	title := fmt.Sprintf("SLB: Request Escalated (%s)", request.RiskTier)
	body := fmt.Sprintf("Request %s timed out.\nCommand: %s\nAgent: %s", truncateID(request.ID, 8),
		truncateString(core.ApplyRedaction(request.Command.Raw, nil), 50), request.RequestorAgent)
	if err := notify(title, body); err != nil {
		h.logger.Debug("desktop notification failed", "error", err)
	}
}

func (h *TimeoutHandler) sendAutoApproveWarning(request *db.Request) {
	body := fmt.Sprintf("Request %s passed its CAUTION review delay.\nCommand: %s", truncateID(request.ID, 8),
		truncateString(core.ApplyRedaction(request.Command.Raw, nil), 50))
	if err := notify("SLB: Request Auto-Approved (WARNING)", body); err != nil {
		h.logger.Debug("desktop notification failed", "error", err)
	}
}

// Notification helpers are bounded so a missing desktop service cannot stall
// the timer indefinitely. They run only after a committed policy decision.
func notify(title, body string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	switch runtime.GOOS {
	case "darwin":
		script := fmt.Sprintf(`display notification "%s" with title "%s"`, escapeAppleScript(body), escapeAppleScript(title))
		return exec.CommandContext(ctx, "osascript", "-e", script).Run()
	case "linux":
		return exec.CommandContext(ctx, "notify-send", "-u", "critical", title, body).Run()
	case "windows":
		script := fmt.Sprintf(`
			[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] | Out-Null
			$template = [Windows.UI.Notifications.ToastNotificationManager]::GetTemplateContent([Windows.UI.Notifications.ToastTemplateType]::ToastText02)
			$textNodes = $template.GetElementsByTagName("text")
			$textNodes.Item(0).AppendChild($template.CreateTextNode("%s")) | Out-Null
			$textNodes.Item(1).AppendChild($template.CreateTextNode("%s")) | Out-Null
			$toast = [Windows.UI.Notifications.ToastNotification]::new($template)
			[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier("SLB").Show($toast)
		`, escapePowerShellDoubleQuoted(title), escapePowerShellDoubleQuoted(body))
		return exec.CommandContext(ctx, "powershell", "-Command", script).Run()
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}

func escapePowerShellDoubleQuoted(s string) string {
	s = strings.ReplaceAll(s, "`", "``")
	s = strings.ReplaceAll(s, "\"", "`\"")
	s = strings.ReplaceAll(s, "$", "`$")
	s = strings.ReplaceAll(s, "\r\n", "`n")
	s = strings.ReplaceAll(s, "\n", "`n")
	s = strings.ReplaceAll(s, "\r", "`n")
	return s
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

func truncateID(id string, maxLen int) string {
	if len(id) <= maxLen {
		return id
	}
	return id[:maxLen]
}

func CheckExpiredRequests(database *db.DB) ([]*db.Request, error) {
	return database.FindExpiredRequests()
}

func StartTimeoutChecker(ctx context.Context, database *db.DB, logger *log.Logger) (*TimeoutHandler, error) {
	cfg := DefaultTimeoutConfig()
	cfg.Logger = logger
	handler := NewTimeoutHandler(database, cfg)
	if err := handler.Start(ctx); err != nil {
		return nil, err
	}
	return handler, nil
}
