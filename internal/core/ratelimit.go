// Package core implements per-session rate limiting to prevent request floods.
package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

// RateLimitAction determines what to do when limits are exceeded.
type RateLimitAction string

const (
	RateLimitActionReject RateLimitAction = "reject"
	RateLimitActionQueue  RateLimitAction = "queue"
	RateLimitActionWarn   RateLimitAction = "warn"
)

// RateLimitConfig configures the per-session rate limiter.
type RateLimitConfig struct {
	MaxPendingPerSession int
	MaxRequestsPerMinute int
	Action               RateLimitAction
}

// DefaultRateLimitConfig returns default limits from the plan.
func DefaultRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{
		MaxPendingPerSession: 5,
		MaxRequestsPerMinute: 10,
		Action:               RateLimitActionReject,
	}
}

func (c RateLimitConfig) normalized() RateLimitConfig {
	out := c
	def := DefaultRateLimitConfig()

	if out.MaxPendingPerSession <= 0 {
		out.MaxPendingPerSession = def.MaxPendingPerSession
	}
	if out.MaxRequestsPerMinute <= 0 {
		out.MaxRequestsPerMinute = def.MaxRequestsPerMinute
	}
	switch out.Action {
	case RateLimitActionReject, RateLimitActionQueue, RateLimitActionWarn:
		// ok
	default:
		out.Action = def.Action
	}

	return out
}

// RateLimitResult describes whether a session can submit a new request right now.
type RateLimitResult struct {
	Allowed            bool            `json:"allowed"`
	Action             RateLimitAction `json:"action"`
	RemainingPending   int             `json:"remaining_pending"`
	RemainingPerMinute int             `json:"remaining_per_minute"`
	ResetAt            time.Time       `json:"reset_at"`
	Message            string          `json:"message,omitempty"`
}

// RateLimitError is returned when a strict admission exceeds its limits.
type RateLimitError struct {
	SessionID    string
	Pending      int
	MaxPending   int
	Recent       int
	MaxPerMinute int
	ResetAt      time.Time
}

func (e *RateLimitError) Error() string {
	parts := ""
	if e.MaxPending > 0 && e.Pending >= e.MaxPending {
		parts += fmt.Sprintf("pending limit exceeded (%d/%d)", e.Pending, e.MaxPending)
	}
	if e.MaxPerMinute > 0 && e.Recent >= e.MaxPerMinute {
		if parts != "" {
			parts += "; "
		}
		parts += fmt.Sprintf("per-minute limit exceeded (%d/%d)", e.Recent, e.MaxPerMinute)
	}
	if parts == "" {
		parts = "rate limit exceeded"
	}
	if !e.ResetAt.IsZero() {
		parts += fmt.Sprintf(" (reset_at=%s)", e.ResetAt.UTC().Format(time.RFC3339))
	}
	return parts
}

// RateLimiter enforces per-session request rate limits.
type RateLimiter struct {
	db  *db.DB
	cfg RateLimitConfig

	now func() time.Time
}

// NewRateLimiter constructs a rate limiter.
func NewRateLimiter(database *db.DB, cfg RateLimitConfig) *RateLimiter {
	return &RateLimiter{
		db:  database,
		cfg: cfg.normalized(),
		now: time.Now,
	}
}

// AdmitRequest atomically consumes quota AND persists the request. A successful
// CheckRateLimit is only advisory and must never authorize a separate insert.
// Both queue and reject return a typed RateLimitError when no slot is available;
// callers may retry queue admissions, but must not retry other storage errors.
func (rl *RateLimiter) AdmitRequest(ctx context.Context, request *db.Request) (*RateLimitResult, error) {
	cfg := rl.cfg.normalized()
	stats, err := rl.db.AdmitRequest(ctx, request, db.RequestAdmissionLimits{
		MaxPending: cfg.MaxPendingPerSession, MaxPerMinute: cfg.MaxRequestsPerMinute,
		WarnOnly: cfg.Action == RateLimitActionWarn,
	})
	var denied *db.RequestAdmissionLimitError
	if err != nil && !errors.As(err, &denied) {
		return nil, err
	}
	result := &RateLimitResult{
		Allowed: err == nil, Action: cfg.Action,
		RemainingPending:   max(0, cfg.MaxPendingPerSession-stats.Pending),
		RemainingPerMinute: max(0, cfg.MaxRequestsPerMinute-stats.Recent),
		ResetAt:            stats.ResetAt, Message: "ok",
	}
	limitErr := &RateLimitError{
		SessionID: request.RequestorSessionID,
		Pending:   stats.Pending, MaxPending: cfg.MaxPendingPerSession,
		Recent: stats.Recent, MaxPerMinute: cfg.MaxRequestsPerMinute, ResetAt: stats.ResetAt,
	}
	if stats.Exceeded {
		result.Message = limitErr.Error()
	}
	if err != nil {
		return result, limitErr
	}
	// Remaining capacity describes the state AFTER this successful admission.
	result.RemainingPending = max(0, result.RemainingPending-1)
	result.RemainingPerMinute = max(0, result.RemainingPerMinute-1)
	return result, nil
}

