package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/db"
)

// The mismatched command is harmless even if the gate regresses: printf, not
// a deletion or other destructive operation. A custom CRITICAL rule supplies
// the policy requirement that the misleading SAFE display must not bypass.
func commandSpecExecutionFixture(t *testing.T, spec db.CommandSpec) (*db.DB, *db.Request, *db.Session) {
	t.Helper()
	database, original, session := policyExecutionFixture(t)
	if _, err := database.InsertCustomPattern("critical", `^printf\s+command-spec-probe$`, "test invocation policy", "human"); err != nil {
		t.Fatal(err)
	}
	spec.Cwd = original.ProjectPath
	request := &db.Request{
		ProjectPath: original.ProjectPath, Status: db.StatusApproved, RiskTier: db.RiskTierDangerous,
		Command:            spec,
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
	database, request, session := commandSpecExecutionFixture(t, db.CommandSpec{Raw: "git stash", Argv: []string{"printf", "command-spec-probe"}})
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
			database, request, session := commandSpecExecutionFixture(t, db.CommandSpec{Raw: "git stash", Argv: []string{"printf", "command-spec-probe"}})
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

func TestCommandSpecExecutionRejectsUnboundWorkingDirectory(t *testing.T) {
	for _, cwd := range []string{"", ".", "relative"} {
		t.Run(cwd, func(t *testing.T) {
			database, request, session := commandSpecExecutionFixture(t, db.CommandSpec{
				Raw: "printf cwd-probe", Argv: []string{"printf", "cwd-probe"},
			})
			// Model an older, otherwise authentic approval, not hash tampering.
			request.Command.Cwd = cwd
			request.Command.Hash = db.ComputeCommandHash(request.Command)
			if _, err := database.Exec(`UPDATE requests SET command_cwd = ?, command_hash = ? WHERE id = ?`,
				cwd, request.Command.Hash, request.ID); err != nil {
				t.Fatal(err)
			}
			executor := NewExecutor(database, NewPatternEngine())
			if allowed, reason := executor.CanExecute(request.ID); allowed || reason == "" {
				t.Fatalf("unbound cwd passed advisory check: %v %q", allowed, reason)
			}
			logDir := filepath.Join(request.ProjectPath, "must-not-create")
			result, err := executor.ExecuteApprovedRequest(context.Background(), ExecuteOptions{
				RequestID: request.ID, SessionID: session.ID, LogDir: logDir, SuppressOutput: true,
			})
			if result != nil || !errors.Is(err, ErrUnboundCommandCWD) {
				t.Fatalf("unbound cwd reached preflight: %+v %v", result, err)
			}
			if _, err := os.Stat(logDir); !os.IsNotExist(err) {
				t.Fatalf("rejected context created a log directory: %v", err)
			}
			for _, authenticated := range []bool{false, true} {
				if authenticated {
					err = database.ClaimRequestExecutionAuthenticated(request, policyExecutionIdentity(session), session.SessionKey, ExecutionPolicyGuard(""))
				} else {
					err = database.ClaimRequestExecution(request, policyExecutionIdentity(session), ExecutionPolicyGuard(""))
				}
				if !errors.Is(err, ErrUnboundCommandCWD) {
					t.Fatalf("authenticated=%t accepted unbound cwd: %v", authenticated, err)
				}
				assertPolicyUnclaimed(t, database, request.ID)
			}
		})
	}
}

// A non-executed display is not a reason to reject an otherwise valid signed
// approval. Execute only this test binary with no tests selected; the display
// is subject to a custom CRITICAL rule, not a destructive command.
func TestCommandSpecExecutionIgnoresUnusedDisplay(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	database, request, session := commandSpecExecutionFixture(t, db.CommandSpec{
		Raw: "printf command-spec-probe", Argv: []string{executable, "-test.run=^$"},
	})
	engine, err := LoadCommandPolicy(database, config.LoadOptions{ProjectDir: request.ProjectPath})
	if err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(database, engine)
	if allowed, reason := executor.CanExecute(request.ID); !allowed {
		t.Fatalf("non-executed display blocked valid argv: %s", reason)
	}
	result, err := executor.ExecuteApprovedRequest(context.Background(), ExecuteOptions{
		RequestID: request.ID, SessionID: session.ID, LogDir: filepath.Join(request.ProjectPath, "logs"),
		SuppressOutput: true,
	})
	if err != nil || result == nil || result.ExitCode != 0 || result.Request.Status != db.StatusExecuted {
		t.Fatalf("valid executable did not complete: result=%+v error=%v", result, err)
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil || stored.Status != db.StatusExecuted || stored.Execution == nil {
		t.Fatalf("execution outcome not durable: request=%+v error=%v", stored, err)
	}
}

// A block applied after review must affect advisory checks, preflight and
// both final claim routes, including commands that are currently allowlisted.
// The probe is harmless printf; none of these tests invokes a deletion.
func TestCommandSpecExecutionHonorsBlockedRequestor(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		for _, allowlisted := range []bool{false, true} {
			t.Run(fmt.Sprintf("explicit=%t/allowlisted=%t", explicit, allowlisted), func(t *testing.T) {
				database, request, session := commandSpecExecutionFixture(t, db.CommandSpec{
					Raw: "printf blocked-request-probe", Argv: []string{"printf", "blocked-request-probe"},
				})
				policyText := ""
				if allowlisted {
					policyText = "[patterns.safe]\npatterns = ['^printf blocked-request-probe$']\n"
				}
				path := filepath.Join(request.ProjectPath, ".slb", "config.toml")
				configPath := ""
				if explicit {
					path = filepath.Join(t.TempDir(), "execution-policy.toml")
					configPath = path
				}
				writePolicy := func(text string) {
					t.Helper()
					if err := os.WriteFile(path, []byte(text), 0600); err != nil {
						t.Fatal(err)
					}
				}
				writePolicy(policyText)
				engine, err := LoadCommandPolicy(database, config.LoadOptions{ProjectDir: request.ProjectPath, ConfigPath: configPath})
				if err != nil {
					t.Fatal(err)
				}
				executor := NewExecutor(database, engine)
				if allowed, reason := executor.CanExecute(request.ID); !allowed {
					t.Fatalf("valid baseline approval rejected: %s", reason)
				}
				writePolicy(policyText + fmt.Sprintf("[agents]\nblocked = [%q]\n", strings.ToUpper(session.AgentName)))
				if err := CheckExecutionPolicy(database, request, configPath); !errors.Is(err, ErrAgentBlocked) {
					t.Fatalf("fresh policy ignored requester block: %v", err)
				}
				if allowed, reason := executor.CanExecute(request.ID); allowed || reason == "" {
					t.Fatalf("advisory check ignored revocation: %v %q", allowed, reason)
				}
				logDir := filepath.Join(request.ProjectPath, "must-not-create")
				result, err := executor.ExecuteApprovedRequest(context.Background(), ExecuteOptions{
					RequestID: request.ID, SessionID: session.ID, LogDir: logDir,
					CaptureRollback: true, SuppressOutput: true,
				})
				if result != nil || !errors.Is(err, ErrAgentBlocked) {
					t.Fatalf("blocked request reached execution: %+v %v", result, err)
				}
				if _, err := os.Stat(logDir); !os.IsNotExist(err) {
					t.Fatalf("blocked request touched preflight files: %v", err)
				}
				for _, authenticated := range []bool{false, true} {
					if authenticated {
						err = database.ClaimRequestExecutionAuthenticated(request, policyExecutionIdentity(session), session.SessionKey, ExecutionPolicyGuard(configPath))
					} else {
						err = database.ClaimRequestExecution(request, policyExecutionIdentity(session), ExecutionPolicyGuard(configPath))
					}
					if !errors.Is(err, ErrAgentBlocked) {
						t.Fatalf("authenticated=%t claim ignored block: %v", authenticated, err)
					}
					assertPolicyUnclaimed(t, database, request.ID)
				}
				if _, err := database.VerifyRequestApproval(request.ID); err != nil {
					t.Fatalf("policy revocation damaged signed review evidence: %v", err)
				}
				// Lifting this block does not mint or renew approval. Existing
				// signatures, expiry and quorum must still pass their own gates.
				writePolicy(policyText + "[agents]\nblocked = ['unrelated-agent']\n")
				if allowed, reason := executor.CanExecute(request.ID); !allowed {
					t.Fatalf("unrelated block rejected valid requester: %s", reason)
				}
				assertPolicyUnclaimed(t, database, request.ID)
			})
		}
	}
}
