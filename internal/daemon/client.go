// Package daemon provides client-side daemon communication and graceful degradation.
// When the daemon is unavailable, commands continue to work with degraded functionality.
package daemon

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/log"
)

// DaemonStatus represents the current state of the daemon.
type DaemonStatus int

const (
	// DaemonRunning indicates the daemon is running and responsive.
	DaemonRunning DaemonStatus = iota
	// DaemonNotRunning indicates no daemon process was found.
	DaemonNotRunning
	// DaemonUnresponsive indicates a process exists but is not responding.
	DaemonUnresponsive
)

// String returns a human-readable status description.
func (s DaemonStatus) String() string {
	switch s {
	case DaemonRunning:
		return "running"
	case DaemonNotRunning:
		return "not running"
	case DaemonUnresponsive:
		return "unresponsive"
	default:
		return "unknown"
	}
}

// Client provides methods to interact with the daemon.
type Client struct {
	socketPath string
	pidFile    string
	logger     *log.Logger
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithSocketPath sets a custom socket path.
func WithSocketPath(path string) ClientOption {
	return func(c *Client) {
		c.socketPath = path
	}
}

// WithPIDFile sets a custom PID file path.
func WithPIDFile(path string) ClientOption {
	return func(c *Client) {
		c.pidFile = path
	}
}

// WithLogger sets a custom logger.
func WithLogger(l *log.Logger) ClientOption {
	return func(c *Client) {
		c.logger = l
	}
}

// NewClient creates a new daemon client with optional configuration.
func NewClient(opts ...ClientOption) *Client {
	c := &Client{
		socketPath: DefaultSocketPath(),
		pidFile:    DefaultPIDFile(),
		logger:     log.Default(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// DefaultSocketPath returns the default Unix socket path for the current project.
// Format: /tmp/slb-{project-hash}.sock
//
// The project hash is derived from the path of the nearest ancestor
// directory containing a .slb/ directory (walked up from CWD), not
// from CWD itself. This ensures the daemon and any hook invoked from
// a sub-directory of the same project converge on the same socket
// (issue #3) — without it, a daemon started in /path/A and a hook
// fired from /path/A/sub silently miss each other.
//
// Falls back to the raw CWD if no .slb/ ancestor exists, which
// preserves the v0.3.x behavior for setups that don't use `slb init`.
func DefaultSocketPath() string {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	return SocketPathForCWD(cwd)
}

// SocketPathForCWD returns the project-scoped Unix socket path that a hook
// running from cwd will use. This is the explicit-CWD counterpart to
// DefaultSocketPath and must stay in lockstep with generated hook discovery.
func SocketPathForCWD(cwd string) string {
	hashBase := projectRootForSocket(cwd)
	hash := sha256.Sum256([]byte(hashBase))
	shortHash := hex.EncodeToString(hash[:])[:12]
	return filepath.Join(os.TempDir(), fmt.Sprintf("slb-%s.sock", shortHash))
}

// projectRootForSocket walks up from `start` looking for a `.slb/`
// directory and returns the absolute path of the directory containing
// it. Returns the input directory unchanged if no .slb/ ancestor is
// found, so installations without an `slb init` still behave the
// pre-existing way.
func projectRootForSocket(start string) string {
	abs, err := filepath.Abs(start)
	if err != nil {
		return start
	}
	dir := abs
	for {
		if info, err := os.Stat(filepath.Join(dir, ".slb")); err == nil && info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Hit the filesystem root without finding .slb — fall
			// back to the original CWD so behavior matches pre-fix
			// installations.
			return abs
		}
		dir = parent
	}
}

// DefaultPIDFile returns the default PID file path.
// Format: /tmp/slb-daemon-{username}.pid
func DefaultPIDFile() string {
	username := "unknown"
	if u, err := user.Current(); err == nil {
		username = u.Username
	}
	// Sanitize username for filename
	username = strings.ReplaceAll(username, string(filepath.Separator), "_")
	return filepath.Join(os.TempDir(), fmt.Sprintf("slb-daemon-%s.pid", username))
}

// IsDaemonRunning checks if the daemon is running and responsive.
// Returns true if the daemon is available for IPC communication.
func (c *Client) IsDaemonRunning() bool {
	return c.GetStatus() == DaemonRunning
}

// GetStatus returns the detailed daemon status.
func (c *Client) GetStatus() DaemonStatus {
	// Prefer real connectivity checks (supports TCP when SLB_HOST is set).
	if c.canConnectSocket() {
		return DaemonRunning
	}

	// Fall back to PID checks to distinguish "not running" vs "unresponsive".
	pid, err := c.readPID()
	if err != nil {
		return DaemonNotRunning
	}
	if !c.isProcessAlive(pid) {
		return DaemonNotRunning
	}
	return DaemonUnresponsive
}

// StatusInfo returns detailed status information for diagnostics.
type StatusInfo struct {
	Status      DaemonStatus
	PID         int
	PIDFile     string
	SocketPath  string
	SocketAlive bool
	Message     string
}

// GetStatusInfo returns detailed status information.
func (c *Client) GetStatusInfo() StatusInfo {
	info := StatusInfo{
		PIDFile:    c.pidFile,
		SocketPath: c.socketPath,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	socketCheck := c.checkSocketConnectivity(ctx)

	if socketCheck.host != "" && socketCheck.transport == "tcp" {
		info.PIDFile = ""
		info.SocketPath = socketCheck.host
		info.Status = DaemonRunning
		info.SocketAlive = true
		info.Message = fmt.Sprintf("Daemon reachable at %s", socketCheck.host)
		return info
	}

	pid, err := c.readPID()
	if err != nil {
		if socketCheck.transport == "unix" {
			info.Status = DaemonRunning
			info.SocketAlive = true
			if socketCheck.host != "" {
				info.Message = fmt.Sprintf("SLB_HOST unreachable at %s; using local unix socket", socketCheck.host)
			} else {
				info.Message = "Daemon running (PID file missing)"
			}
		} else {
			info.Status = DaemonNotRunning
			if socketCheck.host != "" {
				info.Message = fmt.Sprintf("Daemon not reachable at %s", socketCheck.host)
			} else {
				info.Message = fmt.Sprintf("PID file not found or invalid: %v", err)
			}
		}
		return info
	}
	info.PID = pid

	if !c.isProcessAlive(pid) {
		info.Status = DaemonNotRunning
		info.Message = fmt.Sprintf("Process %d is not running (stale PID file)", pid)
		return info
	}

	if socketCheck.transport != "unix" {
		info.Status = DaemonUnresponsive
		if socketCheck.host != "" {
			info.Message = fmt.Sprintf("Daemon not reachable at %s and local socket ping failed", socketCheck.host)
		} else {
			info.Message = fmt.Sprintf("Process %d exists but socket connection failed", pid)
		}
		return info
	}

	info.Status = DaemonRunning
	info.SocketAlive = true
	if socketCheck.host != "" && socketCheck.tcpErr != nil {
		info.Message = fmt.Sprintf("SLB_HOST unreachable at %s; using local unix socket", socketCheck.host)
	} else {
		info.Message = fmt.Sprintf("Daemon running with PID %d", pid)
	}
	return info
}

// readPID reads the PID from the PID file.
func (c *Client) readPID() (int, error) {
	data, err := os.ReadFile(c.pidFile)
	if err != nil {
		return 0, err
	}
	pidStr := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return 0, fmt.Errorf("invalid PID in file: %w", err)
	}
	return pid, nil
}

// isProcessAlive checks if a process is alive using kill -0.
func (c *Client) isProcessAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Send signal 0 to check if process exists
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}

// canConnectSocket attempts to connect to the Unix socket.
func (c *Client) canConnectSocket() bool {
	// Quick timeout for checking socket availability
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	result := c.checkSocketConnectivity(ctx)
	return result.transport != ""
}

type socketConnectivityResult struct {
	host      string
	transport string // "", "tcp", "unix"
	tcpErr    error
	unixErr   error
}

func (c *Client) checkSocketConnectivity(ctx context.Context) socketConnectivityResult {
	result := socketConnectivityResult{
		host: strings.TrimSpace(os.Getenv("SLB_HOST")),
	}

	if result.host != "" {
		result.tcpErr = pingDaemonTCP(ctx, result.host, strings.TrimSpace(os.Getenv("SLB_SESSION_KEY")))
		if result.tcpErr == nil {
			result.transport = "tcp"
			return result
		}
	}

	result.unixErr = pingDaemonUnix(ctx, c.socketPath)
	if result.unixErr == nil {
		result.transport = "unix"
	}
	return result
}

func pingDaemonUnix(ctx context.Context, socketPath string) error {
	if strings.TrimSpace(socketPath) == "" {
		return fmt.Errorf("socket path is empty")
	}
	return dialAndPing(ctx, "unix", socketPath, nil)
}

func pingDaemonTCP(ctx context.Context, addr string, sessionKey string) error {
	if strings.TrimSpace(addr) == "" {
		return fmt.Errorf("tcp addr is empty")
	}
	auth := strings.TrimSpace(sessionKey)
	return dialAndPing(ctx, "tcp", addr, &auth)
}

func dialAndPing(ctx context.Context, network, addr string, auth *string) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
	}

	if auth != nil {
		hello, err := json.Marshal(map[string]string{
			"auth": strings.TrimSpace(*auth),
		})
		if err != nil {
			return fmt.Errorf("marshal handshake: %w", err)
		}
		hello = append(hello, '\n')
		if _, err := conn.Write(hello); err != nil {
			return fmt.Errorf("write handshake: %w", err)
		}
	}

	req := RPCRequest{
		Method: "ping",
		ID:     1,
	}
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal ping: %w", err)
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		return fmt.Errorf("write ping: %w", err)
	}

	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("read ping response: %w", err)
	}

	var resp RPCResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return fmt.Errorf("unmarshal ping response: %w", err)
	}
	if resp.Error != nil {
		return fmt.Errorf("ping error: %s", resp.Error.Message)
	}

	if m, ok := resp.Result.(map[string]any); ok {
		if v, ok := m["pong"].(bool); ok && v {
			return nil
		}
	}
	return fmt.Errorf("unexpected ping response")
}

