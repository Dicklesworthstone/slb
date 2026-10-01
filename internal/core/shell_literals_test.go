package core

import (
	"strings"
	"testing"

	"mvdan.cc/sh/v3/syntax"
)

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
		"printf no -v":     `S=/bin/ls; if true; then $S /; fi; printf '%s\n' done`,
		"read other name":  `S=/bin/ls; read -r a b <<< "1 2"; if true; then $S /; fi`,
		"command -v check": `S=/bin/ls; if command -v "$S" >/dev/null; then $S /; fi`,
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
		// Names spelled so the identifier never appears in the raw text.
		"escaped printf -v":    `XY=/bin/ls; printf -v X\Y rm; if true; then $XY -rf /srv; fi`,
		"attached printf -v":   `XY=/bin/ls; printf -vXY rm; if true; then $XY -rf /srv; fi`,
		"quoted printf -v":     `XY=/bin/ls; printf -v 'X''Y' rm; if true; then $XY -rf /srv; fi`,
		"escaped read":         `XY=/bin/ls; read X\Y <<< rm; if true; then $XY -rf /srv; fi`,
		"attached read -a":     `XY=/bin/ls; read -aXY <<< rm; if true; then $XY -rf /srv; fi`,
		"brace read":           `XY=/bin/ls; read X{Y,Z} <<< "rm x"; if true; then $XY -rf /srv; fi`,
		"escaped unset":        `XY=/bin/ls; unset X\Y; if true; then $XY rm -rf /srv; fi`,
		"attached wait -p":     `XY=/bin/ls; wait -pXY; if true; then $XY -rf /srv; fi`,
		"line continuation":    "XY=/bin/ls; printf -v X\\\nY rm; if true; then $XY -rf /srv; fi",
		"escaped builtin name": `XY=/bin/ls; pr\intf -v X\Y rm; if true; then $XY -rf /srv; fi`,
		"quoted builtin name":  `XY=/bin/ls; pr""intf -v X\Y rm; if true; then $XY -rf /srv; fi`,
		"wrapped printf":       `XY=/bin/ls; builtin printf -v X\Y rm; if true; then $XY -rf /srv; fi`,
		"wrapped glob builtin": `XY=/bin/ls; builtin prin?f -v X\Y rm; if true; then $XY -rf /srv; fi`,
		"trap action":          `XY=/bin/ls; trap 'printf -v X\Y rm' DEBUG; if true; then $XY -rf /srv; fi`,
		"attached ifs":         `S=/bin/rmX-rfX/srv; printf -vIFS X; if true; then $S; fi`,
		"escaped ifs":          `S=/bin/rmX-rfX/srv; read I\FS <<< X; if true; then $S; fi`,
		"trap value":           `S=trap; if true; then $S x DEBUG; fi`,
		"mapfile -C name":      `XY=/bin/ls; mapfile -C 'printf -v X\Y rm' -c 1 a < /etc/hosts; if true; then $XY -rf /srv; fi`,
		"readarray -tC name":   `XY=/bin/ls; readarray -tC 'printf -v X\Y rm' -c 1 a < /etc/hosts; if true; then $XY -rf /srv; fi`,
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

// GitHub #23: a printf whose options and format are literal cannot assign a
// variable through its later words, whatever they expand to.
func TestPrintfDataOperandsKeepResolution(t *testing.T) {
	engine := NewPatternEngine()
	for name, command := range map[string]string{
		"literal data":        `S=/bin/ls; printf '%s\n' x; if true; then $S /; fi`,
		"expanded data":       `S=/bin/ls; printf '%s\n' "$x"; if true; then $S /; fi`,
		"loop variable data":  `S=/bin/ls; for w in a; do printf '%s\n' "$w"; done; if true; then $S /; fi`,
		"reporter label loop": `B=/tmp/x; for ws in a b; do printf '%s: ' "$ws"; done; "$B/ee" --version`,
		"after double dash":   `S=/bin/ls; printf -- '%s\n' "$x"; if true; then $S /; fi`,
		"dash format":         `S=/bin/ls; printf - "$x"; if true; then $S /; fi`,
		"-v of another name":  `S=/bin/ls; printf -v T '%s' "$x"; if true; then $S /; fi`,
	} {
		t.Run(name, func(t *testing.T) {
			got := engine.ClassifyCommand(command, "")
			if got.ParseError || got.NeedsApproval {
				t.Fatalf("valid script requested approval: %#v\ncommand: %s", got, command)
			}
		})
	}
	got := engine.ClassifyCommand(`X=rm; printf '%s\n' "$x"; if true; then $X -rf /srv/data; fi`, "")
	if got.Tier != RiskTierDangerous && got.Tier != RiskTierCritical {
		t.Fatalf("resolved command not exposed: %#v", got)
	}
}

