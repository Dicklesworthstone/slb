package core

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestWaitReturnsEveryNonPendingDecision(t *testing.T) {
	for _, status := range []db.RequestStatus{
		db.StatusApproved, db.StatusRejected, db.StatusCancelled, db.StatusTimeout,
		db.StatusEscalated, db.StatusExecuting, db.StatusExecuted,
		db.StatusExecutionFailed, db.StatusTimedOut, db.RequestStatus("unknown"),
	} {
		t.Run(string(status), func(t *testing.T) {
			calls := 0
			request := &db.Request{ID: "test", Status: status}
			got, err := waitForDecision(context.Background(), WaitOptions{Timeout: time.Second}, func(context.Context) (*db.Request, error) {
				calls++
				return request, nil
			})
			if err != nil || got != request || calls != 1 {
				t.Fatalf("decision not returned immediately: request=%+v err=%v calls=%d", got, err, calls)
			}
		})
	}
}

func TestWaitRefreshesUntilDecision(t *testing.T) {
	calls := 0
	got, err := waitForDecision(context.Background(), WaitOptions{Timeout: time.Second, PollInterval: time.Millisecond}, func(context.Context) (*db.Request, error) {
		calls++
		status := db.StatusPending
		if calls == 3 {
			status = db.StatusApproved
		}
		return &db.Request{ID: "test", Status: status}, nil
	})
	if err != nil || got.Status != db.StatusApproved || calls != 3 {
		t.Fatalf("did not observe decision: %+v %v calls=%d", got, err, calls)
	}
}

func TestWaitClientDeadlinePreservesPendingSnapshot(t *testing.T) {
	request := &db.Request{ID: "test", Status: db.StatusPending}
	got, err := waitForDecision(context.Background(), WaitOptions{Timeout: 20 * time.Millisecond, PollInterval: time.Hour}, func(context.Context) (*db.Request, error) {
		return request, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || got != request || request.Status != db.StatusPending || request.ResolvedAt != nil {
		t.Fatalf("client deadline rewrote or lost shared request: %+v %v", got, err)
	}
}

func TestWaitCancellationStopsRefreshAndSleep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	_, err := waitForDecision(ctx, WaitOptions{}, func(context.Context) (*db.Request, error) {
		calls++
		return nil, nil
	})
	if !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("cancelled waiter performed work: %v calls=%d", err, calls)
	}

	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	refreshed := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := waitForDecision(ctx, WaitOptions{PollInterval: time.Hour}, func(context.Context) (*db.Request, error) {
			close(refreshed)
			return &db.Request{Status: db.StatusPending}, nil
		})
		done <- err
	}()
	<-refreshed
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt the poll interval")
	}
}

func TestWaitCancellationDuringApprovalNeverAuthorizesCaller(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := waitForDecision(ctx, WaitOptions{}, func(context.Context) (*db.Request, error) {
		cancel()
		return &db.Request{Status: db.StatusApproved}, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("approval escaped a cancelled wait: %v", err)
	}
}

func TestWaitPropagatesPolicyErrors(t *testing.T) {
	want := errors.New("policy cannot be loaded")
	_, err := waitForDecision(context.Background(), WaitOptions{}, func(context.Context) (*db.Request, error) {
		return nil, want
	})
	if !errors.Is(err, want) {
		t.Fatalf("policy failure hidden: %v", err)
	}
}

func TestWaitValidatesInputs(t *testing.T) {
	for _, opts := range []WaitOptions{{Timeout: -1}, {PollInterval: -1}} {
		calls := 0
		_, err := waitForDecision(context.Background(), opts, func(context.Context) (*db.Request, error) {
			calls++
			return nil, nil
		})
		if err == nil || calls != 0 {
			t.Fatalf("invalid wait performed work: %v calls=%d", err, calls)
		}
	}
	if _, err := WaitForDecision(context.Background(), nil, "test", WaitOptions{}); err == nil {
		t.Fatal("nil database accepted")
	}
	if _, err := WaitForDecision(context.Background(), &db.DB{}, "", WaitOptions{}); err == nil {
		t.Fatal("empty request ID accepted")
	}
	if _, err := waitForDecision(context.Background(), WaitOptions{}, func(context.Context) (*db.Request, error) {
		return nil, nil
	}); err == nil {
		t.Fatal("nil request accepted")
	}
}
