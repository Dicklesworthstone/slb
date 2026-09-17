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
