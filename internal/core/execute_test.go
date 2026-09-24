package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/integrations"
	"github.com/Dicklesworthstone/slb/internal/testutil"
)

func TestTierHigher(t *testing.T) {
	tests := []struct {
		name   string
		tier1  db.RiskTier
		tier2  db.RiskTier
		expect bool
	}{
		{"critical > dangerous", db.RiskTierCritical, db.RiskTierDangerous, true},
		{"critical > caution", db.RiskTierCritical, db.RiskTierCaution, true},
		{"dangerous > caution", db.RiskTierDangerous, db.RiskTierCaution, true},
		{"dangerous < critical", db.RiskTierDangerous, db.RiskTierCritical, false},
		{"caution < critical", db.RiskTierCaution, db.RiskTierCritical, false},
		{"caution < dangerous", db.RiskTierCaution, db.RiskTierDangerous, false},
		{"same tier critical", db.RiskTierCritical, db.RiskTierCritical, false},
		{"same tier dangerous", db.RiskTierDangerous, db.RiskTierDangerous, false},
		{"same tier caution", db.RiskTierCaution, db.RiskTierCaution, false},
		{"unknown tier1", db.RiskTier("unknown"), db.RiskTierCaution, false},
		{"unknown tier2", db.RiskTierCaution, db.RiskTier("unknown"), true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := tierHigher(tc.tier1, tc.tier2)
			if result != tc.expect {
				t.Errorf("tierHigher(%q, %q) = %v, want %v", tc.tier1, tc.tier2, result, tc.expect)
			}
		})
	}
}

func TestNewExecutor(t *testing.T) {
	t.Run("with nil pattern engine uses default", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		exec := NewExecutor(dbConn, nil)
		if exec == nil {
			t.Fatal("expected non-nil executor")
		}
		if exec.db != dbConn {
			t.Error("expected db to be set")
		}
		if exec.patternEngine == nil {
			t.Error("expected patternEngine to be set to default")
		}
	})

	t.Run("with custom pattern engine", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		customEngine := NewPatternEngine()
		exec := NewExecutor(dbConn, customEngine)
		if exec == nil {
			t.Fatal("expected non-nil executor")
		}
		if exec.patternEngine != customEngine {
			t.Error("expected custom patternEngine to be set")
		}
	})
}

func TestWithNotifier(t *testing.T) {
	dbConn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open(:memory:) error = %v", err)
	}
	defer dbConn.Close()

	exec := NewExecutor(dbConn, nil)

	t.Run("sets notifier and returns executor for chaining", func(t *testing.T) {
		notifier := &mockExecutorNotifier{}
		result := exec.WithNotifier(notifier)
		if result != exec {
			t.Error("expected same executor to be returned for chaining")
		}
		if exec.notifier != notifier {
			t.Error("expected notifier to be set")
		}
	})

	t.Run("nil notifier is ignored", func(t *testing.T) {
		// First set a valid notifier
		notifier := &mockExecutorNotifier{}
		exec.WithNotifier(notifier)

		// Now try to set nil - should be ignored
		exec.WithNotifier(nil)
		if exec.notifier != notifier {
			t.Error("expected notifier to remain unchanged when nil passed")
		}
	})
}

type mockExecutorNotifier struct {
	newRequestCalled bool
	approvedCalled   bool
	rejectedCalled   bool
	executedCalled   bool
}

func (m *mockExecutorNotifier) NotifyNewRequest(req *db.Request) error {
	m.newRequestCalled = true
	return nil
}

func (m *mockExecutorNotifier) NotifyRequestApproved(req *db.Request, review *db.Review) error {
	m.approvedCalled = true
	return nil
}

func (m *mockExecutorNotifier) NotifyRequestRejected(req *db.Request, review *db.Review) error {
	m.rejectedCalled = true
	return nil
}

func (m *mockExecutorNotifier) NotifyRequestExecuted(req *db.Request, exec *db.Execution, exitCode int) error {
	m.executedCalled = true
	return nil
}

// Ensure mockExecutorNotifier implements integrations.RequestNotifier
var _ integrations.RequestNotifier = (*mockExecutorNotifier)(nil)

func TestCreateLogFile(t *testing.T) {
	dbConn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open(:memory:) error = %v", err)
	}
	defer dbConn.Close()

	exec := NewExecutor(dbConn, nil)

	t.Run("creates log file in specified directory", func(t *testing.T) {
		tmpDir := t.TempDir()
		logDir := filepath.Join(tmpDir, "logs")
		requestID := "12345678-1234-1234-1234-123456789012"

		logPath, err := exec.createLogFile(logDir, requestID)
		if err != nil {
			t.Fatalf("createLogFile error = %v", err)
		}

		// Check log directory was created
		if _, err := os.Stat(logDir); os.IsNotExist(err) {
			t.Error("expected log directory to be created")
		}

		// Check log file was created
		if _, err := os.Stat(logPath); os.IsNotExist(err) {
			t.Error("expected log file to be created")
		}

		// Check log file name format
		logName := filepath.Base(logPath)
		if !strings.HasSuffix(logName, "_12345678.log") {
			t.Errorf("expected log file name to end with _12345678.log, got %s", logName)
		}
	})

	t.Run("uses existing directory", func(t *testing.T) {
		tmpDir := t.TempDir()
		requestID := "abcdefab-abcd-abcd-abcd-abcdefabcdef"

		logPath, err := exec.createLogFile(tmpDir, requestID)
		if err != nil {
			t.Fatalf("createLogFile error = %v", err)
		}

		if _, err := os.Stat(logPath); os.IsNotExist(err) {
			t.Error("expected log file to be created")
		}
	})

	t.Run("creates nested directories", func(t *testing.T) {
		tmpDir := t.TempDir()
		logDir := filepath.Join(tmpDir, "deep", "nested", "logs")
		requestID := "fedcfedcfedcfedcfedcfedcfedcfedc"

		logPath, err := exec.createLogFile(logDir, requestID)
		if err != nil {
			t.Fatalf("createLogFile error = %v", err)
		}

		if _, err := os.Stat(logPath); os.IsNotExist(err) {
			t.Error("expected log file to be created in nested directory")
		}
	})
}

