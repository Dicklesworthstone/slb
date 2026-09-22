//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package background

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func supported() error { return nil }
func detach(command *exec.Cmd) error {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return nil
}
func terminate(process *os.Process) error      { return syscall.Kill(-process.Pid, syscall.SIGTERM) }
func forceTerminate(process *os.Process) error { return syscall.Kill(-process.Pid, syscall.SIGKILL) }

// OpenWorkerFiles opens only the launcher-provided pipes. Close-on-exec is
// essential: the reviewed command must not inherit or keep the handshake alive.
func OpenWorkerFiles() (*os.File, *os.File, error) {
	for _, fd := range []int{3, 4} {
		var info syscall.Stat_t
		if err := syscall.Fstat(fd, &info); err != nil || info.Mode&syscall.S_IFMT != syscall.S_IFIFO {
			return nil, nil, errors.New("background worker requires inherited launcher pipes")
		}
	}
	for _, fd := range []int{3, 4} {
		// os.NewFile registers an inherited descriptor with Go's poller only
		// when it is nonblocking. This makes deadlines/cancellation effective.
		if err := syscall.SetNonblock(fd, true); err != nil {
			return nil, nil, err
		}
		syscall.CloseOnExec(fd)
	}
	input := os.NewFile(3, "background-job")
	output := os.NewFile(4, "background-receipt")
	if err := input.SetReadDeadline(time.Now().Add(StartTimeout)); err != nil {
		_ = input.Close()
		_ = output.Close()
		return nil, nil, err
	}
	if err := output.SetWriteDeadline(time.Now().Add(StartTimeout)); err != nil {
		_ = input.Close()
		_ = output.Close()
		return nil, nil, err
	}
	return input, output, nil
}
