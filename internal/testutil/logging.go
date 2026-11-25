package testutil

import (
	"io"
	"os"
	"testing"

	"github.com/charmbracelet/log"
)

// TestLogger returns a structured logger suitable for tests.
//
// By default it discards output unless `go test -v` is used.
func TestLogger(t *testing.T) *log.Logger {
	t.Helper()

	out := io.Discard
	if testing.Verbose() {
		out = os.Stderr
	}

	return log.NewWithOptions(out, log.Options{
		Level:  log.DebugLevel,
		Prefix: t.Name(),
	})
}
