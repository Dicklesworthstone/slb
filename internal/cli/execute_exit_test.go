package cli

import (
	"errors"
	"fmt"
	"testing"
)

func TestCommandExitError(t *testing.T) {
	for _, tc := range []struct{ code, want int }{{1, 1}, {42, 42}, {255, 255}, {-1, 1}, {0, 1}, {256, 1}} {
		err := fmt.Errorf("wrapped: %w", commandExitError{code: tc.code})
		var exitErr interface{ ExitCode() int }
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != tc.want {
			t.Errorf("exit code %d: %v, want %d", tc.code, err, tc.want)
		}
	}
}
