package core

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/integrations"
)

type admissionNotifier struct {
	integrations.NoopNotifier
	calls atomic.Int32
}

func (n *admissionNotifier) NotifyNewRequest(*db.Request) error {
	n.calls.Add(1)
	return nil
}

func creatorAdmissionFixture(t *testing.T, action RateLimitAction) (*RequestCreator, *db.Session, *admissionNotifier) {
	t.Helper()
	project := t.TempDir()
	database, err := db.OpenAndMigrate(filepath.Join(project, "admission.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	session := &db.Session{AgentName: "creator", ProjectPath: project, Model: "model-a"}
	if err := database.CreateSession(session); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultRequestCreatorConfig()
	cfg.AgentMailEnabled = false
	limiter := NewRateLimiter(database, RateLimitConfig{MaxPendingPerSession: 1, MaxRequestsPerMinute: 10, Action: action})
	creator := NewRequestCreator(database, limiter, NewPatternEngine(), cfg)
	notifier := &admissionNotifier{}
	creator.notifier = notifier
	return creator, session, notifier
}

func creatorAdmissionOptions(session *db.Session) CreateRequestOptions {
	return CreateRequestOptions{
		SessionID: session.ID, Command: "rm -rf ./build", Cwd: session.ProjectPath,
		Justification: Justification{Reason: "clear rebuildable test artifacts"},
	}
}

func TestRequestCreatorAtomicAdmission(t *testing.T) {
	creator, session, notifier := creatorAdmissionFixture(t, RateLimitActionReject)
	// This successful snapshot must not become a reusable insertion permit.
	if snapshot, err := creator.rateLimiter.CheckRateLimit(session.ID); err != nil || !snapshot.Allowed {
		t.Fatalf("initial snapshot: %+v %v", snapshot, err)
	}
	const clients = 12
	start := make(chan struct{})
	results := make(chan error, clients)
	var wg sync.WaitGroup
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := creator.CreateRequest(creatorAdmissionOptions(session))
			if err == nil && (result.Request == nil || result.RateLimit == nil || !result.RateLimit.Allowed || result.RateLimit.RemainingPending != 0) {
				t.Error("missing committed admission result")
			}
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
			continue
		}
		var denied *RateLimitError
		if !errors.As(err, &denied) {
			t.Errorf("not a typed rate-limit rejection: %v", err)
		}
	}
	count, err := creator.db.CountPendingBySession(session.ID)
	if err != nil || count != 1 || accepted != 1 || notifier.calls.Load() != 1 {
		t.Fatalf("accepted=%d stored=%d notifications=%d err=%v", accepted, count, notifier.calls.Load(), err)
	}
}

func TestRequestCreatorWarningAdmission(t *testing.T) {
	creator, session, notifier := creatorAdmissionFixture(t, RateLimitActionWarn)
	for i := 0; i < 2; i++ {
		result, err := creator.CreateRequest(creatorAdmissionOptions(session))
		if err != nil {
			t.Fatal(err)
		}
		if result.RateLimit == nil || !result.RateLimit.Allowed || result.RateLimit.RemainingPerMinute != 9-i {
			t.Fatalf("wrong admission counters: %+v", result.RateLimit)
		}
		if i == 1 && !strings.Contains(result.RateLimit.Message, "pending limit exceeded") {
			t.Fatalf("warning lost: %+v", result.RateLimit)
		}
	}
	if notifier.calls.Load() != 2 {
		t.Fatal("successful warning admissions must notify")
	}
}

func TestRequestCreatorFailedAdmissionNeverNotifies(t *testing.T) {
	for _, name := range []string{"write-failure", "project-mismatch"} {
		t.Run(name, func(t *testing.T) {
			creator, session, notifier := creatorAdmissionFixture(t, RateLimitActionWarn)
			opts := creatorAdmissionOptions(session)
			if name == "write-failure" {
				if _, err := creator.db.Exec(`CREATE TRIGGER creator_admission_failure BEFORE INSERT ON requests BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
					t.Fatal(err)
				}
			} else {
				opts.ProjectPath = t.TempDir()
			}
			if _, err := creator.CreateRequest(opts); err == nil {
				t.Fatal("invalid admission succeeded")
			}
			count, err := creator.db.CountPendingBySession(session.ID)
			if err != nil || count != 0 || notifier.calls.Load() != 0 {
				t.Fatalf("failed request persisted or notified: count=%d notifications=%d err=%v", count, notifier.calls.Load(), err)
			}
		})
	}
}

func TestRateLimiterCancelledAdmission(t *testing.T) {
	creator, session, notifier := creatorAdmissionFixture(t, RateLimitActionReject)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &db.Request{
		ProjectPath: session.ProjectPath, RequestorSessionID: session.ID,
		RequestorAgent: session.AgentName, RequestorModel: session.Model,
		Command: db.CommandSpec{Raw: "echo test", Cwd: session.ProjectPath},
	}
	if _, err := creator.rateLimiter.AdmitRequest(ctx, r); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
	if r.ID != "" || notifier.calls.Load() != 0 {
		t.Fatal("cancelled admission had side effects")
	}
}
