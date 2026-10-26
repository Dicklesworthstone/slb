package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/testutil"
)

// Use the daemon's policy-loading and classification path. The strings below
// are never passed to an executor; zero-quorum CAUTION must still be tracked
// and blocked by the default hook policy, not confused with SAFE.
func TestHookQueryRMOperandBoundariesAndRootGlobs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell paths")
	}
	home := filepath.Join(testutil.NonSystemTempDir(t), "agent home")
	project := filepath.Join(home, "project")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	srv := &IPCServer{}
	// Physical policy discovery requires existing directories. Keep the
	// classification cases independent of paths installed on the test host.
	for _, cwd := range []string{"", testutil.NonSystemTempDir(t), testutil.NonSystemTempDir(t), home, project} {
		for _, tc := range []struct {
			command, tier string
			approvals     int
		}{
			{`rm -f /home/ubuntu/notes.txt`, "caution", 0},
			{`rm -rf build`, "dangerous", 1},
			{`rm -rf ./build`, "dangerous", 1},
			{`rm -rf /?*`, "critical", 2},
			{`rm -rf /[!.]*`, "critical", 2},
			{`rm -rf /./?*`, "critical", 2},
			{`rm -rf '/../[!.]*'`, "critical", 2},
			{`rm -f /?*`, "caution", 0},
			{`rm -- -r /?*`, "caution", 0},
			{`rm -rf build /?*`, "critical", 2},
			{`rm -f 'local --' -r /etc`, "critical", 2},
			{`rm -f 'local -r' /etc/notes.txt`, "caution", 0},
			{`rm -rf ~alice/**/*`, "critical", 2},
			{`rm -rf ~/.*`, "critical", 2},
			{`rm -rf ~/project/build`, "dangerous", 1},
			{`rm -rf "$HOME"`, "critical", 2},
			{`rm -R "${HOME}"/*`, "critical", 2},
			{`rm -f build --recursive "$HOME"/**/*`, "critical", 2},
			{`rm -rf "${HOME}/project/.."`, "critical", 2},
			{`rm -rf "$HOME/build"`, "dangerous", 1},
			{`rm -f "$HOME/notes.txt"`, "caution", 0},
			{`rm -- -r "$HOME"`, "caution", 0},
			{`rm --r /etc`, "critical", 2},
			{`rm -f --rec "$HOME"`, "critical", 2},
			{`rm --rec build`, "dangerous", 1},
			{`rm -- --rec /etc`, "caution", 0},
		} {
			got := srv.classifyCommand(HookQueryParams{Command: tc.command, CWD: cwd})
			if got.Tier != tc.tier || got.MinApprovals != tc.approvals || got.Action != "block" || got.MatchedPattern == "" {
				t.Errorf("cwd=%q command=%q: %+v, want %s/%d/block", cwd, tc.command, got, tc.tier, tc.approvals)
			}
		}
	}
}
