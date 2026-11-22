package core

import (
	"reflect"
	"strings"
	"testing"
)

func TestExecutionWrappersPreserveCriticalClassification(t *testing.T) {
	engine := NewPatternEngine()
	for _, command := range []string{
		"/bin/rm -rf /",
		"/usr/bin/sudo -n -u root /bin/rm -rf /",
		"sudo --user=root -- /bin/rm -rf /",
		"sudo -Eu root rm -rf /",
		"doas -n -u root rm -rf /",
		"env -i -u DEBUG PROFILE=production rm -rf /",
		"PROFILE=production command -p rm -rf /",
		"nice -n 10 ionice -c 3 rm -rf /",
		"timeout --signal=TERM -k 2s 5s /bin/rm -rf /",
		"nohup /bin/rm -rf /",
		"exec -a cleanup /bin/rm -rf /",
		"strace -f -e trace=file -o trace.log /bin/rm -rf /",
		"ltrace /bin/rm -rf /",
		"sh -c 'echo ready; rm -rf /'",
		"bash -lc 'git stash && /bin/rm -rf /'",
		"bash --noprofile --norc -e -o pipefail -c 'echo ready; rm -rf /'",
		"sudo -u root sh -c 'echo ready; rm -rf /'",
		"sh -c 'rm -rf /' worker harmless-argument",
		"sh -c 'echo ready\nrm -rf /'",
		"printf ready\nrm -rf /",
		"printf ready # unmatched quotes '\" (\nrm -rf /",
		"printf ready; # ignored command\nrm -rf /",
		"r\\\nm -rf /",
		"(echo ready | cat; rm -rf /) | cat",
		"(echo ready; (echo nested; rm -rf /))",
	} {
		t.Run(command, func(t *testing.T) {
			result := engine.ClassifyCommand(command, "")
			if result.Tier != RiskTierCritical || !result.NeedsApproval || result.IsSafe || result.MinApprovals < 2 {
				t.Fatalf("critical command hidden by shell syntax: %+v; normalized=%+v", result, NormalizeCommand(command))
			}
		})
	}
}

func TestShellCommandBodySeparatesScriptFromArguments(t *testing.T) {
	for _, command := range []string{
		`sh -c 'printf "%s" "$1"' worker 'rm -rf /'`,
		`bash -lc 'echo harmless' worker 'rm -rf /'`,
		`echo 'sh -c "rm -rf /"'`,
		`command -v rm`,
		`sh script.sh 'rm -rf /'`,
		`echo 'line one
rm -rf /'`,
		`jq -r '.[] | .name' records.json`,
		`awk '/a|b/ { print $0 }' records.txt`,
	} {
		result := NewPatternEngine().ClassifyCommand(command, "")
		if result.NeedsApproval || result.ParseError {
			t.Errorf("inert argument or ordinary read-only command treated as execution: %q: %+v", command, result)
		}
	}
}

func TestNestedShellNormalizationKeepsAllSegments(t *testing.T) {
	result := NormalizeCommand(`sudo -u root bash -lc 'printf ready; (git stash && rm -rf /)'`)
	want := []string{"printf ready", "git stash", "rm -rf /"}
	if !reflect.DeepEqual(result.Segments, want) || !result.IsCompound || result.ParseError {
		t.Fatalf("lost executable body: %+v, want %q", result, want)
	}
	if result.Original != `sudo -u root bash -lc 'printf ready; (git stash && rm -rf /)'` {
		t.Fatal("normalization changed execution input")
	}
}

func TestUnknownWrapperOptionsAreNotSilentlyAllowed(t *testing.T) {
	for _, command := range []string{
		"sudo --unknown-option value rm -rf /",
		"sudo -u",
		"env --split-string 'rm -rf /'",
		"bash -c",
	} {
		result := NewPatternEngine().ClassifyCommand(command, "")
		if !result.ParseError || !result.NeedsApproval || result.IsSafe {
			t.Errorf("ambiguous invocation silently allowed: %q: %+v", command, result)
		}
	}
}

