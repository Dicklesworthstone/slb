//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/log"
)

func ownershipTestDir(t *testing.T) string {
	t.Helper()
	// Unix sockets have small pathname limits; avoid long test names.
	dir, err := os.MkdirTemp("", "slb-owner-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestFileLeaseProcessHelper(t *testing.T) {
	path := os.Getenv("SLB_TEST_OWNER_PATH")
	if path == "" {
		return
	}
	lease, err := acquireFileLease(path)
	if err != nil {
		fmt.Fprintln(os.Stdout, "blocked")
		return
	}
	defer lease.Close()
	fmt.Fprintln(os.Stdout, "owned")
	_, _ = bufio.NewReader(os.Stdin).ReadByte()
}

func TestFileLeaseSurvivesContendersAndRecoversAfterCrash(t *testing.T) {
	path := filepath.Join(ownershipTestDir(t), "project.lock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestFileLeaseProcessHelper$")
	cmd.Env = append(os.Environ(), "SLB_TEST_OWNER_PATH="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "owned\n" {
		t.Fatalf("child did not acquire lease: %q %v", line, err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			other, err := acquireFileLease(path)
			if other != nil {
				_ = other.Close()
			}
			if !errors.Is(err, ErrDaemonOwned) {
				t.Errorf("contender was not refused: %v", err)
			}
		}()
	}
	wg.Wait()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	replacement, err := acquireFileLease(path)
	if err != nil {
		t.Fatalf("crashed owner left a permanent lock: %v", err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("lease inode was replaced or removed: %v", err)
	}
}

func TestFileLeaseRejectsUnsafeFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo", "public", "hardlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "lock")
			target := filepath.Join(dir, "target")
			if err := os.WriteFile(target, []byte("preserve"), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(target, path)
			case "fifo":
				err = syscall.Mkfifo(path, 0600)
			case "public":
				err = os.WriteFile(path, []byte("preserve"), 0644)
			case "hardlink":
				err = os.Link(target, path)
			case "directory":
				err = os.Mkdir(path, 0700)
			}
			if err != nil {
				t.Fatal(err)
			}
			lease, err := acquireFileLease(path)
			if lease != nil {
				_ = lease.Close()
			}
			if err == nil {
				t.Fatal("unsafe lease accepted")
			}
			data, err := os.ReadFile(target)
			if err != nil || string(data) != "preserve" {
				t.Fatal("target changed")
			}
		})
	}
}

func TestOwnedSocketPreservesLiveListener(t *testing.T) {
	for _, cooperating := range []bool{false, true} {
		t.Run(fmt.Sprint(cooperating), func(t *testing.T) {
			path := filepath.Join(ownershipTestDir(t), "s")
			var ln net.Listener
			var cleanup func() error
			var err error
			if cooperating {
				ln, cleanup, err = listenOwnedUnix(path)
			} else {
				ln, err = net.Listen("unix", path)
				cleanup = ln.Close
			}
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			before, _ := os.Lstat(path)
			other, cleanOther, err := listenOwnedUnix(path)
			if other != nil {
				_ = cleanOther()
			}
			if !errors.Is(err, ErrDaemonOwned) {
				t.Fatalf("live socket not protected: %v", err)
			}
			after, _ := os.Lstat(path)
			if !os.SameFile(before, after) {
				t.Fatal("contender replaced live socket")
			}
			conn, err := net.Dial("unix", path)
			if err != nil {
				t.Fatal("original listener disconnected", err)
			}
			_ = conn.Close()
		})
	}
}

