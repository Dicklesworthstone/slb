package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func notaryPolicyFixture(t *testing.T) (*db.DB, *db.Request, *db.Session) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	project := t.TempDir()
	database, err := db.OpenAndMigrate(filepath.Join(project, ".slb", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	requester := &db.Session{AgentName: "notary-policy-requester", Model: "model-a", Program: "test", ProjectPath: project}
	reviewer := &db.Session{AgentName: "notary-policy-reviewer", Model: "model-b", Program: "test", ProjectPath: project}
	for _, session := range []*db.Session{requester, reviewer} {
		if err := database.CreateSession(session); err != nil {
			t.Fatal(err)
		}
	}
	expires := time.Now().UTC().Add(time.Hour)
	request := &db.Request{
		ProjectPath: project, Status: db.StatusApproved, RiskTier: db.RiskTierDangerous,
		Command:            db.CommandSpec{Raw: "echo policy-test", Argv: []string{"echo", "policy-test"}, Cwd: project, Shell: true},
		RequestorSessionID: requester.ID, RequestorAgent: requester.AgentName, RequestorModel: requester.Model,
		MinApprovals: 1, ApprovalExpiresAt: &expires, Justification: db.Justification{Reason: "test current policy"},
	}
	if err := database.CreateRequest(request); err != nil {
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
	return database, request, requester
}

func writeNotaryPolicy(t *testing.T, project, policy string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(project, ".slb", "config.toml"), []byte(policy), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestNotaryRechecksConfiguredQuorumAndModel(t *testing.T) {
	for _, policy := range []string{
		"[patterns.dangerous]\nmin_approvals = 2\n",
		"[general]\nrequire_different_model = true\n",
		"[patterns.safe]\npatterns = ['[']\n",
	} {
		t.Run(policy, func(t *testing.T) {
			database, request, session := notaryPolicyFixture(t)
			verifier := NewVerifier(database)
			params := VerifyExecuteParams{RequestID: request.ID, SessionID: session.ID,
				SessionKey: session.SessionKey, CommandHash: request.Command.Hash}
			before, err := verifier.VerifyExecutionAllowed(params)
			if err != nil || !before.Allowed {
				t.Fatalf("baseline approval rejected: %+v %v", before, err)
			}
			writeNotaryPolicy(t, request.ProjectPath, policy)
			after, err := verifier.VerifyAndMarkExecuting(params)
			if err != nil || after == nil || after.Allowed || after.ExecutionReceipt != "" || after.Reason == "" {
				t.Fatalf("notary authorized outdated/invalid policy: %+v %v", after, err)
			}
			stored, err := database.GetRequest(request.ID)
			if err != nil || stored.Status != db.StatusApproved || stored.Execution != nil {
				t.Fatalf("rejected policy consumed approval: %+v %v", stored, err)
			}
		})
	}
}

func TestHookUsesConfiguredPatternsAndReloads(t *testing.T) {
	_, request, _ := notaryPolicyFixture(t)
	writeNotaryPolicy(t, request.ProjectPath, "[patterns.critical]\npatterns = ['^hook-config-risk$']\nmin_approvals = 4\n")
	server := &IPCServer{}
	result := server.classifyCommand(HookQueryParams{Command: "hook-config-risk", CWD: request.ProjectPath})
	if result.Action != "block" || result.Tier != "critical" || result.MinApprovals != 4 {
		t.Fatalf("hook ignored configured policy: %+v", result)
	}
	writeNotaryPolicy(t, request.ProjectPath, "[patterns.critical]\npatterns = ['^different-risk$']\nmin_approvals = 5\n")
	result = server.classifyCommand(HookQueryParams{Command: "different-risk", CWD: request.ProjectPath})
	if result.Action != "block" || result.MinApprovals != 5 {
		t.Fatalf("hook retained stale config: %+v", result)
	}
	writeNotaryPolicy(t, request.ProjectPath, "[patterns.safe]\npatterns = ['[']\n")
	result = server.classifyCommand(HookQueryParams{Command: "git stash", CWD: request.ProjectPath})
	if result.Action == "allow" || result.ExecutionHandoff != nil {
		t.Fatalf("invalid config silently granted permission: %+v", result)
	}
}

func TestHookAcceptsValidDynamicQuorumHandoff(t *testing.T) {
	database, request, session := notaryPolicyFixture(t)
	writeNotaryPolicy(t, request.ProjectPath, "[patterns.dangerous]\npatterns = ['^echo policy-test$']\nmin_approvals = 3\ndynamic_quorum = true\ndynamic_quorum_floor = 1\n")
	server := &IPCServer{}
	params := HookQueryParams{Command: request.Command.Raw, CWD: request.ProjectPath, SessionID: session.ID, ExecutionHandoff: true}
	result := server.classifyCommand(params)
	if result.Action != "execute" || result.RequestID != request.ID || result.MinApprovals != 1 || result.ExecutionHandoff == nil {
		t.Fatalf("valid reduced quorum compared to advisory full quorum: %+v", result)
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil || stored.Status != db.StatusApproved {
		t.Fatalf("advisory hook consumed approval: %+v %v", stored, err)
	}
	// The same effective policy must authorize the authenticated notary claim.
	claim, err := NewVerifier(database).VerifyAndMarkExecuting(VerifyExecuteParams{
		RequestID: request.ID, SessionID: session.ID, SessionKey: session.SessionKey, CommandHash: request.Command.Hash,
	})
	if err != nil || claim == nil || !claim.Allowed || !validNotaryReceipt(claim.ExecutionReceipt) {
		t.Fatalf("notary disagreed with hook policy: %+v %v", claim, err)
	}
	result = server.classifyCommand(params)
	if result.Action == "execute" || result.Action == "allow" || result.ExecutionHandoff != nil {
		t.Fatalf("consumed request authorized a second execution: %+v", result)
	}
}