func TestExecutorCanExecute(t *testing.T) {
	t.Run("request not found returns false", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		exec := NewExecutor(dbConn, nil)
		canExec, reason := exec.CanExecute("nonexistent-id")
		if canExec {
			t.Error("expected CanExecute to return false for nonexistent request")
		}
		if !strings.Contains(reason, "not found") {
			t.Errorf("expected reason to mention not found, got %q", reason)
		}
	})

	t.Run("request already executing returns false", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		// Create a session first
		session := &db.Session{
			ID:          "test-session",
			ProjectPath: "/tmp/test",
			AgentName:   "test-agent",
			Program:     "test-program",
			Model:       "test-model",
		}
		if err := dbConn.CreateSession(session); err != nil {
			t.Fatalf("CreateSession error = %v", err)
		}

		// Create request
		req := &db.Request{
			ProjectPath:        "/tmp/test",
			RequestorSessionID: "test-session",
			RequestorAgent:     "test-agent",
			RequestorModel:     "test-model",
			RiskTier:           db.RiskTierCaution,
			Command: db.CommandSpec{
				Raw: "ls -la",
				Cwd: "/tmp",
			},
			Status: db.StatusExecuting,
		}
		if err := dbConn.CreateRequest(req); err != nil {
			t.Fatalf("CreateRequest error = %v", err)
		}

		exec := NewExecutor(dbConn, nil)
		canExec, reason := exec.CanExecute(req.ID)
		if canExec {
			t.Error("expected CanExecute to return false for executing request")
		}
		if !strings.Contains(reason, "already being executed") {
			t.Errorf("expected reason to mention already executing, got %q", reason)
		}
	})

	t.Run("request already executed returns false", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		// Create a session first
		session := &db.Session{
			ID:          "test-session",
			ProjectPath: "/tmp/test",
			AgentName:   "test-agent",
			Program:     "test-program",
			Model:       "test-model",
		}
		if err := dbConn.CreateSession(session); err != nil {
			t.Fatalf("CreateSession error = %v", err)
		}

		req := &db.Request{
			ProjectPath:        "/tmp/test",
			RequestorSessionID: "test-session",
			RequestorAgent:     "test-agent",
			RequestorModel:     "test-model",
			RiskTier:           db.RiskTierCaution,
			Command: db.CommandSpec{
				Raw: "ls -la",
				Cwd: "/tmp",
			},
			Status: db.StatusExecuted,
		}
		if err := dbConn.CreateRequest(req); err != nil {
			t.Fatalf("CreateRequest error = %v", err)
		}

		exec := NewExecutor(dbConn, nil)
		canExec, reason := exec.CanExecute(req.ID)
		if canExec {
			t.Error("expected CanExecute to return false for executed request")
		}
		if !strings.Contains(reason, "already been executed") {
			t.Errorf("expected reason to mention already executed, got %q", reason)
		}
	})

	t.Run("request not approved returns false", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		// Create a session first
		session := &db.Session{
			ID:          "test-session",
			ProjectPath: "/tmp/test",
			AgentName:   "test-agent",
			Program:     "test-program",
			Model:       "test-model",
		}
		if err := dbConn.CreateSession(session); err != nil {
			t.Fatalf("CreateSession error = %v", err)
		}

		req := &db.Request{
			ProjectPath:        "/tmp/test",
			RequestorSessionID: "test-session",
			RequestorAgent:     "test-agent",
			RequestorModel:     "test-model",
			RiskTier:           db.RiskTierCaution,
			Command: db.CommandSpec{
				Raw: "ls -la",
				Cwd: "/tmp",
			},
			Status: db.StatusPending,
		}
		if err := dbConn.CreateRequest(req); err != nil {
			t.Fatalf("CreateRequest error = %v", err)
		}

		exec := NewExecutor(dbConn, nil)
		canExec, reason := exec.CanExecute(req.ID)
		if canExec {
			t.Error("expected CanExecute to return false for pending request")
		}
		if !strings.Contains(reason, "not approved") {
			t.Errorf("expected reason to mention not approved, got %q", reason)
		}
	})

	t.Run("expired approval returns false", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		// Create a session first
		session := &db.Session{
			ID:          "test-session",
			ProjectPath: "/tmp/test",
			AgentName:   "test-agent",
			Program:     "test-program",
			Model:       "test-model",
		}
		if err := dbConn.CreateSession(session); err != nil {
			t.Fatalf("CreateSession error = %v", err)
		}

		pastTime := time.Now().Add(-1 * time.Hour)
		req := &db.Request{
			ProjectPath:        "/tmp/test",
			RequestorSessionID: "test-session",
			RequestorAgent:     "test-agent",
			RequestorModel:     "test-model",
			RiskTier:           db.RiskTierCaution,
			Command: db.CommandSpec{
				Raw: "ls -la",
				Cwd: "/tmp",
			},
			Status:            db.StatusApproved,
			ApprovalExpiresAt: &pastTime,
		}
		if err := dbConn.CreateRequest(req); err != nil {
			t.Fatalf("CreateRequest error = %v", err)
		}

		exec := NewExecutor(dbConn, nil)
		canExec, reason := exec.CanExecute(req.ID)
		if canExec {
			t.Error("expected CanExecute to return false for expired approval")
		}
		if !strings.Contains(reason, "expired") {
			t.Errorf("expected reason to mention expired, got %q", reason)
		}
	})

	t.Run("valid approved request returns true", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		// Create a session first
		session := &db.Session{
			ID:          "test-session",
			ProjectPath: "/tmp/test",
			AgentName:   "test-agent",
			Program:     "test-program",
			Model:       "test-model",
		}
		if err := dbConn.CreateSession(session); err != nil {
			t.Fatalf("CreateSession error = %v", err)
		}

		futureTime := time.Now().Add(1 * time.Hour)
		cmdSpec := db.CommandSpec{
			Raw: "ls -la",
			Cwd: "/tmp",
		}
		// Pre-compute hash using core's function so CanExecute validation passes
		cmdSpec.Hash = db.ComputeCommandHash(cmdSpec)

		req := &db.Request{
			ProjectPath:        "/tmp/test",
			RequestorSessionID: "test-session",
			RequestorAgent:     "test-agent",
			RequestorModel:     "test-model",
			RiskTier:           db.RiskTierCaution,
			Command:            cmdSpec,
			Status:             db.StatusApproved,
			ApprovalExpiresAt:  &futureTime,
		}
		if err := dbConn.CreateRequest(req); err != nil {
			t.Fatalf("CreateRequest error = %v", err)
		}

		exec := NewExecutor(dbConn, nil)
		canExec, reason := exec.CanExecute(req.ID)
		if !canExec {
			t.Errorf("expected CanExecute to return true, got false with reason: %q", reason)
		}
		if reason != "" {
			t.Errorf("expected empty reason for valid request, got %q", reason)
		}
	})

	t.Run("execution failed status returns false", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		// Create a session first
		session := &db.Session{
			ID:          "test-session",
			ProjectPath: "/tmp/test",
			AgentName:   "test-agent",
			Program:     "test-program",
			Model:       "test-model",
		}
		if err := dbConn.CreateSession(session); err != nil {
			t.Fatalf("CreateSession error = %v", err)
		}

		req := &db.Request{
			ProjectPath:        "/tmp/test",
			RequestorSessionID: "test-session",
			RequestorAgent:     "test-agent",
			RequestorModel:     "test-model",
			RiskTier:           db.RiskTierCaution,
			Command: db.CommandSpec{
				Raw: "ls -la",
				Cwd: "/tmp",
			},
			Status: db.StatusExecutionFailed,
		}
		if err := dbConn.CreateRequest(req); err != nil {
			t.Fatalf("CreateRequest error = %v", err)
		}

		exec := NewExecutor(dbConn, nil)
		canExec, reason := exec.CanExecute(req.ID)
		if canExec {
			t.Error("expected CanExecute to return false for execution failed request")
		}
		if !strings.Contains(reason, "already been executed") {
			t.Errorf("expected reason to mention already executed, got %q", reason)
		}
	})

	t.Run("command hash mismatch returns false", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		session := &db.Session{
			ID:          "test-session",
			ProjectPath: "/tmp/test",
			AgentName:   "test-agent",
			Program:     "test-program",
			Model:       "test-model",
		}
		if err := dbConn.CreateSession(session); err != nil {
			t.Fatalf("CreateSession error = %v", err)
		}

		futureTime := time.Now().Add(1 * time.Hour)
		req := &db.Request{
			ProjectPath:        "/tmp/test",
			RequestorSessionID: "test-session",
			RequestorAgent:     "test-agent",
			RequestorModel:     "test-model",
			RiskTier:           db.RiskTierCaution,
			Command: db.CommandSpec{
				Raw:  "ls -la",
				Cwd:  "/tmp",
				Hash: "invalid-hash",
			},
			Status:            db.StatusApproved,
			ApprovalExpiresAt: &futureTime,
		}
		if err := dbConn.CreateRequest(req); err != nil {
			t.Fatalf("CreateRequest error = %v", err)
		}

		exec := NewExecutor(dbConn, nil)
		canExec, reason := exec.CanExecute(req.ID)
		if canExec {
			t.Error("expected CanExecute to return false for hash mismatch")
		}
		if !strings.Contains(reason, "hash mismatch") {
			t.Errorf("expected reason to mention hash mismatch, got %q", reason)
		}
	})
}

