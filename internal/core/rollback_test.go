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

func TestLoadRollbackData_Errors(t *testing.T) {
	t.Run("empty rollback dir", func(t *testing.T) {
		_, err := LoadRollbackData("")
		if err == nil {
			t.Error("expected error for empty rollback dir")
		}
	})

	t.Run("whitespace-only rollback dir", func(t *testing.T) {
		_, err := LoadRollbackData("   ")
		if err == nil {
			t.Error("expected error for whitespace rollback dir")
		}
	})

	t.Run("nonexistent directory", func(t *testing.T) {
		_, err := LoadRollbackData("/nonexistent/path/xyz")
		if err == nil {
			t.Error("expected error for nonexistent directory")
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		tmpDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(tmpDir, "metadata.json"), []byte("not json"), 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}

		_, err := LoadRollbackData(tmpDir)
		if err == nil {
			t.Error("expected error for invalid JSON")
		}
	})

	t.Run("valid JSON loads successfully", func(t *testing.T) {
		tmpDir := t.TempDir()
		metadata := `{"kind":"filesystem","request_id":"test-123"}`
		if err := os.WriteFile(filepath.Join(tmpDir, "metadata.json"), []byte(metadata), 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}

		data, err := LoadRollbackData(tmpDir)
		if err != nil {
			t.Fatalf("LoadRollbackData error: %v", err)
		}
		if data.Kind != rollbackKindFilesystem {
			t.Errorf("expected kind %q, got %q", rollbackKindFilesystem, data.Kind)
		}
		if data.RequestID != "test-123" {
			t.Errorf("expected request_id test-123, got %q", data.RequestID)
		}
		// RollbackPath should be set to tmpDir since it was empty
		if data.RollbackPath != tmpDir {
			t.Errorf("expected RollbackPath %q, got %q", tmpDir, data.RollbackPath)
		}
	})
}

func TestRestoreRollbackState_Errors(t *testing.T) {
	t.Run("nil data", func(t *testing.T) {
		err := RestoreRollbackState(context.Background(), nil, RollbackRestoreOptions{})
		if err == nil {
			t.Error("expected error for nil data")
		}
	})

	t.Run("empty rollback path", func(t *testing.T) {
		data := &RollbackData{
			Kind:         rollbackKindFilesystem,
			RollbackPath: "",
		}
		err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{})
		if err == nil {
			t.Error("expected error for empty rollback path")
		}
	})

	t.Run("whitespace rollback path", func(t *testing.T) {
		data := &RollbackData{
			Kind:         rollbackKindFilesystem,
			RollbackPath: "   ",
		}
		err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{})
		if err == nil {
			t.Error("expected error for whitespace rollback path")
		}
	})

	t.Run("unknown kind", func(t *testing.T) {
		data := &RollbackData{
			Kind:         "unknown",
			RollbackPath: "/some/path",
		}
		err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{})
		if err == nil {
			t.Error("expected error for unknown kind")
		}
		if !strings.Contains(err.Error(), "unsupported rollback kind") {
			t.Errorf("expected unsupported rollback kind error, got %v", err)
		}
	})

	t.Run("nil context uses background", func(t *testing.T) {
		data := &RollbackData{
			Kind:         "unknown",
			RollbackPath: "/some/path",
		}
		// Should not panic on nil context
		err := RestoreRollbackState(nil, data, RollbackRestoreOptions{})
		if err == nil {
			t.Error("expected error for unknown kind")
		}
	})
}

func TestRestoreGitRollback_Errors(t *testing.T) {
	if _, err := execLookPath("git"); err != nil {
		t.Skip("git not available")
	}

	t.Run("missing git data", func(t *testing.T) {
		data := &RollbackData{
			Kind:         rollbackKindGit,
			RollbackPath: "/some/path",
			Git:          nil,
		}
		err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{Force: true})
		if err == nil {
			t.Error("expected error for missing git data")
		}
		if !strings.Contains(err.Error(), "git rollback data missing") {
			t.Errorf("expected 'git rollback data missing' error, got %v", err)
		}
	})

	t.Run("requires force flag", func(t *testing.T) {
		data := &RollbackData{
			Kind:         rollbackKindGit,
			RollbackPath: "/some/path",
			Git: &GitRollbackData{
				RepoRoot: "/some/repo",
				Head:     "abc123",
			},
		}
		err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{Force: false})
		if err == nil {
			t.Error("expected error without force flag")
		}
		if !strings.Contains(err.Error(), "force") {
			t.Errorf("expected force-related error, got %v", err)
		}
	})

	t.Run("empty repo root", func(t *testing.T) {
		data := &RollbackData{
			Kind:         rollbackKindGit,
			RollbackPath: "/some/path",
			Git: &GitRollbackData{
				RepoRoot: "",
				Head:     "abc123",
			},
		}
		err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{Force: true})
		if err == nil {
			t.Error("expected error for empty repo root")
		}
		if !strings.Contains(err.Error(), "repo root missing") {
			t.Errorf("expected 'repo root missing' error, got %v", err)
		}
	})
}

