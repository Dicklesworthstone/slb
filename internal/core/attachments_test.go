package core

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestDefaultAttachmentConfig(t *testing.T) {
	cfg := DefaultAttachmentConfig()
	if cfg.MaxFileSize <= 0 {
		t.Fatalf("expected MaxFileSize > 0, got %d", cfg.MaxFileSize)
	}
	if cfg.MaxOutputSize <= 0 {
		t.Fatalf("expected MaxOutputSize > 0, got %d", cfg.MaxOutputSize)
	}
	if cfg.MaxCommandRuntime <= 0 {
		t.Fatalf("expected MaxCommandRuntime > 0, got %s", cfg.MaxCommandRuntime)
	}
	if cfg.MaxImageSize <= 0 {
		t.Fatalf("expected MaxImageSize > 0, got %d", cfg.MaxImageSize)
	}
}

func TestAttachmentError_Error(t *testing.T) {
	err := &AttachmentError{
		Type:    db.AttachmentTypeFile,
		Path:    "foo.txt",
		Message: "nope",
	}
	if got := err.Error(); !strings.Contains(got, "attachment error") || !strings.Contains(got, "nope") {
		t.Fatalf("unexpected Error(): %q", got)
	}
}

func TestCappedBuffer(t *testing.T) {
	var nilBuf *cappedBuffer
	if n, err := nilBuf.Write([]byte("abc")); err != nil || n != 3 {
		t.Fatalf("nil Write() = (%d,%v)", n, err)
	}
	if nilBuf.String() != "" {
		t.Fatalf("nil String()=%q", nilBuf.String())
	}
	if nilBuf.Truncated() {
		t.Fatalf("nil Truncated()=true")
	}

	b := &cappedBuffer{max: 5}
	if _, err := b.Write([]byte("hello")); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if b.String() != "hello" {
		t.Fatalf("String=%q want %q", b.String(), "hello")
	}
	if b.Truncated() {
		t.Fatalf("expected not truncated")
	}
	if _, err := b.Write([]byte("world")); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if !b.Truncated() {
		t.Fatalf("expected truncated")
	}
	if b.String() != "hello" {
		t.Fatalf("String=%q want %q", b.String(), "hello")
	}
}

func TestCappedBuffer_UnlimitedMode(t *testing.T) {
	// max <= 0 means unlimited
	b := &cappedBuffer{max: 0}
	data := []byte("hello world this is a long string")
	if _, err := b.Write(data); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if b.String() != string(data) {
		t.Fatalf("String=%q want %q", b.String(), string(data))
	}
	if b.Truncated() {
		t.Fatalf("expected not truncated in unlimited mode")
	}

	// Write more data
	if _, err := b.Write([]byte(" more data")); err != nil {
		t.Fatalf("Second Write failed: %v", err)
	}
	if !strings.Contains(b.String(), "more data") {
		t.Fatalf("expected content to include more data")
	}
}

func TestCappedBuffer_PartialWrite(t *testing.T) {
	// Buffer has room for partial write
	b := &cappedBuffer{max: 7}
	if _, err := b.Write([]byte("hello")); err != nil {
		t.Fatalf("First Write failed: %v", err)
	}
	// Only 2 bytes remaining
	n, err := b.Write([]byte("world"))
	if err != nil {
		t.Fatalf("Second Write failed: %v", err)
	}
	if n != 5 {
		t.Fatalf("expected n=5 (input length), got %d", n)
	}
	if b.String() != "hellowo" {
		t.Fatalf("String=%q want %q", b.String(), "hellowo")
	}
	if !b.Truncated() {
		t.Fatalf("expected truncated after partial write")
	}
}

func TestCappedBuffer_AlreadyFull(t *testing.T) {
	b := &cappedBuffer{max: 5}
	if _, err := b.Write([]byte("hello")); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	// Buffer is exactly full now (remaining = 0)
	n, err := b.Write([]byte("x"))
	if err != nil {
		t.Fatalf("Write to full buffer failed: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected n=1 (input length), got %d", n)
	}
	if b.String() != "hello" {
		t.Fatalf("String=%q want %q", b.String(), "hello")
	}
	if !b.Truncated() {
		t.Fatalf("expected truncated")
	}
}

