package core

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func snapshotRequest(project string, targets ...string) *db.Request {
	quoted := []string{"rm", "-rf", "--"}
	for _, target := range targets {
		quoted = append(quoted, "'"+strings.ReplaceAll(target, "'", `'\''`)+"'")
	}
	return &db.Request{
		ID: "snapshot-request", ProjectPath: project,
		Command: db.CommandSpec{
			Raw: strings.Join(quoted, " "), Cwd: project,
			Argv: append([]string{"rm", "-rf", "--"}, targets...),
		},
	}
}

func readSnapshotArchive(t *testing.T, data *RollbackData) map[string]string {
	t.Helper()
	file, err := os.Open(filepath.Join(data.RollbackPath, data.Filesystem.TarGz))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	archive := tar.NewReader(reader)
	contents := make(map[string]string)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		contents[header.Name] = string(body)
	}
	// Consume the gzip trailer too; a readable tar prefix is not proof that
	// its enclosing compressed stream was successfully finalized.
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatal(err)
	}
	return contents
}

func TestRollbackSnapshotUsesLiteralArgvAndRestores(t *testing.T) {
	project := t.TempDir()
	targets := []string{"two words", "[draft]", " notes ", "--"}
	if runtime.GOOS == "windows" {
		// Windows normalizes trailing spaces in file names.
		targets = []string{"two words", "[draft]", "--"}
	}
	for _, target := range append(append([]string(nil), targets...), "d", "notes", "display-only-target") {
		if err := os.WriteFile(filepath.Join(project, target), []byte("original "+target), 0600); err != nil {
			t.Fatal(err)
		}
	}
	request := snapshotRequest(project, targets...)
	request.Command.Raw = "rm -rf display-only-target"
	originalArgv := append([]string(nil), request.Command.Argv...)
	data, err := CaptureRollbackState(context.Background(), request, RollbackCaptureOptions{})
	if err != nil || data == nil || data.Filesystem == nil {
		t.Fatalf("capture failed: data=%v err=%v", data, err)
	}
	if !reflect.DeepEqual(request.Command.Argv, originalArgv) {
		t.Fatal("capture mutated the approved execution argv")
	}
	if len(data.Filesystem.Roots) != len(targets) {
		t.Fatalf("captured different targets: %+v", data.Filesystem.Roots)
	}
	archive := readSnapshotArchive(t, data)
	for i, target := range targets {
		root := data.Filesystem.Roots[i]
		if filepath.Base(root.Path) != target || archive[root.ID] != "original "+target {
			t.Fatalf("wrong target captured: root=%+v contents=%q", root, archive[root.ID])
		}
		// Retain the live fixture under a different name rather than delete it.
		if err := os.Rename(root.Path, filepath.Join(project, fmt.Sprintf("retained-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := LoadRollbackData(data.RollbackPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := RestoreRollbackState(context.Background(), loaded, RollbackRestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		got, err := os.ReadFile(filepath.Join(project, target))
		if err != nil || string(got) != "original "+target {
			t.Fatalf("incomplete restore for %q: %q %v", target, got, err)
		}
	}
	info, err := os.Stat(filepath.Join(data.RollbackPath, data.Filesystem.TarGz))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("archive exposes captured data: mode=%v", info.Mode())
	}
}

func TestRollbackShellSnapshotPreservesQuoting(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "two words"), []byte("recoverable"), 0600); err != nil {
		t.Fatal(err)
	}
	request := snapshotRequest(project, "irrelevant-argv")
	request.Command.Raw = `rm -rf 'two words'`
	request.Command.Shell = true
	data, err := CaptureRollbackState(context.Background(), request, RollbackCaptureOptions{})
	if err != nil || data == nil || data.Filesystem == nil {
		t.Fatalf("quoted shell target lost: %v %v", data, err)
	}
	if contents := readSnapshotArchive(t, data); contents["p0"] != "recoverable" {
		t.Fatalf("wrong quoted-path contents: %q", contents)
	}
}

