package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassifyCaseStatements(t *testing.T) {
	engine := NewPatternEngine()
	tests := map[string]string{
		"reporter":             `case "$x" in a*) echo a;; esac`,
		"alternatives":         `case "$x" in a*|b*) echo ab;; *) printf other;; esac`,
		"optional parenthesis": `case "$x" in (a|b) echo ab;; (*) echo other;; esac`,
		"quoted patterns":      `case "$x" in 'a)|b'|"esac") echo ';; esac | )';; esac`,
		"bracket patterns":     `case "$x" in [a-z]*) echo letter;; esac`,
		"extended patterns":    `case "$x" in @(a|b)) echo ab;; esac`,
		"empty case":           `case "$x" in esac`,
		"empty arms":           `case "$x" in a) ;; b) ;; esac`,
		"no final terminator":  `case "$x" in a) echo a; esac`,
		"fallthrough":          `case "$x" in a) echo a ;& b) echo b ;;& *) echo other;; esac`,
		"nested":               `case "$x" in a) case "$y" in b) echo b;; esac;; esac`,
		"conditionals":         `if true; then case "$x" in a) if true; then echo a; fi;; esac; fi`,
		"test builtin":         `case "$x" in a) [ -d /tmp ] && echo a;; esac`,
		"test executable":      `case "$x" in a) /usr/bin/[ -d /tmp ] && echo a;; esac`,
		"loop":                 `for d in /proc/[0-9]*; do case "$d" in */1) echo "$d";; esac; done`,
		"compound":             `echo before && case "$x" in a) echo a | cat;; esac; echo after`,
		"subshell":             `(case "$x" in a) echo a;; esac)`,
		"shell wrapper":        `sudo bash -lc 'case "$x" in a*) echo a;; esac'`,
		"command substitution": `echo "$(case "$x" in a) echo a;; esac)"`,
		"process substitution": `cat <(case "$x" in a) echo a;; esac)`,
		"selector expansion":   `case "$(printf a)" in a) echo a;; esac`,
		"pattern expansion":    `case "$x" in "$(printf a)") echo a;; esac`,
		"selector slice":       `case "${x:0:1}" in a) echo a;; esac`,
		"slice expansion":      `case "${x:$(printf 0):$(printf 1)}" in a) echo a;; esac`,
		"arithmetic":           `case "$x" in a) echo "$((1+2))";; esac`,
		"fd redirects":         `case "$x" in a) echo a 2>/dev/null;; esac 2>&1`,
		"input redirect":       `case "$x" in a) cat </proc/1/status;; esac`,
		"quoted data":          `echo 'case "$x" in a*) echo a;; esac'`,
		"multiline":            "case \"$x\" in\n# ) | ;; ' \" are comment text\na*)\n echo a\n ;;\n*) echo other;;\nesac",
		"continued keyword":    "ca\\\nse \"$x\" in a) echo a;; esac",
	}
	for name, command := range tests {
		t.Run(name, func(t *testing.T) {
			got := engine.ClassifyCommand(command, "")
			if got.ParseError || got.MatchedPattern == "parse_error" || got.NeedsApproval || got.MinApprovals != 0 {
				t.Fatalf("benign case command requested approval: %#v\ncommand: %s", got, command)
			}
			if got.Tier != "" && got.Tier != RiskTier(RiskSafe) {
				t.Fatalf("benign case command has risk tier %q", got.Tier)
			}
		})
	}
}

