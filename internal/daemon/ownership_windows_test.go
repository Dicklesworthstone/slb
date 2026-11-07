//go:build windows

package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWindowsFileLeaseExclusiveAndStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")
	lease, err := acquireFileLease(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	original, err := lstatOwnedPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := acquireFileLease(path); !errors.Is(err, ErrDaemonOwned) {
		if other != nil {
			_ = other.Close()
		}
		t.Fatalf("second handle acquired the lease: %v", err)
	}
	if err := os.Rename(path, path+".replaced"); err == nil {
		t.Fatal("a live lease allowed its pathname to be replaced")
	}
	runContender := func(want string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for {
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWindowsOwnershipHelper$")
			cmd.Env = append(os.Environ(), "SLB_WINDOWS_OWNERSHIP_HELPER=lease", "SLB_WINDOWS_OWNERSHIP_PATH="+path)
			output, err := cmd.CombinedOutput()
			got := strings.TrimSpace(string(output))
			if err == nil && got == want {
				return
			}
			// Windows may finish releasing a closed handle's byte-range locks
			// asynchronously. Require eventual release within the deadline.
			if want == "acquired" && err == nil && got == "owned" && ctx.Err() == nil {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			t.Fatalf("contender: got %q (%v), want %q", output, err, want)
		}
	}
	runContender("owned")
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	// The helper exits without closing its lease. Process exit must release
	// the OS lock, and neither successful owner may replace the lock file.
	runContender("acquired")
	deadline := time.Now().Add(5 * time.Second)
	for {
		lease, err = acquireFileLease(path)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrDaemonOwned) || time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer lease.Close()
	current, err := lstatOwnedPath(path)
	if err != nil || !os.SameFile(original, current) {
		t.Fatalf("lease file identity changed across owners: %v", err)
	}
}

func TestWindowsDaemonSecurity(t *testing.T) {
	user, defaultOwner, err := windowsDaemonIdentity()
	if err != nil {
		t.Fatal(err)
	}
	owner := "O:" + user.String()
	allowUser := "(A;;FA;;;" + user.String() + ")"
	for _, tc := range []struct {
		name    string
		sddl    string
		private bool
		wantErr bool
	}{
		{"private owner", owner + "D:P" + allowUser, true, false},
		{"token default owner", "O:" + defaultOwner.String() + "D:P" + allowUser, true, false},
		{"readable legacy PID", owner + "D:P" + allowUser + "(A;;GR;;;WD)", false, false},
		{"public lease", owner + "D:P" + allowUser + "(A;;GR;;;WD)", true, true},
		{"public data write", owner + "D:P" + allowUser + "(A;;0x2;;;WD)", false, true},
		{"public child deletion", owner + "D:P" + allowUser + "(A;;0x40;;;WD)", false, true},
		{"public ACL write", owner + "D:P" + allowUser + "(A;;0x40000;;;WD)", false, true},
		{"null DACL", owner + "D:NO_ACCESS_CONTROL", false, true},
		{"foreign owner", "O:WDD:P" + allowUser, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(tc.sddl)
			if err != nil {
				t.Fatal(err)
			}
			err = validateWindowsDaemonSecurity(sd, user, defaultOwner, tc.private)
			if (err != nil) != tc.wantErr {
				t.Fatalf("security check: %v, want error=%v", err, tc.wantErr)
			}
		})
	}
}

