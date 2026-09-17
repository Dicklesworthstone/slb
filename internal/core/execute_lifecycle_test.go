package core

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/integrations"
)

type executionOutcomeNotifier struct {
	integrations.NoopNotifier
	status db.RequestStatus
	code   int
}

func (n *executionOutcomeNotifier) NotifyRequestExecuted(req *db.Request, execution *db.Execution, code int) error {
	n.status = req.Status
	n.code = code
	return nil
}

func TestExecutorLifecyclePersistsOutcome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    string
		timeout time.Duration
		status  db.RequestStatus
	}{
		{"timeout", "wait", time.Second, db.StatusTimedOut},
		{"nonzero with suppressed output", "exit", 10 * time.Second, db.StatusExecutionFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, err := db.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			project := t.TempDir()
			session := &db.Session{
				ID: "lifecycle-session", AgentName: "LifecycleAgent", Program: "test",
				Model: "test-model", ProjectPath: project,
			}
			if err := database.CreateSession(session); err != nil {
				t.Fatal(err)
			}
			spec := *lifecycleCommand(t, tc.mode)
			spec.Cwd = project
			spec.Hash = db.ComputeCommandHash(spec)
			req := &db.Request{
				ProjectPath: project, RequestorSessionID: session.ID,
				RequestorAgent: session.AgentName, RequestorModel: session.Model,
				Command: spec, RiskTier: db.RiskTierCaution, Status: db.StatusApproved,
				Justification: db.Justification{Reason: "Test execution lifecycle"},
			}
			if err := database.CreateRequest(req); err != nil {
				t.Fatal(err)
			}
			notifier := &executionOutcomeNotifier{}
			executor := NewExecutor(database, NewPatternEngine()).WithNotifier(notifier)
			result, err := executor.ExecuteApprovedRequest(context.Background(), ExecuteOptions{
				RequestID: req.ID, SessionID: session.ID, Timeout: tc.timeout,
				LogDir: project, SuppressOutput: true,
			})
			if tc.status == db.StatusTimedOut {
				if !errors.Is(err, ErrExecutionTimeout) || result == nil || !result.TimedOut {
					t.Fatalf("expected timeout outcome, got %+v, %v", result, err)
				}
			} else if err != nil {
				t.Fatalf("ordinary nonzero exit must not be an I/O failure: %v", err)
			}
			if result == nil || result.ExitCode == 0 || result.Duration <= 0 || result.Output == "" {
				t.Fatalf("lost child outcome: %+v", result)
			}
			stored, err := database.GetRequest(req.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, request := range []*db.Request{stored, result.Request} {
				if request.Status != tc.status || request.Execution == nil {
					t.Fatalf("stale request outcome: %+v", request)
				}
				e := request.Execution
				if e.ExitCode == nil || *e.ExitCode != result.ExitCode || e.DurationMs == nil || *e.DurationMs != result.Duration.Milliseconds() {
					t.Errorf("execution metadata disagrees with child: %+v vs %+v", e, result)
				}
			}
			if notifier.status != tc.status || notifier.code != result.ExitCode {
				t.Errorf("notifier saw stale outcome: %+v", notifier)
			}
			log, err := os.ReadFile(result.LogPath)
			if err != nil || !strings.Contains(string(log), strings.TrimSpace(result.Output)) {
				t.Errorf("suppressed output was not logged: %q, %v", log, err)
			}
		})
	}
}
