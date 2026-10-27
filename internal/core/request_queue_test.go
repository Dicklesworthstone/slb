package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
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

// The second preflight quota check can only be reached after the first full
// queue result passed through the retry loop. Pause at the existing clock seam
// so tests change capacity/cancellation at a known retry, not after a sleep.
func pauseQueuedRetry(t *testing.T, creator *RequestCreator, opts CreateRequestOptions, ctx context.Context) (<-chan queuedRequestResult, func()) {
	t.Helper()
	creator.config.EnableDryRun = true
	paused, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	resume := func() { once.Do(func() { close(release) }) }
	t.Cleanup(resume)
	checks := 0
	creator.rateLimiter.now = func() time.Time {
		checks++
		if checks == 2 {
			close(paused)
			<-release
		}
		return time.Now()
	}
	done := make(chan queuedRequestResult, 1)
	go func() {
		result, err := creator.CreateRequestContext(ctx, opts)
		done <- queuedRequestResult{result, err}
	}()
	select {
	case <-paused:
	case result := <-done:
		t.Fatalf("request returned before observing a full queue and retrying: %+v", result)
	case <-time.After(5 * time.Second):
		t.Fatal("request did not reach its second quota check")
	}
	return done, resume
}

func TestRequestQueueWaitsWithoutPersistingOrHoldingWriter(t *testing.T) {
	creator, session, notifier := creatorAdmissionFixture(t, RateLimitActionQueue)
	creator.config.RequestTimeout = time.Minute
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
	for _, name := range []string{"cancel", "deadline-before-attempt", "pre-cancelled"} {
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
			} else if name == "deadline-before-attempt" {
				// A deadline may expire before any quota is observed. Exercise
				// that case explicitly instead of assuming a 60ms timer permits
				// one database attempt on every host.
				deadlineCtx, stop := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer stop()
				finished.result, finished.err = creator.CreateRequestContext(deadlineCtx, creatorAdmissionOptions(session))
				want = context.DeadlineExceeded
			} else {
				done, resume := pauseQueuedRetry(t, creator, creatorAdmissionOptions(session), ctx)
				cancel()
				resume()
				finished = takeQueuedResult(t, done)
			}
			if finished.result != nil || !errors.Is(finished.err, want) || strings.Contains(finished.err.Error(), "PANIC") {
				t.Fatalf("invalid cancellation: %+v", finished)
			}
			var limit *RateLimitError
			if name == "cancel" && !errors.As(finished.err, &limit) {
				t.Fatalf("lost quota reason: %v", finished.err)
			}
			if name != "cancel" && errors.As(finished.err, &limit) {
				t.Fatalf("cancellation before admission invented a quota failure: %v", finished.err)
			}
			count, err := creator.db.CountPendingBySession(session.ID)
			if err != nil || count != 1 || notifier.calls.Load() != 1 {
				t.Fatal("cancelled waiter persisted or notified")
			}
		})
	}
}

func TestRequestQueueCancellationDuringRetryPreservesLimit(t *testing.T) {
	creator, session, notifier := creatorAdmissionFixture(t, RateLimitActionQueue)
	seed, err := creator.CreateRequest(creatorAdmissionOptions(session))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done, resume := pauseQueuedRetry(t, creator, creatorAdmissionOptions(session), ctx)
	// Free capacity while the retry is paused. It now reaches context-aware
	// DB admission rather than returning a second quota failure. Skip preview
	// tools so cancellation is returned by the real DB BeginTx boundary.
	if _, err := creator.db.Exec(`UPDATE requests SET status = ? WHERE id = ?`, string(db.StatusCancelled), seed.Request.ID); err != nil {
		t.Fatal(err)
	}
	creator.config.EnableDryRun = false
	cancel()
	resume()
	finished := takeQueuedResult(t, done)
	var limit *RateLimitError
	if finished.result != nil || !errors.Is(finished.err, context.Canceled) || !errors.As(finished.err, &limit) {
		t.Fatalf("retry cancellation lost its observed quota: result=%+v err=%v", finished.result, finished.err)
	}
	if limit.SessionID != session.ID || limit.Pending != 1 || limit.MaxPending != 1 || !strings.Contains(finished.err.Error(), "beginning request admission") {
		t.Fatalf("retry cancellation lost original DB context or quota details: %v", finished.err)
	}
	var count int
	if err := creator.db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&count); err != nil || count != 1 || notifier.calls.Load() != 1 {
		t.Fatalf("cancelled retry persisted or notified: count=%d notices=%d err=%v", count, notifier.calls.Load(), err)
	}
}

