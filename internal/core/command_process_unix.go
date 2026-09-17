//go:build unix

package core

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func commandSignals() []os.Signal { return []os.Signal{os.Interrupt, syscall.SIGTERM} }

// configureCommandCancellation isolates noninteractive commands so cancelling
// a shell also terminates its pipelines and ordinary descendant processes.
// Interactive commands retain their foreground terminal group: moving them
// without a job-control handoff would suspend reads with SIGTTIN.
// This is lifecycle management, not containment of deliberate setsid/setpgid
// escapes or descendants that change credentials beyond our signal authority.
func configureCommandCancellation(cmd *exec.Cmd, interactive bool) func() error {
	if interactive {
		return nil
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stop := func() error {
		if cmd.Process == nil || cmd.Process.Pid <= 0 {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.Cancel = stop
	return stop
}