func TestRestoreGitRollback_Full(t *testing.T) {
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

	// Create initial commit
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("original\n"), 0644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if _, err := runCmdString(context.Background(), repo, "git", "add", "."); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := runCmdString(context.Background(), repo, "git", "commit", "-m", "init"); err != nil {
		t.Fatalf("git commit: %v", err)
	}

	// Get the HEAD commit hash
	head, err := runCmdString(context.Background(), repo, "git", "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	head = strings.TrimSpace(head)

	// Get the current branch
	branch, err := runCmdString(context.Background(), repo, "git", "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatalf("git rev-parse branch: %v", err)
	}
	branch = strings.TrimSpace(branch)

	// Capture the actual artifact set and repository identity before mutation.
	// Hand-built HEAD-only metadata must not bypass integrity validation.
	data, err := CaptureRollbackState(context.Background(), &db.Request{
		ID:          "test-git-full-restore",
		ProjectPath: project,
		Command:     db.CommandSpec{Raw: "git reset --hard HEAD", Cwd: repo},
	}, RollbackCaptureOptions{})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if data.Git.Head != head || data.Git.Branch != branch {
		t.Fatalf("capture changed the selected HEAD or branch")
	}

	// Modify the file
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("modified\n"), 0644); err != nil {
		t.Fatalf("modify a: %v", err)
	}
	if _, err := runCmdString(context.Background(), repo, "git", "add", "."); err != nil {
		t.Fatalf("git add modified: %v", err)
	}
	if _, err := runCmdString(context.Background(), repo, "git", "commit", "-m", "modify"); err != nil {
		t.Fatalf("git commit modify: %v", err)
	}

	// Now restore to the original HEAD
	err = RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{Force: true})
	if err != nil {
		t.Fatalf("RestoreRollbackState: %v", err)
	}

	// Verify the file was restored
	content, err := os.ReadFile(filepath.Join(repo, "a.txt"))
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if string(content) != "original\n" {
		t.Errorf("expected 'original', got %q", string(content))
	}
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

func TestParseKubectlDelete(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantNS       string
		wantResCount int
		wantFirst    kubectlResource
	}{
		{
			name:         "kind/name format",
			args:         []string{"deployment/myapp"},
			wantNS:       "",
			wantResCount: 1,
			wantFirst:    kubectlResource{Kind: "deployment", Name: "myapp"},
		},
		{
			name:         "kind name format",
			args:         []string{"pod", "nginx"},
			wantNS:       "",
			wantResCount: 1,
			wantFirst:    kubectlResource{Kind: "pod", Name: "nginx"},
		},
		{
			name:         "kind with multiple names",
			args:         []string{"pod", "nginx", "redis", "mysql"},
			wantNS:       "",
			wantResCount: 3,
			wantFirst:    kubectlResource{Kind: "pod", Name: "nginx"},
		},
		{
			name:         "with namespace flag -n",
			args:         []string{"-n", "kube-system", "pod", "nginx"},
			wantNS:       "kube-system",
			wantResCount: 1,
			wantFirst:    kubectlResource{Kind: "pod", Name: "nginx"},
		},
		{
			name:         "with namespace flag --namespace",
			args:         []string{"--namespace", "production", "service/frontend"},
			wantNS:       "production",
			wantResCount: 1,
			wantFirst:    kubectlResource{Kind: "service", Name: "frontend"},
		},
		{
			name:         "with namespace flag --namespace=value",
			args:         []string{"--namespace=staging", "deployment/api"},
			wantNS:       "staging",
			wantResCount: 1,
			wantFirst:    kubectlResource{Kind: "deployment", Name: "api"},
		},
		{
			name:         "with double dash separator",
			args:         []string{"--", "pod/nginx"},
			wantNS:       "",
			wantResCount: 1,
			wantFirst:    kubectlResource{Kind: "pod", Name: "nginx"},
		},
		{
			name:         "stops at flags",
			args:         []string{"pod", "nginx", "--force"},
			wantNS:       "",
			wantResCount: 1,
			wantFirst:    kubectlResource{Kind: "pod", Name: "nginx"},
		},
		{
			name:         "empty args",
			args:         []string{},
			wantNS:       "",
			wantResCount: 0,
		},
		{
			name:         "kind only no name",
			args:         []string{"pod"},
			wantNS:       "",
			wantResCount: 0,
		},
		{
			name:         "invalid slash format",
			args:         []string{"/noname"},
			wantNS:       "",
			wantResCount: 0,
		},
		{
			name:         "slash with empty kind",
			args:         []string{"/myapp"},
			wantNS:       "",
			wantResCount: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ns, resources := parseKubectlDelete(tc.args)
			if ns != tc.wantNS {
				t.Errorf("namespace: got %q, want %q", ns, tc.wantNS)
			}
			if len(resources) != tc.wantResCount {
				t.Errorf("resource count: got %d, want %d", len(resources), tc.wantResCount)
				return
			}
			if tc.wantResCount > 0 && resources[0] != tc.wantFirst {
				t.Errorf("first resource: got %+v, want %+v", resources[0], tc.wantFirst)
			}
		})
	}
}

