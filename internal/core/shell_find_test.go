package core

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindActionsPreserveRisk(t *testing.T) {
	engine := NewPatternEngine()
	for _, tc := range []struct {
		command string
		tier    RiskTier
	}{
		{`find / -delete`, RiskTierCritical},
		{`find /srv -type f -delete`, RiskTierCritical},
		{`find . -name '*.tmp' -delete`, RiskTierDangerous},
		{`find -delete`, RiskTierDangerous},
		{`find -H /etc -delete`, RiskTierCritical},
		{`find -f /etc -delete`, RiskTierCritical},
		{`find -L /home/ubuntu -depth -delete`, RiskTierCritical},
		{`find . /etc -delete`, RiskTierCritical},
		{`find -O2 -D tree /srv -mindepth 1 -delete`, RiskTierCritical},
		{`find . -files0-from paths -delete`, RiskTierDangerous},
		{`/usr/bin/find / -delete`, RiskTierCritical},
		{`sudo -n find / -delete`, RiskTierCritical},
		{`if true; then find / -delete; fi`, RiskTierCritical},
		{`sh -c 'find / -delete'`, RiskTierCritical},
		{`find /srv -type f -exec rm -f '{}' +`, RiskTierCaution},
		{`find . -exec rm -rf / '{}' +`, RiskTierCritical},
		{`find / -exec rm -rf '{}' +`, RiskTierCritical},
		{`find /srv -exec sudo rm -rf '{}' \;`, RiskTierCritical},
		{`find /tmp/project -exec rm -rf '{}/../..' \;`, RiskTierCritical},
		{`find /tmp/project -execdir rm -rf ../.. \;`, RiskTierCritical},
		{`find /home/alice/project -execdir rm -rf .. \;`, RiskTierCritical},
		{`find / -execdir rm -rf /tmp/build \;`, RiskTierDangerous},
		{`find / -exec rm -f '{}' +`, RiskTierCaution},
		{`find /etc/"$TARGET" -delete`, RiskTierCritical},
		{`find /etc/* -delete`, RiskTierCritical},
		{`find "$HOME" -delete`, RiskTierCritical},
		{`find "${HOME}" -delete`, RiskTierCritical},
		{`find ""$HOME -delete`, RiskTierCritical},
		{`find "$HOME/build" -delete`, RiskTierDangerous},
		{`find "$HOME_BACKUP" -delete`, RiskTierDangerous},
		{`find "${HOME}suffix" -delete`, RiskTierDangerous},
		{`find "${HOME:-fallback}" -delete`, RiskTierDangerous},
		{`find /tmp/"$TARGET" -execdir rm -rf .. \;`, RiskTierCritical},
		{`find . -exec sh -c 'git push --force origin main' \;`, RiskTierCritical},
		{`find . -exec sh -c 'git --no-pager push --force origin main' \;`, RiskTierCritical},
		{`find . -ok git reset --hard \;`, RiskTierDangerous},
		{`find . -okdir git reset --hard \;`, RiskTierDangerous},
		{`find . -execdir rm -rf build '{}' +`, RiskTierDangerous},
		{`find . -exec rm -f '{}.log' \;`, RiskTierCaution},
		{`find . -exec kubectl delete pod '{}' +`, RiskTierDangerous},
		{`find . -exec echo 'rm -rf /' \;`, RiskTierCaution},
		{`find . -exec printf %s + -delete \;`, RiskTierCaution},
		{`find . -exec echo '{}' + -delete`, RiskTierDangerous},
		{`find . -exec sh -c 'echo {}; rm -rf /' \;`, RiskTierCritical},
		{`find . -exec find / -delete \;`, RiskTierCritical},
		{`find . -false -fprint results`, RiskTierCaution},
		{`find . -fprint0 results`, RiskTierCaution},
		{`find . -fls results`, RiskTierCaution},
		{`find . -fprintf results '%p\n'`, RiskTierCaution},
	} {
		t.Run(tc.command, func(t *testing.T) {
			got := engine.ClassifyCommand(tc.command, "")
			if got.Tier != tc.tier || !got.NeedsApproval || got.IsSafe || got.ParseError {
				t.Fatalf("find action escaped risk: %+v; normalized=%+v, want %s", got, NormalizeCommand(tc.command), tc.tier)
			}
			if got.MinApprovals != tierApprovals(tc.tier) {
				t.Fatalf("approvals=%d, want %d", got.MinApprovals, tierApprovals(tc.tier))
			}
		})
	}
}

func TestFindPredicateOperandsStayInert(t *testing.T) {
	for _, command := range []string{
		`find .`,
		`find . -name '-delete'`,
		`find . -name -exec`,
		`find . -path '-fprint'`,
		`find . -regex '-execdir'`,
		`find . -printf '-delete'`,
		`find . -newermt '-delete'`,
		`find . -name '*.log' -type f -print`,
		`find . \( -name '-delete' -o -name '-exec' \) -print`,
		`find . ! -name '-delete' -print0`,
		`find ./-delete -maxdepth 1 -ls`,
		`find . -fprintf /dev/null '-delete'`,
		`find . -fprint /dev/stdout`,
		`echo 'find / -delete'`,
		`printf '%s' 'find . -exec rm -rf / ;'`,
	} {
		t.Run(command, func(t *testing.T) {
			got := NewPatternEngine().ClassifyCommand(command, "")
			if got.NeedsApproval || got.ParseError || got.Opaque {
				t.Fatalf("inert find operand treated as an action: %+v; normalized=%+v", got, NormalizeCommand(command))
			}
		})
	}
}

