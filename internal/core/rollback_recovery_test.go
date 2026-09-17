package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestGitRecoveryWithoutRequestDatabase(t *testing.T) {
	root := newGitRecoveryRepo(t)
	recoveryWrite(t, root, ".gitignore", []byte(".slb/\n"))
	recoveryGit(t, root, "add", ".gitignore")
	recoveryGit(t, root, "commit", "-qm", "ignore SLB state")
	recoveryWrite(t, root, "tracked.txt", []byte("staged work\n"))
	recoveryGit(t, root, "add", "tracked.txt")
	recoveryWrite(t, root, "tracked.txt", []byte("unstaged work\n"))
	recoveryWrite(t, root, "extra.txt", []byte("untracked work\n"))
	data, err := CaptureRollbackState(context.Background(), &db.Request{
		ID: "offline", ProjectPath: root,
		Command: db.CommandSpec{Raw: "git clean -fdx", Cwd: root},
	}, RollbackCaptureOptions{MaxSizeBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	// This deliberately invalid DB represents state created after capture.
	// Recovery must neither open it nor depend on its survival.
	recoveryWrite(t, root, ".slb/state.db", []byte("not a database"))
	recoveryGit(t, root, "reset", "--hard", "HEAD")
	recoveryGit(t, root, "clean", "-fdx")
	snapshots, err := ListGitRollbackSnapshots(context.Background(), root)
	if err != nil || len(snapshots) != 1 || snapshots[0].RequestID != "offline" {
		t.Fatalf("discovery needs missing DB: %+v, %v", snapshots, err)
	}
	if _, err := RestoreGitRollbackSnapshot(context.Background(), snapshots[0].Path, RollbackRestoreOptions{}); err == nil {
		t.Fatal("unconfirmed offline recovery accepted")
	}
	result, err := RestoreGitRollbackSnapshot(context.Background(), snapshots[0].Path, RollbackRestoreOptions{Force: true})
	if err != nil || result == nil || result.Status != "rolled_back" || result.DatabaseUpdated {
		t.Fatalf("offline recovery failed: %+v, %v", result, err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "tracked.txt")); err != nil || string(got) != "unstaged work\n" {
		t.Fatalf("worktree not recovered: %q, %v", got, err)
	}
	if got := recoveryGit(t, root, "show", ":tracked.txt"); got != "staged work\n" {
		t.Fatalf("index not recovered: %q", got)
	}
	if got, err := os.ReadFile(filepath.Join(root, "extra.txt")); err != nil || string(got) != "untracked work\n" {
		t.Fatalf("untracked not recovered: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".slb", "state.db")); !os.IsNotExist(err) {
		t.Fatalf("offline recovery unexpectedly opened/created DB: %v", err)
	}
	receipt, err := os.ReadFile(result.ReceiptPath)
	if err != nil {
		t.Fatal(err)
	}
	var recorded GitRollbackRecoveryResult
	if err := json.Unmarshal(receipt, &recorded); err != nil || recorded.RequestID != data.RequestID || recorded.DatabaseUpdated {
		t.Fatalf("incorrect durable receipt: %q, %v", receipt, err)
	}
}

func TestGitRecoveryDiscoveryReportsPartialCaptures(t *testing.T) {
	root := newGitRecoveryRepo(t)
	empty, err := ListGitRollbackSnapshots(context.Background(), root)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty discovery: %+v, %v", empty, err)
	}
	data := recoveryCapture(t, root)
	bad := filepath.Join(filepath.Dir(data.RollbackPath), "req-partial")
	if err := os.Mkdir(bad, 0700); err != nil {
		t.Fatal(err)
	}
	list, err := ListGitRollbackSnapshots(context.Background(), root)
	if err != nil || len(list) != 2 || list[0].RequestID == "" || list[1].Error == "" {
		t.Fatalf("partial capture hid valid snapshot: %+v, %v", list, err)
	}
	encoded, err := json.Marshal(list)
	if err != nil || strings.Contains(string(encoded), "command_raw") || strings.Contains(string(encoded), "staged work") {
		t.Fatalf("discovery leaked command/contents: %s, %v", encoded, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ListGitRollbackSnapshots(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost discovery cancellation: %v", err)
	}
	if _, err := RestoreGitRollbackSnapshot(ctx, data.RollbackPath, RollbackRestoreOptions{Force: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost restore cancellation: %v", err)
	}
}

func TestGitRecoveryBindsSelectedSnapshotDirectory(t *testing.T) {
	root := newGitRecoveryRepo(t)
	data := recoveryCapture(t, root)
	data.RollbackPath = filepath.Join(t.TempDir(), "wrong-source")
	actual := strings.TrimSpace(recoveryGit(t, root, "rev-parse", "--absolute-git-dir"))
	base := filepath.Join(actual, "slb-rollback")
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) != 1 {
		t.Fatal("missing snapshot", err)
	}
	selected := filepath.Join(base, entries[0].Name())
	if err := writeRollbackMetadata(selected, data); err != nil {
		t.Fatal(err)
	}
	result, err := RestoreGitRollbackSnapshot(context.Background(), selected, RollbackRestoreOptions{Force: true})
	if err != nil || result.RollbackPath != selected {
		t.Fatalf("metadata redirected selected archive: %+v, %v", result, err)
	}
}
