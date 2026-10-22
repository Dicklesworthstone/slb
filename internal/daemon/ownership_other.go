//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly && !windows

package daemon

import (
	"errors"
	"os"
	"syscall"
)

func ownedByCurrentUser(string, os.FileInfo) bool { return false }

func lstatOwnedPath(path string) (os.FileInfo, error) { return os.Lstat(path) }

func protectSocket(string, os.FileInfo) error {
	return errors.New("safe daemon sockets are unsupported on this platform")
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

func openPIDFile(string, int) (*os.File, os.FileInfo, error) {
	return nil, nil, errors.New("safe daemon PID files are unsupported on this platform")
}

func acquireFileLease(string) (*fileLease, error) {
	return nil, errors.New("exclusive daemon ownership is unsupported on this platform")
}