// Anything that could make printf (or what runs as printf) assign a variable
// still disables resolution.
func TestPrintfThatMightAssignStaysUnresolved(t *testing.T) {
	engine := NewPatternEngine()
	for name, command := range map[string]string{
		"expanded format":         `XY=/bin/ls; printf "$fmt" x; if true; then $XY -rf /srv; fi`,
		"expanded after --":       `XY=/bin/ls; printf -- "$fmt" x; if true; then $XY -rf /srv; fi`,
		"expansion before -v":     `XY=/bin/ls; printf $e -vXY '%s' rm; if true; then $XY -rf /srv; fi`,
		"expanded -v name":        `XY=/bin/ls; printf -v "$n" '%s' rm; if true; then $XY -rf /srv; fi`,
		"expanded attached -v":    `XY=/bin/ls; printf -v"$n" '%s' rm; if true; then $XY -rf /srv; fi`,
		"-v same name":            `XY=/bin/ls; printf -v XY '%s' "$x"; if true; then $XY -rf /srv; fi`,
		"escaped -v name":         `XY=/bin/ls; printf -v X\Y '%s' "$x"; if true; then $XY -rf /srv; fi`,
		"wrapped printf":          `XY=/bin/ls; builtin printf '%s' "$n"; if true; then $XY -rf /srv; fi`,
		"assigner as data":        `XY=/bin/ls; printf '%s' read "$n"; if true; then $XY -rf /srv; fi`,
		"redefined printf":        `printf() { read -r "$2"; }; XY=/bin/ls; printf '%s' "$n"; if true; then $XY -rf /srv; fi`,
		"aliased printf":          "shopt -s expand_aliases\nalias printf='printf -vXY'\nXY=/bin/ls\nprintf '%s' \"$n\"\nif true; then $XY -rf /srv; fi",
		"alias array":             "shopt -s expand_aliases\nBASH_ALIASES[printf]='printf -vXY'\nXY=/bin/ls\nprintf '%s' \"$n\"\nif true; then $XY -rf /srv; fi",
		"arithmetic in data":      `XY=/bin/ls; printf '%s' $(( $n = 1 )); if true; then $XY -rf /srv; fi`,
		"indirect assign in data": `XY=/bin/ls; printf '%s' "${!n:=rm}"; if true; then $XY -rf /srv; fi`,
		"no format":               `XY=/bin/ls; printf -v Z; echo "$n"; if true; then $XY -rf /srv; fi; printf "$n"`,
	} {
		t.Run(name, func(t *testing.T) {
			got := engine.ClassifyCommand(command, "")
			if !got.NeedsApproval || (!got.ParseError && got.MatchedPattern != "unresolved_execution") {
				t.Fatalf("unprovable script was cleared: %#v\ncommand: %s", got, command)
			}
		})
	}
}

func TestPrintfDataStart(t *testing.T) {
	for command, want := range map[string]int{
		`printf '%s' "$x"`:         2,
		`printf -- '%s' "$x"`:      3,
		`printf -v Z '%s' "$x"`:    4,
		`printf -vZ '%s' "$x"`:     3,
		`printf - "$x"`:            2,
		`printf "$f" x`:            -1,
		`printf -- "$f" x`:         -1,
		`printf --`:                -1,
		`printf -v`:                -1,
		`printf`:                   -1,
		`builtin printf '%s' "$x"`: -1,
		`printf '%s' eval "$x"`:    -1,
		`echo '%s' "$x"`:           -1,
	} {
		file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
		if err != nil {
			t.Fatalf("parse %q: %v", command, err)
		}
		call := file.Stmts[0].Cmd.(*syntax.CallExpr)
		if got := printfDataStart(call.Args); got != want {
			t.Errorf("printfDataStart(%s) = %d, want %d", command, got, want)
		}
	}
}