func TestCreateDiffAttachmentAndHelpers(t *testing.T) {
	diff := CreateDiffAttachment("diff --git a/x b/x\n", "HEAD")
	if diff.Type != db.AttachmentTypeGitDiff {
		t.Fatalf("Type=%q want %q", diff.Type, db.AttachmentTypeGitDiff)
	}
	if diff.Metadata["ref"] != "HEAD" {
		t.Fatalf("ref=%v want %q", diff.Metadata["ref"], "HEAD")
	}

	if !isImageFile("x.PNG") || isImageFile("x.txt") {
		t.Fatalf("isImageFile unexpected results")
	}
	if detectImageMimeType("x.png") != "image/png" {
		t.Fatalf("detectImageMimeType png unexpected")
	}
	if detectImageMimeType("x.unknown") != "application/octet-stream" {
		t.Fatalf("detectImageMimeType default unexpected")
	}
	if !isDiffFile("x.diff") || !isDiffFile("x.patch") || isDiffFile("x.txt") {
		t.Fatalf("isDiffFile unexpected results")
	}
	if !isDiffContent([]byte("diff --git a/x b/x\n")) || !isDiffContent([]byte("--- a/x\n")) || !isDiffContent([]byte("@@ -1 +1 @@\n")) {
		t.Fatalf("isDiffContent expected true for diff markers")
	}
	if isDiffContent([]byte("nope")) {
		t.Fatalf("isDiffContent expected false")
	}
}