func TestShellAnalysisBoundsNesting(t *testing.T) {
	command := strings.Repeat("(", maxCommandNesting+10) + "echo test" + strings.Repeat(")", maxCommandNesting+10)
	if result := NormalizeCommand(command); !result.ParseError {
		t.Fatal("unbounded subshell nesting accepted")
	}
	command = strings.Repeat("sudo ", maxCommandNesting+10) + "rm -rf /"
	if result := NormalizeCommand(command); !result.ParseError {
		t.Fatal("unbounded wrapper nesting accepted")
	}
}

// These are classification inputs only; no Git command or shell is executed.
func TestGitGlobalOptionsPreserveRisk(t *testing.T) {
	for _, tc := range []struct {
		command string
		tier    RiskTier
	}{
		{`git -C /tmp reset --hard`, RiskTierDangerous},
		{`git -C '' -C 'repository with spaces' reset --hard`, RiskTierDangerous},
		{`git --git-dir /tmp/repo.git --work-tree=/tmp/work reset --hard`, RiskTierDangerous},
		{`git --bare --git-dir=/tmp/repo.git push --force origin main`, RiskTierCritical},
		{`git --namespace=team --no-pager push --force origin main`, RiskTierCritical},
		{`git -P --no-optional-locks --literal-pathspecs clean -fd`, RiskTierDangerous},
		{`git --glob-pathspecs --no-lazy-fetch --no-replace-objects reset --hard`, RiskTierDangerous},
		{`git --attr-source=HEAD reset --hard`, RiskTierDangerous},
		{`git --shallow-file /tmp/shallow reset --hard`, RiskTierDangerous},
		{`/usr/bin/git -C /tmp reset --hard`, RiskTierDangerous},
		{`sudo -u root git -C /tmp reset --hard`, RiskTierDangerous},
		{`if true; then git -C /tmp reset --hard; fi`, RiskTierDangerous},
		{`sh -c 'git -C /tmp reset --hard'`, RiskTierDangerous},
		{`git -C "$REPO" reset --hard`, RiskTierDangerous},
		{`git --git-dir=${REPO} reset --hard`, RiskTierDangerous},
		{`git -c core.fsmonitor=/tmp/monitor reset --hard`, RiskTierDangerous},
		{`git --config-env=core.sshCommand=SSH_COMMAND push --force origin main`, RiskTierCritical},
	} {
		t.Run(tc.command, func(t *testing.T) {
			result := NewPatternEngine().ClassifyCommand(tc.command, "")
			if result.Tier != tc.tier || !result.NeedsApproval || result.IsSafe || result.ParseError {
				t.Fatalf("Git global options hid the subcommand: %+v; normalized=%+v", result, NormalizeCommand(tc.command))
			}
		})
	}
}

func TestGitGlobalOptionsDoNotBorrowSafeClassification(t *testing.T) {
	for _, command := range []string{
		`git -c core.fsmonitor=/tmp/monitor stash`,
		`git -c 'alias.wipe=!rm -rf /' wipe`,
		`git --config-env=core.fsmonitor=MONITOR stash`,
		`git --config-env core.fsmonitor=MONITOR stash`,
		`git --exec-path=/tmp/programs stash`,
		`git -p stash`,
		`git --paginate stash`,
		`git -C "$REPO" stash`,
		`git --git-dir=${REPO} stash`,
		`if true; then git -c core.fsmonitor=/tmp/monitor stash; fi`,
		`(git -c core.fsmonitor=/tmp/monitor stash)`,
		`sh -c 'git -c core.fsmonitor=/tmp/monitor stash'`,
		`git -c core.pager=/tmp/pager --help stash`,
	} {
		t.Run(command, func(t *testing.T) {
			result := NewPatternEngine().ClassifyCommand(command, "")
			if !result.Opaque || !result.NeedsApproval || result.IsSafe || result.Tier == "" {
				t.Fatalf("unknown Git code or expansion borrowed SAFE: %+v; normalized=%+v", result, NormalizeCommand(command))
			}
		})
	}
}

