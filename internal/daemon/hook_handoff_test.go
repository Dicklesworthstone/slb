package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func hookApprovalFixture(t *testing.T) (*db.DB, *db.Request, HookQueryParams) {
	t.Helper()
	root := t.TempDir()
	cwd := filepath.Join(root, "src", "nested")
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	conn, err := db.Open(filepath.Join(root, ".slb", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	session := &db.Session{AgentName: "HookAgent", Model: "test", Program: "test", ProjectPath: root}
	if err := conn.CreateSession(session); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Hour)
	req := &db.Request{
		ProjectPath: root, RequestorSessionID: session.ID, RequestorAgent: session.AgentName,
		RequestorModel: session.Model, Status: db.StatusApproved, RiskTier: db.RiskTierDangerous,
		Command:      db.CommandSpec{Raw: "git reset --hard", Argv: []string{"git", "reset", "--hard"}, Cwd: cwd},
		MinApprovals: 1, ApprovalExpiresAt: &expires, Justification: db.Justification{Reason: "Test hook handoff"},
	}
	if err := conn.CreateRequest(req); err != nil {
		t.Fatal(err)
	}
	return conn, req, HookQueryParams{Command: req.Command.Raw, SessionID: session.ID, CWD: cwd, ExecutionHandoff: true}
}

func TestHookApprovalHandoffRequiresAtomicExecutor(t *testing.T) {
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
	if result := srv.classifyCommand(params); result.Action != "ask" {
		t.Fatalf("invalid policy failed open: %+v", result)
	}
}
