package daemon

import (
	"bytes"
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
	return claimPIDFileForRuntime(path, pid, nil)
}

// Only the project owner may recover a recorded PID that has been reused.
// Close cannot release that authority while proof or mutation is in progress.
func (owner *projectRuntimeOwner) claimPIDFile(path string, pid int) (*pidFileOwner, error) {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.lease == nil {
		return nil, errors.New("project runtime ownership has been released")
	}
	return claimPIDFileForRuntime(path, pid, owner)
}

func claimPIDFileForRuntime(path string, pid int, runtime *projectRuntimeOwner) (*pidFileOwner, error) {
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
	if err := validateRuntimeParents(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if runtime != nil {
		if err := runtime.verifyRecord(); err != nil {
			return nil, err
		}
		if runtime.recordInfo != nil {
			// Repeat the old-generation proof even when the selected TMPDIR
			// differs. Keep its resource leases through the new PID write.
			release, err := runtime.lockRetiredRuntime(lease)
			if err != nil {
				return nil, err
			}
			defer func() { _ = release() }()
		}
	}
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
		// Directory aliases may spell the same PID path differently. Bind
		// recovery to the actual no-follow file identity, never a leaf link.
		if runtime == nil || runtime.retired == nil || previous != runtime.record.PID ||
			!sameRuntimeFile(info, runtime.retired.pid.info) || !bytes.Equal(data, runtime.retired.pid.data) {
			return nil, fmt.Errorf("%w: daemon already running (pid=%d)", ErrDaemonOwned, previous)
		}
	}
	if err := verifyFileLease(lease); err != nil {
		return nil, err
	}
	if runtime != nil {
		if err := runtime.verifyRecord(); err != nil {
			return nil, err
		}
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

// fileLeaseMatches validates the pathname without following the leaf. Inode
// identity permits trusted directory aliases but not symlink or hardlink
// substitution, including a second spelling of an already-held PID lease.
func fileLeaseMatches(lease *fileLease, path string) (bool, error) {
	if err := validateRuntimeParents(filepath.Dir(path)); err != nil {
		return false, err
	}
	original, err := lease.file.Stat()
	if err != nil {
		return false, err
	}
	file, current, err := openDiscoveryFile(path, os.O_RDONLY)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	return os.SameFile(original, current), nil
}

func verifyFileLease(lease *fileLease) error {
	matches, err := fileLeaseMatches(lease, lease.file.Name())
	if err != nil {
		return err
	}
	if !matches {
		return errors.New("daemon lease path changed during ownership recovery")
	}
	return nil
}

type runtimePIDSnapshot struct {
	info os.FileInfo
	data []byte
}

func readRuntimePIDSnapshot(path string) (runtimePIDSnapshot, error) {
	file, info, err := openPIDFile(path, os.O_RDONLY)
	if os.IsNotExist(err) {
		return runtimePIDSnapshot{}, nil
	}
	if err != nil {
		return runtimePIDSnapshot{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 129))
	if err != nil {
		return runtimePIDSnapshot{}, err
	}
	current, err := lstatOwnedPath(path)
	if err != nil || !os.SameFile(info, current) || info.Size() != current.Size() {
		return runtimePIDSnapshot{}, errors.New("pid file changed during ownership recovery")
	}
	return runtimePIDSnapshot{info: info, data: data}, nil
}

func sameRuntimeFile(first, second os.FileInfo) bool {
	if first == nil || second == nil {
		return first == nil && second == nil
	}
	return os.SameFile(first, second)
}

func (snapshot runtimePIDSnapshot) matches(other runtimePIDSnapshot) bool {
	return sameRuntimeFile(snapshot.info, other.info) && bytes.Equal(snapshot.data, other.data) &&
		(snapshot.info == nil || snapshot.info.Size() == other.info.Size())
}

type retiredRuntimeSnapshot struct {
	pid         runtimePIDSnapshot
	socket      os.FileInfo
	pidLease    os.FileInfo
	socketLease os.FileInfo
}

func (snapshot *retiredRuntimeSnapshot) matches(other retiredRuntimeSnapshot) bool {
	// A wholly absent old directory has no lease inode to preserve. Starting
	// in that directory may create new leases, but not change old PID/socket
	// evidence. Previously observed persistent locks must never be replaced.
	return snapshot.pid.matches(other.pid) && sameRuntimeFile(snapshot.socket, other.socket) &&
		(snapshot.pidLease == nil || sameRuntimeFile(snapshot.pidLease, other.pidLease)) &&
		(snapshot.socketLease == nil || sameRuntimeFile(snapshot.socketLease, other.socketLease))
}

// lockRetiredRuntime proves retirement without relying on PID liveness. It
// fences the recorded resources even when a new TMPDIR selects different
// paths. The selected PID lease may be reused only by verified file identity.
// The caller holds the project lease and retains the returned resource locks
// until any recovery mutation is complete.
func (owner *projectRuntimeOwner) lockRetiredRuntime(selectedPIDLease *fileLease) (func() error, error) {
	if err := owner.verifyRecord(); err != nil {
		return nil, err
	}
	if owner.recordInfo == nil {
		return nil, errors.New("ownership recovery requires a discovery record")
	}
	var acquiredPID, socketLease *fileLease
	release := func() error { return errors.Join(socketLease.Close(), acquiredPID.Close()) }
	keep := false
	defer func() {
		if !keep {
			_ = release()
		}
	}()
	pidLease, acquired, err := acquireRetiredRuntimeLease(owner.record.PIDFile, selectedPIDLease)
	if err != nil {
		return nil, err
	}
	if acquired {
		acquiredPID = pidLease
	}
	socketLease, _, err = acquireRetiredRuntimeLease(owner.record.SocketPath, nil)
	if err != nil {
		return nil, err
	}
	pid, err := readRuntimePIDSnapshot(owner.record.PIDFile)
	if err != nil {
		return nil, err
	}
	// A crash between PID publication and record publication can leave dead
	// or incomplete PID contents. Only conflicting live evidence blocks that
	// repair; a matching recorded PID can belong to an unrelated new process.
	if previous, err := parsePID(pid.data); err == nil && previous != owner.record.PID && processAlive(previous) {
		return nil, fmt.Errorf("%w: recorded PID file identifies a different live process", ErrDaemonOwned)
	}
	socket, err := verifyRetiredRuntimeSocket(owner.record.SocketPath)
	if err != nil {
		return nil, err
	}
	currentPID, err := readRuntimePIDSnapshot(owner.record.PIDFile)
	if err != nil || !pid.matches(currentPID) {
		return nil, errors.New("recorded pid file changed during ownership recovery")
	}
	pidInfo, err := verifyRetiredRuntimeLease(owner.record.PIDFile, pidLease)
	if err != nil {
		return nil, err
	}
	socketInfo, err := verifyRetiredRuntimeLease(owner.record.SocketPath, socketLease)
	if err != nil {
		return nil, err
	}
	if err := owner.verifyRecord(); err != nil {
		return nil, err
	}
	snapshot := retiredRuntimeSnapshot{pid: pid, socket: socket, pidLease: pidInfo, socketLease: socketInfo}
	if owner.retired != nil && !owner.retired.matches(snapshot) {
		return nil, errors.New("recorded runtime changed during ownership recovery")
	}
	owner.retired = &snapshot
	keep = true
	return release, nil
}

// Old service runtime directories may disappear at reboot. Validate the
// existing ancestry without recreating an obsolete namespace merely to lock
// it. A missing parent means neither its resource nor its old lease exists.
func validateRetiredRuntimeParents(path string) (bool, error) {
	present := true
	for dir, remaining := filepath.Dir(path), 512; remaining > 0; dir, remaining = filepath.Dir(dir), remaining-1 {
		if _, err := os.Lstat(dir); err == nil {
			return present, validateRuntimeParents(dir)
		} else if !os.IsNotExist(err) {
			return false, err
		}
		present = false
		if filepath.Dir(dir) == dir {
			break
		}
	}
	return false, errors.New("runtime has no existing trusted ancestor within the path limit")
}

func acquireRetiredRuntimeLease(path string, held *fileLease) (*fileLease, bool, error) {
	present, err := validateRetiredRuntimeParents(path)
	if err != nil || !present {
		return nil, false, err
	}
	if held != nil {
		if err := verifyFileLease(held); err != nil {
			return nil, false, err
		}
		matches, err := fileLeaseMatches(held, path+".lock")
		if err != nil {
			return nil, false, err
		}
		if matches {
			return held, false, nil
		}
	}
	lease, err := acquireFileLease(path + ".lock")
	return lease, err == nil, err
}

func verifyRetiredRuntimeLease(path string, lease *fileLease) (os.FileInfo, error) {
	if lease == nil {
		present, err := validateRetiredRuntimeParents(path)
		if err != nil {
			return nil, err
		}
		if present {
			return nil, errors.New("retired runtime directory appeared during ownership recovery")
		}
		return nil, nil
	}
	if err := verifyFileLease(lease); err != nil {
		return nil, err
	}
	matches, err := fileLeaseMatches(lease, path+".lock")
	if err != nil {
		return nil, err
	}
	if !matches {
		return nil, errors.New("recorded daemon lease changed during ownership recovery")
	}
	return lease.file.Stat()
}

// Only absence or an owned private socket returning ECONNREFUSED proves
// retirement. A live listener, timeout, permission failure, or changed inode
// is preserved. No protocol data is sent during this liveness check.
func verifyRetiredRuntimeSocket(path string) (os.FileInfo, error) {
	if _, err := validateRetiredRuntimeParents(path); err != nil {
		return nil, err
	}
	original, err := validateRuntimeSocket(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: recorded Unix socket has a live listener", ErrDaemonOwned)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return nil, fmt.Errorf("cannot establish that recorded socket is stale: %w", err)
	}
	current, err := validateRuntimeSocket(path)
	if err != nil || !os.SameFile(original, current) {
		return nil, errors.New("recorded socket changed during ownership recovery")
	}
	return original, nil
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
	if !validRuntimePath(path) || len(path) > maxUnixSocketPathBytes {
		return nil, nil, errors.New("Unix socket path must be absolute and at most 103 bytes")
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
	if err := validateRuntimeParents(filepath.Dir(path)); err != nil {
		return nil, nil, err
	}
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