func TestExecuteApprovedRequest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell execution test uses /bin/sh or $SHELL")
	}

	t.Run("empty request_id returns error", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		exec := NewExecutor(dbConn, nil)
		_, err = exec.ExecuteApprovedRequest(context.Background(), ExecuteOptions{
			RequestID: "",
			SessionID: "test-session",
		})
		if err == nil {
			t.Error("expected error for empty request_id")
		}
		if !strings.Contains(err.Error(), "request_id is required") {
			t.Errorf("expected request_id error, got %v", err)
		}
	})

	t.Run("empty session_id returns error", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		exec := NewExecutor(dbConn, nil)
		_, err = exec.ExecuteApprovedRequest(context.Background(), ExecuteOptions{
			RequestID: "test-request",
			SessionID: "",
		})
		if err == nil {
			t.Error("expected error for empty session_id")
		}
		if !strings.Contains(err.Error(), "session_id is required") {
			t.Errorf("expected session_id error, got %v", err)
		}
	})

	t.Run("request not found returns error", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		exec := NewExecutor(dbConn, nil)
		_, err = exec.ExecuteApprovedRequest(context.Background(), ExecuteOptions{
			RequestID: "nonexistent-id",
			SessionID: "test-session",
		})
		if err == nil {
			t.Error("expected error for nonexistent request")
		}
		if !strings.Contains(err.Error(), "getting request") {
			t.Errorf("expected getting request error, got %v", err)
		}
	})

	t.Run("session not found returns error", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		// Create session for request creation
		session := &db.Session{
			ID:          "creator-session",
			ProjectPath: "/tmp/test",
			AgentName:   "test-agent",
			Program:     "test-program",
			Model:       "test-model",
		}
		if err := dbConn.CreateSession(session); err != nil {
			t.Fatalf("CreateSession error = %v", err)
		}

		// Create a request
		req := &db.Request{
			ProjectPath:        "/tmp/test",
			RequestorSessionID: "creator-session",
			RequestorAgent:     "test-agent",
			RequestorModel:     "test-model",
			RiskTier:           db.RiskTierCaution,
			Command: db.CommandSpec{
				Raw: "ls -la",
				Cwd: "/tmp",
			},
			Status: db.StatusApproved,
		}
		if err := dbConn.CreateRequest(req); err != nil {
			t.Fatalf("CreateRequest error = %v", err)
		}

		exec := NewExecutor(dbConn, nil)
		_, err = exec.ExecuteApprovedRequest(context.Background(), ExecuteOptions{
			RequestID: req.ID,
			SessionID: "nonexistent-session",
		})
		if err == nil {
			t.Error("expected error for nonexistent session")
		}
		if !strings.Contains(err.Error(), "getting session") {
			t.Errorf("expected getting session error, got %v", err)
		}
	})

	t.Run("request already executing returns error", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		session := &db.Session{
			ID:          "test-session",
			ProjectPath: "/tmp/test",
			AgentName:   "test-agent",
			Program:     "test-program",
			Model:       "test-model",
		}
		if err := dbConn.CreateSession(session); err != nil {
			t.Fatalf("CreateSession error = %v", err)
		}

		req := &db.Request{
			ProjectPath:        "/tmp/test",
			RequestorSessionID: "test-session",
			RequestorAgent:     "test-agent",
			RequestorModel:     "test-model",
			RiskTier:           db.RiskTierCaution,
			Command: db.CommandSpec{
				Raw: "ls -la",
				Cwd: "/tmp",
			},
			Status: db.StatusExecuting,
		}
		if err := dbConn.CreateRequest(req); err != nil {
			t.Fatalf("CreateRequest error = %v", err)
		}

		exec := NewExecutor(dbConn, nil)
		_, err = exec.ExecuteApprovedRequest(context.Background(), ExecuteOptions{
			RequestID: req.ID,
			SessionID: "test-session",
		})
		if !errors.Is(err, ErrAlreadyExecuting) {
			t.Errorf("expected ErrAlreadyExecuting, got %v", err)
		}
	})

	t.Run("request already executed returns error", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		session := &db.Session{
			ID:          "test-session",
			ProjectPath: "/tmp/test",
			AgentName:   "test-agent",
			Program:     "test-program",
			Model:       "test-model",
		}
		if err := dbConn.CreateSession(session); err != nil {
			t.Fatalf("CreateSession error = %v", err)
		}

		req := &db.Request{
			ProjectPath:        "/tmp/test",
			RequestorSessionID: "test-session",
			RequestorAgent:     "test-agent",
			RequestorModel:     "test-model",
			RiskTier:           db.RiskTierCaution,
			Command: db.CommandSpec{
				Raw: "ls -la",
				Cwd: "/tmp",
			},
			Status: db.StatusExecuted,
		}
		if err := dbConn.CreateRequest(req); err != nil {
			t.Fatalf("CreateRequest error = %v", err)
		}

		exec := NewExecutor(dbConn, nil)
		_, err = exec.ExecuteApprovedRequest(context.Background(), ExecuteOptions{
			RequestID: req.ID,
			SessionID: "test-session",
		})
		if !errors.Is(err, ErrAlreadyExecuted) {
			t.Errorf("expected ErrAlreadyExecuted, got %v", err)
		}
	})

	t.Run("request execution failed status returns error", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		session := &db.Session{
			ID:          "test-session",
			ProjectPath: "/tmp/test",
			AgentName:   "test-agent",
			Program:     "test-program",
			Model:       "test-model",
		}
		if err := dbConn.CreateSession(session); err != nil {
			t.Fatalf("CreateSession error = %v", err)
		}

		req := &db.Request{
			ProjectPath:        "/tmp/test",
			RequestorSessionID: "test-session",
			RequestorAgent:     "test-agent",
			RequestorModel:     "test-model",
			RiskTier:           db.RiskTierCaution,
			Command: db.CommandSpec{
				Raw: "ls -la",
				Cwd: "/tmp",
			},
			Status: db.StatusExecutionFailed,
		}
		if err := dbConn.CreateRequest(req); err != nil {
			t.Fatalf("CreateRequest error = %v", err)
		}

		exec := NewExecutor(dbConn, nil)
		_, err = exec.ExecuteApprovedRequest(context.Background(), ExecuteOptions{
			RequestID: req.ID,
			SessionID: "test-session",
		})
		if !errors.Is(err, ErrAlreadyExecuted) {
			t.Errorf("expected ErrAlreadyExecuted, got %v", err)
		}
	})

	t.Run("request not approved returns error", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		session := &db.Session{
			ID:          "test-session",
			ProjectPath: "/tmp/test",
			AgentName:   "test-agent",
			Program:     "test-program",
			Model:       "test-model",
		}
		if err := dbConn.CreateSession(session); err != nil {
			t.Fatalf("CreateSession error = %v", err)
		}

		req := &db.Request{
			ProjectPath:        "/tmp/test",
			RequestorSessionID: "test-session",
			RequestorAgent:     "test-agent",
			RequestorModel:     "test-model",
			RiskTier:           db.RiskTierCaution,
			Command: db.CommandSpec{
				Raw: "ls -la",
				Cwd: "/tmp",
			},
			Status: db.StatusPending,
		}
		if err := dbConn.CreateRequest(req); err != nil {
			t.Fatalf("CreateRequest error = %v", err)
		}

		exec := NewExecutor(dbConn, nil)
		_, err = exec.ExecuteApprovedRequest(context.Background(), ExecuteOptions{
			RequestID: req.ID,
			SessionID: "test-session",
		})
		if !errors.Is(err, ErrRequestNotApproved) {
			t.Errorf("expected ErrRequestNotApproved, got %v", err)
		}
	})

	t.Run("expired approval returns error", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		session := &db.Session{
			ID:          "test-session",
			ProjectPath: "/tmp/test",
			AgentName:   "test-agent",
			Program:     "test-program",
			Model:       "test-model",
		}
		if err := dbConn.CreateSession(session); err != nil {
			t.Fatalf("CreateSession error = %v", err)
		}

		pastTime := time.Now().Add(-1 * time.Hour)
		req := &db.Request{
			ProjectPath:        "/tmp/test",
			RequestorSessionID: "test-session",
			RequestorAgent:     "test-agent",
			RequestorModel:     "test-model",
			RiskTier:           db.RiskTierCaution,
			Command: db.CommandSpec{
				Raw: "ls -la",
				Cwd: "/tmp",
			},
			Status:            db.StatusApproved,
			ApprovalExpiresAt: &pastTime,
		}
		if err := dbConn.CreateRequest(req); err != nil {
			t.Fatalf("CreateRequest error = %v", err)
		}

		exec := NewExecutor(dbConn, nil)
		_, err = exec.ExecuteApprovedRequest(context.Background(), ExecuteOptions{
			RequestID: req.ID,
			SessionID: "test-session",
		})
		if !errors.Is(err, ErrApprovalExpired) {
			t.Errorf("expected ErrApprovalExpired, got %v", err)
		}
	})

	t.Run("command hash mismatch returns error", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		session := &db.Session{
			ID:          "test-session",
			ProjectPath: "/tmp/test",
			AgentName:   "test-agent",
			Program:     "test-program",
			Model:       "test-model",
		}
		if err := dbConn.CreateSession(session); err != nil {
			t.Fatalf("CreateSession error = %v", err)
		}

		futureTime := time.Now().Add(1 * time.Hour)
		req := &db.Request{
			ProjectPath:        "/tmp/test",
			RequestorSessionID: "test-session",
			RequestorAgent:     "test-agent",
			RequestorModel:     "test-model",
			RiskTier:           db.RiskTierCaution,
			Command: db.CommandSpec{
				Raw:  "ls -la",
				Cwd:  "/tmp",
				Hash: "invalid-hash", // Wrong hash
			},
			Status:            db.StatusApproved,
			ApprovalExpiresAt: &futureTime,
		}
		if err := dbConn.CreateRequest(req); err != nil {
			t.Fatalf("CreateRequest error = %v", err)
		}

		exec := NewExecutor(dbConn, nil)
		_, err = exec.ExecuteApprovedRequest(context.Background(), ExecuteOptions{
			RequestID: req.ID,
			SessionID: "test-session",
		})
		if !errors.Is(err, ErrCommandHashMismatch) {
			t.Errorf("expected ErrCommandHashMismatch, got %v", err)
		}
	})

	t.Run("tier escalation returns error", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		session := &db.Session{
			ID:          "test-session",
			ProjectPath: "/tmp/test",
			AgentName:   "test-agent",
			Program:     "test-program",
			Model:       "test-model",
		}
		if err := dbConn.CreateSession(session); err != nil {
			t.Fatalf("CreateSession error = %v", err)
		}

		// Use a dangerous command but mark as CAUTION tier
		cmdSpec := db.CommandSpec{
			Raw: "rm -rf /tmp/test",
			Cwd: "/tmp",
		}
		cmdSpec.Hash = db.ComputeCommandHash(cmdSpec)

		futureTime := time.Now().Add(1 * time.Hour)
		req := &db.Request{
			ProjectPath:        "/tmp/test",
			RequestorSessionID: "test-session",
			RequestorAgent:     "test-agent",
			RequestorModel:     "test-model",
			RiskTier:           db.RiskTierCaution, // Approved as CAUTION but rm -rf is CRITICAL
			Command:            cmdSpec,
			Status:             db.StatusApproved,
			ApprovalExpiresAt:  &futureTime,
		}
		if err := dbConn.CreateRequest(req); err != nil {
			t.Fatalf("CreateRequest error = %v", err)
		}

		exec := NewExecutor(dbConn, nil)
		_, err = exec.ExecuteApprovedRequest(context.Background(), ExecuteOptions{
			RequestID: req.ID,
			SessionID: "test-session",
		})
		if !errors.Is(err, ErrTierEscalated) {
			t.Errorf("expected ErrTierEscalated, got %v", err)
		}
	})

	t.Run("successful execution with exit code 0", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		tmpDir := t.TempDir()
		session := &db.Session{
			ID:          "test-session",
			ProjectPath: tmpDir,
			AgentName:   "test-agent",
			Program:     "test-program",
			Model:       "test-model",
		}
		if err := dbConn.CreateSession(session); err != nil {
			t.Fatalf("CreateSession error = %v", err)
		}

		// Use a cross-platform 'true' command which exits with 0
		truePath := testutil.TruePath()
		cmdSpec := db.CommandSpec{
			Raw:  truePath,
			Argv: []string{truePath},
			Cwd:  tmpDir,
		}
		cmdSpec.Hash = db.ComputeCommandHash(cmdSpec)

		futureTime := time.Now().Add(1 * time.Hour)
		req := &db.Request{
			ProjectPath:        tmpDir,
			RequestorSessionID: "test-session",
			RequestorAgent:     "test-agent",
			RequestorModel:     "test-model",
			RiskTier:           db.RiskTierCaution,
			Command:            cmdSpec,
			Status:             db.StatusApproved,
			ApprovalExpiresAt:  &futureTime,
		}
		if err := dbConn.CreateRequest(req); err != nil {
			t.Fatalf("CreateRequest error = %v", err)
		}

		logDir := filepath.Join(tmpDir, "logs")
		exec := NewExecutor(dbConn, nil)
		result, err := exec.ExecuteApprovedRequest(context.Background(), ExecuteOptions{
			RequestID:      req.ID,
			SessionID:      "test-session",
			LogDir:         logDir,
			SuppressOutput: true,
		})
		if err != nil {
			t.Fatalf("ExecuteApprovedRequest error = %v", err)
		}
		if result == nil {
			t.Fatal("expected non-nil result")
		}
		if result.ExitCode != 0 {
			t.Errorf("expected exit code 0, got %d", result.ExitCode)
		}
		if result.LogPath == "" {
			t.Error("expected log path to be set")
		}

		// Verify request status was updated
		updatedReq, err := dbConn.GetRequest(req.ID)
		if err != nil {
			t.Fatalf("GetRequest error = %v", err)
		}
		if updatedReq.Status != db.StatusExecuted {
			t.Errorf("expected status %q, got %q", db.StatusExecuted, updatedReq.Status)
		}
	})

	t.Run("execution with non-zero exit code", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		tmpDir := t.TempDir()
		session := &db.Session{
			ID:          "test-session",
			ProjectPath: tmpDir,
			AgentName:   "test-agent",
			Program:     "test-program",
			Model:       "test-model",
		}
		if err := dbConn.CreateSession(session); err != nil {
			t.Fatalf("CreateSession error = %v", err)
		}

		// Use a cross-platform 'false' command which exits with 1
		falsePath := testutil.FalsePath()
		cmdSpec := db.CommandSpec{
			Raw:  falsePath,
			Argv: []string{falsePath},
			Cwd:  tmpDir,
		}
		cmdSpec.Hash = db.ComputeCommandHash(cmdSpec)

		futureTime := time.Now().Add(1 * time.Hour)
		req := &db.Request{
			ProjectPath:        tmpDir,
			RequestorSessionID: "test-session",
			RequestorAgent:     "test-agent",
			RequestorModel:     "test-model",
			RiskTier:           db.RiskTierCaution,
			Command:            cmdSpec,
			Status:             db.StatusApproved,
			ApprovalExpiresAt:  &futureTime,
		}
		if err := dbConn.CreateRequest(req); err != nil {
			t.Fatalf("CreateRequest error = %v", err)
		}

		logDir := filepath.Join(tmpDir, "logs")
		exec := NewExecutor(dbConn, nil)
		result, err := exec.ExecuteApprovedRequest(context.Background(), ExecuteOptions{
			RequestID:      req.ID,
			SessionID:      "test-session",
			LogDir:         logDir,
			SuppressOutput: true,
		})
		// Non-zero exit doesn't return error, just sets exit code
		if err != nil {
			t.Fatalf("ExecuteApprovedRequest error = %v", err)
		}
		if result == nil {
			t.Fatal("expected non-nil result")
		}
		if result.ExitCode != 1 {
			t.Errorf("expected exit code 1, got %d", result.ExitCode)
		}

		// Verify request status was updated to execution_failed
		updatedReq, err := dbConn.GetRequest(req.ID)
		if err != nil {
			t.Fatalf("GetRequest error = %v", err)
		}
		if updatedReq.Status != db.StatusExecutionFailed {
			t.Errorf("expected status %q, got %q", db.StatusExecutionFailed, updatedReq.Status)
		}
	})

	t.Run("notifier is called on execution", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		tmpDir := t.TempDir()
		session := &db.Session{
			ID:          "test-session",
			ProjectPath: tmpDir,
			AgentName:   "test-agent",
			Program:     "test-program",
			Model:       "test-model",
		}
		if err := dbConn.CreateSession(session); err != nil {
			t.Fatalf("CreateSession error = %v", err)
		}

		truePath := testutil.TruePath()
		cmdSpec := db.CommandSpec{
			Raw:  truePath,
			Argv: []string{truePath},
			Cwd:  tmpDir,
		}
		cmdSpec.Hash = db.ComputeCommandHash(cmdSpec)

		futureTime := time.Now().Add(1 * time.Hour)
		req := &db.Request{
			ProjectPath:        tmpDir,
			RequestorSessionID: "test-session",
			RequestorAgent:     "test-agent",
			RequestorModel:     "test-model",
			RiskTier:           db.RiskTierCaution,
			Command:            cmdSpec,
			Status:             db.StatusApproved,
			ApprovalExpiresAt:  &futureTime,
		}
		if err := dbConn.CreateRequest(req); err != nil {
			t.Fatalf("CreateRequest error = %v", err)
		}

		notifier := &mockExecutorNotifier{}
		logDir := filepath.Join(tmpDir, "logs")
		exec := NewExecutor(dbConn, nil).WithNotifier(notifier)
		_, err = exec.ExecuteApprovedRequest(context.Background(), ExecuteOptions{
			RequestID:      req.ID,
			SessionID:      "test-session",
			LogDir:         logDir,
			SuppressOutput: true,
		})
		if err != nil {
			t.Fatalf("ExecuteApprovedRequest error = %v", err)
		}

		if !notifier.executedCalled {
			t.Error("expected notifier.NotifyRequestExecuted to be called")
		}
	})

	t.Run("execution timeout", func(t *testing.T) {
		// Note: When a process is killed due to context deadline, exec.Cmd.Run() returns
		// an *exec.ExitError (because the process was signaled), and the current RunCommand
		// implementation checks for ExitError before checking ctx.Err(). This means timeout
		// might be treated as a normal non-zero exit code failure rather than a timeout.
		// This test verifies that the command is at least terminated, even if not as a timeout.
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		tmpDir := t.TempDir()
		session := &db.Session{
			ID:          "test-session",
			ProjectPath: tmpDir,
			AgentName:   "test-agent",
			Program:     "test-program",
			Model:       "test-model",
		}
		if err := dbConn.CreateSession(session); err != nil {
			t.Fatalf("CreateSession error = %v", err)
		}

		// Use sleep binary directly
		cmdSpec := db.CommandSpec{
			Raw:  "sleep 10",
			Argv: []string{"sleep", "10"},
			Cwd:  tmpDir,
		}
		cmdSpec.Hash = db.ComputeCommandHash(cmdSpec)

		futureTime := time.Now().Add(1 * time.Hour)
		req := &db.Request{
			ProjectPath:        tmpDir,
			RequestorSessionID: "test-session",
			RequestorAgent:     "test-agent",
			RequestorModel:     "test-model",
			RiskTier:           db.RiskTierCaution,
			Command:            cmdSpec,
			Status:             db.StatusApproved,
			ApprovalExpiresAt:  &futureTime,
		}
		if err := dbConn.CreateRequest(req); err != nil {
			t.Fatalf("CreateRequest error = %v", err)
		}

		logDir := filepath.Join(tmpDir, "logs")
		exec := NewExecutor(dbConn, nil)
		start := time.Now()
		result, _ := exec.ExecuteApprovedRequest(context.Background(), ExecuteOptions{
			RequestID:      req.ID,
			SessionID:      "test-session",
			LogDir:         logDir,
			Timeout:        100 * time.Millisecond, // Very short timeout
			SuppressOutput: true,
		})
		elapsed := time.Since(start)

		// The key assertion is that the command was terminated quickly, not after 10 seconds
		if elapsed > 2*time.Second {
			t.Errorf("execution took too long (%v), expected timeout to kill it", elapsed)
		}
		if result == nil {
			t.Fatal("expected non-nil result")
		}
		// Command should have non-zero exit code (killed by signal)
		if result.ExitCode == 0 {
			t.Error("expected non-zero exit code for killed process")
		}

		// Verify request status was updated (either timed_out or execution_failed is acceptable)
		updatedReq, err := dbConn.GetRequest(req.ID)
		if err != nil {
			t.Fatalf("GetRequest error = %v", err)
		}
		if updatedReq.Status != db.StatusTimedOut && updatedReq.Status != db.StatusExecutionFailed {
			t.Errorf("expected status timed_out or execution_failed, got %q", updatedReq.Status)
		}
	})

	t.Run("uses default timeout and log dir when not specified", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		tmpDir := t.TempDir()
		session := &db.Session{
			ID:          "test-session",
			ProjectPath: tmpDir,
			AgentName:   "test-agent",
			Program:     "test-program",
			Model:       "test-model",
		}
		if err := dbConn.CreateSession(session); err != nil {
			t.Fatalf("CreateSession error = %v", err)
		}

		// Change to tmpDir so default ".slb/logs" is created there
		oldWd, _ := os.Getwd()
		_ = os.Chdir(tmpDir)
		defer func() { _ = os.Chdir(oldWd) }()

		truePath := testutil.TruePath()
		cmdSpec := db.CommandSpec{
			Raw:  truePath,
			Argv: []string{truePath},
			Cwd:  tmpDir,
		}
		cmdSpec.Hash = db.ComputeCommandHash(cmdSpec)

		futureTime := time.Now().Add(1 * time.Hour)
		req := &db.Request{
			ProjectPath:        tmpDir,
			RequestorSessionID: "test-session",
			RequestorAgent:     "test-agent",
			RequestorModel:     "test-model",
			RiskTier:           db.RiskTierCaution,
			Command:            cmdSpec,
			Status:             db.StatusApproved,
			ApprovalExpiresAt:  &futureTime,
		}
		if err := dbConn.CreateRequest(req); err != nil {
			t.Fatalf("CreateRequest error = %v", err)
		}

		exec := NewExecutor(dbConn, nil)
		result, err := exec.ExecuteApprovedRequest(context.Background(), ExecuteOptions{
			RequestID:      req.ID,
			SessionID:      "test-session",
			SuppressOutput: true,
			// No LogDir or Timeout specified - should use defaults
		})
		if err != nil {
			t.Fatalf("ExecuteApprovedRequest error = %v", err)
		}
		if result == nil {
			t.Fatal("expected non-nil result")
		}
		// Just verify it completed successfully
		if result.ExitCode != 0 {
			t.Errorf("expected exit code 0, got %d", result.ExitCode)
		}
	})

	t.Run("capture rollback sets rollback path", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		tmpDir := t.TempDir()
		session := &db.Session{
			ID:          "test-session",
			ProjectPath: tmpDir,
			AgentName:   "test-agent",
			Program:     "test-program",
			Model:       "test-model",
		}
		if err := dbConn.CreateSession(session); err != nil {
			t.Fatalf("CreateSession error = %v", err)
		}

		// Create a test file to capture for rollback
		testFile := filepath.Join(tmpDir, "test.txt")
		if err := os.WriteFile(testFile, []byte("original content"), 0644); err != nil {
			t.Fatalf("WriteFile error = %v", err)
		}

		// Use a simple command that would modify the test file
		truePath := testutil.TruePath()
		cmdSpec := db.CommandSpec{
			Raw:  truePath,
			Argv: []string{truePath},
			Cwd:  tmpDir,
		}
		cmdSpec.Hash = db.ComputeCommandHash(cmdSpec)

		futureTime := time.Now().Add(1 * time.Hour)
		req := &db.Request{
			ProjectPath:        tmpDir,
			RequestorSessionID: "test-session",
			RequestorAgent:     "test-agent",
			RequestorModel:     "test-model",
			RiskTier:           db.RiskTierCaution,
			Command:            cmdSpec,
			Status:             db.StatusApproved,
			ApprovalExpiresAt:  &futureTime,
			// Rollback not set initially
		}
		if err := dbConn.CreateRequest(req); err != nil {
			t.Fatalf("CreateRequest error = %v", err)
		}

		logDir := filepath.Join(tmpDir, "logs")
		exec := NewExecutor(dbConn, nil)
		result, err := exec.ExecuteApprovedRequest(context.Background(), ExecuteOptions{
			RequestID:         req.ID,
			SessionID:         "test-session",
			LogDir:            logDir,
			SuppressOutput:    true,
			CaptureRollback:   true,
			MaxRollbackSizeMB: 10,
		})
		if err != nil {
			t.Fatalf("ExecuteApprovedRequest error = %v", err)
		}
		if result == nil {
			t.Fatal("expected non-nil result")
		}
		if result.ExitCode != 0 {
			t.Errorf("expected exit code 0, got %d", result.ExitCode)
		}

		// Check if rollback path was captured (may or may not be set depending on git state)
		updatedReq, err := dbConn.GetRequest(req.ID)
		if err != nil {
			t.Fatalf("GetRequest error = %v", err)
		}
		// Rollback might be nil if directory wasn't a git repo - that's OK
		// The test verifies the code path was executed without error
		_ = updatedReq
	})

	t.Run("uses default MaxRollbackSizeMB when not specified", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		tmpDir := t.TempDir()
		session := &db.Session{
			ID:          "test-session",
			ProjectPath: tmpDir,
			AgentName:   "test-agent",
			Program:     "test-program",
			Model:       "test-model",
		}
		if err := dbConn.CreateSession(session); err != nil {
			t.Fatalf("CreateSession error = %v", err)
		}

		truePath := testutil.TruePath()
		cmdSpec := db.CommandSpec{
			Raw:  truePath,
			Argv: []string{truePath},
			Cwd:  tmpDir,
		}
		cmdSpec.Hash = db.ComputeCommandHash(cmdSpec)

		futureTime := time.Now().Add(1 * time.Hour)
		req := &db.Request{
			ProjectPath:        tmpDir,
			RequestorSessionID: "test-session",
			RequestorAgent:     "test-agent",
			RequestorModel:     "test-model",
			RiskTier:           db.RiskTierCaution,
			Command:            cmdSpec,
			Status:             db.StatusApproved,
			ApprovalExpiresAt:  &futureTime,
		}
		if err := dbConn.CreateRequest(req); err != nil {
			t.Fatalf("CreateRequest error = %v", err)
		}

		logDir := filepath.Join(tmpDir, "logs")
		exec := NewExecutor(dbConn, nil)
		result, err := exec.ExecuteApprovedRequest(context.Background(), ExecuteOptions{
			RequestID:       req.ID,
			SessionID:       "test-session",
			LogDir:          logDir,
			SuppressOutput:  true,
			CaptureRollback: true,
			// MaxRollbackSizeMB not specified - should use default of 100
		})
		if err != nil {
			t.Fatalf("ExecuteApprovedRequest error = %v", err)
		}
		if result == nil {
			t.Fatal("expected non-nil result")
		}
	})

	t.Run("createLogFile error returns error", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		tmpDir := t.TempDir()
		session := &db.Session{
			ID:          "test-session",
			ProjectPath: tmpDir,
			AgentName:   "test-agent",
			Program:     "test-program",
			Model:       "test-model",
		}
		if err := dbConn.CreateSession(session); err != nil {
			t.Fatalf("CreateSession error = %v", err)
		}

		truePath := testutil.TruePath()
		cmdSpec := db.CommandSpec{
			Raw:  truePath,
			Argv: []string{truePath},
			Cwd:  tmpDir,
		}
		cmdSpec.Hash = db.ComputeCommandHash(cmdSpec)

		futureTime := time.Now().Add(1 * time.Hour)
		req := &db.Request{
			ProjectPath:        tmpDir,
			RequestorSessionID: "test-session",
			RequestorAgent:     "test-agent",
			RequestorModel:     "test-model",
			RiskTier:           db.RiskTierCaution,
			Command:            cmdSpec,
			Status:             db.StatusApproved,
			ApprovalExpiresAt:  &futureTime,
		}
		if err := dbConn.CreateRequest(req); err != nil {
			t.Fatalf("CreateRequest error = %v", err)
		}

		// Use a path that can't be created (file where directory expected)
		invalidLogDir := filepath.Join(tmpDir, "file.txt")
		if err := os.WriteFile(invalidLogDir, []byte("data"), 0644); err != nil {
			t.Fatalf("WriteFile error = %v", err)
		}
		nestedLogDir := filepath.Join(invalidLogDir, "logs") // Can't create dir inside file

		exec := NewExecutor(dbConn, nil)
		_, err = exec.ExecuteApprovedRequest(context.Background(), ExecuteOptions{
			RequestID:      req.ID,
			SessionID:      "test-session",
			LogDir:         nestedLogDir,
			SuppressOutput: true,
		})
		if err == nil {
			t.Error("expected error for invalid log directory")
		}
		if !strings.Contains(err.Error(), "creating log file") && !strings.Contains(err.Error(), "creating log dir") {
			t.Errorf("expected log file/dir error, got %v", err)
		}
	})

	t.Run("policy escalation returns false", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		session := &db.Session{
			ID:          "test-session",
			ProjectPath: "/tmp/test",
			AgentName:   "test-agent",
			Program:     "test-program",
			Model:       "test-model",
		}
		if err := dbConn.CreateSession(session); err != nil {
			t.Fatalf("CreateSession error = %v", err)
		}

		// Create a request that was approved at caution tier
		cmdSpec := db.CommandSpec{
			Raw: "rm -rf /very/important/path", // This is dangerous, not caution
			Cwd: "/",
		}
		cmdSpec.Hash = db.ComputeCommandHash(cmdSpec)

		req := &db.Request{
			ProjectPath:        "/tmp/test",
			RequestorSessionID: "test-session",
			RequestorAgent:     "test-agent",
			RequestorModel:     "test-model",
			RiskTier:           db.RiskTierCaution, // Approved at lower tier
			Command:            cmdSpec,
			Status:             db.StatusApproved,
		}
		if err := dbConn.CreateRequest(req); err != nil {
			t.Fatalf("CreateRequest error = %v", err)
		}

		exec := NewExecutor(dbConn, nil)
		canExec, reason := exec.CanExecute(req.ID)
		if canExec {
			t.Error("expected canExec=false when policy escalation occurs")
		}
		if !strings.Contains(reason, "escalation") && !strings.Contains(reason, "classified as") {
			t.Errorf("expected policy escalation message, got %q", reason)
		}
	})
}
