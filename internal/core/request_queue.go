package core

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

// CreateRequest submits using the configured admission policy. Queue mode is
// bounded by the request timeout; cancellable callers should use the context API.
func (rc *RequestCreator) CreateRequest(opts CreateRequestOptions) (*CreateRequestResult, error) {
	return rc.CreateRequestContext(context.Background(), opts)
}

// CreateRequestContext waits for capacity only when rate_limit_action="queue".
// Waiting is client-side backpressure, NOT a persisted request or permission to
// execute: no ID, reviewer notification, or pending row exists until admission.
//
// Each retry reloads the session and re-evaluates the supplied policy engine and
// quorum. Request expiry starts at the successful attempt, not at queue entry.
// Storage/identity errors are never retried as if they were quota exhaustion.
func (rc *RequestCreator) CreateRequestContext(ctx context.Context, opts CreateRequestOptions) (*CreateRequestResult, error) {
	queue := rc.rateLimiter.cfg.normalized().Action == RateLimitActionQueue
	if _, hasDeadline := ctx.Deadline(); queue && !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, rc.requestLifetime())
		defer cancel()
	}
	started := time.Now()
	var lastLimit *RateLimitError
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, requestAdmissionWaitError(err, lastLimit)
		}
		result, err := rc.createRequestOnce(ctx, opts)
		if err == nil {
			// A successful commit wins over concurrent cancellation: returning
			// a cancellation here would hide a real, durable request from callers.
			if lastLimit != nil {
				result.Queued = true
				result.QueueWait = time.Since(started)
			}
			return result, nil
		}
		var limit *RateLimitError
		if !queue || !errors.As(err, &limit) {
			return nil, err
		}
		lastLimit = limit
		delay := requestAdmissionRetryDelay(limit, attempt)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, requestAdmissionWaitError(ctx.Err(), lastLimit)
		case <-timer.C:
		}
	}
}

func requestAdmissionWaitError(cause error, limit *RateLimitError) error {
	// A typed nil *RateLimitError is not a nil error interface. Do not put it
	// into errors.Join: formatting a pre-cancelled call would otherwise panic.
	if limit != nil {
		cause = errors.Join(cause, limit)
	}
	return fmt.Errorf("waiting for request capacity: %w", cause)
}

func (rc *RequestCreator) requestLifetime() time.Duration {
	if rc.config != nil && rc.config.RequestTimeoutMinutes > 0 {
		minutes := int64(rc.config.RequestTimeoutMinutes)
		if minutes <= int64((1<<63-1)/time.Minute) {
			return time.Duration(minutes) * time.Minute
		}
	}
	return db.DefaultRequestTimeout
}

// Bounded exponential backoff with jitter avoids a synchronized writer stampede.
// Poll at least once per second so pending decisions, resets and ended sessions
// are noticed even when the rolling-window reset would be much farther away.
func requestAdmissionRetryDelay(limit *RateLimitError, attempt int) time.Duration {
	attempt = max(0, min(attempt, 2))
	base := (200 * time.Millisecond) << attempt
	delay := base + time.Duration(rand.Int64N(int64(base/4)+1))
	if limit != nil && limit.Pending < limit.MaxPending && !limit.ResetAt.IsZero() {
		untilReset := time.Until(limit.ResetAt) + 10*time.Millisecond
		if untilReset > 0 && untilReset < delay {
			delay = untilReset
		}
	}
	return max(25*time.Millisecond, delay)
}
