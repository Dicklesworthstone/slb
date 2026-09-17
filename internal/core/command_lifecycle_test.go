package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

// Use the test binary itself so lifecycle tests do not depend on Unix tools.
func TestCommandLifecycleHelper(t *testing.T) {
	for i, arg := range os.Args {
		if arg != "--slb-command-lifecycle-helper" || i+1 >= len(os.Args) {
			continue
		}
		switch os.Args[i+1] {
		case "wait":
			fmt.Fprintln(os.Stdout, "ready-before-cancel")
			time.Sleep(time.Minute)
			os.Exit(0)
		case "exit":
			fmt.Fprintln(os.Stderr, "nonzero-output")
			os.Exit(23)
		}
		t.Fatal("unknown helper mode")
	}
}

func lifecycleCommand(t *testing.T, mode string) *db.CommandSpec {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &db.CommandSpec{
		Raw:  "slb lifecycle test helper",
		Argv: []string{binary, "-test.run=^TestCommandLifecycleHelper$", "--", "--slb-command-lifecycle-helper", mode},
	}
}

func TestRunCommandDeadlineReturnsPartialResult(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	logPath := filepath.Join(t.TempDir(), "timeout.log")
	result, err := RunCommand(ctx, lifecycleCommand(t, "wait"), logPath, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error, got result=%+v err=%v", result, err)
	}
	if result == nil || result.ExitCode == 0 || result.Duration <= 0 {
		t.Fatalf("missing failed-process metadata: %+v", result)
	}
	if !strings.Contains(result.Output, "ready-before-cancel") {
		t.Fatalf("lost partial output: %q", result.Output)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ready-before-cancel", "[started pid=", "Exit Code:", "Error: context deadline exceeded", "Completed:"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("execution log missing %q", want)
		}
	}
}

type cancelOnOutput struct {
	bytes.Buffer
	cancel context.CancelFunc
}

func (w *cancelOnOutput) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	w.cancel()
	return n, err
}

func TestRunCommandCancellationReturnsPartialResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &cancelOnOutput{cancel: cancel}
	result, err := RunCommand(ctx, lifecycleCommand(t, "wait"), "", stream)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got result=%+v err=%v", result, err)
	}
	if result == nil || result.ExitCode == 0 || result.Output != stream.String() {
		t.Fatalf("lost cancellation result: %+v, stream=%q", result, stream.String())
	}
}

func TestRunCommandCanceledBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := RunCommand(ctx, lifecycleCommand(t, "wait"), "", nil)
	if result != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected no started process and cancellation, got %+v, %v", result, err)
	}
}

func TestRunCommandNonzeroExitIsNotCancellation(t *testing.T) {
	result, err := RunCommand(context.Background(), lifecycleCommand(t, "exit"), "", nil)
	if err != nil || result == nil || result.ExitCode != 23 || !strings.Contains(result.Output, "nonzero-output") {
		t.Fatalf("expected ordinary exit 23 with output, got %+v, %v", result, err)
	}
}

func TestRunCommandStartFailureHasNoResult(t *testing.T) {
	spec := &db.CommandSpec{Argv: []string{filepath.Join(t.TempDir(), "missing-executable")}}
	result, err := RunCommand(context.Background(), spec, "", nil)
	if result != nil || err == nil {
		t.Fatalf("expected start failure without exit metadata, got %+v, %v", result, err)
	}
}

func TestRunCommandNilSpecification(t *testing.T) {
	if result, err := RunCommand(context.Background(), nil, "", nil); result != nil || err == nil {
		t.Fatalf("expected validation failure, got %+v, %v", result, err)
	}
}
