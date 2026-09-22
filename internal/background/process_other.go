//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package background

import (
	"errors"
	"os"
	"os/exec"
)

func supported() error                             { return errors.New("background execution is unsupported on this platform") }
func detach(*exec.Cmd) error                       { return supported() }
func terminate(process *os.Process) error          { return process.Kill() }
func forceTerminate(process *os.Process) error     { return process.Kill() }
func OpenWorkerFiles() (*os.File, *os.File, error) { return nil, nil, supported() }
