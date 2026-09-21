// Package core implements attachment handling for SLB requests.
package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // Register GIF format
	_ "image/jpeg" // Register JPEG format
	_ "image/png"  // Register PNG format
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

// AttachmentConfig holds configuration for attachment handling.
type AttachmentConfig struct {
	// MaxFileSize bounds file/image bytes and selected log excerpts (default 1MB).
	// Nonpositive values mean no size limit.
	MaxFileSize int64
	// MaxOutputSize is the maximum output size for context commands (default 100KB).
	MaxOutputSize int64
	// MaxCommandRuntime is the maximum runtime for context commands (default 10s).
	// Zero means no timeout.
	MaxCommandRuntime time.Duration
	// MaxImageSize is the maximum dimension for images (default 4096x4096).
	MaxImageSize int
	// AllowedFileTypes restricts filename extensions (empty means all allowed).
	// Extensions are case-insensitive and may include the leading dot.
	AllowedFileTypes []string
}

// DefaultAttachmentConfig returns default configuration.
func DefaultAttachmentConfig() AttachmentConfig {
	return AttachmentConfig{
		MaxFileSize:       1024 * 1024, // 1MB
		MaxOutputSize:     100 * 1024,  // 100KB
		MaxCommandRuntime: 10 * time.Second,
		MaxImageSize:      4096,       // 4096px
		AllowedFileTypes:  []string{}, // Allow all
	}
}

// AttachmentError represents an attachment processing error.
type AttachmentError struct {
	Type    db.AttachmentType
	Path    string
	Message string
}

func (e *AttachmentError) Error() string {
	return fmt.Sprintf("attachment error (%s): %s", e.Type, e.Message)
}

// LoadAttachmentFromFile reads a file and creates an attachment.
func LoadAttachmentFromFile(path string, config *AttachmentConfig) (*db.Attachment, error) {
	if config == nil {
		cfg := DefaultAttachmentConfig()
		config = &cfg
	}

	// Resolve path
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, &AttachmentError{
			Type:    db.AttachmentTypeFile,
			Path:    path,
			Message: fmt.Sprintf("resolving path: %v", err),
		}
	}

	content, err := readAttachmentFile(absPath, config)
	if err != nil {
		return nil, &AttachmentError{
			Type:    db.AttachmentTypeFile,
			Path:    path,
			Message: fmt.Sprintf("reading file: %v", err),
		}
	}

	// Detect if this is an image
	attachType := db.AttachmentTypeFile
	if isImageFile(absPath) {
		return screenshotFromContent(absPath, content, config)
	} else if isDiffFile(absPath) || isDiffContent(content) {
		attachType = db.AttachmentTypeGitDiff
	}

	return &db.Attachment{
		Type:    attachType,
		Content: string(content),
		Metadata: map[string]any{
			"source":   absPath,
			"filename": filepath.Base(absPath),
			"size":     int64(len(content)),
		},
	}, nil
}

// LoadScreenshot loads an image file as a screenshot attachment.
func LoadScreenshot(path string, config *AttachmentConfig) (*db.Attachment, error) {
	if config == nil {
		cfg := DefaultAttachmentConfig()
		config = &cfg
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, &AttachmentError{
			Type:    db.AttachmentTypeScreenshot,
			Path:    path,
			Message: fmt.Sprintf("resolving path: %v", err),
		}
	}

	// Verify it's an image
	if !isImageFile(absPath) {
		return nil, &AttachmentError{
			Type:    db.AttachmentTypeScreenshot,
			Path:    path,
			Message: "file is not a recognized image format",
		}
	}

	content, err := readAttachmentFile(absPath, config)
	if err != nil {
		return nil, &AttachmentError{
			Type:    db.AttachmentTypeScreenshot,
			Path:    path,
			Message: fmt.Sprintf("reading file: %v", err),
		}
	}
	return screenshotFromContent(absPath, content, config)
}

// Validate and encode the same bounded snapshot, rather than reopening a path
// after checking its image header. Both file and screenshot flags use this.
func screenshotFromContent(path string, content []byte, config *AttachmentConfig) (*db.Attachment, error) {
	imgConfig, format, err := image.DecodeConfig(bytes.NewReader(content))
	if err != nil {
		return nil, &AttachmentError{
			Type:    db.AttachmentTypeScreenshot,
			Path:    path,
			Message: fmt.Sprintf("decoding image: %v", err),
		}
	}

	if config.MaxImageSize > 0 {
		if imgConfig.Width > config.MaxImageSize || imgConfig.Height > config.MaxImageSize {
			return nil, &AttachmentError{
				Type:    db.AttachmentTypeScreenshot,
				Path:    path,
				Message: fmt.Sprintf("image too large: %dx%d (max %d)", imgConfig.Width, imgConfig.Height, config.MaxImageSize),
			}
		}
	}

	mimeType := detectImageMimeType("image." + format)
	dataURI := fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(content))

	return &db.Attachment{
		Type:    db.AttachmentTypeScreenshot,
		Content: dataURI,
		Metadata: map[string]any{
			"source":      path,
			"filename":    filepath.Base(path),
			"size":        int64(len(content)),
			"width":       imgConfig.Width,
			"height":      imgConfig.Height,
			"description": "",
		},
	}, nil
}