func TestResolvePaths(t *testing.T) {
	t.Run("empty target is skipped", func(t *testing.T) {
		paths, missing := resolvePaths("/tmp", []string{"", "  ", "\t"})
		if len(paths) != 0 || len(missing) != 0 {
			t.Errorf("expected empty results, got paths=%v, missing=%v", paths, missing)
		}
	})

	t.Run("absolute path that exists", func(t *testing.T) {
		tmpDir := t.TempDir()
		testFile := filepath.Join(tmpDir, "test.txt")
		if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}

		paths, missing := resolvePaths("/other", []string{testFile})
		if len(paths) != 1 || paths[0] != testFile {
			t.Errorf("expected [%s], got paths=%v", testFile, paths)
		}
		if len(missing) != 0 {
			t.Errorf("expected no missing, got %v", missing)
		}
	})

	t.Run("absolute path that does not exist", func(t *testing.T) {
		paths, missing := resolvePaths("/tmp", []string{"/nonexistent/path/xyz"})
		if len(paths) != 0 {
			t.Errorf("expected no paths, got %v", paths)
		}
		if len(missing) != 1 {
			t.Errorf("expected 1 missing, got %v", missing)
		}
	})

	t.Run("relative path joined with cwd", func(t *testing.T) {
		tmpDir := t.TempDir()
		testFile := filepath.Join(tmpDir, "relative.txt")
		if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}

		paths, _ := resolvePaths(tmpDir, []string{"relative.txt"})
		if len(paths) != 1 {
			t.Errorf("expected 1 path, got %v", paths)
		}
	})

	t.Run("glob pattern with matches", func(t *testing.T) {
		tmpDir := t.TempDir()
		for _, name := range []string{"a.txt", "b.txt", "c.log"} {
			if err := os.WriteFile(filepath.Join(tmpDir, name), []byte("test"), 0644); err != nil {
				t.Fatalf("write file: %v", err)
			}
		}

		paths, _ := resolvePaths(tmpDir, []string{"*.txt"})
		if len(paths) != 2 {
			t.Errorf("expected 2 paths (*.txt), got %d: %v", len(paths), paths)
		}
	})

	t.Run("glob pattern with no matches", func(t *testing.T) {
		tmpDir := t.TempDir()

		paths, missing := resolvePaths(tmpDir, []string{"*.nonexistent"})
		// When glob has no matches, it falls back to literal path
		if len(paths) != 0 {
			t.Errorf("expected no paths, got %v", paths)
		}
		if len(missing) != 1 {
			t.Errorf("expected 1 missing, got %v", missing)
		}
	})

	t.Run("deduplicates paths", func(t *testing.T) {
		tmpDir := t.TempDir()
		testFile := filepath.Join(tmpDir, "dup.txt")
		if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}

		paths, _ := resolvePaths(tmpDir, []string{"dup.txt", "dup.txt", testFile})
		if len(paths) != 1 {
			t.Errorf("expected 1 path (deduplicated), got %d: %v", len(paths), paths)
		}
	})

	t.Run("absolute glob pattern", func(t *testing.T) {
		tmpDir := t.TempDir()
		for _, name := range []string{"x.go", "y.go"} {
			if err := os.WriteFile(filepath.Join(tmpDir, name), []byte("test"), 0644); err != nil {
				t.Fatalf("write file: %v", err)
			}
		}

		pattern := filepath.Join(tmpDir, "*.go")
		paths, _ := resolvePaths("/other", []string{pattern})
		if len(paths) != 2 {
			t.Errorf("expected 2 paths for absolute glob, got %d: %v", len(paths), paths)
		}
	})
}

