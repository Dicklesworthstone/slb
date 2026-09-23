//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package daemon

import (
	"errors"
	"os"
)

func ownedByCurrentUser(os.FileInfo) bool { return false }

func acquireFileLease(string) (*fileLease, error) {
	return nil, errors.New("exclusive daemon ownership is unsupported on this platform")
}
