package daemon

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

var ErrDaemonOwned = errors.New("daemon resource is already owned")

// pidFileOwner fences publication and cleanup for one daemon generation. The
// persistent lease is separate from the public PID file so removing that file
// cannot let a second daemon acquire ownership during shutdown.
type pidFileOwner struct {
	path  string
	pid   int
	info  os.FileInfo
	lease *fileLease
}

func claimPIDFile(path string, pid int) (*pidFileOwner, error) {
	lease, err := acquireFileLease(path + ".lock")
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = lease.Close()
		}
	}()
	file, info, err := openPIDFile(path, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 129))
	if err != nil {
		return nil, err
	}
	// A free lease permits stale or malformed PID recovery, but not adoption
	// of a live process left by an older daemon without the lease protocol.
	if previous, err := parsePID(data); err == nil && processAlive(previous) {
		return nil, fmt.Errorf("%w: daemon already running (pid=%d)", ErrDaemonOwned, previous)
	}
	if err := file.Chmod(0600); err != nil {
		return nil, err
	}
	if err := file.Truncate(0); err != nil {
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(file, "%d\n", pid); err != nil {
		return nil, err
	}
	current, err := lstatOwnedPath(path)
	if err != nil || !os.SameFile(info, current) {
		return nil, errors.New("pid file path changed while claiming ownership")
	}
	keep = true
	return &pidFileOwner{path: path, pid: pid, info: info, lease: lease}, nil
}

func (owner *pidFileOwner) Close() error {
	return errors.Join(removeOwnedPIDFile(owner.path, owner.info, owner.pid), owner.lease.Close())
}

// Callers hold the PID lease while checking and removing a snapshot. Checking
// both inode and content preserves replacements even when the same process
// starts a new generation or an external writer reuses the existing inode.
func removeOwnedPIDFile(path string, original os.FileInfo, pid int) error {
	currentPID, current, err := readPIDFileInfo(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if currentPID != pid || !os.SameFile(original, current) {
		return fmt.Errorf("pid file ownership changed; preserving %s", path)
	}
	return removeOwnedPath(path, original)
}

// fileLease holds an OS lock for the whole lifetime of a resource. The lock
// file is deliberately NEVER removed: unlinking it would let a contender lock
// a different inode while another process still owns the old one.
type fileLease struct {
	file *os.File
	once sync.Once
	err  error
}

func (l *fileLease) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() { l.err = l.file.Close() })
	return l.err
}

// removeOwnedPath must not remove a newer generation's file on shutdown.
func removeOwnedPath(path string, original os.FileInfo) error {
	current, err := lstatOwnedPath(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !os.SameFile(original, current) {
		return fmt.Errorf("resource path changed; preserving %s", path)
	}
	return os.Remove(path)
}

// listenOwnedUnix protects standalone IPC servers as well as full daemons.
// A cooperating owner is fenced by the persistent lock; an older/foreign
// listener is preserved even if it does not implement SLB's lock protocol.
func listenOwnedUnix(path string) (net.Listener, func() error, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, nil, errors.New("Unix socket path must be absolute")
	}
	lease, err := acquireFileLease(path + ".lock")
	if err != nil {
		return nil, nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = lease.Close()
		}
	}()
	if original, err := lstatOwnedPath(path); err == nil {
		if original.Mode()&os.ModeSocket == 0 {
			return nil, nil, errors.New("preserving existing path: not a socket")
		}
		if !ownedByCurrentUser(path, original) {
			return nil, nil, errors.New("preserving socket owned by another user")
		}
		conn, dialErr := net.DialTimeout("unix", path, 200*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			return nil, nil, fmt.Errorf("%w: Unix socket has a live listener", ErrDaemonOwned)
		}
		// Timeout, EACCES and other ambiguous failures are not stale evidence.
		if !errors.Is(dialErr, syscall.ECONNREFUSED) {
			return nil, nil, fmt.Errorf("cannot establish that existing socket is stale: %w", dialErr)
		}
		if err := removeOwnedPath(path, original); err != nil {
			return nil, nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, nil, fmt.Errorf("creating Unix socket: %w", err)
	}
	// net.UnixListener otherwise unlinks by pathname on Close, even when the
	// path was replaced after Listen. Only our inode-checked cleanup unlinks.
	ln.SetUnlinkOnClose(false)
	original, err := lstatOwnedPath(path)
	if err != nil {
		_ = ln.Close()
		return nil, nil, err
	}
	var once sync.Once
	var cleanupErr error
	cleanup := func() error {
		once.Do(func() {
			_ = ln.Close()
			cleanupErr = errors.Join(removeOwnedPath(path, original), lease.Close())
		})
		return cleanupErr
	}
	if err := protectSocket(path, original); err != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("setting socket permissions: %w", err)
	}
	keep = true
	return ln, cleanup, nil
}