func TestCreateLogExcerpt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.txt")
	if err := os.WriteFile(path, []byte("a\nb\nc\nd\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	att, err := CreateLogExcerpt(path, 2, 3, nil)
	if err != nil {
		t.Fatalf("CreateLogExcerpt: %v", err)
	}
	if att.Type != db.AttachmentTypeFile {
		t.Fatalf("Type=%q want %q", att.Type, db.AttachmentTypeFile)
	}
	if strings.TrimSpace(att.Content) != "b\nc" {
		t.Fatalf("Content=%q", att.Content)
	}
	if att.Metadata["type"] != "log_excerpt" {
		t.Fatalf("metadata.type=%v", att.Metadata["type"])
	}
}

func TestLoadAttachmentFromFile_DiffAndSizeLimit(t *testing.T) {
	dir := t.TempDir()

	diffPath := filepath.Join(dir, "x.diff")
	if err := os.WriteFile(diffPath, []byte("diff --git a/x b/x\n"), 0o644); err != nil {
		t.Fatalf("write diff: %v", err)
	}

	att, err := LoadAttachmentFromFile(diffPath, nil)
	if err != nil {
		t.Fatalf("LoadAttachmentFromFile(diff): %v", err)
	}
	if att.Type != db.AttachmentTypeGitDiff {
		t.Fatalf("Type=%q want %q", att.Type, db.AttachmentTypeGitDiff)
	}

	smallCfg := &AttachmentConfig{MaxFileSize: 1}
	if _, err := LoadAttachmentFromFile(diffPath, smallCfg); err == nil {
		t.Fatalf("expected size limit error")
	}
}

func TestLoadScreenshot_ValidAndRejectsNonImage(t *testing.T) {
	dir := t.TempDir()

	txtPath := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(txtPath, []byte("hi"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadScreenshot(txtPath, nil); err == nil {
		t.Fatalf("expected error for non-image")
	}

	pngPath := filepath.Join(dir, "x.png")
	writeTinyPNG(t, pngPath, 8, 8)

	att, err := LoadScreenshot(pngPath, nil)
	if err != nil {
		t.Fatalf("LoadScreenshot: %v", err)
	}
	if att.Type != db.AttachmentTypeScreenshot {
		t.Fatalf("Type=%q want %q", att.Type, db.AttachmentTypeScreenshot)
	}
	if !strings.HasPrefix(att.Content, "data:image/png;base64,") {
		t.Fatalf("unexpected data URI prefix: %q", att.Content[:min(len(att.Content), 24)])
	}

	tooSmall := &AttachmentConfig{MaxImageSize: 1}
	if _, err := LoadScreenshot(pngPath, tooSmall); err == nil {
		t.Fatalf("expected image too large error")
	}
}

func TestLoadAttachmentFromFile_ImageBecomesScreenshot(t *testing.T) {
	dir := t.TempDir()
	pngPath := filepath.Join(dir, "x.png")
	writeTinyPNG(t, pngPath, 2, 2)

	att, err := LoadAttachmentFromFile(pngPath, nil)
	if err != nil {
		t.Fatalf("LoadAttachmentFromFile: %v", err)
	}
	if att.Type != db.AttachmentTypeScreenshot {
		t.Fatalf("Type=%q want %q", att.Type, db.AttachmentTypeScreenshot)
	}
	if !strings.HasPrefix(att.Content, "data:image/png;base64,") {
		t.Fatalf("unexpected data URI prefix")
	}
}

func TestRunContextCommand_BasicAndTruncation(t *testing.T) {
	cfg := DefaultAttachmentConfig()
	cfg.MaxCommandRuntime = 0

	att, err := RunContextCommand(context.Background(), "echo hello", &cfg)
	if err != nil {
		t.Fatalf("RunContextCommand: %v", err)
	}
	if att.Type != db.AttachmentTypeContext {
		t.Fatalf("Type=%q want %q", att.Type, db.AttachmentTypeContext)
	}
	if !strings.Contains(strings.ToLower(att.Content), "hello") {
		t.Fatalf("Content=%q", att.Content)
	}

	// Truncation + stderr separator path.
	cfg.MaxOutputSize = 5
	var cmd string
	if runtime.GOOS == "windows" {
		cmd = "echo out & echo err 1>&2"
	} else {
		cmd = "echo out; echo err 1>&2"
	}
	att, err = RunContextCommand(context.Background(), cmd, &cfg)
	if err != nil {
		t.Fatalf("RunContextCommand(trunc): %v", err)
	}
	if _, ok := att.Metadata["truncated"]; !ok {
		t.Fatalf("expected truncated metadata")
	}
	if !strings.Contains(att.Content, "[truncated]") {
		t.Fatalf("expected content to include truncation marker, got %q", att.Content)
	}
}

func TestRunContextCommand_Timeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip timeout test on windows")
	}

	cfg := DefaultAttachmentConfig()
	cfg.MaxCommandRuntime = 10 * time.Millisecond

	att, err := RunContextCommand(context.Background(), "sleep 2", &cfg)
	if err != nil {
		t.Fatalf("RunContextCommand(timeout): %v", err)
	}
	if _, ok := att.Metadata["timed_out"]; !ok {
		t.Fatalf("expected timed_out metadata, got %v", att.Metadata)
	}
}

func writeTinyPNG(t *testing.T, path string, width, height int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	img.Set(0, 0, color.RGBA{R: 255, G: 0, B: 0, A: 255})

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write png: %v", err)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestCreateLogExcerpt_EdgeCases(t *testing.T) {
	t.Run("file not found", func(t *testing.T) {
		_, err := CreateLogExcerpt("/nonexistent/file.log", 1, 10, nil)
		if err == nil {
			t.Fatal("expected error for nonexistent file")
		}
		var ae *AttachmentError
		if !errors.As(err, &ae) {
			t.Fatalf("expected AttachmentError, got %T", err)
		}
	})

	t.Run("startLine less than 1", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "log.txt")
		if err := os.WriteFile(path, []byte("line1\nline2\nline3\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}

		att, err := CreateLogExcerpt(path, 0, 2, nil)
		if err != nil {
			t.Fatalf("CreateLogExcerpt: %v", err)
		}
		// startLine should be adjusted to 1
		if !strings.Contains(att.Content, "line1") {
			t.Fatalf("expected content to start with line1, got %q", att.Content)
		}
	})

	t.Run("endLine exceeds total lines", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "log.txt")
		if err := os.WriteFile(path, []byte("line1\nline2\nline3"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}

		att, err := CreateLogExcerpt(path, 1, 100, nil)
		if err != nil {
			t.Fatalf("CreateLogExcerpt: %v", err)
		}
		// Should include all lines
		if !strings.Contains(att.Content, "line3") {
			t.Fatalf("expected content to include line3, got %q", att.Content)
		}
	})

	t.Run("startLine greater than endLine", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "log.txt")
		if err := os.WriteFile(path, []byte("line1\nline2\nline3\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}

		att, err := CreateLogExcerpt(path, 5, 2, nil)
		if err != nil {
			t.Fatalf("CreateLogExcerpt: %v", err)
		}
		// startLine should be adjusted to equal endLine
		if att.Metadata["lines"] != "2-2" {
			t.Fatalf("expected lines metadata to be 2-2, got %v", att.Metadata["lines"])
		}
	})

	t.Run("endLine less than 1", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "log.txt")
		if err := os.WriteFile(path, []byte("line1\nline2\nline3\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}

		att, err := CreateLogExcerpt(path, 1, 0, nil)
		if err != nil {
			t.Fatalf("CreateLogExcerpt: %v", err)
		}
		// Should include all lines (endLine adjusted to len(lines))
		totalLines, ok := att.Metadata["total_lines"].(int)
		if !ok || totalLines < 3 {
			t.Fatalf("expected total_lines >= 3, got %v", att.Metadata["total_lines"])
		}
	})
}

