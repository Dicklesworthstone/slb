package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestCommandStartupAcknowledgment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command fixture")
	}
	log := filepath.Join(t.TempDir(), "execution.log")
	spec := &db.CommandSpec{Raw: "printf startup-output; exit 7", Shell: true, Cwd: t.TempDir()}
	t.Setenv("SHELL", "/bin/sh")
	calls := 0
	result, err := runCommand(context.Background(), spec, log, nil, func(pid int) error {
		calls++
		if pid <= 0 {
			return errors.New("missing command PID")
		}
		data, err := os.ReadFile(log)
		if err != nil {
			return err
		}
		if !strings.Contains(string(data), fmt.Sprintf("[started pid=%d]", pid)) {
			return errors.New("PID was not logged before acknowledgment")
		}
		return nil
	})
	if err != nil || result == nil || result.ExitCode != 7 || calls != 1 || !strings.Contains(result.Output, "startup-output") {
		t.Fatalf("startup lost command outcome: %+v %v calls=%d", result, err, calls)
	}
}

func TestCommandFailedStartupNeverAcknowledged(t *testing.T) {
	called := false
	spec := &db.CommandSpec{Raw: "missing", Argv: []string{filepath.Join(t.TempDir(), "missing-executable")}}
	result, err := runCommand(context.Background(), spec, "", nil, func(int) error { called = true; return nil })
	if result != nil || err == nil || called {
		t.Fatalf("failed start was acknowledged: %+v %v called=%t", result, err, called)
	}
}

func TestCommandFailedAcknowledgmentCancelsAndReaps(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command fixture")
	}
	t.Setenv("SHELL", "/bin/sh")
	failure := errors.New("startup reader disappeared")
	start := time.Now()
	result, err := runCommand(context.Background(), &db.CommandSpec{Raw: "sleep 30", Shell: true}, "", nil, func(int) error { return failure })
	if result == nil || !errors.Is(err, failure) || time.Since(start) > 3*time.Second {
		t.Fatalf("failed acknowledgment did not cancel/reap: %+v %v", result, err)
	}
}

func TestCommandCancellationBeforeStartHasNoAcknowledgment(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	result, err := runCommand(ctx, &db.CommandSpec{Raw: "not-run"}, "", nil, func(int) error { called = true; return nil })
	if result != nil || !errors.Is(err, context.Canceled) || called {
		t.Fatalf("canceled start acknowledged: %+v %v", result, err)
	}
}