func TestOwnedSocketStaleRecoveryAndFencedCleanup(t *testing.T) {
	path := filepath.Join(ownershipTestDir(t), "s")
	old, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	old.SetUnlinkOnClose(false)
	_ = old.Close()
	ln, cleanup, err := listenOwnedUnix(path)
	if err != nil {
		t.Fatal("stale recovery", err)
	}
	// Closing a listener must not drop the ownership lease before cleanup.
	_ = ln.Close()
	if next, finish, err := listenOwnedUnix(path); err == nil {
		_ = next.Close()
		_ = finish()
		t.Fatal("shutdown released socket ownership before cleanup")
	}
	// A replacement made by an external actor must survive old cleanup.
	if err := os.Rename(path, path+".previous"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new owner"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cleanup(); err == nil {
		t.Fatal("replacement was not diagnosed")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "new owner" {
		t.Fatal("old cleanup removed replacement")
	}
}

func TestOwnedSocketCleanShutdownAndRestart(t *testing.T) {
	path := filepath.Join(ownershipTestDir(t), "s")
	for i := 0; i < 3; i++ {
		_, cleanup, err := listenOwnedUnix(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := cleanup(); err != nil {
			t.Fatal(err)
		}
		if err := cleanup(); err != nil {
			t.Fatal("cleanup not idempotent", err)
		}
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatal("owned socket not cleaned up")
		}
	}
}

func TestPIDFileRejectsUnsafeFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo", "writable", "hardlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			dir := ownershipTestDir(t)
			path := filepath.Join(dir, "daemon.pid")
			target := filepath.Join(dir, "target")
			if err := os.WriteFile(target, []byte("preserve\n"), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(target, path)
			case "fifo":
				err = syscall.Mkfifo(path, 0600)
			case "writable":
				err = os.WriteFile(path, []byte("12345\n"), 0600)
				if err == nil {
					err = os.Chmod(path, 0666)
				}
			case "hardlink":
				err = os.Link(target, path)
			case "directory":
				err = os.Mkdir(path, 0700)
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err = RunDaemon(ctx, ServerOptions{
				ProjectDir: dir, PIDFile: path, SocketPath: filepath.Join(dir, "s"), Logger: log.New(io.Discard),
			})
			if err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("unsafe PID file was accepted or blocked startup: %v", err)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("startup replaced unsafe PID path: %v", err)
			}
			data, err := os.ReadFile(target)
			if err != nil || string(data) != "preserve\n" {
				t.Fatalf("startup modified PID link target: %q, %v", data, err)
			}
		})
	}
}

func TestStopDaemonRejectsInvalidPIDWithoutRemovingRecord(t *testing.T) {
	for _, contents := range []string{"0\n", "-1\n", "-23\n", "invalid\n", "", "1" + strings.Repeat(" ", 127) + "2"} {
		t.Run(fmt.Sprintf("pid_%q", contents), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "daemon.pid")
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			// Check parsing before exercising stop: a regression must fail the
			// test without ever attempting to signal a process-group PID.
			if _, err := readPIDFile(path); err == nil {
				t.Fatal("invalid/nonpositive PID accepted")
			}
			if err := StopDaemonWithOptions(ServerOptions{PIDFile: path, SocketPath: filepath.Join(dir, "absent.sock")}, time.Second); err == nil {
				t.Fatal("stop accepted invalid/nonpositive PID")
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != contents {
				t.Fatalf("failed stop removed or rewrote PID record: %q, %v", data, err)
			}
		})
	}
}

func TestStopDaemonCleansStalePID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.pid")
	if err := os.WriteFile(path, []byte("99999999\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := StopDaemonWithOptions(ServerOptions{PIDFile: path, SocketPath: filepath.Join(dir, "absent.sock")}, time.Second); err != nil {
		t.Fatalf("could not clean stale PID: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("stale PID survived stop: %v", err)
	}
}

func TestStopDaemonIgnoresRemoteHost(t *testing.T) {
	lifecycleEnvironment(t)
	child := startLifecycleChild(t, ServerOptions{ProjectDir: lifecycleProject(t)}, false)
	child.ready(t)
	remote, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	t.Setenv("SLB_HOST", remote.Addr().String())
	if err := StopDaemonWithOptions(child.opts, 5*time.Second); err != nil {
		t.Fatalf("remote setting interfered with local project stop: %v", err)
	}
	child.wait(t, true)
	if err := remote.SetDeadline(time.Now().Add(25 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	conn, err := remote.Accept()
	if conn != nil {
		_ = conn.Close()
		t.Fatal("local daemon stop contacted SLB_HOST")
	}
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("checking remote listener: %v", err)
	}
}
