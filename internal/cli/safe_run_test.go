package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/spf13/cobra"
)

// A SAFE execution has no request ID. Timestamp-only paths caused separate
// runs in the same second to truncate and share one audit log.
func TestSafeRunLogsPreserveEachExecution(t *testing.T) {
	for _, prefix := range []string{"safe", ""} {
		t.Run("prefix="+prefix, func(t *testing.T) {
			project := t.TempDir()
			paths := make(map[string]string)
			for i := 0; i < 32; i++ {
				path, err := createRunLogFile(project, prefix)
				if err != nil {
					t.Fatal(err)
				}
				if _, exists := paths[path]; exists {
					t.Fatalf("new execution reused existing audit log %q", path)
				}
				if filepath.Dir(path) != filepath.Join(project, ".slb", "logs") || !strings.HasSuffix(path, ".log") {
					t.Fatalf("unexpected audit location: %q", path)
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
					t.Fatalf("audit log must be a private regular file: %v", info.Mode())
				}
				body := fmt.Sprintf("execution %d\n", i)
				if err := os.WriteFile(path, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
				paths[path] = body
			}
			for path, want := range paths {
				got, err := os.ReadFile(path)
				if err != nil || string(got) != want {
					t.Fatalf("later run changed prior audit log %q: %q (%v)", path, got, err)
				}
			}
		})
	}
}

func TestSafeRunLogsConcurrentCreation(t *testing.T) {
	project := t.TempDir()
	const count = 64
	type result struct {
		path, body string
		err        error
	}
	start := make(chan struct{})
	results := make(chan result, count)
	for i := 0; i < count; i++ {
		go func(index int) {
			<-start
			path, err := createRunLogFile(project, "safe")
			body := fmt.Sprintf("concurrent execution %d\n", index)
			if err == nil {
				err = os.WriteFile(path, []byte(body), 0600)
			}
			results <- result{path, body, err}
		}(i)
	}
	close(start)
	paths := make(map[string]string, count)
	// Drain every worker before checking failures so no writer outlives the
	// test's temporary directory, including against the broken implementation.
	for i := 0; i < count; i++ {
		got := <-results
		if got.err != nil {
			t.Errorf("creating audit log: %v", got.err)
			continue
		}
		if _, exists := paths[got.path]; exists {
			t.Errorf("concurrent runs share audit log %q", got.path)
		}
		paths[got.path] = got.body
	}
	if len(paths) != count {
		t.Fatalf("created %d independent logs for %d executions", len(paths), count)
	}
	for path, want := range paths {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Errorf("audit evidence lost in %q: got %q (%v), want %q", path, got, err, want)
		}
	}
}

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
