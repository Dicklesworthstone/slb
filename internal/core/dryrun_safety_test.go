package core

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestDryRunKubectlCannotDisablePreview(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"default", []string{"pod", "demo"}},
		{"none", []string{"pod", "demo", "--dry-run=none"}},
		{"false", []string{"--dry-run=false", "pod", "demo"}},
		{"server", []string{"--dry-run=server", "pod", "demo"}},
		{"bare", []string{"--dry-run", "pod", "demo"}},
		{"separate none", []string{"--dry-run", "none", "pod", "demo"}},
		{"separate false", []string{"pod", "demo", "--dry-run", "false"}},
		{"duplicate override", []string{"--dry-run=client", "pod", "demo", "--dry-run=none"}},
		{"terminator", []string{"--", "pod", "demo"}},
		{"positional mode", []string{"pod", "--", "--dry-run=none"}},
		{"dangling value flag", []string{"pod", "demo", "--namespace"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := append([]string{"kubectl", "delete"}, tt.args...)
			original := append([]string(nil), input...)
			got, ok := dryRunKubectl(input)
			if !ok || len(got) < 3 || got[2] != "--dry-run=client" {
				t.Fatalf("preview must enforce client mode before all user arguments: %v, %v", got, ok)
			}
			modes := 0
			for _, arg := range got[2:] {
				if arg == "--" {
					break
				}
				if strings.HasPrefix(arg, "--dry-run") {
					modes++
					if arg != "--dry-run=client" {
						t.Fatalf("unsafe override remains: %v", got)
					}
				}
			}
			if modes != 1 {
				t.Fatalf("expected exactly one enforced mode, got %v", got)
			}
			if !reflect.DeepEqual(input, original) {
				t.Fatalf("preview mutated requested argv: %v", input)
			}
			if boundary := indexOfTerminator(input); boundary >= 0 {
				outputBoundary := indexOfTerminator(got)
				if outputBoundary < 0 || !reflect.DeepEqual(input[boundary:], got[outputBoundary:]) {
					t.Fatalf("operands changed after --: input=%v output=%v", input, got)
				}
			}
		})
	}
}

func indexOfTerminator(args []string) int {
	for i, arg := range args {
		if arg == "--" {
			return i
		}
	}
	return -1
}

func TestDryRunKubectlRejectsRawDeletion(t *testing.T) {
	for _, args := range [][]string{
		{"--raw=/api/v1/namespaces/demo"},
		{"--raw", "/api/v1/namespaces/demo"},
		{"pod", "demo", "--dry-run-unknown"},
	} {
		if got, ok := dryRunKubectl(append([]string{"kubectl", "delete"}, args...)); ok {
			t.Errorf("unsafe/ambiguous preview accepted: %v", got)
		}
	}
}

func TestDryRunKubectlPreservesOutputAndScope(t *testing.T) {
	for _, output := range [][]string{{"-o", "json"}, {"-o=json"}, {"--output", "json"}, {"--output=json"}} {
		args := append([]string{"kubectl", "delete", "pod", "demo", "--namespace", "staging", "--context=test"}, output...)
		got, ok := dryRunKubectl(args)
		want := append([]string{"kubectl", "delete", "--dry-run=client"}, args[2:]...)
		if !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("scope/output changed: got=%v want=%v", got, want)
		}
	}
}

func TestDryRunRejectsIncompleteCommandDescriptions(t *testing.T) {
	for _, raw := range []string{
		`kubectl delete pod demo; echo done`,
		`kubectl delete pod demo && echo done`,
		`kubectl delete pod demo | cat`,
		`kubectl delete pod "unterminated`,
		`kubectl delete pod $(printf demo)`,
	} {
		if got, ok := GetDryRunCommand(raw); ok {
			t.Errorf("partial/ambiguous preview for %q: %q", raw, got)
		}
	}
}

func TestRunDryRunEnforcesClientModeAtProcessBoundary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test executable is a POSIX shell script")
	}
	dir := t.TempDir()
	// The fake kubectl only records argv. No cluster or destructive command is used.
	tool := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, raw := range []string{
		"kubectl delete pod demo --dry-run=none",
		"kubectl delete -- pod demo",
	} {
		result, err := RunDryRun(&db.CommandSpec{Raw: raw, Cwd: dir})
		if err != nil || result == nil {
			t.Fatalf("RunDryRun(%q): result=%v err=%v", raw, result, err)
		}
		if !strings.HasPrefix(result.Output, "delete\n--dry-run=client\n") {
			t.Fatalf("actual process did not receive the enforced mode first: %q", result.Output)
		}
	}
}

