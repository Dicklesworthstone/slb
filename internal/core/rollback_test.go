package core

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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

func TestRollbackFilesystemRestoreRefusesRedirectedAncestors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture requires symlink support")
	}
	for _, kind := range []string{"file", "directory", "symlink"} {
		for _, force := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/force=%v", kind, force), func(t *testing.T) {
				project := t.TempDir()
				parent := filepath.Join(project, "parent")
				outside := filepath.Join(project, "unrelated")
				for _, dir := range []string{parent, outside} {
					if err := os.Mkdir(dir, 0700); err != nil {
						t.Fatal(err)
					}
				}
				target := filepath.Join(parent, "item")
				switch kind {
				case "directory":
					if err := os.Mkdir(target, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(target, "leaf"), []byte("captured"), 0600); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					if err := os.Symlink("saved-link-target", target); err != nil {
						t.Fatal(err)
					}
				default:
					if err := os.WriteFile(target, []byte("captured"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				data, err := CaptureRollbackState(context.Background(), &db.Request{
					ID: "redirected-ancestor", ProjectPath: project,
					Command: db.CommandSpec{Raw: "rm -rf parent/item", Cwd: project},
				}, RollbackCaptureOptions{MaxSizeBytes: 1 << 20})
				if err != nil {
					t.Fatal(err)
				}
				// Preserve the original tree while redirecting its old pathname.
				if err := os.Rename(parent, filepath.Join(project, "saved-parent")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, parent); err != nil {
					t.Fatal(err)
				}
				unrelated := filepath.Join(outside, "item")
				if force {
					if kind == "directory" {
						if err := os.Mkdir(unrelated, 0700); err != nil {
							t.Fatal(err)
						}
						unrelated = filepath.Join(unrelated, "leaf")
					}
					if err := os.WriteFile(unrelated, []byte("unrelated current work"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{Force: force}); err == nil {
					t.Error("restoration accepted an ancestor redirected to another directory")
				}
				if force {
					got, err := os.ReadFile(unrelated)
					if err != nil || string(got) != "unrelated current work" {
						t.Fatalf("restoration changed unrelated contents: %q, %v", got, err)
					}
				} else if _, err := os.Lstat(unrelated); !os.IsNotExist(err) {
					t.Fatalf("restoration created an entry in the unrelated directory: %v", err)
				}
			})
		}
	}
}

func TestRollbackFilesystemRestorePreflightsCompleteArchive(t *testing.T) {
	for _, damage := range []string{
		"unknown root", "truncated body", "unsupported type", "gzip checksum",
		"duplicate name", "path traversal", "payload exceeds record", "payload below record",
	} {
		t.Run(damage, func(t *testing.T) {
			project := t.TempDir()
			for _, name := range []string{"a", "b"} {
				if err := os.WriteFile(filepath.Join(project, name), []byte("captured"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			data, err := CaptureRollbackState(context.Background(), &db.Request{
				ID: "archive-preflight", ProjectPath: project,
				Command: db.CommandSpec{Raw: "rm a b", Cwd: project},
			}, RollbackCaptureOptions{MaxSizeBytes: 1 << 20})
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"a", "b"} {
				if err := os.WriteFile(filepath.Join(project, name), []byte("current "+name), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var encoded bytes.Buffer
			gz := gzip.NewWriter(&encoded)
			tw := tar.NewWriter(gz)
			if err := tw.WriteHeader(&tar.Header{Name: "p0", Mode: 0600, Typeflag: tar.TypeReg, Size: 8}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte("captured")); err != nil {
				t.Fatal(err)
			}
			second := &tar.Header{Name: "p1", Mode: 0600, Typeflag: tar.TypeReg, Size: 8}
			if damage == "unknown root" {
				second.Name = "not-a-captured-root"
			}
			if damage == "duplicate name" {
				second.Name = "p0"
			}
			if damage == "path traversal" {
				second.Name = "p1/../p0"
			}
			if damage == "payload exceeds record" {
				data.Filesystem.TotalBytes--
			}
			if damage == "payload below record" {
				data.Filesystem.TotalBytes++
			}
			if damage == "unsupported type" {
				second.Typeflag, second.Size, second.Linkname = tar.TypeLink, 0, "p0"
			}
			if err := tw.WriteHeader(second); err != nil {
				t.Fatal(err)
			}
			if second.Size > 0 {
				body := "captured"
				if damage == "truncated body" {
					body = "cut"
				}
				if _, err := tw.Write([]byte(body)); err != nil {
					t.Fatal(err)
				}
			}
			// Complete the gzip stream even when the enclosed tar body is short.
			if damage != "truncated body" {
				if err := tw.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			archive := encoded.Bytes()
			if damage == "gzip checksum" {
				archive[len(archive)-8] ^= 0xff
			}
			if err := os.WriteFile(filepath.Join(data.RollbackPath, data.Filesystem.TarGz), archive, 0600); err != nil {
				t.Fatal(err)
			}
			if err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{Force: true}); err == nil {
				t.Error("restoration accepted a malformed archive")
			}
			for _, name := range []string{"a", "b"} {
				got, err := os.ReadFile(filepath.Join(project, name))
				if err != nil || string(got) != "current "+name {
					t.Errorf("malformed archive changed live file %s before rejection: %q, %v", name, got, err)
				}
			}
		})
	}
}

func TestRollbackFilesystemRestoreRefusesPlannedNonDirectoryAncestors(t *testing.T) {
	for _, kind := range []byte{tar.TypeReg, tar.TypeSymlink} {
		for _, parentFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("type=%d/parent-first=%v", kind, parentFirst), func(t *testing.T) {
				project := t.TempDir()
				outside := filepath.Join(project, "unrelated")
				if err := os.Mkdir(outside, 0700); err != nil {
					t.Fatal(err)
				}
				outsideFile := filepath.Join(outside, "child")
				if err := os.WriteFile(outsideFile, []byte("unrelated current work"), 0600); err != nil {
					t.Fatal(err)
				}
				parent := filepath.Join(project, "restore-parent")
				data := &RollbackData{
					Kind: rollbackKindFilesystem, RollbackPath: t.TempDir(),
					Filesystem: &FilesystemRollbackData{
						TarGz: rollbackFilesystemTarGz, TotalBytes: 8,
						Roots: []FilesystemRoot{{ID: "p0", Path: parent}, {ID: "p1", Path: filepath.Join(parent, "child")}},
					},
				}
				parentHeader := tar.Header{Name: "p0", Typeflag: kind, Mode: 0700, Linkname: outside}
				if kind == tar.TypeReg {
					parentHeader.Size, parentHeader.Linkname = 8, ""
					data.Filesystem.TotalBytes += 8
				}
				headers := []tar.Header{parentHeader, {Name: "p1", Typeflag: tar.TypeReg, Mode: 0600, Size: 8}}
				if !parentFirst {
					headers[0], headers[1] = headers[1], headers[0]
				}
				var encoded bytes.Buffer
				gz := gzip.NewWriter(&encoded)
				tw := tar.NewWriter(gz)
				for _, hdr := range headers {
					if err := tw.WriteHeader(&hdr); err != nil {
						t.Fatal(err)
					}
					if hdr.Size > 0 {
						if _, err := tw.Write([]byte("captured")); err != nil {
							t.Fatal(err)
						}
					}
				}
				if err := tw.Close(); err != nil {
					t.Fatal(err)
				}
				if err := gz.Close(); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(data.RollbackPath, data.Filesystem.TarGz), encoded.Bytes(), 0600); err != nil {
					t.Fatal(err)
				}
				err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{Force: true})
				if err == nil || !strings.Contains(err.Error(), "file or symlink parent") {
					t.Fatalf("planned non-directory ancestor was not rejected: %v", err)
				}
				if _, err := os.Lstat(parent); !os.IsNotExist(err) {
					t.Fatalf("invalid restore plan changed a destination: %v", err)
				}
				got, err := os.ReadFile(outsideFile)
				if err != nil || string(got) != "unrelated current work" {
					t.Fatalf("invalid restore plan changed unrelated contents: %q, %v", got, err)
				}
			})
		}
	}
}

func TestRollbackFilesystemRestorePreservesHistoricalCaptures(t *testing.T) {
	for _, variant := range []string{"empty", "overlapping operands", "leaf symlink"} {
		t.Run(variant, func(t *testing.T) {
			if variant == "leaf symlink" && runtime.GOOS == "windows" {
				t.Skip("fixture requires symlink support")
			}
			project := t.TempDir()
			tree := filepath.Join(project, "tree")
			if err := os.MkdirAll(filepath.Join(tree, "empty-directory"), 0700); err != nil {
				t.Fatal(err)
			}
			body := ""
			if variant != "empty" {
				body = "captured contents"
			}
			if err := os.WriteFile(filepath.Join(tree, "file"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if variant == "leaf symlink" {
				if err := os.WriteFile(filepath.Join(project, "unrelated"), []byte("keep unchanged"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../unrelated", filepath.Join(tree, "link")); err != nil {
					t.Fatal(err)
				}
			}
			argv := []string{"rm", "-rf", "tree"}
			if variant == "overlapping operands" {
				argv = append(argv, "tree/file")
			}
			data, err := CaptureRollbackState(context.Background(), &db.Request{
				ID: "historical-filesystem", ProjectPath: project,
				Command: db.CommandSpec{Raw: strings.Join(argv, " "), Argv: argv, Cwd: project},
			}, RollbackCaptureOptions{MaxSizeBytes: 1 << 20})
			if err != nil {
				t.Fatal(err)
			}
			if variant == "empty" && data.Filesystem.TotalBytes != 0 {
				t.Fatalf("expected a zero-payload capture: %+v", data.Filesystem)
			}
			data.Version, data.CommandHash = 1, ""
			if err := writeRollbackMetadata(data.RollbackPath, data); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(tree, filepath.Join(project, "retained-tree")); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadRollbackData(data.RollbackPath)
			if err != nil || loaded.Version != 1 {
				t.Fatalf("reading existing snapshot format: %+v, %v", loaded, err)
			}
			if err := RestoreRollbackState(context.Background(), loaded, RollbackRestoreOptions{}); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(tree, "file"))
			if err != nil || string(got) != body {
				t.Fatalf("historical file was not restored: %q, %v", got, err)
			}
			if info, err := os.Stat(filepath.Join(tree, "empty-directory")); err != nil || !info.IsDir() {
				t.Fatalf("empty directory was not restored: %v", err)
			}
			if variant == "leaf symlink" {
				if target, err := os.Readlink(filepath.Join(tree, "link")); err != nil || target != "../unrelated" {
					t.Fatalf("leaf symlink was not preserved: %q, %v", target, err)
				}
				got, err := os.ReadFile(filepath.Join(project, "unrelated"))
				if err != nil || string(got) != "keep unchanged" {
					t.Fatalf("restoration followed a leaf symlink: %q, %v", got, err)
				}
			}
		})
	}
}

func TestRollbackFilesystemRestorePreflightsConflictsAndCancellation(t *testing.T) {
	project := t.TempDir()
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(project, name), []byte("captured "+name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	data, err := CaptureRollbackState(context.Background(), &db.Request{
		ID: "destination-preflight", ProjectPath: project,
		Command: db.CommandSpec{Raw: "rm a b", Cwd: project},
	}, RollbackCaptureOptions{MaxSizeBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(project, "a"), filepath.Join(project, "retained-a")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "b"), []byte("current b"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RestoreRollbackState(ctx, data, RollbackRestoreOptions{Force: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("restore ignored cancellation: %v", err)
	}
	if err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{}); err == nil || !strings.Contains(err.Error(), "use --force") {
		t.Fatalf("existing destination was not rejected: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(project, "a")); !os.IsNotExist(err) {
		t.Fatalf("restore changed the first destination before refusing the later conflict: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(project, "b")); err != nil || string(got) != "current b" {
		t.Fatalf("restore changed an existing destination: %q, %v", got, err)
	}
	if err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if got, err := os.ReadFile(filepath.Join(project, name)); err != nil || string(got) != "captured "+name {
			t.Fatalf("explicit force restore failed for %s: %q, %v", name, got, err)
		}
	}
}

func TestRollbackFilesystemRestoreSeparatesMetadataAndPayloadBudgets(t *testing.T) {
	for _, scenario := range []string{"payload fits", "PAX header exceeds budget", "trailing padding exceeds budget"} {
		t.Run(scenario, func(t *testing.T) {
			body := strings.Repeat("x", 16<<10)
			hdr := &tar.Header{Name: "p0", Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(body)), Format: tar.FormatUSTAR}
			payloadBudget, metadataBudget := int64(len(body)), int64(3*512)
			if scenario == "PAX header exceeds budget" {
				hdr.Name, hdr.Format = "p0/"+strings.Repeat("long-name", 40), tar.FormatPAX
				// A large declared payload cannot pay for metadata consumed by
				// tar.Next before it even exposes an entry to the validator.
				payloadBudget, metadataBudget = 1<<30, 512
			}
			var encoded bytes.Buffer
			gz := gzip.NewWriter(&encoded)
			tw := tar.NewWriter(gz)
			if err := tw.WriteHeader(hdr); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if scenario == "trailing padding exceeds budget" {
				if _, err := gz.Write(make([]byte, 512)); err != nil {
					t.Fatal(err)
				}
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			visited := 0
			err := walkFilesystemRollbackArchive(context.Background(), bytes.NewReader(encoded.Bytes()),
				map[string]string{"p0": filepath.Join(t.TempDir(), "restored")}, payloadBudget, metadataBudget,
				func(_ *tar.Header, _ string, body io.Reader) error {
					visited++
					_, err := io.Copy(io.Discard, body)
					return err
				})
			if scenario == "payload fits" {
				if err != nil || visited != 1 {
					t.Fatalf("valid payload was charged to the metadata budget: visited=%d, %v", visited, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "metadata") || !strings.Contains(err.Error(), "size limit") {
				t.Fatalf("metadata budget was not enforced independently: %v", err)
			}
			if scenario == "PAX header exceeds budget" && visited != 0 {
				t.Fatal("oversized metadata was decoded before its budget was enforced")
			}
		})
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
	req, _, logPath := kubernetesRollbackFixture(t)
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
	if !strings.Contains(string(b), "create") || strings.Contains(string(b), "apply") {
		t.Fatalf("expected create-only restoration, got: %q", string(b))
	}
}

// This helper acts as kubectl in a disposable project. Its two contexts expose
// different resource contents, so losing --context is an observable data-loss
// bug rather than just an argument-format assertion.
func TestKubernetesRollbackKubectlHelper(t *testing.T) {
	if os.Getenv("SLB_KUBECTL_HELPER") != "1" {
		return
	}
	var args []string
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	if len(args) == 0 {
		os.Exit(2)
	}
	logFile, err := os.OpenFile(os.Getenv("SLB_KUBECTL_CALLS"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(3)
	}
	err = json.NewEncoder(logFile).Encode(map[string]any{"args": args, "kubeconfig": os.Getenv("KUBECONFIG")})
	if closeErr := logFile.Close(); err != nil || closeErr != nil {
		os.Exit(4)
	}
	var positional []string
	flags := map[string]string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			key, value, hasValue := strings.Cut(arg, "=")
			if !hasValue && key != "--minify" && i+1 < len(args) {
				i++
				value = args[i]
			}
			flags[key] = value
			continue
		}
		positional = append(positional, arg)
	}
	if len(positional) == 0 {
		os.Exit(5)
	}
	configPaths := filepath.SplitList(os.Getenv("KUBECONFIG"))
	if flags["--kubeconfig"] != "" {
		configPaths = []string{flags["--kubeconfig"]}
	}
	if len(configPaths) == 0 {
		os.Exit(6)
	}
	configBytes, err := os.ReadFile(configPaths[0])
	if err != nil {
		os.Exit(7)
	}
	var cfg map[string]any
	if json.Unmarshal(configBytes, &cfg) != nil {
		os.Exit(8)
	}
	selected, _ := cfg["current-context"].(string)
	if flags["--context"] != "" {
		selected = flags["--context"]
	}
	var selectedContext, selectedCluster map[string]any
	for _, raw := range cfg["contexts"].([]any) {
		item := raw.(map[string]any)
		if item["name"] == selected {
			selectedContext = item
		}
	}
	if selectedContext == nil {
		os.Exit(9)
	}
	contextConfig := selectedContext["context"].(map[string]any)
	for _, raw := range cfg["clusters"].([]any) {
		item := raw.(map[string]any)
		if item["name"] == contextConfig["cluster"] {
			selectedCluster = item
		}
	}
	switch positional[0] {
	case "config":
		cfg["current-context"] = selected
		cfg["contexts"] = []any{selectedContext}
		cfg["clusters"] = []any{selectedCluster}
		if json.NewEncoder(os.Stdout).Encode(cfg) != nil {
			os.Exit(10)
		}
	case "get":
		if os.Getenv("SLB_KUBECTL_HANG") == "1" {
			time.Sleep(time.Minute)
		}
		if size, _ := strconv.Atoi(os.Getenv("SLB_KUBECTL_OUTPUT_BYTES")); size > 0 {
			if _, err := fmt.Fprint(os.Stdout, strings.Repeat("x", size)); err != nil {
				os.Exit(11)
			}
			os.Exit(0)
		}
		if len(positional) < 3 {
			os.Exit(12)
		}
		namespace, _ := contextConfig["namespace"].(string)
		if flags["--namespace"] != "" {
			namespace = flags["--namespace"]
		} else if flags["-n"] != "" {
			namespace = flags["-n"]
		}
		kind, apiVersion := "Deployment", "apps/v1"
		if positional[1] == "secret" {
			kind, apiVersion = "Secret", "v1"
		}
		resource := map[string]any{
			"apiVersion": apiVersion, "kind": kind,
			"metadata": map[string]any{"name": positional[2], "namespace": namespace, "uid": "server-uid", "resourceVersion": "42", "managedFields": []any{}},
			"spec":     map[string]any{"captured_context": selected}, "status": map[string]any{"observedGeneration": 7},
		}
		if kind == "Secret" {
			resource["data"] = map[string]string{"token": "ZmFrZS1hcHBsaWNhdGlvbi1zZWNyZXQ="}
			delete(resource, "spec")
		}
		if json.NewEncoder(os.Stdout).Encode(resource) != nil {
			os.Exit(13)
		}
	case "create", "apply":
		if positional[0] == "create" && os.Getenv("SLB_KUBECTL_EXISTS") == "1" {
			os.Exit(14)
		}
		if flags["-f"] == "-" {
			var resource map[string]any
			if json.NewDecoder(os.Stdin).Decode(&resource) != nil {
				os.Exit(15)
			}
		}
	default:
		os.Exit(16)
	}
	os.Exit(0)
}

func kubernetesRollbackFixture(t *testing.T) (*db.Request, string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake kubectl launcher requires a POSIX shell")
	}
	project := t.TempDir()
	bin := filepath.Join(project, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(executable, "'", "'\"'\"'") + "' -test.run='^TestKubernetesRollbackKubectlHelper$' -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(project, "kubeconfig.json")
	configJSON := `{"current-context":"dev","contexts":[{"name":"dev","context":{"cluster":"dev-cluster","namespace":"devspace"}},{"name":"prod","context":{"cluster":"prod-cluster","namespace":"prodspace"}}],"clusters":[{"name":"dev-cluster","cluster":{"server":"https://dev.invalid"}},{"name":"prod-cluster","cluster":{"server":"https://prod.invalid"}}],"users":[{"name":"fixture","user":{"token":"fixture-credential-do-not-copy"}}]}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0600); err != nil {
		t.Fatal(err)
	}
	callsPath := filepath.Join(project, "calls.jsonl")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KUBECONFIG", configPath)
	t.Setenv("KUBECTL_KUBERC", "false")
	t.Setenv("SLB_KUBECTL_HELPER", "1")
	t.Setenv("SLB_KUBECTL_CALLS", callsPath)
	req := &db.Request{ID: "kubernetes-context", ProjectPath: project, Command: db.CommandSpec{
		Raw: "kubectl delete deployment api --context prod -n workloads", Cwd: project,
	}}
	return req, configPath, callsPath
}

func TestKubernetesRollbackCapturesRequestedContext(t *testing.T) {
	req, _, _ := kubernetesRollbackFixture(t)
	data, err := CaptureRollbackState(context.Background(), req, RollbackCaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if data == nil || data.Kubernetes == nil || len(data.Kubernetes.Manifests) != 1 {
		t.Fatalf("missing captured manifest: %+v", data)
	}
	contents, err := os.ReadFile(filepath.Join(data.RollbackPath, data.Kubernetes.Manifests[0]))
	if err != nil {
		t.Fatal(err)
	}
	var resource struct {
		Spec struct {
			CapturedContext string `json:"captured_context"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(contents, &resource); err != nil {
		t.Fatal(err)
	}
	if resource.Spec.CapturedContext != "prod" {
		t.Fatalf("backup came from %q; destructive request targets prod", resource.Spec.CapturedContext)
	}
	if data.Kubernetes.Context != "prod" || data.Kubernetes.Namespace != "workloads" || data.Kubernetes.Server != "https://prod.invalid" {
		t.Fatalf("incorrect captured target: %+v", data.Kubernetes)
	}
	if strings.Contains(string(contents), "resourceVersion") || strings.Contains(string(contents), "server-uid") || strings.Contains(string(contents), `"status"`) {
		t.Fatalf("server-owned fields survived normalization: %s", contents)
	}
	metadata, err := os.ReadFile(filepath.Join(data.RollbackPath, rollbackMetadataFilename))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(metadata), "fixture-credential-do-not-copy") || strings.Contains(string(metadata), `"users"`) {
		t.Fatal("kubeconfig credentials were copied into recovery metadata")
	}
	for _, item := range []struct {
		path string
		mode os.FileMode
	}{
		{data.RollbackPath, 0700},
		{filepath.Join(data.RollbackPath, rollbackKubernetesDirName), 0700},
		{filepath.Join(data.RollbackPath, data.Kubernetes.Manifests[0]), 0600},
	} {
		info, err := os.Stat(item.path)
		if err != nil || info.Mode().Perm() != item.mode {
			t.Fatalf("private recovery mode %s: info=%v err=%v", item.path, info, err)
		}
	}
}

type kubernetesCall struct {
	Args       []string `json:"args"`
	Kubeconfig string   `json:"kubeconfig"`
}

func readKubernetesCalls(t *testing.T, path string) []kubernetesCall {
	t.Helper()
	contents, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []kubernetesCall
	decoder := json.NewDecoder(strings.NewReader(string(contents)))
	for {
		var call kubernetesCall
		if err := decoder.Decode(&call); err == io.EOF {
			return calls
		} else if err != nil {
			t.Fatal(err)
		}
		calls = append(calls, call)
	}
}

func assertNoKubernetesWrites(t *testing.T, callsPath string) {
	t.Helper()
	for _, call := range readKubernetesCalls(t, callsPath) {
		for _, arg := range call.Args {
			if arg == "create" || arg == "apply" {
				t.Fatalf("invalid snapshot reached a mutating kubectl call: %v", call.Args)
			}
		}
	}
}

func TestKubernetesRollbackConfigurationSelection(t *testing.T) {
	t.Run("explicit file before verb overrides environment and preserves colon filename", func(t *testing.T) {
		req, configPath, _ := kubernetesRollbackFixture(t)
		contents, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		explicit := filepath.Join(req.ProjectPath, "config:production")
		if err := os.WriteFile(explicit, contents, 0600); err != nil {
			t.Fatal(err)
		}
		req.Command.Argv = []string{"kubectl", "--kubeconfig", "config:production", "--context=prod", "-nworkloads", "delete", "deployment/api"}
		data, err := CaptureRollbackState(context.Background(), req, RollbackCaptureOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(data.Kubernetes.ConfigFiles) != 1 || data.Kubernetes.ConfigFiles[0].Path != explicit || data.Kubernetes.Context != "prod" {
			t.Fatalf("explicit configuration was not preserved: %+v", data.Kubernetes)
		}
		if err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("environment merge list is fixed and empty entries ignored", func(t *testing.T) {
		req, configPath, callsPath := kubernetesRollbackFixture(t)
		other := filepath.Join(req.ProjectPath, "second-config")
		if err := os.WriteFile(other, []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("KUBECONFIG", string(os.PathListSeparator)+configPath+string(os.PathListSeparator)+other+string(os.PathListSeparator))
		data, err := CaptureRollbackState(context.Background(), req, RollbackCaptureOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(data.Kubernetes.ConfigFiles) != 2 || data.Kubernetes.ConfigFiles[1].Path != other {
			t.Fatalf("merge list lost: %+v", data.Kubernetes.ConfigFiles)
		}
		t.Setenv("KUBECONFIG", filepath.Join(req.ProjectPath, "wrong-config"))
		if err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{}); err != nil {
			t.Fatal(err)
		}
		want := configPath + string(os.PathListSeparator) + other
		for _, call := range readKubernetesCalls(t, callsPath) {
			if call.Kubeconfig != want {
				t.Fatalf("mutable environment retargeted recovery: %+v", call)
			}
		}
	})
	t.Run("default home config and context namespace", func(t *testing.T) {
		req, configPath, _ := kubernetesRollbackFixture(t)
		contents, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		homeDir := filepath.Join(req.ProjectPath, "home")
		if err := os.MkdirAll(filepath.Join(homeDir, ".kube"), 0700); err != nil {
			t.Fatal(err)
		}
		defaultPath := filepath.Join(homeDir, ".kube", "config")
		if err := os.WriteFile(defaultPath, contents, 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", homeDir)
		t.Setenv("KUBECONFIG", "")
		req.Command.Raw = "kubectl delete deployment api"
		data, err := CaptureRollbackState(context.Background(), req, RollbackCaptureOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if data.Kubernetes.Context != "dev" || data.Kubernetes.Namespace != "devspace" || data.Kubernetes.ConfigFiles[0].Path != defaultPath {
			t.Fatalf("default target was not resolved: %+v", data.Kubernetes)
		}
	})
}

func TestKubernetesRollbackRefusesChangedConfiguration(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%t", force), func(t *testing.T) {
			req, configPath, callsPath := kubernetesRollbackFixture(t)
			data, err := CaptureRollbackState(context.Background(), req, RollbackCaptureOptions{})
			if err != nil {
				t.Fatal(err)
			}
			contents, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			contents = []byte(strings.ReplaceAll(string(contents), `"current-context":"dev"`, `"current-context":"prod"`))
			if err := os.WriteFile(configPath, contents, 0600); err != nil {
				t.Fatal(err)
			}
			if err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{Force: force}); err == nil || !strings.Contains(err.Error(), "kubeconfig changed") {
				t.Fatalf("changed configuration accepted: %v", err)
			}
			assertNoKubernetesWrites(t, callsPath)
		})
	}
}

func TestKubernetesRollbackDoesNotOverwriteWithoutForce(t *testing.T) {
	req, _, callsPath := kubernetesRollbackFixture(t)
	data, err := CaptureRollbackState(context.Background(), req, RollbackCaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SLB_KUBECTL_EXISTS", "1")
	if err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{}); err == nil {
		t.Fatal("existing remote resource was silently overwritten")
	}
	for _, call := range readKubernetesCalls(t, callsPath) {
		for _, arg := range call.Args {
			if arg == "apply" {
				t.Fatal("default restore used apply")
			}
		}
	}
	if err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	var created, applied bool
	for _, call := range readKubernetesCalls(t, callsPath) {
		for _, arg := range call.Args {
			created = created || arg == "create"
			applied = applied || arg == "apply"
		}
	}
	if !created || !applied {
		t.Fatalf("restore operations did not honor force: create=%t apply=%t", created, applied)
	}
}

func TestKubernetesRollbackPreflightsAllManifests(t *testing.T) {
	for _, scenario := range []string{"corrupt second", "traversal", "symlink", "wrong namespace", "legacy target", "missing digest"} {
		t.Run(scenario, func(t *testing.T) {
			req, _, callsPath := kubernetesRollbackFixture(t)
			req.Command.Raw = "kubectl delete deployment api worker --context prod -n workloads"
			data, err := CaptureRollbackState(context.Background(), req, RollbackCaptureOptions{})
			if err != nil {
				t.Fatal(err)
			}
			rel := data.Kubernetes.Manifests[1]
			switch scenario {
			case "corrupt second":
				if err := os.WriteFile(filepath.Join(data.RollbackPath, rel), []byte(`{broken`), 0600); err != nil {
					t.Fatal(err)
				}
			case "traversal":
				data.Kubernetes.Manifests[1] = "../outside.json"
			case "symlink":
				link := filepath.Join(data.RollbackPath, "redirect.json")
				if err := os.Symlink(filepath.Join(data.RollbackPath, rel), link); err != nil {
					t.Fatal(err)
				}
				data.Kubernetes.Manifests[1] = "redirect.json"
				data.Kubernetes.ManifestSHA256["redirect.json"] = data.Kubernetes.ManifestSHA256[rel]
			case "wrong namespace":
				body := []byte(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"worker","namespace":"other"}}`)
				if err := os.WriteFile(filepath.Join(data.RollbackPath, rel), body, 0600); err != nil {
					t.Fatal(err)
				}
				data.Kubernetes.ManifestSHA256[rel] = fmt.Sprintf("%x", sha256.Sum256(body))
			case "legacy target":
				data.Kubernetes.Context = ""
			case "missing digest":
				delete(data.Kubernetes.ManifestSHA256, rel)
			}
			if err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{Force: true}); err == nil {
				t.Fatal("invalid snapshot was accepted")
			}
			assertNoKubernetesWrites(t, callsPath)
		})
	}
}

func TestKubernetesRollbackRefusesIncompleteSelections(t *testing.T) {
	for _, raw := range []string{
		"kubectl delete deployment api --context prod --selector app=x",
		"kubectl --server https://other.invalid delete deployment api",
		"kubectl delete deployment api --all",
		"kubectl delete deployment api --all-namespaces",
		"kubectl delete -f manifest.json",
		"kubectl delete deployment api --namespace",
		"kubectl delete pod/api service",
		"kubectl delete deployments,services api",
		"kubectl delete pod api --token fixture-secret-never-echo",
	} {
		t.Run(raw, func(t *testing.T) {
			req, _, callsPath := kubernetesRollbackFixture(t)
			req.Command.Raw = raw
			if data, err := CaptureRollbackState(context.Background(), req, RollbackCaptureOptions{}); err == nil || data != nil {
				t.Fatalf("partial selection captured: data=%+v error=%v", data, err)
			} else if strings.Contains(err.Error(), "fixture-secret-never-echo") {
				t.Fatal("credential argument leaked into error")
			}
			if len(readKubernetesCalls(t, callsPath)) != 0 {
				t.Fatal("unsupported selection reached kubectl")
			}
		})
	}
	selection, err := parseKubectlDelete([]string{"kubectl", "--context=prod", "delete", "deployment/api", "--force", "deployment/api", "--grace-period=0", "--cascade=foreground", "--timeout=10s"})
	if err != nil || selection == nil || len(selection.Resources) != 1 {
		t.Fatalf("complete named selection with controls rejected: %+v %v", selection, err)
	}
}

func TestKubernetesRollbackCaptureBoundsAndCancellation(t *testing.T) {
	t.Run("maximum size", func(t *testing.T) {
		req, _, _ := kubernetesRollbackFixture(t)
		t.Setenv("SLB_KUBECTL_OUTPUT_BYTES", "8192")
		if data, err := CaptureRollbackState(context.Background(), req, RollbackCaptureOptions{MaxSizeBytes: 1024}); err == nil || data != nil || !strings.Contains(err.Error(), "max size") {
			t.Fatalf("oversized output was accepted: %+v %v", data, err)
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		req, _, _ := kubernetesRollbackFixture(t)
		t.Setenv("SLB_KUBECTL_HANG", "1")
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		started := time.Now()
		if data, err := CaptureRollbackState(ctx, req, RollbackCaptureOptions{}); data != nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("cancellation did not stop capture: %+v %v", data, err)
		}
		if time.Since(started) > 3*time.Second {
			t.Fatal("capture failed to reap its cancelled subprocess promptly")
		}
	})
}

func TestKubernetesManifestPreservesLargeIntegers(t *testing.T) {
	body := []byte(`{"apiVersion":"example.io/v1","kind":"Counter","metadata":{"name":"counter","namespace":"workloads"},"spec":{"positive":9007199254740993,"negative":-9007199254740993,"nested":[9223372036854775807]}}`)
	manifest, err := normalizeKubernetesManifest(body, "workloads", "counter")
	if err != nil {
		t.Fatal(err)
	}
	for _, number := range []string{"9007199254740993", "-9007199254740993", "9223372036854775807"} {
		if !strings.Contains(string(manifest), number) {
			t.Fatalf("custom-resource integer %s lost precision: %s", number, manifest)
		}
	}
	if _, err := normalizeKubernetesManifest(append(body, []byte(` {}`)...), "workloads", "counter"); err == nil {
		t.Fatal("multiple JSON values were accepted as one resource")
	}
}

func TestKubernetesRollbackPreservesRequestedSecret(t *testing.T) {
	req, _, _ := kubernetesRollbackFixture(t)
	req.Command.Raw = "kubectl delete secret application-token --context prod -n workloads"
	data, err := CaptureRollbackState(context.Background(), req, RollbackCaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(data.RollbackPath, data.Kubernetes.Manifests[0]))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), "ZmFrZS1hcHBsaWNhdGlvbi1zZWNyZXQ=") {
		t.Fatal("requested application Secret data was discarded")
	}
	metadata, err := os.ReadFile(filepath.Join(data.RollbackPath, rollbackMetadataFilename))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(metadata), "fixture-credential-do-not-copy") || strings.Contains(string(metadata), "ZmFrZS1hcHBsaWNhdGlvbi1zZWNyZXQ=") {
		t.Fatal("resource or kubeconfig credentials leaked into metadata")
	}
	if err := RestoreRollbackState(context.Background(), data, RollbackRestoreOptions{}); err != nil {
		t.Fatal(err)
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
		metadata := `{"version":1,"kind":"filesystem","request_id":"test-123"}`
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
		selected, err := filepath.EvalSymlinks(tmpDir)
		if err != nil {
			t.Fatal(err)
		}
		if data.RollbackPath != selected {
			t.Errorf("expected selected RollbackPath %q, got %q", selected, data.RollbackPath)
		}
	})
}

func TestLoadRollbackDataUsesSelectedDirectory(t *testing.T) {
	project := t.TempDir()
	target := filepath.Join(project, "item")
	if err := os.WriteFile(target, []byte("snapshot-one"), 0600); err != nil {
		t.Fatal(err)
	}
	request := &db.Request{ID: "selected-directory", ProjectPath: project,
		Command: db.CommandSpec{Raw: "rm item", Cwd: project, Shell: true}}
	first, err := CaptureRollbackState(context.Background(), request, RollbackCaptureOptions{MaxSizeBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	selected := first.RollbackPath
	if err := os.WriteFile(target, []byte("snapshot-two"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := CaptureRollbackState(context.Background(), request, RollbackCaptureOptions{MaxSizeBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	// A copied metadata file must not redirect the selected capture to a
	// different archive, even when both captures have the same request ID.
	first.RollbackPath = second.RollbackPath
	if err := writeRollbackMetadata(selected, first); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("current work"), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadRollbackData(selected)
	if err != nil {
		t.Fatal(err)
	}
	if err := RestoreRollbackState(context.Background(), loaded, RollbackRestoreOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "snapshot-one" {
		t.Fatalf("metadata redirected restoration away from the selected archive: %q %v", got, err)
	}
}

func TestLoadRollbackDataRejectsInvalidMetadataSource(t *testing.T) {
	for _, scenario := range []string{"missing version", "future version", "directory", "symlink", "oversized"} {
		t.Run(scenario, func(t *testing.T) {
			base := t.TempDir()
			path := filepath.Join(base, rollbackMetadataFilename)
			switch scenario {
			case "missing version", "future version":
				version := 0
				if scenario == "future version" {
					version = 999
				}
				if err := writeRollbackMetadata(base, &RollbackData{Version: version, RequestID: "unsupported", Kind: rollbackKindFilesystem}); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				other := t.TempDir()
				if err := writeRollbackMetadata(other, &RollbackData{Version: 1, RequestID: "other", Kind: rollbackKindFilesystem}); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(other, rollbackMetadataFilename), path); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			case "oversized":
				file, err := os.Create(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Truncate(maxRollbackMetadataBytes + 1); err != nil {
					_ = file.Close()
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if data, err := LoadRollbackData(base); err == nil || data != nil {
				t.Fatalf("invalid metadata source was accepted: %+v %v", data, err)
			}
		})
	}
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
			selection, err := parseKubectlDelete(append([]string{"kubectl", "delete"}, tc.args...))
			if tc.wantResCount == 0 {
				if err == nil {
					t.Fatal("incomplete resource selection was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			ns, resources := selection.Namespace, selection.Resources
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
		// A fixed path such as /nonexistent/path can exist on a test host.
		_, err := estimateFileBytes([]string{filepath.Join(t.TempDir(), "missing")}, 0)
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
		roots := []FilesystemRoot{{ID: "p0", Path: filepath.Join(tmpDir, "missing")}}
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
	t.Run("oversized metadata preserves existing destination", func(t *testing.T) {
		base := t.TempDir()
		path := filepath.Join(base, rollbackMetadataFilename)
		original := []byte("existing recovery metadata")
		if err := os.WriteFile(path, original, 0600); err != nil {
			t.Fatal(err)
		}
		// Empty targets can exceed the metadata budget while consuming no
		// file-payload budget. A capture must never report such data usable.
		roots := make([]FilesystemRoot, maxRollbackMetadataBytes/512+1)
		for i := range roots {
			roots[i] = FilesystemRoot{ID: strconv.Itoa(i), Path: "/" + strings.Repeat("x", 512) + "/" + strconv.Itoa(i)}
		}
		data := &RollbackData{Version: rollbackDataVersion, CommandHash: strings.Repeat("a", 64),
			Kind: rollbackKindFilesystem, RequestID: "many-empty-targets", RollbackPath: base,
			Filesystem: &FilesystemRollbackData{TarGz: rollbackFilesystemTarGz, Roots: roots}}
		if err := writeRollbackMetadata(base, data); err == nil || !strings.Contains(err.Error(), "metadata exceeds") {
			t.Fatalf("oversized metadata was not refused: %v", err)
		}
		if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, original) {
			t.Fatalf("refused metadata write changed the existing destination: %q %v", got, err)
		}
	})

	t.Run("writes valid metadata file", func(t *testing.T) {
		tmpDir := t.TempDir()
		data := &RollbackData{
			Version:      rollbackDataVersion,
			CommandHash:  strings.Repeat("a", 64),
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
		// Use a file as the parent so the path is invalid on every host.
		parent := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(parent, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
		data := &RollbackData{
			Kind:      rollbackKindFilesystem,
			RequestID: "test-123",
		}

		err := writeRollbackMetadata(filepath.Join(parent, "rollback"), data)
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
