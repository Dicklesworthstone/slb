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