func TestPreviewPreservesLiteralArgumentBoundaries(t *testing.T) {
	tests := []struct {
		raw  string
		want []string
	}{
		{`rm -rf 'directory with spaces'`, []string{"ls", "-la", "--", "directory with spaces"}},
		{`rm -- 'literal; echo not-a-command'`, []string{"ls", "-la", "--", "literal; echo not-a-command"}},
		{`rm -- 'literal$HOME'`, []string{"ls", "-la", "--", "literal$HOME"}},
		{`rm -- "price\$5"`, []string{"ls", "-la", "--", "price$5"}},
		{`rm -- 'a\b'`, []string{"ls", "-la", "--", `a\b`}},
		{`rm -- ''`, []string{"ls", "-la", "--", ""}},
		{`rm -- file~backup#suffix`, []string{"ls", "-la", "--", "file~backup#suffix"}},
		{`git reset --hard HEAD~1`, []string{"git", "diff", "--no-ext-diff", "--no-textconv", "HEAD~1..HEAD"}},
		{`bash -c 'rm -rf "directory with spaces"'`, []string{"ls", "-la", "--", "directory with spaces"}},
		{`sudo rm -rf 'directory with spaces'`, []string{"ls", "-la", "--", "directory with spaces"}},
		{`kubectl delete pod --selector='env in (one, two)'`, []string{"kubectl", "delete", "--dry-run=client", "-o", "yaml", "pod", "--selector=env in (one, two)"}},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, ok := getDryRunTokens(tt.raw)
			if !ok || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("preview changed arguments: got=%q want=%q supported=%v", got, tt.want, ok)
			}
			command, ok := GetDryRunCommand(tt.raw)
			if !ok || !reflect.DeepEqual(parseShellTokens(command), tt.want) {
				t.Fatalf("displayed preview does not round-trip to actual argv: %q", command)
			}
		})
	}
}

func TestPreviewRejectsDynamicOrContextChangingCommands(t *testing.T) {
	for _, raw := range []string{
		"kubectl delete pod demo\nprintf done",
		`bash -c 'kubectl delete pod demo; printf done'`,
		`bash -c 'kubectl delete pod demo' extra-argument`,
		`env KUBECONFIG=production kubectl delete pod demo`,
		`sudo -u other kubectl delete pod demo`,
		`command -v kubectl delete pod demo`,
		`rm -rf "$HOME/cache"`,
		`rm -rf $(printf target)`,
		"rm -rf `printf target`",
		`rm *.log`,
		`rm ~/cache`,
		`rm ~other/cache`,
		`rm file > evidence.txt`,
		`rm file # ignored-by-shell`,
		"rm \\\nfile",
		`rm "a\q"`,
	} {
		if got, ok := GetDryRunCommand(raw); ok {
			t.Errorf("incomplete or changed-context preview accepted for %q: %q", raw, got)
		}
	}
	deep := "rm file"
	for i := 0; i < 10; i++ {
		deep = shellJoin([]string{"bash", "-c", deep})
	}
	if _, ok := GetDryRunCommand(deep); ok {
		t.Fatal("unbounded shell-wrapper nesting accepted")
	}
}

func TestHelmPreviewPreservesClusterAndNamespace(t *testing.T) {
	for _, tt := range []struct {
		raw  string
		want []string
	}{
		{
			`helm uninstall myapp --namespace production --kube-context=prod --wait --timeout 5m`,
			[]string{"helm", "get", "manifest", "myapp", "--namespace=production", "--kube-context=prod"},
		},
		{
			`helm --kubeconfig '/tmp/kube config' uninstall myapp -nproduction --keep-history --no-hooks`,
			[]string{"helm", "get", "manifest", "myapp", "--kubeconfig=/tmp/kube config", "-n=production"},
		},
		{
			`helm uninstall --kube-as-group=team-a myapp --kube-as-group team-b --kube-insecure-skip-tls-verify=false`,
			[]string{"helm", "get", "manifest", "myapp", "--kube-as-group=team-a", "--kube-as-group=team-b", "--kube-insecure-skip-tls-verify=false"},
		},
	} {
		got, ok := getDryRunTokens(tt.raw)
		if !ok || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("Helm preview changed scope: raw=%q got=%q want=%q", tt.raw, got, tt.want)
		}
	}
	for _, raw := range []string{
		"helm uninstall first second",
		"helm uninstall myapp --namespace",
		"helm uninstall myapp --unknown-scope=production",
		"helm uninstall --wait",
		"helm install myapp",
	} {
		if got, ok := GetDryRunCommand(raw); ok {
			t.Errorf("partial/ambiguous Helm preview accepted for %q: %q", raw, got)
		}
	}
}

func TestTerraformPreviewKeepsPlanningInputs(t *testing.T) {
	raw := `terraform -chdir='infra root' destroy -auto-approve -input=true -var='name=two words' -target=module.app -var-file='production vars.tfvars'`
	want := []string{"terraform", "-chdir=infra root", "plan", "-destroy", "-input=false", "-var=name=two words", "-target=module.app", "-var-file=production vars.tfvars"}
	got, ok := getDryRunTokens(raw)
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("Terraform preview changed planning inputs: got=%q want=%q", got, want)
	}
	for _, arg := range []string{"-out=plan.bin", "-out plan.bin", "-state-out=new.tfstate", "-backup=state.bak", "-destroy=false", "-refresh-only"} {
		if got, ok := GetDryRunCommand("terraform destroy " + arg); ok {
			t.Errorf("unsafe/misleading planning option accepted: %q", got)
		}
	}
}

