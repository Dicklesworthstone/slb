package core

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestGitCleanRollbackRecoversUntrackedContents(t *testing.T) {
	for _, ignored := range []bool{false, true} {
		t.Run(map[bool]string{false: "standard", true: "including-ignored"}[ignored], func(t *testing.T) {
			root := newGitRecoveryRepo(t)
			recoveryWrite(t, root, ".gitignore", []byte("ignored.bin\n.slb/\n"))
			recoveryGit(t, root, "add", ".gitignore")
			recoveryGit(t, root, "commit", "-qm", "ignore rules")
			files := map[string][]byte{
				"new.txt":              []byte("uncommitted source\n"),
				"space dir/binary.bin": {0, 1, 255, 0, 4},
				"[literal].txt":        []byte("not a glob\n"),
				"ignored.bin":          []byte("ignored data\n"),
			}
			if runtime.GOOS != "windows" {
				files["line\nbreak.txt"] = []byte("newline filename\n")
			}
			for name, content := range files {
				recoveryWrite(t, root, name, content)
			}
			if runtime.GOOS != "windows" {
				if err := os.Symlink("new.txt", filepath.Join(root, "symbolic")); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(filepath.Join(root, "new.txt"), 0751); err != nil {
					t.Fatal(err)
				}
			}
			flag := "-fd"
			if ignored {
				flag = "-fdx"
			}
			data, err := CaptureRollbackState(context.Background(), &db.Request{
				ID: "clean-recovery", ProjectPath: root, Command: db.CommandSpec{
					Raw: "git clean " + flag, Argv: []string{"git", "clean", flag}, Cwd: root,
				},
			}, RollbackCaptureOptions{MaxSizeBytes: 10 << 20})
			if err != nil {
				t.Fatal(err)
			}
			// Git reports the resolved repository path (on macOS the temp
			// dir /var/... is a symlink to /private/var/...).
			resolvedRoot, err := filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(data.RollbackPath, filepath.Join(root, ".git")+string(os.PathSeparator)) &&
				!strings.HasPrefix(data.RollbackPath, filepath.Join(resolvedRoot, ".git")+string(os.PathSeparator)) {
				t.Fatalf("recovery data would be swept away by git clean: %s", data.RollbackPath)
			}
			// Only this disposable fixture worktree is cleaned.
			recoveryGit(t, root, "clean", flag)
			if _, err := os.Stat(filepath.Join(root, "new.txt")); !os.IsNotExist(err) {
				t.Fatal("clean fixture did not remove untracked content")
			}
			if !ignored {
				recoveryWrite(t, root, "ignored.bin", []byte("still ignored and newer\n"))
			}
			recoveryWrite(t, root, "later.txt", []byte("unrelated later work\n"))
			loaded, err := LoadRollbackData(data.RollbackPath)
			if err != nil {
				t.Fatalf("cleanup destroyed the recovery metadata: %v", err)
			}
			if err := RestoreRollbackState(context.Background(), loaded, RollbackRestoreOptions{Force: true}); err != nil {
				t.Fatal(err)
			}
			for name, want := range files {
				if name == "ignored.bin" && !ignored {
					want = []byte("still ignored and newer\n")
				}
				got, err := os.ReadFile(filepath.Join(root, name))
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("lost %q: %q, %v", name, got, err)
				}
			}
			if got, err := os.ReadFile(filepath.Join(root, "later.txt")); err != nil || string(got) != "unrelated later work\n" {
				t.Fatal("rollback removed unrelated untracked work")
			}
			if runtime.GOOS != "windows" {
				if link, err := os.Readlink(filepath.Join(root, "symbolic")); err != nil || link != "new.txt" {
					t.Fatalf("lost symlink: %q, %v", link, err)
				}
				if info, err := os.Stat(filepath.Join(root, "new.txt")); err != nil || info.Mode().Perm() != 0751 {
					t.Fatal("lost file permissions")
				}
			}
			if err := RestoreRollbackState(context.Background(), loaded, RollbackRestoreOptions{Force: true}); err != nil {
				t.Fatalf("retry failed: %v", err)
			}
			if names, err := filepath.Glob(filepath.Join(root, ".slb-*")); err != nil || len(names) > 0 {
				t.Fatalf("restore leaked staging paths: %v, %v", names, err)
			}
		})
	}
}

func TestGitUntrackedIntegrityBeforeTrackedRestore(t *testing.T) {
	root := newGitRecoveryRepo(t)
	recoveryWrite(t, root, "extra", []byte("untracked\n"))
	data := recoveryCapture(t, root)
	archive := filepath.Join(data.RollbackPath, data.Git.UntrackedArchive)
	if err := os.WriteFile(archive, nil, 0600); err != nil {
		t.Fatal(err)
	}
	recoveryWrite(t, root, "tracked.txt", []byte("do not touch\n"))
	if err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{Force: true}); err == nil {
		t.Fatal("corrupt archive accepted")
	}
	if got, err := os.ReadFile(filepath.Join(root, "tracked.txt")); err != nil || string(got) != "do not touch\n" {
		t.Fatal("tracked work changed before archive validation")
	}
}

