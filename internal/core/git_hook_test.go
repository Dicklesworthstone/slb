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

	"github.com/Dicklesworthstone/slb/internal/db"
)

func gitHookApprovalFixture(t *testing.T) (*db.DB, GitHookIntent, *db.Request, *db.Session) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	project := t.TempDir()
	database, err := db.OpenAndMigrate(filepath.Join(project, ".slb", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	requester := &db.Session{AgentName: "hook-requester", Program: "test", Model: "model-a", ProjectPath: project}
	reviewer := &db.Session{AgentName: "hook-reviewer", Program: "test", Model: "model-b", ProjectPath: project}
	for _, session := range []*db.Session{requester, reviewer} {
		if err := database.CreateSession(session); err != nil {
			t.Fatal(err)
		}
	}
	intent := GitHookIntent{Operation: "pre-commit", ProjectPath: project, Snapshot: strings.Repeat("a", 64), Evidence: "staged file deletion"}
	expires := time.Now().UTC().Add(time.Hour)
	request := &db.Request{ProjectPath: project, Command: intent.CommandSpec(), RiskTier: db.RiskTierDangerous, Status: db.StatusApproved,
		RequestorSessionID: requester.ID, RequestorAgent: requester.AgentName, RequestorModel: requester.Model,
		MinApprovals: 1, ApprovalExpiresAt: &expires, Justification: db.Justification{Reason: "reviewed staged deletion"}}
	if err := database.CreateRequest(request); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	review := &db.Review{RequestID: request.ID, ReviewerSessionID: reviewer.ID, ReviewerAgent: reviewer.AgentName, ReviewerModel: reviewer.Model,
		Decision: db.DecisionApprove, SignatureTimestamp: now, Signature: db.ComputeReviewSignature(reviewer.SessionKey, request.ID, db.DecisionApprove, now)}
	if err := database.CreateReview(review); err != nil {
		t.Fatal(err)
	}
	return database, intent, request, requester
}

func TestGitHookAuthorizationConsumesExactlyOnce(t *testing.T) {
	database, intent, request, session := gitHookApprovalFixture(t)
	executor := NewExecutor(database, NewPatternEngine())
	checked := 0
	result, err := executor.AuthorizeGitHook(context.Background(), intent, request.ID, session.ID, func(context.Context) error { checked++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != db.StatusExecuted || checked != 1 {
		t.Fatalf("bad authorization: %+v checks=%d", result, checked)
	}
	log, err := os.ReadFile(result.Execution.LogPath)
	if err != nil || !strings.Contains(string(log), `"git_outcome":"not_observed"`) {
		t.Fatalf("missing authorization-only evidence: %s %v", log, err)
	}
	if _, err := executor.AuthorizeGitHook(context.Background(), intent, request.ID, session.ID, func(context.Context) error { return nil }); err == nil {
		t.Fatal("replayed approval")
	}
}

func TestGitHookAuthorizationRejectsInvalidBindingAndProof(t *testing.T) {
	for _, kind := range []string{"snapshot", "operation", "project", "session", "signature", "expired", "revalidation", "policy", "safe-policy"} {
		t.Run(kind, func(t *testing.T) {
			database, intent, request, session := gitHookApprovalFixture(t)
			sessionID := session.ID
			validator := func(context.Context) error { return nil }
			switch kind {
			case "snapshot":
				intent.Snapshot = strings.Repeat("b", 64)
			case "operation":
				intent.Operation = "pre-push"
			case "project":
				intent.ProjectPath = t.TempDir()
			case "session":
				sessionID = "another-session"
			case "signature":
				if _, err := database.Exec("UPDATE reviews SET signature = 'forged' WHERE request_id = ?", request.ID); err != nil {
					t.Fatal(err)
				}
			case "expired":
				if _, err := database.Exec("UPDATE requests SET approval_expires_at = '2000-01-01T00:00:00Z' WHERE id = ?", request.ID); err != nil {
					t.Fatal(err)
				}
			case "revalidation":
				validator = func(context.Context) error { return errors.New("index changed") }
			case "policy":
				writeExecutionPolicy(t, intent.ProjectPath, "[patterns.dangerous]\nmin_approvals = 3\n")
			case "safe-policy":
				writeExecutionPolicy(t, intent.ProjectPath, "[patterns.safe]\npatterns = ['.*']\n[patterns.dangerous]\nmin_approvals = 3\n")
			}
			if _, err := NewExecutor(database, NewPatternEngine()).AuthorizeGitHook(context.Background(), intent, request.ID, sessionID, validator); err == nil {
				t.Fatal("invalid authorization accepted")
			}
			stored, err := database.GetRequest(request.ID)
			if err != nil || stored.Status != db.StatusApproved || stored.Execution != nil {
				t.Fatalf("failed gate consumed approval: %+v %v", stored, err)
			}
		})
	}
}

func TestGitHookAuthorizationConcurrentClaim(t *testing.T) {
	database, intent, request, session := gitHookApprovalFixture(t)
	var wg sync.WaitGroup
	results := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := NewExecutor(database, NewPatternEngine()).AuthorizeGitHook(context.Background(), intent, request.ID, session.ID, func(context.Context) error { return nil })
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent authorization successes = %d, want 1", successes)
	}
}

func TestCreateGitHookRequestCannotBorrowAllowlist(t *testing.T) {
	database, intent, _, session := gitHookApprovalFixture(t)
	engine := NewPatternEngine()
	if err := engine.AddPattern(db.RiskTier("safe"), ".*", "test broad allowlist", "human"); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultRequestCreatorConfig()
	cfg.AgentMailEnabled = false
	creator := NewRequestCreator(database, nil, engine, cfg)
	for _, operation := range []string{"pre-commit", "pre-push"} {
		intent.Operation = operation
		request, err := creator.CreateGitHookRequest(context.Background(), intent, session.ID, "review requested")
		if err != nil {
			t.Fatal(err)
		}
		if request.Status != db.StatusPending || request.MinApprovals < 1 || request.RiskTier != intent.riskFloor() {
			t.Fatalf("allowlist weakened native hook: %+v", request)
		}
		if request.Command.Hash != intent.CommandSpec().Hash {
			t.Fatal("snapshot command changed")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := creator.CreateGitHookRequest(ctx, intent, session.ID, ""); err == nil {
		t.Fatal("canceled admission succeeded")
	}
	intent.Snapshot = "not-a-hash"
	if _, err := creator.CreateGitHookRequest(context.Background(), intent, session.ID, ""); err == nil {
		t.Fatal("invalid snapshot admitted")
	}
}
