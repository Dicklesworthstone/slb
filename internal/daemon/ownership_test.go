//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package daemon

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
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