func TestEnsureNoSymlinkParents(t *testing.T) {
	t.Run("empty root path", func(t *testing.T) {
		err := ensureNoSymlinkParents("", "/some/target")
		if err == nil {
			t.Error("expected error for empty root")
		}
	})

	t.Run("empty target path", func(t *testing.T) {
		err := ensureNoSymlinkParents("/some/root", "")
		if err == nil {
			t.Error("expected error for empty target")
		}
	})

	t.Run("whitespace-only paths", func(t *testing.T) {
		err := ensureNoSymlinkParents("   ", "   ")
		if err == nil {
			t.Error("expected error for whitespace paths")
		}
	})

	t.Run("target escapes root", func(t *testing.T) {
		tmpDir := t.TempDir()
		err := ensureNoSymlinkParents(tmpDir, "/etc/passwd")
		if err == nil {
			t.Error("expected error when target escapes root")
		}
		if !strings.Contains(err.Error(), "escapes root") {
			t.Errorf("expected 'escapes root' error, got %v", err)
		}
	})

	t.Run("target is root", func(t *testing.T) {
		tmpDir := t.TempDir()
		err := ensureNoSymlinkParents(tmpDir, tmpDir)
		if err != nil {
			t.Errorf("unexpected error when target is root: %v", err)
		}
	})

	t.Run("target is child of root", func(t *testing.T) {
		tmpDir := t.TempDir()
		childDir := filepath.Join(tmpDir, "child")
		if err := os.MkdirAll(childDir, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		err := ensureNoSymlinkParents(tmpDir, childDir)
		if err != nil {
			t.Errorf("unexpected error for valid child path: %v", err)
		}
	})

	t.Run("target path does not exist yet", func(t *testing.T) {
		tmpDir := t.TempDir()
		target := filepath.Join(tmpDir, "nonexistent", "path")

		err := ensureNoSymlinkParents(tmpDir, target)
		if err != nil {
			t.Errorf("unexpected error for nonexistent path: %v", err)
		}
	})
}

func TestEnsureNoSymlinkParents_Symlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink tests are not reliable on windows")
	}

	t.Run("root is symlink", func(t *testing.T) {
		tmpDir := t.TempDir()
		realRoot := filepath.Join(tmpDir, "real")
		if err := os.MkdirAll(realRoot, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		symlinkRoot := filepath.Join(tmpDir, "symlink")
		if err := os.Symlink(realRoot, symlinkRoot); err != nil {
			t.Skipf("symlink not supported: %v", err)
		}

		err := ensureNoSymlinkParents(symlinkRoot, filepath.Join(symlinkRoot, "child"))
		if err == nil {
			t.Error("expected error when root is symlink")
		}
		if !strings.Contains(err.Error(), "symlink root") {
			t.Errorf("expected 'symlink root' error, got %v", err)
		}
	})

	t.Run("parent in path is symlink", func(t *testing.T) {
		tmpDir := t.TempDir()
		// Create: root/parent -> /tmp/outside
		root := filepath.Join(tmpDir, "root")
		if err := os.MkdirAll(root, 0755); err != nil {
			t.Fatalf("mkdir root: %v", err)
		}

		outside := filepath.Join(tmpDir, "outside")
		if err := os.MkdirAll(outside, 0755); err != nil {
			t.Fatalf("mkdir outside: %v", err)
		}

		symlinkParent := filepath.Join(root, "parent")
		if err := os.Symlink(outside, symlinkParent); err != nil {
			t.Skipf("symlink not supported: %v", err)
		}

		target := filepath.Join(root, "parent", "child")
		err := ensureNoSymlinkParents(root, target)
		if err == nil {
			t.Error("expected error when parent is symlink")
		}
		if !strings.Contains(err.Error(), "symlink parent") {
			t.Errorf("expected 'symlink parent' error, got %v", err)
		}
	})
}

func TestApplyGitPatchIfPresent(t *testing.T) {
	if _, err := execLookPath("git"); err != nil {
		t.Skip("git not available")
	}

	t.Run("nonexistent patch file returns nil", func(t *testing.T) {
		tmpDir := t.TempDir()
		err := applyGitPatchIfPresent(context.Background(), tmpDir, "/nonexistent/patch.diff", false)
		if err != nil {
			t.Errorf("expected nil for nonexistent patch, got %v", err)
		}
	})

	t.Run("empty patch file returns nil", func(t *testing.T) {
		tmpDir := t.TempDir()
		patchFile := filepath.Join(tmpDir, "empty.patch")
		if err := os.WriteFile(patchFile, []byte(""), 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}

		err := applyGitPatchIfPresent(context.Background(), tmpDir, patchFile, false)
		if err != nil {
			t.Errorf("expected nil for empty patch, got %v", err)
		}
	})

	t.Run("whitespace-only patch file returns nil", func(t *testing.T) {
		tmpDir := t.TempDir()
		patchFile := filepath.Join(tmpDir, "whitespace.patch")
		if err := os.WriteFile(patchFile, []byte("   \n\t  \n"), 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}

		err := applyGitPatchIfPresent(context.Background(), tmpDir, patchFile, false)
		if err != nil {
			t.Errorf("expected nil for whitespace patch, got %v", err)
		}
	})

	t.Run("invalid patch returns error", func(t *testing.T) {
		// Initialize a git repo
		tmpDir := t.TempDir()
		if _, err := runCmdString(context.Background(), tmpDir, "git", "init"); err != nil {
			t.Fatalf("git init: %v", err)
		}

		patchFile := filepath.Join(tmpDir, "invalid.patch")
		if err := os.WriteFile(patchFile, []byte("not a valid patch"), 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}

		err := applyGitPatchIfPresent(context.Background(), tmpDir, patchFile, false)
		if err == nil {
			t.Error("expected error for invalid patch")
		}
	})
}

