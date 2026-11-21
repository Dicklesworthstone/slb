//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/log"
)

func TestLocalUserPrivateGroup(t *testing.T) {
	const nsswitch = "passwd: files\ngroup: files\n"
	const passwd = "root:x:0:0:root:/root:/bin/sh\nalice:x:1000:1000:Alice:/home/alice:/bin/sh\n"
	const group = "root:x:0:\nalice:x:1000:\n"
	type fixture struct {
		Name     string `json:"name"`
		NSS      string `json:"nss"`
		Passwd   string `json:"passwd"`
		Group    string `json:"group"`
		Expected bool   `json:"expected"`
	}
	cases := []fixture{
		{"private primary group", nsswitch, passwd, group, true},
		{"self listed explicitly", nsswitch, passwd, "alice:x:1000:alice\n", true},
		{"comments and local initgroups", "# local only\npasswd: files # accounts\ngroup:\tfiles\ninitgroups: files\nhosts: files dns\n", passwd, group, true},
		{"no group password", nsswitch, passwd, "alice::1000:\n", true},
		{"locked group password", nsswitch, passwd, "alice:!locked:1000:\n", true},
		{"disabled group password", nsswitch, passwd, "alice:*:1000:\n", true},
		{"active group password", nsswitch, passwd, "alice:$6$hash:1000:\n", false},
		{"shared primary group", nsswitch, passwd + "bob:x:1001:1000:Bob:/home/bob:/bin/sh\n", group, false},
		{"shared supplementary group", nsswitch, passwd, "alice:x:1000:alice,bob\n", false},
		{"empty member in list", nsswitch, passwd, "alice:x:1000:alice,\n", false},
		{"different primary GID", nsswitch, strings.ReplaceAll(passwd, "1000:1000", "1000:1001"), group, false},
		{"different group name", nsswitch, passwd, "developers:x:1000:\n", false},
		{"duplicate UID", nsswitch, passwd + "alias:x:1000:1001::/:/bin/sh\n", group, false},
		{"duplicate username", nsswitch, passwd + "alice:x:1001:1001::/:/bin/sh\n", group, false},
		{"duplicate GID", nsswitch, passwd, group + "other:x:1000:\n", false},
		{"duplicate group name", nsswitch, passwd, group + "alice:x:1001:\n", false},
		{"unknown UID", nsswitch, "root:x:0:0:root:/root:/bin/sh\n", group, false},
		{"unknown GID", nsswitch, passwd, "root:x:0:\n", false},
		{"LDAP passwd provider", "passwd: files ldap\ngroup: files\n", passwd, group, false},
		{"SSSD group provider", "passwd: files\ngroup: files sss\n", passwd, group, false},
		{"systemd extra memberships", "passwd: files systemd\ngroup: files systemd\n", passwd, group, false},
		{"remote initgroups", nsswitch + "initgroups: sss files\n", passwd, group, false},
		{"NSS merge action", "passwd: files\ngroup: files [SUCCESS=merge]\n", passwd, group, false},
		{"duplicate NSS entry", nsswitch + "group: files\n", passwd, group, false},
		{"colonless group override", nsswitch + "group ldap\n", passwd, group, false},
		{"colonless passwd override", nsswitch + "passwd\tldap\n", passwd, group, false},
		{"colonless initgroups", nsswitch + "initgroups sss\n", passwd, group, false},
		{"missing NSS entry", "passwd: files\n", passwd, group, false},
		{"missing NSS config", "", passwd, group, false},
		{"NIS passwd entry", nsswitch, passwd + "+::::::\n", group, false},
		{"NIS group entry", nsswitch, passwd, group + "+:::\n", false},
		{"malformed passwd", nsswitch, passwd + "broken\n", group, false},
		{"malformed group", nsswitch, passwd, group + "broken\n", false},
		{"signed UID", nsswitch, strings.ReplaceAll(passwd, "1000:1000", "+1000:1000"), group, false},
		{"overflow GID", nsswitch, passwd, "alice:x:4294967296:\n", false},
		{"non-ASCII UID", nsswitch, strings.ReplaceAll(passwd, "1000:1000", "１０００:1000"), group, false},
		{"leading zero UID", nsswitch, strings.ReplaceAll(passwd, "1000:1000", strings.Repeat("0", 5000)+"1000:1000"), group, true},
		{"NUL account data", nsswitch, passwd + "\x00", group, false},
		{"oversized account data", nsswitch, passwd + strings.Repeat("#", maxLocalAccountBytes), group, false},
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			if got := localUserPrivateGroup(1000, 1000, tc.NSS, tc.Passwd, tc.Group); got != tc.Expected {
				t.Fatalf("private group = %v, want %v", got, tc.Expected)
			}
		})
	}

	// Run the same independent acceptance cases through the standalone Python
	// hook. A Go-only fix would leave existing generated hooks offline (#34).
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required for private-group parser parity")
	}
	source, err := os.ReadFile(filepath.Join("..", "cli", "hook_runtime.py"))
	if err != nil {
		t.Fatal(err)
	}
	script := "__name__ = 'slb_upg_test'\n" + string(source) + `
cases = json.load(sys.stdin)
for case in cases:
    actual = _local_user_private_group(1000, 1000, case["nss"], case["passwd"], case["group"])
    assert actual == case["expected"], (case["name"], actual, case["expected"])
`
	input, err := json.Marshal(cases)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, "-c", script)
	cmd.Stdin = strings.NewReader(string(input))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Python private-group decisions differ: %v\n%s", err, output)
	}
}

