package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/charmbracelet/log"
)

func TestDaemonModeEnabled(t *testing.T) {
	t.Setenv(daemonModeEnv, "1")
	if !daemonModeEnabled() {
		t.Fatalf("expected daemonModeEnabled to be true")
	}

	t.Setenv(daemonModeEnv, "true")
	if !daemonModeEnabled() {
		t.Fatalf("expected daemonModeEnabled to be true for 'true'")
	}

	t.Setenv(daemonModeEnv, "0")
	if daemonModeEnabled() {
		t.Fatalf("expected daemonModeEnabled to be false for '0'")
	}
}

func TestRunDaemon_WritesPIDAndSocketAndCleansUp(t *testing.T) {
	tmp := shortSocketDir(t)
	pidFile := filepath.Join(tmp, "slb.pid")
	socketPath := filepath.Join(tmp, "s.sock")

	logger := log.NewWithOptions(io.Discard, log.Options{})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunDaemon(ctx, ServerOptions{
			SocketPath: socketPath,
			PIDFile:    pidFile,
			Logger:     logger,
		})
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(pidFile); err == nil {
			if _, err := os.Stat(socketPath); err == nil {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}

	cancel()

	if err := <-errCh; err != nil {
		t.Fatalf("RunDaemon returned error: %v", err)
	}

	if _, err := os.Stat(pidFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected pid file to be removed, stat err=%v", err)
	}
	if _, err := os.Stat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected socket to be removed, stat err=%v", err)
	}
}

// ============== daemonRunning Tests ==============

func TestDaemonRunningFunc_NoPIDFile(t *testing.T) {
	tmp := t.TempDir()
	opts := ServerOptions{
		PIDFile:    filepath.Join(tmp, "nonexistent.pid"),
		SocketPath: filepath.Join(tmp, "test.sock"),
	}

	running, pid := daemonRunning(opts)
	if running {
		t.Error("expected not running when pid file doesn't exist")
	}
	if pid != 0 {
		t.Errorf("expected pid 0, got %d", pid)
	}
}

func TestDaemonRunning_InvalidPID(t *testing.T) {
	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "test.pid")

	// Write invalid (non-numeric) content
	if err := os.WriteFile(pidFile, []byte("invalid"), 0644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	opts := ServerOptions{
		PIDFile:    pidFile,
		SocketPath: filepath.Join(tmp, "test.sock"),
	}

	running, pid := daemonRunning(opts)
	if running {
		t.Error("expected not running with invalid pid")
	}
	if pid != 0 {
		t.Errorf("expected pid 0, got %d", pid)
	}
}

func TestDaemonRunning_ZeroPID(t *testing.T) {
	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "test.pid")

	// Write zero PID
	if err := os.WriteFile(pidFile, []byte("0"), 0644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	opts := ServerOptions{
		PIDFile:    pidFile,
		SocketPath: filepath.Join(tmp, "test.sock"),
	}

	running, pid := daemonRunning(opts)
	if running {
		t.Error("expected not running with zero pid")
	}
	if pid != 0 {
		t.Errorf("expected pid 0, got %d", pid)
	}
}

func TestDaemonRunning_DeadProcess(t *testing.T) {
	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "test.pid")

	// Write a PID that doesn't exist (very high number)
	if err := os.WriteFile(pidFile, []byte("99999999"), 0644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	opts := ServerOptions{
		PIDFile:    pidFile,
		SocketPath: filepath.Join(tmp, "test.sock"),
	}

	running, pid := daemonRunning(opts)
	if running {
		t.Error("expected not running with dead process")
	}
	if pid != 0 {
		t.Errorf("expected pid 0 for dead process, got %d", pid)
	}
}

func TestDaemonRunning_LiveProcess(t *testing.T) {
	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "test.pid")

	// Write current process PID (which is alive)
	myPid := os.Getpid()
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", myPid)), 0644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	opts := ServerOptions{
		PIDFile:    pidFile,
		SocketPath: filepath.Join(tmp, "test.sock"),
	}

	running, pid := daemonRunning(opts)
	if !running {
		t.Error("expected running with live process")
	}
	if pid != myPid {
		t.Errorf("expected pid %d, got %d", myPid, pid)
	}
}

// ============== processAlive Tests ==============

func TestProcessAlive_CurrentProcess(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Error("current process should be alive")
	}
}