func TestLoadAttachmentFromFile_Errors(t *testing.T) {
	t.Run("file not found", func(t *testing.T) {
		_, err := LoadAttachmentFromFile("/nonexistent/file.txt", nil)
		if err == nil {
			t.Fatal("expected error for nonexistent file")
		}
	})

	t.Run("unreadable file", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("permission test not reliable on windows")
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "noperm.txt")
		if err := os.WriteFile(path, []byte("test"), 0o000); err != nil {
			t.Fatalf("write: %v", err)
		}
		defer os.Chmod(path, 0o644) // cleanup

		_, err := LoadAttachmentFromFile(path, nil)
		if err == nil {
			t.Fatal("expected error for unreadable file")
		}
	})
}

func TestLoadScreenshot_Errors(t *testing.T) {
	t.Run("file not found", func(t *testing.T) {
		_, err := LoadScreenshot("/nonexistent/image.png", nil)
		if err == nil {
			t.Fatal("expected error for nonexistent file")
		}
	})

	t.Run("corrupt image file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "corrupt.png")
		// Write invalid PNG data
		if err := os.WriteFile(path, []byte("not a png file"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}

		// LoadScreenshot errors on corrupt images (it tries to decode them)
		_, err := LoadScreenshot(path, nil)
		if err == nil {
			t.Fatal("expected error for corrupt image")
		}
		var ae *AttachmentError
		if !errors.As(err, &ae) {
			t.Fatalf("expected AttachmentError, got %T", err)
		}
	})
}

func TestRunContextCommand_NilConfig(t *testing.T) {
	// RunContextCommand uses defaults for nil config
	att, err := RunContextCommand(context.Background(), "echo hello", nil)
	if err != nil {
		t.Fatalf("RunContextCommand with nil config error: %v", err)
	}
	if att.Type != db.AttachmentTypeContext {
		t.Fatalf("Type=%q want %q", att.Type, db.AttachmentTypeContext)
	}
}

func TestRunContextCommand_EmptyCommand(t *testing.T) {
	cfg := DefaultAttachmentConfig()
	cfg.MaxCommandRuntime = 0

	// Empty command runs shell -c "" which returns quickly with empty output
	att, err := RunContextCommand(context.Background(), "", &cfg)
	if err != nil {
		t.Fatalf("RunContextCommand with empty command error: %v", err)
	}
	if att.Type != db.AttachmentTypeContext {
		t.Fatalf("Type=%q want %q", att.Type, db.AttachmentTypeContext)
	}
}

func TestRunContextCommand_ContextCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip context test on windows")
	}

	cfg := DefaultAttachmentConfig()
	cfg.MaxCommandRuntime = 0 // No timeout from config

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	// Cancelled context returns immediately with output (possibly empty)
	att, err := RunContextCommand(ctx, "sleep 10", &cfg)
	if err != nil {
		t.Fatalf("RunContextCommand with cancelled context error: %v", err)
	}
	// Should have some metadata about the result
	if att.Type != db.AttachmentTypeContext {
		t.Fatalf("Type=%q want %q", att.Type, db.AttachmentTypeContext)
	}
}