func TestGitGlobalOptionsFailClosedWhenAmbiguous(t *testing.T) {
	for _, command := range []string{
		`git -C`, `git --git-dir`, `git --work-tree`, `git -c`, `git --config-env`,
		`git --unknown-option /tmp stash`, `git -C/tmp stash`, `git -ccolor.ui=false stash`,
		`git -- stash`, `git "$OPTIONS" stash`, `git $ACTION --hard`, `git re* --hard`,
		`git -C /tmp`, `git -P`,
		`xargs git -C /tmp`, `xargs git -C`,
	} {
		t.Run(command, func(t *testing.T) {
			result := NewPatternEngine().ClassifyCommand(command, "")
			if !result.NeedsApproval || result.IsSafe || result.Tier == "" {
				t.Fatalf("ambiguous Git invocation escaped classification: %+v", result)
			}
		})
	}
}

func TestGitGlobalOptionsPreserveBenignAndInformationalCommands(t *testing.T) {
	for _, command := range []string{
		`git -C /tmp stash`, `git --no-pager stash`, `git -P --bare stash`,
		`git --git-dir=/tmp/repo.git --work-tree /tmp/work stash`,
	} {
		result := NewPatternEngine().ClassifyCommand(command, "")
		if !result.IsSafe || result.NeedsApproval || result.Opaque || result.ParseError || result.HasUnmatchedSegment {
			t.Errorf("literal Git options changed an ordinary safe command: %q: %+v", command, result)
		}
	}
	for _, command := range []string{
		`git`, `git --help reset --hard`, `git -C /tmp -h push --force`,
		`git -P --version reset --hard`, `git --exec-path reset --hard`,
		`git --html-path reset --hard`, `git --list-cmds=builtins reset --hard`,
		`git log -C /tmp -- reset --hard`,
	} {
		result := NewPatternEngine().ClassifyCommand(command, "")
		if result.NeedsApproval || result.Opaque || result.ParseError {
			t.Errorf("inert Git operands became an executable subcommand: %q: %+v", command, result)
		}
	}
}

func TestGitGlobalOptionsPreserveCommandSpecification(t *testing.T) {
	spec := CommandSpec{
		Raw: "display text is not authoritative in argv mode", Cwd: "/tmp",
		Argv: []string{"/usr/bin/git", "-C", "repository with spaces", "reset", "--hard"},
		Hash: "the-original-reviewed-command-hash",
	}
	before := spec
	before.Argv = append([]string(nil), spec.Argv...)
	result := NewPatternEngine().ClassifyCommandSpec(spec)
	if result.Tier != RiskTierDangerous || !result.NeedsApproval || !reflect.DeepEqual(spec, before) {
		t.Fatalf("classification changed execution input or missed argv: result=%+v, spec=%+v", result, spec)
	}
	command := `git -C 'repository with spaces' reset --hard`
	if normalized := NormalizeCommand(command); normalized.Original != command {
		t.Fatalf("classification changed the original command: %+v", normalized)
	}
}

func TestGitGlobalOptionsRetainCustomRiskPolicies(t *testing.T) {
	engine := NewPatternEngine()
	pattern := `^git\s+-C\s+/production\s+stash$`
	if err := engine.AddPattern(RiskTierCritical, pattern, "production repository requires review", "test"); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		`git -C /production stash`,
		`/usr/bin/git -C /production stash`,
		`sudo -u root git -C /production stash`,
		`if true; then git -C /production stash; fi`,
		`(git -C /production stash)`,
		`sh -c 'git -C /production stash'`,
		`eval 'git -C /production stash'`,
		`printf 'git -C /production stash' | sh`,
		`bash <<< 'git -C /production stash'`,
	} {
		t.Run(command, func(t *testing.T) {
			result := engine.ClassifyCommand(command, "")
			if result.Tier != RiskTierCritical || result.IsSafe || !result.NeedsApproval || result.MinApprovals < 2 || result.MatchedPattern != pattern {
				t.Fatalf("normalizing Git options weakened a custom policy: %+v; normalized=%+v", result, NormalizeCommand(command))
			}
		})
	}
	normalized := NormalizeCommand(`git -C /production stash`)
	if normalized.IsCompound || !reflect.DeepEqual(normalized.Segments, []string{"git stash"}) ||
		!reflect.DeepEqual(normalized.OriginalGitSegments, []string{"git -C /production stash"}) {
		t.Fatalf("original policy view became another executed command: %+v", normalized)
	}
}