func TestGitUntrackedRejectsEscapingArchives(t *testing.T) {
	for _, name := range []string{"../outside", "/absolute", ".git/config", "nested/../escape"} {
		t.Run(name, func(t *testing.T) {
			root := newGitRecoveryRepo(t)
			data := recoveryCapture(t, root)
			var archive bytes.Buffer
			tw := tar.NewWriter(&archive)
			if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: 3, Typeflag: tar.TypeReg}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte("bad")); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(data.RollbackPath, data.Git.UntrackedArchive)
			if err := os.WriteFile(path, archive.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			// Even a matching checksum must not authorize unsafe destinations.
			hash := sha256.Sum256(archive.Bytes())
			data.Git.Artifacts[data.Git.UntrackedArchive] = GitRollbackArtifact{Size: int64(archive.Len()), SHA256: hex.EncodeToString(hash[:])}
			recoveryWrite(t, root, "tracked.txt", []byte("untouched\n"))
			if err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{Force: true}); err == nil {
				t.Fatal("unsafe archive accepted")
			}
			if got, err := os.ReadFile(filepath.Join(root, "tracked.txt")); err != nil || string(got) != "untouched\n" {
				t.Fatal("unsafe archive caused tracked mutation")
			}
		})
	}
}

func TestGitUntrackedDoesNotTraverseChangedParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink fixture")
	}
	root := newGitRecoveryRepo(t)
	recoveryWrite(t, root, "dir/extra.txt", []byte("original\n"))
	data := recoveryCapture(t, root)
	if err := os.Rename(filepath.Join(root, "dir"), filepath.Join(t.TempDir(), "saved")); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	recoveryWrite(t, outside, "extra.txt", []byte("outside\n"))
	if err := os.Symlink(outside, filepath.Join(root, "dir")); err != nil {
		t.Fatal(err)
	}
	if err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{Force: true}); err == nil {
		t.Fatal("restore followed symlink parent")
	}
	if got, err := os.ReadFile(filepath.Join(outside, "extra.txt")); err != nil || string(got) != "outside\n" {
		t.Fatal("restore wrote outside the worktree")
	}
}

func TestGitUntrackedBudgetAndNestedRepositories(t *testing.T) {
	root := newGitRecoveryRepo(t)
	recoveryWrite(t, root, "extra.bin", bytes.Repeat([]byte{0}, 256<<10))
	if data, err := CaptureRollbackState(context.Background(), &db.Request{
		ID: "too-large", ProjectPath: t.TempDir(), Command: db.CommandSpec{Raw: "git clean -fd", Cwd: root},
	}, RollbackCaptureOptions{MaxSizeBytes: 4096}); err == nil || data != nil {
		t.Fatalf("untracked bytes bypassed budget: %v, %v", data, err)
	}
	if err := os.Rename(filepath.Join(root, "extra.bin"), filepath.Join(t.TempDir(), "saved")); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	recoveryGit(t, nested, "init", "-q")
	if data, err := CaptureRollbackState(context.Background(), &db.Request{
		ID: "nested", ProjectPath: t.TempDir(), Command: db.CommandSpec{Raw: "git clean -ffd", Cwd: root},
	}, RollbackCaptureOptions{}); err == nil || data != nil {
		t.Fatalf("partial nested-repo capture accepted: %v, %v", data, err)
	}
}

func TestGitCleanIgnoredFlagDetection(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"git", "clean", "-fdx"}, true},
		{[]string{"git", "-C", "elsewhere", "clean", "-fdX"}, true},
		{[]string{"git", "clean", "-fd", "-e", "x"}, false},
		{[]string{"git", "clean", "-fex"}, false},
		{[]string{"git", "clean", "--", "-x"}, false},
		{[]string{"git", "reset", "--hard", "x"}, false},
	} {
		if got := gitCleanRemovesIgnored(tc.args); got != tc.want {
			t.Errorf("%q: got %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestGitCleanRollbackInLinkedWorktree(t *testing.T) {
	root := newGitRecoveryRepo(t)
	worktree := filepath.Join(t.TempDir(), "linked worktree")
	recoveryGit(t, root, "worktree", "add", "-b", "linked", worktree, "HEAD")
	recoveryWrite(t, worktree, "extra.txt", []byte("linked worktree content\n"))
	data, err := CaptureRollbackState(context.Background(), &db.Request{
		ID: "linked-clean", ProjectPath: worktree,
		Command: db.CommandSpec{Raw: "git clean -fdx", Argv: []string{"git", "clean", "-fdx"}, Cwd: worktree},
	}, RollbackCaptureOptions{MaxSizeBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	gitDir := strings.TrimSuffix(recoveryGit(t, worktree, "rev-parse", "--absolute-git-dir"), "\n")
	if !strings.HasPrefix(data.RollbackPath, gitDir+string(os.PathSeparator)) {
		t.Fatalf("snapshot not in linked worktree's Git directory: %q", data.RollbackPath)
	}
	recoveryGit(t, worktree, "clean", "-fdx")
	loaded, err := LoadRollbackData(data.RollbackPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := RestoreRollbackState(context.Background(), loaded, RollbackRestoreOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(worktree, "extra.txt")); err != nil || string(got) != "linked worktree content\n" {
		t.Fatalf("linked worktree data not recovered: %q, %v", got, err)
	}
	if got := strings.TrimSpace(recoveryGit(t, root, "rev-parse", "--abbrev-ref", "HEAD")); got != "main" {
		t.Fatalf("rollback switched the main worktree: %q", got)
	}
}
