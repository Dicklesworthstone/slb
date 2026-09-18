package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

// WaitOptions controls a client's wait, not the lifetime of the shared request.
// Timeout zero waits until a decision or context cancellation. Expiry and
// automatic approval always use the request's current project policy.
type WaitOptions struct {
	ConfigPath   string
	Timeout      time.Duration
	PollInterval time.Duration
}

// WaitForDecision advances due pending policy even when no daemon is running.
// It returns as soon as the request leaves PENDING, including APPROVED,
// EXECUTING, TIMEOUT and ESCALATED (none need be a lifecycle terminal state).
// A client deadline/cancellation never cancels or times out the shared request.
// Execution remains a separate, single-use claim with fresh authorization.
func WaitForDecision(ctx context.Context, database *db.DB, id string, opts WaitOptions) (*db.Request, error) {
	if database == nil || id == "" {
		return nil, errors.New("database and request ID are required")
	}
	return waitForDecision(ctx, opts, func(ctx context.Context) (*db.Request, error) {
		result, err := AdvancePendingRequest(ctx, database, id, PendingOptions{ConfigPath: opts.ConfigPath})
		if err != nil {
			return nil, fmt.Errorf("advancing request %s: %w", id, err)
		}
		if result == nil {
			return nil, errors.New("pending resolver returned no result")
		}
		return result.Request, nil
	})
}

// waitForDecision owns cancellation and polling; refresh owns the atomic policy
// decision. No stale snapshot is ever written back by the waiter.
func waitForDecision(ctx context.Context, opts WaitOptions, refresh func(context.Context) (*db.Request, error)) (*db.Request, error) {
	if opts.Timeout < 0 || opts.PollInterval < 0 {
		return nil, errors.New("wait timeout and poll interval must not be negative")
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = 500 * time.Millisecond
	}
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	var last *db.Request
	for {
		if err := ctx.Err(); err != nil {
			return last, err
		}
		request, err := refresh(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return last, ctx.Err()
			}
			return last, err
		}
		if request == nil {
			return last, errors.New("pending resolver returned no request")
		}
		last = request
		// Cancellation wins before a caller may act on the returned approval.
		if err := ctx.Err(); err != nil {
			return last, err
		}
		if request.Status != db.StatusPending {
			return request, nil
		}

		timer := time.NewTimer(opts.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return last, ctx.Err()
		case <-timer.C:
		}
	}
}
