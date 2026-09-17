package core

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func newGitRecoveryRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	for _, name := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_EXTERNAL_DIFF"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "absent-config"))
	root := t.TempDir()
	recoveryGit(t, root, "init", "-q", "--initial-branch=main")
	recoveryGit(t, root, "config", "user.name", "SLB recovery test")
	recoveryGit(t, root, "config", "user.email", "slb-recovery@example.invalid")
	recoveryGit(t, root, "config", "commit.gpgsign", "false")
	recoveryWrite(t, root, "tracked.txt", []byte("base\n"))
	recoveryGit(t, root, "add", "tracked.txt")
	recoveryGit(t, root, "commit", "-qm", "baseline")
	return root
}

func recoveryGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = root
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %q: %v: %s", args, err, out)
	}
	return string(out)
}

func recoveryWrite(t *testing.T, root, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, path), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func recoveryCapture(t *testing.T, root string) *RollbackData {
	t.Helper()
	data, err := CaptureRollbackState(context.Background(), &db.Request{
		ID: "git-recovery", ProjectPath: t.TempDir(),
		Command: db.CommandSpec{Raw: "git reset --hard HEAD", Argv: []string{"git", "reset", "--hard", "HEAD"}, Cwd: root},
	}, RollbackCaptureOptions{MaxSizeBytes: 20 << 20})
	if err != nil || data == nil || data.Git == nil {
		t.Fatalf("capture: %+v, %v", data, err)
	}
	// Exercise the actual on-disk JSON contract as well as in-memory data.
	loaded, err := LoadRollbackData(data.RollbackPath)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func TestGitRollbackRestoresIndexAndWorktree(t *testing.T) {
	for _, kind := range []string{"text", "binary", "staged-add", "staged-delete", "unstaged-delete", "mode"} {
		t.Run(kind, func(t *testing.T) {
			root := newGitRecoveryRepo(t)
			if kind == "mode" && runtime.GOOS == "windows" {
				t.Skip("Unix executable bit")
			}
			switch kind {
			case "text":
				recoveryWrite(t, root, "tracked.txt", []byte("staged\n"))
				recoveryGit(t, root, "add", "tracked.txt")
				recoveryWrite(t, root, "tracked.txt", []byte("unstaged\n"))
			case "binary":
				recoveryWrite(t, root, "tracked.txt", []byte{0, 1, 2, 3, 255})
				recoveryGit(t, root, "add", "tracked.txt")
				recoveryWrite(t, root, "tracked.txt", []byte{0, 3, 2, 1, 254})
			case "staged-add":
				recoveryWrite(t, root, "new file.txt", []byte("staged new\n"))
				recoveryGit(t, root, "add", "new file.txt")
				recoveryWrite(t, root, "new file.txt", []byte("unstaged new\n"))
			case "staged-delete":
				// Only disposable fixture data is removed by Git here.
				recoveryGit(t, root, "rm", "tracked.txt")
			case "unstaged-delete":
				if err := os.Rename(filepath.Join(root, "tracked.txt"), filepath.Join(t.TempDir(), "saved.txt")); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(filepath.Join(root, "tracked.txt"), 0755); err != nil {
					t.Fatal(err)
				}
				recoveryGit(t, root, "add", "tracked.txt")
			}
			index := recoveryGit(t, root, "ls-files", "--stage", "-z")
			patch := recoveryGit(t, root, "diff", "--binary", "--no-ext-diff", "--no-textconv")
			status := recoveryGit(t, root, "status", "--porcelain=v1", "-z")
			data := recoveryCapture(t, root)
			if index != recoveryGit(t, root, "ls-files", "--stage", "-z") || patch != recoveryGit(t, root, "diff", "--binary", "--no-ext-diff", "--no-textconv") {
				t.Fatal("capture changed Git state")
			}
			// Simulate the already-approved destructive command in the fixture.
			recoveryGit(t, root, "reset", "--hard", "HEAD")
			if err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{Force: true}); err != nil {
				t.Fatal(err)
			}
			if got := recoveryGit(t, root, "ls-files", "--stage", "-z"); got != index {
				t.Fatalf("index differs: got=%q want=%q", got, index)
			}
			if got := recoveryGit(t, root, "diff", "--binary", "--no-ext-diff", "--no-textconv"); got != patch {
				t.Fatalf("worktree differs: got=%q want=%q", got, patch)
			}
			if got := recoveryGit(t, root, "status", "--porcelain=v1", "-z"); got != status {
				t.Fatalf("status differs: got=%q want=%q", got, status)
			}
		})
	}
}

