package core

import (
	"reflect"
	"strings"
	"testing"

	"github.com/mattn/go-shellwords"
)

func TestCommandSpecPolicyTextPreservesInvocation(t *testing.T) {
	for _, argv := range [][]string{
		{"rm", "--recursive", "/etc"},
		{"rm", "--", "-r", "/etc"},
		{"echo", "", "a b", "a\tb", "a\nb"},
		{"echo", "a'b", `a"b`, `a\b`},
		{"echo", "# comment", "next"},
		{"echo", "$(printf not-executed)", "`printf not-executed`", "$HOME", "${HOME}", "~"},
		{"echo", ";", "&&", "||", "|", ">", "<", "(", ")", "*", "?", "[abc]"},
		{"sh", "-c", "printf '%s' 'a b'"},
		{"/a path/tool", "an argument"},
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			before := append([]string(nil), argv...)
			text := commandSpecPolicyText("git stash", argv, false)
			parser := shellwords.NewParser()
			parser.ParseEnv, parser.ParseBacktick = false, false
			got, err := parser.Parse(text)
			if err != nil || parser.Position >= 0 || !reflect.DeepEqual(got, before) {
				t.Fatalf("policy text %q: argv=%#v, error=%v, position=%d; want %#v", text, got, err, parser.Position, before)
			}
			if !reflect.DeepEqual(argv, before) {
				t.Fatal("classification mutated executable argv")
			}
		})
	}
	for _, raw := range []string{"", "   ", "printf  hello\tworld", `printf 'a b'`, "echo one; echo two"} {
		parser := shellwords.NewParser()
		parser.ParseEnv, parser.ParseBacktick = false, false
		got, err := parser.Parse(commandSpecPolicyText(raw, nil, false))
		want := strings.Fields(raw)
		if err != nil || !reflect.DeepEqual(got, want) {
			// Both nil and empty represent an empty invocation.
			if err != nil || len(got) != 0 || len(want) != 0 {
				t.Errorf("legacy raw %q: got %#v, error %v; want %#v", raw, got, err, want)
			}
		}
		if got := commandSpecPolicyText(raw, []string{"rm", "-rf", "/"}, true); got != raw {
			t.Errorf("shell mode changed script: got %q, want %q", got, raw)
		}
	}
}

func TestClassifyCommandSpecUsesExecutableInput(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	engine := NewPatternEngine()
	for _, tc := range []struct {
		name      string
		spec      CommandSpec
		tier      RiskTier
		approvals int
	}{
		{"misleading safe display", CommandSpec{Raw: "git stash", Argv: []string{"rm", "-rf", "/etc"}}, RiskTierCritical, 2},
		{"misleading caution display", CommandSpec{Raw: "rm notes.txt", Argv: []string{"rm", "--rec", "/home/ubuntu"}}, RiskTierCritical, 2},
		{"project cleanup", CommandSpec{Raw: "git stash", Argv: []string{"rm", "-rf", "./build"}, Cwd: "/home/ubuntu"}, RiskTierDangerous, 1},
		{"literal option operand", CommandSpec{Raw: "rm -rf /etc", Argv: []string{"rm", "--", "-rf", "/etc"}}, RiskTierCaution, 0},
		{"safe executable", CommandSpec{Raw: "rm -rf /etc", Argv: []string{"git", "stash"}}, RiskTier(RiskSafe), 0},
		{"shell uses raw", CommandSpec{Raw: "rm -rf /etc", Argv: []string{"git", "stash"}, Shell: true}, RiskTierCritical, 2},
		{"shell ignores unused argv", CommandSpec{Raw: "git stash", Argv: []string{"rm", "-rf", "/etc"}, Shell: true}, RiskTier(RiskSafe), 0},
		{"explicit interpreter", CommandSpec{Raw: "git stash", Argv: []string{"sh", "-c", "rm -rf /etc"}}, RiskTierCritical, 2},
		{"legacy direct argv", CommandSpec{Raw: "rm -rf /etc"}, RiskTierCritical, 2},
		{"literal shell operators", CommandSpec{Raw: "git stash", Argv: []string{"printf", "%s", "&&", "rm -rf /etc"}}, "", 0},
		{"literal backticks", CommandSpec{Raw: "git stash", Argv: []string{"printf", "%s", "`rm -rf /etc`"}}, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := engine.ClassifyCommandSpec(tc.spec)
			if got.Tier != tc.tier || got.MinApprovals != tc.approvals || got.ParseError {
				t.Fatalf("got %+v, want %s/%d without parse error", got, tc.tier, tc.approvals)
			}
		})
	}
}
