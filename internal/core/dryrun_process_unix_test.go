//go:build unix

package core

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestDryRunCancellationStopsGrandchild(t *testing.T) {
	directory := prepareProcessFixture(t, "tree")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ready := make(chan bool, 1)
	go func() {
		started := waitProcessHeartbeat(ctx, directory)
		ready <- started
		cancel()
	}()
	output, err := runDryRunProcess(ctx, processFixtureSpec(t).Argv, directory)
	if !<-ready {
		t.Fatal("fixture did not start")
	}
	if !errors.Is(err, context.Canceled) || !strings.Contains(output, "descendant ready") {
		t.Fatalf("preview lost cancellation/output: %q, %v", output, err)
	}
	assertDescendantStopped(t, directory)
}

func TestDryRunTimeoutStopsGrandchild(t *testing.T) {
	directory := prepareProcessFixture(t, "tree")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := runDryRunProcess(ctx, processFixtureSpec(t).Argv, directory)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("preview lost its deadline: %v", err)
	}
	assertDescendantStopped(t, directory)
}

func TestDryRunOrphanPipeDoesNotHang(t *testing.T) {
	for _, mode := range []string{"orphan", "failed-orphan"} {
		t.Run(mode, func(t *testing.T) {
			directory := prepareProcessFixture(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			started := time.Now()
			_, err := runDryRunProcess(ctx, processFixtureSpec(t).Argv, directory)
			if mode == "orphan" && !errors.Is(err, exec.ErrWaitDelay) {
				t.Fatalf("expected inherited-pipe timeout, got %v", err)
			}
			if mode == "failed-orphan" && (err == nil || !strings.Contains(err.Error(), "code 7")) {
				t.Fatalf("lost parent exit status: %v", err)
			}
			if time.Since(started) > 4*time.Second {
				t.Fatal("preview waited for the outer deadline instead of bounding pipe draining")
			}
			assertDescendantStopped(t, directory)
		})
	}
}

func TestDryRunProcessRejectsCancelledOrInvalidInput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runDryRunProcess(ctx, []string{"not-an-executable"}, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled preview tried to start: %v", err)
	}
	for _, argv := range [][]string{nil, {""}, {"/definitely/not/an/slb-executable"}} {
		if _, err := runDryRunProcess(context.Background(), argv, ""); err == nil {
			t.Fatalf("invalid preview accepted: %v", argv)
		}
	}
}
