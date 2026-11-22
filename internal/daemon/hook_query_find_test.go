package daemon

import (
	"testing"

	"github.com/Dicklesworthstone/slb/internal/testutil"
)

func TestHookQueryFindActionsRiskAndQuorum(t *testing.T) {
	server := &IPCServer{}
	project := testutil.NonSystemTempDir(t)
	for _, tc := range []struct {
		command, tier string
		approvals     int
	}{
		{`find / -delete`, "critical", 2},
		{`find . -delete`, "dangerous", 1},
		{`find /srv -type f -exec rm -f '{}' +`, "caution", 0},
		{`find . -exec sh -c 'git push --force origin main' \;`, "critical", 2},
		{`find / -exec rm -rf '{}' +`, "critical", 2},
		{`find /tmp/project -execdir rm -rf ../.. \;`, "critical", 2},
		{`find /etc/"$TARGET" -delete`, "critical", 2},
		{`find . -exec kubectl delete pod '{}' +`, "dangerous", 1},
		{`find . -fprint results`, "caution", 0},
	} {
		t.Run(tc.command, func(t *testing.T) {
			got := server.classifyCommand(HookQueryParams{Command: tc.command, CWD: project})
			if got.Action != "block" || got.Tier != tc.tier || got.MinApprovals != tc.approvals {
				t.Errorf("got action=%s tier=%s approvals=%d; want block/%s/%d", got.Action, got.Tier, got.MinApprovals, tc.tier, tc.approvals)
			}
			if got.ExecutionHandoff != nil {
				t.Fatal("unapproved find action received an execution handoff")
			}
		})
	}
	got := server.classifyCommand(HookQueryParams{Command: `find . -name '-delete'`, CWD: project})
	if got.Action != "allow" {
		t.Fatalf("inert find predicate was blocked: %+v", got)
	}
}