func TestGitRollbackRejectsBrokenArtifactsBeforeMutation(t *testing.T) {
	for _, damage := range []string{"missing", "checksum", "size", "path", "no-record"} {
		t.Run(damage, func(t *testing.T) {
			root := newGitRecoveryRepo(t)
			recoveryWrite(t, root, "tracked.txt", []byte("captured\n"))
			data := recoveryCapture(t, root)
			patchPath := filepath.Join(data.RollbackPath, filepath.FromSlash(data.Git.DiffFile))
			switch damage {
			case "missing":
				if err := os.Rename(patchPath, patchPath+".saved"); err != nil {
					t.Fatal(err)
				}
			case "checksum":
				patch, err := os.ReadFile(patchPath)
				if err != nil {
					t.Fatal(err)
				}
				patch[len(patch)-2] ^= 1
				if err := os.WriteFile(patchPath, patch, 0600); err != nil {
					t.Fatal(err)
				}
			case "size":
				if err := os.WriteFile(patchPath, nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "path":
				data.Git.DiffFile = "../outside.patch"
			case "no-record":
				data.Git.Artifacts = nil
			}
			recoveryWrite(t, root, "tracked.txt", []byte("precious current work\n"))
			head := recoveryGit(t, root, "rev-parse", "HEAD")
			if err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{Force: true}); err == nil {
				t.Fatal("damaged snapshot accepted")
			}
			content, err := os.ReadFile(filepath.Join(root, "tracked.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if string(content) != "precious current work\n" || recoveryGit(t, root, "rev-parse", "HEAD") != head {
				t.Fatal("restore changed data before checking all artifacts")
			}
		})
	}
}

func TestGitRollbackRestoresOriginalBranchOrDetachedHead(t *testing.T) {
	for _, detached := range []bool{false, true} {
		t.Run(map[bool]string{false: "branch", true: "detached"}[detached], func(t *testing.T) {
			root := newGitRecoveryRepo(t)
			if detached {
				recoveryGit(t, root, "checkout", "--detach", "HEAD")
			}
			recoveryWrite(t, root, "tracked.txt", []byte("captured work\n"))
			data := recoveryCapture(t, root)
			recoveryGit(t, root, "checkout", "-b", "other")
			recoveryWrite(t, root, "tracked.txt", []byte("other branch\n"))
			recoveryGit(t, root, "add", "tracked.txt")
			recoveryGit(t, root, "commit", "-qm", "other")
			otherHead := recoveryGit(t, root, "rev-parse", "other")
			if err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{Force: true}); err != nil {
				t.Fatal(err)
			}
			if recoveryGit(t, root, "rev-parse", "other") != otherHead {
				t.Fatal("rollback rewound the wrong branch")
			}
			if got := strings.TrimSpace(recoveryGit(t, root, "rev-parse", "--abbrev-ref", "HEAD")); got != data.Git.Branch {
				t.Fatalf("wrong branch %q", got)
			}
			if got := strings.TrimSpace(recoveryGit(t, root, "rev-parse", "HEAD")); got != data.Git.Head {
				t.Fatal("wrong HEAD")
			}
		})
	}
}

func TestGitRollbackHonorsRepositorySelection(t *testing.T) {
	root := newGitRecoveryRepo(t)
	caller := newGitRecoveryRepo(t)
	recoveryWrite(t, root, "tracked.txt", []byte("selected repo\n"))
	data, err := CaptureRollbackState(context.Background(), &db.Request{
		ID: "scope", ProjectPath: t.TempDir(), Command: db.CommandSpec{
			Raw: "git -C selected reset --hard", Argv: []string{"git", "-C", root, "reset", "--hard"}, Cwd: caller,
		},
	}, RollbackCaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if data.Git.RepoRoot != canonical {
		t.Fatalf("wrong repository: %q", data.Git.RepoRoot)
	}
	data.Git.RepoRoot = caller
	if err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{Force: true}); err == nil {
		t.Fatal("different repository accepted")
	}
}

func TestGitRollbackCaptureFailsClosed(t *testing.T) {
	root := newGitRecoveryRepo(t)
	recoveryWrite(t, root, "tracked.txt", bytes.Repeat([]byte("changed\n"), 1024))
	req := &db.Request{ID: "limits", ProjectPath: t.TempDir(), Command: db.CommandSpec{Raw: "git reset --hard", Cwd: root}}
	if data, err := CaptureRollbackState(context.Background(), req, RollbackCaptureOptions{MaxSizeBytes: 16}); err == nil || data != nil {
		t.Fatalf("oversized capture published: %v, %v", data, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CaptureRollbackState(ctx, req, RollbackCaptureOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
	for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "unexpected")
			if _, err := CaptureRollbackState(context.Background(), req, RollbackCaptureOptions{}); err == nil {
				t.Fatal("redirected capture accepted")
			}
		})
	}
}

func TestGitRollbackDoesNotRunDiffPrograms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	root := newGitRecoveryRepo(t)
	marker := filepath.Join(t.TempDir(), "ran")
	program := filepath.Join(t.TempDir(), "diff.sh")
	t.Setenv("SLB_GIT_RECOVERY_MARKER", marker)
	if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf ran > \"$SLB_GIT_RECOVERY_MARKER\"\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	recoveryGit(t, root, "config", "diff.external", program)
	recoveryWrite(t, root, "tracked.txt", []byte("changed\n"))
	data := recoveryCapture(t, root)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("capture ran an external diff program: %v", err)
	}
	patch, err := os.ReadFile(filepath.Join(data.RollbackPath, data.Git.DiffFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(patch, []byte("+changed")) {
		t.Fatal("capture did not preserve a usable builtin patch")
	}
}
