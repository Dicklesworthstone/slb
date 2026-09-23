package daemon

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

var ErrDaemonOwned = errors.New("daemon resource is already owned")

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
	current, err := os.Lstat(path)
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
	if original, err := os.Lstat(path); err == nil {
		if original.Mode()&os.ModeSocket == 0 || !ownedByCurrentUser(original) {
			return nil, nil, errors.New("preserving existing non-socket or foreign socket")
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
	original, err := os.Lstat(path)
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
	if err := os.Chmod(path, 0600); err != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("setting socket permissions: %w", err)
	}
	keep = true
	return ln, cleanup, nil
}
