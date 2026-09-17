package core

import (
	"fmt"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestRequestPolicyRecordsConfiguredRequirements(t *testing.T) {
	for _, differentModel := range []bool{false, true} {
		t.Run(fmt.Sprintf("different-model=%t", differentModel), func(t *testing.T) {
			database, existing, session := policyExecutionFixture(t)
			writeExecutionPolicy(t, existing.ProjectPath, fmt.Sprintf(`[general]
require_different_model = %t
[patterns.dangerous]
min_approvals = 3
[patterns.critical]
min_approvals = 5
`, differentModel))
			engine, err := LoadCommandPolicy(database, config.LoadOptions{ProjectDir: existing.ProjectPath})
			if err != nil {
				t.Fatal(err)
			}
			options := DefaultRequestCreatorConfig()
			options.AgentMailEnabled = false
			creator := NewRequestCreator(database, nil, engine, options)
			cautionApprovals := 0
			if differentModel {
				cautionApprovals = 1
			}
			for _, tc := range []struct {
				command string
				tier    db.RiskTier
				minimum int
			}{
				{"git reset --hard HEAD", db.RiskTierDangerous, 3},
				{"rm -rf /etc/policy-test", db.RiskTierCritical, 5},
				{"git branch -d obsolete", db.RiskTierCaution, cautionApprovals},
				{"opaque-policy-wrapper", db.RiskTierDangerous, 3},
			} {
				result, err := creator.CreateRequest(CreateRequestOptions{
					SessionID: session.ID, Command: tc.command, Cwd: existing.ProjectPath,
					Justification: Justification{Reason: "test configured request policy"},
				})
				if err != nil || result == nil || result.Skipped || result.Request == nil {
					t.Fatalf("%s: request creation failed: %+v %v", tc.command, result, err)
				}
				stored, err := database.GetRequest(result.Request.ID)
				if err != nil {
					t.Fatal(err)
				}
				if stored.RiskTier != tc.tier || stored.MinApprovals != tc.minimum || stored.RequireDifferentModel != differentModel {
					t.Fatalf("%s: wrong stored requirements: tier=%s minimum=%d model=%t", tc.command,
						stored.RiskTier, stored.MinApprovals, stored.RequireDifferentModel)
				}
				if err := CheckExecutionPolicy(database, stored, ""); err != nil {
					t.Fatalf("request policy disagrees with execution policy: %v", err)
				}
			}
		})
	}
}

func TestRequestPolicyDynamicQuorumUsesResolvedProject(t *testing.T) {
	database, existing, session := policyExecutionFixture(t)
	writeExecutionPolicy(t, existing.ProjectPath, `[patterns.critical]
min_approvals = 4
dynamic_quorum = true
dynamic_quorum_floor = 1
`)
	for i := 0; i < 5; i++ {
		other := &db.Session{AgentName: fmt.Sprintf("unrelated-%d", i), Model: "model-b", Program: "test", ProjectPath: t.TempDir()}
		if err := database.CreateSession(other); err != nil {
			t.Fatal(err)
		}
	}
	engine, err := LoadCommandPolicy(database, config.LoadOptions{ProjectDir: existing.ProjectPath})
	if err != nil {
		t.Fatal(err)
	}
	options := DefaultRequestCreatorConfig()
	options.AgentMailEnabled = false
	creator := NewRequestCreator(database, nil, engine, options)
	for _, minimum := range []int{1, 2} {
		if minimum == 2 {
			joined := &db.Session{AgentName: "joined-local-reviewer", Model: "model-b", Program: "test", ProjectPath: existing.ProjectPath}
			if err := database.CreateSession(joined); err != nil {
				t.Fatal(err)
			}
		}
		// ProjectPath is intentionally omitted: scope must come from session,
		// never all projects in the database or the caller's working directory.
		result, err := creator.CreateRequest(CreateRequestOptions{SessionID: session.ID,
			Command: "rm -rf /etc/policy-test", Cwd: existing.ProjectPath})
		if err != nil || result == nil || result.Request == nil {
			t.Fatalf("dynamic request failed: %+v %v", result, err)
		}
		if result.Request.ProjectPath != existing.ProjectPath || result.Request.MinApprovals != minimum {
			t.Fatalf("wrong project-local quorum: %+v", result.Request)
		}
		if err := CheckExecutionPolicy(database, result.Request, ""); err != nil {
			t.Fatalf("creation/execution quorum disagreement: %v", err)
		}
	}
}