func TestCappedBuffer_Write(t *testing.T) {
	t.Run("nil buffer accepts all writes", func(t *testing.T) {
		var buf *cappedBuffer = nil
		n, err := buf.Write([]byte("hello"))
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if n != 5 {
			t.Errorf("expected n=5, got %d", n)
		}
	})

	t.Run("no max writes everything", func(t *testing.T) {
		buf := &cappedBuffer{max: 0}
		n, err := buf.Write([]byte("hello"))
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if n != 5 {
			t.Errorf("expected n=5, got %d", n)
		}
		if buf.buf.String() != "hello" {
			t.Errorf("expected buffer 'hello', got %q", buf.buf.String())
		}
		if buf.truncated {
			t.Error("expected truncated=false")
		}
	})

	t.Run("respects max limit", func(t *testing.T) {
		buf := &cappedBuffer{max: 10}
		n, err := buf.Write([]byte("hello"))
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if n != 5 {
			t.Errorf("expected n=5, got %d", n)
		}
		if buf.truncated {
			t.Error("expected truncated=false after first write")
		}

		// Write more data to exceed max
		n, err = buf.Write([]byte("world!"))
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if n != 6 {
			t.Errorf("expected n=6, got %d", n)
		}
		if !buf.truncated {
			t.Error("expected truncated=true after exceeding max")
		}
		// Buffer should only contain 10 bytes
		if buf.buf.Len() != 10 {
			t.Errorf("expected buffer length 10, got %d", buf.buf.Len())
		}
	})

	t.Run("already at max drops writes", func(t *testing.T) {
		buf := &cappedBuffer{max: 5}
		buf.Write([]byte("hello"))
		// Now at max
		n, err := buf.Write([]byte("more"))
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if n != 4 {
			t.Errorf("expected n=4, got %d", n)
		}
		if !buf.truncated {
			t.Error("expected truncated=true")
		}
		if buf.buf.Len() != 5 {
			t.Errorf("expected buffer length 5, got %d", buf.buf.Len())
		}
	})
}

func TestAttachmentFileLimits(t *testing.T) {
	for _, loader := range []struct {
		name string
		load func(string, *AttachmentConfig) (*db.Attachment, error)
	}{
		{"file", LoadAttachmentFromFile},
		{"screenshot", LoadScreenshot},
		{"excerpt", func(path string, cfg *AttachmentConfig) (*db.Attachment, error) {
			return CreateLogExcerpt(path, 1, 0, cfg)
		}},
	} {
		t.Run(loader.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fixture.txt")
			if loader.name == "screenshot" {
				path = filepath.Join(t.TempDir(), "fixture.png")
				writeTinyPNG(t, path, 2, 2)
			} else if err := os.WriteFile(path, []byte("alpha\nbeta"), 0o600); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, limit := range []int64{0, -1, 1<<63 - 1, info.Size(), info.Size() - 1} {
				cfg := DefaultAttachmentConfig()
				cfg.MaxFileSize = limit
				att, err := loader.load(path, &cfg)
				if limit > 0 && limit < info.Size() {
					var ae *AttachmentError
					if att != nil || !errors.As(err, &ae) || !strings.Contains(ae.Message, "too large") {
						t.Errorf("limit %d: expected typed size error and no attachment, got %#v, %v", limit, att, err)
					}
				} else if err != nil || att == nil {
					t.Errorf("limit %d: expected attachment, got %#v, %v", limit, att, err)
				}
			}
			cfg := DefaultAttachmentConfig()
			cfg.AllowedFileTypes = []string{".pdf"}
			if att, err := loader.load(path, &cfg); err == nil || att != nil {
				t.Fatalf("file-type restriction ignored: %#v, %v", att, err)
			}
			cfg.AllowedFileTypes = []string{strings.ToUpper(strings.TrimPrefix(filepath.Ext(path), "."))}
			if _, err := loader.load(path, &cfg); err != nil {
				t.Fatalf("case-insensitive extension without dot: %v", err)
			}
		})
	}
}

func TestAttachmentImageValidation(t *testing.T) {
	for _, loader := range []struct {
		name string
		load func(string, *AttachmentConfig) (*db.Attachment, error)
	}{
		{"file", LoadAttachmentFromFile},
		{"screenshot", LoadScreenshot},
	} {
		t.Run(loader.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "image.jpg")
			writeTinyPNG(t, path, 8, 2) // MIME must describe the bytes, not the suffix.
			cfg := DefaultAttachmentConfig()
			cfg.MaxImageSize = 4
			if att, err := loader.load(path, &cfg); err == nil || att != nil {
				t.Errorf("image dimension limit ignored: %#v, %v", att, err)
			}
			cfg.MaxImageSize = 8
			att, err := loader.load(path, &cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(att.Content, "data:image/png;base64,") || att.Metadata["width"] != 8 || att.Metadata["height"] != 2 {
				t.Errorf("incorrect image metadata: %#v", att)
			}
			corrupt := filepath.Join(t.TempDir(), "corrupt.png")
			if err := os.WriteFile(corrupt, []byte("not an image"), 0o600); err != nil {
				t.Fatal(err)
			}
			if att, err := loader.load(corrupt, nil); err == nil || att != nil {
				t.Errorf("corrupt image accepted: %#v, %v", att, err)
			}
		})
	}
}

