package core

import (
	"encoding/json"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// These inputs are classified only. No cloud client or requested command runs.
func TestCloudOptionsPreserveDestructiveRisk(t *testing.T) {
	for _, tc := range []struct {
		command string
		tier    RiskTier
	}{
		{`kubectl --context production delete namespace production`, RiskTierCritical},
		{`kubectl --context=production --namespace team delete node worker`, RiskTierCritical},
		{`kubectl delete --namespace team namespace production`, RiskTierCritical},
		{`kubectl -nteam delete pv volume`, RiskTierCritical},
		{`kubectl delete --context production --grace-period 0 node worker`, RiskTierCritical},
		{`kubectl delete --cascade namespace production`, RiskTierCritical},
		{`kubectl delete --cascade=foreground namespace production`, RiskTierCritical},
		{`kubectl --context production delete ns production`, RiskTierCritical},
		{`kubectl delete --context production pod/victim namespace/production`, RiskTierCritical},
		{`kubectl --request-timeout=10s delete deployment app`, RiskTierDangerous},
		{`kubectl delete --namespace team deployment app`, RiskTierDangerous},
		{`kubectl delete --namespace team pod victim`, RiskTierDangerous},
		{`kubectl --kubeconfig=/tmp/config delete namespace production`, RiskTierCritical},
		{`/usr/bin/kubectl --context production delete namespace production`, RiskTierCritical},
		{`sudo kubectl -nteam delete namespace production`, RiskTierCritical},
		{`if true; then kubectl --context production delete namespace production; fi`, RiskTierCritical},
		{`kubectl --help --help=false delete namespace production`, RiskTierCritical},
		{`kubectl --help delete namespace production --help=false`, RiskTierCritical},
		{`kubectl -h delete namespace production --help=false`, RiskTierCritical},
		{`kubectl --context production delete --help --help=false namespace production`, RiskTierCritical},
		{`kubectl --context --help delete namespace production`, RiskTierCritical},
		{`kubectl --help --help="$HELP" delete namespace production`, RiskTierCritical},
		{`helm --kube-context production uninstall release`, RiskTierDangerous},
		{`helm -nteam --debug=false uninstall release`, RiskTierDangerous},
		{`helm --kubeconfig /tmp/config uninstall release`, RiskTierDangerous},
		{`sh -c 'helm --kube-context production uninstall release'`, RiskTierDangerous},
		{`helm --help --help=false uninstall release`, RiskTierDangerous},
		{`helm --help uninstall release --help=false`, RiskTierDangerous},
		{`helm --help uninstall release --future-option value`, RiskTierDangerous},
		{`terraform -chdir=/tmp destroy`, RiskTierCritical},
		{`terraform -chdir='repository with spaces' destroy -input=false`, RiskTierCritical},
		{`terraform destroy -lock=false -parallelism=2`, RiskTierCritical},
		{`terraform destroy -var 'label=-target=example'`, RiskTierCritical},
		{`terraform destroy -var=label=-target=example`, RiskTierCritical},
		{`terraform destroy -target=aws_instance.legacy`, RiskTierDangerous},
		{`terraform -chdir=/tmp destroy -input=false -target aws_instance.legacy`, RiskTierDangerous},
		{`terraform -chdir=/tmp apply -destroy -auto-approve`, RiskTierCritical},
		{`terraform apply -destroy=true -input=false`, RiskTierCritical},
		{`terraform apply -destroy -target=aws_instance.legacy`, RiskTierDangerous},
		{`/usr/bin/terraform -chdir=/tmp destroy`, RiskTierCritical},
	} {
		t.Run(tc.command, func(t *testing.T) {
			got := NewPatternEngine().ClassifyCommand(tc.command, "")
			if got.Tier != tc.tier || !got.NeedsApproval || got.IsSafe || got.ParseError {
				t.Fatalf("cloud options hid destructive intent: %+v; normalized=%+v", got, NormalizeCommand(tc.command))
			}
		})
	}
}

func TestCloudOptionsKeepUnknownInputsOutOfSafe(t *testing.T) {
	for _, command := range []string{
		`kubectl --kubeconfig=/tmp/config delete pod victim`,
		`kubectl --kuberc=/tmp/preferences get pods`,
		`kubectl --context "$CLUSTER" delete pod victim`,
		`kubectl --future-option value delete pod victim`,
		`kubectl --namespace`, `kubectl delete --namespace`,
		`kubectl delete pod victim --future-option value`,
		`kubectl delete pod victim -f resources.yaml`,
		`kubectl delete --filename resources.yaml`,
		`kubectl delete "$RESOURCE" victim`,
		`helm --unknown-option value uninstall release`,
		`helm --kube-context`,
		`terraform -chdir`, `terraform -chdir /tmp destroy`,
		`terraform destroy -future-option`,
		`terraform destroy -target`,
		`xargs kubectl --context production delete`,
	} {
		t.Run(command, func(t *testing.T) {
			got := NewPatternEngine().ClassifyCommand(command, "")
			if !got.NeedsApproval || got.IsSafe || !got.Opaque {
				t.Fatalf("unknown cloud input borrowed an allowlist result: %+v", got)
			}
		})
	}
}

func TestCloudOptionsInspectGlobalsAfterSubcommands(t *testing.T) {
	engine := NewPatternEngine()
	for _, pattern := range []string{`^kubectl\s+get\b`, `^helm\s+list\b`} {
		if err := engine.AddPattern(RiskTier(RiskSafe), pattern, "read-only cloud command", "test"); err != nil {
			t.Fatal(err)
		}
	}
	for _, command := range []string{
		`kubectl get pods --kubeconfig=/tmp/config`,
		`kubectl get pods --kuberc=/tmp/preferences`,
		`kubectl get --namespace team pods --kubeconfig /tmp/config`,
		`helm list --kubeconfig /tmp/config`,
		`kubectl get pods --future-option --kubeconfig=/tmp/config`,
		`helm list --future-option --help=false`,
	} {
		got := engine.ClassifyCommand(command, "")
		if !got.NeedsApproval || got.IsSafe || !got.Opaque {
			t.Errorf("post-subcommand configuration or unknown grammar borrowed SAFE: %q: %+v", command, got)
		}
	}
	for _, command := range []string{
		`kubectl get pods --namespace team`,
		`kubectl get pods --namespace --kubeconfig`,
		`kubectl get pods -- --kubeconfig=/tmp/config`,
		`helm list --kube-context --kubeconfig`,
	} {
		got := engine.ClassifyCommand(command, "")
		if !got.IsSafe || got.NeedsApproval || got.Opaque || got.ParseError {
			t.Errorf("known option value or -- operand was reinterpreted as a flag: %q: %+v", command, got)
		}
	}
}

func TestCloudOptionsKeepPreviewsAndDataInert(t *testing.T) {
	for _, command := range []string{
		`kubectl --context delete get pods`,
		`helm --kube-context uninstall list`,
		`terraform -chdir=destroy plan`,
		`terraform -chdir=/tmp plan -destroy -input=false`,
		`terraform apply -destroy=false`,
		`terraform apply -var label=-destroy`,
		`kubectl --help delete namespace production`,
		`kubectl --help=false --help delete namespace production`,
		`kubectl --help=false delete namespace production --help=true`,
		`helm --help uninstall release`,
		`helm --help=false uninstall release --help=true`,
		`terraform -help destroy`,
		`echo 'kubectl --context production delete namespace production'`,
	} {
		got := NewPatternEngine().ClassifyCommand(command, "")
		if got.NeedsApproval || got.ParseError || got.Opaque {
			t.Errorf("cloud option data became an executable action: %q: %+v", command, got)
		}
	}
	for _, command := range []string{
		`kubectl delete pod victim --namespace=development`,
		`kubectl --namespace development delete pod victim`,
	} {
		got := NewPatternEngine().ClassifyCommand(command, "")
		if !got.IsSafe || got.NeedsApproval || got.HasUnmatchedSegment {
			t.Errorf("known pod deletion lost its safe classification: %q: %+v", command, got)
		}
	}
}

func TestCloudOptionsKeepStdinOutOfArguments(t *testing.T) {
	for _, tc := range []struct {
		command string
		tier    RiskTier
	}{
		{`kubectl --context production delete namespace production <<< '--help'`, RiskTierCritical},
		{`sudo kubectl --context production delete namespace production <<< '--help=true'`, RiskTierCritical},
		{`if true; then kubectl --context production delete namespace production <<< '--help'; fi`, RiskTierCritical},
		{`sh -c "kubectl --context production delete namespace production <<< '--help'"`, RiskTierCritical},
		{`terraform -chdir=/tmp destroy <<< '-target=aws_instance.legacy'`, RiskTierCritical},
		{`sudo terraform -chdir=/tmp destroy <<< '-target aws_instance.legacy'`, RiskTierCritical},
		{`if true; then terraform -chdir=/tmp destroy <<< '-target=aws_instance.legacy'; fi`, RiskTierCritical},
		{`terraform apply <<< '-destroy'`, ""},
		{`kubectl --help=true --context production delete namespace production <<< '--help=false'`, ""},
		{`psql <<< 'TRUNCATE users'`, RiskTierCritical},
		{`env psql <<< 'TRUNCATE users'`, RiskTierCritical},
		{`kubectl exec -i database-pod -- psql <<< 'DROP DATABASE production'`, RiskTierCritical},
		{`kubectl --context production exec -i database-pod -- psql <<< 'TRUNCATE users'`, RiskTierCritical},
		{`if true; then kubectl exec -i database-pod -- psql <<< 'DELETE FROM items
WHERE id=1'; fi`, RiskTierDangerous},
	} {
		t.Run(tc.command, func(t *testing.T) {
			got := NewPatternEngine().ClassifyCommand(tc.command, "")
			if got.Tier != tc.tier || got.NeedsApproval != (tc.tier != "") || got.ParseError {
				t.Errorf("stdin data changed cloud flags or SQL inspection: %+v; normalized=%+v", got, NormalizeCommand(tc.command))
			}
		})
	}
}

func TestCloudOptionsRetainCustomRiskAndSpecification(t *testing.T) {
	engine := NewPatternEngine()
	pattern := `^kubectl\s+--context\s+production\s+delete\s+pod\s+victim$`
	if err := engine.AddPattern(RiskTierCritical, pattern, "protect production context", "test"); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		`kubectl --context production delete pod victim`,
		`sh -c 'kubectl --context production delete pod victim'`,
		`printf 'kubectl --context production delete pod victim' | sh`,
	} {
		got := engine.ClassifyCommand(command, "")
		if got.Tier != RiskTierCritical || !got.NeedsApproval || got.MatchedPattern != pattern || got.MinApprovals < 2 {
			t.Errorf("normalization weakened an existing project rule: %q: %+v", command, got)
		}
	}

	spec := CommandSpec{Raw: "display text is not authoritative", Cwd: "/tmp",
		Argv: []string{"/usr/bin/kubectl", "--namespace", "team", "delete", "namespace", "production"},
		Hash: "the-original-reviewed-command-hash"}
	before := spec
	before.Argv = append([]string(nil), spec.Argv...)
	got := engine.ClassifyCommandSpec(spec)
	if got.Tier != RiskTierCritical || !reflect.DeepEqual(spec, before) {
		t.Fatalf("cloud classification rewrote execution input or missed argv: result=%+v spec=%+v", got, spec)
	}
}

