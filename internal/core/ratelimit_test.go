package core

import (
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestRateLimitErrorError(t *testing.T) {
	tests := []struct {
		name     string
		err      RateLimitError
		contains []string
	}{
		{
			name: "pending limit exceeded",
			err: RateLimitError{
				Pending:    10,
				MaxPending: 5,
			},
			contains: []string{"pending limit exceeded", "10/5"},
		},
		{
			name: "per-minute limit exceeded",
			err: RateLimitError{
				Recent:       20,
				MaxPerMinute: 10,
			},
			contains: []string{"per-minute limit exceeded", "20/10"},
		},
		{
			name: "both limits exceeded",
			err: RateLimitError{
				Pending:      10,
				MaxPending:   5,
				Recent:       20,
				MaxPerMinute: 10,
			},
			contains: []string{"pending limit exceeded", "per-minute limit exceeded", ";"},
		},
		{
			name:     "generic rate limit exceeded (no specific reason)",
			err:      RateLimitError{},
			contains: []string{"rate limit exceeded"},
		},
		{
			name: "with reset time",
			err: RateLimitError{
				Pending:    10,
				MaxPending: 5,
				ResetAt:    time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
			},
			contains: []string{"reset_at=", "2024-01-15T10:30:00Z"},
		},
		{
			name: "per-minute limit with reset time",
			err: RateLimitError{
				Recent:       20,
				MaxPerMinute: 10,
				ResetAt:      time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC),
			},
			contains: []string{"per-minute limit exceeded", "reset_at="},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := tc.err.Error()
			for _, s := range tc.contains {
				if !strings.Contains(msg, s) {
					t.Errorf("Error() = %q, want to contain %q", msg, s)
				}
			}
		})
	}
}

func TestRateLimitErrorImplementsError(t *testing.T) {
	var err error = &RateLimitError{Pending: 5, MaxPending: 3}
	if err.Error() == "" {
		t.Error("RateLimitError.Error() should not return empty string")
	}
}

func TestRateLimitConfigNormalized(t *testing.T) {
	tests := []struct {
		name       string
		input      RateLimitConfig
		wantPending int
		wantMinute  int
		wantAction  RateLimitAction
	}{
		{
			name:        "zero values get defaults",
			input:       RateLimitConfig{},
			wantPending: 5,  // default
			wantMinute:  10, // default
			wantAction:  RateLimitActionReject,
		},
		{
			name: "negative pending gets default",
			input: RateLimitConfig{
				MaxPendingPerSession: -1,
				MaxRequestsPerMinute: 20,
				Action:               RateLimitActionQueue,
			},
			wantPending: 5,
			wantMinute:  20,
			wantAction:  RateLimitActionQueue,
		},
		{
			name: "negative per-minute gets default",
			input: RateLimitConfig{
				MaxPendingPerSession: 10,
				MaxRequestsPerMinute: -5,
				Action:               RateLimitActionWarn,
			},
			wantPending: 10,
			wantMinute:  10,
			wantAction:  RateLimitActionWarn,
		},
		{
			name: "unknown action gets default",
			input: RateLimitConfig{
				MaxPendingPerSession: 3,
				MaxRequestsPerMinute: 5,
				Action:               RateLimitAction("unknown"),
			},
			wantPending: 3,
			wantMinute:  5,
			wantAction:  RateLimitActionReject,
		},
		{
			name: "valid config passes through",
			input: RateLimitConfig{
				MaxPendingPerSession: 15,
				MaxRequestsPerMinute: 30,
				Action:               RateLimitActionQueue,
			},
			wantPending: 15,
			wantMinute:  30,
			wantAction:  RateLimitActionQueue,
		},
		{
			name: "action reject is valid",
			input: RateLimitConfig{
				MaxPendingPerSession: 5,
				MaxRequestsPerMinute: 10,
				Action:               RateLimitActionReject,
			},
			wantPending: 5,
			wantMinute:  10,
			wantAction:  RateLimitActionReject,
		},
		{
			name: "action warn is valid",
			input: RateLimitConfig{
				MaxPendingPerSession: 5,
				MaxRequestsPerMinute: 10,
				Action:               RateLimitActionWarn,
			},
			wantPending: 5,
			wantMinute:  10,
			wantAction:  RateLimitActionWarn,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.input.normalized()
			if got.MaxPendingPerSession != tc.wantPending {
				t.Errorf("MaxPendingPerSession = %d, want %d", got.MaxPendingPerSession, tc.wantPending)
			}
			if got.MaxRequestsPerMinute != tc.wantMinute {
				t.Errorf("MaxRequestsPerMinute = %d, want %d", got.MaxRequestsPerMinute, tc.wantMinute)
			}
			if got.Action != tc.wantAction {
				t.Errorf("Action = %q, want %q", got.Action, tc.wantAction)
			}
		})
	}
}