func TestClassifyCaseStatementsPreservesDanger(t *testing.T) {
	engine := NewPatternEngine()
	tests := map[string]string{
		"first arm":             `case "$x" in a) rm -rf /;; esac`,
		"later arm":             `case "$x" in a) ls;; *) rm -rf /;; esac`,
		"unselected arm":        `case a in a) ls;; b) rm -rf /;; esac`,
		"nested":                `case "$x" in a) case "$y" in b) rm -rf /;; esac;; esac`,
		"conditional":           `case "$x" in a) if true; then rm -rf /; fi;; esac`,
		"loop":                  `case "$x" in a) for d in /tmp; do rm -rf /; done;; esac`,
		"selector":              `case "$(rm -rf /)" in a) ls;; esac`,
		"pattern":               `case "$x" in "$(rm -rf /)") ls;; esac`,
		"selector slice offset": `case "${x:$(rm -rf /)}" in a) ls;; esac`,
		"selector slice length": `case "${x:0:$(rm -rf /)}" in a) ls;; esac`,
		"pattern slice":         `case "$x" in "${y:$(rm -rf /)}") ls;; esac`,
		"case in slice":         `echo "${x:$(case a in a) rm -rf /;; esac)}"`,
		"backtick pattern":      "case \"$x\" in \"`rm -rf /`\") ls;; esac",
		"body substitution":     `case "$x" in a) echo "$(rm -rf /)";; esac`,
		"arithmetic expansion":  `case "$x" in a) echo "$((1+$(rm -rf /)))";; esac`,
		"parameter expansion":   `case "$x" in a) echo "${v:-$(rm -rf /)}";; esac`,
		"case in substitution":  `echo "$(case "$x" in a) rm -rf /;; esac)"`,
		"process substitution":  `cat <(case "$x" in a) rm -rf /;; esac)`,
		"redirect expansion":    `case "$x" in a) cat <"$(rm -rf /)";; esac`,
		"redirect between args": `case "$x" in a) echo <"$(case x in x) rm -rf /;; esac)" ok;; esac`,
		"assignment":            `case "$x" in a) v=$(rm -rf /);; esac`,
		"declaration":           `case "$x" in a) export v=$(rm -rf /);; esac`,
		"function":              `f() { rm -rf /; }; case "$x" in a) f;; esac`,
		"wrapper":               `case "$x" in a) sudo -u root /bin/rm -rf /;; esac`,
		"shell wrapper":         `sh -c 'case "$x" in a) rm -rf /;; esac'`,
		"subshell":              `case "$x" in a) (rm -rf /);; esac`,
		"before case":           `rm -rf /; case "$x" in a) echo a;; esac`,
		"after case":            `case "$x" in a) echo a;; esac; rm -rf /`,
		"pipeline":              `case "$x" in a) echo a;; esac | sh -c 'rm -rf /'`,
		"fallthrough":           `case "$x" in a) ls ;& b) rm -rf /;; esac`,
		"resume":                `case "$x" in a) ls ;;& b) rm -rf /;; esac`,
		"heredoc body":          "case \"$x\" in a) sh <<'EOF'\nrm -rf /\nEOF\n;; esac",
	}
	for name, command := range tests {
		t.Run(name, func(t *testing.T) {
			got := engine.ClassifyCommand(command, "")
			if got.ParseError || got.Tier != RiskTierCritical || !got.NeedsApproval || got.IsSafe || got.MinApprovals < 1 {
				t.Fatalf("destructive case command lost its classification: %#v\ncommand: %s", got, command)
			}
		})
	}
}

func TestClassifyCaseUnresolvedExecutionFailClosed(t *testing.T) {
	engine := NewPatternEngine()
	for _, command := range []string{
		`case "$x" in @($(rm -rf /)|a)) echo a;; esac`,
		"case \"$x\" in @(`rm -rf /`|a)) echo a;; esac",
		`case "$x" in a) "$cmd" -rf /;; esac`,
		`case "$x" in a) sudo "$cmd" -rf /;; esac`,
		`case "$x" in a) "$(printf rm)" -rf /;; esac`,
		`case "$x" in a) r{m,mdir} -rf /;; esac`,
		`case "$x" in a) $'r\x6d' -rf /;; esac`,
		`case "$x" in a) eval 'rm -rf /';; esac`,
		`case "$x" in a) builtin eval "$script";; esac`,
		`case "$x" in a) source "$script";; esac`,
		`case "$x" in a) . "$script";; esac`,
		`case "$x" in a) exec "$cmd";; esac`,
	} {
		t.Run(command, func(t *testing.T) {
			got := engine.ClassifyCommand(command, "")
			if !got.ParseError || !got.NeedsApproval || got.IsSafe {
				t.Fatalf("unresolved execution in case failed open: %#v", got)
			}
		})
	}
}

