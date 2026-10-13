package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

// The mismatched command is harmless even if the gate regresses: printf, not
// a deletion or other destructive operation. A custom CRITICAL rule supplies
// the policy requirement that the misleading SAFE display must not bypass.
func commandSpecExecutionFixture(t *testing.T) (*db.DB, *db.Request, *db.Session) {
	t.Helper()
	database, original, session := policyExecutionFixture(t)
	if _, err := database.InsertCustomPattern("critical", `^printf\s+command-spec-probe$`, "test invocation policy", "human"); err != nil {
		t.Fatal(err)
	}
	request := &db.Request{
		ProjectPath: original.ProjectPath, Status: db.StatusApproved, RiskTier: db.RiskTierDangerous,
		Command:            db.CommandSpec{Raw: "git stash", Argv: []string{"printf", "command-spec-probe"}, Cwd: original.ProjectPath},
		RequestorSessionID: session.ID, RequestorAgent: session.AgentName, RequestorModel: session.Model,
		MinApprovals: 1, ApprovalExpiresAt: original.ApprovalExpiresAt,
		Justification: db.Justification{Reason: "test executable/display mismatch"},
	}
	if err := database.CreateRequest(request); err != nil {
		t.Fatal(err)
	}
	reviewer := &db.Session{AgentName: "spec-reviewer", Program: "test", Model: "model-b", ProjectPath: original.ProjectPath}
	if err := database.CreateSession(reviewer); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	review := &db.Review{RequestID: request.ID, ReviewerSessionID: reviewer.ID,
		ReviewerAgent: reviewer.AgentName, ReviewerModel: reviewer.Model,
		Decision: db.DecisionApprove, SignatureTimestamp: now,
		Signature: db.ComputeReviewSignature(reviewer.SessionKey, request.ID, db.DecisionApprove, now)}
	if err := database.CreateReview(review); err != nil {
		t.Fatal(err)
	}
	return database, request, session
}

func TestCommandSpecPolicyBlocksBeforePreflight(t *testing.T) {
	database, request, session := commandSpecExecutionFixture(t)
	executor := NewExecutor(database, NewPatternEngine())
	if allowed, reason := executor.CanExecute(request.ID); allowed || reason == "" {
		t.Fatalf("advisory check trusted display: allowed=%v reason=%q", allowed, reason)
	}
	logDir := filepath.Join(request.ProjectPath, "must-not-create")
	result, err := executor.ExecuteApprovedRequest(context.Background(), ExecuteOptions{
		RequestID: request.ID, SessionID: session.ID, LogDir: logDir,
		CaptureRollback: true, SuppressOutput: true,
	})
	if result != nil || !errors.Is(err, ErrTierEscalated) {
		t.Fatalf("mismatched display reached execution: result=%+v error=%v", result, err)
	}
	if _, err := os.Stat(logDir); !os.IsNotExist(err) {
		t.Fatalf("policy rejection touched preflight files: %v", err)
	}
	assertPolicyUnclaimed(t, database, request.ID)
}

func TestCommandSpecPolicyBlocksBothAtomicClaimPaths(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		name := "client"
		if authenticated {
			name = "authenticated-daemon"
		}
		t.Run(name, func(t *testing.T) {
			database, request, session := commandSpecExecutionFixture(t)
			var err error
			if authenticated {
				err = database.ClaimRequestExecutionAuthenticated(request, policyExecutionIdentity(session), session.SessionKey, ExecutionPolicyGuard(""))
			} else {
				err = database.ClaimRequestExecution(request, policyExecutionIdentity(session), ExecutionPolicyGuard(""))
			}
			if !errors.Is(err, ErrTierEscalated) {
				t.Fatalf("atomic claim trusted display: %v", err)
			}
			assertPolicyUnclaimed(t, database, request.ID)
		})
	}
}
