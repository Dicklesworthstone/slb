package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrUnboundCommandCWD means the invocation depends on the executor's ambient
// directory rather than a directory bound into the reviewed command hash.
var ErrUnboundCommandCWD = errors.New("command working directory must be absolute and contain no NUL bytes")

// bindCommandCWD captures a missing directory at admission, not execution.
// Explicit relative directories are rejected: silently cleaning link/.. while
// making it absolute could change its filesystem meaning. Absolute spellings
// are retained verbatim for the command hash and the eventual chdir.
// This does not pin directory inodes or prevent later filesystem changes.
func bindCommandCWD(cwd string) (string, error) {
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("capturing command working directory: %w", err)
		}
	}
	if err := validateCommandCWD(cwd); err != nil {
		return "", err
	}
	return cwd, nil
}

// validateCommandCWD never fills in an old request's missing context. Doing so
// at approval/execution time would give an existing signature a new meaning.
func validateCommandCWD(cwd string) error {
	if !filepath.IsAbs(cwd) || strings.IndexByte(cwd, 0) >= 0 {
		return ErrUnboundCommandCWD
	}
	return nil
}