func TestClassifyMalformedCaseStatementsFailClosed(t *testing.T) {
	engine := NewPatternEngine()
	commands := []string{
		`case`,
		`case "$x"`,
		`case "$x" a) echo a;; esac`,
		`case "$x" in a echo a;; esac`,
		`case "$x" in a) echo a;;`,
		`case "$x" in a) echo a; b) echo b;; esac`,
		`case "$x" in a) echo "unterminated;; esac`,
		`case "$x" in a) ls && ;; esac`,
		`case "$x" in a) ls | ;; esac`,
		`case "$x" in a) ls > ;; esac`,
		`case "$x" in a) ls;; esac esac`,
		`case "$x" in a) ls;; esac echo after`,
		`case "$x" in a) case "$y" in b) ls;; esac`,
		`case "$(echo x" in a) ls;; esac`,
		"case x in a) python3 - <<'EOF'\nprint(1)\n;; esac",
		"case x in a) echo \x00;; esac",
	}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			got := engine.ClassifyCommand(command, "")
			if !got.ParseError || !got.NeedsApproval || got.IsSafe || got.Tier == "" || got.Tier == RiskTier(RiskSafe) {
				t.Fatalf("malformed case command failed open: %#v", got)
			}
		})
	}
	got := engine.ClassifyCommand(`rm -rf /; case "$x" in`, "")
	if !got.ParseError || got.Tier != RiskTierCritical || !got.NeedsApproval {
		t.Fatalf("syntax error discarded stronger risk match: %#v", got)
	}
}

func TestClassifyCaseUnsupportedWritesFailClosed(t *testing.T) {
	engine := NewPatternEngine()
	for _, suffix := range []string{`>/etc/passwd`, `>>/etc/passwd`, `<>/etc/passwd`, `&>/etc/passwd`, `>|/etc/passwd`, `>&/etc/passwd`, `>&"$fd"`, `<<<'rm -rf /'`} {
		got := engine.ClassifyCommand(`case "$x" in a) ls;; esac `+suffix, "")
		if !got.ParseError || !got.NeedsApproval || got.IsSafe {
			t.Fatalf("unmodeled file write %q failed open: %#v", suffix, got)
		}
	}
	got := engine.ClassifyCommand("case x in a) sh <<EOF\n$(rm -rf /)\nEOF\n;; esac", "")
	if !got.ParseError || !got.NeedsApproval || got.IsSafe {
		t.Fatalf("expanding heredoc lost fail-closed behavior: %#v", got)
	}
}

func TestClassifyCaseIsStaticAndBounded(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	command := `case "$(touch ` + shellQuote(marker) + `)" in "$(touch ` + shellQuote(marker) + `)") echo ok;; esac`
	NewPatternEngine().ClassifyCommand(command, "")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("classification executed a substitution: %v", err)
	}
	for _, command := range []string{
		strings.Repeat("case x in x) ", maxCommandNesting+1) + "ls" + strings.Repeat(";; esac", maxCommandNesting+1),
		"case x in x) echo " + strings.Repeat("x", 1<<20) + ";; esac",
	} {
		got := NewPatternEngine().ClassifyCommand(command, "")
		if !got.ParseError || !got.NeedsApproval || got.IsSafe {
			t.Fatalf("case parsing limit failed open: %#v", got)
		}
	}
}