func TestNewRateLimiter(t *testing.T) {
	dbConn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open(:memory:) error = %v", err)
	}
	defer dbConn.Close()

	cfg := RateLimitConfig{
		MaxPendingPerSession: 10,
		MaxRequestsPerMinute: 20,
		Action:               RateLimitActionWarn,
	}

	rl := NewRateLimiter(dbConn, cfg)
	if rl == nil {
		t.Fatal("NewRateLimiter returned nil")
	}
	if rl.db != dbConn {
		t.Error("RateLimiter.db not set correctly")
	}
	if rl.cfg.MaxPendingPerSession != 10 {
		t.Errorf("cfg.MaxPendingPerSession = %d, want 10", rl.cfg.MaxPendingPerSession)
	}
	if rl.now == nil {
		t.Error("RateLimiter.now function not set")
	}
}

func TestResetRateLimits(t *testing.T) {
	dbConn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open(:memory:) error = %v", err)
	}
	defer dbConn.Close()

	// Create a session
	sess := &db.Session{
		AgentName:   "TestAgent",
		Program:     "test-cli",
		Model:       "test-model",
		ProjectPath: "/test/project",
	}
	if err := dbConn.CreateSession(sess); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	rl := NewRateLimiter(dbConn, DefaultRateLimitConfig())

	t.Run("empty session ID returns error", func(t *testing.T) {
		_, err := rl.ResetRateLimits("")
		if err == nil {
			t.Error("Expected error for empty session ID")
		}
	})

	t.Run("valid session ID succeeds", func(t *testing.T) {
		resetAt, err := rl.ResetRateLimits(sess.ID)
		if err != nil {
			t.Fatalf("ResetRateLimits() error = %v", err)
		}
		if resetAt.IsZero() {
			t.Error("Expected non-zero reset timestamp")
		}
	})
}