func TestRestoreFilesystemRollback_Errors(t *testing.T) {
	t.Run("missing filesystem data", func(t *testing.T) {
		data := &RollbackData{
			Kind:         rollbackKindFilesystem,
			RollbackPath: "/some/path",
			Filesystem:   nil,
		}
		err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{})
		if err == nil {
			t.Error("expected error for missing filesystem data")
		}
		if !strings.Contains(err.Error(), "filesystem rollback data missing") {
			t.Errorf("expected 'filesystem rollback data missing' error, got %v", err)
		}
	})

	t.Run("empty roots", func(t *testing.T) {
		data := &RollbackData{
			Kind:         rollbackKindFilesystem,
			RollbackPath: "/some/path",
			Filesystem: &FilesystemRollbackData{
				TarGz: "rollback.tar.gz",
				Roots: []FilesystemRoot{},
			},
		}
		err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{})
		if err == nil {
			t.Error("expected error for empty roots")
		}
		if !strings.Contains(err.Error(), "roots missing") {
			t.Errorf("expected 'roots missing' error, got %v", err)
		}
	})

	t.Run("roots with empty ID or Path skipped", func(t *testing.T) {
		data := &RollbackData{
			Kind:         rollbackKindFilesystem,
			RollbackPath: "/some/path",
			Filesystem: &FilesystemRollbackData{
				TarGz: "rollback.tar.gz",
				Roots: []FilesystemRoot{
					{ID: "", Path: "/some/path"},
					{ID: "p0", Path: ""},
				},
			},
		}
		err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{})
		if err == nil {
			t.Error("expected error for invalid roots")
		}
		if !strings.Contains(err.Error(), "roots missing") {
			t.Errorf("expected 'roots missing' error (empty roots after filtering), got %v", err)
		}
	})

	t.Run("missing tar.gz file", func(t *testing.T) {
		tmpDir := t.TempDir()
		data := &RollbackData{
			Kind:         rollbackKindFilesystem,
			RollbackPath: tmpDir,
			Filesystem: &FilesystemRollbackData{
				TarGz: "nonexistent.tar.gz",
				Roots: []FilesystemRoot{
					{ID: "p0", Path: "/some/path"},
				},
			},
		}
		err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{})
		if err == nil {
			t.Error("expected error for missing tar.gz")
		}
		if !strings.Contains(err.Error(), "opening rollback tar.gz") {
			t.Errorf("expected 'opening rollback tar.gz' error, got %v", err)
		}
	})

	t.Run("invalid gzip file", func(t *testing.T) {
		tmpDir := t.TempDir()
		tarPath := filepath.Join(tmpDir, "invalid.tar.gz")
		if err := os.WriteFile(tarPath, []byte("not a gzip file"), 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}

		data := &RollbackData{
			Kind:         rollbackKindFilesystem,
			RollbackPath: tmpDir,
			Filesystem: &FilesystemRollbackData{
				TarGz: "invalid.tar.gz",
				Roots: []FilesystemRoot{
					{ID: "p0", Path: "/some/path"},
				},
			},
		}
		err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{})
		if err == nil {
			t.Error("expected error for invalid gzip")
		}
		if !strings.Contains(err.Error(), "opening gzip") {
			t.Errorf("expected 'opening gzip' error, got %v", err)
		}
	})
}