// The same executable must keep its classification wherever shell control
// flow places it. These fixtures are inputs to the classifier, never executed.
func TestClassifyControlFlowPreservesCommandRisk(t *testing.T) {
	engine := NewPatternEngine()
	templates := map[string]string{
		"then body":          `if true; then __COMMAND__; fi`,
		"else body":          `if true; then :; else __COMMAND__; fi`,
		"elif body":          `if false; then :; elif true; then __COMMAND__; fi`,
		"if condition":       `if __COMMAND__; then :; fi`,
		"elif condition":     `if false; then :; elif __COMMAND__; then :; fi`,
		"for body":           `for item in a b; do __COMMAND__; done`,
		"empty for list":     `for item in; do __COMMAND__; done`,
		"for word expansion": `for item in "$(__COMMAND__)"; do :; done`,
		"while body":         `while false; do __COMMAND__; done`,
		"while condition":    `while __COMMAND__; do break; done`,
		"until body":         `until true; do __COMMAND__; done`,
		"until condition":    `until __COMMAND__; do break; done`,
		"select body":        `select item in a; do __COMMAND__; break; done`,
		"arithmetic loop":    `for ((i=0; i<1; i++)); do __COMMAND__; done`,
		"loop initializer":   `for ((i=$(__COMMAND__); i<1; i++)); do :; done`,
		"loop condition":     `for ((i=0; $(__COMMAND__); i++)); do :; done`,
		"loop increment":     `for ((i=0; i<1; i+=$(__COMMAND__))); do :; done`,
		"block":              `{ __COMMAND__; }`,
		"function body":      `f() { __COMMAND__; }`,
		"function call":      `f() { __COMMAND__; }; f`,
		"function keyword":   `function f { __COMMAND__; }; f`,
		"function subshell":  `f() ( __COMMAND__ ); f`,
		"negation":           `! __COMMAND__`,
		"negated pipeline":   `! __COMMAND__ | cat`,
		"time clause":        `time __COMMAND__`,
		"coprocess":          `coproc __COMMAND__`,
		"named coprocess":    `coproc WORKER { __COMMAND__; }`,
		"arithmetic command": `(( 1 + $(__COMMAND__) ))`,
		"test clause":        `[[ "$(__COMMAND__)" = ok ]]`,
		"let clause":         `let "i=$(__COMMAND__)"`,
		"nested control":     `for item in a; do if true; then { __COMMAND__; }; fi; done`,
		"subshell":           `(if true; then __COMMAND__; fi)`,
		"shell wrapper":      `sudo bash -lc 'if true; then __COMMAND__; fi'`,
		"substitution":       `echo "$(if true; then __COMMAND__; fi)"`,
		"parameter slice":    `echo "${value:$(if true; then __COMMAND__; fi)}"`,
		"process input":      `cat <(if true; then __COMMAND__; fi)`,
		"redirect expansion": `if true; then cat <"$(__COMMAND__)"; fi`,
		"descriptor output":  `if true; then __COMMAND__ 2>/dev/null; fi 2>&1`,
		"continued keyword":  "i\\\nf true; then __COMMAND__; fi",
		"multiline":          "if true\nthen\n# fi ' \" are only comment text\n__COMMAND__\nfi",
	}
	for _, command := range []string{"echo harmless", "git stash", "git stash drop", "git reset --hard", "rm -rf /"} {
		want := engine.ClassifyCommand(command, "")
		for name, template := range templates {
			t.Run(command+"/"+name, func(t *testing.T) {
				script := strings.ReplaceAll(template, "__COMMAND__", command)
				got := engine.ClassifyCommand(script, "")
				if got.ParseError || got.Tier != want.Tier || got.NeedsApproval != want.NeedsApproval ||
					got.IsSafe != want.IsSafe || got.MinApprovals != want.MinApprovals {
					t.Fatalf("control flow changed command risk:\nscript: %s\ngot: %#v\nwant: %#v", script, got, want)
				}
				// An unrelated no-op case must not decide whether other shell
				// control structures are inspected (the original dispatch gap).
				withCase := engine.ClassifyCommand("case x in esac; "+script, "")
				if got.ParseError != withCase.ParseError || got.Tier != withCase.Tier ||
					got.NeedsApproval != withCase.NeedsApproval || got.MinApprovals != withCase.MinApprovals {
					t.Fatalf("unrelated case changed classification: plain=%#v, with case=%#v", got, withCase)
				}
			})
		}
	}
}

func TestClassifyControlFlowLiteralData(t *testing.T) {
	engine := NewPatternEngine()
	for _, script := range []string{
		`echo 'if true; then rm -rf /; fi'`,
		`if true; then echo 'for item in a; do rm -rf /; done'; fi`,
		`for item in 'rm -rf /'; do printf '%s' "$item"; done`,
		`[[ 'rm -rf /' = "$value" ]]`,
		`if [ -d /tmp ]; then printf '%s' "${value#/tmp/}"; fi`,
	} {
		t.Run(script, func(t *testing.T) {
			got := engine.ClassifyCommand(script, "")
			if got.ParseError || got.NeedsApproval || got.Tier != "" || got.MinApprovals != 0 {
				t.Fatalf("literal data or read-only control flow requested approval: %#v", got)
			}
		})
	}
}