func TestCreateLogExcerpt_LargeSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 128*1024)+"\nok\nlast"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultAttachmentConfig()
	cfg.MaxFileSize = 7
	for _, tc := range []struct {
		start, end int
		want, span string
	}{
		{2, 3, "ok\nlast", "2-3"},
		{2, 2, "ok", "2-2"},
		{20, 30, "last", "3-3"},
	} {
		att, err := CreateLogExcerpt(path, tc.start, tc.end, &cfg)
		if err != nil {
			t.Fatalf("small excerpt from large log: %v", err)
		}
		if att.Content != tc.want || att.Metadata["lines"] != tc.span || att.Metadata["total_lines"] != 3 {
			t.Errorf("range %d-%d: %#v", tc.start, tc.end, att)
		}
	}
	if att, err := CreateLogExcerpt(path, 1, 1, &cfg); err == nil || att != nil {
		t.Fatalf("oversized selected line was not rejected: attachment present=%v, error=%v", att != nil, err)
	}
}

func TestReadAttachmentContent_Bounded(t *testing.T) {
	for _, tc := range []struct {
		data  string
		limit int64
		valid bool
	}{
		{"", 4, true},
		{"123", 4, true},
		{"1234", 4, true},
		{"12345", 4, false},
		{strings.Repeat("x", 128*1024), 4, false},
		{"12345", 0, true},
		{"12345", -1, true},
		{"12345", 1<<63 - 1, true},
	} {
		reader := strings.NewReader(tc.data)
		data, err := readAttachmentContent(reader, tc.limit)
		if tc.valid {
			if err != nil || string(data) != tc.data {
				t.Errorf("limit %d: read %q, %v", tc.limit, data, err)
			}
		} else {
			if err == nil || data != nil {
				t.Errorf("limit %d: oversized input accepted", tc.limit)
			}
			if consumed := len(tc.data) - reader.Len(); int64(consumed) > tc.limit+1 {
				t.Errorf("read %d bytes with limit %d", consumed, tc.limit)
			}
		}
	}
	readErr := errors.New("read failed")
	for _, reader := range []io.Reader{
		iotest.ErrReader(readErr),
		io.MultiReader(strings.NewReader("1234"), iotest.ErrReader(readErr)),
	} {
		if data, err := readAttachmentContent(reader, 4); data != nil || !errors.Is(err, readErr) {
			t.Errorf("read failure was hidden: %q, %v", data, err)
		}
	}
}

func TestCreateLogExcerpt_RangeCompatibility(t *testing.T) {
	for _, content := range []string{"", "\n", "a", "a\n", "a\r\nb\n", "one\n\nthree", strings.Repeat("x", 8192) + "\nlast\n"} {
		path := filepath.Join(t.TempDir(), "range.log")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(content, "\n")
		for _, start := range []int{-1, 0, 1, 2, 3, 10} {
			for _, end := range []int{-1, 0, 1, 2, 3, 10} {
				// Reference the original slicing contract independently of the
				// streaming implementation, including clamping past EOF.
				first, last := start, end
				if first < 1 {
					first = 1
				}
				if last < 1 || last > len(lines) {
					last = len(lines)
				}
				if first > last {
					first = last
				}
				want := strings.Join(lines[first-1:last], "\n")
				cfg := DefaultAttachmentConfig()
				cfg.MaxFileSize = 8
				att, err := CreateLogExcerpt(path, start, end, &cfg)
				if len(want) > 8 {
					if err == nil || att != nil {
						t.Fatalf("range %d-%d: oversized excerpt accepted", start, end)
					}
					continue
				}
				if err != nil {
					t.Fatalf("range %d-%d: %v", start, end, err)
				}
				if att.Content != want || att.Metadata["total_lines"] != len(lines) {
					t.Errorf("range %d-%d: got %#v, want %q", start, end, att, want)
				}
			}
		}
	}
}

func TestAttachmentRejectsNonRegularFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "directory.png")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, load := range []func(string, *AttachmentConfig) (*db.Attachment, error){
		LoadAttachmentFromFile, LoadScreenshot,
		func(path string, cfg *AttachmentConfig) (*db.Attachment, error) {
			return CreateLogExcerpt(path, 1, 0, cfg)
		},
	} {
		att, err := load(path, nil)
		var ae *AttachmentError
		if att != nil || !errors.As(err, &ae) || !strings.Contains(ae.Message, "regular file") {
			t.Fatalf("expected typed non-file error, got %#v, %v", att, err)
		}
	}
}