func TestCheckRateLimit(t *testing.T) {
	t.Run("empty session ID returns error", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		rl := NewRateLimiter(dbConn, DefaultRateLimitConfig())
		_, err = rl.CheckRateLimit("")
		if err == nil {
			t.Error("Expected error for empty session ID")
		}
	})

	t.Run("allowed when under limits", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		// Create a session
		sess := &db.Session{
			AgentName:   "TestAgent",
			Program:     "test-cli",
			Model:       "test-model",
			ProjectPath: "/test/project",
		}
		if err := dbConn.CreateSession(sess); err != nil {
			t.Fatalf("CreateSession() error = %v", err)
		}

		rl := NewRateLimiter(dbConn, DefaultRateLimitConfig())
		result, err := rl.CheckRateLimit(sess.ID)
		if err != nil {
			t.Fatalf("CheckRateLimit() error = %v", err)
		}
		if !result.Allowed {
			t.Error("Expected Allowed=true when no requests exist")
		}
		if result.Message != "ok" {
			t.Errorf("Message = %q, want 'ok'", result.Message)
		}
		if result.RemainingPending != 5 {
			t.Errorf("RemainingPending = %d, want 5", result.RemainingPending)
		}
		if result.RemainingPerMinute != 10 {
			t.Errorf("RemainingPerMinute = %d, want 10", result.RemainingPerMinute)
		}
	})

	t.Run("blocked when pending limit exceeded with reject action", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		// Create a session
		sess := &db.Session{
			AgentName:   "TestAgent",
			Program:     "test-cli",
			Model:       "test-model",
			ProjectPath: "/test/project",
		}
		if err := dbConn.CreateSession(sess); err != nil {
			t.Fatalf("CreateSession() error = %v", err)
		}

		// Create pending requests to exceed limit
		for i := 0; i < 6; i++ {
			req := &db.Request{
				ProjectPath:        "/test/project",
				RequestorSessionID: sess.ID,
				RequestorAgent:     sess.AgentName,
				RequestorModel:     sess.Model,
				RiskTier:           db.RiskTierCaution,
				MinApprovals:       1,
				Status:             db.StatusPending,
				Command: db.CommandSpec{
					Raw: "test command",
					Cwd: "/test/project",
				},
				Justification: db.Justification{
					Reason: "Testing rate limits",
				},
			}
			if err := dbConn.CreateRequest(req); err != nil {
				t.Fatalf("CreateRequest() error = %v", err)
			}
		}

		cfg := RateLimitConfig{
			MaxPendingPerSession: 5,
			MaxRequestsPerMinute: 100, // High to not trigger
			Action:               RateLimitActionReject,
		}
		rl := NewRateLimiter(dbConn, cfg)
		result, err := rl.CheckRateLimit(sess.ID)

		// With reject action, should return error
		if err == nil {
			t.Fatal("Expected error when pending limit exceeded with reject action")
		}
		if _, ok := err.(*RateLimitError); !ok {
			t.Errorf("Expected RateLimitError, got %T", err)
		}
		if result == nil {
			t.Fatal("Expected result even with error")
		}
		if result.Allowed {
			t.Error("Expected Allowed=false when limit exceeded")
		}
	})

	t.Run("warn action allows but sets message", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		// Create a session
		sess := &db.Session{
			AgentName:   "TestAgent",
			Program:     "test-cli",
			Model:       "test-model",
			ProjectPath: "/test/project",
		}
		if err := dbConn.CreateSession(sess); err != nil {
			t.Fatalf("CreateSession() error = %v", err)
		}

		// Create pending requests to exceed limit
		for i := 0; i < 6; i++ {
			req := &db.Request{
				ProjectPath:        "/test/project",
				RequestorSessionID: sess.ID,
				RequestorAgent:     sess.AgentName,
				RequestorModel:     sess.Model,
				RiskTier:           db.RiskTierCaution,
				MinApprovals:       1,
				Status:             db.StatusPending,
				Command: db.CommandSpec{
					Raw: "test command",
					Cwd: "/test/project",
				},
				Justification: db.Justification{
					Reason: "Testing rate limits",
				},
			}
			if err := dbConn.CreateRequest(req); err != nil {
				t.Fatalf("CreateRequest() error = %v", err)
			}
		}

		cfg := RateLimitConfig{
			MaxPendingPerSession: 5,
			MaxRequestsPerMinute: 100,
			Action:               RateLimitActionWarn,
		}
		rl := NewRateLimiter(dbConn, cfg)
		result, err := rl.CheckRateLimit(sess.ID)

		// With warn action, should NOT return error
		if err != nil {
			t.Fatalf("CheckRateLimit() with warn action should not error = %v", err)
		}
		if !result.Allowed {
			t.Error("Expected Allowed=true with warn action")
		}
		if !strings.Contains(result.Message, "pending limit exceeded") {
			t.Errorf("Message should contain warning, got %q", result.Message)
		}
	})

	t.Run("queue action blocks but no error", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		// Create a session
		sess := &db.Session{
			AgentName:   "TestAgent",
			Program:     "test-cli",
			Model:       "test-model",
			ProjectPath: "/test/project",
		}
		if err := dbConn.CreateSession(sess); err != nil {
			t.Fatalf("CreateSession() error = %v", err)
		}

		// Create pending requests to exceed limit
		for i := 0; i < 6; i++ {
			req := &db.Request{
				ProjectPath:        "/test/project",
				RequestorSessionID: sess.ID,
				RequestorAgent:     sess.AgentName,
				RequestorModel:     sess.Model,
				RiskTier:           db.RiskTierCaution,
				MinApprovals:       1,
				Status:             db.StatusPending,
				Command: db.CommandSpec{
					Raw: "test command",
					Cwd: "/test/project",
				},
				Justification: db.Justification{
					Reason: "Testing rate limits",
				},
			}
			if err := dbConn.CreateRequest(req); err != nil {
				t.Fatalf("CreateRequest() error = %v", err)
			}
		}

		cfg := RateLimitConfig{
			MaxPendingPerSession: 5,
			MaxRequestsPerMinute: 100,
			Action:               RateLimitActionQueue,
		}
		rl := NewRateLimiter(dbConn, cfg)
		result, err := rl.CheckRateLimit(sess.ID)

		// With queue action, should NOT return error but blocked
		if err != nil {
			t.Fatalf("CheckRateLimit() with queue action should not error = %v", err)
		}
		if result.Allowed {
			t.Error("Expected Allowed=false with queue action when blocked")
		}
	})

	t.Run("remaining values capped at zero", func(t *testing.T) {
		dbConn, err := db.Open(":memory:")
		if err != nil {
			t.Fatalf("db.Open(:memory:) error = %v", err)
		}
		defer dbConn.Close()

		// Create a session
		sess := &db.Session{
			AgentName:   "TestAgent",
			Program:     "test-cli",
			Model:       "test-model",
			ProjectPath: "/test/project",
		}
		if err := dbConn.CreateSession(sess); err != nil {
			t.Fatalf("CreateSession() error = %v", err)
		}

		// Create many pending requests
		for i := 0; i < 10; i++ {
			req := &db.Request{
				ProjectPath:        "/test/project",
				RequestorSessionID: sess.ID,
				RequestorAgent:     sess.AgentName,
				RequestorModel:     sess.Model,
				RiskTier:           db.RiskTierCaution,
				MinApprovals:       1,
				Status:             db.StatusPending,
				Command: db.CommandSpec{
					Raw: "test command",
					Cwd: "/test/project",
				},
				Justification: db.Justification{
					Reason: "Testing rate limits",
				},
			}
			if err := dbConn.CreateRequest(req); err != nil {
				t.Fatalf("CreateRequest() error = %v", err)
			}
		}

		cfg := RateLimitConfig{
			MaxPendingPerSession: 3,
			MaxRequestsPerMinute: 100,
			Action:               RateLimitActionWarn,
		}
		rl := NewRateLimiter(dbConn, cfg)
		result, err := rl.CheckRateLimit(sess.ID)
		if err != nil {
			t.Fatalf("CheckRateLimit() error = %v", err)
		}

		// Remaining should be 0, not negative
		if result.RemainingPending != 0 {
			t.Errorf("RemainingPending = %d, want 0 (not negative)", result.RemainingPending)
		}
	})
}