func TestClassifyControlFlowFailClosed(t *testing.T) {
	engine := NewPatternEngine()
	for _, script := range []string{
		`if true; echo ok; fi`,
		`if true; then echo ok`,
		`if true; then echo ok; fi echo after`,
		`for item in a; echo ok; done`,
		`for item in a; do echo ok`,
		`while true; echo ok; done`,
		`until true; do echo ok`,
		`{ echo ok;`,
		`f() { echo ok;`,
		`if true; then echo ok |; fi`,
		`echo ok &&`,
		`echo ok |`,
		`echo ok >`,
		`if true; then sudo "$cmd"; fi`,
		`for item in a; do eval "$script"; done`,
		`f() { source "$script"; }; f`,
		`{ . "$script"; }`,
		`! "$(printf rm)" -rf /`,
		`if true; then echo ok; fi >/etc/passwd`,
		`for item in a; do echo ok; done >&"$fd"`,
		`if true; then bash <<<'rm -rf /'; fi`,
		"if true; then sh <<EOF\n$(rm -rf /)\nEOF\nfi",
	} {
		t.Run(script, func(t *testing.T) {
			got := engine.ClassifyCommand(script, "")
			if !got.ParseError || !got.NeedsApproval || got.IsSafe || got.Tier == "" {
				t.Fatalf("malformed or unresolved control flow failed open: %#v", got)
			}
		})
	}
	for _, script := range []string{`rm -rf /; if true; then`, `rm -rf /; for item in a; do`} {
		got := engine.ClassifyCommand(script, "")
		if !got.ParseError || got.Tier != RiskTierCritical || !got.NeedsApproval {
			t.Fatalf("syntax failure discarded an existing critical match: %#v", got)
		}
	}
}

func TestClassifyControlFlowSafeConditionCannotHideDanger(t *testing.T) {
	engine := NewPatternEngine()
	if err := engine.AddPattern(RiskTier(RiskSafe), `^echo\s`, "read-only echo", "human"); err != nil {
		t.Fatal(err)
	}
	for _, script := range []string{
		`if echo ready; then rm -rf /; fi`,
		`while echo ready; do rm -rf /; done`,
		`for item in a; do echo ready && rm -rf /; done`,
		`echo ready; f() { rm -rf /; }; f`,
	} {
		got := engine.ClassifyCommand(script, "")
		if got.ParseError || got.Tier != RiskTierCritical || got.IsSafe || !got.NeedsApproval || got.MinApprovals < 2 {
			t.Fatalf("safe condition/body hid a critical executable: %#v", got)
		}
	}
}

func TestClassifyControlFlowIsStaticAndBounded(t *testing.T) {
	engine := NewPatternEngine()
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	for _, template := range []string{
		`if touch __MARKER__; then :; fi`,
		`for item in "$(touch __MARKER__)"; do :; done`,
		`f() { touch __MARKER__; }; f`,
		`coproc touch __MARKER__`,
		`[[ "$(touch __MARKER__)" = ok ]]`,
	} {
		engine.ClassifyCommand(strings.ReplaceAll(template, "__MARKER__", shellQuote(marker)), "")
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("classification executed a command: %v", err)
		}
	}
	for _, script := range []string{
		strings.Repeat("if true; then ", maxCommandNesting+1) + ":" + strings.Repeat("; fi", maxCommandNesting+1),
		strings.Repeat("for item in a; do ", maxCommandNesting+1) + ":" + strings.Repeat("; done", maxCommandNesting+1),
		"if true; then echo " + strings.Repeat("x", 1<<20) + "; fi",
	} {
		got := engine.ClassifyCommand(script, "")
		if !got.ParseError || !got.NeedsApproval || got.IsSafe {
			t.Fatalf("control-flow parsing limit failed open: %#v", got)
		}
	}
}
