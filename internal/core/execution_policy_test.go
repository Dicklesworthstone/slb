package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
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
	// Model a resolved approval, not merely a forged status bit. The current
	// TTL constraint needs the original approval time even for legacy fixtures.
	if _, err := database.Exec(`UPDATE requests SET resolved_at = ? WHERE id = ?`, now.Format(time.RFC3339Nano), request.ID); err != nil {
		t.Fatal(err)
	}
	request.ResolvedAt = &now
	return database, request, requester
}

func writeExecutionPolicy(t *testing.T, project, text string) string {
	t.Helper()
	path := filepath.Join(project, ".slb", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
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
	service := NewReviewService(database, DefaultReviewConfig())
	writeExecutionPolicy(t, request.ProjectPath, "[general]\napproval_policy = 'independent'\n")
	if _, err := service.SubmitReview(reviewOptions); !errors.Is(err, db.ErrReviewPolicyMismatch) {
		t.Fatalf("current independent policy accepted a same-host request: %v", err)
	}
	// Current explicit opt-in is authoritative even for a service whose
	// original snapshot predates it. The request remains separately bound.
	writeExecutionPolicy(t, request.ProjectPath, "[general]\napproval_policy = 'same_host'\n")
	result, err := service.SubmitReview(reviewOptions)
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

func TestSameHostExecutionRechecksTightenedConflictPolicy(t *testing.T) {
	for _, resolution := range []ConflictResolution{ConflictAnyRejectionBlocks, ConflictHumanBreaksTie} {
		for _, safe := range []bool{false, true} {
			for _, authenticated := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/safe=%t/authenticated=%t", resolution, safe, authenticated), func(t *testing.T) {
					database, existing, requester := policyExecutionFixture(t)
					firstWins := "[general]\napproval_policy = 'same_host'\nconflict_resolution = 'first_wins'\n"
					writeExecutionPolicy(t, existing.ProjectPath, firstWins)
					request := &db.Request{
						ProjectPath: existing.ProjectPath, ApprovalPolicy: db.ApprovalPolicySameHost,
						RequestorSessionID: requester.ID, RequestorAgent: requester.AgentName, RequestorModel: requester.Model,
						Command:  db.CommandSpec{Raw: "echo conflict-policy", Argv: []string{"echo", "conflict-policy"}, Cwd: existing.ProjectPath},
						RiskTier: db.RiskTierDangerous, MinApprovals: 2,
						Justification: db.Justification{Reason: "Check current conflict policy before executing"},
					}
					if err := database.CreateRequest(request); err != nil {
						t.Fatal(err)
					}
					reviewConfig := DefaultReviewConfig()
					reviewConfig.ApprovalPolicy, reviewConfig.ConflictResolution = db.ApprovalPolicySameHost, ConflictFirstWins
					service := NewReviewService(database, reviewConfig)
					for i, decision := range []db.Decision{db.DecisionApprove, db.DecisionReject, db.DecisionApprove} {
						reviewer := &db.Session{AgentName: fmt.Sprintf("conflict-reviewer-%d", i), Model: "review-model", ProjectPath: request.ProjectPath}
						if err := database.CreateSession(reviewer); err != nil {
							t.Fatal(err)
						}
						if _, err := service.SubmitReview(ReviewOptions{RequestID: request.ID, SessionID: reviewer.ID,
							SessionKey: reviewer.SessionKey, Decision: decision}); err != nil {
							t.Fatal(err)
						}
					}
					var err error
					request, err = database.GetRequest(request.ID)
					if err != nil {
						t.Fatal(err)
					}
					if request.Status != db.StatusApproved {
						t.Fatalf("first_wins did not resolve the reviewed request: %s", request.Status)
					}
					executor := NewExecutor(database, nil)
					if allowed, reason := executor.CanExecute(request.ID); !allowed {
						t.Fatalf("original conflict policy rejected its valid approval: %s", reason)
					}

					tightened := fmt.Sprintf("[general]\napproval_policy = 'same_host'\nconflict_resolution = '%s'\n", resolution)
					if safe {
						tightened += "[patterns.safe]\npatterns = ['^echo conflict-policy$']\n"
					}
					writeExecutionPolicy(t, request.ProjectPath, tightened)
					if allowed, reason := executor.CanExecute(request.ID); allowed || reason == "" {
						t.Fatalf("current policy ignored the recorded rejection: allowed=%t reason=%q", allowed, reason)
					}
					if authenticated {
						err = database.ClaimRequestExecutionAuthenticated(request, policyExecutionIdentity(requester), requester.SessionKey, ExecutionPolicyGuard(""))
					} else {
						err = database.ClaimRequestExecution(request, policyExecutionIdentity(requester), ExecutionPolicyGuard(""))
					}
					if !errors.Is(err, ErrApprovalPolicyChanged) {
						t.Fatalf("claim ignored tightened conflict policy: %v", err)
					}
					assertPolicyUnclaimed(t, database, request.ID)
					if _, err := database.VerifyRequestApproval(request.ID); err != nil {
						t.Fatalf("policy change rewrote the historical review evidence: %v", err)
					}
					writeExecutionPolicy(t, request.ProjectPath, firstWins)
					if err := database.ClaimRequestExecution(request, policyExecutionIdentity(requester), ExecutionPolicyGuard("")); err != nil {
						t.Fatalf("restored original policy rejected unchanged evidence: %v", err)
					}
				})
			}
		}
	}
}

