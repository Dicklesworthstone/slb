package core

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestRollbackFilesystemCaptureAndRestore(t *testing.T) {
	project := t.TempDir()
	work := filepath.Join(project, "work")
	if err := os.MkdirAll(work, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	targetDir := filepath.Join(work, "build")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatalf("mkdir build: %v", err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "a.txt"), []byte("hello"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	req := &db.Request{
		ID:          "test-req",
		ProjectPath: project,
		Command: db.CommandSpec{
			Raw:   "rm -rf build",
			Cwd:   work,
			Shell: false,
		},
	}

	data, err := CaptureRollbackState(context.Background(), req, RollbackCaptureOptions{MaxSizeBytes: 10 << 20})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if data == nil || data.Filesystem == nil {
		t.Fatalf("expected filesystem rollback data")
	}

	tarPath := filepath.Join(data.RollbackPath, data.Filesystem.TarGz)
	if _, err := os.Stat(tarPath); err != nil {
		t.Fatalf("missing tar.gz: %v", err)
	}

	// Simulate deletion.
	if err := os.RemoveAll(targetDir); err != nil {
		t.Fatalf("remove build: %v", err)
	}
	if _, err := os.Stat(targetDir); err == nil {
		t.Fatalf("expected build dir removed")
	}

	loaded, err := LoadRollbackData(data.RollbackPath)
	if err != nil {
		t.Fatalf("load rollback: %v", err)
	}

	if err := RestoreRollbackState(context.Background(), loaded, RollbackRestoreOptions{}); err != nil {
		t.Fatalf("restore: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(targetDir, "a.txt"))
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("unexpected restored content: %q", string(got))
	}
}

func TestRollbackFilesystemCaptureStoresSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink tests are not reliable on windows")
	}

	project := t.TempDir()
	work := filepath.Join(project, "work")
	if err := os.MkdirAll(work, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	targetDir := filepath.Join(work, "build")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatalf("mkdir build: %v", err)
	}

	realFile := filepath.Join(targetDir, "real.txt")
	if err := os.WriteFile(realFile, []byte("hello"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	linkFile := filepath.Join(targetDir, "link.txt")
	if err := os.Symlink("real.txt", linkFile); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	req := &db.Request{
		ID:          "test-symlink",
		ProjectPath: project,
		Command: db.CommandSpec{
			Raw:   "rm -rf build",
			Cwd:   work,
			Shell: false,
		},
	}

	data, err := CaptureRollbackState(context.Background(), req, RollbackCaptureOptions{MaxSizeBytes: 10 << 20})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if data == nil || data.Filesystem == nil {
		t.Fatalf("expected filesystem rollback data")
	}

	tarPath := filepath.Join(data.RollbackPath, data.Filesystem.TarGz)
	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatalf("open tar.gz: %v", err)
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	wantName := "p0/link.txt"
	found := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		if hdr.Name != wantName {
			continue
		}
		found = true
		if hdr.Typeflag != tar.TypeSymlink {
			t.Fatalf("expected %s to be symlink, got type=%v", wantName, hdr.Typeflag)
		}
		if strings.TrimSpace(hdr.Linkname) != "real.txt" {
			t.Fatalf("expected symlink linkname real.txt, got %q", hdr.Linkname)
		}
	}
	if !found {
		t.Fatalf("expected symlink entry %s in tar", wantName)
	}
}

func TestRollbackFilesystemRestoreRefusesSymlinkParents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink tests are not reliable on windows")
	}

	project := t.TempDir()
	work := filepath.Join(project, "work")
	if err := os.MkdirAll(work, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	buildDir := filepath.Join(work, "build")
	subDir := filepath.Join(buildDir, "sub")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "a.txt"), []byte("hello"), 0644); err != nil {
		t.Fatalf("write a: %v", err)
	}

	req := &db.Request{
		ID:          "test-symlink-parent",
		ProjectPath: project,
		Command: db.CommandSpec{
			Raw:   "rm -rf build",
			Cwd:   work,
			Shell: false,
		},
	}

	data, err := CaptureRollbackState(context.Background(), req, RollbackCaptureOptions{MaxSizeBytes: 10 << 20})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if data == nil || data.Filesystem == nil {
		t.Fatalf("expected filesystem rollback data")
	}

	// Simulate deletion.
	if err := os.RemoveAll(buildDir); err != nil {
		t.Fatalf("remove build: %v", err)
	}

	// Create a symlink in the restore parent chain (build/sub -> outside).
	if err := os.MkdirAll(buildDir, 0755); err != nil {
		t.Fatalf("mkdir build: %v", err)
	}
	outside := filepath.Join(work, "outside")
	if err := os.MkdirAll(outside, 0755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	if err := os.Symlink(outside, subDir); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	loaded, err := LoadRollbackData(data.RollbackPath)
	if err != nil {
		t.Fatalf("load rollback: %v", err)
	}

	if err := RestoreRollbackState(context.Background(), loaded, RollbackRestoreOptions{}); err == nil {
		t.Fatalf("expected restore to fail due to symlink parent, got nil")
	}
	if _, err := os.Stat(filepath.Join(outside, "a.txt")); err == nil {
		t.Fatalf("restore wrote through symlink parent to outside path")
	}
}

