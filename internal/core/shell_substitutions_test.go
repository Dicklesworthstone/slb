package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSubstitutionsCannotHideCriticalCommands(t *testing.T) {
	for _, command := range []string{
		`echo $(rm -rf /)`,
		`echo "$(rm -rf /)"`,
		`kubectl delete pod "$(rm -rf /)"`,
		`echo "prefix$(echo harmless; rm -rf /)suffix"`,
		`echo "$(printf '%s' "$(rm -rf /)")"`,
		`echo $(echo "a)b"; rm -rf /)`,
		`echo $((1 + $(rm -rf /)))`,
		`echo "$((1 + $(rm -rf /)))"`,
		`echo $((a[$(rm -rf /)] + 1))`,
		"echo `rm -rf /`",
		"echo \"`rm -rf /`\"",
		"echo `echo \\`rm -rf /\\``",
		"echo $((1 + `rm -rf /`))",
		`diff <(rm -rf /) <(cat input.txt)`,
		`cat >(rm -rf /)`,
		`VALUE="$(rm -rf /)" echo ready`,
		`bash -lc 'echo "$(rm -rf /)"'`,
		"echo $(echo ready # ignored ) ' \"\nrm -rf /)",
	} {
		t.Run(command, func(t *testing.T) {
			result := NewPatternEngine().ClassifyCommand(command, "")
			if result.Tier != RiskTierCritical || !result.NeedsApproval || result.IsSafe || result.MinApprovals < 2 {
				t.Fatalf("executable substitution bypassed classification: %+v; normalized=%+v", result, NormalizeCommand(command))
			}
		})
	}
}

func TestLiteralSubstitutionExamplesStayInert(t *testing.T) {
	for _, command := range []string{
		`echo '$(rm -rf /)'`,
		`echo '$((1 + $(rm -rf /)))'`,
		"echo '`rm -rf /`'",
		`echo "\$(rm -rf /)"`,
		`echo '<(rm -rf /)'`,
		`echo "<(rm -rf /)"`,
		`echo ready # $(rm -rf /)`,
		`echo $((1 + (2 * 3)))`,
		`i=$((i + 1))`,
		`printf '%s' "$(git status --porcelain)"`,
		`echo "$(printf '%s' 'rm -rf /')"`,
	} {
		result := NewPatternEngine().ClassifyCommand(command, "")
		if result.NeedsApproval || result.ParseError {
			t.Errorf("literal or harmless substitution was blocked: %q: %+v", command, result)
		}
	}
}

func TestSubstitutionScannerNeverExecutesCommands(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	for _, command := range []string{
		"echo $(touch " + marker + ")",
		"echo `touch " + marker + "`",
		"echo $(( $(touch " + marker + ") + 1 ))",
	} {
		_ = NewPatternEngine().ClassifyCommand(command, "")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("static classification executed a command: %v", err)
	}
}

func TestMalformedSubstitutionsFailConservatively(t *testing.T) {
	for _, command := range []string{
		"echo $(unterminated", "echo `unterminated", "echo $((1 + 2)",
		"echo \"$(echo 'unterminated)\"", "echo \x00",
		strings.Repeat("echo $(", maxCommandNesting+5) + "echo ready" + strings.Repeat(")", maxCommandNesting+5),
	} {
		result := NewPatternEngine().ClassifyCommand(command, "")
		if !result.ParseError || !result.NeedsApproval || result.IsSafe {
			t.Errorf("malformed executable syntax silently allowed: %q: %+v", command, result)
		}
	}
}

func FuzzNormalizeExecutableSyntax(f *testing.F) {
	for _, seed := range []string{
		`echo "$(echo "$(rm -rf /)")"`, "echo `echo \\`date\\``",
		"printf ready # ' (\nrm -rf /", `sudo -u root sh -lc 'echo ok; rm -rf /'`,
		`echo $((a[$(date)] + 1))`, `jq -r '.[] | .a' file.json`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 16*1024 {
			t.Skip()
		}
		result := NormalizeCommand(raw)
		if result == nil || result.Original != raw {
			t.Fatal("normalization changed the original execution input")
		}
		if result.IsCompound != (len(result.Segments) > 1) {
			t.Fatal("inconsistent compound-command metadata")
		}
	})
}
