//go:build unix

package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"golang.org/x/term"
)

// TestCommandProcessHelper uses only disposable files and this test executable.
// A grandchild keeps making observable changes until its process group stops.
func TestCommandProcessHelper(t *testing.T) {
	mode := os.Getenv("SLB_TEST_PROCESS_HELPER")
	if mode == "" {
		return
	}
	directory := os.Getenv("SLB_TEST_PROCESS_DIR")
	if err := os.WriteFile(filepath.Join(directory, mode+".pid"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		os.Exit(2)
	}
	if mode == "writer" {
		file, err := os.OpenFile(filepath.Join(directory, "heartbeat"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			os.Exit(2)
		}
		fmt.Println("descendant ready")
		for {
			if _, err := file.Write([]byte("x")); err != nil {
				os.Exit(2)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	if mode == "supervisor" {
		// Deliberately use Background: RunCommand itself must catch SIGTERM
		// rather than relying on a particular CLI entry point's context.
		os.Setenv("SLB_TEST_PROCESS_HELPER", "tree")
		result, err := RunCommand(context.Background(), processFixtureSpec(t), "", nil)
		if !errors.Is(err, context.Canceled) || result == nil || !strings.Contains(result.Output, "descendant ready") {
			fmt.Fprintf(os.Stderr, "lost cancellation outcome: %+v, %v\n", result, err)
			os.Exit(3)
		}
		fmt.Println("cancellation outcome preserved")
		os.Exit(0)
	}
	childMode := "writer"
	if mode == "tree" {
		childMode = "branch"
	}
	child := exec.Command(os.Args[0], "-test.run=^TestCommandProcessHelper$")
	child.Env = append(os.Environ(), "SLB_TEST_PROCESS_HELPER="+childMode)
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		os.Exit(2)
	}
	if mode == "orphan" {
		os.Exit(0) // Writer still owns our inherited stdout/stderr pipe.
	}
	if mode == "failed-orphan" {
		os.Exit(7) // Wait reports ExitError, hiding the pipe timeout error.
	}
	_ = child.Wait()
	os.Exit(0)
}

func processFixtureSpec(t *testing.T) *db.CommandSpec {
	t.Helper()
	return &db.CommandSpec{Raw: "SLB process lifecycle fixture", Argv: []string{os.Args[0], "-test.run=^TestCommandProcessHelper$"}}
}

func prepareProcessFixture(t *testing.T, mode string) string {
	t.Helper()
	if term.IsTerminal(int(os.Stdin.Fd())) {
		t.Skip("noninteractive process-group fixture requires redirected stdin")
	}
	directory := t.TempDir()
	t.Setenv("SLB_TEST_PROCESS_HELPER", mode)
	t.Setenv("SLB_TEST_PROCESS_DIR", directory)
	// Clean up our known fixture processes even when testing broken code.
	t.Cleanup(func() {
		for _, name := range []string{"writer", "branch", "tree", "orphan", "failed-orphan", "supervisor"} {
			data, err := os.ReadFile(filepath.Join(directory, name+".pid"))
			if err == nil {
				if pid, err := strconv.Atoi(string(data)); err == nil && pid > 0 {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			}
		}
	})
	return directory
}

func waitProcessHeartbeat(ctx context.Context, directory string) bool {
	for {
		if info, err := os.Stat(filepath.Join(directory, "heartbeat")); err == nil && info.Size() > 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func assertDescendantStopped(t *testing.T, directory string) {
	t.Helper()
	// Allow an already-delivered SIGKILL to be scheduled before observing.
	time.Sleep(30 * time.Millisecond)
	before, err := os.Stat(filepath.Join(directory, "heartbeat"))
	if err != nil {
		t.Fatal("descendant never started:", err)
	}
	time.Sleep(100 * time.Millisecond)
	after, err := os.Stat(filepath.Join(directory, "heartbeat"))
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != after.Size() {
		t.Errorf("descendant kept changing files after command returned: %d -> %d", before.Size(), after.Size())
	}
}

func TestRunCommandCancellationStopsGrandchild(t *testing.T) {
	directory := prepareProcessFixture(t, "tree")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ready := make(chan bool, 1)
	go func() {
		started := waitProcessHeartbeat(ctx, directory)
		ready <- started
		cancel()
	}()
	result, err := RunCommand(ctx, processFixtureSpec(t), "", nil)
	if !<-ready {
		t.Fatal("fixture did not start")
	}
	if !errors.Is(err, context.Canceled) || result == nil || result.ExitCode == 0 {
		t.Fatalf("incorrect cancellation result: %+v, %v", result, err)
	}
	assertDescendantStopped(t, directory)
}

func TestRunCommandTimeoutStopsGrandchild(t *testing.T) {
	directory := prepareProcessFixture(t, "tree")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := RunCommand(ctx, processFixtureSpec(t), "", nil)
	if !errors.Is(err, context.DeadlineExceeded) || result == nil || result.ExitCode == 0 {
		t.Fatalf("incorrect timeout result: %+v, %v", result, err)
	}
	assertDescendantStopped(t, directory)
}

func TestRunCommandPipeTimeoutStopsOrphan(t *testing.T) {
	directory := prepareProcessFixture(t, "orphan")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := RunCommand(ctx, processFixtureSpec(t), "", nil)
	if !errors.Is(err, exec.ErrWaitDelay) || result == nil {
		t.Fatalf("expected bounded orphan-pipe wait: %+v, %v", result, err)
	}
	assertDescendantStopped(t, directory)
}

func TestRunCommandSIGTERMPreservesOutcomeAndStopsGrandchild(t *testing.T) {
	directory := prepareProcessFixture(t, "supervisor")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	supervisor := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCommandProcessHelper$")
	supervisor.Env = os.Environ()
	var output bytes.Buffer
	supervisor.Stdout, supervisor.Stderr = &output, &output
	if err := supervisor.Start(); err != nil {
		t.Fatal(err)
	}
	if !waitProcessHeartbeat(ctx, directory) {
		_ = supervisor.Process.Kill()
		_ = supervisor.Wait()
		t.Fatal("fixture did not start")
	}
	if err := supervisor.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Wait(); err != nil || !strings.Contains(output.String(), "cancellation outcome preserved") {
		t.Fatalf("supervisor lost the outcome instead of returning it: %v, %s", err, output.String())
	}
	assertDescendantStopped(t, directory)
}

func TestRunCommandFailedParentStopsOrphan(t *testing.T) {
	directory := prepareProcessFixture(t, "failed-orphan")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := RunCommand(ctx, processFixtureSpec(t), "", nil)
	if err != nil || result == nil || result.ExitCode != 7 {
		t.Fatalf("lost nonzero parent exit: %+v, %v", result, err)
	}
	assertDescendantStopped(t, directory)
}

func TestInteractiveCommandKeepsForegroundGroup(t *testing.T) {
	command := exec.CommandContext(context.Background(), "unused")
	called := false
	command.Cancel = func() error { called = true; return nil }
	if stop := configureCommandCancellation(command, true); stop != nil || command.SysProcAttr != nil {
		t.Fatal("interactive command would lose its foreground terminal group")
	}
	_ = command.Cancel()
	if !called {
		t.Fatal("interactive cancellation callback was replaced")
	}
}

func TestProcessGroupCancellationBeforeStartIsHarmless(t *testing.T) {
	command := exec.CommandContext(context.Background(), "unused")
	stop := configureCommandCancellation(command, false)
	if !errors.Is(stop(), os.ErrProcessDone) {
		t.Fatal("unstarted command must not signal the caller's process group")
	}
}
