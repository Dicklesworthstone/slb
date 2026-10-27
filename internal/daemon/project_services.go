package daemon

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Dicklesworthstone/slb/internal/audit"
	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/db"
	slbgit "github.com/Dicklesworthstone/slb/internal/git"
	blockedalerts "github.com/Dicklesworthstone/slb/internal/notifications"
	"github.com/charmbracelet/log"
)

// startProjectServices wires the existing components into the running daemon.
// The caller owns database and listeners; stop joins every worker before either
// can be closed. No component here executes an approved command.
func startProjectServices(parent context.Context, database *db.DB, project, configPath string, cfg config.Config, servers []*IPCServer, logger *log.Logger) (func(), error) {
	ctx, cancel := context.WithCancel(parent)
	if err := reconcileRequestState(ctx, database, project, servers); err != nil {
		cancel()
		return nil, fmt.Errorf("initial request snapshot: %w", err)
	}
	verifier := NewVerifier(database)
	verifier.projectPath, verifier.configPath = project, configPath
	for _, server := range servers {
		server.SetVerifier(verifier)
		server.hookPolicies.projectPath, server.hookPolicies.configPath = project, configPath
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
	pendingConfig.ConfigPath = configPath
	// Administrative handlers may force an action, but a running project
	// daemon must re-read the selected policy at each writer reservation.
	pendingConfig.Action = ""
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
	var requestNotifications *blockedalerts.RequestService
	if cfg.Notifications.Requests.Enabled {
		var err error
		requestNotifications, err = blockedalerts.NewRequestService(project, cfg.Notifications.Requests,
			requestJournalSource{database: database}, os.Getenv("SLB_AGENT_MAIL_TOKEN"), os.Getenv("SLB_AGENT_MAIL_SENDER_TOKEN"))
		if err != nil {
			logger.Warn("request notifications unavailable; journal retained", "error", err)
		}
	}
	var workers sync.WaitGroup
	workers.Add(5)
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
	go func() {
		defer workers.Done()
		lastError := ""
		requestNotifications.Run(ctx, func(err error) {
			if err.Error() != lastError {
				logger.Warn("request notification delivery degraded; journal retained", "error", err)
			}
			lastError = err.Error()
		})
	}()
	go func() {
		defer workers.Done()
		runRequestHistory(ctx, database, project, cfg.History, logger)
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

// Git history is an optional projection of already committed journal events.
// Its failures must not change an authorization or execution outcome. The Git
// commit itself stores the cursor, so a later daemon can replay after downtime.
func runRequestHistory(ctx context.Context, database *db.DB, project string, cfg config.HistoryConfig, logger *log.Logger) {
	if !cfg.AutoGitCommit || strings.TrimSpace(cfg.GitRepoPath) == "" {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastError := ""
	for ctx.Err() == nil {
		attempt, cancel := context.WithTimeout(ctx, 30*time.Second)
		report, err := slbgit.SyncRequestHistory(attempt, database, cfg.GitRepoPath, project, db.DefaultRequestEventLimit)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if err.Error() != lastError {
				logger.Warn("Git request history synchronization failed; journal retained", "error", err)
			}
			lastError = err.Error()
		} else {
			if report.Events > 0 || lastError != "" {
				logger.Info("Git request history synchronized", "events", report.Events, "ref", report.Ref)
			}
			lastError = ""
			if report.HasMore {
				continue
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
