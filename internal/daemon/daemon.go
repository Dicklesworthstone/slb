// Package daemon implements the SLB daemon that acts as an approval notary.
//
// The daemon does not execute commands - it only verifies approvals and provides
// local IPC for faster coordination. Commands still execute client-side.
package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/utils"
	"github.com/charmbracelet/log"
)

const daemonModeEnv = "SLB_DAEMON_MODE"

// ServerOptions configures daemon lifecycle behavior.
type ServerOptions struct {
	SocketPath string
	PIDFile    string
	Logger     *log.Logger
}

// DefaultServerOptions returns defaults aligned with the daemon client.
func DefaultServerOptions() ServerOptions {
	return ServerOptions{
		SocketPath: DefaultSocketPath(),
		PIDFile:    DefaultPIDFile(),
		Logger:     nil,
	}
}

// StartDaemon starts the daemon.
//
// If SLB_DAEMON_MODE=1, it runs in-process (blocks until shutdown).
// Otherwise it forks a detached subprocess with SLB_DAEMON_MODE=1 and returns.
func StartDaemon() error {
	return StartDaemonWithOptions(context.Background(), DefaultServerOptions())
}

// StartDaemonWithOptions starts the daemon with explicit configuration.
func StartDaemonWithOptions(ctx context.Context, opts ServerOptions) error {
	opts = normalizeServerOptions(opts)

	if daemonModeEnabled() {
		return RunDaemon(ctx, opts)
	}

	// Prevent duplicates via PID file.
	if running, pid := daemonRunning(opts); running {
		return fmt.Errorf("daemon already running (pid=%d)", pid)
	}

	// Fork this binary with the same args, but in daemon mode.
	cmd := exec.Command(os.Args[0], os.Args[1:]...)
	cmd.Env = append(os.Environ(), daemonModeEnv+"=1")

	// Best-effort: detach. Parent writes PID immediately.
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting daemon subprocess: %w", err)
	}

	if err := writePIDFile(opts.PIDFile, cmd.Process.Pid); err != nil {
		return err
	}

	// Detach so the daemon keeps running after the parent exits.
	_ = cmd.Process.Release()
	return nil
}

// StopDaemon attempts to stop the daemon gracefully.
func StopDaemon(timeout time.Duration) error {
	return StopDaemonWithOptions(DefaultServerOptions(), timeout)
}

// StopDaemonWithOptions attempts to stop the daemon gracefully.
func StopDaemonWithOptions(opts ServerOptions, timeout time.Duration) error {
	opts = normalizeServerOptions(opts)

	pid, err := readPIDFile(opts.PIDFile)
	if err != nil {
		return fmt.Errorf("reading pid file: %w", err)
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}

	// Prefer SIGTERM on unix-like systems; fall back to Interrupt.
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		_ = proc.Signal(os.Interrupt)
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			_ = os.Remove(opts.PIDFile)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("daemon did not exit within %s (pid=%d)", timeout, pid)
}

