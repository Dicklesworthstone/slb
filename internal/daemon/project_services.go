package daemon

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/db"
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
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		runRequestStateMonitor(ctx, database, project, servers, watcher, logger)
	}()
	go func() {
		defer workers.Done()
		notifications.Run(ctx, 10*time.Second)
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