func TestReadLocalAccountFileRejectsUntrustedSources(t *testing.T) {
	for _, kind := range []string{"missing", "user_owned", "symlink", "directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "accounts")
			switch kind {
			case "user_owned", "symlink":
				if err := os.WriteFile(path, []byte("passwd: files\ngroup: files\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if kind == "symlink" {
					alias := path + ".alias"
					if err := os.Symlink(path, alias); err != nil {
						t.Fatal(err)
					}
					path = alias
				} else if os.Geteuid() == 0 {
					if err := os.Chmod(path, 0666); err != nil {
						t.Fatal(err)
					}
				}
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Skipf("FIFO unavailable: %v", err)
				}
			}
			if _, err := readLocalAccountFile(path); err == nil {
				t.Fatal("untrusted account source was accepted")
			}
		})
	}
}

func TestRuntimeParentsPrivateGroupPermissions(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux private-group ancestor policy")
	}
	path := t.TempDir()
	for _, tc := range []struct {
		name    string
		mode    os.FileMode
		private bool
		accept  bool
	}{
		{"private group writable", 0775, true, true},
		{"shared group writable", 0775, false, false},
		{"world writable private group", 0777, true, false},
		{"sticky shared directory", 0777 | os.ModeSticky, false, true},
		{"ordinary owned directory", 0755, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatal(err)
			}
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := runtimeAncestorPermissions(path, info, func() bool { return tc.private }); got != tc.accept {
				t.Fatalf("ancestor accepted = %v, want %v", got, tc.accept)
			}
		})
	}
	if err := os.Chmod(path, 0775); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"foreign user", "other group"} {
		t.Run(kind, func(t *testing.T) {
			stat := *info.Sys().(*syscall.Stat_t)
			if kind == "foreign user" {
				stat.Uid++
			} else {
				stat.Gid++
			}
			if runtimeAncestorPermissions(path, runtimeAncestorTestInfo{info, stat}, func() bool { return true }) {
				t.Fatal("private group exception accepted a different identity")
			}
		})
	}
}

func TestRuntimeParentsPrivateGroupAncestorTraversal(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux private-group ancestor policy")
	}
	parent := t.TempDir()
	project := filepath.Join(parent, "project")
	if err := os.Mkdir(project, 0775); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{parent, project} {
		if err := os.Chmod(path, 0775); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(project, alias); err != nil {
		t.Fatal(err)
	}
	// Exercise the real recursive path/ACL checks with explicit account-file
	// evidence. Unlike the native integration test, this does not require the
	// test host's /etc files to qualify (e.g. inside a UID-mapped sandbox).
	uid, gid := uint32(os.Geteuid()), uint32(os.Getegid())
	passwd := fmt.Sprintf("alice:x:%d:%d::/:/bin/sh\n", uid, gid)
	group := fmt.Sprintf("alice:x:%d:\n", gid)
	privateGroup := func() bool {
		return localUserPrivateGroup(uid, gid, "passwd: files\ngroup: files\n", passwd, group)
	}
	remaining := 512
	if err := validateRuntimeAncestors(alias, &remaining, privateGroup); err != nil {
		t.Fatalf("0775 private-group ancestors rejected: %v", err)
	}
	group = fmt.Sprintf("alice:x:%d:bob\n", gid)
	remaining = 512
	if err := validateRuntimeAncestors(alias, &remaining, privateGroup); err == nil {
		t.Fatal("shared group accepted after membership changed")
	}
}

type runtimeAncestorTestInfo struct {
	os.FileInfo
	stat syscall.Stat_t
}

func (info runtimeAncestorTestInfo) Sys() any { return &info.stat }

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
				if err != nil {
					t.Fatal(err)
				}
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