func TestRunDryRunUsesExecutionArgv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("rm preview requires a POSIX ls executable")
	}
	directory := t.TempDir()
	for _, name := range []string{"actual target", "display-target"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("untouched"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, shell := range []bool{false, true} {
		spec := &db.CommandSpec{
			Raw:   "rm -rf display-target",
			Argv:  []string{"rm", "-rf", "actual target"},
			Shell: shell,
			Cwd:   directory,
		}
		result, err := RunDryRun(spec)
		if err != nil || result == nil {
			t.Fatalf("preview failed: %v, %v", result, err)
		}
		want := "actual target"
		if shell {
			want = "display-target"
		}
		if !strings.Contains(result.Output, want) {
			t.Fatalf("preview did not follow execution mode (shell=%v): %q", shell, result.Output)
		}
	}
	for _, name := range []string{"actual target", "display-target"} {
		if got, err := os.ReadFile(filepath.Join(directory, name)); err != nil || string(got) != "untouched" {
			t.Fatalf("preview modified %q: %q, %v", name, got, err)
		}
	}
}

func TestGitPreviewDoesNotRunConfiguredDiffPrograms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("custom diff fixture uses a POSIX shell script")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "absent-config"))
	t.Setenv("GIT_EXTERNAL_DIFF", "")
	if err := os.Unsetenv("GIT_EXTERNAL_DIFF"); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"external", "textconv"} {
		t.Run(kind, func(t *testing.T) {
			directory := t.TempDir()
			git := func(args ...string) string {
				t.Helper()
				command := exec.Command("git", args...)
				command.Dir = directory
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("git %q: %v: %s", args, err, output)
				}
				return string(output)
			}
			git("init", "-q")
			git("config", "user.name", "SLB preview test")
			git("config", "user.email", "slb-preview@example.invalid")
			git("config", "commit.gpgsign", "false")
			if err := os.WriteFile(filepath.Join(directory, ".gitattributes"), []byte("tracked.txt diff=slbpreview\n"), 0600); err != nil {
				t.Fatal(err)
			}
			for _, content := range []string{"old\n", "new\n"} {
				if err := os.WriteFile(filepath.Join(directory, "tracked.txt"), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
				git("add", ".gitattributes", "tracked.txt")
				git("commit", "-qm", "preview fixture")
			}
			marker := filepath.Join(directory, "custom-program-ran")
			t.Setenv("SLB_TEST_DIFF_MARKER", marker)
			program := filepath.Join(directory, "custom-diff.sh")
			if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf 'invoked\\n' >> \"$SLB_TEST_DIFF_MARKER\"\nprintf 'custom output\\n'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			if kind == "external" {
				git("config", "diff.external", program)
				t.Setenv("GIT_EXTERNAL_DIFF", program)
			} else {
				git("config", "diff.slbpreview.textconv", program)
			}

			// Demonstrate that ordinary git diff really executes the configured
			// program; the fixture only writes to a disposable marker file.
			git("diff", "HEAD~1..HEAD")
			if contents, err := os.ReadFile(marker); err != nil || len(contents) == 0 {
				t.Fatalf("custom-program fixture did not run: %q, %v", contents, err)
			}
			if err := os.WriteFile(marker, nil, 0600); err != nil {
				t.Fatal(err)
			}
			head := git("rev-parse", "HEAD")
			if preview, ok := GetDryRunCommand("git reset --hard HEAD~1"); !ok || !strings.Contains(preview, "--no-ext-diff --no-textconv") {
				t.Fatalf("raw-command preview lost its Git revision or safety options: %q", preview)
			}
			result, err := RunDryRun(&db.CommandSpec{
				Raw:  "git reset --hard HEAD~1",
				Argv: []string{"git", "reset", "--hard", "HEAD~1"},
				Cwd:  directory,
			})
			if err != nil || result == nil {
				t.Fatalf("Git preview failed: %v, %v", result, err)
			}
			if contents, err := os.ReadFile(marker); err != nil || len(contents) != 0 {
				t.Fatalf("Git preview executed %s before approval: %q, %v", kind, contents, err)
			}
			if !strings.Contains(result.Command, "--no-ext-diff") || !strings.Contains(result.Command, "--no-textconv") {
				t.Fatalf("preview omitted its safety options: %s", result.Command)
			}
			if !strings.Contains(result.Output, "-old") || !strings.Contains(result.Output, "+new") {
				t.Fatalf("preview did not produce the builtin diff: %q", result.Output)
			}
			if head != git("rev-parse", "HEAD") {
				t.Fatal("preview changed HEAD")
			}
			if contents, err := os.ReadFile(filepath.Join(directory, "tracked.txt")); err != nil || string(contents) != "new\n" {
				t.Fatalf("preview changed the working tree: %q, %v", contents, err)
			}
		})
	}
}
