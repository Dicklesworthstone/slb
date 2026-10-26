package core

import (
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/mattn/go-shellwords"
)

func TestRMNormalizedArgvRoundTrip(t *testing.T) {
	// These are literal argv elements, never commands to execute. In
	// particular the apparent -- inside a filename must not hide a later -r.
	for _, name := range []string{"local --", "local -r", "a'b", `a"b`, `a\b`, "a; b", "a|b", "a&b", "a>b", "a<b", "a(b)", "`id`", "a\tb", "a\nb", ""} {
		t.Run(name, func(t *testing.T) {
			want := []string{"rm", "-f", name, "-r", "/etc"}
			before := append([]string(nil), want...)
			parser := shellwords.NewParser()
			parser.ParseEnv, parser.ParseBacktick = false, false
			got, err := parser.Parse(joinNormalizedTokens(want))
			if err != nil || parser.Position >= 0 || !reflect.DeepEqual(got, before) {
				t.Fatalf("round trip: got %#v, error %v, position %d; want %#v", got, err, parser.Position, before)
			}
			if !reflect.DeepEqual(want, before) {
				t.Fatal("normalization mutated argv")
			}
		})
	}
	// SQL and other existing regex-facing text are deliberately unchanged.
	if got := joinNormalizedTokens([]string{"psql", "-c", "TRUNCATE users;"}); got != "psql -c TRUNCATE users;" {
		t.Fatalf("non-rm normalization changed: %q", got)
	}
}

func TestResolveRMPathTokenHomes(t *testing.T) {
	home := filepath.Join(t.TempDir(), "custom home")
	cwd := filepath.Join(home, "project")
	for _, tc := range []struct{ name, input, want string }{
		{"custom root", home, "~"},
		{"home alias", "~", "~"},
		{"parent home", "..", "~"},
		{"named root", "~alice/", "~alice"},
		{"named glob", "~alice/**/*", "~alice/**/*"},
		{"named hidden glob", "~alice/.*", "~alice/.*"},
		{"named project", "~alice/project/build", "~alice/project/build"},
		{"named root traversal", "~alice/project/..", "~alice"},
		{"unknown home parent", "~alice/../bob", "~"},
		{"local", "build", filepath.Join(cwd, "build")},
		{"dot local", "./build", filepath.Join(cwd, "build")},
		{"literal empty operand", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveRMPathToken(tc.input, cwd, home); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	for _, glob := range []string{"*", "**", "**/*", ".*", "?*", "[!.]*"} {
		for _, target := range []string{filepath.Join(home, glob), "~/" + glob, filepath.Join("..", glob)} {
			if got := resolveRMPathToken(target, cwd, home); got != "~/"+glob {
				t.Errorf("%q: got %q, want %q", target, got, "~/"+glob)
			}
		}
		// A literal project prefix is not a home-wide wipe.
		target := filepath.Join(home, "project", glob)
		if got := resolveRMPathToken(target, cwd, home); got != target {
			t.Errorf("project descendant changed: %q -> %q", target, got)
		}
	}
	// A similar prefix must not count as the home, and mapping custom homes
	// must not erase the /var or /srv protection of literal descendants.
	other := filepath.Join(home+"-backup", "*")
	if got := resolveRMPathToken(other, cwd, home); got != other {
		t.Errorf("unrelated home prefix changed: %q -> %q", other, got)
	}
	if runtime.GOOS != "windows" {
		target := "/var/lib/agent/cache"
		if got := resolveRMPathToken(target, cwd, "/var/lib/agent"); got != target {
			t.Errorf("system descendant changed: %q -> %q", target, got)
		}
	}
	if got := resolveRMPathToken("-r", "", home); got != "./-r" {
		t.Fatalf("literal option filename lost: %q", got)
	}
}

func TestRMResolutionPreservesRealOptionTerminator(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	target := filepath.Join(t.TempDir(), "etc")
	for _, tc := range []struct {
		args, want []string
	}{
		{[]string{"rm", "-f", "local --", "-r", target}, []string{"rm", "-f", "-r", "local --", target}},
		{[]string{"rm", "-f", "local --", "--", "-r", target}, []string{"rm", "-f", "local --", "./-r", target}},
		{[]string{"rm", "-r", "--", "--", target}, []string{"rm", "-r", "./--", target}},
		{[]string{"rm", "-r", ""}, []string{"rm", "-r", ""}},
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

func TestRMClassificationPreservesOperandsAndHomes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell paths")
	}
	home := filepath.Join(testutil.NonSystemTempDir(t), "custom home")
	t.Setenv("HOME", home)
	// os.UserHomeDir uses USERPROFILE on Windows.
	t.Setenv("USERPROFILE", home)
	engine := NewPatternEngine()
	for _, cwd := range []string{"", "/home/ubuntu", "/data/tmp", home} {
		for _, tc := range []struct {
			command   string
			tier      RiskTier
			approvals int
		}{
			{`rm -f 'local --' -r /etc`, RiskTierCritical, 2},
			{`rm -f 'local --' -r build`, RiskTierDangerous, 1},
			{`rm -f 'local --' -- -r /etc`, RiskTierCaution, 0},
			{`rm -f 'local -r' /etc/notes.txt`, RiskTierCaution, 0},
			{`sudo rm -f 'local --' -r /etc`, RiskTierCritical, 2},
			{`echo ok && rm -f 'local --' -r /etc`, RiskTierCritical, 2},
			{`sh -c "rm -f 'local --' -r /etc"`, RiskTierCritical, 2},
			{`rm -rf ~/**/*`, RiskTierCritical, 2},
			{`rm -rf ~alice/**/*`, RiskTierCritical, 2},
			{`rm -rf ~alice/`, RiskTierCritical, 2},
			{`rm -rf ~alice/project/..`, RiskTierCritical, 2},
			{`rm -rf ~alice/project/build`, RiskTierDangerous, 1},
			{`rm -rf '` + home + `'`, RiskTierCritical, 2},
			{`rm -rf '` + home + `/**/*'`, RiskTierCritical, 2},
			{`rm -rf '` + home + `/project/**/*'`, RiskTierDangerous, 1},
			{`rm -f /home/ubuntu/notes.txt`, RiskTierCaution, 0},
			{`rm -rf build`, RiskTierDangerous, 1},
			{`rm -rf ./build`, RiskTierDangerous, 1},
			{`rm -rf app.log /`, RiskTierCritical, 2},
			{`rm -- -r /etc`, RiskTierCaution, 0},
			{`rm -r ''`, RiskTierDangerous, 1},
		} {
			got := engine.ClassifyCommand(tc.command, cwd)
			if got.Tier != tc.tier || got.MinApprovals != tc.approvals || !got.NeedsApproval || got.IsSafe || got.ParseError {
				t.Errorf("cwd=%q command=%q: %+v; want %s/%d without parse error", cwd, tc.command, got, tc.tier, tc.approvals)
			}
		}
	}
}
