package daemon

import (
	"testing"

	"github.com/Dicklesworthstone/slb/internal/testutil"
)

func TestHookQueryCloudOptionsRiskAndQuorum(t *testing.T) {
	t.Setenv("HOME", testutil.NonSystemTempDir(t))
	server := &IPCServer{}
	project := testutil.NonSystemTempDir(t)
	for _, tc := range []struct {
		command, tier string
		approvals     int
	}{
		{"kubectl --context production delete namespace production", "critical", 2},
		{"kubectl delete --namespace team namespace production", "critical", 2},
		{"kubectl delete --cascade namespace production", "critical", 2},
		{"kubectl --help delete namespace production --help=false", "critical", 2},
		{"helm --kube-context production uninstall release", "dangerous", 1},
		{"helm --help --help=false uninstall release", "dangerous", 1},
		{"terraform -chdir=/tmp destroy -input=false", "critical", 2},
		{"terraform destroy -var=label=-target=example", "critical", 2},
		{"terraform -chdir=/tmp destroy -target=aws_instance.legacy", "dangerous", 1},
		{"terraform apply -destroy -auto-approve", "critical", 2},
	} {
		t.Run(tc.command, func(t *testing.T) {
			got := server.classifyCommand(HookQueryParams{Command: tc.command, CWD: project})
			if got.Action != "block" || got.Tier != tc.tier || got.MinApprovals != tc.approvals {
				t.Errorf("got action=%s tier=%s approvals=%d; want block/%s/%d", got.Action, got.Tier, got.MinApprovals, tc.tier, tc.approvals)
			}
			if got.ExecutionHandoff != nil {
				t.Fatal("unapproved cloud command received an execution handoff")
			}
		})
	}
}
