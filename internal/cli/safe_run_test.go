package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/spf13/cobra"
)

func TestSafeRunRechecksBeforeLogsAndSpawn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell marker probe")
	}
	for _, change := range []string{"unchanged", "revoked", "ended", "invalid"} {
		t.Run(change, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			h := testutil.NewHarness(t)
			resetRunFlags()
			t.Cleanup(resetRunFlags)
			session := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir), testutil.WithAgent("safe-probe"))
			flagSessionID, flagOutput = session.ID, "json"
			path := filepath.Join(h.ProjectDir, ".slb", "config.toml")
			if err := os.WriteFile(path, []byte("[patterns.safe]\npatterns = ['^printf safe-run-probe$']\n"), 0600); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(h.ProjectDir, "spawn-marker")
			command := fmt.Sprintf("printf safe-run-probe > %q", marker)
			old, err := core.LoadCommandPolicy(h.DB, config.LoadOptions{ProjectDir: h.ProjectDir})
			if err != nil || !old.ClassifyCommand(command, h.ProjectDir).IsSafe {
				t.Fatalf("probe did not start SAFE: %v", err)
			}
			var want error
			switch change {
			case "revoked":
				if err := os.WriteFile(path, []byte("[patterns.safe]\npatterns = []\n"), 0600); err != nil {
					t.Fatal(err)
				}
				want = core.ErrApprovalPolicyChanged
			case "ended":
				if err := h.DB.EndSession(session.ID); err != nil {
					t.Fatal(err)
				}
				want = core.ErrSessionInactive
			case "invalid":
				if err := os.WriteFile(path, []byte("[invalid"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			code := -2
			cmd := &cobra.Command{Use: "safe-probe", RunE: func(cmd *cobra.Command, _ []string) error {
				var err error
				code, err = runSafeCommandChecked(cmd, output.New(output.Format(GetOutput())), h.DB, command, h.ProjectDir, h.ProjectDir)
				return err
			}}
			stdout, err := executeCommandCapture(t, cmd)
			var response map[string]any
			if parseErr := json.Unmarshal([]byte(stdout), &response); parseErr != nil {
				t.Fatalf("invalid JSON response: %v\n%s", parseErr, stdout)
			}
			if change == "unchanged" {
				if err != nil || code != 0 || response["executed"] != true {
					t.Fatalf("valid SAFE run failed: code=%d err=%v response=%v", code, err, response)
				}
				body, readErr := os.ReadFile(marker)
				if readErr != nil || string(body) != "safe-run-probe" {
					t.Fatalf("probe did not run: %q %v", body, readErr)
				}
				return
			}
			if err == nil || (want != nil && !errors.Is(err, want)) || code != -1 || response["status"] != "safe_recheck_failed" {
				t.Fatalf("stale SAFE decision reached runner: code=%d err=%v response=%v", code, err, response)
			}
			for _, path := range []string{marker, filepath.Join(h.ProjectDir, ".slb", "logs")} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("rejected run touched %s: %v", path, err)
				}
			}
		})
	}
}