func sameHostPolicyText(resolution string, quorum int) string {
	return fmt.Sprintf("[general]\napproval_policy = 'same_host'\nconflict_resolution = %q\nrequire_different_model = true\n[patterns.dangerous]\npatterns = ['^printf same-host-policy$']\nmin_approvals = %d\ndynamic_quorum = false\n[integrations]\nagent_mail_enabled = false\n", resolution, quorum)
}

func sameHostPolicyFixture(t *testing.T, quorum int) (*db.DB, *db.Request, *db.Session, []*db.Session) {
	t.Helper()
	t.Setenv(db.SimulatedIdentityEnvForTest, "")
	t.Setenv("HOME", testutil.NonSystemTempDir(t))
	t.Setenv("USERPROFILE", testutil.NonSystemTempDir(t))
	project := testutil.NonSystemTempDir(t)
	database, err := db.OpenAndMigrate(filepath.Join(project, ".slb", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	writeExecutionPolicy(t, project, sameHostPolicyText(string(ConflictFirstWins), quorum))
	author := &db.Session{AgentName: "same-host-author", Model: "model-a", ProjectPath: project}
	peers := []*db.Session{
		{AgentName: "first-peer", Model: "model-b", ProjectPath: project},
		{AgentName: "second-peer", Model: "model-c", ProjectPath: project},
		{AgentName: "third-peer", Model: "model-d", ProjectPath: project},
	}
	for _, session := range append([]*db.Session{author}, peers...) {
		if err := database.CreateSession(session); err != nil {
			t.Fatal(err)
		}
	}
	engine, err := LoadCommandPolicy(database, config.LoadOptions{ProjectDir: project})
	if err != nil {
		t.Fatal(err)
	}
	options := DefaultRequestCreatorConfig()
	options.ApprovalPolicy = db.ApprovalPolicySameHost
	options.AgentMailEnabled, options.EnableDryRun = false, false
	created, err := NewRequestCreator(database, nil, engine, options).CreateRequest(CreateRequestOptions{
		SessionID: author.ID, Command: "printf same-host-policy", Cwd: project,
		Justification: Justification{Reason: "disposable same-host policy fixture"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Request.ApprovalPolicy != db.ApprovalPolicySameHost || created.Request.MinApprovals != quorum {
		t.Fatalf("wrong fixture admission: %+v", created.Request)
	}
	return database, created.Request, author, peers
}

func applySameHostFixtureReview(t *testing.T, database *db.DB, request *db.Request, reviewer *db.Session, decision db.Decision) {
	t.Helper()
	options := DefaultReviewConfig()
	options.ApprovalPolicy, options.ConflictResolution = db.ApprovalPolicySameHost, ConflictFirstWins
	result, err := NewReviewService(database, options).SubmitReview(ReviewOptions{
		RequestID: request.ID, SessionID: reviewer.ID, SessionKey: reviewer.SessionKey, Decision: decision,
	})
	if err != nil || result == nil || !db.VerifyReviewEvidenceSignature(result.Review, reviewer.SessionKey) {
		t.Fatalf("same-host fixture review: %+v %v", result, err)
	}
}

func TestSameHostReviewServiceReloadsPolicyBeforeCommit(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit=%t", explicit), func(t *testing.T) {
			database, request, _, peers := sameHostPolicyFixture(t, 1)
			options := DefaultReviewConfig()
			options.ApprovalPolicy = db.ApprovalPolicySameHost
			path := filepath.Join(request.ProjectPath, ".slb", "config.toml")
			if explicit {
				path = filepath.Join(t.TempDir(), "explicit-policy.toml")
				options.ConfigPath = path
				if err := os.WriteFile(path, []byte(sameHostPolicyText(string(ConflictFirstWins), 1)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			service := NewReviewService(database, options)
			// The service retains its original configuration while the operator
			// revokes local approval in its actual policy source.
			if err := os.WriteFile(path, []byte("[general]\napproval_policy = 'independent'\n"), 0600); err != nil {
				t.Fatal(err)
			}
			review := ReviewOptions{RequestID: request.ID, SessionID: peers[0].ID, SessionKey: peers[0].SessionKey, Decision: db.DecisionApprove}
			if _, err := service.SubmitReview(review); !errors.Is(err, db.ErrReviewPolicyMismatch) {
				t.Fatalf("long-lived reviewer ignored opt-out: %v", err)
			}
			if reviews, err := database.ListReviewsForRequest(request.ID); err != nil || len(reviews) != 0 {
				t.Fatalf("refused approval left evidence: %+v %v", reviews, err)
			}
			// Opt-out must still allow an authenticated reviewer to reject.
			review.Decision = db.DecisionReject
			if _, err := service.SubmitReview(review); err != nil {
				t.Fatalf("policy opt-out disabled rejection: %v", err)
			}
		})
	}
}

func TestSameHostExecutionUsesCurrentConflictPolicy(t *testing.T) {
	for _, resolution := range []ConflictResolution{ConflictFirstWins, ConflictAnyRejectionBlocks, ConflictHumanBreaksTie} {
		for _, safe := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/safe=%t", resolution, safe), func(t *testing.T) {
				database, request, author, peers := sameHostPolicyFixture(t, 2)
				for i, decision := range []db.Decision{db.DecisionApprove, db.DecisionReject, db.DecisionApprove} {
					applySameHostFixtureReview(t, database, request, peers[i], decision)
				}
				request, err := database.VerifyRequestApproval(request.ID)
				if err != nil {
					t.Fatalf("valid first-wins proof rejected: %v", err)
				}
				if err := CheckExecutionPolicy(database, request, ""); err != nil {
					t.Fatalf("first-wins advisory rejected: %v", err)
				}
				policy := sameHostPolicyText(string(resolution), 2)
				if safe {
					policy += "[patterns.safe]\npatterns = ['^printf same-host-policy$']\n"
				}
				writeExecutionPolicy(t, request.ProjectPath, policy)
				advisoryErr := CheckExecutionPolicy(database, request, "")
				claimErr := database.ClaimRequestExecution(request, policyExecutionIdentity(author), ExecutionPolicyGuard(""))
				if resolution == ConflictFirstWins {
					if advisoryErr != nil || claimErr != nil {
						t.Fatalf("unchanged first-wins should permit quorum: advisory=%v claim=%v", advisoryErr, claimErr)
					}
					return
				}
				if !errors.Is(advisoryErr, ErrApprovalPolicyChanged) || !errors.Is(claimErr, ErrApprovalPolicyChanged) {
					t.Fatalf("current conflict policy was ignored: advisory=%v claim=%v", advisoryErr, claimErr)
				}
				assertPolicyUnclaimed(t, database, request.ID)
			})
		}
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

func TestExecutionCurrentTTLCannotExtendHistoricalApproval(t *testing.T) {
	for _, tier := range []db.RiskTier{db.RiskTierDangerous, db.RiskTierCritical} {
		for _, safe := range []bool{false, true} {
			for _, authenticated := range []bool{false, true} {
				for _, shorten := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/safe=%t/authenticated=%t/shorten=%t", tier, safe, authenticated, shorten), func(t *testing.T) {
						database, request, author, peers := sameHostPolicyFixture(t, 1)
						// Creation time is signed. Age it before the real review,
						// then advance only its resolution clock to model elapsed time.
						created := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Second)
						if _, err := database.Exec(`UPDATE requests SET created_at = ?, risk_tier = ? WHERE id = ?`, created.Format(time.RFC3339Nano), tier, request.ID); err != nil {
							t.Fatal(err)
						}
						writeExecutionPolicy(t, request.ProjectPath, sameHostReviewSettingsText("first_wins", 30, 10, 1))
						applySameHostFixtureReview(t, database, request, peers[0], db.DecisionApprove)
						anchor := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
						if _, err := database.Exec(`UPDATE requests SET resolved_at = ? WHERE id = ?`, anchor.Format(time.RFC3339Nano), request.ID); err != nil {
							t.Fatal(err)
						}
						approved, err := database.VerifyRequestApproval(request.ID)
						if err != nil {
							t.Fatalf("original signed approval is invalid: %v", err)
						}
						if err := CheckExecutionPolicy(database, approved, ""); err != nil {
							t.Fatalf("original approval policy rejected valid evidence: %v", err)
						}

						minutes := 1
						if !shorten {
							// A larger current TTL cannot revive the persisted grant
							// after its original deadline has passed.
							minutes = 60
							expired := time.Now().UTC().Add(-time.Second)
							if _, err := database.Exec(`UPDATE requests SET approval_expires_at = ? WHERE id = ?`, expired.Format(time.RFC3339Nano), request.ID); err != nil {
								t.Fatal(err)
							}
						}
						before, reviewsBefore, err := database.GetRequestWithReviews(request.ID)
						if err != nil {
							t.Fatal(err)
						}
						var eventsBefore int
						if err := database.QueryRow(`SELECT COUNT(*) FROM request_events WHERE request_id = ?`, request.ID).Scan(&eventsBefore); err != nil {
							t.Fatal(err)
						}
						policy := sameHostReviewSettingsText("first_wins", minutes, minutes, 1)
						if safe {
							policy += "[patterns.safe]\npatterns = ['^printf same-host-policy$']\n"
						}
						writeExecutionPolicy(t, request.ProjectPath, policy)
						if err := CheckExecutionPolicy(database, before, ""); !errors.Is(err, ErrApprovalPolicyChanged) || !strings.Contains(err.Error(), "approval has expired under current TTL") {
							t.Fatalf("current TTL or original deadline was ignored: %v", err)
						}
						// The claim must re-read policy even when the caller holds
						// a proof that passed before configuration changed.
						if authenticated {
							err = database.ClaimRequestExecutionAuthenticated(approved, policyExecutionIdentity(author), author.SessionKey, ExecutionPolicyGuard(""))
						} else {
							err = database.ClaimRequestExecution(approved, policyExecutionIdentity(author), ExecutionPolicyGuard(""))
						}
						wantErr := ErrApprovalPolicyChanged
						if !shorten {
							wantErr = db.ErrInvalidApprovalProof
						}
						if !errors.Is(err, wantErr) {
							t.Fatalf("claim did not revalidate the deadline: got %v, want %v", err, wantErr)
						}
						after, reviewsAfter, err := database.GetRequestWithReviews(request.ID)
						if err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(reviewsBefore, reviewsAfter) {
							t.Fatalf("refused execution changed historical evidence: before=%+v after=%+v", before, after)
						}
						var eventsAfter int
						if err := database.QueryRow(`SELECT COUNT(*) FROM request_events WHERE request_id = ?`, request.ID).Scan(&eventsAfter); err != nil {
							t.Fatal(err)
						}
						if eventsAfter != eventsBefore {
							t.Fatalf("refused execution changed committed audit events: got %d, want %d", eventsAfter, eventsBefore)
						}
					})
				}
			}
		}
	}
}

