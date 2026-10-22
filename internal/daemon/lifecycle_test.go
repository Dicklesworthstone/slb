package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/log"
)

func TestRunDaemon_SocketCreationFailure(t *testing.T) {
	// Create a file where the socket directory should be to force mkdir error
	tmp := t.TempDir()
	socketDir := filepath.Join(tmp, "socket")
	if err := os.WriteFile(socketDir, []byte("blocker"), 0600); err != nil {
		t.Fatal(err)
	}

	socketPath := filepath.Join(socketDir, "slb.sock")
	pidFile := filepath.Join(tmp, "slb.pid")

	logger := log.New(os.Stdout)
	ctx := context.Background()

	err := RunDaemon(ctx, ServerOptions{
		ProjectDir: tmp,
		SocketPath: socketPath,
		PIDFile:    pidFile,
		Logger:     logger,
	})

	if err == nil {
		t.Fatal("expected error when socket directory cannot be created")
	}
	if !strings.Contains(err.Error(), "creating socket directory") {
		t.Errorf("expected socket directory creation error, got: %v", err)
	}
}

func TestRunDaemon_IPCServerFailure(t *testing.T) {
	tmp := t.TempDir()
	// Create a directory where the socket should be to force listen error
	// (net.Listen("unix", dir) usually fails)
	socketPath := filepath.Join(tmp, "slb.sock")
	if err := os.MkdirAll(socketPath, 0700); err != nil {
		t.Fatal(err)
	}

	pidFile := filepath.Join(tmp, "slb.pid")

	logger := log.New(os.Stdout)
	ctx := context.Background()

	err := RunDaemon(ctx, ServerOptions{
		ProjectDir: tmp,
		SocketPath: socketPath,
		PIDFile:    pidFile,
		Logger:     logger,
	})

	if err == nil {
		t.Error("expected error when ipc server creation fails")
	}
}

func TestStopDaemon_RefusesUnrelatedProcess(t *testing.T) {
	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "test.pid")

	// A live PID alone is not proof that this process owns the project socket.
	cmd := exec.Command("sleep", "10")
	if err := cmd.Start(); err != nil {
		t.Skipf("skipping test, cannot start sleep: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	if err := writePIDFile(pidFile, cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}

	err := StopDaemonWithOptions(ServerOptions{
		PIDFile:    pidFile,
		SocketPath: filepath.Join(tmp, "absent.sock"),
	}, 10*time.Millisecond)

	if err == nil {
		t.Fatal("stop accepted an unrelated process with no project socket")
	}
	if !processAlive(cmd.Process.Pid) {
		t.Fatal("stop terminated the unrelated process")
	}
}

func TestStopDaemon_NoPIDFile(t *testing.T) {
	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "nonexistent.pid")

	err := StopDaemonWithOptions(ServerOptions{
		PIDFile:    pidFile,
		SocketPath: filepath.Join(tmp, "absent.sock"),
	}, 1*time.Second)

	if err == nil {
		t.Error("expected error when pid file missing")
	}
}

func TestStartDaemon_DaemonMode(t *testing.T) {
	// Simulate daemon mode enabled
	t.Setenv("SLB_DAEMON_MODE", "1")

	// We can't actually run RunDaemon fully without blocking or setting up complex context cancellation
	// But we can verify it calls RunDaemon by checking if it tries to use the socket path
	// or by using a mock logger/options.

	// Using a socket path that will fail quickly is a good way to verify it entered RunDaemon
	tmp := t.TempDir()
	socketDir := filepath.Join(tmp, "socket")
	if err := os.WriteFile(socketDir, []byte("blocker"), 0600); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(socketDir, "slb.sock")
	pidFile := filepath.Join(tmp, "slb.pid")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := StartDaemonWithOptions(ctx, ServerOptions{
		ProjectDir: tmp,
		SocketPath: socketPath,
		PIDFile:    pidFile,
	})

	// Should fail with directory creation error from RunDaemon
	if err == nil {
		t.Fatal("expected error from RunDaemon")
	}
	if !strings.Contains(err.Error(), "creating socket directory") {
		t.Errorf("expected RunDaemon execution error, got: %v", err)
	}
}