func TestProcessAlive_DeadProcess(t *testing.T) {
	// Very high PID that shouldn't exist
	if processAlive(99999999) {
		t.Error("non-existent process should not be alive")
	}
}

func TestProcessAlive_ZeroPID(t *testing.T) {
	// PID 0 behavior varies by OS, but signal should fail
	// This tests the edge case
	_ = processAlive(0) // Just ensure no panic
}

func TestProcessAlive_NegativePID(t *testing.T) {
	// Negative PID - os.FindProcess may not fail, but signal will
	if processAlive(-1) {
		t.Error("negative PID should not be alive")
	}
}

// ============== writePIDFile Tests ==============

func TestWritePIDFile_Success(t *testing.T) {
	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "subdir", "test.pid")

	err := writePIDFile(pidFile, 12345)
	if err != nil {
		t.Fatalf("writePIDFile failed: %v", err)
	}

	// Verify file contents
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	if string(data) != "12345\n" {
		t.Errorf("expected '12345\\n', got %q", string(data))
	}
}

func TestWritePIDFile_EmptyPath(t *testing.T) {
	err := writePIDFile("", 12345)
	if err == nil {
		t.Error("expected error for empty path")
	}
}

func TestWritePIDFile_WhitespacePath(t *testing.T) {
	err := writePIDFile("   ", 12345)
	if err == nil {
		t.Error("expected error for whitespace path")
	}
}

func TestWritePIDFile_ZeroPID(t *testing.T) {
	tmp := t.TempDir()
	err := writePIDFile(filepath.Join(tmp, "test.pid"), 0)
	if err == nil {
		t.Error("expected error for zero pid")
	}
}

func TestWritePIDFile_NegativePID(t *testing.T) {
	tmp := t.TempDir()
	err := writePIDFile(filepath.Join(tmp, "test.pid"), -1)
	if err == nil {
		t.Error("expected error for negative pid")
	}
}

// ============== readPIDFile Tests ==============

func TestReadPIDFile_Success(t *testing.T) {
	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "test.pid")

	if err := os.WriteFile(pidFile, []byte("12345\n"), 0644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	pid, err := readPIDFile(pidFile)
	if err != nil {
		t.Fatalf("readPIDFile failed: %v", err)
	}
	if pid != 12345 {
		t.Errorf("expected pid 12345, got %d", pid)
	}
}

func TestReadPIDFile_NoNewline(t *testing.T) {
	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "test.pid")

	if err := os.WriteFile(pidFile, []byte("12345"), 0644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	pid, err := readPIDFile(pidFile)
	if err != nil {
		t.Fatalf("readPIDFile failed: %v", err)
	}
	if pid != 12345 {
		t.Errorf("expected pid 12345, got %d", pid)
	}
}

func TestReadPIDFile_NonExistent(t *testing.T) {
	_, err := readPIDFile("/nonexistent/path/test.pid")
	if err == nil {
		t.Error("expected error for non-existent file")
	}
}

func TestReadPIDFileFunc_EmptyFile(t *testing.T) {
	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "test.pid")

	if err := os.WriteFile(pidFile, []byte(""), 0644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	_, err := readPIDFile(pidFile)
	if err == nil {
		t.Error("expected error for empty file")
	}
}

func TestReadPIDFile_WhitespaceOnly(t *testing.T) {
	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "test.pid")

	if err := os.WriteFile(pidFile, []byte("   \n  "), 0644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	_, err := readPIDFile(pidFile)
	if err == nil {
		t.Error("expected error for whitespace-only file")
	}
}

func TestReadPIDFile_InvalidContent(t *testing.T) {
	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "test.pid")

	if err := os.WriteFile(pidFile, []byte("not-a-number"), 0644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	_, err := readPIDFile(pidFile)
	if err == nil {
		t.Error("expected error for invalid content")
	}
}

// ============== DefaultServerOptions Tests ==============

func TestDefaultServerOptions(t *testing.T) {
	opts := DefaultServerOptions()
	if opts.SocketPath == "" {
		t.Error("expected non-empty socket path")
	}
	if opts.PIDFile == "" {
		t.Error("expected non-empty pid file")
	}
	if opts.Logger != nil {
		t.Error("expected nil logger by default")
	}
}