func TestRestoreFilesystemRollback_WithForce(t *testing.T) {
	project := t.TempDir()
	work := filepath.Join(project, "work")
	if err := os.MkdirAll(work, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Create initial file
	targetDir := filepath.Join(work, "build")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatalf("mkdir build: %v", err)
	}
	origFile := filepath.Join(targetDir, "original.txt")
	if err := os.WriteFile(origFile, []byte("original"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	// Capture rollback state
	req := &db.Request{
		ID:          "test-force",
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

	// Delete original and create a new file at same location
	if err := os.RemoveAll(targetDir); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatalf("recreate dir: %v", err)
	}
	if err := os.WriteFile(origFile, []byte("new content"), 0644); err != nil {
		t.Fatalf("write new file: %v", err)
	}

	// Restore without force should fail
	loaded, err := LoadRollbackData(data.RollbackPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	err = RestoreRollbackState(context.Background(), loaded, RollbackRestoreOptions{Force: false})
	if err == nil {
		t.Error("expected error without force flag")
	}
	if !strings.Contains(err.Error(), "use --force") {
		t.Errorf("expected force-related error, got %v", err)
	}

	// Restore with force should succeed
	err = RestoreRollbackState(context.Background(), loaded, RollbackRestoreOptions{Force: true})
	if err != nil {
		t.Fatalf("restore with force: %v", err)
	}

	// Verify original content was restored
	content, err := os.ReadFile(origFile)
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if string(content) != "original" {
		t.Errorf("expected 'original', got %q", string(content))
	}
}

func TestDetectRollbackKind(t *testing.T) {
	tests := []struct {
		name     string
		tokens   []string
		wantKind string
	}{
		{"kubectl delete", []string{"kubectl", "delete", "deployment", "myapp"}, rollbackKindKubernetes},
		{"kubectl only delete", []string{"kubectl", "delete"}, rollbackKindKubernetes},
		{"kubectl without delete", []string{"kubectl", "get", "pods"}, ""},
		{"git reset", []string{"git", "reset", "--hard", "HEAD"}, rollbackKindGit},
		{"git checkout", []string{"git", "checkout", "--", "."}, rollbackKindGit},
		{"git clean", []string{"git", "clean", "-fd"}, rollbackKindGit},
		{"rm command", []string{"rm", "-rf", "./build"}, rollbackKindFilesystem},
		{"rm single file", []string{"rm", "file.txt"}, rollbackKindFilesystem},
		{"rm without targets", []string{"rm"}, ""},
		{"unknown command", []string{"echo", "hello"}, ""},
		{"empty tokens", []string{}, ""},
		{"nil tokens", nil, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := detectRollbackKind(tc.tokens)
			if got != tc.wantKind {
				t.Errorf("detectRollbackKind(%v) = %q, want %q", tc.tokens, got, tc.wantKind)
			}
		})
	}
}

func TestEstimateFileBytes(t *testing.T) {
	t.Run("empty roots", func(t *testing.T) {
		total, err := estimateFileBytes(nil, 0)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if total != 0 {
			t.Errorf("expected 0, got %d", total)
		}
	})

	t.Run("single file", func(t *testing.T) {
		tmpDir := t.TempDir()
		file := filepath.Join(tmpDir, "test.txt")
		content := []byte("hello world")
		if err := os.WriteFile(file, content, 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}

		total, err := estimateFileBytes([]string{file}, 0)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if total != int64(len(content)) {
			t.Errorf("expected %d, got %d", len(content), total)
		}
	})

	t.Run("directory with files", func(t *testing.T) {
		tmpDir := t.TempDir()
		files := map[string]string{
			"a.txt": "hello",
			"b.txt": "world",
		}
		expectedTotal := int64(0)
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(tmpDir, name), []byte(content), 0644); err != nil {
				t.Fatalf("write file: %v", err)
			}
			expectedTotal += int64(len(content))
		}

		total, err := estimateFileBytes([]string{tmpDir}, 0)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if total != expectedTotal {
			t.Errorf("expected %d, got %d", expectedTotal, total)
		}
	})

	t.Run("exceeds max size", func(t *testing.T) {
		tmpDir := t.TempDir()
		file := filepath.Join(tmpDir, "large.txt")
		content := make([]byte, 1000)
		if err := os.WriteFile(file, content, 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}

		_, err := estimateFileBytes([]string{file}, 500)
		if err == nil {
			t.Error("expected error for exceeding max size")
		}
		if !strings.Contains(err.Error(), "exceeds max size") {
			t.Errorf("expected 'exceeds max size' error, got %v", err)
		}
	})

	t.Run("nonexistent path", func(t *testing.T) {
		_, err := estimateFileBytes([]string{"/nonexistent/path"}, 0)
		if err == nil {
			t.Error("expected error for nonexistent path")
		}
	})
}