func TestCloudOptionsOriginalViewsDoNotAuthorizeUnknownOperands(t *testing.T) {
	for _, command := range []string{
		`xargs kubectl delete pod victim --namespace`,
		`find . -exec kubectl delete pod victim --namespace {} \;`,
	} {
		got := NewPatternEngine().ClassifyCommand(command, "")
		if got.Tier != RiskTierDangerous || !got.NeedsApproval || got.IsSafe {
			t.Errorf("stripped unknown option value regained a SAFE exemption: %q: %+v", command, got)
		}
	}
}

func TestCloudOptionsPreserveSafePrecedence(t *testing.T) {
	engine := NewPatternEngine()
	if err := engine.AddPattern(RiskTierCritical, `^kubectl\s+delete\s+pod\s+victim\s+--namespace=development$`, "overlapped by prior safe rule", "test"); err != nil {
		t.Fatal(err)
	}
	if err := engine.AddPattern(RiskTier(RiskSafe), `^kubectl\s+--context\s+reviewed\s+delete`, "original view only", "test"); err != nil {
		t.Fatal(err)
	}
	got := engine.ClassifyCommand("kubectl delete pod victim --namespace=development", "")
	if !got.IsSafe || got.NeedsApproval {
		t.Errorf("original SAFE view reactivated an overlapping hazardous rule: %+v", got)
	}
	got = engine.ClassifyCommand("kubectl --context reviewed delete namespace production", "")
	if got.Tier != RiskTierCritical || !got.NeedsApproval || got.IsSafe {
		t.Errorf("original SAFE view weakened the canonical operation: %+v", got)
	}
}