// ============== normalizeServerOptions Tests ==============

func TestNormalizeServerOptions_EmptyPaths(t *testing.T) {
	opts := normalizeServerOptions(ServerOptions{})
	if opts.SocketPath == "" {
		t.Error("expected non-empty socket path after normalization")
	}
	if opts.PIDFile == "" {
		t.Error("expected non-empty pid file after normalization")
	}
}

func TestNormalizeServerOptions_WhitespacePaths(t *testing.T) {
	opts := normalizeServerOptions(ServerOptions{
		SocketPath: "   ",
		PIDFile:    "  \t  ",
	})
	if opts.SocketPath == "   " {
		t.Error("expected whitespace socket path to be replaced with default")
	}
	if opts.PIDFile == "  \t  " {
		t.Error("expected whitespace pid file to be replaced with default")
	}
}

func TestNormalizeServerOptions_ValidPaths(t *testing.T) {
	opts := normalizeServerOptions(ServerOptions{
		SocketPath: "/custom/path.sock",
		PIDFile:    "/custom/daemon.pid",
	})
	if opts.SocketPath != "/custom/path.sock" {
		t.Errorf("expected socket path /custom/path.sock, got %s", opts.SocketPath)
	}
	if opts.PIDFile != "/custom/daemon.pid" {
		t.Errorf("expected pid file /custom/daemon.pid, got %s", opts.PIDFile)
	}
}

// ============== StartDaemonWithOptions Tests ==============

func TestStartDaemonWithOptions_AlreadyRunning(t *testing.T) {
	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "test.pid")
	socketPath := filepath.Join(tmp, "test.sock")

	// Write current process PID to simulate already running
	myPid := os.Getpid()
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", myPid)), 0644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	err := StartDaemonWithOptions(context.Background(), ServerOptions{
		PIDFile:    pidFile,
		SocketPath: socketPath,
	})

	if err == nil {
		t.Error("expected error when daemon already running")
	}
}

// Regression for the daemon-side gap caught while reviewing #2:
// loadDaemonCustomPatterns must merge persisted custom_patterns
// rows into the shared engine before the daemon starts serving
// classify queries. Without this, the daemon-vs-fallback paths
// would diverge: the daemon would only see builtins while the
// generated `slb_guard.py` (post-fix) sees customs.
func TestLoadDaemonCustomPatterns_MergesRowsIntoEngine(t *testing.T) {
	// Set up a project structure: /<tmp>/.slb/state.db with a
	// custom_patterns table populated with one unique row.
	projectPath := t.TempDir()
	slbDir := filepath.Join(projectPath, ".slb")
	if err := os.MkdirAll(slbDir, 0o750); err != nil {
		t.Fatalf("MkdirAll .slb: %v", err)
	}
	dbPath := filepath.Join(slbDir, "state.db")
	dbConn, err := db.OpenAndMigrate(dbPath)
	if err != nil {
		t.Fatalf("OpenAndMigrate: %v", err)
	}
	uniqPattern := `^uniq-daemon-load-test-marker-z3y7$`
	if _, err := dbConn.InsertCustomPattern("dangerous", uniqPattern, "regression test", "test"); err != nil {
		dbConn.Close()
		t.Fatalf("InsertCustomPattern: %v", err)
	}
	dbConn.Close()

	// Capture warning logs to /dev/null so they don't pollute -v output.
	logger := log.NewWithOptions(io.Discard, log.Options{Level: log.WarnLevel})
	loadDaemonCustomPatterns(projectPath, logger)

	// The engine must now contain the unique pattern in the
	// dangerous tier. Iterate via the public AllPatterns view.
	engine := core.GetDefaultEngine()
	found := false
	for _, p := range engine.AllPatterns()["dangerous"] {
		if p.Pattern == uniqPattern {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("loadDaemonCustomPatterns did not merge persisted pattern %q into engine", uniqPattern)
	}

	// Idempotency: a second call must NOT duplicate the in-memory
	// engine entry, mirroring the CLI loader's contract.
	before := len(engine.AllPatterns()["dangerous"])
	loadDaemonCustomPatterns(projectPath, logger)
	after := len(engine.AllPatterns()["dangerous"])
	if before != after {
		t.Errorf("loadDaemonCustomPatterns is not idempotent: dangerous-tier count %d -> %d", before, after)
	}
}