func TestCurrentApprovalTTLValidatesOriginalAnchor(t *testing.T) {
	now := time.Date(2026, time.October, 27, 12, 0, 0, 0, time.UTC)
	for _, problem := range []string{"missing anchor", "zero anchor", "future anchor", "before creation", "missing creation", "missing expiry", "zero TTL", "overflow TTL"} {
		t.Run(problem, func(t *testing.T) {
			anchor, expiry := now.Add(-time.Minute), now.Add(time.Hour)
			request := &db.Request{
				RiskTier: db.RiskTierDangerous, MinApprovals: 1, CreatedAt: now.Add(-2 * time.Minute),
				ResolvedAt: &anchor, ApprovalExpiresAt: &expiry,
			}
			cfg := config.DefaultConfig().General
			switch problem {
			case "missing anchor":
				request.ResolvedAt = nil
			case "zero anchor":
				anchor = time.Time{}
			case "future anchor":
				anchor = now.Add(time.Second)
			case "before creation":
				anchor = now.Add(-3 * time.Minute)
			case "missing creation":
				request.CreatedAt = time.Time{}
			case "missing expiry":
				request.ApprovalExpiresAt = nil
			case "zero TTL":
				cfg.ApprovalTTLMins = 0
			case "overflow TTL":
				cfg.ApprovalTTLMins = int(^uint(0) >> 1)
			}
			if err := checkCurrentApprovalTTL(request, cfg, now); !errors.Is(err, ErrApprovalPolicyChanged) {
				t.Fatalf("invalid approval anchor or TTL accepted: %v", err)
			}
		})
	}
}

