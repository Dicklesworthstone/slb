//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func ownedByCurrentUser(_ string, info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func lstatOwnedPath(path string) (os.FileInfo, error) { return os.Lstat(path) }

func protectSocket(path string, original os.FileInfo) error {
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(original, current) || current.Mode()&os.ModeSocket == 0 {
		return errors.New("socket path changed before setting permissions")
	}
	return os.Chmod(path, 0600)
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	defer func() { _ = proc.Release() }()
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// openPIDFile never follows a symlink or blocks on a FIFO. Existing readable
// PID files remain compatible, but a foreign or writable-by-others file cannot
// be trusted to identify a daemon.
func openPIDFile(path string, flags int) (*os.File, os.FileInfo, error) {
	fd, err := syscall.Open(path, flags|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, nil, &os.PathError{Op: "open pid file", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || !ownedByCurrentUser(path, info) || info.Mode().Perm()&0022 != 0 || !ok || stat.Nlink != 1 {
			err = errors.New("pid file must be an owned regular file with one link and no group/other write permission")
		}
	}
	if err == nil {
		current, statErr := os.Lstat(path)
		if statErr != nil || !os.SameFile(info, current) {
			err = errors.New("pid file path changed while opening it")
		}
	}
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return file, info, nil
}

func openDiscoveryFile(path string, flags int) (*os.File, os.FileInfo, error) {
	file, info, err := openPIDFile(path, flags)
	if err != nil {
		return nil, nil, err
	}
	if err := validateRuntimePermissions(path, info, true); err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return file, info, nil
}

func validateRuntimePermissions(_ string, info os.FileInfo, private bool) error {
	mask := os.FileMode(0022)
	if private {
		mask = 0077
	}
	if info.Mode().Perm()&mask != 0 {
		return errors.New("unsafe daemon runtime permissions")
	}
	return nil
}

// Ancestors may be owned by this user or root. A shared writable directory
// is safe only with the sticky bit (e.g. /tmp), which prevents another user
// from replacing the owned socket. Resolve platform aliases such as /var
// before inspecting them; the socket itself is never followed as a symlink.
func validateRuntimeParents(directory string) error {
	remaining := 512
	return validateRuntimeAncestors(directory, &remaining)
}

func validateRuntimeAncestors(directory string, remaining *int) error {
	for dir := filepath.Clean(directory); ; dir = filepath.Dir(dir) {
		*remaining--
		if *remaining < 0 {
			return errors.New("daemon runtime directory has too many links or ancestors")
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) {
			return errors.New("unsafe daemon runtime directory")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(dir)
			if err != nil {
				return err
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(dir), target)
			}
			// Check both the target chain and the lexical containing directory.
			// Resolving first alone could hide a writable alias directory.
			if err := validateRuntimeAncestors(target, remaining); err != nil {
				return err
			}
		} else if !info.IsDir() || (info.Mode().Perm()&0022 != 0 && info.Mode()&os.ModeSticky == 0) {
			return errors.New("unsafe daemon runtime directory")
		}
		if filepath.Dir(dir) == dir {
			return nil
		}
	}
}

func acquireFileLease(path string) (*fileLease, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("daemon lease path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	// NOFOLLOW rejects symlinks. NONBLOCK prevents a hostile FIFO from
	// hanging before we can check its type. Never truncate a contender's file.
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, fmt.Errorf("opening daemon lease: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	keep := false
	defer func() {
		if !keep {
			_ = file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ownedByCurrentUser(path, info) || info.Mode().Perm()&0077 != 0 || !ok || stat.Nlink != 1 {
		return nil, errors.New("daemon lease must be a private, owned regular file with one link")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("%w: %s", ErrDaemonOwned, path)
		}
		return nil, fmt.Errorf("locking daemon lease: %w", err)
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		return nil, errors.New("daemon lease path changed while acquiring ownership")
	}
	keep = true
	return &fileLease{file: file}, nil
}