func TestRollbackGitCaptureWritesMetadata(t *testing.T) {
	if _, err := execLookPath("git"); err != nil {
		t.Skip("git not available")
	}

	project := t.TempDir()
	repo := filepath.Join(project, "repo")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}

	if _, err := runCmdString(context.Background(), repo, "git", "init"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	_, _ = runCmdString(context.Background(), repo, "git", "config", "user.name", "Test")
	_, _ = runCmdString(context.Background(), repo, "git", "config", "user.email", "test@example.com")

	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if _, err := runCmdString(context.Background(), repo, "git", "add", "."); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := runCmdString(context.Background(), repo, "git", "commit", "-m", "init"); err != nil {
		t.Fatalf("git commit: %v", err)
	}

	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("modified\n"), 0644); err != nil {
		t.Fatalf("modify a: %v", err)
	}

	req := &db.Request{
		ID:          "test-git",
		ProjectPath: project,
		Command: db.CommandSpec{
			Raw: "git reset --hard HEAD",
			Cwd: repo,
		},
	}
	data, err := CaptureRollbackState(context.Background(), req, RollbackCaptureOptions{})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if data == nil || data.Git == nil {
		t.Fatalf("expected git rollback data")
	}
	if data.Git.Head == "" {
		t.Fatalf("expected head hash")
	}
	diffPath := filepath.Join(data.RollbackPath, filepath.FromSlash(data.Git.DiffFile))
	b, err := os.ReadFile(diffPath)
	if err != nil {
		t.Fatalf("read diff: %v", err)
	}
	if !strings.Contains(string(b), "a.txt") {
		t.Fatalf("expected diff to mention a.txt")
	}
}

