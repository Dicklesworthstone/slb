package core

import "testing"

// GitHub #20: valid control-flow scripts that were classified parse_error.
func TestClassifyControlFlowLiteralVariablesAndHereStrings(t *testing.T) {
	engine := NewPatternEngine()
	benign := map[string]string{
		"if condition":     `S=/bin/ls; if $S / | grep -q etc; then echo y; fi`,
		"reporter if":      `S=/home/ubuntu/go/bin/slb; if $S daemon status 2>/dev/null | grep -q x; then echo y; fi`,
		"reporter loop":    `S=/home/ubuntu/go/bin/slb; for i in 1 2; do if $S daemon status 2>/dev/null | grep -q x; then break; fi; done`,
		"braced while":     `S="/bin/ls"; while ${S} /; do break; done`,
		"quoted until":     `S='/bin/true'; until "$S"; do sleep 1; done`,
		"case subject":     `S=/bin/ls; case x in x) $S /;; esac`,
		"read here-string": `for s in "a 1"; do read -r r n <<< "$s"; echo "$r"; done`,
		"reporter gh loop": `for spec in "repo 1" "repo 2"; do read -r r n <<< "$spec"; gh api "repos/x/$r/issues/$n"; done`,
		"grep here-string": `if grep -q x <<< "$v"; then echo y; fi`,
		"cat here-string":  `while true; do cat <<< "$v"; break; done`,
	}
	for name, command := range benign {
		t.Run(name, func(t *testing.T) {
			got := engine.ClassifyCommand(command, "")
			if got.ParseError || got.MatchedPattern == "parse_error" || got.NeedsApproval {
				t.Fatalf("valid script requested approval: %#v\ncommand: %s", got, command)
			}
		})
	}
}

func TestClassifyControlFlowLiteralVariablesExposeDanger(t *testing.T) {
	engine := NewPatternEngine()
	for name, command := range map[string]string{
		"resolved rm":          `X=rm; if true; then $X -rf /srv/data; fi`,
		"resolved quoted path": `X=/bin/rm; for f in a; do "$X" -rf /srv/data; done`,
		"resolved behind sudo": `X=rm; if true; then sudo $X -rf /srv/data; fi`,
		"here-string subst":    `for s in a; do read -r a <<< "$(rm -rf /srv/data)"; done`,
		"arg subst":            `S=/bin/ls; if $S "$(rm -rf /srv/data)"; then :; fi`,
	} {
		t.Run(name, func(t *testing.T) {
			got := engine.ClassifyCommand(command, "")
			if got.Tier != RiskTierDangerous && got.Tier != RiskTierCritical {
				t.Fatalf("destructive command not exposed: %#v\ncommand: %s", got, command)
			}
		})
	}
}

// Anything the resolver cannot prove stays a computed executable, and every
// here-string that might be executed stays fail-closed.
func TestClassifyControlFlowUnprovableStaysParseError(t *testing.T) {
	engine := NewPatternEngine()
	for name, command := range map[string]string{
		"from environment":        `if $S /; then echo y; fi`,
		"assigned after use":      `if $S /; then echo y; fi; S=/bin/ls`,
		"assigned twice":          `S=/bin/ls; S=/bin/rm; if $S /; then :; fi`,
		"read overwrites":         `S=/bin/ls; read S; if $S /; then :; fi`,
		"for overwrites":          `S=/bin/ls; for S in rm; do $S x; done`,
		"default expansion":       `S=/bin/ls; if ${S:-rm} x; then :; fi`,
		"assign expansion":        `S=/bin/ls; if ${T:=rm} x; then :; fi`,
		"mentioned in comment":    "S=/bin/ls; if $S /; then :; fi # S",
		"dynamic printf -v":       `S=/bin/ls; printf -v "$P" rm; if $S x; then :; fi`,
		"dynamic read":            `S=/bin/ls; read "$P" <<< rm; if $S x; then :; fi`,
		"dynamic declare":         `S=/bin/ls; declare "$P=rm"; if $S x; then :; fi`,
		"nameref":                 `S=/bin/ls; declare -n R; if $S x; then :; fi`,
		"ifs":                     `S=/bin/ls; IFS=/; if $S x; then :; fi`,
		"word splitting":          `S="ls -la"; if $S x; then :; fi`,
		"glob value":              `S=ls*; if $S x; then :; fi`,
		"tilde value":             `S=~/bin/tool; if $S x; then :; fi`,
		"expanding value":         `S=$HOME/bin/tool; if $S x; then :; fi`,
		"background assignment":   `S=/bin/ls & if $S x; then :; fi`,
		"conditional assignment":  `true && S=/bin/ls; if $S x; then :; fi`,
		"branch assignment":       `if true; then S=/bin/ls; fi; if $S x; then :; fi`,
		"prefix assignment":       `S=/bin/ls env; if $S x; then :; fi`,
		"eval value":              `S=eval; if $S x; then :; fi`,
		"builtin value":           `S=read; if $S x <<< rm; then :; fi`,
		"interpreter here-string": `for s in a; do bash <<< "echo hi"; done`,
		"sed here-string":         `for s in a; do sed e <<< "echo hi"; done`,
		"awk here-string":         `for s in a; do awk '{system($0)}' <<< "echo hi"; done`,
		"mapfile callback":        `for s in a; do mapfile -C eval -c 1 l <<< "echo hi"; done`,
		"computed here-string":    `for s in a; do $R <<< "x"; done`,
		"wrapped here-string":     `for s in a; do command bash <<< "echo hi"; done`,
		"loop here-string":        `while read -r l; do $l; done <<< "$cmds"`,
	} {
		t.Run(name, func(t *testing.T) {
			got := engine.ClassifyCommand(command, "")
			if !got.ParseError || !got.NeedsApproval {
				t.Fatalf("unprovable script was cleared: %#v\ncommand: %s", got, command)
			}
		})
	}
}

func TestIdentifierCount(t *testing.T) {
	if got := identifierCount(`S=1; $S ${S} $SS S_ x/S/y`, "S"); got != 4 {
		t.Fatalf("identifierCount = %d, want 4", got)
	}
}
