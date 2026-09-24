// Package daemon implements the SLB daemon that acts as an approval notary.
//
// The daemon does not execute commands - it only verifies approvals and provides
// local IPC for faster coordination. Commands still execute client-side.
package daemon

import (
	"context"
	"errors"
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
	"github.com/Dicklesworthstone/slb/internal/core"
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
	return ServerOptions{SocketPath: DefaultSocketPath(), PIDFile: DefaultPIDFile()}
}

// StartDaemon starts the daemon in process if SLB_DAEMON_MODE=1, or launches
// the same executable in daemon mode otherwise.
func StartDaemon() error {
	return StartDaemonWithOptions(context.Background(), DefaultServerOptions())
}

func StartDaemonWithOptions(ctx context.Context, opts ServerOptions) error {
	opts = normalizeServerOptions(opts)
	if daemonModeEnabled() {
		return RunDaemon(ctx, opts)
	}
	if running, pid := daemonRunning(opts); running {
		return fmt.Errorf("daemon already running (pid=%d)", pid)
	}
	cmd := exec.Command(os.Args[0], os.Args[1:]...)
	cmd.Env = append(os.Environ(), daemonModeEnv+"=1")
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting daemon subprocess: %w", err)
	}
	if err := writePIDFile(opts.PIDFile, cmd.Process.Pid); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	return nil
}

func StopDaemon(timeout time.Duration) error {
	return StopDaemonWithOptions(DefaultServerOptions(), timeout)
}

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

// RunDaemon owns the project DB, notary, pending processor, filesystem watcher,
// snapshot publisher, notifications and listeners for one daemon generation.
func RunDaemon(ctx context.Context, opts ServerOptions) error {
	opts = normalizeServerOptions(opts)
	if err := ctx.Err(); err != nil {
		return err
	}
	logger := opts.Logger
	if logger == nil {
		l, err := utils.InitDaemonLogger()
		if err != nil {
			return fmt.Errorf("init daemon logger: %w", err)
		}
		logger = l
	}
	projectPath, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolving daemon project: %w", err)
	}
	cfg, err := config.Load(config.LoadOptions{ProjectDir: projectPath})
	if err != nil {
		// A daemon running a weaker default policy is not graceful degradation.
		return fmt.Errorf("loading daemon config: %w", err)
	}
	dbPath := filepath.Join(projectPath, ".slb", "state.db")
	database, err := db.OpenAndMigrate(dbPath)
	if err != nil {
		return fmt.Errorf("opening daemon database: %w", err)
	}
	defer database.Close()

	if err := writePIDFile(opts.PIDFile, os.Getpid()); err != nil {
		return err
	}
	defer func() { _ = os.Remove(opts.PIDFile) }()
	if err := os.MkdirAll(filepath.Dir(opts.SocketPath), 0700); err != nil {
		return fmt.Errorf("creating socket directory: %w", err)
	}
	ipcServer, err := NewIPCServer(opts.SocketPath, logger)
	if err != nil {
		return fmt.Errorf("creating ipc server: %w", err)
	}
	signalCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	loadDaemonCustomPatterns(projectPath, logger)

	servers := []*IPCServer{ipcServer}
	defer func() {
		for _, server := range servers {
			if err := server.Stop(); err != nil {
				logger.Warn("ipc server stop error", "addr", server.socketPath, "error", err)
			}
		}
	}()
	if strings.TrimSpace(cfg.Daemon.TCPAddr) != "" {
		tcpSrv, err := NewTCPServer(TCPServerOptions{
			Addr: cfg.Daemon.TCPAddr, RequireAuth: cfg.Daemon.TCPRequireAuth, AllowedIPs: cfg.Daemon.TCPAllowedIPs,
			ValidateAuth: func(ctx context.Context, sessionKey string) (bool, error) {
				if err := ctx.Err(); err != nil {
					return false, err
				}
				var count int
				if err := database.QueryRow(`SELECT COUNT(*) FROM sessions WHERE session_key = ? AND ended_at IS NULL AND project_path = ?`, sessionKey, projectPath).Scan(&count); err != nil {
					return false, err
				}
				return count > 0, nil
			},
		}, logger)
		if err != nil {
			// An explicitly configured transport must not silently disappear.
			return fmt.Errorf("starting configured TCP listener: %w", err)
		}
		servers = append(servers, tcpSrv)
	}
	stopServices, err := startProjectServices(signalCtx, database, projectPath, cfg, servers, logger)
	if err != nil {
		// A shutdown request (signal or caller cancellation) that arrives while
		// startup is still reconciling state is a clean stop, exactly as it is
		// once the daemon is serving. Any other startup failure is reported.
		if signalCtx.Err() != nil && errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	defer stopServices()

	errCh := make(chan error, len(servers))
	for _, server := range servers {
		go func(server *IPCServer) { errCh <- server.Start(signalCtx) }(server)
	}
	logger.Info("daemon started", "pid", os.Getpid(), "project", projectPath,
		"pid_file", opts.PIDFile, "socket", opts.SocketPath)
	select {
	case <-signalCtx.Done():
		return nil
	case err := <-errCh:
		if signalCtx.Err() != nil {
			return nil
		}
		if err == nil {
			err = fmt.Errorf("listener stopped unexpectedly")
		}
		return fmt.Errorf("ipc server: %w", err)
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

// loadDaemonCustomPatterns mirrors the CLI's legacy custom-pattern loader.
// Execution and pending-policy gates independently load authoritative policy.
func loadDaemonCustomPatterns(projectPath string, logger *log.Logger) {
	dbPath := filepath.Join(projectPath, ".slb", "state.db")
	dbConn, err := db.OpenWithOptions(dbPath, db.OpenOptions{
		CreateIfNotExists: false, InitSchema: false, ReadOnly: true,
	})
	if err != nil {
		logger.Debug("custom_patterns load skipped (no project DB)", "path", dbPath, "error", err)
		return
	}
	defer dbConn.Close()
	rows, err := dbConn.ListCustomPatterns()
	if err != nil {
		logger.Warn("custom_patterns query failed", "error", err)
		return
	}
	engine := core.GetDefaultEngine()
	existing := make(map[string]struct{})
	for tierName, list := range engine.AllPatterns() {
		for _, p := range list {
			existing[tierName+"\x00"+p.Pattern] = struct{}{}
		}
	}
	loaded, skipped := 0, 0
	for _, row := range rows {
		tier := parseDaemonTier(row.Tier)
		if tier == "" {
			logger.Warn("skipping persisted pattern with unrecognized tier", "tier", row.Tier, "pattern", row.Pattern)
			skipped++
			continue
		}
		key := string(tier) + "\x00" + row.Pattern
		if _, dup := existing[key]; dup {
			continue
		}
		if err := engine.AddPattern(tier, row.Pattern, row.Description, row.Source); err != nil {
			logger.Warn("skipping invalid persisted pattern", "pattern", row.Pattern, "tier", row.Tier, "error", err)
			skipped++
			continue
		}
		existing[key] = struct{}{}
		loaded++
	}
	if loaded > 0 || skipped > 0 {
		logger.Info("custom_patterns merged into engine", "loaded", loaded, "skipped", skipped)
	}
}

func parseDaemonTier(s string) core.RiskTier {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical":
		return core.RiskTierCritical
	case "dangerous":
		return core.RiskTierDangerous
	case "caution":
		return core.RiskTierCaution
	case "safe":
		return core.RiskTier(core.RiskSafe)
	default:
		return ""
	}
}
