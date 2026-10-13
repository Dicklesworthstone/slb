package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestSafeExecutionReloadsPolicyAndSession(t *testing.T) {
	for _, change := range []string{"unchanged", "removed-safe", "malformed-config", "custom-rule", "ended-session", "blocked-agent", "wrong-project", "missing-session", "missing-config", "cancelled", "unmatched-segment", "unbound-cwd"} {
		t.Run(change, func(t *testing.T) {
			database, request, session := policyExecutionFixture(t)
			project := request.ProjectPath
			const safePolicy = "[patterns.safe]\npatterns = ['^printf safe-run-probe$']\n"
			writeExecutionPolicy(t, project, safePolicy)
			spec := CommandSpec{Raw: "printf safe-run-probe", Argv: []string{"printf", "safe-run-probe"}, Cwd: project}
			old, err := LoadCommandPolicy(database, config.LoadOptions{ProjectDir: project})
			if err != nil || !old.ClassifyCommandSpec(spec).IsSafe {
				t.Fatalf("baseline must be SAFE: %v", err)
			}
			ctx := context.Background()
			sessionID, configPath := session.ID, ""
			var want error
			switch change {
			case "removed-safe":
				writeExecutionPolicy(t, project, "[patterns.safe]\npatterns = []\n")
				want = ErrApprovalPolicyChanged
			case "malformed-config":
				writeExecutionPolicy(t, project, "[broken")
			case "custom-rule":
				// The original classification remains cached in old. The new
				// engine must consult both current TOML and persisted rules.
				writeExecutionPolicy(t, project, "[patterns.safe]\npatterns = []\n")
				if _, err := database.InsertCustomPattern("critical", `^printf safe-run-probe$`, "new restriction", "human"); err != nil {
					t.Fatal(err)
				}
				want = ErrApprovalPolicyChanged
			case "ended-session":
				if err := database.EndSession(session.ID); err != nil {
					t.Fatal(err)
				}
				want = ErrSessionInactive
			case "blocked-agent":
				writeExecutionPolicy(t, project, safePolicy+"[agents]\nblocked = ['"+strings.ToUpper(session.AgentName)+"']\n")
				want = ErrAgentBlocked
			case "wrong-project":
				project = t.TempDir()
				want = db.ErrExecutionAuthentication
			case "missing-session":
				sessionID = "not-a-session"
				want = ErrSessionNotFound
			case "missing-config":
				configPath = filepath.Join(t.TempDir(), "missing.toml")
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			case "unmatched-segment":
				spec.Raw, spec.Shell = "printf safe-run-probe && unknown-command", true
				want = ErrApprovalPolicyChanged
			case "unbound-cwd":
				spec.Cwd = ""
				want = ErrUnboundCommandCWD
			}
			err = CheckSafeExecution(ctx, database, spec, project, sessionID, configPath)
			if change == "unchanged" {
				if err != nil {
					t.Fatalf("valid safe execution rejected: %v", err)
				}
			} else if err == nil || (want != nil && !errors.Is(err, want)) {
				t.Fatalf("%s accepted or wrong error: %v, want %v", change, err, want)
			}
			if _, err := os.Stat(filepath.Join(request.ProjectPath, ".slb", "logs")); !os.IsNotExist(err) {
				t.Fatalf("authorization check produced execution logs: %v", err)
			}
		})
	}
}

func TestSafeExecutionUsesSelectedInvocationAndExplicitConfig(t *testing.T) {
	database, request, session := policyExecutionFixture(t)
	path := filepath.Join(t.TempDir(), "explicit.toml")
	if err := os.WriteFile(path, []byte("[patterns.safe]\npatterns = ['^printf safe-run-probe$']\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		spec CommandSpec
		ok   bool
	}{
		{CommandSpec{Raw: "git stash", Argv: []string{"printf", "safe-run-probe"}}, true},
		{CommandSpec{Raw: "printf safe-run-probe", Argv: []string{"printf", "unknown"}}, false},
		{CommandSpec{Raw: "printf safe-run-probe", Argv: []string{"printf", "unknown"}, Shell: true}, true},
	} {
		tc.spec.Cwd = request.ProjectPath
		err := CheckSafeExecution(context.Background(), database, tc.spec, request.ProjectPath, session.ID, path)
		if (err == nil) != tc.ok {
			t.Errorf("spec=%+v: %v, want allowed=%t", tc.spec, err, tc.ok)
		}
	}
	if err := CheckSafeExecution(context.Background(), nil, CommandSpec{}, request.ProjectPath, session.ID, path); err == nil {
		t.Fatal("safe execution accepted missing database")
	}
}
