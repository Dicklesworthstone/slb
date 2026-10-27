package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/testutil"
)

func policyExecutionFixture(t *testing.T) (*db.DB, *db.Request, *db.Session) {
	t.Helper()
	t.Setenv("HOME", testutil.NonSystemTempDir(t))
	t.Setenv("USERPROFILE", testutil.NonSystemTempDir(t))
	project := testutil.NonSystemTempDir(t)
	database, err := db.OpenAndMigrate(filepath.Join(project, ".slb", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	requester := &db.Session{AgentName: "policy-requester", Program: "test", Model: "model-a", ProjectPath: project}
	reviewer := &db.Session{AgentName: "policy-reviewer", Program: "test", Model: "model-b", ProjectPath: project}
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
		MinApprovals: 1, ApprovalExpiresAt: &expires, Justification: db.Justification{Reason: "test policy gate"},
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

func writeExecutionPolicy(t *testing.T, project, text string) string {
	t.Helper()
	path := filepath.Join(project, ".slb", "config.toml")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func policyExecutionIdentity(session *db.Session) *db.Execution {
	now := time.Now().UTC()
	return &db.Execution{ExecutedAt: &now, ExecutedBySessionID: session.ID,
		ExecutedByAgent: session.AgentName, ExecutedByModel: session.Model, LogPath: "policy-test-claim"}
}

func assertPolicyUnclaimed(t *testing.T, database *db.DB, id string) {
	t.Helper()
	stored, err := database.GetRequest(id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != db.StatusApproved || stored.Execution != nil {
		t.Fatalf("rejected claim consumed approval: %+v", stored)
	}
}

func TestSameHostExecutionPolicyRevocationIsCheckedAtClaim(t *testing.T) {
	// This exercises real local reviews without simulated independent identity.
	database, existing, requester := policyExecutionFixture(t)
	writeExecutionPolicy(t, existing.ProjectPath, "[general]\napproval_policy = 'same_host'\n")
	engine, err := LoadCommandPolicy(database, config.LoadOptions{ProjectDir: existing.ProjectPath})
	if err != nil {
		t.Fatal(err)
	}
	options := DefaultRequestCreatorConfig()
	options.ApprovalPolicy = db.ApprovalPolicySameHost
	options.AgentMailEnabled, options.EnableDryRun = false, false
	creator := NewRequestCreator(database, nil, engine, options)
	created, err := creator.CreateRequest(CreateRequestOptions{SessionID: requester.ID,
		Command: "echo same-host-policy-test", Cwd: existing.ProjectPath,
		Justification: Justification{Reason: "exercise explicit local review"}})
	if err != nil {
		t.Fatal(err)
	}
	request := created.Request
	if request.ApprovalPolicy != db.ApprovalPolicySameHost {
		t.Fatalf("new request lost explicit policy: %+v", request)
	}
	reviewer := &db.Session{AgentName: "SameHostReviewer", Program: "test", Model: "different-model", ProjectPath: request.ProjectPath}
	if err := database.CreateSession(reviewer); err != nil {
		t.Fatal(err)
	}
	reviewOptions := ReviewOptions{RequestID: request.ID, SessionID: reviewer.ID, SessionKey: reviewer.SessionKey, Decision: db.DecisionApprove}
	if _, err := NewReviewService(database, DefaultReviewConfig()).SubmitReview(reviewOptions); err == nil {
		t.Fatal("default review service accepted same-host request without opt-in")
	}
	reviewConfig := DefaultReviewConfig()
	reviewConfig.ApprovalPolicy = db.ApprovalPolicySameHost
	result, err := NewReviewService(database, reviewConfig).SubmitReview(reviewOptions)
	if err != nil || result == nil || !VerifyReview(result.Review, reviewer.SessionKey) {
		t.Fatalf("same-host review failed signature verification: %+v %v", result, err)
	}
	request, err = database.GetRequest(request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckExecutionPolicy(database, request, ""); err != nil {
		t.Fatal(err)
	}
	// Even adding a SAFE rule cannot promote the recorded local review after
	// the operator restores the independent policy between preflight and claim.
	writeExecutionPolicy(t, request.ProjectPath, "[general]\napproval_policy = 'independent'\n[patterns.safe]\npatterns = ['^echo same-host-policy-test$']\n")
	if err := database.ClaimRequestExecution(request, policyExecutionIdentity(requester), ExecutionPolicyGuard("")); !errors.Is(err, ErrApprovalPolicyChanged) {
		t.Fatalf("claim ignored same-host opt-in revocation: %v", err)
	}
	assertPolicyUnclaimed(t, database, request.ID)
	writeExecutionPolicy(t, request.ProjectPath, "[general]\napproval_policy = 'same_host'\n")
	if err := database.ClaimRequestExecution(request, policyExecutionIdentity(requester), ExecutionPolicyGuard("")); err != nil {
		t.Fatalf("restoring explicit same-host policy should allow unchanged evidence: %v", err)
	}
	claimed, err := database.GetRequest(request.ID)
	if err != nil || claimed.Execution == nil || claimed.Execution.ApprovalPolicy != db.ApprovalPolicySameHost || claimed.Execution.Assurance != db.AssuranceSameHost {
		t.Fatalf("claim lost same-host evidence classification: %+v %v", claimed, err)
	}
}

func TestExecutionPolicyRejectsChangesBeforePreflight(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	for _, tc := range []struct {
		name, text string
		want       error
	}{
		{"quorum", "[patterns.dangerous]\nmin_approvals = 2\n", ErrApprovalPolicyChanged},
		{"model", "[general]\nrequire_different_model = true\n", ErrApprovalPolicyChanged},
		{"regex", "[patterns.safe]\npatterns = ['[']\n", nil},
		{"syntax", "not valid TOML", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, request, session := policyExecutionFixture(t)
			executor := NewExecutor(database, NewPatternEngine())
			writeExecutionPolicy(t, request.ProjectPath, tc.text)
			if allowed, reason := executor.CanExecute(request.ID); allowed || reason == "" {
				t.Fatalf("advisory check ignored changed policy: %v %q", allowed, reason)
			}
			logDir := filepath.Join(request.ProjectPath, "must-not-create")
			result, err := executor.ExecuteApprovedRequest(context.Background(), ExecuteOptions{
				RequestID: request.ID, SessionID: session.ID, LogDir: logDir, CaptureRollback: true, SuppressOutput: true,
			})
			if result != nil || err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("changed policy reached execution: %+v %v", result, err)
			}
			if _, err := os.Stat(logDir); !os.IsNotExist(err) {
				t.Fatalf("policy rejection touched preflight files: %v", err)
			}
			assertPolicyUnclaimed(t, database, request.ID)
		})
	}
}

func TestExecutionPolicyClaimRechecksAfterAdvisory(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	for _, authenticated := range []bool{false, true} {
		for _, change := range []string{"custom-rule", "config-quorum"} {
			t.Run(fmt.Sprintf("authenticated=%t/%s", authenticated, change), func(t *testing.T) {
				database, request, session := policyExecutionFixture(t)
				if err := CheckExecutionPolicy(database, request, ""); err != nil {
					t.Fatalf("baseline policy: %v", err)
				}
				want := ErrApprovalPolicyChanged
				if change == "custom-rule" {
					if _, err := database.InsertCustomPattern("critical", `^echo\s+policy-test$`, "new restriction", "human"); err != nil {
						t.Fatal(err)
					}
					want = ErrTierEscalated
				} else {
					writeExecutionPolicy(t, request.ProjectPath, "[patterns.dangerous]\nmin_approvals = 3\n")
				}
				var err error
				if authenticated {
					err = database.ClaimRequestExecutionAuthenticated(request, policyExecutionIdentity(session), session.SessionKey, ExecutionPolicyGuard(""))
				} else {
					err = database.ClaimRequestExecution(request, policyExecutionIdentity(session), ExecutionPolicyGuard(""))
				}
				if !errors.Is(err, want) {
					t.Fatalf("claim ignored policy change after advisory: %v", err)
				}
				assertPolicyUnclaimed(t, database, request.ID)
			})
		}
	}
}

func TestExecutionPolicyClaimSerializesPolicyWriters(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	database, request, session := policyExecutionFixture(t)
	other, err := db.OpenAndMigrate(database.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	writerDone := make(chan error, 1)
	guard := func(tx *sql.Tx, current *db.Request) error {
		if err := CheckExecutionPolicy(tx, current, ""); err != nil {
			return err
		}
		started := make(chan struct{})
		go func() {
			close(started)
			_, err := other.InsertCustomPattern("critical", `^echo\s+policy-test$`, "concurrent restriction", "human")
			writerDone <- err
		}()
		<-started
		select {
		case err := <-writerDone:
			return fmt.Errorf("policy writer crossed execution reservation: %v", err)
		case <-time.After(30 * time.Millisecond):
			return nil
		}
	}
	if err := database.ClaimRequestExecution(request, policyExecutionIdentity(session), guard); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-writerDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("policy writer did not resume after claim committed")
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil || stored.Status != db.StatusExecuting {
		t.Fatalf("serialized execution claim lost: %+v %v", stored, err)
	}
}

func TestExecutionPolicyFailedGuardRollsBackClaim(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	database, request, session := policyExecutionFixture(t)
	sentinel := errors.New("policy read failed")
	err := database.ClaimRequestExecution(request, policyExecutionIdentity(session), func(*sql.Tx, *db.Request) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("policy error swallowed: %v", err)
	}
	assertPolicyUnclaimed(t, database, request.ID)
	if err := database.ClaimRequestExecution(request, policyExecutionIdentity(session), nil); err == nil {
		t.Fatal("nil policy guard accepted")
	}
	assertPolicyUnclaimed(t, database, request.ID)
}

func TestExecutionPolicyDynamicQuorumIsProjectScoped(t *testing.T) {
	database, request, session := policyExecutionFixture(t)
	writeExecutionPolicy(t, request.ProjectPath, "[patterns.dangerous]\nmin_approvals = 3\ndynamic_quorum = true\ndynamic_quorum_floor = 1\n")
	for i := 0; i < 4; i++ {
		other := &db.Session{AgentName: fmt.Sprintf("other-%d", i), Program: "test", Model: "model-b", ProjectPath: t.TempDir()}
		if err := database.CreateSession(other); err != nil {
			t.Fatal(err)
		}
	}
	if count, err := CountPolicyReviewers(database, request.ProjectPath, session.ID, session.AgentName); err != nil || count != 1 {
		t.Fatalf("wrong project pool: count=%d err=%v", count, err)
	}
	if err := CheckExecutionPolicy(database, request, ""); err != nil {
		t.Fatalf("valid dynamic quorum rejected: %v", err)
	}
	joined := &db.Session{AgentName: "new-local-reviewer", Program: "test", Model: "model-b", ProjectPath: request.ProjectPath}
	if err := database.CreateSession(joined); err != nil {
		t.Fatal(err)
	}
	if err := CheckExecutionPolicy(database, request, ""); !errors.Is(err, ErrApprovalPolicyChanged) {
		t.Fatalf("larger current pool ignored: %v", err)
	}
	if err := database.EndSession(joined.ID); err != nil {
		t.Fatal(err)
	}
	if err := CheckExecutionPolicy(database, request, ""); err != nil {
		t.Fatalf("ended reviewer still counted: %v", err)
	}
}

func TestExecutionPolicyPreservesExplicitConfigSource(t *testing.T) {
	database, request, _ := policyExecutionFixture(t)
	path := filepath.Join(t.TempDir(), "explicit.toml")
	if err := os.WriteFile(path, []byte("[patterns.dangerous]\nmin_approvals = 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	engine, err := LoadCommandPolicy(database, config.LoadOptions{ProjectDir: request.ProjectPath, ConfigPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if allowed, _ := NewExecutor(database, engine).CanExecute(request.ID); allowed {
		t.Fatal("executor discarded explicit config and used project defaults")
	}
	missing := filepath.Join(t.TempDir(), "missing.toml")
	if _, err := LoadCommandPolicy(database, config.LoadOptions{ConfigPath: missing}); err == nil {
		t.Fatal("explicit missing config silently downgraded policy")
	}
}

func TestExecutionPolicyKeepsSignedProofAndProjectGates(t *testing.T) {
	t.Run("allowlist does not bypass proof", func(t *testing.T) {
		database, request, session := policyExecutionFixture(t)
		writeExecutionPolicy(t, request.ProjectPath, "[patterns.safe]\npatterns = ['^echo policy-test$']\n")
		if _, err := database.Exec(`UPDATE reviews SET signature = 'forged' WHERE request_id = ?`, request.ID); err != nil {
			t.Fatal(err)
		}
		_, err := NewExecutor(database, NewPatternEngine()).ExecuteApprovedRequest(context.Background(), ExecuteOptions{RequestID: request.ID, SessionID: session.ID})
		if !errors.Is(err, db.ErrInvalidApprovalProof) {
			t.Fatalf("current allowlist bypassed original proof: %v", err)
		}
		assertPolicyUnclaimed(t, database, request.ID)
	})
	t.Run("local executor cannot cross projects", func(t *testing.T) {
		database, request, _ := policyExecutionFixture(t)
		outsider := &db.Session{AgentName: "outside", Model: "model-a", Program: "test", ProjectPath: t.TempDir()}
		if err := database.CreateSession(outsider); err != nil {
			t.Fatal(err)
		}
		_, err := NewExecutor(database, NewPatternEngine()).ExecuteApprovedRequest(context.Background(), ExecuteOptions{RequestID: request.ID, SessionID: outsider.ID})
		if !errors.Is(err, db.ErrExecutionAuthentication) {
			t.Fatalf("local scope mismatch accepted: %v", err)
		}
		if err := database.ClaimRequestExecution(request, policyExecutionIdentity(outsider), ExecutionPolicyGuard("")); !errors.Is(err, db.ErrInvalidTransition) {
			t.Fatalf("database scope mismatch accepted: %v", err)
		}
		assertPolicyUnclaimed(t, database, request.ID)
	})
}

func TestExecutionPolicyValidRequestExecutesOnce(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	database, request, session := policyExecutionFixture(t)
	executor := NewExecutor(database, NewPatternEngine())
	opts := ExecuteOptions{RequestID: request.ID, SessionID: session.ID,
		LogDir: filepath.Join(request.ProjectPath, "logs"), SuppressOutput: true}
	result, err := executor.ExecuteApprovedRequest(context.Background(), opts)
	if err != nil || result == nil || result.ExitCode != 0 || result.Request.Status != db.StatusExecuted {
		t.Fatalf("valid request failed: %+v %v", result, err)
	}
	if _, err := executor.ExecuteApprovedRequest(context.Background(), opts); !errors.Is(err, ErrAlreadyExecuted) {
		t.Fatalf("execution replay allowed: %v", err)
	}
}