func TestCloudOptionsExportParity(t *testing.T) {
	python := findPython3(t)
	engine := NewPatternEngine()
	for _, pattern := range []string{`^kubectl\s+get\b`, `^helm\s+list\b`} {
		if err := engine.AddPattern(RiskTier(RiskSafe), pattern, "read-only cloud command", "test"); err != nil {
			t.Fatal(err)
		}
	}
	if err := engine.AddPattern(RiskTierCritical, `^kubectl\s+--context\s+production\s+delete\s+pod\s+victim$`, "protect production context", "test"); err != nil {
		t.Fatal(err)
	}
	if err := engine.AddPattern(RiskTierCritical, `^kubectl\s+delete\s+pod\s+victim\s+--namespace=development$`, "overlapped by prior safe rule", "test"); err != nil {
		t.Fatal(err)
	}
	if err := engine.AddPattern(RiskTier(RiskSafe), `^kubectl\s+--context\s+reviewed\s+delete`, "original view only", "test"); err != nil {
		t.Fatal(err)
	}
	// Expected tiers make this a behavioral check as well as a parity check:
	// identical missed operations in both implementations must still fail.
	cases := []struct{ command, tier string }{
		{"kubectl --context production delete namespace production", "critical"},
		{"kubectl delete --namespace team namespace production", "critical"},
		{"kubectl delete --cascade namespace production", "critical"},
		{"kubectl --context production delete ns production", "critical"},
		{"kubectl delete --context production pod/victim namespace/production", "critical"},
		{"kubectl delete pod victim --namespace=development", "safe"},
		{"kubectl delete --namespace development pod victim", "dangerous"},
		{"kubectl --namespace development delete pod victim", "safe"},
		{"kubectl --context production delete pod victim", "critical"},
		{"kubectl --context reviewed delete namespace production", "critical"},
		{"kubectl --kubeconfig=/tmp/config delete pod victim", "caution"},
		{"kubectl --future-option value delete pod victim", "caution"},
		{"kubectl delete --namespace", "critical"},
		{"kubectl --help --help=false delete namespace production", "critical"},
		{"kubectl --help delete namespace production --help=false", "critical"},
		{"kubectl --context production delete --help --help=false namespace production", "critical"},
		{"kubectl --context --help delete namespace production", "critical"},
		{"helm --kube-context production uninstall release", "dangerous"},
		{"helm -nteam --debug=false uninstall release", "dangerous"},
		{"helm --help --help=false uninstall release", "dangerous"},
		{"helm --help uninstall release --help=false", "dangerous"},
		{"kubectl get pods --kubeconfig=/tmp/config", "caution"},
		{"kubectl get pods --kuberc=/tmp/preferences", "caution"},
		{"helm list --kubeconfig /tmp/config", "caution"},
		{"kubectl get pods --future-option --kubeconfig=/tmp/config", "caution"},
		{"kubectl get pods --namespace --kubeconfig", "safe"},
		{"kubectl get pods -- --kubeconfig=/tmp/config", "safe"},
		{"helm list --kube-context --kubeconfig", "safe"},
		{"terraform -chdir=/tmp destroy -input=false", "critical"},
		{"terraform destroy -input=false", "critical"},
		{"terraform destroy -lock=false -parallelism=2", "critical"},
		{"terraform destroy -var=label=-target=example", "critical"},
		{"terraform -chdir=/tmp destroy -target=aws_instance.legacy", "dangerous"},
		{"terraform apply -destroy -auto-approve", "critical"},
		{"terraform apply -destroy -target=aws_instance.legacy", "dangerous"},
		{"terraform apply -destroy=false", "unknown"},
		{"terraform -chdir=/tmp plan -destroy", "unknown"},
		{"kubectl --help delete namespace production", "unknown"},
		{"kubectl --help=false --help delete namespace production", "unknown"},
		{"kubectl --help=false delete namespace production --help=true", "unknown"},
		{"helm --help uninstall release", "unknown"},
		{"helm --help=false uninstall release --help=true", "unknown"},
		{"terraform -help destroy", "unknown"},
		{"kubectl delete pod victim --namespace __slb_xargs_arguments__", "dangerous"},
		{"kubectl delete pod victim --namespace __slb_find_path__", "dangerous"},
	}
	commands := make([]string, len(cases))
	for i, tc := range cases {
		commands[i] = tc.command
	}
	input, err := json.Marshal(commands)
	if err != nil {
		t.Fatal(err)
	}
	program := engine.ExportClaudeHook() + `
import json, sys
print(json.dumps([classify(command)[0] for command in json.load(sys.stdin)]))
`
	cmd := exec.Command(python, "-c", program)
	cmd.Stdin = strings.NewReader(string(input))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("exported cloud classifier failed: %v\n%s", err, out)
	}
	var tiers []string
	if err := json.Unmarshal(out, &tiers); err != nil || len(tiers) != len(commands) {
		t.Fatalf("invalid exported classifications: %v\n%s", err, out)
	}
	for i, tc := range cases {
		native := engine.ClassifyCommand(tc.command, "")
		want := string(native.Tier)
		if want == "" {
			want = "unknown"
		}
		if tiers[i] != tc.tier || tiers[i] != want {
			t.Errorf("%q: Python=%s native=%s expected=%s", tc.command, tiers[i], want, tc.tier)
		}
	}
}
