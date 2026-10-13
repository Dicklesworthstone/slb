package core

import (
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/mattn/go-shellwords"
)

func TestResolveRMHomeParameters(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell parameters")
	}
	for _, home := range []string{"/home/ubuntu", "/data/agent home", "/data/agent'home", "/var/lib/agent", "", "relative-home"} {
		for _, cwd := range []string{"", "/home/ubuntu", "/data/tmp"} {
			for _, alias := range []string{"$HOME", "${HOME}"} {
				for _, tail := range []string{"", "/", "/*", "/**/*", "/.*", "/?*", "/[!.]*", "/project/build", "/project/..", "/../.."} {
					t.Run(home+":"+cwd+":"+alias+tail, func(t *testing.T) {
						want := resolveRMPathToken("~"+tail, cwd, home)
						got := resolveRMPathToken(alias+tail, cwd, home)
						if got != want {
							t.Fatalf("%q -> %q, want home-equivalent %q", alias+tail, got, want)
						}
						if tail == "" && got != "~" {
							t.Fatalf("home root lost: %q", got)
						}
					})
				}
			}
			for _, literal := range []string{"$HOME_BACKUP", "${HOME_BACKUP}", "$HOMEish", "${HOME}suffix", "prefix$HOME", "$home"} {
				want := filepath.Clean(literal)
				if cwd != "" {
					want = filepath.Join(cwd, literal)
				}
				if got := resolveRMPathToken(literal, cwd, home); got != want {
					t.Errorf("non-HOME parameter %q changed to %q, want %q", literal, got, want)
				}
			}
		}
	}
}

func TestRMHomeParameterOptionBoundaries(t *testing.T) {
	home := filepath.Join(t.TempDir(), "agent home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, tc := range []struct{ args, want []string }{
		{[]string{"rm", "-r", "$HOME"}, []string{"rm", "-r", "~"}},
		{[]string{"rm", "-f", "local --", "-r", "${HOME}/**/*"}, []string{"rm", "-f", "-r", "local --", "~/**/*"}},
		{[]string{"rm", "--", "-r", "$HOME"}, []string{"rm", "./-r", "~"}},
		{[]string{"rm", "-f", "${HOME}/notes.txt"}, []string{"rm", "-f", filepath.Join(home, "notes.txt")}},
		{[]string{"rm", "-r", "", "${HOME}"}, []string{"rm", "-r", "", "~"}},
	} {
		parser := shellwords.NewParser()
		parser.ParseEnv, parser.ParseBacktick = false, false
		resolved := ResolvePathsInCommand(joinNormalizedTokens(tc.args), "")
		got, err := parser.Parse(resolved)
		if err != nil || parser.Position >= 0 || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%#v: %q -> %#v (%v), want %#v", tc.args, resolved, got, err, tc.want)
		}
	}
}

func TestRMHomeParameterClassification(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell parameters")
	}
	t.Setenv("HOME", filepath.Join(t.TempDir(), "agent home"))
	engine := NewPatternEngine()
	for _, cwd := range []string{"", "/home/ubuntu", "/data/tmp"} {
		for _, tc := range []struct {
			command   string
			tier      RiskTier
			approvals int
		}{
			{`rm -rf "$HOME"`, RiskTierCritical, 2},
			{`rm -R "${HOME}"/*`, RiskTierCritical, 2},
			{`rm -f build --recursive "$HOME"/**/*`, RiskTierCritical, 2},
			{`rm -rf "${HOME}/project/.."`, RiskTierCritical, 2},
			{`rm -rf "$HOME/build"`, RiskTierDangerous, 1},
			{`rm -rf "${HOME}"/project/build`, RiskTierDangerous, 1},
			{`rm -f "$HOME/notes.txt"`, RiskTierCaution, 0},
			{`rm -- -r "$HOME"`, RiskTierCaution, 0},
			{`rm -rf $HOME_BACKUP`, RiskTierDangerous, 1},
			{`rm -rf ${HOME}suffix`, RiskTierDangerous, 1},
			{`rm --r /etc`, RiskTierCritical, 2},
			{`rm -f --rec "$HOME"`, RiskTierCritical, 2},
			{`rm --rec build`, RiskTierDangerous, 1},
			{`rm -- --rec /etc`, RiskTierCaution, 0},
		} {
			got := engine.ClassifyCommand(tc.command, cwd)
			if got.Tier != tc.tier || got.MinApprovals != tc.approvals || !got.NeedsApproval || got.IsSafe || got.ParseError {
				t.Errorf("%q cwd=%q: %+v, want %s/%d", tc.command, cwd, got, tc.tier, tc.approvals)
			}
		}
	}
}
