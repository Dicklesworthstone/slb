package core

import (
	"strings"
	"testing"
	"time"
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
