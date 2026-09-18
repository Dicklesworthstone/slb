package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

type queuedRequestResult struct {
	result *CreateRequestResult
	err    error
}

func awaitQueuedAttempt(t *testing.T, creator *RequestCreator, opts CreateRequestOptions, ctx context.Context) <-chan queuedRequestResult {
	t.Helper()
	done := make(chan queuedRequestResult, 1)
	go func() {
		result, err := creator.CreateRequestContext(ctx, opts)
		done <- queuedRequestResult{result, err}
	}()
	select {
	case result := <-done:
		t.Fatalf("full queue returned before capacity changed: %+v", result)
	case <-time.After(350 * time.Millisecond):
	}
	return done
}

func takeQueuedResult(t *testing.T, done <-chan queuedRequestResult) queuedRequestResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(3 * time.Second):
		t.Fatal("queue did not react to capacity/session change")
		return queuedRequestResult{}
	}
}

func TestRequestQueueWaitsWithoutPersistingOrHoldingWriter(t *testing.T) {
	creator, session, notifier := creatorAdmissionFixture(t, RateLimitActionQueue)
	creator.config.RequestTimeoutMinutes = 1
	seed, err := creator.CreateRequest(creatorAdmissionOptions(session))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := awaitQueuedAttempt(t, creator, creatorAdmissionOptions(session), ctx)
	count, err := creator.db.CountPendingBySession(session.ID)
	if err != nil || count != 1 || notifier.calls.Load() != 1 {
		t.Fatalf("waiting persisted/notified: count=%d notices=%d err=%v", count, notifier.calls.Load(), err)
	}
	// A second connection must be able to resolve the blocker while we wait.
	other, err := db.OpenWithOptions(creator.db.Path(), db.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	releasedAt := time.Now()
	if _, err := other.Exec(`UPDATE requests SET status = ? WHERE id = ?`, string(db.StatusCancelled), seed.Request.ID); err != nil {
		t.Fatal(err)
	}
	finished := takeQueuedResult(t, done)
	if finished.err != nil {
		t.Fatal(finished.err)
	}
	r := finished.result
	if !r.Queued || r.QueueWait < 350*time.Millisecond || r.Request.ID == seed.Request.ID || r.Request.CreatedAt.Before(releasedAt) || r.RateLimit.RemainingPending != 0 || notifier.calls.Load() != 2 {
		t.Fatalf("incorrect queued admission: %+v notices=%d", r, notifier.calls.Load())
	}
	if r.Request.ExpiresAt.Sub(r.Request.CreatedAt) < time.Minute-200*time.Millisecond {
		t.Fatal("queue consumed the new request's review lifetime")
	}
}

func TestRequestQueueCancellationHasNoPhantomRequest(t *testing.T) {
	for _, name := range []string{"cancel", "deadline", "pre-cancelled"} {
		t.Run(name, func(t *testing.T) {
			creator, session, notifier := creatorAdmissionFixture(t, RateLimitActionQueue)
			if _, err := creator.CreateRequest(creatorAdmissionOptions(session)); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := context.Canceled
			var finished queuedRequestResult
			if name == "pre-cancelled" {
				cancel()
				finished.result, finished.err = creator.CreateRequestContext(ctx, creatorAdmissionOptions(session))
			} else if name == "deadline" {
				deadlineCtx, stop := context.WithTimeout(ctx, 60*time.Millisecond)
				defer stop()
				finished.result, finished.err = creator.CreateRequestContext(deadlineCtx, creatorAdmissionOptions(session))
				want = context.DeadlineExceeded
			} else {
				done := awaitQueuedAttempt(t, creator, creatorAdmissionOptions(session), ctx)
				cancel()
				finished = takeQueuedResult(t, done)
			}
			if finished.result != nil || !errors.Is(finished.err, want) || strings.Contains(finished.err.Error(), "PANIC") {
				t.Fatalf("invalid cancellation: %+v", finished)
			}
			var limit *RateLimitError
			if name != "pre-cancelled" && !errors.As(finished.err, &limit) {
				t.Fatalf("lost quota reason: %v", finished.err)
			}
			count, err := creator.db.CountPendingBySession(session.ID)
			if err != nil || count != 1 || notifier.calls.Load() != 1 {
				t.Fatal("cancelled waiter persisted or notified")
			}
		})
	}
}

func TestRequestQueueRevalidatesAndDoesNotRetryStorageErrors(t *testing.T) {
	for _, name := range []string{"ended", "model-changed", "write-failure"} {
		t.Run(name, func(t *testing.T) {
			creator, session, notifier := creatorAdmissionFixture(t, RateLimitActionQueue)
			seed, err := creator.CreateRequest(creatorAdmissionOptions(session))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := awaitQueuedAttempt(t, creator, creatorAdmissionOptions(session), ctx)
			switch name {
			case "ended":
				if err := creator.db.EndSession(session.ID); err != nil {
					t.Fatal(err)
				}
			case "model-changed":
				if err := creator.db.UpdateSessionModel(session.ID, "model-b"); err != nil {
					t.Fatal(err)
				}
			case "write-failure":
				if _, err := creator.db.Exec(`CREATE TRIGGER queue_insert_failure BEFORE INSERT ON requests BEGIN SELECT RAISE(ABORT, 'queue storage failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := creator.db.Exec(`UPDATE requests SET status = ? WHERE id = ?`, string(db.StatusCancelled), seed.Request.ID); err != nil {
				t.Fatal(err)
			}
			finished := takeQueuedResult(t, done)
			if name == "model-changed" {
				if finished.err != nil || finished.result.Request.RequestorModel != "model-b" || !finished.result.Queued || notifier.calls.Load() != 2 {
					t.Fatalf("stale session snapshot: %+v", finished)
				}
			} else {
				if finished.err == nil || finished.result != nil || notifier.calls.Load() != 1 {
					t.Fatalf("invalid failed waiter: %+v", finished)
				}
				if name == "ended" && !errors.Is(finished.err, ErrSessionInactive) {
					t.Fatalf("wrong session error: %v", finished.err)
				}
				if name == "write-failure" && (errors.Is(finished.err, context.DeadlineExceeded) || !strings.Contains(finished.err.Error(), "queue storage failure")) {
					t.Fatalf("storage error was retried/hidden: %v", finished.err)
				}
			}
		})
	}
}

func TestRequestQueuePerMinuteReset(t *testing.T) {
	creator, session, _ := creatorAdmissionFixture(t, RateLimitActionQueue)
	creator.rateLimiter.cfg.MaxRequestsPerMinute = 1
	seed, err := creator.CreateRequest(creatorAdmissionOptions(session))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := creator.db.Exec(`UPDATE requests SET status = ?, created_at = ? WHERE id = ?`, string(db.StatusCancelled), time.Now().UTC().Add(-20*time.Second).Format(time.RFC3339), seed.Request.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := awaitQueuedAttempt(t, creator, creatorAdmissionOptions(session), ctx)
	if _, err := creator.rateLimiter.ResetRateLimits(session.ID); err != nil {
		t.Fatal(err)
	}
	finished := takeQueuedResult(t, done)
	if finished.err != nil || !finished.result.Queued || finished.result.RateLimit.RemainingPerMinute != 0 {
		t.Fatalf("queue ignored reset: %+v", finished)
	}
}

func TestRequestQueueDelayAndLifetimeBounds(t *testing.T) {
	for _, attempt := range []int{-1, 0, 1, 2, 100} {
		for i := 0; i < 100; i++ {
			delay := requestAdmissionRetryDelay(nil, attempt)
			if delay < 200*time.Millisecond || delay > time.Second {
				t.Fatalf("retry delay out of bounds: %v", delay)
			}
		}
	}
	limit := &RateLimitError{Pending: 0, MaxPending: 1, ResetAt: time.Now().Add(30 * time.Millisecond)}
	if delay := requestAdmissionRetryDelay(limit, 10); delay < 25*time.Millisecond || delay > 100*time.Millisecond {
		t.Fatalf("did not use near reset: %v", delay)
	}
	creator := &RequestCreator{}
	if creator.requestLifetime() != db.DefaultRequestTimeout {
		t.Fatal("missing config has no bounded fallback")
	}
	creator.config = &RequestCreatorConfig{RequestTimeoutMinutes: 2}
	if creator.requestLifetime() != 2*time.Minute {
		t.Fatal("configured lifetime ignored")
	}
	creator.config.RequestTimeoutMinutes = -1
	if creator.requestLifetime() != db.DefaultRequestTimeout {
		t.Fatal("invalid lifetime became unbounded")
	}
}
