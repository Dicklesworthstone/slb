//go:build !unix

package core

import (
	"os"
	"os/exec"
)

func commandSignals() []os.Signal { return []os.Signal{os.Interrupt} }

// Other platforms retain CommandContext's direct-child cancellation. In
// particular, Windows process-tree termination needs a Job Object lifecycle.
func configureCommandCancellation(_ *exec.Cmd, _ bool) func() error { return nil }
