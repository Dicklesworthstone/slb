package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/spf13/cobra"
)

func newTestSnapshotRollbackCmd(dbPath string) *cobra.Command {
	root := newTestRollbackCmd(dbPath)
	cmd, _, _ := root.Find([]string{"rollback"})
	cmd.Args = rollbackCmd.Args
	cmd.Flags().String("from", "", "snapshot directory")
	cmd.Flags().Bool("list", false, "list snapshots")
	return root
}

func TestSnapshotRollbackArgumentValidation(t *testing.T) {
	for _, args := range [][]string{
		{"rollback"},
		{"rollback", "--from", "/snapshot"},
		{"rollback", "request", "--from", "/snapshot", "--force"},
		{"rollback", "request", "--list"},
		{"rollback", "--from", "/snapshot", "--list", "--force"},
	} {
		resetRollbackFlags()
		cmd := newTestSnapshotRollbackCmd(filepath.Join(t.TempDir(), "missing.db"))
		if _, _, err := executeCommand(cmd, args...); err == nil {
			t.Errorf("invalid recovery arguments accepted: %q", args)
		}
	}
	t.Cleanup(resetRollbackFlags)
}

func TestSnapshotRollbackCLIWithoutDatabase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	for _, name := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %q: %v: %s", args, err, out)
		}
	}
	git("init", "-q", "--initial-branch=main")
	git("-c", "user.name=SLB", "-c", "user.email=slb@example.invalid", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "baseline")
	if err := os.WriteFile(filepath.Join(root, "extra.txt"), []byte("recover me"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := core.CaptureRollbackState(context.Background(), &db.Request{
		ID: "offline-cli", ProjectPath: root, Command: db.CommandSpec{Raw: "git clean -fd", Cwd: root},
	}, core.RollbackCaptureOptions{MaxSizeBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	git("clean", "-fd")
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old); resetRollbackFlags() })
	// A directory is not a database. Either new mode opening it would fail.
	dbPath := t.TempDir()
	resetRollbackFlags()
	cmd := newTestSnapshotRollbackCmd(dbPath)
	out, err := executeCommandCapture(t, cmd, "rollback", "--list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var snapshots []core.GitRollbackSnapshot
	if err := json.Unmarshal([]byte(out), &snapshots); err != nil || len(snapshots) != 1 {
		t.Fatalf("snapshot discovery failed: %s, %v", out, err)
	}
	resetRollbackFlags()
	cmd = newTestSnapshotRollbackCmd(dbPath)
	out, err = executeCommandCapture(t, cmd, "rollback", "--from", data.RollbackPath, "--force", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var result core.GitRollbackRecoveryResult
	if err := json.Unmarshal([]byte(out), &result); err != nil || result.Status != "rolled_back" || result.DatabaseUpdated {
		t.Fatalf("bad recovery result: %s, %v", out, err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "extra.txt")); err != nil || string(got) != "recover me" {
		t.Fatalf("CLI did not recover file: %q, %v", got, err)
	}
}
