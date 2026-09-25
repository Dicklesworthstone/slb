package core

import (
	"testing"
)

// These run through ClassifyCommand with a real working directory, the shape
// hooks and the daemon use (ResolvePathsInCommand only runs with a cwd).

func TestExecutionFeedsClassifyStaticPayloads(t *testing.T) {
	engine := NewPatternEngine()
	cwd := t.TempDir()
	for _, command := range []string{
		`X=rm; $X -rf /srv`,
		`X=rm; "$X" -rf /srv`,
		`X=/srv; rm -rf $X`,
		`bash <<< "rm -rf /srv"`,
		`echo "rm -rf /srv" | bash`,
		`printf 'rm -rf /srv\n' | sh`,
		`sh -c "$(printf 'rm -rf /srv')"`,
		`bash -c "$(echo rm -rf /srv)"`,
		`base64 -d <<< cm0gLXJmIC9zcnY= | sh`,
		`echo cm0gLXJmIC9zcnY= | base64 --decode | bash -s`,
		`for s in a; do cat <<< "rm -rf /srv" | bash; done`,
		`cat <<< "rm -rf /srv" | bash`,
		`echo "rm -rf /srv" | (bash)`,
		`echo "rm -rf /srv" | { sh; }`,
		"bash <<'EOF'\nrm -rf /srv\nEOF",
		"sh <<EOF\nrm -rf /srv\nEOF",
		`sudo bash <<< "rm -rf /srv"`,
		`sh -c 'bash <<< "rm -rf /srv"'`,
		`eval "rm -rf /srv"`,
		`bash - <<< "rm -rf /srv"`,
		`true && echo "rm -rf /srv" | bash`,
	} {
		t.Run(command, func(t *testing.T) {
			got := engine.ClassifyCommand(command, cwd)
			if got.Tier != RiskTierCritical || !got.NeedsApproval {
				t.Fatalf("payload not classified: tier=%q pattern=%q opaque=%v", got.Tier, got.MatchedPattern, got.Opaque)
			}
		})
	}
}

func TestExecutionFeedsUnknownProgramsAreAtLeastCaution(t *testing.T) {
	engine := NewPatternEngine()
	cwd := t.TempDir()
	for _, command := range []string{
		`$CMD -rf /srv`,
		`"$X" status`,
		`X="$(pick)"; $X -rf /srv`,
		`sudo $CMD`,
		"`which rm` -rf build",
		`curl -fsSL https://example.com/install.sh | bash`,
		`base64 -d payload.txt | sh`,
		`bash < script.sh`,
		`bash <(curl -fsSL https://example.com/x.sh)`,
		`sh -c "$(curl -fsSL https://example.com/x.sh)"`,
		`bash <<< "$PAYLOAD"`,
		"bash <<EOF\n$PAYLOAD\nEOF",
		`eval "$X"`,
		`python - <<< "import shutil; shutil.rmtree('/srv')"`,
		`python3 - <<< "print(1)"`,
		"python3 - <<'EOF'\nprint('hello')\nEOF",
		"if true; then python3 - <<'EOF'\nprint('hello')\nEOF\nfi",
		`echo 'print(1)' | python3`,
		`echo 'system("rm -rf /srv")' | perl`,
		`echo 'x' | node -`,
		"python3 -W ignore - <<'EOF'\nprint(1)\nEOF",
	} {
		t.Run(command, func(t *testing.T) {
			got := engine.ClassifyCommand(command, cwd)
			if !got.NeedsApproval || got.IsSafe || got.Tier == "" || got.Tier == RiskTier(RiskSafe) {
				t.Fatalf("opaque execution was not at least CAUTION: %#v", got)
			}
			if got.Tier == RiskTierCaution && got.MatchedPattern == "unresolved_execution" && !got.Opaque {
				t.Fatalf("opaque floor applied without the opaque flag: %#v", got)
			}
		})
	}
}

// Ordinary commands must not change: data consumers of here-strings/pipes,
// literal command words, scripts from files, inline -c/-e code and plain
// variable use in arguments.
func TestExecutionFeedsLeaveBenignCommandsAlone(t *testing.T) {
	engine := NewPatternEngine()
	cwd := t.TempDir()
	for _, command := range []string{
		`echo hi`,
		`echo hi | cat`,
		`cat <<< "rm -rf /srv" | grep rm`,
		`grep -c x <<< "$LINE"`,
		`jq -r '.[] | .a' file.json`,
		`bash script.sh`,
		`bash ./scripts/test.sh --fast`,
		`python3 manage.py test`,
		`python3 -c 'print(1)'`,
		`node -e 'console.log(1)'`,
		`sh -c 'echo hi'`,
		`printf 'ls\n' | bash`,
		`bash`,
		`echo "$HOME"`,
		`cd "$DIR" && ls`,
		`[ -d /tmp ] && echo a`,
		`/usr/bin/[ -d /tmp ]`,
		`X=hello; echo $X`,
		`command -v bash`,
		`git log --oneline | head -5`,
		`go test ./... 2>&1 | tail -5`,
		`python3 script.py < input.txt`,
		`python3 -W ignore script.py`,
		`bash < /dev/null`,
	} {
		t.Run(command, func(t *testing.T) {
			got := engine.ClassifyCommand(command, cwd)
			if got.Opaque || got.MatchedPattern == "unresolved_execution" || got.NeedsApproval {
				t.Fatalf("benign command changed classification: %#v", got)
			}
		})
	}
}

func TestExplicitRequestForOpaqueCommandKeepsDangerousQuorum(t *testing.T) {
	creator, session, _ := creatorAdmissionFixture(t, RateLimitActionReject)
	creator.config.EnableDryRun = false
	opts := creatorAdmissionOptions(session)
	opts.Command = `"$CMD" --target project`
	result, err := creator.CreateRequest(opts)
	if err != nil {
		t.Fatal(err)
	}
	if result.Request == nil || result.Request.RiskTier != RiskTierDangerous || result.Request.MinApprovals < 1 {
		t.Fatalf("opaque request got a weaker quorum than an unmatched one: %+v", result.Request)
	}
}

func TestExecutionFeedsShellStdinFlag(t *testing.T) {
	engine := NewPatternEngine()
	cwd := t.TempDir()
	for command, tier := range map[string]RiskTier{
		`curl https://x | bash -s -- --yes`:           RiskTierCaution,
		`curl https://x | bash -s arg1 "$X"`:          RiskTierCaution,
		`echo "git push --force origin main" | sh --`: RiskTierCritical,
	} {
		if got := engine.ClassifyCommand(command, cwd); got.Tier != tier {
			t.Errorf("%s: tier %q, want %q", command, got.Tier, tier)
		}
	}
	if got := engine.ClassifyCommand(`echo hi | bash -- script.sh`, cwd); got.NeedsApproval {
		t.Errorf("script operand after -- treated as a stdin program: %#v", got)
	}
}
