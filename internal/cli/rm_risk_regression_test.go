package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/testutil"
)

// Exercise the production check command, not a second copy of its rules.
// Commands are classifier inputs only; none of the deletion strings is run.
func TestCheckRMOperandBoundariesAndRootGlobs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell paths")
	}
	home := filepath.Join(t.TempDir(), "agent home")
	t.Setenv("HOME", home)
	h := testutil.NewHarness(t)
	t.Cleanup(resetPatternsFlags)
	for _, cwd := range []string{home, filepath.Join(home, "project"), t.TempDir()} {
		if err := os.MkdirAll(cwd, 0700); err != nil {
			t.Fatal(err)
		}
		t.Chdir(cwd)
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
		} {
			resetPatternsFlags()
			cmd := newTestPatternsCmd(h.DBPath)
			stdout, err := executeCommandCapture(t, cmd, "check", tc.command, "--json")
			if err != nil {
				t.Fatalf("cwd=%q command=%q: %v", cwd, tc.command, err)
			}
			var got struct {
				Tier          string `json:"tier"`
				MinApprovals  int    `json:"min_approvals"`
				NeedsApproval bool   `json:"needs_approval"`
				IsSafe        bool   `json:"is_safe"`
				ParseError    bool   `json:"parse_error"`
			}
			if err := json.Unmarshal([]byte(stdout), &got); err != nil {
				t.Fatalf("decode check response: %v\n%s", err, stdout)
			}
			if got.Tier != tc.tier || got.MinApprovals != tc.approvals || !got.NeedsApproval || got.IsSafe || got.ParseError {
				t.Errorf("cwd=%q command=%q: %+v, want %s/%d without parse error", cwd, tc.command, got, tc.tier, tc.approvals)
			}
		}
	}
}
