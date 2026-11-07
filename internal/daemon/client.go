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
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
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
	socketPath   string
	pidFile      string
	runtime      *RuntimePaths
	discoveryErr error
	logger       *log.Logger
	localOnly    bool
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithSocketPath sets a custom socket path.
func WithSocketPath(path string) ClientOption {
	return func(c *Client) {
		c.socketPath = path
		// An explicit path is a deliberate transport selection, not project
		// discovery. Project callers use NewProjectClient instead.
		c.runtime, c.discoveryErr = nil, nil
	}
}

// WithPIDFile sets a custom PID file path.
func WithPIDFile(path string) ClientOption {
	return func(c *Client) {
		c.pidFile = path
	}
}

// WithLocalOnly binds status checks to the selected local project's socket
// and PID file. Lifecycle commands use this so SLB_HOST cannot substitute an
// unrelated remote daemon; ordinary clients retain their TCP behavior.
func WithLocalOnly() ClientOption {
	return func(c *Client) {
		c.localOnly = true
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
	defaults := DefaultServerOptions()
	return NewProjectClient(defaults.ProjectDir, opts...)
}

// NewProjectClient shares discovery with hooks and lifecycle commands, so a
// changed caller environment cannot select a different socket or PID file.
func NewProjectClient(cwd string, opts ...ClientOption) *Client {
	paths, err := DiscoverRuntime(cwd)
	c := &Client{
		socketPath:   paths.SocketPath,
		pidFile:      paths.PIDFile,
		runtime:      &paths,
		discoveryErr: err,
		logger:       log.Default(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// DefaultSocketPath returns the current project's recorded socket, or its
// expected default when discovery is unavailable. Communication must use a
// project client, which retains discovery errors and verifies the listener.
func DefaultSocketPath() string {
	return NewClient().socketPath
}

// SocketPathForCWD computes a new daemon's default socket placement, using a
// hash of its physical project root and the daemon's temporary directory.
// Clients use DiscoverRuntime/NewProjectIPCClient to find the published path
// instead of recomputing placement from their own environment (issue #33).
// The generated hook mirrors this default only when no record exists.
func SocketPathForCWD(cwd string) string {
	return projectRuntimePath(cwd, ".sock")
}

// PIDFileForCWD returns the PID file for the same project identity used by
// SocketPathForCWD. The shared stem keeps lifecycle commands and IPC aligned.
func PIDFileForCWD(cwd string) string {
	return projectRuntimePath(cwd, ".pid")
}

func projectRuntimePath(cwd, extension string) string {
	// Runtime resources use the physical project identity across -C, PWD,
	// and hook aliases. Approval records retain their original path spelling
	// through ProjectRootForCWD, so canonicalize only the runtime lookup.
	if abs, err := filepath.Abs(cwd); err == nil {
		if physical, err := filepath.EvalSymlinks(abs); err == nil {
			cwd = physical
		}
	}
	hashBase := ProjectRootForCWD(cwd)
	hash := sha256.Sum256([]byte(hashBase))
	shortHash := hex.EncodeToString(hash[:])[:12]
	return filepath.Join(os.TempDir(), "slb-"+shortHash+extension)
}

// ProjectRootForCWD walks up from start looking for a .slb/ directory and
// returns the absolute path of the directory containing it. Returns the
// absolute input directory if no .slb/ ancestor is
// found, so installations without an `slb init` still behave the
// pre-existing way. slb's per-user directory without state.db is not a
// project marker (config.IsUserDirWithoutState), as in PhysicalProjectRootForCWD.
func ProjectRootForCWD(start string) string {
	abs, err := filepath.Abs(start)
	if err != nil {
		return start
	}
	dir := abs
	for {
		marker := filepath.Join(dir, ".slb")
		if info, err := os.Stat(marker); err == nil && info.IsDir() {
			if userDir, err := config.IsUserDirWithoutState(marker); err != nil || !userDir {
				return dir
			}
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

// PhysicalProjectRootForCWD selects policy and state for an existing absolute
// working directory. Resolve aliases before ancestor discovery so an alias into
// another project's descendant cannot select its lexical parent's policy.
// Approval records keep their literal paths through ProjectRootForCWD instead.
// slb's per-user directory (~/.slb) without state.db marks no project, so the
// walk continues past it; any other .slb directory is the project's marker,
// and its missing state is refused by the callers.
func PhysicalProjectRootForCWD(cwd string) (string, error) {
	if !filepath.IsAbs(cwd) {
		return "", fmt.Errorf("project working directory must be absolute")
	}
	physical, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", fmt.Errorf("resolving project working directory: %w", err)
	}
	info, err := os.Stat(physical)
	if err != nil {
		return "", fmt.Errorf("reading project working directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("project working directory is not a directory")
	}
	for dir := physical; ; dir = filepath.Dir(dir) {
		marker := filepath.Join(dir, ".slb")
		if _, err := os.Lstat(marker); err == nil {
			info, err := os.Stat(marker)
			if err != nil {
				return "", fmt.Errorf("reading project state directory: %w", err)
			}
			if !info.IsDir() {
				return "", fmt.Errorf("project state path %q is not a directory", marker)
			}
			userDir, err := config.IsUserDirWithoutState(marker)
			if err != nil {
				return "", fmt.Errorf("reading project state directory: %w", err)
			}
			if !userDir {
				return dir, nil
			}
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("finding project state directory: %w", err)
		}
		if filepath.Dir(dir) == dir {
			return physical, nil
		}
	}
}

// DefaultPIDFile returns the recorded PID file for the current project, or its
// expected default when discovery is unavailable.
func DefaultPIDFile() string {
	return NewClient().pidFile
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
	if c.discoveryErr != nil {
		return DaemonNotRunning
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
	if c.discoveryErr != nil {
		info.Status = DaemonNotRunning
		info.Message = "Project daemon discovery is invalid; refusing fallback socket lookup"
		return info
	}

	pid, err := c.readPID()
	if socketCheck.transport == "unix" {
		daemonPID, ok := socketCheck.pid, socketCheck.pid > 0
		if c.runtime == nil {
			daemonPID, ok = socketDaemonPID(ctx, c.socketPath)
		}
		if ok {
			// The selected listener identifies its own process. A stale file
			// may name another live daemon, so existence alone cannot bind
			// that PID to this project's socket.
			info.Status = DaemonRunning
			info.SocketAlive = true
			info.PID = daemonPID
			switch {
			case err != nil:
				info.Message = fmt.Sprintf("Daemon running with PID %d (PID file missing or invalid)", daemonPID)
			case pid != daemonPID:
				info.Message = fmt.Sprintf("Daemon running with PID %d (PID file mismatch: recorded %d)", daemonPID, pid)
			default:
				info.Message = fmt.Sprintf("Daemon running with PID %d", daemonPID)
			}
			if socketCheck.host != "" {
				info.Message = fmt.Sprintf("SLB_HOST unreachable at %s; using local unix socket; %s", socketCheck.host, info.Message)
			}
			return info
		}
	}
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
		if socketCheck.transport == "unix" {
			// A responsive project socket is authoritative even if a previous
			// daemon generation left a stale PID file behind.
			info.Status = DaemonRunning
			info.SocketAlive = true
			info.PID = 0
			info.Message = "Daemon running (stale PID file)"
			return info
		}
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

// socketDaemonPID reads the identity published by current daemon listeners.
// Older listeners and standalone IPC servers omit it; callers retain their
// existing socket/PID fallback when no valid identity is available.
func socketDaemonPID(ctx context.Context, socketPath string) (int, bool) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return 0, false
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return 0, false
		}
	}
	if err := json.NewEncoder(conn).Encode(RPCRequest{Method: "status", ID: 1}); err != nil {
		return 0, false
	}
	var response struct {
		Result struct {
			PID         int    `json:"pid"`
			ProjectPath string `json:"project_path"`
		} `json:"result"`
		Error *Error `json:"error"`
		ID    int64  `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(conn, 4096)).Decode(&response); err != nil || response.Error != nil || response.ID != 1 || response.Result.PID <= 0 || !filepath.IsAbs(response.Result.ProjectPath) {
		return 0, false
	}
	return response.Result.PID, true
}

// readPID reads the PID from the PID file.
func (c *Client) readPID() (int, error) {
	return readPIDFile(c.pidFile)
}

// isProcessAlive checks if a process is alive using kill -0.
func (c *Client) isProcessAlive(pid int) bool {
	return processAlive(pid)
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
	pid       int
}

func (c *Client) checkSocketConnectivity(ctx context.Context) socketConnectivityResult {
	var result socketConnectivityResult
	if !c.localOnly {
		result.host = strings.TrimSpace(os.Getenv("SLB_HOST"))
	}

	if result.host != "" {
		result.tcpErr = pingDaemonTCP(ctx, result.host, strings.TrimSpace(os.Getenv("SLB_SESSION_KEY")))
		if result.tcpErr == nil {
			result.transport = "tcp"
			return result
		}
	}

	if c.runtime != nil {
		client := &IPCClient{socketPath: c.socketPath, runtime: c.runtime, discoveryErr: c.discoveryErr, localOnly: true}
		defer func() { _ = client.Close() }()
		var status *DaemonStatusInfo
		status, result.unixErr = client.Status(ctx)
		if result.unixErr == nil {
			result.pid = status.PID
		}
	} else {
		result.unixErr = pingDaemonUnix(ctx, c.socketPath)
	}
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