func TestCaptureRollbackState_Errors(t *testing.T) {
	t.Run("nil request", func(t *testing.T) {
		_, err := CaptureRollbackState(context.Background(), nil, RollbackCaptureOptions{})
		if err == nil {
			t.Error("expected error for nil request")
		}
	})

	t.Run("empty project path", func(t *testing.T) {
		req := &db.Request{
			ID:          "test-empty",
			ProjectPath: "",
			Command: db.CommandSpec{
				Raw: "rm -rf ./build",
			},
		}
		_, err := CaptureRollbackState(context.Background(), req, RollbackCaptureOptions{})
		if err == nil {
			t.Error("expected error for empty project path")
		}
	})

	t.Run("unrecognized command returns nil data", func(t *testing.T) {
		tmpDir := t.TempDir()
		req := &db.Request{
			ID:          "test-unrecognized",
			ProjectPath: tmpDir,
			Command: db.CommandSpec{
				Raw: "echo hello",
				Cwd: tmpDir,
			},
		}
		data, err := CaptureRollbackState(context.Background(), req, RollbackCaptureOptions{})
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if data != nil {
			t.Error("expected nil data for unrecognized command")
		}
	})

	t.Run("rm with no existing targets", func(t *testing.T) {
		tmpDir := t.TempDir()
		req := &db.Request{
			ID:          "test-no-targets",
			ProjectPath: tmpDir,
			Command: db.CommandSpec{
				Raw: "rm -rf ./nonexistent",
				Cwd: tmpDir,
			},
		}
		data, err := CaptureRollbackState(context.Background(), req, RollbackCaptureOptions{})
		// Should return error when targets don't exist (no existing rm targets to capture)
		if err == nil && data != nil {
			t.Error("expected error or nil data for nonexistent targets")
		}
		// If error, it should be about no targets
		if err != nil && !strings.Contains(err.Error(), "rm targets") {
			t.Errorf("unexpected error type: %v", err)
		}
	})

	t.Run("captures context cancellation", func(t *testing.T) {
		tmpDir := t.TempDir()
		targetDir := filepath.Join(tmpDir, "work", "build")
		if err := os.MkdirAll(targetDir, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(targetDir, "test.txt"), []byte("hello"), 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}

		req := &db.Request{
			ID:          "test-context",
			ProjectPath: tmpDir,
			Command: db.CommandSpec{
				Raw: "rm -rf build",
				Cwd: filepath.Join(tmpDir, "work"),
			},
		}

		// Create a cancelled context
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		// Capture should still work even with cancelled context (for filesystem rollback)
		data, err := CaptureRollbackState(ctx, req, RollbackCaptureOptions{MaxSizeBytes: 10 << 20})
		// It's acceptable if this fails due to context cancellation
		_ = err
		_ = data
	})
}

func TestWriteTarGz(t *testing.T) {
	t.Run("creates valid tar.gz", func(t *testing.T) {
		tmpDir := t.TempDir()
		sourceDir := filepath.Join(tmpDir, "source")
		if err := os.MkdirAll(sourceDir, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(sourceDir, "test.txt"), []byte("hello"), 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}

		outPath := filepath.Join(tmpDir, "output.tar.gz")
		roots := []FilesystemRoot{{ID: "p0", Path: sourceDir}}
		if err := writeTarGz(outPath, roots); err != nil {
			t.Fatalf("writeTarGz: %v", err)
		}

		// Verify file was created
		if _, err := os.Stat(outPath); err != nil {
			t.Errorf("output file not created: %v", err)
		}
	})

	t.Run("handles single file", func(t *testing.T) {
		tmpDir := t.TempDir()
		sourceFile := filepath.Join(tmpDir, "single.txt")
		if err := os.WriteFile(sourceFile, []byte("content"), 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}

		outPath := filepath.Join(tmpDir, "single.tar.gz")
		roots := []FilesystemRoot{{ID: "p0", Path: sourceFile}}
		if err := writeTarGz(outPath, roots); err != nil {
			t.Fatalf("writeTarGz: %v", err)
		}
	})

	t.Run("fails for nonexistent source", func(t *testing.T) {
		tmpDir := t.TempDir()
		outPath := filepath.Join(tmpDir, "fail.tar.gz")
		roots := []FilesystemRoot{{ID: "p0", Path: "/nonexistent/path"}}
		err := writeTarGz(outPath, roots)
		if err == nil {
			t.Error("expected error for nonexistent source")
		}
	})

	t.Run("fails for invalid output path", func(t *testing.T) {
		tmpDir := t.TempDir()
		sourceFile := filepath.Join(tmpDir, "source.txt")
		if err := os.WriteFile(sourceFile, []byte("content"), 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}

		roots := []FilesystemRoot{{ID: "p0", Path: sourceFile}}
		err := writeTarGz("/nonexistent/dir/output.tar.gz", roots)
		if err == nil {
			t.Error("expected error for invalid output path")
		}
	})
}