func TestRollbackRejectsPartialShellSnapshots(t *testing.T) {
	project := t.TempDir()
	for _, name := range []string{"first", "second"} {
		if err := os.WriteFile(filepath.Join(project, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{
		`rm first; rm second`, `rm first && rm second`, `rm first | cat`,
		`rm "$(printf first)"`, `rm $TARGET`, `rm *`, `rm first > output`,
	} {
		request := snapshotRequest(project, "first")
		request.Command.Raw, request.Command.Shell = raw, true
		if data, err := CaptureRollbackState(context.Background(), request, RollbackCaptureOptions{}); err == nil || data != nil {
			t.Errorf("partial/ambiguous snapshot advertised for %q: %v %v", raw, data, err)
		}
	}
}

func TestRollbackCaptureAttemptsDoNotClobberPreviousSnapshot(t *testing.T) {
	project := t.TempDir()
	target := filepath.Join(project, "target")
	if err := os.WriteFile(target, []byte("first snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	request := snapshotRequest(project, "target")
	first, err := CaptureRollbackState(context.Background(), request, RollbackCaptureOptions{})
	if err != nil || first == nil {
		t.Fatalf("first capture: %v %v", first, err)
	}
	if err := os.WriteFile(target, []byte("later state"), 0600); err != nil {
		t.Fatal(err)
	}
	const attempts = 8
	results := make(chan *RollbackData, attempts)
	errorsCh := make(chan error, attempts)
	var captures sync.WaitGroup
	for i := 0; i < attempts; i++ {
		captures.Add(1)
		go func() {
			defer captures.Done()
			data, err := CaptureRollbackState(context.Background(), request, RollbackCaptureOptions{})
			results <- data
			errorsCh <- err
		}()
	}
	captures.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	paths := map[string]bool{first.RollbackPath: true}
	for data := range results {
		if data == nil || paths[data.RollbackPath] {
			t.Fatalf("capture reused another attempt's recovery storage: %v", data)
		}
		paths[data.RollbackPath] = true
		if got := readSnapshotArchive(t, data)["p0"]; got != "later state" {
			t.Fatalf("incomplete concurrent snapshot: %q", got)
		}
	}
	if got := readSnapshotArchive(t, first)["p0"]; got != "first snapshot" {
		t.Fatalf("later attempt destroyed the original snapshot: %q", got)
	}
}

func TestRollbackRefusesSelfContainingSnapshot(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "important"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	request := snapshotRequest(project, ".")
	if data, err := CaptureRollbackState(context.Background(), request, RollbackCaptureOptions{}); err == nil || data != nil || !strings.Contains(err.Error(), "recovery storage") {
		t.Fatalf("self-containing archive not rejected: %v %v", data, err)
	}
	if err := filepath.WalkDir(filepath.Join(project, ".slb"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Name() == rollbackFilesystemTarGz || entry.Name() == rollbackMetadataFilename {
			t.Errorf("published a snapshot that its own command would destroy: %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackResolvesSymlinkBeforeParentTraversal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires unprivileged symlink creation")
	}
	project, outside := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(outside, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "child"), filepath.Join(project, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "target"), []byte("real target"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "target"), []byte("decoy"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := CaptureRollbackState(context.Background(), snapshotRequest(project, "alias/../target"), RollbackCaptureOptions{})
	if err != nil || data == nil {
		t.Fatalf("capture: %v %v", data, err)
	}
	if got := readSnapshotArchive(t, data)["p0"]; got != "real target" {
		t.Fatalf("lexical path cleaning captured a different file: %q", got)
	}
	data, err = CaptureRollbackState(context.Background(), snapshotRequest(project, "alias"), RollbackCaptureOptions{})
	if err != nil || data == nil || data.Filesystem.TotalBytes != 0 {
		t.Fatalf("leaf symlink followed instead of captured as a link: %v %v", data, err)
	}
}

func TestRollbackAlreadyCancelledDoesNotCapture(t *testing.T) {
	project := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	data, err := CaptureRollbackState(ctx, snapshotRequest(project, "missing"), RollbackCaptureOptions{})
	if data != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled capture ignored caller: %v %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(project, ".slb")); !os.IsNotExist(err) {
		t.Fatalf("cancelled capture still wrote recovery storage: %v", err)
	}
}

type rollbackFailingWriter struct {
	remaining int
}

func (w *rollbackFailingWriter) Write(p []byte) (int, error) {
	if len(p) <= w.remaining {
		w.remaining -= len(p)
		return len(p), nil
	}
	n := w.remaining
	w.remaining = 0
	return n, io.ErrClosedPipe
}

func TestRollbackArchiveReportsFinalizationFailure(t *testing.T) {
	// Ten bytes admit the gzip header; buffered body/trailer writes then fail.
	if err := writeRollbackArchive(&rollbackFailingWriter{remaining: 10}, nil); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("archive finalization failure was discarded: %v", err)
	}
	var complete bytes.Buffer
	if err := writeRollbackArchive(&complete, nil); err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(&complete)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatalf("archive trailer not finalized: %v", err)
	}
}