func TestWindowsRuntimeAncestorSecurity(t *testing.T) {
	// Keep the expected owner distinct from SYSTEM and Administrators even
	// when the test itself runs under an elevated or service account.
	user, err := windows.StringToSid("S-1-5-21-1-2-3-1001")
	if err != nil {
		t.Fatal(err)
	}
	owner := "O:" + user.String()
	allowUser := "(A;;FA;;;" + user.String() + ")"
	for _, tc := range []struct {
		name    string
		sddl    string
		kind    windowsRuntimeObject
		wantErr bool
	}{
		{"user ancestor", owner + "D:P" + allowUser + "(A;;GR;;;WD)", windowsRuntimeAncestor, false},
		{"system ancestor", "O:SYD:P(A;;FA;;;SY)" + allowUser, windowsRuntimeAncestor, false},
		{"administrator ancestor", "O:BAD:P(A;;FA;;;BA)" + allowUser, windowsRuntimeAncestor, false},
		{"TrustedInstaller ancestor", "O:" + windowsTrustedInstallerSID + "D:P(A;;FA;;;" + windowsTrustedInstallerSID + ")" + allowUser, windowsRuntimeAncestor, false},
		{"TrustedInstaller reparse ancestor", "O:" + windowsTrustedInstallerSID + "D:P(A;;FA;;;" + windowsTrustedInstallerSID + ")" + allowUser, windowsRuntimeReparseAncestor, false},
		{"foreign ancestor", "O:WDD:P" + allowUser, windowsRuntimeAncestor, true},
		{"foreign service ancestor", "O:S-1-5-80-1-2-3-4-5D:P" + allowUser, windowsRuntimeAncestor, true},
		{"system final file", "O:SYD:P" + allowUser, windowsRuntimeFile, true},
		{"administrator final file", "O:BAD:P" + allowUser, windowsRuntimeFile, true},
		{"TrustedInstaller final file", "O:" + windowsTrustedInstallerSID + "D:P" + allowUser, windowsRuntimeFile, true},
		{"TrustedInstaller access to final file", owner + "D:P" + allowUser + "(A;;FA;;;" + windowsTrustedInstallerSID + ")", windowsRuntimeFile, true},
		{"foreign service access to ancestor", owner + "D:P" + allowUser + "(A;;FA;;;S-1-5-80-1-2-3-4-5)", windowsRuntimeAncestor, true},
		{"creation only ancestor", owner + "D:P" + allowUser + "(A;;0x6;;;AU)", windowsRuntimeAncestor, false},
		{"creation rights on final file", owner + "D:P" + allowUser + "(A;;0x6;;;AU)", windowsRuntimeFile, true},
		{"creation rights on reparse point", owner + "D:P" + allowUser + "(A;;0x6;;;AU)", windowsRuntimeReparseAncestor, true},
		{"child deletion", owner + "D:P" + allowUser + "(A;;0x40;;;WD)", windowsRuntimeAncestor, true},
		{"object deletion", owner + "D:P" + allowUser + "(A;;0x10000;;;WD)", windowsRuntimeAncestor, true},
		{"attribute write", owner + "D:P" + allowUser + "(A;;0x100;;;WD)", windowsRuntimeAncestor, true},
		{"extended attribute write", owner + "D:P" + allowUser + "(A;;0x10;;;WD)", windowsRuntimeAncestor, true},
		{"ACL write", owner + "D:P" + allowUser + "(A;;0x40000;;;WD)", windowsRuntimeAncestor, true},
		{"ownership write", owner + "D:P" + allowUser + "(A;;0x80000;;;WD)", windowsRuntimeAncestor, true},
		{"generic write", owner + "D:P" + allowUser + "(A;;GW;;;WD)", windowsRuntimeAncestor, true},
		{"inherit only rights", owner + "D:P" + allowUser + "(A;OIIO;FA;;;WD)", windowsRuntimeAncestor, false},
		{"null DACL", owner + "D:NO_ACCESS_CONTROL", windowsRuntimeAncestor, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(tc.sddl)
			if err != nil {
				t.Fatal(err)
			}
			err = validateWindowsRuntimeSecurity(sd, user, user, false, tc.kind)
			if (err != nil) != tc.wantErr {
				t.Fatalf("security check: %v, want error=%v", err, tc.wantErr)
			}
		})
	}
}

func TestWindowsRuntimeParentsChecksLinkAncestors(t *testing.T) {
	for _, unsafeParent := range []string{"lexical", "target"} {
		t.Run(unsafeParent, func(t *testing.T) {
			dir := t.TempDir()
			aliasParent := filepath.Join(dir, "aliases")
			targetParent := filepath.Join(dir, "targets")
			target := filepath.Join(targetParent, "runtime")
			for _, path := range []string{aliasParent, target} {
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			alias := filepath.Join(aliasParent, "runtime")
			if err := os.Symlink(filepath.Join("..", "targets", "runtime"), alias); err != nil {
				t.Skipf("symlink creation requires Windows developer mode or privilege: %v", err)
			}
			if err := validateRuntimeParents(alias); err != nil {
				t.Fatalf("safe link ancestors: %v", err)
			}
			user, _, err := windowsDaemonIdentity()
			if err != nil {
				t.Fatal(err)
			}
			sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.String() + ")(A;;0x40;;;WD)")
			if err != nil {
				t.Fatal(err)
			}
			acl, _, err := sd.DACL()
			if err != nil {
				t.Fatal(err)
			}
			path := aliasParent
			if unsafeParent == "target" {
				path = targetParent
			}
			if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
				windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
				t.Fatal(err)
			}
			if err := validateRuntimeParents(alias); err == nil {
				t.Fatal("accepted a link with an ancestor granting child deletion to another user")
			}
		})
	}
}

