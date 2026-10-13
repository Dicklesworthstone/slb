package core

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestCommandSpecPendingPolicyUsesExecutableInput(t *testing.T) {
	for _, expired := range []bool{false, true} {
		for _, tc := range []struct {
			name    string
			spec    db.CommandSpec
			approve bool
		}{
			{"critical argv behind safe display", db.CommandSpec{Raw: "git stash", Argv: []string{"printf", "command-spec-probe"}}, false},
			{"caution argv behind critical display", db.CommandSpec{Raw: "printf command-spec-probe", Argv: []string{"git", "branch", "-d", "obsolete"}}, true},
			{"shell uses raw not unused argv", db.CommandSpec{Raw: "git branch -d obsolete", Argv: []string{"printf", "command-spec-probe"}, Shell: true}, true},
			{"shell cannot borrow safe argv", db.CommandSpec{Raw: "printf command-spec-probe", Argv: []string{"git", "stash"}, Shell: true}, false},
		} {
			t.Run(fmt.Sprintf("expired=%t/%s", expired, tc.name), func(t *testing.T) {
				database, original := pendingFixture(t)
				if _, err := database.InsertCustomPattern("critical", `^printf\s+command-spec-probe$`, "test invocation policy", "human"); err != nil {
					t.Fatal(err)
				}
				request := &db.Request{
					ProjectPath: original.ProjectPath, Command: tc.spec,
					RiskTier: db.RiskTierCaution, Status: db.StatusPending,
					RequestorSessionID: original.RequestorSessionID, RequestorAgent: original.RequestorAgent,
					RequestorModel: original.RequestorModel, Justification: original.Justification,
				}
				request.Command.Cwd = original.ProjectPath
				if err := database.CreateRequest(request); err != nil {
					t.Fatal(err)
				}
				pendingSQL(t, database, `UPDATE requests SET created_at = ? WHERE id = ?`, time.Now().UTC().Add(-2*time.Minute).Format(time.RFC3339), request.ID)
				if expired {
					pendingSQL(t, database, `UPDATE requests SET expires_at = ? WHERE id = ?`, time.Now().UTC().Add(-time.Second).Format(time.RFC3339), request.ID)
				}
				result, err := AdvancePendingRequest(context.Background(), database, request.ID, PendingOptions{TimeoutAction: "auto_approve_warn"})
				want := db.StatusPending
				if tc.approve {
					want = db.StatusApproved
				} else if expired {
					want = db.StatusEscalated
				}
				if err != nil || result == nil || result.Request.Status != want {
					t.Fatalf("timer trusted wrong command representation: %+v %v; want %s", result, err, want)
				}
				if !tc.approve && result.Request.ApprovalExpiresAt != nil {
					t.Fatal("ineligible invocation acquired an approval deadline")
				}
			})
		}
	}
}