// RunDaemon runs the daemon main loop in-process (daemon mode).
func RunDaemon(ctx context.Context, opts ServerOptions) error {
	opts = normalizeServerOptions(opts)

	logger := opts.Logger
	if logger == nil {
		l, err := utils.InitDaemonLogger()
		if err != nil {
			return fmt.Errorf("init daemon logger: %w", err)
		}
		logger = l
	}

	// Ensure PID file exists for clients.
	if err := writePIDFile(opts.PIDFile, os.Getpid()); err != nil {
		return err
	}
	defer func() {
		_ = os.Remove(opts.PIDFile)
	}()

	// Ensure socket directory exists.
	if err := os.MkdirAll(filepath.Dir(opts.SocketPath), 0700); err != nil {
		return fmt.Errorf("creating socket directory: %w", err)
	}

	// Create and start the IPC server.
	ipcServer, err := NewIPCServer(opts.SocketPath, logger)
	if err != nil {
		return fmt.Errorf("creating ipc server: %w", err)
	}

	// Stop on signal or context cancellation.
	signalCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("daemon started", "pid", os.Getpid(), "pid_file", opts.PIDFile, "socket", opts.SocketPath)

	projectPath, _ := os.Getwd()
	cfg := config.DefaultConfig()
	if loaded, err := config.Load(config.LoadOptions{ProjectDir: projectPath}); err != nil {
		logger.Warn("failed to load config; using defaults", "error", err)
	} else {
		cfg = loaded
	}

	notifications := NewNotificationManager(projectPath, cfg.Notifications, logger, nil)
	go notifications.Run(signalCtx, 10*time.Second)

	servers := []*IPCServer{ipcServer}
	if strings.TrimSpace(cfg.Daemon.TCPAddr) != "" {
		tcpSrv, err := NewTCPServer(TCPServerOptions{
			Addr:        cfg.Daemon.TCPAddr,
			RequireAuth: cfg.Daemon.TCPRequireAuth,
			AllowedIPs:  cfg.Daemon.TCPAllowedIPs,
			ValidateAuth: func(ctx context.Context, sessionKey string) (bool, error) {
				dbPath := filepath.Join(projectPath, ".slb", "state.db")
				opts := db.OpenOptions{
					CreateIfNotExists: false,
					InitSchema:        false,
					ReadOnly:          true,
				}
				dbConn, err := db.OpenWithOptions(dbPath, opts)
				if err != nil {
					return false, err
				}
				defer dbConn.Close()

				var count int
				if err := dbConn.QueryRow(`SELECT COUNT(*) FROM sessions WHERE session_key = ? AND ended_at IS NULL`, sessionKey).Scan(&count); err != nil {
					return false, err
				}
				return count > 0, nil
			},
		}, logger)
		if err != nil {
			logger.Warn("tcp listener disabled", "error", err)
		} else {
			servers = append(servers, tcpSrv)
			logger.Info("tcp listener started", "addr", cfg.Daemon.TCPAddr, "require_auth", cfg.Daemon.TCPRequireAuth)
		}
	}

	errCh := make(chan error, len(servers))
	for _, srv := range servers {
		srv := srv
		go func() {
			errCh <- srv.Start(signalCtx)
		}()
	}

	select {
	case <-signalCtx.Done():
		logger.Info("daemon stopping", "reason", "signal_or_context")
		for _, srv := range servers {
			if err := srv.Stop(); err != nil {
				logger.Warn("ipc server stop error", "addr", srv.socketPath, "error", err)
			}
		}
		for i := 0; i < len(servers); i++ {
			<-errCh
		}
		return nil
	case err := <-errCh:
		if err != nil {
			logger.Error("ipc server failed", "error", err)
			for _, srv := range servers {
				_ = srv.Stop()
			}
			return fmt.Errorf("ipc server: %w", err)
		}
		for _, srv := range servers {
			_ = srv.Stop()
		}
		return nil
	}
}

func normalizeServerOptions(opts ServerOptions) ServerOptions {
	if strings.TrimSpace(opts.SocketPath) == "" {
		opts.SocketPath = DefaultSocketPath()
	}
	if strings.TrimSpace(opts.PIDFile) == "" {
		opts.PIDFile = DefaultPIDFile()
	}
	return opts
}

func daemonModeEnabled() bool {
	v := strings.TrimSpace(os.Getenv(daemonModeEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

func daemonRunning(opts ServerOptions) (bool, int) {
	pid, err := readPIDFile(opts.PIDFile)
	if err != nil {
		return false, 0
	}
	if pid <= 0 {
		return false, 0
	}
	if !processAlive(pid) {
		return false, 0
	}
	return true, pid
}

func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func writePIDFile(path string, pid int) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("pid file path is required")
	}
	if pid <= 0 {
		return fmt.Errorf("pid must be > 0")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating pid file dir: %w", err)
	}
	data := []byte(fmt.Sprintf("%d\n", pid))
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write pid file: %w", err)
	}
	return nil
}

func readPIDFile(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return 0, fmt.Errorf("empty pid file")
	}
	pid, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid pid: %w", err)
	}
	return pid, nil
}
