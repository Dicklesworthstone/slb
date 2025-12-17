package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/integrations"
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
		cmdSpec.Hash = ComputeCommandHash(cmdSpec)

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
}
