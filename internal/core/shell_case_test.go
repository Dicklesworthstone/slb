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
		"quoted heredoc":       "case \"$x\" in a) python3 - <<'EOF'\nprint(1)\nEOF\n;; esac",
		"heredoc regression":   "python3 - <<'EOF'\nprint('case is data, not shell syntax')\nEOF",
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