func TestRollbackKubernetesCaptureAndRestoreWithFakeKubectl(t *testing.T) {
	project := t.TempDir()
	work := filepath.Join(project, "work")
	if err := os.MkdirAll(work, 0755); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}

	binDir := filepath.Join(project, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	logPath := filepath.Join(project, "kubectl.log")
	t.Setenv("KUBECTL_LOG", logPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	kubectlPath := filepath.Join(binDir, "kubectl")
	script := "#!/bin/sh\nset -eu\ncmd=\"$1\"\nshift\ncase \"$cmd\" in\n  get)\n    kind=\"$1\"; name=\"$2\";\n    echo \"kind: $kind\"\n    echo \"metadata:\"\n    echo \"  name: $name\"\n    ;;\n  apply)\n    echo \"apply $*\" >> \"${KUBECTL_LOG}\"\n    ;;\n  *)\n    ;;\nesac\n"
	if runtime.GOOS == "windows" {
		t.Skip("shell script kubectl not supported on windows")
	}
	if err := os.WriteFile(kubectlPath, []byte(script), 0755); err != nil {
		t.Fatalf("write kubectl: %v", err)
	}

	req := &db.Request{
		ID:          "test-k8s",
		ProjectPath: project,
		Command: db.CommandSpec{
			Raw: "kubectl delete deployment myapp",
			Cwd: work,
		},
	}
	data, err := CaptureRollbackState(context.Background(), req, RollbackCaptureOptions{})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if data == nil || data.Kubernetes == nil {
		t.Fatalf("expected kubernetes rollback data")
	}
	if len(data.Kubernetes.Manifests) != 1 {
		t.Fatalf("expected 1 manifest, got %d", len(data.Kubernetes.Manifests))
	}

	loaded, err := LoadRollbackData(data.RollbackPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := RestoreRollbackState(context.Background(), loaded, RollbackRestoreOptions{}); err != nil {
		t.Fatalf("restore: %v", err)
	}

	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read kubectl log: %v", err)
	}
	if !strings.Contains(string(b), "apply") {
		t.Fatalf("expected kubectl apply to be invoked, got: %q", string(b))
	}
}

func execLookPath(name string) (string, error) {
	return exec.LookPath(name)
}

func TestBytesTrimSpace(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  []byte
	}{
		{"empty", []byte{}, []byte{}},
		{"no whitespace", []byte("hello"), []byte("hello")},
		{"leading space", []byte("  hello"), []byte("hello")},
		{"trailing space", []byte("hello  "), []byte("hello")},
		{"both sides", []byte("  hello  "), []byte("hello")},
		{"leading tab", []byte("\thello"), []byte("hello")},
		{"trailing newline", []byte("hello\n"), []byte("hello")},
		{"mixed whitespace", []byte(" \t\nhello world\n\t "), []byte("hello world")},
		{"only whitespace", []byte("   \t\n  "), []byte{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := bytesTrimSpace(tc.input)
			if string(got) != string(tc.want) {
				t.Errorf("bytesTrimSpace(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestCleanupOldRollbackCaptures(t *testing.T) {
	t.Run("zero retention does nothing", func(t *testing.T) {
		tmpDir := t.TempDir()
		err := cleanupOldRollbackCaptures(tmpDir, 0, time.Now())
		if err != nil {
			t.Errorf("cleanupOldRollbackCaptures error = %v", err)
		}
	})

	t.Run("negative retention does nothing", func(t *testing.T) {
		tmpDir := t.TempDir()
		err := cleanupOldRollbackCaptures(tmpDir, -1*time.Hour, time.Now())
		if err != nil {
			t.Errorf("cleanupOldRollbackCaptures error = %v", err)
		}
	})

	t.Run("nonexistent directory returns nil", func(t *testing.T) {
		err := cleanupOldRollbackCaptures("/nonexistent/path/xyz", time.Hour, time.Now())
		if err != nil {
			t.Errorf("expected nil error for nonexistent directory, got %v", err)
		}
	})

	t.Run("ignores non-req directories", func(t *testing.T) {
		tmpDir := t.TempDir()
		// Create a directory that doesn't start with "req-"
		otherDir := filepath.Join(tmpDir, "other-dir")
		if err := os.MkdirAll(otherDir, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		// Set modification time to be old
		oldTime := time.Now().Add(-2 * time.Hour)
		if err := os.Chtimes(otherDir, oldTime, oldTime); err != nil {
			t.Fatalf("chtimes: %v", err)
		}

		err := cleanupOldRollbackCaptures(tmpDir, time.Hour, time.Now())
		if err != nil {
			t.Errorf("cleanupOldRollbackCaptures error = %v", err)
		}

		// Directory should still exist
		if _, err := os.Stat(otherDir); os.IsNotExist(err) {
			t.Error("expected non-req directory to not be deleted")
		}
	})

	t.Run("ignores files", func(t *testing.T) {
		tmpDir := t.TempDir()
		// Create a file named req-something
		reqFile := filepath.Join(tmpDir, "req-file")
		if err := os.WriteFile(reqFile, []byte("test"), 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}

		err := cleanupOldRollbackCaptures(tmpDir, time.Hour, time.Now())
		if err != nil {
			t.Errorf("cleanupOldRollbackCaptures error = %v", err)
		}

		// File should still exist
		if _, err := os.Stat(reqFile); os.IsNotExist(err) {
			t.Error("expected file to not be deleted")
		}
	})

	t.Run("deletes old req- directories", func(t *testing.T) {
		tmpDir := t.TempDir()
		// Create an old req- directory
		oldReqDir := filepath.Join(tmpDir, "req-old-request")
		if err := os.MkdirAll(oldReqDir, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		// Set modification time to be old
		oldTime := time.Now().Add(-2 * time.Hour)
		if err := os.Chtimes(oldReqDir, oldTime, oldTime); err != nil {
			t.Fatalf("chtimes: %v", err)
		}

		err := cleanupOldRollbackCaptures(tmpDir, time.Hour, time.Now())
		if err != nil {
			t.Errorf("cleanupOldRollbackCaptures error = %v", err)
		}

		// Directory should be deleted
		if _, err := os.Stat(oldReqDir); !os.IsNotExist(err) {
			t.Error("expected old req- directory to be deleted")
		}
	})

	t.Run("keeps recent req- directories", func(t *testing.T) {
		tmpDir := t.TempDir()
		// Create a recent req- directory
		recentReqDir := filepath.Join(tmpDir, "req-recent-request")
		if err := os.MkdirAll(recentReqDir, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		// Modification time is already recent (just created)

		err := cleanupOldRollbackCaptures(tmpDir, time.Hour, time.Now())
		if err != nil {
			t.Errorf("cleanupOldRollbackCaptures error = %v", err)
		}

		// Directory should still exist
		if _, err := os.Stat(recentReqDir); os.IsNotExist(err) {
			t.Error("expected recent req- directory to not be deleted")
		}
	})

	t.Run("deletes only expired directories", func(t *testing.T) {
		tmpDir := t.TempDir()

		// Create an old req- directory
		oldReqDir := filepath.Join(tmpDir, "req-old")
		if err := os.MkdirAll(oldReqDir, 0755); err != nil {
			t.Fatalf("mkdir old: %v", err)
		}
		oldTime := time.Now().Add(-2 * time.Hour)
		if err := os.Chtimes(oldReqDir, oldTime, oldTime); err != nil {
			t.Fatalf("chtimes old: %v", err)
		}

		// Create a recent req- directory
		recentReqDir := filepath.Join(tmpDir, "req-recent")
		if err := os.MkdirAll(recentReqDir, 0755); err != nil {
			t.Fatalf("mkdir recent: %v", err)
		}

		err := cleanupOldRollbackCaptures(tmpDir, time.Hour, time.Now())
		if err != nil {
			t.Errorf("cleanupOldRollbackCaptures error = %v", err)
		}

		// Old directory should be deleted
		if _, err := os.Stat(oldReqDir); !os.IsNotExist(err) {
			t.Error("expected old req- directory to be deleted")
		}

		// Recent directory should still exist
		if _, err := os.Stat(recentReqDir); os.IsNotExist(err) {
			t.Error("expected recent req- directory to not be deleted")
		}
	})
}