// DegradedMode represents whether we're operating in degraded mode.
var degradedModeWarningShown = false

// WarningMessage returns the standard warning message for degraded mode.
func WarningMessage() string {
	return `Warning: slb daemon not running. Some features disabled.
Start daemon with: slb daemon start
Continuing with file-based polling...`
}

// ShortWarning returns a brief warning for inline display.
func ShortWarning() string {
	return "daemon not running - real-time updates disabled"
}

// ShowDegradedWarning prints the degraded mode warning to stderr (once per session).
func ShowDegradedWarning() {
	if !degradedModeWarningShown {
		fmt.Fprintln(os.Stderr, WarningMessage())
		degradedModeWarningShown = true
	}
}

// ShowDegradedWarningQuiet prints a short warning suitable for non-interactive use.
func ShowDegradedWarningQuiet() {
	if !degradedModeWarningShown {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", ShortWarning())
		degradedModeWarningShown = true
	}
}

// ResetWarningState resets the warning state (useful for testing).
func ResetWarningState() {
	degradedModeWarningShown = false
}

// WithDaemonOrFallback executes fn if daemon is running, otherwise executes fallback.
// Shows a warning when falling back to degraded mode.
func (c *Client) WithDaemonOrFallback(fn, fallback func()) {
	if c.IsDaemonRunning() {
		fn()
	} else {
		ShowDegradedWarning()
		fallback()
	}
}