func TestRequestQueueCancellationDoesNotMaskStorageFailure(t *testing.T) {
	creator, session, notifier := creatorAdmissionFixture(t, RateLimitActionQueue)
	if _, err := creator.CreateRequest(creatorAdmissionOptions(session)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done, resume := pauseQueuedRetry(t, creator, creatorAdmissionOptions(session), ctx)
	// The next non-context DB read must report the closed store even though
	// the caller cancels at the same synchronization point.
	if err := creator.db.Close(); err != nil {
		t.Fatal(err)
	}
	cancel()
	resume()
	finished := takeQueuedResult(t, done)
	var limit *RateLimitError
	if finished.result != nil || finished.err == nil || !strings.Contains(finished.err.Error(), "database is closed") ||
		errors.Is(finished.err, context.Canceled) || errors.As(finished.err, &limit) || notifier.calls.Load() != 1 {
		t.Fatalf("cancellation replaced or retried a storage failure: %+v", finished)
	}
}

type cancellingQueueNotifier struct {
	*admissionNotifier
	cancel context.CancelFunc
}

func (n *cancellingQueueNotifier) NotifyNewRequest(request *db.Request) error {
	err := n.admissionNotifier.NotifyNewRequest(request)
	n.cancel()
	return err
}

func TestRequestQueueCommittedRequestWinsOverCancellation(t *testing.T) {
	creator, session, notifier := creatorAdmissionFixture(t, RateLimitActionQueue)
	seed, err := creator.CreateRequest(creatorAdmissionOptions(session))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	creator.notifier = &cancellingQueueNotifier{admissionNotifier: notifier, cancel: cancel}
	done, resume := pauseQueuedRetry(t, creator, creatorAdmissionOptions(session), ctx)
	if _, err := creator.db.Exec(`UPDATE requests SET status = ? WHERE id = ?`, string(db.StatusCancelled), seed.Request.ID); err != nil {
		t.Fatal(err)
	}
	creator.config.EnableDryRun = false
	resume()
	finished := takeQueuedResult(t, done)
	if finished.err != nil || finished.result == nil || !finished.result.Queued || ctx.Err() != context.Canceled {
		t.Fatalf("post-commit cancellation hid a durable request: %+v", finished)
	}
	stored, err := creator.db.GetRequest(finished.result.Request.ID)
	if err != nil || stored.Status != db.StatusPending || stored.ID == seed.Request.ID || notifier.calls.Load() != 2 {
		t.Fatalf("successful queued request was not committed/notified: %+v %v", stored, err)
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
					t.Fatalf("stale session snapshot: result=%+v err=%v", finished.result, finished.err)
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
	creator.config = &RequestCreatorConfig{RequestTimeout: 2 * time.Second}
	if creator.requestLifetime() != 2*time.Second {
		t.Fatal("configured lifetime ignored")
	}
	creator.config.RequestTimeout = -1
	if creator.requestLifetime() != db.DefaultRequestTimeout {
		t.Fatal("invalid lifetime became unbounded")
	}
}

func TestRequestAdmissionResolvesExpiredCapacityWithCurrentPolicy(t *testing.T) {
	for _, action := range []string{"escalate", "auto_reject", "auto_approve_warn"} {
		t.Run(action, func(t *testing.T) {
			creator, session, notifier := creatorAdmissionFixture(t, RateLimitActionReject)
			seed, err := creator.CreateRequest(creatorAdmissionOptions(session))
			if err != nil {
				t.Fatal(err)
			}
			writeExecutionPolicy(t, session.ProjectPath, "[general]\ntimeout_action = '"+action+"'\n")
			if _, err := creator.db.Exec(`UPDATE requests SET expires_at = ? WHERE id = ?`, time.Now().UTC().Add(-time.Second).Format(time.RFC3339), seed.Request.ID); err != nil {
				t.Fatal(err)
			}
			// Another requester's expired work must not be swept by this admission.
			otherSession := &db.Session{AgentName: "other-requester", Model: "other-model", ProjectPath: session.ProjectPath}
			if err := creator.db.CreateSession(otherSession); err != nil {
				t.Fatal(err)
			}
			other := *seed.Request
			other.ID, other.RequestorSessionID, other.RequestorAgent, other.RequestorModel = "", otherSession.ID, otherSession.AgentName, otherSession.Model
			past := time.Now().UTC().Add(-time.Second)
			other.ExpiresAt = &past
			if err := creator.db.CreateRequest(&other); err != nil {
				t.Fatal(err)
			}
			result, err := creator.CreateRequestContext(context.Background(), creatorAdmissionOptions(session))
			if err != nil {
				t.Fatalf("expired pending row blocked daemonless admission: %v", err)
			}
			want := db.StatusEscalated
			if action == "auto_reject" {
				want = db.StatusTimeout
			}
			resolved, err := creator.db.GetRequest(seed.Request.ID)
			if err != nil || resolved.Status != want || resolved.ResolvedAt == nil || resolved.Execution != nil || resolved.ApprovalExpiresAt != nil {
				t.Fatalf("expiry did not preserve deny/escalate policy: %+v, %v", resolved, err)
			}
			untouched, err := creator.db.GetRequest(other.ID)
			if err != nil || untouched.Status != db.StatusPending || untouched.ResolvedAt != nil {
				t.Fatalf("changed another requester's work: %+v, %v", untouched, err)
			}
			if result.Request.Status != db.StatusPending || result.Request.ID == seed.Request.ID || result.RateLimit.RemainingPending != 0 || result.RateLimit.RemainingPerMinute != 8 || notifier.calls.Load() != 2 {
				t.Fatalf("admission reset quota, reused request, or notified incorrectly: %+v", result)
			}
			var votes int
			if err := creator.db.QueryRow(`SELECT COUNT(*) FROM reviews`).Scan(&votes); err != nil || votes != 0 {
				t.Fatalf("expiry manufactured review evidence: %d %v", votes, err)
			}
		})
	}
}

func TestRequestAdmissionExpiredCapacityFailsClosedOnPolicyError(t *testing.T) {
	creator, session, notifier := creatorAdmissionFixture(t, RateLimitActionQueue)
	seed, err := creator.CreateRequest(creatorAdmissionOptions(session))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := creator.db.Exec(`UPDATE requests SET expires_at = ? WHERE id = ?`, time.Now().UTC().Add(-time.Second).Format(time.RFC3339), seed.Request.ID); err != nil {
		t.Fatal(err)
	}
	writeExecutionPolicy(t, session.ProjectPath, "[general]\ntimeout_action = 'invalid'\n")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := creator.CreateRequestContext(ctx, creatorAdmissionOptions(session))
	if result != nil || err == nil || errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "loading command policy") {
		t.Fatalf("policy failure was ignored or retried as capacity: %+v, %v", result, err)
	}
	stored, err := creator.db.GetRequest(seed.Request.ID)
	if err != nil || stored.Status != db.StatusPending || stored.ResolvedAt != nil || notifier.calls.Load() != 1 {
		t.Fatalf("broken policy changed pending evidence: %+v, %v", stored, err)
	}
	var count int
	if err := creator.db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("failed admission created a request: %d %v", count, err)
	}
}

