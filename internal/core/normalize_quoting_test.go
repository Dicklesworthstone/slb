package core

import "testing"

// GitHub #14: a parse failure is upgraded to CAUTION, and the hook turns
// CAUTION into a human approval prompt. Three ordinary shell constructs were
// failing to parse, so read-only commands stopped sessions waiting for
// someone to approve them:
//
//   - a `|` inside quotes, because pipes were split with a regex while every
//     other separator was already quote-aware;
//   - `$((...))`, which the tokenizer cannot read at all;
//   - `( ... )`, likewise — and that one also *lowered* the tier of a
//     dangerous command, because the parse-failure upgrade only promotes an
//     unclassified command to CAUTION.
//
// The last is the reason these are tested by classification and not only by
// "does it parse": the fix has to keep a dangerous command dangerous.

func TestQuotedSeparatorsDoNotSplitSegments(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  string
		want []string
	}{
		{"pipe in single quotes", `jq -r '.[] | .a' f.json`, []string{`jq -r .[] | .a f.json`}},
		{"pipe in double quotes", `echo "a | b"`, []string{`echo a | b`}},
		{"real pipe still splits", `ls | wc -l`, []string{"ls", "wc -l"}},
		{"real pipe after quoted one", `jq -r '.a | .b' f | wc -l`, []string{`jq -r .a | .b f`, "wc -l"}},
		{"semicolon in quotes", `grep 'a;b' f`, []string{`grep a;b f`}},
		{"paren in quotes", `echo "(" `, []string{`echo (`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Segments hold the normalized (tokenised, quote-stripped) form,
			// so the expectations below are post-normalisation.
			got := NormalizeCommand(tc.cmd)
			if got.ParseError {
				t.Fatalf("NormalizeCommand(%q) reported a parse error", tc.cmd)
			}
			if len(got.Segments) != len(tc.want) {
				t.Fatalf("segments = %q, want %q", got.Segments, tc.want)
			}
			for i := range tc.want {
				if got.Segments[i] != tc.want[i] {
					t.Fatalf("segments = %q, want %q", got.Segments, tc.want)
				}
			}
		})
	}
}

func TestArithmeticExpansionParses(t *testing.T) {
	for _, cmd := range []string{
		`echo $((1+2))`,
		`echo $(( 1 + 2 ))`,
		`n=$((x+1))`,
		`echo $((x)) $((y))`,
	} {
		t.Run(cmd, func(t *testing.T) {
			if got := NormalizeCommand(cmd); got.ParseError {
				t.Fatalf("NormalizeCommand(%q) reported a parse error", cmd)
			}
		})
	}
}

func TestSubshellIsUnwrappedNotDowngraded(t *testing.T) {
	// A whole-segment subshell must expand to the commands inside it, so each
	// one is classified on its own merits.
	for _, tc := range []struct {
		name string
		cmd  string
		want []string
	}{
		{"single command", "(ls)", []string{"ls"}},
		{"compound inside", "(cd /tmp && ls)", []string{"cd /tmp", "ls"}},
		{"nested", "((cd /tmp && ls))", []string{"cd /tmp", "ls"}},
		{"not whole-segment", "(echo a) | (echo b)", []string{"echo a", "echo b"}},
		{"separator outside", "(echo a) && ls", []string{"echo a", "ls"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeCommand(tc.cmd)
			if len(got.Segments) != len(tc.want) {
				t.Fatalf("segments = %q, want %q", got.Segments, tc.want)
			}
			for i := range tc.want {
				if got.Segments[i] != tc.want[i] {
					t.Fatalf("segments = %q, want %q", got.Segments, tc.want)
				}
			}
		})
	}
}

func TestMalformedInputStillReportsParseError(t *testing.T) {
	// The upgrade exists for genuinely unparseable input and must survive.
	for _, cmd := range []string{
		`echo "unbalanced`,
		`echo 'unbalanced`,
		`(a`,
	} {
		t.Run(cmd, func(t *testing.T) {
			if got := NormalizeCommand(cmd); !got.ParseError {
				t.Fatalf("NormalizeCommand(%q) should still report a parse error", cmd)
			}
		})
	}
}

func TestStripSubshellWrapper(t *testing.T) {
	for _, tc := range []struct {
		in    string
		want  string
		wants bool
	}{
		{"(ls)", "ls", true},
		{" ( ls ) ", "ls", true},
		{"((ls))", "(ls)", true},
		{"(echo a) | (echo b)", "", false},
		{"(echo a) && ls", "", false},
		{"()", "", false},
		{"(a", "", false},
		{"a)", "", false},
		{"ls", "", false},
		{`(echo ")")`, `echo ")"`, true},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := stripSubshellWrapper(tc.in)
			if ok != tc.wants || got != tc.want {
				t.Fatalf("stripSubshellWrapper(%q) = (%q, %v), want (%q, %v)",
					tc.in, got, ok, tc.want, tc.wants)
			}
		})
	}
}

func TestQuotedHeredocBodyParsesWithoutUpgrade(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
	}{
		{
			"python stdin",
			"python3 - <<'EOF'\nprint(1)\nEOF",
		},
		{
			"compound prefix",
			"[ -n \"$p\" ] && python3 - \"$p\" <<'EOF'\nprint(1)\nEOF",
		},
		{
			"double quoted delimiter",
			"python3 - <<\"PY\"\nprint('a | b')\nPY",
		},
		{
			"backslash quoted delimiter",
			"python3 - <<\\PY\nprint(1)\nPY",
		},
		{
			"tab stripping delimiter",
			"python3 - <<-'EOF'\n\tprint(1)\n\tEOF",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeCommand(tc.cmd)
			if got.ParseError {
				t.Fatalf("quoted heredoc was treated as parse error: %#v", got)
			}
			classified := NewPatternEngine().ClassifyCommand(tc.cmd, "")
			if classified.ParseError || classified.MatchedPattern == "parse_error" {
				t.Fatalf("quoted heredoc was upgraded by classifier: %#v", classified)
			}
		})
	}
}

func TestQuotedHeredocBodyRemainsVisibleToRiskPatterns(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want RiskTier
	}{
		{"destructive shell-looking data", "rm -rf /", RiskTierCritical},
		{"destructive SQL-looking data", "DROP DATABASE production", RiskTierCritical},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := "cat <<'EOF'\n" + tc.body + "\nEOF"
			normalized := NormalizeCommand(cmd)
			if normalized.ParseError {
				t.Fatalf("quoted heredoc unexpectedly failed to parse: %#v", normalized)
			}
			found := false
			for _, segment := range normalized.Segments {
				if segment == tc.body {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("literal heredoc body disappeared from classification segments: %q", normalized.Segments)
			}
			result := NewPatternEngine().ClassifyCommand(cmd, "")
			if result.Tier != tc.want {
				t.Fatalf("body risk was hidden: tier=%q pattern=%q segments=%#v",
					result.Tier, result.MatchedPattern, normalized.Segments)
			}
		})
	}
}

func TestUnsupportedOrUnterminatedHeredocStillFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  string
	}{
		{
			"unquoted delimiter may expand substitutions",
			"python3 - <<EOF\nprint($(whoami))\nEOF",
		},
		{
			"unterminated quoted heredoc",
			"python3 - <<'EOF'\nprint(1)",
		},
		{
			"mixed quoted delimiter",
			"python3 - <<E'OF'\nprint(1)\nEOF",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeCommand(tc.cmd)
			if !got.ParseError {
				t.Fatalf("unsupported heredoc should remain fail-closed: %#v", got)
			}
		})
	}
}
