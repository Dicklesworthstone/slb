package core

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// These strings are parsed only, never executed.
func TestDirectCommandParsingRejectsTruncatedInput(t *testing.T) {
	for _, command := range []string{
		"", " \t\n", "''", "'' operand", "printf '\x00'", "printf \x00",
		"printf 'unterminated", `printf \`,
		"printf first; printf second", "printf first && printf second",
		"printf first || printf second", "printf first | printf second",
		"printf first & printf second", "printf first > output",
		"printf first 2>output", "printf first <input", ">output printf first",
	} {
		t.Run(command, func(t *testing.T) {
			argv, err := ParseCommandToArgv(command)
			if err == nil || argv != nil {
				t.Fatalf("invalid direct invocation produced argv=%q error=%v", argv, err)
			}
		})
	}
}

func FuzzDirectPolicyArgvRoundTrip(f *testing.F) {
	for _, words := range [][2]string{
		{"", ""}, {"a'b", `a\b`}, {"--", "-r"}, {"# comment", "a\nb"},
		{"$HOME", "$(printf literal)"}, {";", "&&"}, {"α β", "a\tb"},
	} {
		f.Add(words[0], words[1])
	}
	f.Fuzz(func(t *testing.T, first, second string) {
		if !utf8.ValidString(first) || !utf8.ValidString(second) || strings.ContainsRune(first+second, 0) {
			t.Skip("round-trip corpus is UTF-8 without NUL")
		}
		want := []string{"printf", first, second}
		text := commandSpecPolicyText("unused display", want, false)
		got, err := ParseCommandToArgv(text)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("policy serialization changed arguments: text=%q got=%q error=%v want=%q", text, got, err, want)
		}
	})
}

func TestDirectCommandParsingPreservesLiteralArguments(t *testing.T) {
	for _, tc := range []struct {
		command string
		argv    []string
	}{
		{`printf '%s' 'first second' ''`, []string{"printf", "%s", "first second", ""}},
		{`printf 'a'\''b' 'a"b' 'a\b'`, []string{"printf", "a'b", `a"b`, `a\b`}},
		{`printf ';' '&&' '||' '|' '>' '<' '&'`, []string{"printf", ";", "&&", "||", "|", ">", "<", "&"}},
		{`printf "$HOME" '${HOME}' '~' '$(printf literal)'`, []string{"printf", "$HOME", "${HOME}", "~", "$(printf literal)"}},
		{`printf "$(printf command-spec-probe)"`, []string{"printf", "$(printf command-spec-probe)"}},
		{`printf \; \& \| \> \<`, []string{"printf", ";", "&", "|", ">", "<"}},
		{`rm -- '-rf' '/home/user/file name'`, []string{"rm", "--", "-rf", "/home/user/file name"}},
		{"printf 'α β' 'a\nb' 'a\tb'", []string{"printf", "α β", "a\nb", "a\tb"}},
	} {
		t.Run(tc.command, func(t *testing.T) {
			argv, err := ParseCommandToArgv(tc.command)
			if err != nil || !reflect.DeepEqual(argv, tc.argv) {
				t.Fatalf("argv=%q error=%v, want %q", argv, err, tc.argv)
			}
		})
	}
}

func TestRequestArgvRespectsExecutionMode(t *testing.T) {
	for _, command := range []string{"printf one; printf two", "printf x > file", "printf 'unterminated"} {
		if argv, err := parseRequestArgv(command, false); err == nil || argv != nil {
			t.Errorf("direct mode accepted incomplete input: %q: %q, %v", command, argv, err)
		}
		if argv, err := parseRequestArgv(command, true); err != nil || argv != nil {
			t.Errorf("shell mode must keep Raw authoritative, not partial argv: %q: %q, %v", command, argv, err)
		}
	}
	for _, shell := range []bool{false, true} {
		for _, command := range []string{"", " \t", "printf \x00"} {
			if _, err := parseRequestArgv(command, shell); err == nil {
				t.Errorf("unexecutable input accepted: shell=%v raw=%q", shell, command)
			}
		}
		if argv, err := parseRequestArgv(`printf '%s' 'a b'`, shell); err != nil || !reflect.DeepEqual(argv, []string{"printf", "%s", "a b"}) {
			t.Errorf("literal invocation changed: shell=%v argv=%q error=%v", shell, argv, err)
		}
	}
}