func TestRequestQueueAdvancesExpiryWithoutAnotherProcess(t *testing.T) {
	creator, session, _ := creatorAdmissionFixture(t, RateLimitActionQueue)
	creator.config.RequestTimeout = 2 * time.Second
	seed, err := creator.CreateRequest(creatorAdmissionOptions(session))
	if err != nil {
		t.Fatal(err)
	}
	creator.config.RequestTimeout = time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	finished := takeQueuedResult(t, awaitQueuedAttempt(t, creator, creatorAdmissionOptions(session), ctx))
	if finished.err != nil || !finished.result.Queued || finished.result.Request.Status != db.StatusPending {
		t.Fatalf("queue failed to resolve its own expired blocker: %+v", finished)
	}
	stored, err := creator.db.GetRequest(seed.Request.ID)
	if err != nil || stored.Status != db.StatusEscalated || stored.Execution != nil {
		t.Fatalf("expired blocker did not escalate: %+v %v", stored, err)
	}
}

func TestRequestAdmissionExpiredCapacityUsesExplicitConfig(t *testing.T) {
	creator, session, _ := creatorAdmissionFixture(t, RateLimitActionReject)
	seed, err := creator.CreateRequest(creatorAdmissionOptions(session))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := creator.db.Exec(`UPDATE requests SET expires_at = ? WHERE id = ?`, time.Now().UTC().Add(-time.Second).Format(time.RFC3339), seed.Request.ID); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "explicit.toml")
	if err := os.WriteFile(configPath, []byte("[general]\ntimeout_action = 'auto_reject'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	engine, err := LoadCommandPolicy(creator.db, config.LoadOptions{ProjectDir: session.ProjectPath, ConfigPath: configPath})
	if err != nil {
		t.Fatal(err)
	}
	creator.patternEngine = engine
	if _, err := creator.CreateRequest(creatorAdmissionOptions(session)); err != nil {
		t.Fatal(err)
	}
	stored, err := creator.db.GetRequest(seed.Request.ID)
	if err != nil || stored.Status != db.StatusTimeout {
		t.Fatalf("admission ignored explicit timeout policy: %+v, %v", stored, err)
	}
}
