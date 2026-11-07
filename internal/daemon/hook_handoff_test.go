package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
)

func hookApprovalFixture(t *testing.T) (*db.DB, *db.Request, HookQueryParams) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(root, "src", "nested")
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	return hookApprovalFixtureForPaths(t, root, root, cwd)
}

func hookApprovalFixtureForPaths(t *testing.T, databaseRoot, projectPath, cwd string) (*db.DB, *db.Request, HookQueryParams) {
	t.Helper()
	conn, err := db.Open(filepath.Join(databaseRoot, ".slb", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	req, params := hookApprovalFixtureInDB(t, conn, projectPath, cwd)
	return conn, req, params
}

func hookApprovalFixtureInDB(t *testing.T, conn *db.DB, projectPath, cwd string) (*db.Request, HookQueryParams) {
	t.Helper()
	session := &db.Session{AgentName: "HookAgent", Model: "test", Program: "test", ProjectPath: projectPath}
	if err := conn.CreateSession(session); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Hour)
	req := &db.Request{
		ProjectPath: projectPath, RequestorSessionID: session.ID, RequestorAgent: session.AgentName,
		RequestorModel: session.Model, Status: db.StatusApproved, RiskTier: db.RiskTierDangerous,
		Command:      db.CommandSpec{Raw: "git reset --hard", Argv: []string{"git", "reset", "--hard"}, Cwd: cwd},
		MinApprovals: 1, ApprovalExpiresAt: &expires, Justification: db.Justification{Reason: "Test hook handoff"},
	}
	if err := conn.CreateRequest(req); err != nil {
		t.Fatal(err)
	}
	// The status bit is not an approval: handoff eligibility re-verifies
	// signed reviewer evidence (f4fb378), so record one authentic approval.
	reviewer := &db.Session{AgentName: "HookReviewer", Model: "reviewer-model", Program: "test", ProjectPath: projectPath}
	if err := conn.CreateSession(reviewer); err != nil {
		t.Fatal(err)
	}
	signedAt := time.Now().UTC()
	if err := conn.CreateReview(&db.Review{RequestID: req.ID, ReviewerSessionID: reviewer.ID, ReviewerAgent: reviewer.AgentName,
		ReviewerModel: reviewer.Model, Decision: db.DecisionApprove, SignatureTimestamp: signedAt,
		Signature: db.ComputeReviewSignature(reviewer.SessionKey, req.ID, db.DecisionApprove, signedAt)}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec("UPDATE requests SET resolved_at = ? WHERE id = ?", signedAt.Format(time.RFC3339), req.ID); err != nil {
		t.Fatal(err)
	}
	req.ResolvedAt = &signedAt
	return req, HookQueryParams{Command: req.Command.Raw, SessionID: session.ID, CWD: cwd, ExecutionHandoff: true}
}

func hookApprovalCandidateFixture(t *testing.T, conn *db.DB, original *db.Request, createdAt, resolvedAt time.Time) *db.Request {
	t.Helper()
	candidate := *original
	candidate.ID = ""
	if err := conn.CreateRequest(&candidate); err != nil {
		t.Fatal(err)
	}
	reviewer := &db.Session{AgentName: "CandidateReviewer-" + candidate.ID, Model: "reviewer-model", Program: "test", ProjectPath: candidate.ProjectPath}
	if err := conn.CreateSession(reviewer); err != nil {
		t.Fatal(err)
	}
	if err := conn.CreateReview(&db.Review{RequestID: candidate.ID, ReviewerSessionID: reviewer.ID, ReviewerAgent: reviewer.AgentName,
		ReviewerModel: reviewer.Model, Decision: db.DecisionApprove, SignatureTimestamp: resolvedAt,
		Signature: db.ComputeReviewSignature(reviewer.SessionKey, candidate.ID, db.DecisionApprove, resolvedAt)}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec("UPDATE requests SET created_at = ?, resolved_at = ? WHERE id = ?",
		createdAt.Format(time.RFC3339), resolvedAt.Format(time.RFC3339), candidate.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := conn.GetRequest(candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func TestHookApprovalHandoffRequiresAtomicExecutor(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	conn, req, params := hookApprovalFixture(t)
	srv := &IPCServer{}
	for range 2 {
		result := srv.classifyCommand(params)
		if result.Action != "execute" || result.ExecutionHandoff == nil {
			t.Fatalf("approved nested-directory request not handed off: %+v", result)
		}
		handoff := result.ExecutionHandoff
		if handoff.RequestID != req.ID || handoff.CommandHash != req.Command.Hash || handoff.SessionID != params.SessionID ||
			handoff.DatabasePath != filepath.Join(req.ProjectPath, ".slb", "state.db") {
			t.Fatalf("handoff is not bound to the request: %+v", handoff)
		}
	}
	stored, err := conn.GetRequest(req.ID)
	if err != nil || stored.Status != db.StatusApproved {
		t.Fatalf("lookup consumed approval: %+v, %v", stored, err)
	}
	params.ExecutionHandoff = false
	if result := srv.classifyCommand(params); result.Action == "allow" || result.ExecutionHandoff != nil {
		t.Fatalf("legacy client received a raw-shell permit: %+v", result)
	}
	now := time.Now().UTC()
	execution := &db.Execution{ExecutedAt: &now, ExecutedBySessionID: params.SessionID,
		ExecutedByAgent: req.RequestorAgent, ExecutedByModel: req.RequestorModel, LogPath: "unique-execution-log"}
	if err := conn.ClaimRequestExecution(stored, execution); err != nil {
		t.Fatal(err)
	}
	if err := conn.ClaimRequestExecution(stored, execution); !errors.Is(err, db.ErrInvalidTransition) {
		t.Fatalf("replayed handoff obtained a second claim: %v", err)
	}
	params.ExecutionHandoff = true
	if result := srv.classifyCommand(params); result.Action != "block" || result.ExecutionHandoff != nil {
		t.Fatalf("executing approval remained reusable: %+v", result)
	}
}

func TestHookApprovalCandidatesSkipIneligibleNewest(t *testing.T) {
	for _, invalid := range []string{"current TTL", "invalid signature"} {
		t.Run(invalid, func(t *testing.T) {
			db.SimulateIndependentIdentityForTest(t)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("USERPROFILE", t.TempDir())
			conn, older, params := hookApprovalFixture(t)
			now := time.Now().UTC()
			if _, err := conn.Exec("UPDATE requests SET created_at = ? WHERE id = ?", now.Add(-time.Hour).Format(time.RFC3339), older.ID); err != nil {
				t.Fatal(err)
			}
			newer := hookApprovalCandidateFixture(t, conn, older, now.Add(-30*time.Minute), now.Add(-20*time.Minute))
			selected := filepath.Join(t.TempDir(), "selected-candidate-policy.toml")
			writePolicy := func(ttl int) {
				t.Helper()
				text := fmt.Sprintf("[general]\napproval_ttl_minutes = %d\n[patterns.dangerous]\nmin_approvals = 1\n", ttl)
				if err := os.WriteFile(selected, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			writePolicy(30)
			server := &IPCServer{hookPolicies: hookPolicyCache{projectPath: older.ProjectPath, configPath: selected}}
			params.ConfigPath = selected
			if result := server.classifyCommand(params); result.Action != "execute" || result.RequestID != newer.ID {
				t.Fatalf("newest eligible approval lost preference: %+v", result)
			}
			if invalid == "current TTL" {
				// Both persisted expiry values are still in the future. The
				// selected policy now expires B, which resolved before older A.
				writePolicy(10)
			} else if _, err := conn.Exec("UPDATE reviews SET signature = 'invalid' WHERE request_id = ?", newer.ID); err != nil {
				t.Fatal(err)
			}
			policy, err := server.hookPolicies.policyFor(params.CWD)
			if err != nil {
				t.Fatal(err)
			}
			executor := core.NewExecutor(conn, policy.engine)
			if ok, reason := executor.CanExecute(older.ID); !ok {
				t.Fatalf("older approval is not executable: %s", reason)
			}
			if ok, reason := executor.CanExecute(newer.ID); ok ||
				(invalid == "current TTL" && !strings.Contains(reason, "current TTL")) ||
				(invalid == "invalid signature" && !strings.Contains(reason, "invalid signature")) {
				t.Fatalf("newer approval was not rejected for %s: allowed=%t reason=%q", invalid, ok, reason)
			}
			for range 2 {
				result := server.classifyCommand(params)
				if result.Action != "execute" || result.RequestID != older.ID || result.ExecutionHandoff == nil ||
					result.ExecutionHandoff.CommandHash != older.Command.Hash || result.ExecutionHandoff.ConfigPath != selected {
					t.Fatalf("unusable newer approval hid an eligible older grant: %+v", result)
				}
			}
			legacy := params
			legacy.ExecutionHandoff = false
			if result := server.classifyCommand(legacy); result.Action != "block" || result.RequestID != older.ID || result.ExecutionHandoff != nil {
				t.Fatalf("candidate fallback gave a legacy client a raw-shell permit: %+v", result)
			}
			for _, id := range []string{older.ID, newer.ID} {
				stored, err := conn.GetRequest(id)
				if err != nil || stored.Status != db.StatusApproved || stored.Execution != nil {
					t.Fatalf("candidate lookup consumed approval %s: %+v %v", id, stored, err)
				}
			}
			for _, field := range []string{"command", "cwd", "session"} {
				mismatched := params
				switch field {
				case "command":
					mismatched.Command += " HEAD~1"
				case "cwd":
					mismatched.CWD = older.ProjectPath
				case "session":
					mismatched.SessionID = "another-requester"
				}
				if result := server.classifyCommand(mismatched); result.Action != "block" || result.RequestID != "" || result.ExecutionHandoff != nil {
					t.Fatalf("older candidate relaxed exact %s scope: %+v", field, result)
				}
			}
			if got := findHookApproval(context.Background(), conn, policy.engine, params, t.TempDir()); got != nil {
				t.Fatal("older candidate crossed the selected physical project")
			}
			if _, err := conn.Exec("UPDATE reviews SET signature = 'invalid' WHERE request_id = ?", older.ID); err != nil {
				t.Fatal(err)
			}
			if result := server.classifyCommand(params); result.Action != "block" || result.RequestID != "" || result.ExecutionHandoff != nil {
				t.Fatalf("all-invalid candidates authorized execution: %+v", result)
			}
		})
	}
}

func TestHookApprovalCandidatesCrossPagesOnSingleConnection(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	project := hookPolicyProject(t)
	// Each SQLite :memory: connection has its own database. A nested read while
	// a page holds the populated connection would see an empty second database.
	conn, err := db.OpenAndMigrate(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	older, params := hookApprovalFixtureInDB(t, conn, project, project)
	now := time.Now().UTC()
	if _, err := conn.Exec("UPDATE requests SET created_at = ? WHERE id = ?", now.Add(-2*time.Hour).Format(time.RFC3339), older.ID); err != nil {
		t.Fatal(err)
	}
	createdAt := now.Add(-time.Hour)
	// A time.Time round trip would replace +00:00 with Z or trim precision,
	// changing this boundary's lexical position and repeating/skipping rows.
	storedTimestamp := createdAt.Format("2006-01-02T15:04:05.000000000-07:00")
	var invalidIDs []string
	for range hookApprovalPageSize + 1 {
		candidate := hookApprovalCandidateFixture(t, conn, older, createdAt, now.Add(-time.Minute))
		if _, err := conn.Exec("UPDATE requests SET created_at = ? WHERE id = ?", storedTimestamp, candidate.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec("UPDATE reviews SET signature = 'invalid' WHERE request_id = ?", candidate.ID); err != nil {
			t.Fatal(err)
		}
		invalidIDs = append(invalidIDs, candidate.ID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if got := findHookApproval(ctx, conn, core.NewPatternEngine(), params, project); got == nil || got.ID != older.ID {
		t.Fatalf("page boundary hid the sole eligible approval: %+v (context: %v)", got, ctx.Err())
	}

	// Another executor may claim an already scanned row before the next page.
	// OFFSET would then skip an unseen row; the stored ordering key must not.
	newest := hookApprovalCandidateFixture(t, conn, older, now.Add(-30*time.Minute), now.Add(-time.Minute))
	first, err := readHookApprovalCandidates(conn, params, nil)
	if err != nil || len(first) != hookApprovalPageSize || first[0].id != newest.ID {
		t.Fatalf("unexpected first candidate page: %+v %v", first, err)
	}
	last := first[len(first)-1]
	if last.createdAt != storedTimestamp {
		t.Fatalf("candidate cursor reformatted the stored timestamp: %q", last.createdAt)
	}
	execution := &db.Execution{ExecutedAt: &now, ExecutedBySessionID: params.SessionID,
		ExecutedByAgent: newest.RequestorAgent, ExecutedByModel: newest.RequestorModel, LogPath: "candidate-page-claim"}
	if err := conn.ClaimRequestExecution(newest, execution); err != nil {
		t.Fatal(err)
	}
	next, err := readHookApprovalCandidates(conn, params, &last)
	sort.Sort(sort.Reverse(sort.StringSlice(invalidIDs)))
	if err != nil || len(next) != 3 || next[0].id != invalidIDs[hookApprovalPageSize-1] ||
		next[1].id != invalidIDs[hookApprovalPageSize] || next[2].id != older.ID {
		t.Fatalf("concurrent claim skipped unseen candidates: %+v %v", next, err)
	}
	stored, err := conn.GetRequest(older.ID)
	if err != nil || stored.Status != db.StatusApproved || stored.Execution != nil {
		t.Fatalf("page traversal consumed the older approval: %+v %v", stored, err)
	}
}

func TestHookApprovalCandidateLookupFailsClosed(t *testing.T) {
	for _, failure := range []string{"page query", "request read", "cancelled server"} {
		t.Run(failure, func(t *testing.T) {
			db.SimulateIndependentIdentityForTest(t)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("USERPROFILE", t.TempDir())
			conn, older, params := hookApprovalFixture(t)
			now := time.Now().UTC()
			if _, err := conn.Exec("UPDATE requests SET created_at = ? WHERE id = ?", now.Add(-time.Hour).Format(time.RFC3339), older.ID); err != nil {
				t.Fatal(err)
			}
			newer := hookApprovalCandidateFixture(t, conn, older, now.Add(-30*time.Minute), now.Add(-time.Minute))
			engine := core.NewPatternEngine()
			if ok, reason := core.NewExecutor(conn, engine).CanExecute(older.ID); !ok {
				t.Fatalf("fixture lost its eligible older approval: %s", reason)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch failure {
			case "page query":
				if err := conn.Close(); err != nil {
					t.Fatal(err)
				}
			case "request read":
				if _, err := conn.Exec("UPDATE requests SET min_approvals = 'unreadable' WHERE id = ?", newer.ID); err != nil {
					t.Fatal(err)
				}
			case "cancelled server":
				cancel()
				if result := (&IPCServer{ctx: ctx}).classifyCommand(params); result.Action != "block" || result.ExecutionHandoff != nil {
					t.Fatalf("cancelled server handed off an approval: %+v", result)
				}
			}
			if got := findHookApproval(ctx, conn, engine, params, older.ProjectPath); got != nil {
				t.Fatalf("%s still selected an approval: %+v", failure, got)
			}
		})
	}
}

func TestHookHandoffPreservesSelectedPolicy(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	conn, req, params := hookApprovalFixture(t)
	selected := filepath.Join(t.TempDir(), "selected hook policy.toml")
	if err := os.WriteFile(selected, []byte("[patterns.dangerous]\nmin_approvals = 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	server := &IPCServer{hookPolicies: hookPolicyCache{projectPath: req.ProjectPath, configPath: selected}}
	params.ConfigPath = selected
	result := server.classifyCommand(params)
	if result.Action != "execute" || result.ExecutionHandoff == nil {
		t.Fatalf("selected-policy approval not handed off: %+v", result)
	}
	if result.ConfigPath != selected || result.ConfigProject != req.ProjectPath ||
		result.ExecutionHandoff.ConfigPath != selected || result.ExecutionHandoff.ConfigProject != req.ProjectPath {
		t.Fatalf("handoff dropped selected policy: %+v, %+v", result, result.ExecutionHandoff)
	}
	stored, err := conn.GetRequest(req.ID)
	if err != nil || stored.Status != db.StatusApproved {
		t.Fatalf("handoff consumed the approval: %+v %v", stored, err)
	}
	if err := os.WriteFile(selected, []byte("[patterns.dangerous]\nmin_approvals = 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if result := server.classifyCommand(params); result.Action != "block" || result.ExecutionHandoff != nil {
		t.Fatalf("handoff reused policy after a quorum increase: %+v", result)
	}
}

func TestHookSelectedConfigAssertionCannotChangeDaemonPolicy(t *testing.T) {
	_, req, params := hookApprovalFixture(t)
	selected := filepath.Join(t.TempDir(), "selected.toml")
	other := filepath.Join(t.TempDir(), "different.toml")
	for _, path := range []string{selected, other} {
		if err := os.WriteFile(path, []byte("[general]\napproval_policy = 'independent'\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	server := &IPCServer{hookPolicies: hookPolicyCache{projectPath: req.ProjectPath, configPath: selected}}
	params.Command = "git stash" // Normally SAFE: a mismatch must block even this.
	for _, path := range []string{other, "relative.toml", filepath.Join(t.TempDir(), "missing.toml")} {
		params.ConfigPath = path
		result := server.classifyCommand(params)
		if result.Action != "block" || result.MatchedPattern != "policy_config_mismatch" || result.ExecutionHandoff != nil {
			t.Fatalf("query substituted %q for daemon policy: %+v", path, result)
		}
	}
	params.ConfigPath = selected
	if result := server.classifyCommand(params); result.Action != "allow" || result.ConfigPath != selected || result.ConfigProject != req.ProjectPath {
		t.Fatalf("matching policy assertion failed: %+v", result)
	}
	alias := filepath.Join(t.TempDir(), "policy alias.toml")
	if err := os.Symlink(selected, alias); err == nil {
		params.ConfigPath = alias
		if result := server.classifyCommand(params); result.Action != "allow" {
			t.Fatalf("alias of selected file was rejected: %+v", result)
		}
	}
	// A project daemon without an override must not read a supplied file.
	params.ConfigPath = selected
	if result := (&IPCServer{}).classifyCommand(params); result.Action != "block" || result.MatchedPattern != "policy_config_mismatch" {
		t.Fatalf("default daemon accepted caller-supplied policy: %+v", result)
	}
}

func TestHookApprovalHandoffRejectsIneligibleRequests(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"executed", "UPDATE requests SET status='executed'"},
		{"executing", "UPDATE requests SET status='executing'"},
		{"pending", "UPDATE requests SET status='pending'"},
		{"rejected", "UPDATE requests SET status='rejected'"},
		{"cancelled", "UPDATE requests SET status='cancelled'"},
		{"expired", "UPDATE requests SET approval_expires_at='2000-01-01T00:00:00Z'"},
		{"invalid expiry", "UPDATE requests SET approval_expires_at='invalid'"},
		{"changed hash", "UPDATE requests SET command_hash='changed'"},
		{"insufficient quorum", "UPDATE requests SET min_approvals=0"},
		{"policy escalated", "UPDATE requests SET risk_tier='caution'"},
		{"ended session", "UPDATE sessions SET ended_at='2000-01-01T00:00:00Z'"},
		{"foreign project", "UPDATE requests SET project_path='/unrelated-project'"},
		{"display match only", "UPDATE requests SET command_display_redacted=command_raw, command_raw='echo different'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, _, params := hookApprovalFixture(t)
			if _, err := conn.Exec(tc.sql); err != nil {
				t.Fatal(err)
			}
			result := (&IPCServer{}).classifyCommand(params)
			if result.Action != "block" || result.ExecutionHandoff != nil {
				t.Fatalf("ineligible approval was handed off: %+v", result)
			}
		})
	}
	for _, field := range []string{"command", "cwd", "session"} {
		t.Run("different "+field, func(t *testing.T) {
			_, req, params := hookApprovalFixture(t)
			switch field {
			case "command":
				params.Command += " HEAD~1"
			case "cwd":
				params.CWD = req.ProjectPath
			case "session":
				params.SessionID = "unrelated-session"
			}
			result := (&IPCServer{}).classifyCommand(params)
			if result.Action != "block" || result.ExecutionHandoff != nil {
				t.Fatalf("mismatched approval was handed off: %+v", result)
			}
		})
	}
}

func TestHookPolicyReloadsWithoutLeakingAcrossProjects(t *testing.T) {
	conn, _, params := hookApprovalFixture(t)
	_, _, other := hookApprovalFixture(t)
	srv := &IPCServer{}
	params.Command = "example-cleanup"
	if result := srv.classifyCommand(params); result.Action != "allow" {
		t.Fatalf("unexpected baseline: %+v", result)
	}
	if _, err := conn.InsertCustomPattern("dangerous", "^example-cleanup$", "project policy", "human"); err != nil {
		t.Fatal(err)
	}
	if result := srv.classifyCommand(params); result.Action != "block" {
		t.Fatalf("live custom policy not applied: %+v", result)
	}
	other.Command = params.Command
	if result := srv.classifyCommand(other); result.Action != "allow" {
		t.Fatalf("custom policy leaked between projects: %+v", result)
	}
	if _, err := conn.InsertCustomPattern("dangerous", "[invalid", "invalid rule", "human"); err != nil {
		t.Fatal(err)
	}
	// Since b084d38 an unloadable live policy is an infrastructure failure
	// and a hard denial, not an interactive fallback.
	if result := srv.classifyCommand(params); result.Action != "block" || result.MatchedPattern != "policy_load_error" || result.ExecutionHandoff != nil {
		t.Fatalf("invalid policy failed open: %+v", result)
	}
}

func TestHookApprovalRootAliasPreservesLiteralBinding(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "src", "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "project-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	conn, req, params := hookApprovalFixtureForPaths(t, root, alias, filepath.Join(alias, "src", "nested"))
	srv := &IPCServer{}
	result := srv.classifyCommand(params)
	if result.Action != "execute" || result.ExecutionHandoff == nil {
		t.Fatalf("existing alias-bound approval was lost: %+v", result)
	}
	handoff := result.ExecutionHandoff
	if handoff.DatabasePath != filepath.Join(root, ".slb", "state.db") || handoff.CommandHash != req.Command.Hash ||
		handoff.RequestID != req.ID || handoff.SessionID != params.SessionID {
		t.Fatalf("handoff changed the approval binding or selected a lexical database: %+v", handoff)
	}
	stored, err := conn.GetRequest(req.ID)
	if err != nil || stored.ProjectPath != alias || stored.Command.Cwd != params.CWD || stored.Command.Hash != req.Command.Hash ||
		stored.Status != db.StatusApproved {
		t.Fatalf("physical policy selection changed the stored approval: %+v, %v", stored, err)
	}
	params.CWD = filepath.Join(root, "src", "nested")
	if result := srv.classifyCommand(params); result.Action != "block" || result.ExecutionHandoff != nil {
		t.Fatalf("physical equivalence relaxed the exact command cwd binding: %+v", result)
	}
}

func TestHookApprovalRejectsLexicalProjectAcrossDescendantAlias(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	project := hookPolicyProject(t)
	other := hookPolicyProject(t)
	nested := filepath.Join(project, "src")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(other, "project-alias")
	if err := os.Symlink(nested, alias); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	conn, req, params := hookApprovalFixtureForPaths(t, project, other, alias)
	if ProjectRootForCWD(alias) != other {
		t.Fatal("fixture does not exercise a different lexical project")
	}
	if ok, reason := core.NewExecutor(conn, core.NewPatternEngine()).CanExecute(req.ID); !ok {
		t.Fatalf("fixture approval must otherwise be executable: %s", reason)
	}
	result := (&IPCServer{}).classifyCommand(params)
	if result.Action != "block" || result.ExecutionHandoff != nil || result.RequestID != "" {
		t.Fatalf("project B approval was handed off using project A's state: %+v", result)
	}
}

func TestHookApprovalDescendantAliasUsesStoredProjectBinding(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	project := hookPolicyProject(t)
	other := hookPolicyProject(t)
	nested := filepath.Join(project, "src")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(other, "project-alias")
	if err := os.Symlink(nested, alias); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	conn, req, params := hookApprovalFixtureForPaths(t, project, project, alias)
	result := (&IPCServer{}).classifyCommand(params)
	if result.Action != "execute" || result.ExecutionHandoff == nil || result.RequestID != req.ID {
		t.Fatalf("physical project approval was lost through a descendant alias: %+v", result)
	}
	if result.ExecutionHandoff.CommandHash != req.Command.Hash || result.ExecutionHandoff.DatabasePath != filepath.Join(project, ".slb", "state.db") {
		t.Fatalf("handoff changed the stored command or database binding: %+v", result.ExecutionHandoff)
	}
	stored, err := conn.GetRequest(req.ID)
	if err != nil || stored.ProjectPath != project || stored.Command.Cwd != alias || stored.Command.Hash != req.Command.Hash {
		t.Fatalf("discovery rewrote stored approval evidence: %+v, %v", stored, err)
	}
	params.CWD = nested
	if result := (&IPCServer{}).classifyCommand(params); result.Action != "block" || result.ExecutionHandoff != nil {
		t.Fatalf("physical equivalence waived the exact cwd binding: %+v", result)
	}
}
