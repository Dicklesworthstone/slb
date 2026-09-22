package daemon

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/Dicklesworthstone/slb/internal/audit"
	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/db"
	blockedalerts "github.com/Dicklesworthstone/slb/internal/notifications"
	"github.com/charmbracelet/log"
)

// startProjectServices wires the existing components into the running daemon.
// The caller owns database and listeners; stop joins every worker before either
// can be closed. No component here executes an approved command.
func startProjectServices(parent context.Context, database *db.DB, project string, cfg config.Config, servers []*IPCServer, logger *log.Logger) (func(), error) {
	ctx, cancel := context.WithCancel(parent)
	if err := reconcileRequestState(ctx, database, project, servers); err != nil {
		cancel()
		return nil, fmt.Errorf("initial request snapshot: %w", err)
	}
	verifier := NewVerifier(database)
	for _, server := range servers {
		server.SetVerifier(verifier)
	}

	var watcher *Watcher
	if cfg.Daemon.UseFileWatcher {
		var err error
		watcher, err = NewWatcher(project)
		if err != nil {
			logger.Warn("filesystem watcher unavailable; using periodic reconciliation", "error", err)
		} else if err := watcher.Start(ctx); err != nil {
			_ = watcher.Stop()
			watcher = nil
			logger.Warn("filesystem watcher did not start; using periodic reconciliation", "error", err)
		}
	}
	pendingConfig := TimeoutConfigFromConfig(cfg)
	pendingConfig.Logger = logger
	pendingConfig.ProjectPath = project
	pending := NewTimeoutHandler(database, pendingConfig)
	if err := pending.Start(ctx); err != nil {
		cancel()
		if watcher != nil {
			_ = watcher.Stop()
		}
		return nil, fmt.Errorf("starting pending processor: %w", err)
	}

	notifications := NewNotificationManager(project, cfg.Notifications, logger, nil)
	var blocked *blockedalerts.Service
	if cfg.Notifications.Blocked.Enabled {
		directory, err := audit.DefaultDirectory()
		if err == nil {
			blocked, err = blockedalerts.NewService(project, directory, cfg.Notifications.Blocked, blockedalerts.Destinations{
				WebhookURL: cfg.Notifications.WebhookURL, DesktopEnabled: cfg.Notifications.DesktopEnabled,
				AgentMailEnabled: cfg.Integrations.AgentMailEnabled, AgentMailThread: cfg.Integrations.AgentMailThread,
				AgentMailToken: os.Getenv("SLB_AGENT_MAIL_TOKEN"), AgentMailSenderToken: os.Getenv("SLB_AGENT_MAIL_SENDER_TOKEN"),
			})
		}
		if err != nil {
			// Alert configuration cannot weaken or disable the approval notary.
			logger.Warn("blocked-command alerts unavailable; audit records retained", "error", err)
		}
	}
	var workers sync.WaitGroup
	workers.Add(3)
	go func() {
		defer workers.Done()
		runRequestStateMonitor(ctx, database, project, servers, watcher, logger)
	}()
	go func() {
		defer workers.Done()
		notifications.Run(ctx, 10*time.Second)
	}()
	go func() {
		defer workers.Done()
		lastError := ""
		blocked.Run(ctx, 5*time.Second, func(report blockedalerts.Report, err error) {
			if err != nil {
				if err.Error() != lastError {
					logger.Warn("blocked-command alert delivery degraded", "error", err, "failed", report.Failed)
				}
				lastError = err.Error()
				return
			}
			if report.Deferred > 0 && lastError != "" {
				return // Waiting for retry capacity is not a recovered transport.
			}
			if report.Sent > 0 || lastError != "" {
				logger.Info("blocked-command alert delivery", "sent", report.Sent, "deferred", report.Deferred)
			}
			lastError = ""
		})
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			if watcher != nil {
				_ = watcher.Stop()
			}
			pending.Stop()
			workers.Wait()
		})
	}, nil
}