func TestCurrentApprovalTTLAutoApprovalAnchors(t *testing.T) {
	now := time.Date(2026, time.October, 27, 12, 0, 0, 0, time.UTC)
	for _, scenario := range []string{"initial admission", "expired initial admission", "delayed timer", "model requires review", "stored deadline wins"} {
		t.Run(scenario, func(t *testing.T) {
			request := &db.Request{RiskTier: db.RiskTierCaution, CreatedAt: now.Add(-30 * time.Second)}
			cfg := config.DefaultConfig().General
			cfg.ApprovalTTLMins = 1
			wantAllowed := true
			switch scenario {
			case "expired initial admission":
				request.CreatedAt = now.Add(-2 * time.Minute)
				wantAllowed = false
			case "delayed timer":
				request.CreatedAt = now.Add(-10 * time.Minute)
				resolved := now.Add(-30 * time.Second)
				request.ResolvedAt = &resolved
			case "model requires review":
				request.RequireDifferentModel = true
				wantAllowed = false
			case "stored deadline wins":
				expired := now.Add(-time.Second)
				request.ApprovalExpiresAt = &expired
				wantAllowed = false
			}
			if err := checkCurrentApprovalTTL(request, cfg, now); (err == nil) != wantAllowed {
				t.Fatalf("automatic approval anchor: allowed=%t, want %t: %v", err == nil, wantAllowed, err)
			}
		})
	}
}
