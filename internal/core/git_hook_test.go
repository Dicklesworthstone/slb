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

func TestSameHostGitHookRechecksCurrentPolicy(t *testing.T) {
	for _, change := range []string{"none", "opt-out-before", "opt-out-during", "blocked-requester", "conflict-before", "conflict-during"} {
		t.Run(change, func(t *testing.T) {
			quorum := 1
			if strings.HasPrefix(change, "conflict-") {
				quorum = 2
			}
			database, pending, author, peers := sameHostPolicyFixture(t, quorum)
			intent := GitHookIntent{Operation: "pre-commit", ProjectPath: pending.ProjectPath,
				Snapshot: strings.Repeat("a", 64), Evidence: "disposable snapshot; no Git operation runs"}
			engine, err := LoadCommandPolicy(database, config.LoadOptions{ProjectDir: intent.ProjectPath})
			if err != nil {
				t.Fatal(err)
			}
			options := DefaultRequestCreatorConfig()
			options.ApprovalPolicy, options.AgentMailEnabled = db.ApprovalPolicySameHost, false
			request, err := NewRequestCreator(database, nil, engine, options).CreateGitHookRequest(context.Background(), intent, author.ID, "verify current native Git review policy")
			if err != nil {
				t.Fatal(err)
			}
			applySameHostFixtureReview(t, database, request, peers[0], db.DecisionApprove)
			if quorum == 2 {
				applySameHostFixtureReview(t, database, request, peers[1], db.DecisionReject)
				applySameHostFixtureReview(t, database, request, peers[2], db.DecisionApprove)
			}
			mutate := func() {
				policy := "[general]\napproval_policy = 'independent'\n"
				switch change {
				case "blocked-requester":
					policy = sameHostPolicyText(string(ConflictFirstWins), quorum) + "[agents]\nblocked = ['" + author.AgentName + "']\n"
				case "conflict-before", "conflict-during":
					policy = sameHostPolicyText(string(ConflictAnyRejectionBlocks), quorum) + "[patterns.safe]\npatterns = ['.*']\n"
				}
				writeExecutionPolicy(t, intent.ProjectPath, policy)
			}
			if change == "opt-out-before" || change == "blocked-requester" || change == "conflict-before" {
				mutate()
			}
			result, err := NewExecutor(database, engine).AuthorizeGitHook(context.Background(), intent, request.ID, author.ID, func(context.Context) error {
				if change == "opt-out-during" || change == "conflict-during" {
					mutate()
				}
				return nil
			})
			if change != "none" {
				want := ErrApprovalPolicyChanged
				if change == "blocked-requester" {
					want = ErrAgentBlocked
				}
				if !errors.Is(err, want) {
					t.Fatalf("native Git ignored %s: %v", change, err)
				}
				assertPolicyUnclaimed(t, database, request.ID)
				return
			}
			if err != nil || result.Execution == nil || result.Execution.ApprovalPolicy != db.ApprovalPolicySameHost || result.Execution.Assurance != db.AssuranceSameHost {
				t.Fatalf("native same-host authorization: %+v %v", result, err)
			}
			receipt, err := os.ReadFile(result.Execution.LogPath)
			if err != nil || !strings.Contains(string(receipt), `"approval_policy":"same_host"`) || !strings.Contains(string(receipt), "not independently authenticated authorization") {
				t.Fatalf("native receipt lost review category: %s %v", receipt, err)
			}
		})
	}
}

func gitHookApprovalFixture(t *testing.T, operation ...string) (*db.DB, GitHookIntent, *db.Request, *db.Session) {
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
	if len(operation) != 0 {
		intent.Operation = operation[0]
	}
	reviewers := []*db.Session{reviewer}
	if intent.Operation == "pre-rebase" {
		writeExecutionPolicy(t, project, "[patterns.critical]\nmin_approvals = 2\ndynamic_quorum = false\n")
		second := &db.Session{AgentName: "second-hook-reviewer", Program: "test", Model: "model-c", ProjectPath: project}
		if err := database.CreateSession(second); err != nil {
			t.Fatal(err)
		}
		reviewers = append(reviewers, second)
	}
	expires := time.Now().UTC().Add(time.Hour)
	request := &db.Request{ProjectPath: project, Command: intent.CommandSpec(), RiskTier: intent.riskFloor(), Status: db.StatusApproved,
		RequestorSessionID: requester.ID, RequestorAgent: requester.AgentName, RequestorModel: requester.Model,
		MinApprovals: len(reviewers), ApprovalExpiresAt: &expires, Justification: db.Justification{Reason: "reviewed Git snapshot"}}
	if err := database.CreateRequest(request); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, reviewer := range reviewers {
		review := &db.Review{RequestID: request.ID, ReviewerSessionID: reviewer.ID, ReviewerAgent: reviewer.AgentName, ReviewerModel: reviewer.Model,
			Decision: db.DecisionApprove, SignatureTimestamp: now, Signature: db.ComputeReviewSignature(reviewer.SessionKey, request.ID, db.DecisionApprove, now)}
		if err := database.CreateReview(review); err != nil {
			t.Fatal(err)
		}
	}
	return database, intent, request, requester
}

func TestGitHookAuthorizationConsumesExactlyOnce(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
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
	db.SimulateIndependentIdentityForTest(t)
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
	for _, operation := range []string{"pre-commit", "pre-push", "pre-rebase"} {
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
		if operation == "pre-rebase" && (request.RiskTier != db.RiskTierCritical || !strings.Contains(request.Justification.SafetyArgument, "--onto")) {
			t.Fatalf("rebase scope or critical floor lost: %+v", request)
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

func TestGitRebaseAuthorizationGates(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	for _, kind := range []string{"success-and-replay", "stale-ref-state", "wrong-operation", "critical-quorum", "safe-allowlist", "expired"} {
		t.Run(kind, func(t *testing.T) {
			database, intent, request, session := gitHookApprovalFixture(t, "pre-rebase")
			validate := func(context.Context) error { return nil }
			switch kind {
			case "stale-ref-state":
				validate = func(context.Context) error { return errors.New("Git snapshot changed") }
			case "wrong-operation":
				intent.Operation = "pre-push"
			case "critical-quorum":
				writeExecutionPolicy(t, intent.ProjectPath, "[patterns.critical]\nmin_approvals = 3\ndynamic_quorum = false\n")
			case "safe-allowlist":
				writeExecutionPolicy(t, intent.ProjectPath, "[patterns.safe]\npatterns = ['.*']\n[patterns.critical]\nmin_approvals = 3\ndynamic_quorum = false\n")
			case "expired":
				if _, err := database.Exec("UPDATE requests SET approval_expires_at = '2000-01-01T00:00:00Z' WHERE id = ?", request.ID); err != nil {
					t.Fatal(err)
				}
			}
			executor := NewExecutor(database, NewPatternEngine())
			result, err := executor.AuthorizeGitHook(context.Background(), intent, request.ID, session.ID, validate)
			if kind == "success-and-replay" {
				if err != nil || result.Status != db.StatusExecuted {
					t.Fatalf("valid rebase approval: %+v %v", result, err)
				}
				if _, err := executor.AuthorizeGitHook(context.Background(), intent, request.ID, session.ID, validate); err == nil {
					t.Fatal("replayed rebase authorization")
				}
			} else {
				if err == nil {
					t.Fatal("invalid rebase approval accepted")
				}
				stored, err := database.GetRequest(request.ID)
				if err != nil || stored.Status != db.StatusApproved || stored.Execution != nil {
					t.Fatalf("failed rebase gate consumed approval: %+v %v", stored, err)
				}
			}
		})
	}
}