func TestWindowsDiscoveryReaderAllowsAtomicReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	for _, record := range []struct {
		path string
		data string
	}{
		{path, "original"},
		{path + ".pending", "replacement"},
	} {
		file, _, err := openDiscoveryFile(record.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString(record.data); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	reader, original, err := openDiscoveryFile(path, os.O_RDONLY)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	if err := os.Rename(path+".pending", path); err != nil {
		t.Fatalf("reader blocked atomic discovery publication: %v", err)
	}
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != "original" {
		t.Fatalf("reader lost its original snapshot: %q, %v", data, err)
	}
	data, err = os.ReadFile(path)
	if err != nil || string(data) != "replacement" {
		t.Fatalf("new discovery generation: %q, %v", data, err)
	}
	current, err := lstatOwnedPath(path)
	if err != nil || os.SameFile(original, current) {
		t.Fatalf("discovery publication did not replace the old file: %v", err)
	}
}

func TestWindowsPIDFileRejectsUnsafeObjects(t *testing.T) {
	t.Run("hard link", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "daemon.pid")
		file, _, err := openPIDFile(path, os.O_CREATE|os.O_RDWR)
		if err != nil {
			t.Fatal(err)
		}
		_ = file.Close()
		if err := os.Link(path, path+".linked"); err != nil {
			t.Fatal(err)
		}
		if file, _, err := openPIDFile(path, os.O_RDWR); err == nil {
			_ = file.Close()
			t.Fatal("accepted a multiply linked PID file")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, []byte("untouched"), 0600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "daemon.pid")
		if err := os.Symlink(target, path); err != nil {
			t.Skipf("symlink creation requires Windows developer mode or privilege: %v", err)
		}
		if file, _, err := openPIDFile(path, os.O_CREATE|os.O_RDWR); err == nil {
			_ = file.Close()
			t.Fatal("accepted a symlink PID file")
		}
		data, err := os.ReadFile(target)
		if err != nil || string(data) != "untouched" {
			t.Fatalf("symlink target changed: %q, %v", data, err)
		}
	})
	t.Run("directory", func(t *testing.T) {
		if file, _, err := openPIDFile(t.TempDir(), os.O_RDONLY); err == nil {
			_ = file.Close()
			t.Fatal("accepted a directory PID file")
		}
	})
}

func TestWindowsPIDOwnershipSnapshotAndCleanup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.pid")
	owner, err := claimPIDFile(path, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if pid, err := readPIDFile(path); err != nil || pid != os.Getpid() {
		t.Fatalf("published PID = %d, %v", pid, err)
	}
	original, err := lstatOwnedPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	file, replacement, err := openPIDFile(path, os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(file, "%d\n", os.Getpid()); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	_ = file.Close()
	if os.SameFile(original, replacement) {
		t.Fatal("snapshot was rebound to a replacement file")
	}
	if err := owner.Close(); err == nil {
		t.Fatal("exiting owner did not detect the replacement PID file")
	}
	if pid, err := readPIDFile(path); err != nil || pid != os.Getpid() {
		t.Fatalf("old owner removed replacement PID: %d, %v", pid, err)
	}
}

func TestWindowsSocketUsesPrivateACL(t *testing.T) {
	// Keep the AF_UNIX pathname below its length limit on Windows runners.
	dir, err := os.MkdirTemp("", "slb-acl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "daemon.sock")
	listener, cleanup, err := listenOwnedUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	user, defaultOwner, err := windowsDaemonIdentity()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateWindowsDaemonSecurity(sd, user, defaultOwner, true); err != nil {
		t.Fatalf("socket does not have a private ACL: %v", err)
	}
	conn, err := net.DialTimeout("unix", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("current user cannot connect to protected socket: %v", err)
	}
	_ = conn.Close()
}

func TestWindowsProcessAlive(t *testing.T) {
	if !processAlive(os.Getpid()) || processAlive(0) || processAlive(-1) {
		t.Fatal("incorrect current or invalid PID liveness")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWindowsOwnershipHelper$")
	cmd.Env = append(os.Environ(), "SLB_WINDOWS_OWNERSHIP_HELPER=exit")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Retain a handle so Windows cannot recycle this PID before the assertion.
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = cmd.Wait()
		t.Fatal(err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	if err := cmd.Wait(); err == nil || cmd.ProcessState.ExitCode() != 259 {
		t.Fatalf("helper did not exit with code 259: %v", err)
	}
	if processAlive(cmd.Process.Pid) {
		t.Fatal("terminated process with exit code 259 was reported alive")
	}
}

func TestWindowsOwnershipHelper(t *testing.T) {
	switch os.Getenv("SLB_WINDOWS_OWNERSHIP_HELPER") {
	case "exit":
		os.Exit(259)
	case "lease":
		lease, err := acquireFileLease(os.Getenv("SLB_WINDOWS_OWNERSHIP_PATH"))
		if errors.Is(err, ErrDaemonOwned) {
			fmt.Println("owned")
		} else if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		} else {
			fmt.Println("acquired")
		}
		runtime.KeepAlive(lease)
		os.Exit(0)
	}
}