func TestFindActionsCannotBorrowSafeRules(t *testing.T) {
	engine := NewPatternEngine()
	if err := engine.AddPattern(RiskTier(RiskSafe), `^find(?:\s|$)`, "broad project find rule", "test"); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		`find / -delete`,
		`find . -exec git stash \;`,
		`find . -exec unknown-tool \;`,
		`find . -exec "$TOOL" '{}' \;`,
		`find . -exec sh -c "$CODE" \;`,
		`find . -exec echo '{}'`,
		`find . -exec \;`,
		`find . -exec rm -f '{}.log' \;`,
		`find . -fprint results`,
		`find . -fprintf results`,
		`find . -name`,
		`find . -unsupported -delete`,
		`find . "$EXPRESSION"`,
		`find . -name $PATTERN`,
		`find -files0-from paths -print`,
	} {
		t.Run(command, func(t *testing.T) {
			got := engine.ClassifyCommand(command, "")
			if !got.NeedsApproval || got.IsSafe {
				t.Fatalf("find action or unknown input borrowed SAFE: %+v", got)
			}
		})
	}
	got := engine.ClassifyCommand(`find . -name '-delete'`, "")
	if !got.IsSafe || got.NeedsApproval {
		t.Fatalf("inert find command lost the project SAFE rule: %+v", got)
	}
}

func TestFindAnalysisNeverExecutesPayload(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	command := `find . -exec sh -c ` + shellQuote("echo marker > "+shellQuote(marker)+"; git push --force origin main") + ` \;`
	got := NewPatternEngine().ClassifyCommand(command, "")
	if got.Tier != RiskTierCritical || NormalizeCommand(command).Original != command {
		t.Fatalf("analysis lost risk or changed original command: %+v", got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("classification executed a payload: %v", err)
	}
}

func TestFindGeneratedPayloadIsBounded(t *testing.T) {
	// A modest input can repeat actions over the same large list of roots.
	// Analysis must stop rather than multiplying that list for every action.
	command := "find " + strings.Repeat("/tmp/path ", 20000) + strings.Repeat(` -exec rm -rf '{}' +`, 10)
	got := NewPatternEngine().ClassifyCommand(command, "")
	if !got.ParseError || !got.NeedsApproval || got.IsSafe {
		t.Fatalf("excessive generated find analysis did not fail closed: %+v", got)
	}
}

func TestFindRootRiskSurvivesSafePathNormalization(t *testing.T) {
	engine := NewPatternEngine()
	if err := engine.AddPattern(RiskTier(RiskSafe), `^rm\s`, "broad project rm rule", "test"); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		`find / -exec rm -rf '{}' +`,
		`find /tmp/project -exec rm -rf '{}/../..' \;`,
		`find /tmp/project -execdir rm -rf ../.. \;`,
	} {
		got := engine.ClassifyCommand(command, "")
		if got.Tier != RiskTierCritical || got.IsSafe || !got.NeedsApproval {
			t.Errorf("root-dependent removal borrowed SAFE after path cleaning: %q: %+v", command, got)
		}
	}
}

func TestExportClaudeHookFindNeedsNativeAnalysis(t *testing.T) {
	python := findPython3(t)
	engine := NewPatternEngine()
	if err := engine.AddPattern(RiskTier(RiskSafe), `^(?:/usr/bin/)?find(?:\s|$)`, "broad project find rule", "test"); err != nil {
		t.Fatal(err)
	}
	// The offline policy intentionally does not duplicate find's expression
	// parser. Even literal argv can execute actions, so a SAFE prefix cannot
	// authorize them while native analysis is unavailable.
	commands := []string{
		`find / -delete`, `/usr/bin/find / -delete`, `find . -fprint results`,
		`find . -exec sh -c 'rm -rf /' \;`, `find . -name -delete`,
	}
	input, err := json.Marshal(commands)
	if err != nil {
		t.Fatal(err)
	}
	program := engine.ExportClaudeHook() + "\nimport json, sys\nprint(json.dumps([is_blocked(command)[0] for command in json.load(sys.stdin)]))\n"
	cmd := exec.Command(python, "-c", program)
	cmd.Stdin = strings.NewReader(string(input))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("exported find guard failed: %v\n%s", err, out)
	}
	var blocked []bool
	if err := json.Unmarshal(out, &blocked); err != nil || len(blocked) != len(commands) {
		t.Fatalf("invalid find guard output: %s, %v", out, err)
	}
	for i, command := range commands {
		if !blocked[i] {
			t.Errorf("offline SAFE rule allowed unparsed find actions: %q", command)
		}
	}
}
