package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestExecutionStartObservesCommittedClaim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	database, request, session := policyExecutionFixture(t)
	t.Setenv("SHELL", "/bin/sh")
	other, err := db.OpenWithOptions(database.Path(), db.OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	calls := 0
	result, err := NewExecutor(database, NewPatternEngine()).ExecuteApprovedRequest(context.Background(), ExecuteOptions{
		RequestID: request.ID, SessionID: session.ID, SuppressOutput: true,
		LogDir: filepath.Join(request.ProjectPath, ".slb", "logs"),
		OnStarted: func(start ExecutionStart) error {
			calls++
			stored, err := other.GetRequest(request.ID)
			if err != nil {
				return err
			}
			if stored.Status != db.StatusExecuting || stored.Execution == nil || stored.Execution.LogPath != start.LogPath ||
				stored.Execution.ExecutedBySessionID != session.ID || start.CommandHash != request.Command.Hash || start.RequestID != request.ID || start.PID <= 0 {
				return errors.New("startup preceded committed execution identity")
			}
			data, err := os.ReadFile(start.LogPath)
			if err != nil || !strings.Contains(string(data), "[started pid=") {
				return errors.New("startup log unavailable")
			}
			return nil
		},
	})
	if err != nil || calls != 1 || result == nil || result.Request.Status != db.StatusExecuted {
		t.Fatalf("bad start lifecycle: %+v %v calls=%d", result, err, calls)
	}
	if stored, err := other.GetRequest(request.ID); err != nil || stored.Status != db.StatusExecuted {
		t.Fatalf("completion not durable: %+v %v", stored, err)
	}
}

func TestExecutionStartupFailureCannotReissueApproval(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	database, request, session := policyExecutionFixture(t)
	sentinel := errors.New("startup confirmation failed")
	executor := NewExecutor(database, NewPatternEngine())
	result, err := executor.ExecuteApprovedRequest(context.Background(), ExecuteOptions{
		RequestID: request.ID, SessionID: session.ID, SuppressOutput: true,
		LogDir:    filepath.Join(request.ProjectPath, ".slb", "logs"),
		OnStarted: func(ExecutionStart) error { return sentinel },
	})
	if result == nil || !errors.Is(err, sentinel) || result.Request.Status != db.StatusExecutionFailed {
		t.Fatalf("failed acknowledgment misreported: %+v %v", result, err)
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil || stored.Status != db.StatusExecutionFailed || stored.Execution == nil {
		t.Fatalf("failed startup not durable: %+v %v", stored, err)
	}
	called := false
	_, err = executor.ExecuteApprovedRequest(context.Background(), ExecuteOptions{RequestID: request.ID, SessionID: session.ID, OnStarted: func(ExecutionStart) error { called = true; return nil }})
	if err == nil || called {
		t.Fatal("consumed approval replayed")
	}
}

func TestExecutionRejectedProofNeverAcknowledged(t *testing.T) {
	for _, fault := range []string{"signature", "hash", "expired", "policy", "background"} {
		t.Run(fault, func(t *testing.T) {
			database, request, session := policyExecutionFixture(t)
			called := false
			opts := ExecuteOptions{RequestID: request.ID, SessionID: session.ID, SuppressOutput: true,
				LogDir: filepath.Join(request.ProjectPath, "must-not-create"), OnStarted: func(ExecutionStart) error { called = true; return nil }}
			switch fault {
			case "signature":
				if _, err := database.Exec("UPDATE reviews SET signature = 'invalid' WHERE request_id = ?", request.ID); err != nil {
					t.Fatal(err)
				}
			case "hash":
				opts.ExpectedCommandHash = strings.Repeat("b", 64)
			case "expired":
				if _, err := database.Exec("UPDATE requests SET approval_expires_at = '2000-01-01T00:00:00Z' WHERE id = ?", request.ID); err != nil {
					t.Fatal(err)
				}
			case "policy":
				writeExecutionPolicy(t, request.ProjectPath, "[patterns.dangerous]\nmin_approvals = 3\n")
			case "background":
				opts.Background = true
			}
			result, err := NewExecutor(database, NewPatternEngine()).ExecuteApprovedRequest(context.Background(), opts)
			if result != nil || err == nil || called {
				t.Fatalf("rejected execution acknowledged: %+v %v", result, err)
			}
			assertPolicyUnclaimed(t, database, request.ID)
			if _, err := os.Stat(opts.LogDir); !os.IsNotExist(err) {
				t.Fatal("rejected execution created a log")
			}
		})
	}
}