// TestDaemonLifecycleProcessHelper runs only in subprocesses created below.
// A separate process gives each daemon its own signal handlers and PID, and
// lets these tests exercise the public lifecycle APIs without signalling the
// test runner or changing its working directory.
func TestDaemonLifecycleProcessHelper(t *testing.T) {
	project := os.Getenv("SLB_TEST_LIFECYCLE_PROJECT")
	if project == "" {
		return
	}
	if os.Getenv("SLB_TEST_LIFECYCLE_GATE") == "1" {
		if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
			t.Fatal(err)
		}
	}
	if err := RunDaemon(context.Background(), ServerOptions{
		ProjectDir: project,
		SocketPath: os.Getenv("SLB_TEST_LIFECYCLE_SOCKET"),
		PIDFile:    os.Getenv("SLB_TEST_LIFECYCLE_PID"),
		Logger:     log.New(os.Stderr),
	}); err != nil {
		t.Fatal(err)
	}
}

type lifecycleChild struct {
	cmd     *exec.Cmd
	opts    ServerOptions
	done    chan struct{}
	err     error
	logPath string
	gate    io.Closer
}

func lifecycleEnvironment(t *testing.T) {
	t.Helper()
	tmp, err := filepath.Abs(shortSocketDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	t.Setenv("SLB_HOST", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

func lifecycleProject(t *testing.T) string {
	t.Helper()
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, ".slb"), 0700); err != nil {
		t.Fatal(err)
	}
	config := "[daemon]\nuse_file_watcher = false\ntcp_addr = ''\n[notifications]\ndesktop_enabled = false\n"
	if err := os.WriteFile(filepath.Join(project, ".slb", "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	return project
}

func startLifecycleChild(t *testing.T, opts ServerOptions, gated bool) *lifecycleChild {
	t.Helper()
	logFile, err := os.CreateTemp(t.TempDir(), "daemon.log-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logFile.Close() })
	cmd := exec.Command(os.Args[0], "-test.run=^TestDaemonLifecycleProcessHelper$")
	cmd.Env = append(os.Environ(),
		"SLB_TEST_LIFECYCLE_PROJECT="+opts.ProjectDir,
		"SLB_TEST_LIFECYCLE_SOCKET="+opts.SocketPath,
		"SLB_TEST_LIFECYCLE_PID="+opts.PIDFile,
		"SLB_TEST_LIFECYCLE_GATE=0",
	)
	child := &lifecycleChild{cmd: cmd, opts: normalizeServerOptions(opts), done: make(chan struct{}), logPath: logFile.Name()}
	if gated {
		cmd.Env = append(cmd.Env, "SLB_TEST_LIFECYCLE_GATE=1")
		child.gate, err = cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		child.err = cmd.Wait()
		close(child.done)
	}()
	t.Cleanup(func() {
		if child.gate != nil {
			_ = child.gate.Close()
		}
		select {
		case <-child.done:
			return
		default:
			_ = cmd.Process.Kill()
		}
		select {
		case <-child.done:
		case <-time.After(10 * time.Second):
			t.Error("daemon helper did not exit after cleanup")
		}
	})
	return child
}

func (c *lifecycleChild) logs() string {
	data, _ := os.ReadFile(c.logPath)
	return string(data)
}

func (c *lifecycleChild) wait(t *testing.T, success bool) {
	t.Helper()
	select {
	case <-c.done:
		if (c.err == nil) != success {
			t.Fatalf("daemon exit=%v, want success=%t\n%s", c.err, success, c.logs())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("daemon did not exit\n%s", c.logs())
	}
}

func (c *lifecycleChild) ready(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-c.done:
			t.Fatalf("daemon exited before readiness: %v\n%s", c.err, c.logs())
		default:
		}
		pid, err := readPIDFile(c.opts.PIDFile)
		if err == nil && pid == c.cmd.Process.Pid {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			err = pingDaemonUnix(ctx, c.opts.SocketPath)
			cancel()
			if err == nil {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("daemon did not become ready\n%s", c.logs())
}

func (c *lifecycleChild) assertRunning(t *testing.T) {
	t.Helper()
	select {
	case <-c.done:
		t.Fatalf("daemon unexpectedly exited: %v\n%s", c.err, c.logs())
	default:
	}
	info := NewClient(WithSocketPath(c.opts.SocketPath), WithPIDFile(c.opts.PIDFile)).GetStatusInfo()
	if info.Status != DaemonRunning || info.PID != c.cmd.Process.Pid || !info.SocketAlive {
		t.Fatalf("wrong project daemon status: %+v\n%s", info, c.logs())
	}
}

func TestDaemonLifecycle_IndependentProjects(t *testing.T) {
	lifecycleEnvironment(t)
	projectA, projectB, projectC := lifecycleProject(t), lifecycleProject(t), lifecycleProject(t)
	a := startLifecycleChild(t, ServerOptions{ProjectDir: projectA}, false)
	a.ready(t)
	b := startLifecycleChild(t, ServerOptions{ProjectDir: projectB}, false)
	b.ready(t)
	if a.opts.PIDFile == b.opts.PIDFile || a.opts.SocketPath == b.opts.SocketPath {
		t.Fatal("independent projects share lifecycle resources")
	}
	a.assertRunning(t)
	b.assertRunning(t)
	// Reject an invalid explicit path before ancestor discovery can turn it
	// into a request to stop A. The same validation applies to both starts.
	filePath := filepath.Join(projectA, "not-a-directory")
	if err := os.WriteFile(filePath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, project := range []string{filepath.Join(projectA, "missing"), filePath} {
		opts := ServerOptions{ProjectDir: project}
		if err := StopDaemonWithOptions(opts, time.Second); err == nil {
			t.Fatalf("stop accepted invalid project directory %s", project)
		}
		if err := StartDaemonWithOptions(context.Background(), opts); err == nil {
			t.Fatalf("start accepted invalid project directory %s", project)
		}
		if err := RunDaemon(context.Background(), opts); err == nil {
			t.Fatalf("foreground start accepted invalid project directory %s", project)
		}
	}
	a.assertRunning(t)
	b.assertRunning(t)
	unused := normalizeServerOptions(ServerOptions{ProjectDir: projectC})
	info := NewClient(WithSocketPath(unused.SocketPath), WithPIDFile(unused.PIDFile)).GetStatusInfo()
	if info.Status != DaemonNotRunning || info.PID != 0 || info.SocketAlive {
		t.Fatalf("never-started project reports another project's daemon: %+v", info)
	}
	if err := StopDaemonWithOptions(unused, time.Second); err == nil {
		t.Fatal("stop succeeded for a project with no daemon")
	}
	a.assertRunning(t)
	b.assertRunning(t)
	if err := StopDaemonWithOptions(ServerOptions{ProjectDir: projectA}, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	a.wait(t, true)
	b.assertRunning(t)
	for _, path := range []string{a.opts.PIDFile, a.opts.SocketPath} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("stopped project left resource %s: %v", path, err)
		}
	}
	restarted := startLifecycleChild(t, ServerOptions{ProjectDir: projectA}, false)
	restarted.ready(t)
	if err := StopDaemonWithOptions(ServerOptions{ProjectDir: projectB}, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	b.wait(t, true)
	restarted.assertRunning(t)
	if err := StopDaemonWithOptions(ServerOptions{ProjectDir: projectA}, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	restarted.wait(t, true)
}

func TestDaemonLifecycle_RejectsCrossProjectPID(t *testing.T) {
	lifecycleEnvironment(t)
	a := startLifecycleChild(t, ServerOptions{ProjectDir: lifecycleProject(t)}, false)
	b := startLifecycleChild(t, ServerOptions{ProjectDir: lifecycleProject(t)}, false)
	a.ready(t)
	b.ready(t)
	misdirected := a.opts
	misdirected.SocketPath = b.opts.SocketPath
	misdirected.PIDFile = b.opts.PIDFile
	if err := StopDaemonWithOptions(misdirected, time.Second); err == nil {
		t.Fatal("stop accepted another project's socket/PID pair")
	}
	a.assertRunning(t)
	b.assertRunning(t)
	if err := os.WriteFile(a.opts.PIDFile, []byte(fmt.Sprintf("%d\n", b.cmd.Process.Pid)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := StopDaemonWithOptions(a.opts, time.Second); err == nil {
		t.Fatal("stop accepted a PID belonging to another project")
	}
	b.assertRunning(t)
	// Restore the first daemon's record before verifying its status and stop.
	if err := os.WriteFile(a.opts.PIDFile, []byte(fmt.Sprintf("%d\n", a.cmd.Process.Pid)), 0600); err != nil {
		t.Fatal(err)
	}
	a.assertRunning(t)
	if err := StopDaemonWithOptions(a.opts, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	a.wait(t, true)
	b.assertRunning(t)
}

func TestDaemonLifecycle_SimultaneousStarts(t *testing.T) {
	lifecycleEnvironment(t)
	opts := ServerOptions{ProjectDir: lifecycleProject(t)}
	children := make([]*lifecycleChild, 4)
	for i := range children {
		children[i] = startLifecycleChild(t, opts, true)
	}
	for _, child := range children {
		if err := child.gate.Close(); err != nil {
			t.Fatal(err)
		}
	}
	var winner *lifecycleChild
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		pid, err := readPIDFile(children[0].opts.PIDFile)
		if err == nil {
			for _, child := range children {
				if child.cmd.Process.Pid == pid {
					winner = child
					break
				}
			}
		}
		if winner != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if winner == nil {
		for _, child := range children {
			t.Log(child.logs())
		}
		t.Fatal("no simultaneous starter published its PID")
	}
	winner.ready(t)
	for _, child := range children {
		if child != winner {
			child.wait(t, false)
		}
	}
	winner.assertRunning(t)
	if err := StopDaemonWithOptions(opts, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	winner.wait(t, true)
}

func TestDaemonLifecycle_StalePIDRecovery(t *testing.T) {
	lifecycleEnvironment(t)
	for _, stale := range []string{"99999999\n", "invalid\n", "", "0\n", "-1\n"} {
		t.Run(fmt.Sprintf("pid_%q", stale), func(t *testing.T) {
			opts := ServerOptions{ProjectDir: lifecycleProject(t)}
			pidPath := PIDFileForCWD(opts.ProjectDir)
			if err := os.WriteFile(pidPath, []byte(stale), 0600); err != nil {
				t.Fatal(err)
			}
			child := startLifecycleChild(t, opts, false)
			child.ready(t)
			if err := StopDaemonWithOptions(opts, 5*time.Second); err != nil {
				t.Fatal(err)
			}
			child.wait(t, true)
		})
	}
}

func TestDaemonLifecycle_PreservesReplacedPID(t *testing.T) {
	lifecycleEnvironment(t)
	for _, replaceInode := range []bool{false, true} {
		t.Run(fmt.Sprintf("replace_inode_%t", replaceInode), func(t *testing.T) {
			child := startLifecycleChild(t, ServerOptions{ProjectDir: lifecycleProject(t)}, false)
			child.ready(t)
			contents := "99999999\n"
			if replaceInode {
				if err := os.Rename(child.opts.PIDFile, child.opts.PIDFile+".previous"); err != nil {
					t.Fatal(err)
				}
				// Even a replacement containing the same PID is a different generation.
				contents = fmt.Sprintf("%d\n", child.cmd.Process.Pid)
			}
			if err := os.WriteFile(child.opts.PIDFile, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if err := child.cmd.Process.Signal(os.Interrupt); err != nil {
				t.Fatal(err)
			}
			child.wait(t, true)
			data, err := os.ReadFile(child.opts.PIDFile)
			if err != nil || string(data) != contents {
				t.Fatalf("exiting daemon removed another owner's PID record: %q, %v", data, err)
			}
		})
	}
}

func TestDaemonLifecycle_RecoversMissingPIDAndCrash(t *testing.T) {
	lifecycleEnvironment(t)
	opts := ServerOptions{ProjectDir: lifecycleProject(t)}
	child := startLifecycleChild(t, opts, false)
	child.ready(t)
	if err := os.Rename(child.opts.PIDFile, child.opts.PIDFile+".missing"); err != nil {
		t.Fatal(err)
	}
	if err := StopDaemonWithOptions(opts, 5*time.Second); err != nil {
		t.Fatalf("could not stop a responsive project whose PID file was missing: %v", err)
	}
	child.wait(t, true)
	crashed := startLifecycleChild(t, opts, false)
	crashed.ready(t)
	if err := crashed.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	crashed.wait(t, false)
	restarted := startLifecycleChild(t, opts, false)
	restarted.ready(t)
	if err := StopDaemonWithOptions(opts, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	restarted.wait(t, true)
}

func TestDaemonLifecycle_MissingPIDStopRecognizesReplacement(t *testing.T) {
	lifecycleEnvironment(t)
	opts := normalizeServerOptions(ServerOptions{ProjectDir: lifecycleProject(t)})
	retiringLease, err := acquireFileLease(opts.PIDFile + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer retiringLease.Close()
	retiring, cleanupRetiring, err := listenOwnedUnix(opts.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupRetiring()
	if err := retiring.(*net.UnixListener).SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}

	// The retiring daemon has lost its public PID file. Hold its shutdown
	// acknowledgment until a replacement owns the project's resources. From
	// the caller's perspective this is the same as being descheduled while an
	// acknowledgment sits in its socket buffer and a new daemon starts.
	stopDone := make(chan struct{})
	var stopErr error
	go func() {
		stopErr = StopDaemonWithOptions(opts, 30*time.Second)
		close(stopDone)
	}()
	t.Cleanup(func() {
		// Registered before the child cleanup below, so the replacement's
		// lease is always released before joining a failed stop assertion.
		_ = cleanupRetiring()
		_ = retiringLease.Close()
		select {
		case <-stopDone:
		case <-time.After(5 * time.Second):
			t.Error("stop goroutine did not exit after resource cleanup")
		}
	})
	conn, err := retiring.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var request RPCRequest
	if err := json.NewDecoder(conn).Decode(&request); err != nil {
		t.Fatal(err)
	}
	var params struct {
		PID         int    `json:"pid"`
		ProjectPath string `json:"project_path"`
	}
	if err := json.Unmarshal(request.Params, &params); err != nil {
		t.Fatal(err)
	}
	if request.Method != "shutdown" || params.PID != 0 || params.ProjectPath != opts.ProjectDir {
		t.Fatalf("unexpected missing-PID shutdown request: %+v, params=%+v", request, params)
	}
	if err := cleanupRetiring(); err != nil {
		t.Fatal(err)
	}
	if err := retiringLease.Close(); err != nil {
		t.Fatal(err)
	}
	replacement := startLifecycleChild(t, opts, false)
	replacement.ready(t)
	if err := json.NewEncoder(conn).Encode(RPCResponse{
		ID: request.ID, Result: map[string]int{"pid": os.Getpid()},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopDone:
		if stopErr != nil {
			t.Fatalf("retired generation's stop failed after replacement startup: %v", stopErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stop is waiting for the replacement daemon instead of the retired generation")
	}
	replacement.assertRunning(t)
	if err := StopDaemonWithOptions(opts, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	replacement.wait(t, true)
}

func TestDaemonLifecycle_CustomResourceConflicts(t *testing.T) {
	lifecycleEnvironment(t)
	for _, shared := range []string{"pid", "socket"} {
		t.Run(shared, func(t *testing.T) {
			owner := startLifecycleChild(t, ServerOptions{ProjectDir: lifecycleProject(t)}, false)
			owner.ready(t)
			contenderOpts := ServerOptions{ProjectDir: lifecycleProject(t)}
			if shared == "pid" {
				contenderOpts.PIDFile = owner.opts.PIDFile
			} else {
				contenderOpts.SocketPath = owner.opts.SocketPath
			}
			contender := startLifecycleChild(t, contenderOpts, false)
			contender.wait(t, false)
			owner.assertRunning(t)
			if err := StopDaemonWithOptions(owner.opts, 5*time.Second); err != nil {
				t.Fatal(err)
			}
			owner.wait(t, true)
		})
	}
}

func TestRunDaemon_WithTCP(t *testing.T) {
	// Create project directory
	tmp := t.TempDir()
	slbDir := filepath.Join(tmp, ".slb")
	if err := os.MkdirAll(slbDir, 0700); err != nil {
		t.Fatal(err)
	}

	// Create config.toml enabling TCP
	configPath := filepath.Join(slbDir, "config.toml")
	configContent := `
[daemon]
tcp_addr = "127.0.0.1:0"
tcp_require_auth = false
`
	if err := os.WriteFile(configPath, []byte(configContent), 0600); err != nil {
		t.Fatal(err)
	}

	socketPath := filepath.Join(shortSocketDir(t), "slb.sock")
	pidFile := filepath.Join(slbDir, "slb.pid")
	logger := log.New(io.Discard)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start daemon in background
	done := make(chan struct{})
	var daemonErr error
	go func() {
		daemonErr = RunDaemon(ctx, ServerOptions{
			ProjectDir: tmp,
			SocketPath: socketPath,
			PIDFile:    pidFile,
			Logger:     logger,
		})
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop during cleanup")
		}
	})

	// Publishing a PID precedes listener startup. Readiness requires a real
	// ping, rather than mere existence of either filesystem resource.
	deadline := time.Now().Add(2 * time.Second)
	started := false
	for time.Now().Before(deadline) {
		select {
		case <-done:
			t.Fatalf("daemon exited before becoming ready: %v", daemonErr)
		default:
		}
		pingCtx, stopPing := context.WithTimeout(ctx, 100*time.Millisecond)
		err := pingDaemonUnix(pingCtx, socketPath)
		stopPing()
		if err == nil {
			started = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !started {
		t.Fatal("timed out waiting for daemon to start")
	}

	// Verify socket exists
	if _, err := os.Stat(socketPath); err != nil {
		t.Errorf("socket not created: %v", err)
	}

	// Shutdown
	cancel()
	select {
	case <-done:
		if daemonErr != nil {
			t.Errorf("RunDaemon exited with error: %v", daemonErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for RunDaemon to exit")
	}
}
