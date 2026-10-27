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
		// Cancellation can arrive inside a retry's DB/preflight work, not
		// only while waiting on the timer. Retain the observed quota and the
		// original operation error, without replacing an unrelated storage or
		// identity failure that happens concurrently with cancellation.
		if ctxErr := ctx.Err(); queue && lastLimit != nil && ctxErr != nil && errors.Is(err, ctxErr) {
			return nil, requestAdmissionWaitError(err, lastLimit)
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
	if rc.config != nil && rc.config.RequestTimeout > 0 {
		return rc.config.RequestTimeout
	}
	return db.DefaultRequestTimeout
}

// advanceExpiredCapacity never treats an expired row as permission to skip a
// quota. It uses the same transactional timeout resolver as waiters and the
// daemon, so current timeout policy, peer decisions and audit events survive.
// Close the listing before resolving: SQLite may have only one connection.
func (rc *RequestCreator) advanceExpiredCapacity(ctx context.Context, sessionID, project string) error {
	ids, err := rc.db.ListExpiredPendingRequestIDs(ctx, sessionID, project, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("finding expired request capacity: %w", err)
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := AdvancePendingRequest(ctx, rc.db, id, PendingOptions{
			ConfigPath: rc.patternEngine.PolicyConfigPath(), ExpiredOnly: true,
		}); err != nil {
			return fmt.Errorf("resolving expired request %s before admission: %w", id, err)
		}
	}
	return nil
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
