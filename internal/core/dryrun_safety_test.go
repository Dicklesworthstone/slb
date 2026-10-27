package core

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func gitResetPreviewFixture(t *testing.T) (string, func(...string) string, func(string, string)) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	repo := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(repo, "no-global-config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-b", "main")
	git("config", "user.name", "SLB Preview Fixture")
	git("config", "user.email", "fixture@example.invalid")
	git("config", "commit.gpgsign", "false")
	for _, name := range []string{"staged.txt", "unstaged.txt", "index-only.txt"} {
		write(name, "original\n")
	}
	git("add", "--", "staged.txt", "unstaged.txt", "index-only.txt")
	git("commit", "-m", "fixture baseline")
	write("committed.txt", "content in the newer commit\n")
	git("add", "--", "committed.txt")
	git("commit", "-m", "fixture newer commit")
	write("staged.txt", "staged content that reset would discard\n")
	write("index-only.txt", "index-only content that reset would discard\n")
	git("add", "--", "staged.txt", "index-only.txt")
	write("index-only.txt", "original\n")
	write("unstaged.txt", "unstaged content that reset would discard\n")
	write("untracked.txt", "untracked content must survive preview\n")
	write(".gitignore", "ignored.txt\n")
	write("ignored.txt", "ignored content must survive preview\n")
	return repo, git, write
}

func TestGitResetPreviewShowsDiscardedLocalState(t *testing.T) {
	repo, git, _ := gitResetPreviewFixture(t)
	headBefore := git("rev-parse", "HEAD")
	worktreeBefore := git("diff", "--no-ext-diff", "--no-textconv", "HEAD", "--")
	indexBefore, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"git reset --hard HEAD", "git reset --hard"} {
		preview, err := RunDryRun(&db.CommandSpec{Raw: command, Cwd: repo})
		if err != nil || preview == nil {
			t.Fatalf("%s preview: %+v, %v", command, preview, err)
		}
		for _, want := range []string{"staged content that reset would discard", "unstaged content that reset would discard", "index-only content that reset would discard", "untracked.txt", "ignored.txt", "not an exact collision list"} {
			if !strings.Contains(preview.Output, want) {
				t.Errorf("%s preview omitted %q:\n%s", command, want, preview.Output)
			}
		}
	}
	indexAfter, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(indexBefore, indexAfter) || git("rev-parse", "HEAD") != headBefore || git("diff", "--no-ext-diff", "--no-textconv", "HEAD", "--") != worktreeBefore {
		t.Fatal("preview changed the index, HEAD, or working tree")
	}
	if content, err := os.ReadFile(filepath.Join(repo, "untracked.txt")); err != nil || string(content) != "untracked content must survive preview\n" {
		t.Fatalf("preview changed untracked content: %q, %v", content, err)
	}
}

func TestGitResetPreviewRespectsModesAndExecutionArgv(t *testing.T) {
	repo, git, _ := gitResetPreviewFixture(t)
	head := strings.TrimSpace(git("rev-parse", "HEAD"))
	target := strings.TrimSpace(git("rev-parse", "HEAD~1"))
	for _, tc := range []struct {
		mode            string
		wantIndex       bool
		wantWorkingTree bool
		wantNotice      string
	}{
		{"--soft", false, false, "Only refs move"},
		{"--mixed", true, false, "working-tree files are preserved"},
		{"", true, false, "working-tree files are preserved"},
		{"--hard", true, true, "may be deleted"},
		{"--merge", true, true, "do not simulate Git's merge checks"},
		{"--keep", true, true, "do not simulate Git's keep checks"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			preview, err := RunDryRun(&db.CommandSpec{Raw: "git reset " + tc.mode + " HEAD~1", Cwd: repo})
			if err != nil || preview == nil {
				t.Fatalf("preview failed: %+v, %v", preview, err)
			}
			if !strings.Contains(preview.Output, "content in the newer commit") || !strings.Contains(preview.Output, tc.wantNotice) {
				t.Fatalf("missing ref change/mode explanation:\n%s", preview.Output)
			}
			if strings.Contains(preview.Output, "index-only content that reset would discard") != tc.wantIndex || strings.Contains(preview.Output, "unstaged content that reset would discard") != tc.wantWorkingTree {
				t.Fatalf("wrong index/worktree evidence for %s:\n%s", tc.mode, preview.Output)
			}
			if !strings.Contains(preview.Command, head) || !strings.Contains(preview.Command, target) || strings.Contains(preview.Command, "HEAD~1") {
				t.Fatalf("inspection did not pin resolved commits: %s", preview.Command)
			}
		})
	}
	argv := []string{"git", "reset", "--soft", "HEAD~1"}
	for _, shell := range []bool{false, true} {
		preview, err := RunDryRun(&db.CommandSpec{Raw: "git reset --hard HEAD", Argv: argv, Shell: shell, Cwd: repo})
		if err != nil || preview == nil || strings.Contains(preview.Output, "unstaged content that reset would discard") != shell {
			t.Fatalf("preview did not follow authoritative execution mode %v: %+v, %v", shell, preview, err)
		}
	}
	if !reflect.DeepEqual(argv, []string{"git", "reset", "--soft", "HEAD~1"}) {
		t.Fatal("preview mutated requested argv")
	}
}