func TestCappedBuffer_EmptyWriteAtLimit(t *testing.T) {
	b := &cappedBuffer{max: 4}
	if _, err := b.Write([]byte("1234")); err != nil {
		t.Fatal(err)
	}
	if n, err := b.Write(nil); n != 0 || err != nil || b.Truncated() {
		t.Fatalf("empty write reported truncation: n=%d, err=%v, truncated=%v", n, err, b.Truncated())
	}
}

func TestRunContextCommand_SingleStreamTruncation(t *testing.T) {
	for _, command := range []string{"echo abcdefghij", "echo abcdefghij 1>&2"} {
		cfg := DefaultAttachmentConfig()
		cfg.MaxOutputSize = 4
		att, err := RunContextCommand(context.Background(), command, &cfg)
		if err != nil {
			t.Fatal(err)
		}
		if att.Content != "abcd\n... [truncated]" || att.Metadata["truncated"] != true {
			t.Errorf("truncation must be visible in content and metadata: %#v", att)
		}
	}
}

func TestRunContextCommand_DescendantPipeBounds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell process fixtures")
	}
	t.Setenv("SHELL", "/bin/sh")
	for _, tc := range []struct {
		name, command string
		timeout       time.Duration
		cancel        bool
		exitCode      int
	}{
		{"deadline", "sleep 3 & wait", 50 * time.Millisecond, false, -1},
		{"cancellation", "sleep 3 & wait", 0, true, -1},
		{"successful shell leaves pipes", "sleep 3 & exit 0", 0, false, -1},
		{"failed shell leaves pipes", "sleep 3 & exit 7", 0, false, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultAttachmentConfig()
			cfg.MaxCommandRuntime = tc.timeout
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				timer := time.AfterFunc(50*time.Millisecond, cancel)
				defer timer.Stop()
			}
			start := time.Now()
			att, err := RunContextCommand(ctx, tc.command, &cfg)
			elapsed := time.Since(start)
			if err != nil {
				t.Fatal(err)
			}
			if elapsed >= 2*time.Second {
				t.Errorf("inherited output pipes defeated the lifetime bound: %s", elapsed)
			}
			if att.Metadata["exit_code"] != tc.exitCode {
				t.Errorf("exit_code=%v, want %d", att.Metadata["exit_code"], tc.exitCode)
			}
			if got := att.Metadata["timed_out"] == true; got != (tc.timeout > 0) {
				t.Errorf("timed_out=%v for %#v", got, att.Metadata)
			}
			if tc.cancel && att.Metadata["cancelled"] != true {
				t.Errorf("cancellation was not disclosed: %#v", att.Metadata)
			}
			if tc.exitCode == -1 && att.Metadata["error"] == nil {
				t.Errorf("transport/cancellation failure was not disclosed: %#v", att.Metadata)
			}
		})
	}
}

func TestRunContextCommand_CancelsDescendantWork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process-group cancellation")
	}
	t.Setenv("SHELL", "/bin/sh")
	marker := filepath.Join(t.TempDir(), "must-not-be-written")
	t.Setenv("SLB_TEST_CONTEXT_MARKER", marker)
	cfg := DefaultAttachmentConfig()
	cfg.MaxCommandRuntime = 50 * time.Millisecond
	att, err := RunContextCommand(context.Background(), `(sleep 1; printf leaked > "$SLB_TEST_CONTEXT_MARKER") & wait`, &cfg)
	if err != nil || att == nil || att.Metadata["timed_out"] != true {
		t.Fatalf("expected timed-out attachment, got %#v, %v", att, err)
	}
	// A bounded pipe wait alone is insufficient: ordinary descendants must
	// also stop working after their context has been cancelled.
	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("descendant continued after cancellation: stat error=%v", err)
	}
}

func TestRunContextCommand_NonzeroExitPreservesOutput(t *testing.T) {
	command := "echo evidence; exit 7"
	if runtime.GOOS == "windows" {
		command = "echo evidence & exit 7"
	}
	att, err := RunContextCommand(nil, command, nil)
	if err != nil {
		t.Fatal(err)
	}
	if att.Metadata["exit_code"] != 7 || !strings.Contains(att.Content, "evidence") || att.Metadata["timed_out"] == true {
		t.Fatalf("lost ordinary child result: %#v", att)
	}
}