// openAttachmentFile rejects ordinary non-file inputs before opening them:
// opening a FIFO can block, and reading a device can be unbounded. Recheck the
// opened descriptor too. This is not a sandbox against concurrent path swaps.
func openAttachmentFile(path string, config *AttachmentConfig) (*os.File, error) {
	if len(config.AllowedFileTypes) > 0 {
		ext := strings.TrimPrefix(filepath.Ext(path), ".")
		allowed := false
		for _, candidate := range config.AllowedFileTypes {
			candidate = strings.TrimPrefix(strings.TrimSpace(candidate), ".")
			if candidate != "" && strings.EqualFold(candidate, ext) {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, fmt.Errorf("file type %q is not allowed", filepath.Ext(path))
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("attachment source is not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("attachment source is not a regular file")
	}
	return f, nil
}

func readAttachmentFile(path string, config *AttachmentConfig) ([]byte, error) {
	f, err := openAttachmentFile(path, config)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readAttachmentContent(f, config.MaxFileSize)
}

// Bound the actual read, not just an earlier Stat size. Read a separate probe
// byte to distinguish exact-limit files without overflowing a MaxInt64 limit.
func readAttachmentContent(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return io.ReadAll(r)
	}
	content, err := io.ReadAll(io.LimitReader(r, limit))
	if err != nil {
		return nil, err
	}
	var probe [1]byte
	n, err := io.ReadFull(r, probe[:])
	if n > 0 {
		return nil, fmt.Errorf("file too large (max %d bytes)", limit)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return content, nil
}

type cappedBuffer struct {
	max       int64
	truncated bool
	buf       bytes.Buffer
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b == nil || len(p) == 0 {
		return len(p), nil
	}
	if b.max <= 0 {
		if _, err := b.buf.Write(p); err != nil {
			return 0, err
		}
		return len(p), nil
	}

	remaining := b.max - int64(b.buf.Len())
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}

	if int64(len(p)) > remaining {
		if _, err := b.buf.Write(p[:remaining]); err != nil {
			return 0, err
		}
		b.truncated = true
		return len(p), nil
	}

	if _, err := b.buf.Write(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string {
	if b == nil {
		return ""
	}
	return b.buf.String()
}

func (b *cappedBuffer) Truncated() bool {
	if b == nil {
		return false
	}
	return b.truncated
}

// RunContextCommand executes a command and captures output as an attachment.
func RunContextCommand(ctx context.Context, command string, config *AttachmentConfig) (*db.Attachment, error) {
	if config == nil {
		cfg := DefaultAttachmentConfig()
		config = &cfg
	}

	if ctx == nil {
		ctx = context.Background()
	}

	execCtx := ctx
	cancel := func() {}
	if config.MaxCommandRuntime > 0 {
		execCtx, cancel = context.WithTimeout(ctx, config.MaxCommandRuntime)
	}
	defer cancel()

	startTime := time.Now()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(execCtx, "cmd.exe", "/C", command)
	} else {
		shell := strings.TrimSpace(os.Getenv("SHELL"))
		if shell == "" {
			shell = "/bin/sh"
		}
		cmd = exec.CommandContext(execCtx, shell, "-c", command)
	}
	cmd.Env = os.Environ()

	stdout := &cappedBuffer{max: config.MaxOutputSize}
	stderr := &cappedBuffer{max: config.MaxOutputSize}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Context attachments are noninteractive. Reuse execution's process-group
	// cancellation, and bound pipe draining even when the shell exits first.
	cmd.WaitDelay = time.Second
	stopGroup := configureCommandCancellation(cmd, false)

	runErr := cmd.Run()
	if runErr != nil {
		if stopGroup != nil {
			if stopErr := stopGroup(); stopErr != nil && !errors.Is(stopErr, os.ErrProcessDone) {
				runErr = errors.Join(runErr, fmt.Errorf("stopping context command process group: %w", stopErr))
			}
		}
		if execCtx.Err() != nil {
			runErr = errors.Join(runErr, execCtx.Err())
		}
	}

	duration := time.Since(startTime)

	// Combine output
	var output strings.Builder
	output.WriteString(stdout.String())
	if stderr.String() != "" {
		if output.Len() > 0 {
			output.WriteString("\n--- stderr ---\n")
		}
		output.WriteString(stderr.String())
	}

	exitCode := 0
	timedOut := false
	var exitErr *exec.ExitError
	if runErr != nil {
		if errors.Is(runErr, context.DeadlineExceeded) || errors.Is(execCtx.Err(), context.DeadlineExceeded) {
			timedOut = true
		}
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	outputStr := output.String()
	if outputStr == "" && runErr != nil {
		outputStr = runErr.Error()
	}

	truncated := stdout.Truncated() || stderr.Truncated()
	if config.MaxOutputSize > 0 && int64(len(outputStr)) > config.MaxOutputSize {
		truncated = true
		outputStr = outputStr[:config.MaxOutputSize]
	}
	// The per-stream buffers may already have discarded data, leaving exactly
	// MaxOutputSize bytes. Disclose that loss even without a second stream.
	if truncated {
		outputStr += "\n... [truncated]"
	}

	meta := map[string]any{
		"source":      command,
		"exit_code":   exitCode,
		"duration_ms": duration.Milliseconds(),
	}
	if timedOut {
		meta["timed_out"] = true
	}
	if errors.Is(runErr, context.Canceled) {
		meta["cancelled"] = true
	}
	if truncated {
		meta["truncated"] = true
	}
	if runErr != nil {
		meta["error"] = runErr.Error()
	}

	return &db.Attachment{
		Type:     db.AttachmentTypeContext,
		Content:  outputStr,
		Metadata: meta,
	}, nil
}

// CreateLogExcerpt creates a log excerpt attachment from a file.
func CreateLogExcerpt(path string, startLine, endLine int, config *AttachmentConfig) (*db.Attachment, error) {
	if config == nil {
		cfg := DefaultAttachmentConfig()
		config = &cfg
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, &AttachmentError{
			Type:    db.AttachmentTypeFile,
			Path:    path,
			Message: fmt.Sprintf("resolving path: %v", err),
		}
	}

	f, err := openAttachmentFile(absPath, config)
	if err != nil {
		return nil, &AttachmentError{
			Type:    db.AttachmentTypeFile,
			Path:    path,
			Message: fmt.Sprintf("reading file: %v", err),
		}
	}
	defer f.Close()

	// Match the existing 1-based clamping semantics, including an empty final
	// line after a trailing newline. Stream skipped lines without retaining
	// the entire log (or allocating an unbounded buffer for a single line).
	if startLine < 1 {
		startLine = 1
	}
	if endLine > 0 && startLine > endLine {
		startLine = endLine
	}
	reader := bufio.NewReader(f)
	line := cappedBuffer{max: config.MaxFileSize}
	excerpt := cappedBuffer{max: config.MaxFileSize}
	totalLines, first, last := 1, 0, 0
	for {
		chunk, readErr := reader.ReadSlice('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, bufio.ErrBufferFull) {
			return nil, &AttachmentError{Type: db.AttachmentTypeFile, Path: path, Message: fmt.Sprintf("reading file: %v", readErr)}
		}
		if len(chunk) > 0 && chunk[len(chunk)-1] == '\n' {
			chunk = chunk[:len(chunk)-1]
		}
		_, _ = line.Write(chunk) // cappedBuffer always consumes writes without error.
		selected := totalLines >= startLine && (endLine < 1 || totalLines <= endLine)
		if selected && line.Truncated() {
			return nil, &AttachmentError{Type: db.AttachmentTypeFile, Path: path, Message: fmt.Sprintf("log excerpt too large (max %d bytes)", config.MaxFileSize)}
		}
		if errors.Is(readErr, bufio.ErrBufferFull) {
			continue
		}
		// A requested start past EOF clamps to the last line.
		if selected || (errors.Is(readErr, io.EOF) && first == 0) {
			if first == 0 {
				first = totalLines
			} else {
				_, _ = excerpt.Write([]byte("\n"))
			}
			_, _ = excerpt.Write([]byte(line.String()))
			if line.Truncated() || excerpt.Truncated() {
				return nil, &AttachmentError{Type: db.AttachmentTypeFile, Path: path, Message: fmt.Sprintf("log excerpt too large (max %d bytes)", config.MaxFileSize)}
			}
			last = totalLines
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		totalLines++
		line.buf.Reset()
		line.truncated = false
	}

	return &db.Attachment{
		Type:    db.AttachmentTypeFile, // Log excerpts are a type of file attachment
		Content: excerpt.String(),
		Metadata: map[string]any{
			"file":        absPath,
			"lines":       fmt.Sprintf("%d-%d", first, last),
			"total_lines": totalLines,
			"type":        "log_excerpt",
		},
	}, nil
}

// CreateDiffAttachment creates a diff attachment from git or a file.
func CreateDiffAttachment(diffContent string, ref string) *db.Attachment {
	return &db.Attachment{
		Type:    db.AttachmentTypeGitDiff,
		Content: diffContent,
		Metadata: map[string]any{
			"ref": ref,
		},
	}
}

// Helper functions

func isImageFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	imageExts := map[string]bool{
		".png":  true,
		".jpg":  true,
		".jpeg": true,
		".gif":  true,
		".bmp":  true,
		".webp": true,
	}
	return imageExts[ext]
}

func detectImageMimeType(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	mimeTypes := map[string]string{
		".png":  "image/png",
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".gif":  "image/gif",
		".bmp":  "image/bmp",
		".webp": "image/webp",
	}
	if mime, ok := mimeTypes[ext]; ok {
		return mime
	}
	return "application/octet-stream"
}

func isDiffFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".diff" || ext == ".patch"
}

func isDiffContent(content []byte) bool {
	s := string(content)
	// Look for diff markers
	return strings.HasPrefix(s, "diff ") ||
		strings.HasPrefix(s, "--- ") ||
		strings.HasPrefix(s, "@@")
}
