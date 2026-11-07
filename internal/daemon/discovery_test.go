//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/log"
)

func discoveryProject(t *testing.T) string {
	t.Helper()
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(project, ".slb"), 0700); err != nil {
		t.Fatal(err)
	}
	return project
}

func discoveryFixture(project, socket string) RuntimePaths {
	return RuntimePaths{
		Version: 1, ProjectPath: project, SocketPath: socket,
		PIDFile: filepath.Join(filepath.Dir(socket), "daemon.pid"), PID: os.Getpid(),
	}
}

func writeDiscoveryFixture(t *testing.T, project string, record RuntimePaths) {
	t.Helper()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".slb", "daemon.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func makeDiscoveryFixtureForeignOwned(t *testing.T, path string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("changing ownership requires root")
	}
	if err := os.Chown(path, 1, -1); err != nil {
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOSYS) {
			t.Skipf("environment cannot assign a foreign owner: %v", err)
		}
		t.Fatal(err)
	}
}

func startDiscoveryDaemon(t *testing.T, project, socket, pidFile string) ServerOptions {
	t.Helper()
	t.Setenv("SLB_HOST", "")
	server, err := NewIPCServer(socket, log.New(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	// Use the real daemon's status RPC and listener. RunDaemon integration
	// tests exercise the DB and process lifecycle separately.
	server.projectPath = project
	server.shutdown = func() {}
	server.stateReady = true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := server.Stop(); err != nil {
			t.Error(err)
		}
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	client := NewIPCClient(socket)
	readyCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	defer func() { _ = client.Close() }()
	if err := client.Ping(readyCtx); err != nil {
		t.Fatalf("starting fixture daemon: %v", err)
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return ServerOptions{ProjectDir: project, SocketPath: socket, PIDFile: pidFile}
}

func TestDiscoverRuntimeAcrossDifferentTMPDIRs(t *testing.T) {
	t.Setenv("SLB_HOST", "")
	project := discoveryProject(t)
	subdir := filepath.Join(project, "agent", "work")
	if err := os.MkdirAll(subdir, 0700); err != nil {
		t.Fatal(err)
	}
	// A user service and an agent can have distinct TMPDIR values while
	// sharing the project and the filesystem namespace (GH #33).
	serviceTemp := filepath.Join(shortSocketDir(t), "service")
	if err := os.Mkdir(serviceTemp, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", serviceTemp)
	opts := startDiscoveryDaemon(t, project, SocketPathForCWD(project), PIDFileForCWD(project))
	cleanup, err := publishRuntime(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Error(err)
		}
	})

	agentTemp := filepath.Join(t.TempDir(), strings.Repeat("agent-temp-", 12))
	if err := os.Mkdir(agentTemp, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", agentTemp)
	if SocketPathForCWD(subdir) == opts.SocketPath {
		t.Fatal("test did not produce differing environment-derived socket paths")
	}
	runtime, err := DiscoverRuntime(subdir)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.ProjectPath != project || runtime.SocketPath != opts.SocketPath || runtime.PIDFile != opts.PIDFile || runtime.PID != os.Getpid() {
		t.Fatalf("discovered wrong daemon: %+v", runtime)
	}
	client := NewProjectIPCClient(subdir)
	defer func() { _ = client.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		t.Fatalf("agent could not reach service across TMPDIR mismatch: %v", err)
	}
	if client.SocketPath() != opts.SocketPath {
		t.Fatalf("diagnostic socket = %q, want %q", client.SocketPath(), opts.SocketPath)
	}
}

func TestDiscoverRuntimeMissingRecordKeepsExactProjectDefaults(t *testing.T) {
	project := discoveryProject(t)
	t.Setenv("TMPDIR", shortSocketDir(t))
	got, err := DiscoverRuntime(project)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProjectPath != project || got.SocketPath != SocketPathForCWD(project) || got.PIDFile != PIDFileForCWD(project) || got.PID != 0 {
		t.Fatalf("missing-record discovery changed project identity: %+v", got)
	}
}

func TestProjectClientsRefreshPublishedRuntime(t *testing.T) {
	t.Setenv("SLB_HOST", "")
	for _, initial := range []string{"missing", "invalid", "previous_daemon"} {
		t.Run(initial, func(t *testing.T) {
			project := discoveryProject(t)
			dir := shortSocketDir(t)
			old := discoveryFixture(project, filepath.Join(dir, "old.sock"))
			old.PID++
			if initial == "invalid" {
				old.ProjectPath = filepath.Dir(project)
			}
			if initial != "missing" {
				writeDiscoveryFixture(t, project, old)
			}
			ipc := NewProjectIPCClient(project)
			defer func() { _ = ipc.Close() }()
			client := NewProjectClient(project, WithLocalOnly())
			if err := ipc.Connect(context.Background()); err == nil {
				t.Fatal("connected before a daemon was published")
			}

			// A service can publish or replace its record after clients have
			// already failed. Each attempt must select that current generation.
			for _, name := range []string{"published", "replacement"} {
				record := discoveryFixture(project, filepath.Join(dir, name+".sock"))
				record.PIDFile = filepath.Join(dir, name+".pid")
				writeDiscoveryFixture(t, project, record)
				if err := os.WriteFile(record.PIDFile, []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
					t.Fatal(err)
				}
				if err := ipc.Connect(context.Background()); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s: did not attempt the freshly published missing socket: %v", name, err)
				}
				if ipc.SocketPath() != record.SocketPath {
					t.Fatalf("%s: socket diagnostic is stale: %q", name, ipc.SocketPath())
				}
				info := client.GetStatusInfo()
				if info.SocketPath != record.SocketPath || info.PIDFile != record.PIDFile || info.PID != record.PID || info.Status != DaemonUnresponsive {
					t.Fatalf("%s: stale status snapshot: %+v", name, info)
				}
				if client.GetStatus() != DaemonUnresponsive {
					t.Fatalf("%s: status did not recover from initial discovery", name)
				}
				if err := ipc.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestProjectClientsRejectInvalidReplacementAndRecover(t *testing.T) {
	t.Setenv("SLB_HOST", "")
	project := discoveryProject(t)
	record := discoveryFixture(project, filepath.Join(shortSocketDir(t), "missing.sock"))
	writeDiscoveryFixture(t, project, record)
	ipc := NewProjectIPCClient(project)
	defer func() { _ = ipc.Close() }()
	client := NewProjectClient(project, WithLocalOnly())
	invalid := record
	invalid.ProjectPath = filepath.Dir(project)
	writeDiscoveryFixture(t, project, invalid)
	if err := ipc.Connect(context.Background()); !errors.Is(err, ErrDiscoveryInvalid) {
		t.Fatalf("reused the previous record after invalid replacement: %v", err)
	}
	info := client.GetStatusInfo()
	if info.Status != DaemonNotRunning || !strings.Contains(info.Message, "discovery is invalid") {
		t.Fatalf("status accepted invalid replacement: %+v", info)
	}
	writeDiscoveryFixture(t, project, record)
	if err := ipc.Connect(context.Background()); !errors.Is(err, os.ErrNotExist) || ipc.SocketPath() != record.SocketPath {
		t.Fatalf("repaired record did not recover: socket=%q err=%v", ipc.SocketPath(), err)
	}
	if info := client.GetStatusInfo(); info.SocketPath != record.SocketPath || strings.Contains(info.Message, "discovery is invalid") {
		t.Fatalf("status retained a repaired discovery error: %+v", info)
	}
}

func TestProjectClientsRefuseProjectRetargeting(t *testing.T) {
	t.Setenv("SLB_HOST", "")
	for _, change := range []string{"marker_removed", "physical_path_redirected"} {
		t.Run(change, func(t *testing.T) {
			parent := discoveryProject(t)
			project := filepath.Join(parent, "nested")
			if err := os.MkdirAll(filepath.Join(project, ".slb"), 0700); err != nil {
				t.Fatal(err)
			}
			record := discoveryFixture(project, filepath.Join(shortSocketDir(t), "original.sock"))
			writeDiscoveryFixture(t, project, record)
			writeDiscoveryFixture(t, parent, discoveryFixture(parent, filepath.Join(shortSocketDir(t), "parent.sock")))
			ipc := NewProjectIPCClient(project)
			defer func() { _ = ipc.Close() }()
			client := NewProjectClient(project, WithLocalOnly())
			if change == "marker_removed" {
				if err := os.Rename(filepath.Join(project, ".slb"), filepath.Join(project, ".slb.saved")); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Rename(project, project+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(parent, project); err != nil {
					t.Fatal(err)
				}
			}
			if err := ipc.Connect(context.Background()); !errors.Is(err, ErrDiscoveryInvalid) {
				t.Fatalf("client followed a different project after %s: %v", change, err)
			}
			if ipc.SocketPath() != record.SocketPath {
				t.Fatalf("diagnostics switched to another project's socket: %q", ipc.SocketPath())
			}
			if info := client.GetStatusInfo(); info.SocketPath != record.SocketPath || info.Status != DaemonNotRunning {
				t.Fatalf("status followed a different project: %+v", info)
			}
		})
	}
}

func TestProjectClientsPinPhysicalProjectAcrossAliasChanges(t *testing.T) {
	t.Setenv("SLB_HOST", "")
	project, other := discoveryProject(t), discoveryProject(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(project, alias); err != nil {
		t.Fatal(err)
	}
	ipc := NewProjectIPCClient(alias)
	defer func() { _ = ipc.Close() }()
	client := NewProjectClient(alias, WithLocalOnly())
	if err := os.Rename(alias, alias+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, alias); err != nil {
		t.Fatal(err)
	}
	record := discoveryFixture(project, filepath.Join(shortSocketDir(t), "original-project.sock"))
	writeDiscoveryFixture(t, project, record)
	if err := ipc.Connect(context.Background()); !errors.Is(err, os.ErrNotExist) || ipc.SocketPath() != record.SocketPath {
		t.Fatalf("client followed changed caller alias: socket=%q err=%v", ipc.SocketPath(), err)
	}
	if info := client.GetStatusInfo(); info.SocketPath != record.SocketPath || info.PIDFile != record.PIDFile {
		t.Fatalf("status followed changed caller alias: %+v", info)
	}
}

func TestProjectClientRefreshPreservesExplicitPaths(t *testing.T) {
	t.Setenv("SLB_HOST", "")
	project := discoveryProject(t)
	dir := shortSocketDir(t)
	explicitSocket, explicitPID := filepath.Join(dir, "explicit.sock"), filepath.Join(dir, "explicit.pid")
	client := NewProjectClient(project, WithSocketPath(explicitSocket), WithPIDFile(explicitPID), WithLocalOnly())
	pidOverride := NewProjectClient(project, WithPIDFile(explicitPID), WithLocalOnly())
	record := discoveryFixture(project, filepath.Join(dir, "published.sock"))
	writeDiscoveryFixture(t, project, record)
	if info := client.GetStatusInfo(); info.SocketPath != explicitSocket || info.PIDFile != explicitPID {
		t.Fatalf("project discovery replaced explicit transport: %+v", info)
	}
	if info := pidOverride.GetStatusInfo(); info.SocketPath != record.SocketPath || info.PIDFile != explicitPID {
		t.Fatalf("refresh lost explicit PID override or retained stale socket: %+v", info)
	}
}

func TestProjectIPCClientKeepsConnectedGenerationAndNeverReplays(t *testing.T) {
	t.Setenv("SLB_HOST", "")
	project := discoveryProject(t)
	record := discoveryFixture(project, filepath.Join(shortSocketDir(t), "original.sock"))
	writeDiscoveryFixture(t, project, record)
	client := NewProjectIPCClient(project)
	local, peer := net.Pipe()
	client.conn, client.scanner, client.connDone = local, bufio.NewScanner(local), make(chan struct{})
	t.Cleanup(func() { _ = client.Close(); _ = peer.Close() })
	invalid := record
	invalid.ProjectPath = filepath.Dir(project)
	writeDiscoveryFixture(t, project, invalid)
	done := make(chan error, 1)
	go func() { done <- client.Ping(context.Background()) }()
	req := receiveIPCRequest(t, peer)
	if req.Method != "ping" {
		t.Fatalf("established connection unexpectedly rediscovered: %+v", req)
	}
	if err := json.NewEncoder(peer).Encode(RPCResponse{ID: req.ID, Result: map[string]bool{"pong": true}}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil || client.SocketPath() != record.SocketPath {
		t.Fatalf("record replacement disrupted established connection: %v", err)
	}
	go func() { _, err := client.request(context.Background(), "claim_execution", nil, true); done <- err }()
	req = receiveIPCRequest(t, peer)
	if req.Method != "claim_execution" {
		t.Fatalf("unexpected RPC: %+v", req)
	}
	// The daemon may have committed the claim before its reply was lost. The
	// original operation must report that loss without rediscovery or replay.
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || errors.Is(err, ErrDiscoveryInvalid) {
		t.Fatalf("failed exchange was retried through invalid replacement: %v", err)
	}
	if err := client.Connect(context.Background()); !errors.Is(err, ErrDiscoveryInvalid) {
		t.Fatalf("subsequent connection did not refresh discovery: %v", err)
	}
}

func TestProjectClientsConcurrentRefreshDiagnostics(t *testing.T) {
	t.Setenv("SLB_HOST", "")
	project := discoveryProject(t)
	ipc := NewProjectIPCClient(project)
	defer func() { _ = ipc.Close() }()
	client := NewProjectClient(project, WithLocalOnly())
	record := discoveryFixture(project, filepath.Join(shortSocketDir(t), "missing.sock"))
	writeDiscoveryFixture(t, project, record)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				_ = ipc.SocketPath()
				if err := ipc.Connect(context.Background()); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("refresh missing endpoint: %v", err)
				}
				if info := client.GetStatusInfo(); info.SocketPath != record.SocketPath || info.PIDFile != record.PIDFile {
					t.Errorf("mixed status snapshot: %+v", info)
				}
			}
		}()
	}
	wg.Wait()
}

func TestProjectIPCClientReconnectsToReplacementDaemon(t *testing.T) {
	lifecycleEnvironment(t)
	for _, placement := range []string{"same_path", "new_TMPDIR"} {
		t.Run(placement, func(t *testing.T) {
			project := lifecycleProject(t)
			t.Setenv("TMPDIR", shortSocketDir(t))
			opts := normalizeServerOptions(ServerOptions{ProjectDir: project})
			original := startLifecycleChild(t, opts, false)
			original.ready(t)
			awaitRecord := func(child *lifecycleChild) {
				t.Helper()
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					paths, err := DiscoverRuntime(project)
					if err == nil && paths.PID == child.cmd.Process.Pid {
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
				t.Fatalf("daemon did not publish its runtime: %s", child.logs())
			}
			awaitRecord(original)
			ipc := NewProjectIPCClient(project)
			defer func() { _ = ipc.Close() }()
			client := NewProjectClient(project, WithLocalOnly())
			if err := ipc.Ping(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := ipc.Close(); err != nil {
				t.Fatal(err)
			}
			if err := StopDaemonWithOptions(opts, 5*time.Second); err != nil {
				t.Fatal(err)
			}
			original.wait(t, true)
			if placement == "new_TMPDIR" {
				t.Setenv("TMPDIR", shortSocketDir(t))
				opts = normalizeServerOptions(ServerOptions{ProjectDir: project})
			}
			replacement := startLifecycleChild(t, opts, false)
			replacement.ready(t)
			awaitRecord(replacement)
			if replacement.cmd.Process.Pid == original.cmd.Process.Pid {
				t.Fatal("test did not produce a different kernel peer PID")
			}
			// An agent can still inherit a third temporary directory.
			t.Setenv("TMPDIR", shortSocketDir(t))
			if err := ipc.Ping(context.Background()); err != nil {
				t.Fatalf("failed to verify replacement process: %v", err)
			}
			if ipc.SocketPath() != opts.SocketPath {
				t.Fatalf("replacement diagnostic path is stale: %q", ipc.SocketPath())
			}
			if info := client.GetStatusInfo(); info.Status != DaemonRunning || info.PID != replacement.cmd.Process.Pid || info.SocketPath != opts.SocketPath {
				t.Fatalf("status client retained old daemon identity: %+v", info)
			}
			if err := StopDaemonWithOptions(opts, 5*time.Second); err != nil {
				t.Fatal(err)
			}
			replacement.wait(t, true)
		})
	}
}

func TestDiscoverRuntimeRejectsInvalidRecords(t *testing.T) {
	for _, kind := range []string{
		"malformed", "null", "array", "missing_fields", "wrong_project", "relative_project",
		"relative_socket", "socket_traversal", "socket_nul", "socket_control", "socket_too_long",
		"socket_multibyte_too_long", "relative_pid_file", "pid_traversal", "pid_nul",
		"zero_pid", "negative_pid", "boolean_pid", "unknown_version", "unknown_field",
		"duplicate_field", "trailing_json", "trailing_garbage", "invalid_utf8", "oversized",
	} {
		t.Run(kind, func(t *testing.T) {
			project := discoveryProject(t)
			record := discoveryFixture(project, filepath.Join(shortSocketDir(t), "daemon.sock"))
			switch kind {
			case "wrong_project":
				record.ProjectPath = filepath.Dir(project)
			case "relative_project":
				record.ProjectPath = "."
			case "relative_socket":
				record.SocketPath = "daemon.sock"
			case "socket_traversal":
				record.SocketPath = filepath.Dir(record.SocketPath) + "/sub/../daemon.sock"
			case "socket_nul":
				record.SocketPath += "\x00suffix"
			case "socket_control":
				record.SocketPath += "\npretend diagnostic"
			case "socket_too_long":
				record.SocketPath = "/" + strings.Repeat("a", 103)
			case "socket_multibyte_too_long":
				record.SocketPath = "/" + strings.Repeat("é", 52)
			case "relative_pid_file":
				record.PIDFile = "daemon.pid"
			case "pid_traversal":
				record.PIDFile = filepath.Dir(record.PIDFile) + "/sub/../daemon.pid"
			case "pid_nul":
				record.PIDFile += "\x00suffix"
			case "zero_pid":
				record.PID = 0
			case "negative_pid":
				record.PID = -1
			case "unknown_version":
				record.Version = 2
			}
			data, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "malformed":
				data = []byte("{")
			case "null":
				data = []byte("null")
			case "array":
				data = []byte("[]")
			case "missing_fields":
				data = []byte(`{"version":1}`)
			case "boolean_pid":
				var fields map[string]any
				if err := json.Unmarshal(data, &fields); err != nil {
					t.Fatal(err)
				}
				fields["pid"] = true
				data, err = json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
			case "unknown_field":
				data = append([]byte(`{"unexpected":true,`), data[1:]...)
			case "duplicate_field":
				data = append([]byte(`{"version":1,`), data[1:]...)
			case "trailing_json":
				data = append(data, []byte("\n{}")...)
			case "trailing_garbage":
				data = append(data, []byte(" garbage")...)
			case "invalid_utf8":
				data = []byte(strings.Replace(string(data), "daemon.sock", "daemon\xff.sock", 1))
			case "oversized":
				data = append(data, []byte(strings.Repeat(" ", 8193))...)
			}
			if err := os.WriteFile(filepath.Join(project, ".slb", "daemon.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			paths, err := DiscoverRuntime(project)
			if !errors.Is(err, ErrDiscoveryInvalid) {
				t.Fatalf("invalid %s record accepted or misclassified: %v", kind, err)
			}
			if paths.SocketPath != SocketPathForCWD(project) || paths.PIDFile != PIDFileForCWD(project) {
				t.Fatalf("invalid record leaked untrusted paths into diagnostics: %+v", paths)
			}
		})
	}
}

func TestDiscoverRuntimeRejectsUnsafeRecordFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "directory", "fifo", "group_read", "other_write", "foreign_owner"} {
		t.Run(kind, func(t *testing.T) {
			project := discoveryProject(t)
			path := filepath.Join(project, ".slb", "daemon.json")
			target := filepath.Join(project, "record-target")
			record := discoveryFixture(project, filepath.Join(shortSocketDir(t), "daemon.sock"))
			data, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, data, 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "symlink":
				err = os.Symlink(target, path)
			case "hardlink":
				err = os.Link(target, path)
			case "directory":
				err = os.Mkdir(path, 0700)
			case "fifo":
				err = syscall.Mkfifo(path, 0600)
			default:
				err = os.WriteFile(path, data, 0600)
				if err == nil {
					switch kind {
					case "group_read":
						err = os.Chmod(path, 0640)
					case "other_write":
						err = os.Chmod(path, 0602)
					case "foreign_owner":
						makeDiscoveryFixtureForeignOwned(t, path)
					}
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			if _, err := DiscoverRuntime(project); !errors.Is(err, ErrDiscoveryInvalid) {
				t.Fatalf("unsafe %s record accepted or misclassified: %v", kind, err)
			}
			if time.Since(start) > time.Second {
				t.Fatal("unsafe record blocked discovery instead of being rejected promptly")
			}
			if after, err := os.ReadFile(target); err != nil || string(after) != string(data) {
				t.Fatalf("discovery modified the record target: %v", err)
			}
		})
	}
}

func TestDiscoverRuntimeRejectsUnsafeProjectStateDirectory(t *testing.T) {
	for _, kind := range []string{"symlink", "group_write", "other_write", "foreign_owner"} {
		t.Run(kind, func(t *testing.T) {
			project := discoveryProject(t)
			marker := filepath.Join(project, ".slb")
			if kind == "symlink" {
				target := marker + ".original"
				if err := os.Rename(marker, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, marker); err != nil {
					t.Fatal(err)
				}
			}
			writeDiscoveryFixture(t, project, discoveryFixture(project, filepath.Join(shortSocketDir(t), "daemon.sock")))
			var err error
			switch kind {
			case "group_write":
				err = os.Chmod(marker, 0770)
			case "other_write":
				err = os.Chmod(marker, 0702)
			case "foreign_owner":
				makeDiscoveryFixtureForeignOwned(t, marker)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DiscoverRuntime(project); !errors.Is(err, ErrDiscoveryInvalid) {
				t.Fatalf("unsafe %s state directory accepted or misclassified: %v", kind, err)
			}
		})
	}
}

func TestDiscoverRuntimeLongProjectKeepsShortSocket(t *testing.T) {
	t.Setenv("TMPDIR", shortSocketDir(t))
	project := filepath.Join(t.TempDir(), strings.Repeat("project-", 24))
	if err := os.MkdirAll(filepath.Join(project, ".slb"), 0700); err != nil {
		t.Fatal(err)
	}
	project, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := DiscoverRuntime(project)
	if err != nil {
		t.Fatal(err)
	}
	if len(runtime.SocketPath) > 103 || strings.HasPrefix(runtime.SocketPath, project) {
		t.Fatalf("long project expanded the Unix socket path: %q", runtime.SocketPath)
	}
}

func TestPublishRuntimeCleanupPreservesReplacement(t *testing.T) {
	project := discoveryProject(t)
	dir := shortSocketDir(t)
	opts := startDiscoveryDaemon(t, project, filepath.Join(dir, "daemon.sock"), filepath.Join(dir, "daemon.pid"))
	cleanup, err := publishRuntime(opts)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, ".slb", "daemon.json")
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode().Perm() != 0600 {
		t.Fatalf("published record permissions = %o, want 600", before.Mode().Perm())
	}
	if err := os.Rename(path, path+".previous"); err != nil {
		t.Fatal(err)
	}
	replacement := []byte("replacement daemon generation\n")
	if err := os.WriteFile(path, replacement, 0600); err != nil {
		t.Fatal(err)
	}
	_ = cleanup() // It may report that ownership changed; it must preserve it.
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(replacement) {
		t.Fatalf("retiring daemon removed or modified a replacement record: %v", err)
	}
}

func TestPublishRuntimeCleansOwnRecord(t *testing.T) {
	project := discoveryProject(t)
	dir := shortSocketDir(t)
	opts := startDiscoveryDaemon(t, project, filepath.Join(dir, "daemon.sock"), filepath.Join(dir, "daemon.pid"))
	cleanup, err := publishRuntime(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(project, ".slb", "daemon.json")); !os.IsNotExist(err) {
		t.Fatalf("owned discovery record survived shutdown: %v", err)
	}
}

func TestPublishRuntimeFailurePreservesExistingRecord(t *testing.T) {
	project := discoveryProject(t)
	dir := shortSocketDir(t)
	writeDiscoveryFixture(t, project, discoveryFixture(project, filepath.Join(dir, "previous.sock")))
	path := filepath.Join(project, ".slb", "daemon.json")
	original, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cleanup, err := publishRuntime(ServerOptions{
		ProjectDir: project, SocketPath: filepath.Join(dir, "missing.sock"), PIDFile: filepath.Join(dir, "missing.pid"),
	})
	if cleanup != nil {
		defer func() { _ = cleanup() }()
	}
	if err == nil {
		t.Fatal("published a daemon without a listener")
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(original, current) {
		t.Fatalf("publication failure replaced the preceding generation: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("publication failure modified the preceding generation: %v", err)
	}
}

// discoveryProbe records every connection and RPC so tests can distinguish
// rejecting an endpoint from contacting it and rejecting a later response.
type discoveryProbe struct {
	listener      net.Listener
	status        map[string]any
	mu            sync.Mutex
	conns         map[net.Conn]struct{}
	accepted      int
	firstAccepted chan struct{}
	methods       []string
	stopped       bool
	wg            sync.WaitGroup
	once          sync.Once
}

func startDiscoveryProbe(t *testing.T, socket string, status map[string]any) *discoveryProbe {
	t.Helper()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socket, 0600); err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	probe := &discoveryProbe{listener: listener, status: status, conns: make(map[net.Conn]struct{}), firstAccepted: make(chan struct{})}
	probe.wg.Add(1)
	go func() {
		defer probe.wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			probe.mu.Lock()
			if probe.stopped {
				probe.mu.Unlock()
				_ = conn.Close()
				return
			}
			probe.conns[conn] = struct{}{}
			probe.accepted++
			if probe.accepted == 1 {
				close(probe.firstAccepted)
			}
			probe.wg.Add(1)
			probe.mu.Unlock()
			go probe.serve(conn)
		}
	}()
	t.Cleanup(probe.stop)
	return probe
}

func (p *discoveryProbe) serve(conn net.Conn) {
	defer p.wg.Done()
	defer func() { _ = conn.Close() }()
	defer func() {
		p.mu.Lock()
		delete(p.conns, conn)
		p.mu.Unlock()
	}()
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		var request RPCRequest
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			return
		}
		p.mu.Lock()
		p.methods = append(p.methods, request.Method)
		p.mu.Unlock()
		result := map[string]any{"pong": true, "action": "allow"}
		if request.Method == "status" {
			result = p.status
		}
		if err := json.NewEncoder(conn).Encode(RPCResponse{ID: request.ID, Result: result}); err != nil {
			return
		}
	}
}

func (p *discoveryProbe) stop() {
	p.once.Do(func() {
		_ = p.listener.Close()
		p.mu.Lock()
		p.stopped = true
		for conn := range p.conns {
			_ = conn.Close()
		}
		p.mu.Unlock()
		p.wg.Wait()
	})
}

func TestProjectIPCClientVerifiesIdentityOnSameConnection(t *testing.T) {
	t.Setenv("SLB_HOST", "")
	project := discoveryProject(t)
	socket := filepath.Join(shortSocketDir(t), "daemon.sock")
	probe := startDiscoveryProbe(t, socket, map[string]any{
		"pid": os.Getpid(), "project_path": project, "state_ready": true,
	})
	writeDiscoveryFixture(t, project, discoveryFixture(project, socket))
	client := NewProjectIPCClient(project)
	defer func() { _ = client.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := client.HookQuery(ctx, HookQueryParams{CWD: project, Command: "echo identity-checked"}); err != nil {
		t.Fatal(err)
	}
	probe.stop()
	if probe.accepted != 1 || !reflect.DeepEqual(probe.methods, []string{"status", "hook_query"}) {
		t.Fatalf("identity and command did not share one connection: connections=%d methods=%v", probe.accepted, probe.methods)
	}
}

func TestProjectIPCClientRejectsWrongDaemonBeforeCommand(t *testing.T) {
	t.Setenv("SLB_HOST", "")
	for _, withRecord := range []bool{false, true} {
		name := "missing_record"
		if withRecord {
			name = "published_record"
		}
		t.Run(name, func(t *testing.T) {
			for _, kind := range []string{"wrong_project", "zero_pid", "not_ready", "wrong_pid"} {
				if !withRecord && kind == "wrong_pid" && runtime.GOOS != "linux" {
					continue // Linux authenticates PID even without a published generation.
				}
				t.Run(kind, func(t *testing.T) {
					project := discoveryProject(t)
					t.Setenv("TMPDIR", shortSocketDir(t))
					socket := SocketPathForCWD(project)
					status := map[string]any{"pid": os.Getpid(), "project_path": project, "state_ready": true}
					switch kind {
					case "wrong_project":
						status["project_path"] = discoveryProject(t)
					case "zero_pid":
						status["pid"] = 0
					case "not_ready":
						status["state_ready"] = false
					case "wrong_pid":
						status["pid"] = os.Getpid() + 1
					}
					probe := startDiscoveryProbe(t, socket, status)
					if withRecord {
						writeDiscoveryFixture(t, project, discoveryFixture(project, socket))
					}
					client := NewProjectIPCClient(project)
					defer func() { _ = client.Close() }()
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					if _, err := client.HookQuery(ctx, HookQueryParams{CWD: project, Command: "private-command-must-not-be-sent"}); err == nil {
						t.Fatal("accepted a daemon with mismatched identity or readiness")
					}
					probe.stop()
					if !reflect.DeepEqual(probe.methods, []string{"status"}) {
						t.Fatalf("sent command before checking daemon identity: %v", probe.methods)
					}
				})
			}
		})
	}
}

func TestProjectIPCClientDoesNotFallbackFromInvalidOrMissingEndpoint(t *testing.T) {
	t.Setenv("SLB_HOST", "")
	for _, kind := range []string{"invalid_record", "missing_socket"} {
		t.Run(kind, func(t *testing.T) {
			project := discoveryProject(t)
			t.Setenv("TMPDIR", shortSocketDir(t))
			legacy := startDiscoveryProbe(t, SocketPathForCWD(project), map[string]any{
				"pid": os.Getpid(), "project_path": project, "state_ready": true,
			})
			record := discoveryFixture(project, filepath.Join(shortSocketDir(t), "absent.sock"))
			if kind == "invalid_record" {
				record.ProjectPath = filepath.Dir(project)
			}
			writeDiscoveryFixture(t, project, record)
			client := NewProjectIPCClient(project)
			defer func() { _ = client.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := client.Ping(ctx); err == nil {
				t.Fatal("discovery fell through to a different socket")
			}
			if kind == "missing_socket" && client.SocketPath() != record.SocketPath {
				t.Fatalf("missing socket diagnostic lost attempted path: %q", client.SocketPath())
			}
			legacy.stop()
			if legacy.accepted != 0 {
				t.Fatalf("contacted legacy fallback after %s: %v", kind, legacy.methods)
			}
		})
	}
}

func TestProjectIPCClientRejectsUnsafeSocketBeforeDial(t *testing.T) {
	t.Setenv("SLB_HOST", "")
	for _, kind := range []string{"symlink", "public_socket", "writable_parent", "foreign_owner"} {
		t.Run(kind, func(t *testing.T) {
			project := discoveryProject(t)
			dir := shortSocketDir(t)
			socket := filepath.Join(dir, "daemon.sock")
			probe := startDiscoveryProbe(t, socket, map[string]any{
				"pid": os.Getpid(), "project_path": project, "state_ready": true,
			})
			var err error
			switch kind {
			case "symlink":
				alias := filepath.Join(dir, "alias.sock")
				err = os.Symlink(socket, alias)
				socket = alias
			case "public_socket":
				err = os.Chmod(socket, 0660)
			case "writable_parent":
				err = os.Chmod(dir, 0777)
			case "foreign_owner":
				makeDiscoveryFixtureForeignOwned(t, socket)
			}
			if err != nil {
				t.Fatal(err)
			}
			writeDiscoveryFixture(t, project, discoveryFixture(project, socket))
			client := NewProjectIPCClient(project)
			defer func() { _ = client.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := client.Ping(ctx); err == nil {
				t.Fatal("unsafe socket accepted")
			}
			probe.stop()
			if probe.accepted != 0 {
				t.Fatalf("dialed unsafe %s endpoint: %v", kind, probe.methods)
			}
		})
	}
}

func TestDiscoverRuntimeMissingSocketRetainsDiagnosticPath(t *testing.T) {
	t.Setenv("SLB_HOST", "")
	project := discoveryProject(t)
	socket := filepath.Join(shortSocketDir(t), "missing.sock")
	writeDiscoveryFixture(t, project, discoveryFixture(project, socket))
	paths, err := DiscoverRuntime(project)
	if err != nil || paths.SocketPath != socket {
		t.Fatalf("valid record with absent endpoint was not retained: %+v, %v", paths, err)
	}
	client := NewProjectIPCClient(project)
	defer func() { _ = client.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Ping(ctx); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing socket should fail before dialing: %v", err)
	}
	if client.SocketPath() != socket {
		t.Fatalf("diagnostic path = %q, want %q", client.SocketPath(), socket)
	}
}

func TestDiscoverRuntimeAcceptsSocketByteLimit(t *testing.T) {
	for _, socket := range []string{"/" + strings.Repeat("a", 102), "/" + strings.Repeat("é", 51)} {
		project := discoveryProject(t)
		writeDiscoveryFixture(t, project, discoveryFixture(project, socket))
		paths, err := DiscoverRuntime(project)
		if err != nil || paths.SocketPath != socket {
			t.Fatalf("103-byte pathname rejected: %+v, %v", paths, err)
		}
	}
}

func TestRuntimeParentsRejectWritableLexicalSymlinkAncestor(t *testing.T) {
	dir := shortSocketDir(t)
	public := filepath.Join(dir, "public")
	private := filepath.Join(dir, "private")
	for _, path := range []string{public, private} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(public, 0777); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(public, "alias")
	if err := os.Symlink(private, alias); err != nil {
		t.Fatal(err)
	}
	if err := validateRuntimeParents(alias); err == nil {
		t.Fatal("resolved target hid a writable lexical ancestor that can redirect the socket")
	}
	ownedAlias := filepath.Join(dir, "owned-alias")
	if err := os.Symlink(private, ownedAlias); err != nil {
		t.Fatal(err)
	}
	if err := validateRuntimeParents(ownedAlias); err != nil {
		t.Fatalf("owner-controlled directory alias rejected: %v", err)
	}
}

func TestProjectRuntimeLeaseFencesDifferentTMPDIRs(t *testing.T) {
	project := discoveryProject(t)
	t.Setenv("TMPDIR", shortSocketDir(t))
	first := normalizeServerOptions(ServerOptions{ProjectDir: project})
	owner, err := claimProjectRuntime(first)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	lock := filepath.Join(project, ".slb", "daemon.lock")
	original, err := os.Lstat(lock)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", shortSocketDir(t))
	second := normalizeServerOptions(ServerOptions{ProjectDir: project})
	if first.SocketPath == second.SocketPath || first.PIDFile == second.PIDFile {
		t.Fatal("test did not vary the socket and PID environments")
	}
	if contender, err := claimProjectRuntime(second); !errors.Is(err, ErrDaemonOwned) {
		if contender != nil {
			_ = contender.Close()
		}
		t.Fatalf("second environment bypassed project lease: %v", err)
	}
	other, err := claimProjectRuntime(ServerOptions{ProjectDir: discoveryProject(t)})
	if err != nil {
		t.Fatalf("one project's lease blocked a different project: %v", err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	replacement, err := claimProjectRuntime(second)
	if err != nil {
		t.Fatalf("project lease was not released after shutdown: %v", err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	current, err := os.Lstat(lock)
	if err != nil || !os.SameFile(original, current) {
		t.Fatalf("project lease inode was removed or replaced: %v", err)
	}
}

func TestProjectIPCClientRejectsForgedRecordPIDAgainstKernelPeer(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux SO_PEERCRED exposes the peer PID; BSD credentials authenticate UID only")
	}
	t.Setenv("SLB_HOST", "")
	project := discoveryProject(t)
	socket := filepath.Join(shortSocketDir(t), "daemon.sock")
	forgedPID := os.Getpid() + 100000
	probe := startDiscoveryProbe(t, socket, map[string]any{
		"pid": forgedPID, "project_path": project, "state_ready": true,
	})
	record := discoveryFixture(project, socket)
	record.PID = forgedPID
	writeDiscoveryFixture(t, project, record)
	client := NewProjectIPCClient(project)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := client.HookQuery(ctx, HookQueryParams{CWD: project, Command: "private-command-must-not-be-sent"}); err == nil {
		t.Fatal("a self-consistent forged record and status bypassed kernel peer identity")
	}
	select {
	case <-probe.firstAccepted:
	case <-time.After(time.Second):
		t.Fatal("forged-generation test did not reach a kernel peer")
	}
	probe.stop()
	if probe.accepted != 1 || len(probe.methods) != 0 {
		t.Fatalf("forged generation received traffic before peer authentication: connections=%d methods=%v", probe.accepted, probe.methods)
	}
}

func TestProjectIPCClientCloseInterruptsIdentityAndAllowsReconnect(t *testing.T) {
	t.Setenv("SLB_HOST", "")
	project := discoveryProject(t)
	socket := filepath.Join(shortSocketDir(t), "daemon.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(socket, 0600); err != nil {
		t.Fatal(err)
	}
	writeDiscoveryFixture(t, project, discoveryFixture(project, socket))
	client := NewProjectIPCClient(project)
	t.Cleanup(func() { _ = client.Close() })
	type callResult struct {
		response *HookQueryResult
		err      error
	}
	query := func(ctx context.Context, command string) <-chan callResult {
		finished := make(chan callResult, 1)
		go func() {
			response, err := client.HookQuery(ctx, HookQueryParams{Command: command, SessionID: "private-session", CWD: project})
			finished <- callResult{response, err}
		}()
		return finished
	}
	readProbe := func(conn *net.UnixConn) *bufio.Reader {
		t.Helper()
		if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(conn)
		frame, err := reader.ReadString('\n')
		var probe map[string]any
		if err != nil || json.Unmarshal([]byte(frame), &probe) != nil || len(probe) != 2 || probe["method"] != "status" || probe["id"] != float64(0) {
			t.Fatalf("expected a bare identity probe before protected traffic: %q (%v)", frame, err)
		}
		return reader
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	firstResult := query(ctx, "first-secret-must-not-be-sent")
	_ = listener.SetDeadline(time.Now().Add(2 * time.Second))
	first, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	firstReader := readProbe(first)
	// The bare status probe synchronizes with the unpublished connection's
	// authentication phase. Deliberately withhold the identity response.
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-firstResult:
		if result.err == nil || result.response != nil {
			t.Fatalf("closed authentication returned a usable response: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("Close waited for the operation deadline instead of interrupting identity setup")
	}
	if _, err := client.connection(); err == nil {
		t.Fatal("Close published an unauthenticated connection")
	}
	if err := first.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	remaining, err := io.ReadAll(firstReader)
	if len(remaining) != 0 || (err != nil && !errors.Is(err, syscall.ECONNRESET)) {
		t.Fatalf("cancelled authentication leaked protected data or retained its socket: data=%q err=%v", remaining, err)
	}

	// A fresh generation must authenticate normally; the retired setup's
	// cancellation must not poison another connection on the same client.
	reconnectCtx, reconnectCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer reconnectCancel()
	secondResult := query(reconnectCtx, "second-authenticated-command")
	_ = listener.SetDeadline(time.Now().Add(2 * time.Second))
	second, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	secondReader := readProbe(second)
	if err := json.NewEncoder(second).Encode(map[string]any{"id": 0, "result": map[string]any{
		"project_path": project, "pid": os.Getpid(), "state_ready": true,
	}}); err != nil {
		t.Fatal(err)
	}
	var request RPCRequest
	if err := json.NewDecoder(secondReader).Decode(&request); err != nil || request.Method != "hook_query" || request.ID != 1 || !strings.Contains(string(request.Params), "second-authenticated-command") {
		t.Fatalf("reconnected client did not send its authenticated query: %+v (%v)", request, err)
	}
	if err := json.NewEncoder(second).Encode(map[string]any{"id": request.ID, "result": map[string]string{"action": "allow"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-secondResult:
		if result.err != nil || result.response == nil || result.response.Action != "allow" {
			t.Fatalf("new authenticated generation was poisoned by Close: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("new authenticated generation did not finish")
	}
}

func TestAuthenticatedProjectPeerRejectsNonUnixTransport(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	if _, err := authenticatedPeerPID(left); err == nil {
		t.Fatal("non-Unix transport accepted as an authenticated local peer")
	}
}

func TestVerifyRuntimeIdentityProtocol(t *testing.T) {
	for _, kind := range []string{
		"valid", "no_record_pid", "wrong_project", "wrong_pid", "zero_pid", "not_ready",
		"wrong_response_id", "missing_response_id", "daemon_error", "malformed", "missing_identity", "oversized",
		"unready_shutdown", "unready_shutdown_wrong_project", "unready_shutdown_wrong_pid",
	} {
		t.Run(kind, func(t *testing.T) {
			expected := RuntimePaths{ProjectPath: "/example/project", PID: os.Getpid()}
			status := map[string]any{"pid": os.Getpid(), "project_path": expected.ProjectPath, "state_ready": true}
			response := RPCResponse{ID: 0, Result: status}
			requireReady := true
			switch kind {
			case "no_record_pid":
				expected.PID = 0
			case "wrong_project":
				status["project_path"] = "/other/project"
			case "wrong_pid":
				status["pid"] = os.Getpid() + 1
			case "zero_pid":
				status["pid"] = 0
			case "not_ready":
				status["state_ready"] = false
			case "wrong_response_id":
				response.ID = 1
			case "daemon_error":
				response.Error = &Error{Code: ErrCodeInternal, Message: "unavailable"}
			case "unready_shutdown", "unready_shutdown_wrong_project", "unready_shutdown_wrong_pid":
				// A broken policy/database must not strand the operator's own
				// daemon, but shutdown still requires the exact identity.
				requireReady = false
				status["state_ready"] = false
				if kind == "unready_shutdown_wrong_project" {
					status["project_path"] = "/other/project"
				}
				if kind == "unready_shutdown_wrong_pid" {
					status["pid"] = os.Getpid() + 1
				}
			}
			data, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "missing_response_id":
				data, err = json.Marshal(map[string]any{"result": status})
				if err != nil {
					t.Fatal(err)
				}
			case "malformed":
				data = []byte("{")
			case "missing_identity":
				data = []byte(`{"id":0,"result":{}}`)
			case "oversized":
				data = append([]byte(strings.Repeat(" ", 4097)), data...)
			}
			local, peer := net.Pipe()
			defer func() { _ = local.Close(); _ = peer.Close() }()
			if err := local.SetDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- verifyRuntimeIdentity(local, bufio.NewReader(local), expected, requireReady) }()
			request := receiveIPCRequest(t, peer)
			if request.Method != "status" || request.ID != 0 || len(request.Params) != 0 {
				t.Fatalf("identity probe sent command data: %+v", request)
			}
			written := make(chan struct{})
			go func() {
				_, _ = peer.Write(append(data, '\n'))
				_ = peer.Close()
				close(written)
			}()
			select {
			case err := <-done:
				valid := kind == "valid" || kind == "no_record_pid" || kind == "unready_shutdown"
				if (err == nil) != valid {
					t.Errorf("identity %s: got %v, want valid=%v", kind, err, valid)
				}
			case <-time.After(2 * time.Second):
				t.Error("identity verification did not honor the connection deadline")
			}
			_ = local.Close()
			<-written
		})
	}
}

func TestVerifyRuntimeIdentityConsumesSplitNewlineAndRejectsUnsolicitedFrame(t *testing.T) {
	for _, unsolicited := range []bool{false, true} {
		t.Run(strconv.FormatBool(unsolicited), func(t *testing.T) {
			local, peer := net.Pipe()
			defer func() { _ = local.Close(); _ = peer.Close() }()
			if err := local.SetDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			expected := RuntimePaths{ProjectPath: "/example/project", PID: os.Getpid()}
			reader := bufio.NewReader(local)
			done := make(chan error, 1)
			go func() { done <- verifyRuntimeIdentity(local, reader, expected, true) }()
			request := receiveIPCRequest(t, peer)
			if request.Method != "status" {
				t.Fatalf("identity request = %q, want status", request.Method)
			}
			frame, err := json.Marshal(RPCResponse{ID: 0, Result: map[string]any{
				"pid": os.Getpid(), "project_path": expected.ProjectPath, "state_ready": true,
			}})
			if err != nil {
				t.Fatal(err)
			}
			written := make(chan error, 1)
			go func() { _, err := peer.Write(frame); written <- err }()
			if err := <-written; err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				t.Fatalf("accepted an unterminated identity frame: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			ending := "\n"
			if unsolicited {
				// No command has been sent. A prefetched allow response must
				// never become authority for the next protected RPC.
				ending += "{\"id\":1,\"result\":{\"action\":\"allow\"}}\n"
			}
			go func() { _, err := peer.Write([]byte(ending)); written <- err }()
			select {
			case err := <-done:
				if (err != nil) != unsolicited {
					t.Fatalf("split identity frame with unsolicited=%t: %v", unsolicited, err)
				}
			case <-time.After(time.Second):
				t.Fatal("complete identity exchange did not finish")
			}
			if err := <-written; err != nil {
				t.Fatal(err)
			}
		})
	}
}