// ResetRateLimits resets the per-minute counter for a session by recording a reset timestamp.
// Callers can expose this via a human-only CLI command (e.g. `slb session reset-limits`).
func (rl *RateLimiter) ResetRateLimits(sessionID string) (time.Time, error) {
	if sessionID == "" {
		return time.Time{}, fmt.Errorf("session_id is required")
	}
	return rl.db.ResetSessionRateLimits(sessionID, rl.now().UTC())
}

// CheckRateLimit returns an advisory snapshot. Use AdmitRequest to submit: the
// counters can change immediately after this method returns.
func (rl *RateLimiter) CheckRateLimit(sessionID string) (*RateLimitResult, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	cfg := rl.cfg.normalized()

	now := rl.now().UTC()
	windowStart := now.Add(-time.Minute)

	if resetAt, err := rl.db.GetSessionRateLimitResetAt(sessionID); err != nil {
		return nil, err
	} else if resetAt != nil && resetAt.After(windowStart) {
		windowStart = resetAt.UTC()
	}

	pending, err := rl.db.CountPendingBySession(sessionID)
	if err != nil {
		return nil, err
	}
	recent, err := rl.db.CountRequestsSince(sessionID, windowStart)
	if err != nil {
		return nil, err
	}

	remainingPending := cfg.MaxPendingPerSession - pending
	if remainingPending < 0 {
		remainingPending = 0
	}
	remainingPerMinute := cfg.MaxRequestsPerMinute - recent
	if remainingPerMinute < 0 {
		remainingPerMinute = 0
	}

	resetAt := time.Time{}
	if recent > 0 {
		oldest, err := rl.db.OldestRequestCreatedAtSince(sessionID, windowStart)
		if err != nil {
			return nil, err
		}
		if oldest != nil {
			resetAt = oldest.UTC().Add(time.Minute)
		}
	}

	result := &RateLimitResult{
		Allowed:            true,
		Action:             cfg.Action,
		RemainingPending:   remainingPending,
		RemainingPerMinute: remainingPerMinute,
		ResetAt:            resetAt,
		Message:            "ok",
	}

	blockedPending := pending >= cfg.MaxPendingPerSession
	blockedPerMinute := recent >= cfg.MaxRequestsPerMinute
	if !blockedPending && !blockedPerMinute {
		return result, nil
	}

	// Preserve the advisory API's blocked-capacity contract. The atomic
	// admission result separately reports each committed counter.
	result.RemainingPending = 0
	result.RemainingPerMinute = 0
	result.Message = (&RateLimitError{
		SessionID:    sessionID,
		Pending:      pending,
		MaxPending:   cfg.MaxPendingPerSession,
		Recent:       recent,
		MaxPerMinute: cfg.MaxRequestsPerMinute,
		ResetAt:      resetAt,
	}).Error()

	switch cfg.Action {
	case RateLimitActionWarn:
		result.Allowed = true
		return result, nil
	case RateLimitActionQueue:
		result.Allowed = false
		return result, nil
	default:
		result.Allowed = false
		return result, &RateLimitError{
			SessionID:    sessionID,
			Pending:      pending,
			MaxPending:   cfg.MaxPendingPerSession,
			Recent:       recent,
			MaxPerMinute: cfg.MaxRequestsPerMinute,
			ResetAt:      resetAt,
		}
	}
}