// WithDaemonOrFallbackErr is like WithDaemonOrFallback but for functions returning errors.
func (c *Client) WithDaemonOrFallbackErr(fn, fallback func() error) error {
	if c.IsDaemonRunning() {
		return fn()
	}
	ShowDegradedWarning()
	return fallback()
}

// MustHaveDaemon returns an error if daemon is not running.
// Use this for commands that absolutely require the daemon.
func (c *Client) MustHaveDaemon() error {
	if c.IsDaemonRunning() {
		return nil
	}
	return fmt.Errorf("daemon not running - start with: slb daemon start")
}

// TryDaemon attempts to communicate with daemon, returning whether it succeeded.
// Does not show warning, just returns status silently.
func (c *Client) TryDaemon(fn func() error) (usedDaemon bool, err error) {
	if c.IsDaemonRunning() {
		return true, fn()
	}
	return false, nil
}

// FeatureAvailability describes which features are available in current mode.
type FeatureAvailability struct {
	RealTimeUpdates        bool
	DesktopNotifications   bool
	AgentMailNotifications bool
	FastIPC                bool
	FilePolling            bool // Always true as fallback
}

// GetFeatureAvailability returns feature availability based on daemon status.
func (c *Client) GetFeatureAvailability() FeatureAvailability {
	if c.IsDaemonRunning() {
		return FeatureAvailability{
			RealTimeUpdates:        true,
			DesktopNotifications:   true,
			AgentMailNotifications: true,
			FastIPC:                true,
			FilePolling:            true,
		}
	}
	return FeatureAvailability{
		RealTimeUpdates:        false,
		DesktopNotifications:   false,
		AgentMailNotifications: false,
		FastIPC:                false,
		FilePolling:            true, // Always available
	}
}

// Default client for convenience functions.
var defaultClient *Client

// getDefaultClient returns or creates the default client.
func getDefaultClient() *Client {
	if defaultClient == nil {
		defaultClient = NewClient()
	}
	return defaultClient
}

// IsDaemonRunning is a convenience function using the default client.
func IsDaemonRunning() bool {
	return getDefaultClient().IsDaemonRunning()
}

// GetStatus is a convenience function using the default client.
func GetStatus() DaemonStatus {
	return getDefaultClient().GetStatus()
}

// GetStatusInfo is a convenience function using the default client.
func GetStatusInfo() StatusInfo {
	return getDefaultClient().GetStatusInfo()
}

// WithDaemonOrFallback is a convenience function using the default client.
func WithDaemonOrFallback(fn, fallback func()) {
	getDefaultClient().WithDaemonOrFallback(fn, fallback)
}

// GetFeatureAvailability is a convenience function using the default client.
func GetFeatureAvailability() FeatureAvailability {
	return getDefaultClient().GetFeatureAvailability()
}