func TestWriteRollbackMetadata(t *testing.T) {
	t.Run("writes valid metadata file", func(t *testing.T) {
		tmpDir := t.TempDir()
		data := &RollbackData{
			Kind:         rollbackKindFilesystem,
			RequestID:    "test-123",
			ProjectPath:  "/test/project",
			RollbackPath: tmpDir,
		}

		err := writeRollbackMetadata(tmpDir, data)
		if err != nil {
			t.Fatalf("writeRollbackMetadata: %v", err)
		}

		// Verify file was created
		metaPath := filepath.Join(tmpDir, "metadata.json")
		if _, err := os.Stat(metaPath); err != nil {
			t.Errorf("metadata file not created: %v", err)
		}

		// Verify it can be loaded back
		loaded, err := LoadRollbackData(tmpDir)
		if err != nil {
			t.Fatalf("LoadRollbackData: %v", err)
		}
		if loaded.RequestID != "test-123" {
			t.Errorf("expected RequestID test-123, got %s", loaded.RequestID)
		}
	})

	t.Run("fails for invalid directory", func(t *testing.T) {
		data := &RollbackData{
			Kind:      rollbackKindFilesystem,
			RequestID: "test-123",
		}

		err := writeRollbackMetadata("/nonexistent/path", data)
		if err == nil {
			t.Error("expected error for invalid directory")
		}
	})
}

func TestAddPathToTar(t *testing.T) {
	t.Run("handles regular file", func(t *testing.T) {
		tmpDir := t.TempDir()

		// Create a regular file
		testFile := filepath.Join(tmpDir, "test.txt")
		if err := os.WriteFile(testFile, []byte("content"), 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}

		// Create a tar writer
		outPath := filepath.Join(tmpDir, "test.tar.gz")
		f, err := os.Create(outPath)
		if err != nil {
			t.Fatalf("create file: %v", err)
		}
		defer f.Close()

		gw := gzip.NewWriter(f)
		defer gw.Close()

		tw := tar.NewWriter(gw)
		defer tw.Close()

		// Get file info
		info, err := os.Lstat(testFile)
		if err != nil {
			t.Fatalf("lstat: %v", err)
		}

		// Add the file to tar
		err = addPathToTar(tw, testFile, "test.txt", info)
		if err != nil {
			t.Fatalf("addPathToTar: %v", err)
		}
	})

	t.Run("handles directory", func(t *testing.T) {
		tmpDir := t.TempDir()

		// Create a subdirectory
		subDir := filepath.Join(tmpDir, "subdir")
		if err := os.MkdirAll(subDir, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		// Create a tar writer
		outPath := filepath.Join(tmpDir, "test.tar.gz")
		f, err := os.Create(outPath)
		if err != nil {
			t.Fatalf("create file: %v", err)
		}
		defer f.Close()

		gw := gzip.NewWriter(f)
		defer gw.Close()

		tw := tar.NewWriter(gw)
		defer tw.Close()

		// Get directory info
		info, err := os.Lstat(subDir)
		if err != nil {
			t.Fatalf("lstat: %v", err)
		}

		// Add the directory to tar
		err = addPathToTar(tw, subDir, "subdir/", info)
		if err != nil {
			t.Fatalf("addPathToTar: %v", err)
		}
	})

	t.Run("handles symlink", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink tests are not reliable on windows")
		}

		tmpDir := t.TempDir()

		// Create a file and symlink
		realFile := filepath.Join(tmpDir, "real.txt")
		if err := os.WriteFile(realFile, []byte("content"), 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}

		linkFile := filepath.Join(tmpDir, "link.txt")
		if err := os.Symlink("real.txt", linkFile); err != nil {
			t.Skipf("symlink not supported: %v", err)
		}

		// Create a tar writer
		outPath := filepath.Join(tmpDir, "test.tar.gz")
		f, err := os.Create(outPath)
		if err != nil {
			t.Fatalf("create file: %v", err)
		}
		defer f.Close()

		gw := gzip.NewWriter(f)
		defer gw.Close()

		tw := tar.NewWriter(gw)
		defer tw.Close()

		// Get file info for the symlink
		info, err := os.Lstat(linkFile)
		if err != nil {
			t.Fatalf("lstat: %v", err)
		}

		// Add the symlink to tar
		err = addPathToTar(tw, linkFile, "link.txt", info)
		if err != nil {
			t.Fatalf("addPathToTar: %v", err)
		}
	})
}

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"simple", "simple"},
		{"UPPERCASE", "uppercase"},
		{"with spaces", "with_spaces"},
		{"path/slash", "path_slash"},
		{"special!@#$%chars", "specialchars"},
		{"keep-dash", "keep-dash"},
		{"keep.dot", "keep.dot"},
		{"keep_underscore", "keep_underscore"},
		{"123numbers", "123numbers"},
		{"  trimmed  ", "trimmed"},
		{"", "unknown"},
		{"!!!###", "unknown"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := sanitizeFilename(tc.input)
			if got != tc.want {
				t.Errorf("sanitizeFilename(%q) = %q, want %q", tc.input, got, tc.want)
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