func TestGitResetPreviewDeclinesAmbiguousForms(t *testing.T) {
	for _, raw := range []string{
		"git reset --hard --soft HEAD", "git reset --hard HEAD extra",
		"git reset --hard HEAD -- tracked.txt", "git reset -- tracked.txt",
		"git reset --patch HEAD", "git reset -N HEAD", "git reset HEAD..main",
		"git reset --pathspec-from-file=paths", "git reset --recurse-submodules HEAD",
		"git -C elsewhere reset --hard", "git reset --unknown-option HEAD",
	} {
		if description, ok := GetDryRunCommand(raw); ok {
			t.Errorf("ambiguous or incomplete preview accepted for %q: %s", raw, description)
		}
	}
	repo, _, _ := gitResetPreviewFixture(t)
	for _, target := range []string{"no-such-ref", "staged.txt"} {
		if result, err := RunDryRun(&db.CommandSpec{Raw: "git reset --hard " + target, Cwd: repo}); err == nil || result == nil {
			t.Errorf("unresolved target %q was treated as a successful preview: %+v, %v", target, result, err)
		}
	}
}

func TestGitResetPreviewDeclinesRecursiveSubmodules(t *testing.T) {
	repo, git, _ := gitResetPreviewFixture(t)
	git("config", "submodule.recurse", "true")
	if _, err := RunDryRun(&db.CommandSpec{Raw: "git reset --hard", Cwd: repo}); err == nil || !strings.Contains(err.Error(), "recursive submodule") {
		t.Fatalf("uninspected recursive work was accepted: %v", err)
	}
	preview, err := RunDryRun(&db.CommandSpec{Raw: "git reset --hard --no-recurse-submodules", Cwd: repo})
	if err != nil || preview == nil || !strings.Contains(preview.Output, "unstaged content that reset would discard") {
		t.Fatalf("explicit nonrecursive reset was not previewed: %+v, %v", preview, err)
	}
}

func TestGitResetPreviewFromSubdirectoryIncludesEntireRepository(t *testing.T) {
	repo, git, _ := gitResetPreviewFixture(t)
	subdir := filepath.Join(repo, "subdir")
	if err := os.Mkdir(subdir, 0700); err != nil {
		t.Fatal(err)
	}
	git("config", "diff.relative", "true")
	t.Setenv("GIT_LITERAL_PATHSPECS", "1")
	preview, err := RunDryRun(&db.CommandSpec{Raw: "git reset --hard", Cwd: subdir})
	if err != nil || preview == nil {
		t.Fatalf("subdirectory preview failed: %+v, %v", preview, err)
	}
	for _, want := range []string{"unstaged content that reset would discard", "index-only content that reset would discard", "untracked.txt", "ignored.txt"} {
		if !strings.Contains(preview.Output, want) {
			t.Errorf("subdirectory preview omitted %q outside its working directory:\n%s", want, preview.Output)
		}
	}
}

func TestGitResetPreviewDoesNotRunCleanFilters(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("clean filter fixture uses a POSIX shell script")
	}
	repo, git, write := gitResetPreviewFixture(t)
	marker := filepath.Join(repo, "filter-ran")
	t.Setenv("SLB_TEST_CLEAN_FILTER_MARKER", marker)
	program := filepath.Join(repo, "clean-filter.sh")
	if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf invoked > \"$SLB_TEST_CLEAN_FILTER_MARKER\"\ncat\n"), 0700); err != nil {
		t.Fatal(err)
	}
	write(".gitattributes", "*.txt filter=slbpreview\n")
	git("config", "filter.slbpreview.clean", program)
	// This demonstrates that --no-textconv alone still runs a clean filter
	// when inspecting working files. Only the fixture's marker is written.
	git("diff", "--no-ext-diff", "--no-textconv", "HEAD", "--")
	if contents, err := os.ReadFile(marker); err != nil || len(contents) == 0 {
		t.Fatalf("clean filter fixture did not execute: %q, %v", contents, err)
	}
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := RunDryRun(&db.CommandSpec{Raw: "git reset --hard", Cwd: repo}); err == nil || !strings.Contains(err.Error(), "clean/process filters") {
		t.Fatalf("working-file filter was not refused: %v", err)
	}
	if contents, err := os.ReadFile(marker); err != nil || len(contents) != 0 {
		t.Fatalf("preview executed a clean filter: %q, %v", contents, err)
	}
}

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
			// Reset replaces the current new content with the target's old
			// content. The prior target-to-HEAD diff showed the reverse action.
			if !strings.Contains(result.Output, "-new") || !strings.Contains(result.Output, "+old") {
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
