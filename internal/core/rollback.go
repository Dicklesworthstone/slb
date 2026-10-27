// Package core implements rollback state capture and restoration.
package core

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/mattn/go-shellwords"
)

const (
	rollbackDataVersion          = 1
	defaultRollbackRetention     = 30 * 24 * time.Hour
	defaultRollbackCmdTimeout    = 30 * time.Second
	rollbackMetadataFilename     = "metadata.json"
	rollbackFilesystemTarGz      = "files.tar.gz"
	rollbackKindFilesystem       = "filesystem"
	rollbackKindGit              = "git"
	rollbackKindKubernetes       = "kubernetes"
	rollbackKubernetesDirName    = "k8s"
	rollbackGitDirName           = "git"
	rollbackGitHeadFilename      = "head.txt"
	rollbackGitBranchFilename    = "branch.txt"
	rollbackGitStatusFilename    = "status.txt"
	rollbackGitDiffFilename      = "diff.patch"
	rollbackGitCachedFilename    = "diff_cached.patch"
	rollbackGitUntrackedFilename = "untracked.txt"
)

type RollbackCaptureOptions struct {
	// MaxSizeBytes limits capture. Kubernetes also has fixed per-object and
	// aggregate bounds; 0 disables the configured limit, not those bounds.
	MaxSizeBytes int64
	// Retention controls cleanup of old rollback captures. 0 uses the default.
	Retention time.Duration
	// Now overrides time.Now for tests.
	Now func() time.Time
}

type RollbackRestoreOptions struct {
	// Force allows overwriting existing files/resources and destructive git restores.
	Force bool
}

type RollbackData struct {
	Version      int       `json:"version"`
	RequestID    string    `json:"request_id"`
	CapturedAt   time.Time `json:"captured_at"`
	ProjectPath  string    `json:"project_path"`
	CommandRaw   string    `json:"command_raw"`
	CommandCwd   string    `json:"command_cwd"`
	RollbackPath string    `json:"rollback_path"`
	Kind         string    `json:"kind"`

	Filesystem *FilesystemRollbackData `json:"filesystem,omitempty"`
	Git        *GitRollbackData        `json:"git,omitempty"`
	Kubernetes *KubernetesRollbackData `json:"kubernetes,omitempty"`
}

type FilesystemRollbackData struct {
	TarGz      string            `json:"tar_gz"`
	Roots      []FilesystemRoot  `json:"roots"`
	TotalBytes int64             `json:"total_bytes"`
	Missing    []string          `json:"missing,omitempty"`
	Notes      map[string]string `json:"notes,omitempty"`
}

type FilesystemRoot struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

type KubernetesRollbackData struct {
	Namespace      string                 `json:"namespace"`
	Context        string                 `json:"context"`
	Server         string                 `json:"server"`
	ConfigFiles    []KubernetesConfigFile `json:"config_files"`
	Manifests      []string               `json:"manifests"`
	ManifestSHA256 map[string]string      `json:"manifest_sha256"`
}

// Kubeconfig contents can contain credentials. Keep only their paths and
// integrity fingerprints, never their raw contents or authentication fields.
type KubernetesConfigFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// CaptureRollbackState captures pre-execution state for supported destructive commands.
// If the command type is unsupported, it returns (nil, nil).
func CaptureRollbackState(ctx context.Context, req *db.Request, opts RollbackCaptureOptions) (*RollbackData, error) {
	if req == nil {
		return nil, fmt.Errorf("request is required")
	}
	if strings.TrimSpace(req.ID) == "" {
		return nil, fmt.Errorf("request id is required")
	}
	if strings.TrimSpace(req.ProjectPath) == "" {
		return nil, fmt.Errorf("project path is required")
	}
	if strings.TrimSpace(req.Command.Raw) == "" {
		return nil, fmt.Errorf("command is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	opts = normalizeRollbackCaptureOptions(opts)

	// A classifier's display form is not execution argv: it strips quoting,
	// expands wrappers, and may contain only the first command in a list.
	tokens, err := rollbackCommandTokens(req.Command)
	if err != nil {
		return nil, err
	}

	kind := detectRollbackKind(tokens)
	if kind == "" {
		return nil, nil
	}

	baseDir := filepath.Join(req.ProjectPath, ".slb", "rollback")
	if kind == rollbackKindGit {
		// git clean -x may remove .slb itself. Git recovery must live under
		// the selected repository's Git directory, not its working tree.
		baseDir, err = gitRollbackStorage(ctx, req, tokens)
		if err != nil {
			return nil, err
		}
	}
	_ = cleanupOldRollbackCaptures(baseDir, opts.Retention, opts.Now())

	if err := os.MkdirAll(baseDir, 0700); err != nil {
		return nil, fmt.Errorf("creating rollback base dir: %w", err)
	}
	// Each attempt owns a new directory. A concurrent executor or later retry
	// must never truncate an archive already referenced by a previous attempt.
	rollbackDir, err := os.MkdirTemp(baseDir, "req-"+sanitizeFilename(req.ID)+"-*")
	if err != nil {
		return nil, fmt.Errorf("creating rollback dir: %w", err)
	}

	data := &RollbackData{
		Version:      rollbackDataVersion,
		RequestID:    req.ID,
		CapturedAt:   opts.Now().UTC(),
		ProjectPath:  req.ProjectPath,
		CommandRaw:   req.Command.Raw,
		CommandCwd:   req.Command.Cwd,
		RollbackPath: rollbackDir,
		Kind:         kind,
	}

	switch kind {
	case rollbackKindFilesystem:
		fsData, err := captureFilesystemRollback(rollbackDir, req, tokens, opts)
		if err != nil {
			return nil, err
		}
		data.Filesystem = fsData
	case rollbackKindGit:
		gitData, err := captureGitRollback(ctx, rollbackDir, req, tokens, opts)
		if err != nil {
			return nil, err
		}
		data.Git = gitData
	case rollbackKindKubernetes:
		k8sData, err := captureKubernetesRollback(ctx, rollbackDir, req, tokens, opts)
		if err != nil {
			return nil, err
		}
		data.Kubernetes = k8sData
	default:
		return nil, nil
	}

	if err := writeRollbackMetadata(rollbackDir, data); err != nil {
		return nil, err
	}

	return data, nil
}

func LoadRollbackData(rollbackDir string) (*RollbackData, error) {
	if strings.TrimSpace(rollbackDir) == "" {
		return nil, fmt.Errorf("rollback dir is required")
	}
	b, err := os.ReadFile(filepath.Join(rollbackDir, rollbackMetadataFilename))
	if err != nil {
		return nil, fmt.Errorf("reading rollback metadata: %w", err)
	}
	var data RollbackData
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, fmt.Errorf("parsing rollback metadata: %w", err)
	}
	if data.RollbackPath == "" {
		data.RollbackPath = rollbackDir
	}
	return &data, nil
}

func RestoreRollbackState(ctx context.Context, data *RollbackData, opts RollbackRestoreOptions) error {
	if data == nil {
		return fmt.Errorf("rollback data is required")
	}
	if strings.TrimSpace(data.RollbackPath) == "" {
		return fmt.Errorf("rollback path is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	switch data.Kind {
	case rollbackKindFilesystem:
		return restoreFilesystemRollback(ctx, data, opts)
	case rollbackKindGit:
		return restoreGitRollback(ctx, data, opts)
	case rollbackKindKubernetes:
		return restoreKubernetesRollback(ctx, data, opts)
	default:
		return fmt.Errorf("unsupported rollback kind: %s", data.Kind)
	}
}

func normalizeRollbackCaptureOptions(opts RollbackCaptureOptions) RollbackCaptureOptions {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Retention == 0 {
		opts.Retention = defaultRollbackRetention
	}
	return opts
}

// rollbackCommandTokens follows the command runner's execution mode without
// evaluating shell code. A snapshot of only part of a compound/dynamic command
// would promise recovery for paths that were never captured.
func rollbackCommandTokens(spec db.CommandSpec) ([]string, error) {
	var tokens []string
	if !spec.Shell {
		if len(spec.Argv) > 0 {
			tokens = append([]string(nil), spec.Argv...)
		} else {
			// Match RunCommand's legacy raw-only non-shell execution exactly.
			tokens = strings.Fields(spec.Raw)
		}
	} else {
		if !hasLiteralPreviewSyntax(spec.Raw) {
			return nil, fmt.Errorf("rollback capture requires a single literal command; shell expansion or compound commands are unsupported")
		}
		parser := shellwords.NewParser()
		parser.ParseEnv, parser.ParseBacktick = false, false
		var err error
		tokens, err = parser.Parse(spec.Raw)
		if err != nil || parser.Position >= 0 {
			return nil, fmt.Errorf("rollback capture cannot parse the complete command")
		}
	}
	if len(tokens) == 0 || tokens[0] == "" {
		return nil, fmt.Errorf("empty command")
	}
	return tokens, nil
}

func detectRollbackKind(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}
	switch tokens[0] {
	case "rm":
		paths := rollbackRMTargets(tokens[1:])
		if len(paths) == 0 {
			return ""
		}
		return rollbackKindFilesystem
	case "git":
		return rollbackKindGit
	case "kubectl":
		selection, err := parseKubectlDelete(tokens)
		if selection != nil || err != nil {
			// An unsupported option may select another cluster or hide the
			// verb. Let capture return the error, never silently skip it.
			return rollbackKindKubernetes
		}
		return ""
	default:
		return ""
	}
}

func writeRollbackMetadata(dir string, data *RollbackData) error {
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal rollback metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, rollbackMetadataFilename), b, 0600); err != nil {
		return fmt.Errorf("writing rollback metadata: %w", err)
	}
	return nil
}

func cleanupOldRollbackCaptures(baseDir string, retention time.Duration, now time.Time) error {
	if retention <= 0 {
		return nil
	}
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	cutoff := now.Add(-retention)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !strings.HasPrefix(e.Name(), "req-") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.RemoveAll(filepath.Join(baseDir, e.Name()))
		}
	}
	return nil
}

func captureFilesystemRollback(rollbackDir string, req *db.Request, tokens []string, opts RollbackCaptureOptions) (*FilesystemRollbackData, error) {
	targets := rollbackRMTargets(tokens[1:])
	if len(targets) == 0 {
		return nil, fmt.Errorf("no rm targets found")
	}

	cwd := req.Command.Cwd
	if strings.TrimSpace(cwd) == "" {
		cwd = req.ProjectPath
	}

	paths, missing, err := resolveLiteralRollbackTargets(cwd, targets, rollbackDir)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no existing rm targets to capture")
	}

	totalBytes, err := estimateFileBytes(paths, opts.MaxSizeBytes)
	if err != nil {
		return nil, err
	}

	roots := make([]FilesystemRoot, 0, len(paths))
	for i, p := range paths {
		roots = append(roots, FilesystemRoot{
			ID:   fmt.Sprintf("p%d", i),
			Path: p,
		})
	}

	tarPath := filepath.Join(rollbackDir, rollbackFilesystemTarGz)
	if err := writeTarGz(tarPath, roots); err != nil {
		return nil, err
	}

	return &FilesystemRollbackData{
		TarGz:      rollbackFilesystemTarGz,
		Roots:      roots,
		TotalBytes: totalBytes,
		Missing:    missing,
	}, nil
}

// rollbackRMTargets preserves every operand after the first option terminator,
// including a literal second "--". The command's parsed argv is already literal.
func rollbackRMTargets(args []string) []string {
	var targets []string
	operands := false
	for _, arg := range args {
		if !operands && arg == "--" {
			operands = true
			continue
		}
		if !operands && strings.HasPrefix(arg, "-") {
			continue
		}
		targets = append(targets, arg)
	}
	return targets
}

// resolveLiteralRollbackTargets must not glob or trim argv: a file named
// "[draft]" or " notes " is distinct from the files a display parser might
// infer. Resolve parent symlinks before cleaning '..', but preserve a leaf
// symlink itself since rm removes the link, not its destination.
func resolveLiteralRollbackTargets(cwd string, targets []string, archiveDir string) ([]string, []string, error) {
	cwd, err := filepath.Abs(cwd)
	if err != nil {
		return nil, nil, err
	}
	archiveDir, err = filepath.EvalSymlinks(archiveDir)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving archive directory: %w", err)
	}
	archiveDir, err = filepath.Abs(archiveDir)
	if err != nil {
		return nil, nil, err
	}
	separators := string(os.PathSeparator)
	if os.PathSeparator == '\\' {
		separators += "/"
	}
	seen := make(map[string]bool)
	var paths, missing []string
	for _, target := range targets {
		if target == "" || strings.ContainsRune(target, 0) {
			return nil, nil, fmt.Errorf("invalid empty or NUL-containing rollback target")
		}
		candidate := target
		if !filepath.IsAbs(candidate) {
			// filepath.Join would erase link/.. before resolving the symlink.
			candidate = cwd + string(os.PathSeparator) + candidate
		}
		last := strings.LastIndexAny(candidate, separators)
		leaf := candidate[last+1:]
		var resolved string
		if leaf == "" || leaf == "." || leaf == ".." {
			resolved, err = filepath.EvalSymlinks(candidate)
		} else {
			var parent string
			parent, err = filepath.EvalSymlinks(candidate[:last+1])
			resolved = filepath.Join(parent, leaf)
		}
		if err != nil {
			if os.IsNotExist(err) {
				missing = append(missing, filepath.Clean(candidate))
				continue
			}
			return nil, nil, fmt.Errorf("resolving rollback target %q: %w", target, err)
		}
		info, err := os.Lstat(resolved)
		if os.IsNotExist(err) {
			missing = append(missing, resolved)
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("reading rollback target %q: %w", target, err)
		}
		if info.IsDir() {
			rel, err := filepath.Rel(resolved, archiveDir)
			if err != nil {
				return nil, nil, err
			}
			if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
				return nil, nil, fmt.Errorf("rollback target %q contains its recovery storage; refusing a self-containing backup", target)
			}
		}
		if !seen[resolved] {
			seen[resolved] = true
			paths = append(paths, resolved)
		}
	}
	return paths, missing, nil
}

func resolvePaths(cwd string, targets []string) ([]string, []string) {
	var paths []string
	var missing []string
	seen := make(map[string]struct{}, len(targets))

	for _, t := range targets {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}

		var candidates []string
		isGlob := strings.ContainsAny(t, "*?[]")

		if filepath.IsAbs(t) {
			if isGlob {
				matches, _ := filepath.Glob(t)
				if len(matches) > 0 {
					candidates = append(candidates, matches...)
				}
			}
			if len(candidates) == 0 {
				candidates = append(candidates, t)
			}
		} else {
			joined := filepath.Join(cwd, t)
			if isGlob {
				matches, _ := filepath.Glob(joined)
				if len(matches) > 0 {
					candidates = append(candidates, matches...)
				}
			}
			if len(candidates) == 0 {
				candidates = append(candidates, joined)
			}
		}

		for _, c := range candidates {
			clean := filepath.Clean(c)
			if _, ok := seen[clean]; ok {
				continue
			}
			seen[clean] = struct{}{}
			if _, err := os.Lstat(clean); err != nil {
				missing = append(missing, clean)
				continue
			}
			paths = append(paths, clean)
		}
	}

	return paths, missing
}

func estimateFileBytes(roots []string, maxBytes int64) (int64, error) {
	var total int64
	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := os.Lstat(p)
			if err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				total += info.Size()
				if maxBytes > 0 && total > maxBytes {
					return fmt.Errorf("rollback capture exceeds max size (%d bytes)", maxBytes)
				}
			}
			return nil
		})
		if err != nil {
			return 0, fmt.Errorf("estimating rollback size: %w", err)
		}
	}
	return total, nil
}

func writeTarGz(outPath string, roots []FilesystemRoot) (err error) {
	f, err := os.OpenFile(outPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("creating tar.gz: %w", err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	if err := writeRollbackArchive(f, roots); err != nil {
		return fmt.Errorf("writing rollback archive: %w", err)
	}
	// Do not publish metadata until the archive, including both trailers,
	// has been written successfully. Buffered close failures are data loss.
	return f.Sync()
}

func writeRollbackArchive(w io.Writer, roots []FilesystemRoot) error {
	gw := gzip.NewWriter(w)
	tw := tar.NewWriter(gw)
	var captureErr error
	for _, root := range roots {
		if captureErr = addRootToTar(tw, root.ID, root.Path); captureErr != nil {
			break
		}
	}
	tarErr := tw.Close()
	gzipErr := gw.Close()
	return errors.Join(captureErr, tarErr, gzipErr)
}

func addRootToTar(tw *tar.Writer, rootID, rootPath string) error {
	info, err := os.Lstat(rootPath)
	if err != nil {
		return fmt.Errorf("stat %s: %w", rootPath, err)
	}
	if info.IsDir() {
		return filepath.WalkDir(rootPath, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			fi, err := os.Lstat(p)
			if err != nil {
				return err
			}
			rel := ""
			if p != rootPath {
				rel, _ = filepath.Rel(rootPath, p)
			}
			name := rootID
			if rel != "" && rel != "." {
				name = filepath.ToSlash(filepath.Join(rootID, rel))
			}
			if fi.IsDir() && !strings.HasSuffix(name, "/") {
				name += "/"
			}
			return addPathToTar(tw, p, name, fi)
		})
	}

	name := rootID
	if info.IsDir() && !strings.HasSuffix(name, "/") {
		name += "/"
	}
	return addPathToTar(tw, rootPath, name, info)
}

func addPathToTar(tw *tar.Writer, fsPath, tarName string, info fs.FileInfo) error {
	mode := info.Mode()
	if !mode.IsRegular() && !mode.IsDir() && mode&os.ModeSymlink == 0 {
		return fmt.Errorf("rollback cannot restore special file: %s", fsPath)
	}

	var linkTarget string
	if mode&os.ModeSymlink != 0 {
		target, err := os.Readlink(fsPath)
		if err != nil {
			return fmt.Errorf("readlink %s: %w", fsPath, err)
		}
		linkTarget = target
	}

	hdr, err := tar.FileInfoHeader(info, linkTarget)
	if err != nil {
		return fmt.Errorf("tar header for %s: %w", fsPath, err)
	}
	hdr.Name = tarName

	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("write tar header: %w", err)
	}

	if mode.IsRegular() {
		f, err := os.Open(fsPath)
		if err != nil {
			return fmt.Errorf("open %s: %w", fsPath, err)
		}
		defer f.Close()

		// TOCTOU check: verify opened file matches lstat info
		stat, err := f.Stat()
		if err != nil {
			return fmt.Errorf("fstat %s: %w", fsPath, err)
		}
		if !os.SameFile(info, stat) {
			return fmt.Errorf("file changed during rollback capture (possible TOCTOU attack): %s", fsPath)
		}

		if _, err := io.Copy(tw, f); err != nil {
			return fmt.Errorf("write tar body: %w", err)
		}
	}

	return nil
}

func ensureNoSymlinkParents(rootPath, targetPath string) error {
	root := filepath.Clean(rootPath)
	target := filepath.Clean(targetPath)

	if strings.TrimSpace(root) == "" || strings.TrimSpace(target) == "" {
		return fmt.Errorf("invalid rollback restore path")
	}

	rel, err := filepath.Rel(root, target)
	if err != nil {
		return fmt.Errorf("resolving rollback restore path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("rollback restore path escapes root: %s", target)
	}

	if fi, err := os.Lstat(root); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to restore through symlink root: %s", root)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("lstat %s: %w", root, err)
	}

	if rel == "." {
		return nil
	}

	cur := root
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)

		fi, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("lstat %s: %w", cur, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to restore through symlink parent: %s", cur)
		}
	}

	return nil
}

const (
	maxFilesystemRollbackEntries       = 100000
	maxFilesystemRollbackMetadataBytes = 64 << 20
)

type filesystemRollbackEntry struct {
	kind    byte
	mode    os.FileMode
	size    int64
	modTime time.Time
	link    string
	digest  [sha256.Size]byte
}

type filesystemRollbackReader struct {
	ctx               context.Context
	reader            io.Reader
	metadataRemaining *int64
}

func (r filesystemRollbackReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.metadataRemaining != nil {
		if *r.metadataRemaining < 0 {
			return 0, fmt.Errorf("filesystem rollback metadata exceeds its size limit")
		}
		if int64(len(p)) > *r.metadataRemaining+1 {
			p = p[:*r.metadataRemaining+1]
		}
	}
	n, err := r.reader.Read(p)
	if r.metadataRemaining != nil {
		*r.metadataRemaining -= int64(n)
		if *r.metadataRemaining < 0 {
			return 0, fmt.Errorf("filesystem rollback metadata exceeds its size limit")
		}
	}
	return n, err
}

// Filesystem recovery validates all contents and destinations before changing
// live files. The recorded payload size bounds decoding without introducing a
// new payload cap for historical captures. Header/padding and entry limits bound
// the recovery plan itself. These checks do not provide atomic restoration or
// isolation from another process changing paths/files between checks and writes.
func restoreFilesystemRollback(ctx context.Context, data *RollbackData, opts RollbackRestoreOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if data.Filesystem == nil {
		return fmt.Errorf("filesystem rollback data missing")
	}
	if data.Filesystem.TotalBytes < 0 || data.Filesystem.TotalBytes > (1<<63-1)-maxFilesystemRollbackMetadataBytes-1 {
		return fmt.Errorf("invalid filesystem rollback payload size")
	}
	if len(data.Filesystem.Roots) == 0 || len(data.Filesystem.Roots) > maxFilesystemRollbackEntries {
		return fmt.Errorf("filesystem rollback roots missing or excessive")
	}
	rootMap := make(map[string]string, len(data.Filesystem.Roots))
	for _, r := range data.Filesystem.Roots {
		if r.ID == "" || r.ID == "." || r.ID == ".." || strings.ContainsAny(r.ID, "/\\\x00") ||
			!filepath.IsAbs(r.Path) || filepath.Clean(r.Path) != r.Path || strings.ContainsRune(r.Path, 0) {
			return fmt.Errorf("filesystem rollback roots missing or invalid")
		}
		if _, exists := rootMap[r.ID]; exists {
			return fmt.Errorf("duplicate filesystem rollback root id: %s", r.ID)
		}
		rootMap[r.ID] = r.Path
	}
	if data.Filesystem.TarGz == "" || !filepath.IsLocal(filepath.FromSlash(data.Filesystem.TarGz)) {
		return fmt.Errorf("invalid filesystem rollback archive path")
	}
	tarPath := filepath.Join(data.RollbackPath, data.Filesystem.TarGz)
	if err := ensureNoSymlinkParents(data.RollbackPath, tarPath); err != nil {
		return err
	}
	info, err := os.Lstat(tarPath)
	if err != nil {
		return fmt.Errorf("opening rollback tar.gz: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("filesystem rollback archive must be a regular file")
	}
	f, err := os.Open(tarPath)
	if err != nil {
		return fmt.Errorf("opening rollback tar.gz: %w", err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return fmt.Errorf("filesystem rollback archive changed while opening")
	}

	plan := make(map[string]filesystemRollbackEntry)
	err = walkFilesystemRollbackArchive(ctx, f, rootMap, data.Filesystem.TotalBytes, maxFilesystemRollbackMetadataBytes, func(hdr *tar.Header, target string, body io.Reader) error {
		entry := filesystemRollbackEntry{
			kind: hdr.Typeflag, mode: os.FileMode(hdr.Mode) & os.ModePerm,
			size: hdr.Size, modTime: hdr.ModTime, link: hdr.Linkname,
		}
		if entry.kind == tar.TypeRegA {
			entry.kind = tar.TypeReg
		}
		hash := sha256.New()
		if _, err := io.Copy(hash, body); err != nil {
			return fmt.Errorf("validating rollback payload: %w", err)
		}
		copy(entry.digest[:], hash.Sum(nil))
		if previous, exists := plan[target]; exists {
			// Overlapping rm operands can capture one destination under two
			// root IDs. Preserve those historical captures only when both
			// copies describe exactly the same restoration.
			if previous.kind != entry.kind || previous.mode != entry.mode || previous.size != entry.size ||
				!previous.modTime.Equal(entry.modTime) || previous.link != entry.link || previous.digest != entry.digest {
				return fmt.Errorf("conflicting filesystem rollback destination: %s", target)
			}
			return nil
		}
		plan[target] = entry
		return nil
	})
	if err != nil {
		return err
	}
	for target, entry := range plan {
		if err := validateFilesystemRollbackDestination(ctx, target, entry.kind, opts); err != nil {
			return err
		}
		for parent := filepath.Dir(target); ; parent = filepath.Dir(parent) {
			if ancestor, exists := plan[parent]; exists && ancestor.kind != tar.TypeDir {
				return fmt.Errorf("filesystem rollback entry has a file or symlink parent: %s", target)
			}
			if filepath.Dir(parent) == parent {
				break
			}
		}
	}
	current, err := f.Stat()
	if err != nil {
		return err
	}
	if current.Size() != opened.Size() || !current.ModTime().Equal(opened.ModTime()) {
		return fmt.Errorf("filesystem rollback archive changed during validation")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}

	restored := make(map[string]bool, len(plan))
	return walkFilesystemRollbackArchive(ctx, f, rootMap, data.Filesystem.TotalBytes, maxFilesystemRollbackMetadataBytes, func(hdr *tar.Header, target string, body io.Reader) error {
		if restored[target] {
			return nil
		}
		if err := validateFilesystemRollbackDestination(ctx, target, hdr.Typeflag, opts); err != nil {
			return err
		}
		mode := os.FileMode(hdr.Mode) & os.ModePerm
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, mode); err != nil {
				return fmt.Errorf("creating dir %s: %w", target, err)
			}
			restored[target] = true
			return nil
		}
		if info, err := os.Lstat(target); err == nil {
			if !opts.Force {
				return fmt.Errorf("path exists: %s (use --force to overwrite)", target)
			}
			if info.IsDir() {
				err = os.RemoveAll(target)
			} else {
				err = os.Remove(target)
			}
			if err != nil {
				return fmt.Errorf("removing %s: %w", target, err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("lstat %s: %w", target, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return fmt.Errorf("creating parent dir: %w", err)
		}
		if err := ensureFilesystemRollbackParents(ctx, target); err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeSymlink {
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("creating symlink %s: %w", target, err)
			}
		} else {
			file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return fmt.Errorf("creating file %s: %w", target, err)
			}
			_, copyErr := io.Copy(file, body)
			if err := errors.Join(copyErr, file.Close()); err != nil {
				return fmt.Errorf("writing file %s: %w", target, err)
			}
			_ = os.Chtimes(target, hdr.ModTime, hdr.ModTime)
		}
		restored[target] = true
		return nil
	})
}

// A captured root can itself be a file or a symlink. Its parent chain must be
// checked all the way to the filesystem root, not just below the captured root.
// The leaf is intentionally excluded so a captured symlink can be recreated.
func ensureFilesystemRollbackParents(ctx context.Context, target string) error {
	for parent := filepath.Dir(target); ; parent = filepath.Dir(parent) {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := os.Lstat(parent)
		if err == nil && !info.IsDir() {
			return fmt.Errorf("refusing filesystem rollback through a non-directory or symlink parent: %s", parent)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("checking filesystem rollback parent: %w", err)
		}
		if filepath.Dir(parent) == parent {
			return nil
		}
	}
}

func validateFilesystemRollbackDestination(ctx context.Context, target string, kind byte, opts RollbackRestoreOptions) error {
	if err := ensureFilesystemRollbackParents(ctx, target); err != nil {
		return err
	}
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking filesystem rollback destination: %w", err)
	}
	if kind == tar.TypeDir {
		if !info.IsDir() {
			return fmt.Errorf("filesystem rollback directory conflicts with an existing path: %s", target)
		}
	} else if !opts.Force {
		return fmt.Errorf("path exists: %s (use --force to overwrite)", target)
	}
	return nil
}

func walkFilesystemRollbackArchive(ctx context.Context, source io.Reader, roots map[string]string, totalBytes, metadataBytes int64, visit func(*tar.Header, string, io.Reader) error) error {
	if totalBytes < 0 || metadataBytes < 0 || totalBytes > (1<<63-1)-metadataBytes-1 {
		return fmt.Errorf("invalid filesystem rollback decoding limits")
	}
	gr, err := gzip.NewReader(filesystemRollbackReader{ctx: ctx, reader: source})
	if err != nil {
		return fmt.Errorf("opening gzip: %w", err)
	}
	defer gr.Close()
	decoded := &filesystemRollbackReader{ctx: ctx, reader: gr, metadataRemaining: &metadataBytes}
	limited := &io.LimitedReader{R: decoded, N: totalBytes + metadataBytes + 1}
	tr := tar.NewReader(limited)
	remaining := totalBytes
	names := make(map[string]bool)
	seenRoots := make(map[string]bool, len(roots))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar: %w", err)
		}

		clean := strings.TrimSuffix(strings.TrimPrefix(hdr.Name, "./"), "/")
		if clean == "" || clean == "." || path.Clean(clean) != clean || path.IsAbs(clean) ||
			strings.HasPrefix(clean, "../") || strings.ContainsAny(clean, "\\\x00") {
			return fmt.Errorf("invalid tar entry name: %q", hdr.Name)
		}
		if names[clean] {
			return fmt.Errorf("duplicate filesystem rollback archive entry: %s", clean)
		}
		names[clean] = true
		if len(names) > maxFilesystemRollbackEntries {
			return fmt.Errorf("filesystem rollback archive contains too many entries")
		}
		parts := strings.SplitN(clean, "/", 2)
		rootID := parts[0]
		rootPath, ok := roots[rootID]
		if !ok {
			return fmt.Errorf("unknown rollback root id: %s", rootID)
		}
		target := rootPath
		if len(parts) == 1 {
			seenRoots[rootID] = true
		} else {
			rel := filepath.FromSlash(parts[1])
			if !filepath.IsLocal(rel) || filepath.VolumeName(rel) != "" {
				return fmt.Errorf("invalid rollback relative path: %q", rel)
			}
			target = filepath.Join(rootPath, rel)
		}
		switch hdr.Typeflag {
		case tar.TypeDir, tar.TypeSymlink:
			if hdr.Size != 0 || strings.ContainsRune(hdr.Linkname, 0) || (hdr.Typeflag == tar.TypeSymlink && hdr.Linkname == "") {
				return fmt.Errorf("invalid filesystem rollback directory or symlink: %s", hdr.Name)
			}
		case tar.TypeReg, tar.TypeRegA:
			if hdr.Size < 0 || hdr.Size > remaining {
				return fmt.Errorf("filesystem rollback payload exceeds its recorded size")
			}
			remaining -= hdr.Size
		default:
			return fmt.Errorf("unsupported filesystem rollback entry type for %s", hdr.Name)
		}
		// Header parsing (including PAX headers hidden inside tar.Next) and
		// padding have an independent budget. A large payload allowance must
		// not subsidize arbitrarily large metadata or retained entry names.
		decoded.metadataRemaining = nil
		if err := visit(hdr, target, tr); err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return fmt.Errorf("reading filesystem rollback payload: %w", err)
		}
		decoded.metadataRemaining = &metadataBytes
	}
	// tar EOF does not establish gzip integrity: consume the trailer too.
	// Only ordinary zero padding may follow the tar end marker, not another
	// hidden archive or unbounded decompressed data.
	var padding [32 << 10]byte
	for {
		n, err := limited.Read(padding[:])
		if limited.N == 0 {
			return fmt.Errorf("filesystem rollback metadata or padding exceeds its size limit")
		}
		for _, b := range padding[:n] {
			if b != 0 {
				return fmt.Errorf("unexpected data after filesystem rollback archive")
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("validating filesystem rollback gzip trailer: %w", err)
		}
	}
	if remaining != 0 {
		return fmt.Errorf("filesystem rollback payload does not match its recorded size")
	}
	for id := range roots {
		if !seenRoots[id] {
			return fmt.Errorf("filesystem rollback archive is missing root %s", id)
		}
	}
	return nil
}

const (
	maxKubernetesObjectBytes = 16 << 20
	maxKubernetesBackupBytes = 100 << 20
	maxKubernetesObjects     = 256
)

type kubectlResource struct{ Kind, Name string }

type kubectlDeleteSelection struct {
	Context, Namespace, Kubeconfig string
	Resources                      []kubectlResource
}

// Parse the entire invocation. Ignoring an unknown flag can back up a different
// cluster, namespace or resource set than the reviewed delete will affect.
func parseKubectlDelete(tokens []string) (*kubectlDeleteSelection, error) {
	if len(tokens) == 0 || tokens[0] != "kubectl" {
		return nil, errors.New("kubectl invocation is required")
	}
	selection := &kubectlDeleteSelection{}
	seen := map[string]string{}
	var operands []string
	verb, literal := false, false
	for i := 1; i < len(tokens); i++ {
		arg := tokens[i]
		if arg == "--" && verb && !literal {
			literal = true
			continue
		}
		if strings.HasPrefix(arg, "-") && !literal {
			key, value, assigned := strings.Cut(arg, "=")
			if strings.HasPrefix(key, "-n") && key != "-n" && !strings.HasPrefix(key, "--") {
				value, key, assigned = strings.TrimPrefix(arg, "-n"), "-n", true
			}
			if key == "-n" {
				key = "--namespace"
			}
			valueFlag := key == "--context" || key == "--namespace" || key == "--kubeconfig" ||
				(verb && (key == "--grace-period" || key == "--timeout" || key == "--cascade"))
			boolFlag := verb && (key == "--force" || key == "--now" || key == "--wait" || key == "--ignore-not-found")
			if !valueFlag && !boolFlag {
				return nil, fmt.Errorf("rollback capture does not support kubectl option %q", key)
			}
			if valueFlag && !assigned {
				if i+1 >= len(tokens) {
					return nil, fmt.Errorf("kubectl option %s requires a value", key)
				}
				i++
				value = tokens[i]
			}
			if boolFlag {
				if assigned {
					if _, err := strconv.ParseBool(value); err != nil {
						return nil, fmt.Errorf("invalid boolean for kubectl option %s", key)
					}
				}
				continue
			}
			if value == "" || strings.ContainsAny(value, "\x00\r\n") {
				return nil, fmt.Errorf("invalid value for kubectl option %s", key)
			}
			if previous, exists := seen[key]; exists && previous != value {
				return nil, fmt.Errorf("conflicting kubectl option %s", key)
			}
			seen[key] = value
			switch key {
			case "--context":
				selection.Context = value
			case "--namespace":
				if !kubernetesIdentifier(value) {
					return nil, errors.New("invalid Kubernetes namespace")
				}
				selection.Namespace = value
			case "--kubeconfig":
				selection.Kubeconfig = value
			case "--grace-period":
				n, err := strconv.ParseInt(value, 10, 64)
				if err != nil || n < -1 {
					return nil, errors.New("invalid Kubernetes grace period")
				}
			case "--timeout":
				if d, err := time.ParseDuration(value); err != nil || d < 0 {
					return nil, errors.New("invalid Kubernetes delete timeout")
				}
			case "--cascade":
				if value != "background" && value != "foreground" && value != "orphan" {
					return nil, errors.New("unsupported Kubernetes cascade policy")
				}
			}
			continue
		}
		if !verb {
			if arg != "delete" {
				return nil, nil // An ordinary non-delete command needs no snapshot.
			}
			verb = true
			continue
		}
		operands = append(operands, arg)
	}
	if !verb || len(operands) == 0 {
		return nil, errors.New("explicit named kubectl delete targets are required")
	}
	if strings.Contains(operands[0], "/") {
		for _, operand := range operands {
			kind, name, found := strings.Cut(operand, "/")
			if !found || !kubernetesIdentifier(kind) || !kubernetesIdentifier(name) {
				return nil, errors.New("rollback requires complete kind/name resource targets")
			}
			selection.Resources = append(selection.Resources, kubectlResource{kind, name})
		}
	} else {
		if len(operands) < 2 || !kubernetesIdentifier(operands[0]) {
			return nil, errors.New("rollback requires a resource kind and explicit names")
		}
		for _, name := range operands[1:] {
			if !kubernetesIdentifier(name) {
				return nil, errors.New("rollback requires literal Kubernetes resource names")
			}
			selection.Resources = append(selection.Resources, kubectlResource{operands[0], name})
		}
	}
	unique := selection.Resources[:0]
	resourceSeen := map[kubectlResource]bool{}
	for _, resource := range selection.Resources {
		if !resourceSeen[resource] {
			unique = append(unique, resource)
			resourceSeen[resource] = true
		}
	}
	selection.Resources = unique
	if len(selection.Resources) > maxKubernetesObjects {
		return nil, errors.New("too many Kubernetes resources for one rollback capture")
	}
	return selection, nil
}

func kubernetesIdentifier(value string) bool {
	if value == "" || value[0] == '-' || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

func captureKubernetesRollback(ctx context.Context, rollbackDir string, req *db.Request, tokens []string, opts RollbackCaptureOptions) (*KubernetesRollbackData, error) {
	selection, err := parseKubectlDelete(tokens)
	if err != nil || selection == nil {
		return nil, errors.Join(errors.New("unsupported Kubernetes rollback capture"), err)
	}
	ctx, cancel := context.WithTimeout(ctx, defaultRollbackCmdTimeout)
	defer cancel()
	cwd := req.Command.Cwd
	if cwd == "" {
		cwd = req.ProjectPath
	}
	files, err := kubernetesConfigFiles(cwd, selection.Kubeconfig)
	if err != nil {
		return nil, err
	}
	data, err := readKubernetesTarget(ctx, cwd, files, selection.Context, selection.Namespace)
	if err != nil {
		return nil, err
	}
	outDir := filepath.Join(rollbackDir, rollbackKubernetesDirName)
	if err := os.Mkdir(outDir, 0700); err != nil {
		return nil, fmt.Errorf("creating Kubernetes rollback directory: %w", err)
	}
	remaining := int64(maxKubernetesBackupBytes)
	if opts.MaxSizeBytes > 0 && opts.MaxSizeBytes < remaining {
		remaining = opts.MaxSizeBytes
	}
	data.ManifestSHA256 = make(map[string]string)
	for i, resource := range selection.Resources {
		limit := remaining
		if limit > maxKubernetesObjectBytes {
			limit = maxKubernetesObjectBytes
		}
		out, err := runKubernetesCommand(ctx, cwd, data.ConfigFiles, nil, limit,
			kubernetesTargetArgs(data, "get", resource.Kind, resource.Name, "--output=json")...)
		if err != nil {
			return nil, fmt.Errorf("capturing Kubernetes resource %d: %w", i+1, err)
		}
		manifest, err := normalizeKubernetesManifest(out, data.Namespace, resource.Name)
		if err != nil {
			return nil, err
		}
		if int64(len(manifest)) > remaining || len(manifest) > maxKubernetesObjectBytes {
			return nil, errors.New("Kubernetes rollback capture exceeds max size")
		}
		remaining -= int64(len(manifest))
		// Indexes avoid collisions between aliases, API groups or names that
		// sanitize to the same filename. The object itself carries its identity.
		rel := filepath.ToSlash(filepath.Join(rollbackKubernetesDirName, fmt.Sprintf("%03d.json", i)))
		file, err := os.OpenFile(filepath.Join(rollbackDir, filepath.FromSlash(rel)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		_, writeErr := file.Write(manifest)
		if writeErr == nil {
			writeErr = file.Sync()
		}
		if err := errors.Join(writeErr, file.Close()); err != nil {
			return nil, fmt.Errorf("writing Kubernetes manifest: %w", err)
		}
		digest := sha256.Sum256(manifest)
		data.ManifestSHA256[rel] = hex.EncodeToString(digest[:])
		data.Manifests = append(data.Manifests, rel)
	}
	if err := verifyKubernetesConfigFiles(data.ConfigFiles); err != nil {
		return nil, err
	}
	return data, nil
}

func kubernetesConfigFiles(cwd, explicit string) ([]KubernetesConfigFile, error) {
	var paths []string
	switch {
	case explicit != "":
		paths = []string{explicit}
	case os.Getenv("KUBECONFIG") != "":
		paths = filepath.SplitList(os.Getenv("KUBECONFIG"))
	default:
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		paths = []string{filepath.Join(homeDir, ".kube", "config")}
	}
	// kuberc can inject command-specific defaults/aliases not represented in
	// argv. Refuse active preferences rather than backing up a guessed target.
	if !strings.EqualFold(os.Getenv("KUBECTL_KUBERC"), "false") {
		if homeDir, err := os.UserHomeDir(); err != nil {
			return nil, err
		} else if _, err := os.Stat(filepath.Join(homeDir, ".kube", "kuberc")); err == nil || !os.IsNotExist(err) {
			return nil, errors.New("Kubernetes rollback requires kuberc preferences to be disabled")
		}
	}
	seen := map[string]bool{}
	var files []KubernetesConfigFile
	for _, name := range paths {
		if name == "" {
			continue // Match KUBECONFIG's empty-entry behavior.
		}
		if !filepath.IsAbs(name) {
			name = cwd + string(os.PathSeparator) + name
		}
		if !filepath.IsAbs(name) || strings.ContainsRune(name, 0) || seen[name] {
			if seen[name] {
				continue
			}
			return nil, errors.New("an absolute kubeconfig path is required")
		}
		seen[name] = true
		contents, err := readKubernetesFile(name, maxKubernetesObjectBytes)
		if err != nil {
			return nil, fmt.Errorf("reading kubeconfig fingerprint: %w", err)
		}
		digest := sha256.Sum256(contents)
		files = append(files, KubernetesConfigFile{Path: name, SHA256: hex.EncodeToString(digest[:])})
		if len(files) > 32 {
			return nil, errors.New("too many kubeconfig files for rollback capture")
		}
	}
	if len(files) == 0 {
		return nil, errors.New("Kubernetes rollback requires an explicit file-backed configuration")
	}
	return files, nil
}

func verifyKubernetesConfigFiles(files []KubernetesConfigFile) error {
	if len(files) == 0 || len(files) > 32 {
		return errors.New("Kubernetes rollback has no bound kubeconfig; recapture before execution")
	}
	for _, file := range files {
		if !filepath.IsAbs(file.Path) || len(file.SHA256) != sha256.Size*2 {
			return errors.New("invalid Kubernetes configuration fingerprint")
		}
		if len(files) > 1 && strings.ContainsRune(file.Path, os.PathListSeparator) {
			return errors.New("a merged kubeconfig path contains the path-list separator")
		}
		contents, err := readKubernetesFile(file.Path, maxKubernetesObjectBytes)
		if err != nil {
			return fmt.Errorf("reading captured kubeconfig: %w", err)
		}
		digest := sha256.Sum256(contents)
		if hex.EncodeToString(digest[:]) != file.SHA256 {
			return errors.New("kubeconfig changed since capture; reconcile the target before restoring")
		}
	}
	return nil
}

func readKubernetesTarget(ctx context.Context, cwd string, files []KubernetesConfigFile, selected, namespace string) (*KubernetesRollbackData, error) {
	if err := verifyKubernetesConfigFiles(files); err != nil {
		return nil, err
	}
	args := []string{"config", "view", "--minify", "--output=json"}
	if selected != "" {
		args = append(args, "--context="+selected)
	}
	out, err := runKubernetesCommand(ctx, cwd, files, nil, maxCapturedOutputBytes, args...)
	if err != nil {
		return nil, fmt.Errorf("resolving Kubernetes context: %w", err)
	}
	var cfg struct {
		Current  string `json:"current-context"`
		Contexts []struct {
			Name    string                              `json:"name"`
			Context struct{ Cluster, Namespace string } `json:"context"`
		} `json:"contexts"`
		Clusters []struct {
			Name    string                  `json:"name"`
			Cluster struct{ Server string } `json:"cluster"`
		} `json:"clusters"`
	}
	if json.Unmarshal(out, &cfg) != nil || len(cfg.Contexts) != 1 || len(cfg.Clusters) != 1 ||
		cfg.Current == "" || cfg.Contexts[0].Name != cfg.Current || cfg.Contexts[0].Context.Cluster != cfg.Clusters[0].Name ||
		(selected != "" && cfg.Current != selected) {
		return nil, errors.New("kubectl did not resolve one unambiguous context and cluster")
	}
	if namespace == "" {
		namespace = cfg.Contexts[0].Context.Namespace
		if namespace == "" {
			namespace = "default"
		}
	}
	server, err := url.Parse(cfg.Clusters[0].Cluster.Server)
	if err != nil || (server.Scheme != "http" && server.Scheme != "https") || server.Host == "" ||
		server.User != nil || server.RawQuery != "" || server.Fragment != "" || !kubernetesIdentifier(namespace) {
		return nil, errors.New("unsupported Kubernetes target address or namespace")
	}
	return &KubernetesRollbackData{Context: cfg.Current, Namespace: namespace, Server: server.String(), ConfigFiles: files}, nil
}

func kubernetesTargetArgs(target *KubernetesRollbackData, args ...string) []string {
	prefix := []string{"--context=" + target.Context, "--namespace=" + target.Namespace, "--server=" + target.Server}
	return append(prefix, args...)
}

func runKubernetesCommand(ctx context.Context, cwd string, files []KubernetesConfigFile, input []byte, limit int64, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > maxKubernetesObjectBytes {
		return nil, errors.New("Kubernetes rollback capture exceeds max size")
	}
	var paths []string
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	var env []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if !strings.EqualFold(key, "KUBECONFIG") && !strings.EqualFold(key, "KUBECTL_KUBERC") && !strings.EqualFold(key, "KUBERNETES_MASTER") {
			env = append(env, item)
		}
	}
	env = append(env, "KUBECONFIG="+strings.Join(paths, string(os.PathListSeparator)), "KUBECTL_KUBERC=false")
	if len(paths) == 1 {
		// A filename such as config:prod is valid on POSIX. It is one
		// --kubeconfig file, not two entries in the KUBECONFIG merge list.
		args = append([]string{"--kubeconfig=" + paths[0]}, args...)
	}
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Dir, cmd.Env, cmd.Stdin = cwd, env, bytes.NewReader(input)
	stdout := &outputCapture{limit: int(limit)}
	cmd.Stdout, cmd.Stderr, cmd.WaitDelay = stdout, io.Discard, time.Second
	stopGroup := configureCommandCancellation(cmd, false)
	err := cmd.Run()
	if err != nil {
		if stopGroup != nil {
			if stopErr := stopGroup(); stopErr != nil && !errors.Is(stopErr, os.ErrProcessDone) {
				err = errors.Join(err, stopErr)
			}
		}
		// kubectl/credential plugins may echo credentials in diagnostics. Do
		// not persist or return their output, command line or kubeconfig bytes.
		return nil, fmt.Errorf("kubectl process failed: %w", errors.Join(ctx.Err(), err))
	}
	if stdout.omitted > 0 {
		return nil, errors.New("Kubernetes rollback output exceeds max size")
	}
	return []byte(stdout.String()), nil
}

func readKubernetesFile(name string, limit int64) ([]byte, error) {
	info, err := os.Stat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("Kubernetes recovery requires a bounded regular file")
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	opened, statErr := file.Stat()
	if statErr != nil || !opened.Mode().IsRegular() || opened.Size() > limit || !os.SameFile(info, opened) {
		return nil, errors.Join(errors.New("Kubernetes recovery requires a bounded regular file"), statErr, file.Close())
	}
	contents, readErr := io.ReadAll(io.LimitReader(file, limit+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return nil, err
	}
	if int64(len(contents)) > limit {
		return nil, errors.New("Kubernetes recovery file exceeds max size")
	}
	return contents, nil
}

func normalizeKubernetesManifest(contents []byte, namespace, expectedName string) ([]byte, error) {
	var resource map[string]any
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.UseNumber() // CRDs can contain int64 values beyond float64 precision.
	if decoder.Decode(&resource) != nil {
		return nil, errors.New("Kubernetes recovery requires one complete JSON object")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, errors.New("Kubernetes recovery requires one complete JSON object")
	}
	apiVersion, _ := resource["apiVersion"].(string)
	kind, _ := resource["kind"].(string)
	metadata, _ := resource["metadata"].(map[string]any)
	name, _ := metadata["name"].(string)
	objectNamespace, _ := metadata["namespace"].(string)
	if apiVersion == "" || !kubernetesIdentifier(kind) || strings.HasSuffix(kind, "List") || !kubernetesIdentifier(name) ||
		(expectedName != "" && name != expectedName) || (objectNamespace != "" && objectNamespace != namespace) {
		return nil, errors.New("Kubernetes object identity differs from the captured target")
	}
	delete(resource, "status")
	for _, key := range []string{"uid", "resourceVersion", "generation", "creationTimestamp", "deletionTimestamp", "deletionGracePeriodSeconds", "managedFields", "selfLink", "generateName"} {
		delete(metadata, key)
	}
	if annotations, ok := metadata["annotations"].(map[string]any); ok {
		delete(annotations, "kubectl.kubernetes.io/last-applied-configuration")
	}
	// Compact encoding avoids quadratic indentation growth for deeply nested
	// custom resources; the serialized object is still checked against budget.
	return json.Marshal(resource)
}

func restoreKubernetesRollback(ctx context.Context, data *RollbackData, opts RollbackRestoreOptions) error {
	target := data.Kubernetes
	if target == nil || target.Context == "" || target.Server == "" || target.Namespace == "" ||
		len(target.Manifests) == 0 || len(target.Manifests) > maxKubernetesObjects {
		return errors.New("Kubernetes snapshot lacks a bound target; recapture before execution")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*DefaultExecutionTimeout)
	defer cancel()
	cwd := data.CommandCwd
	if cwd == "" {
		cwd = data.ProjectPath
	}
	current, err := readKubernetesTarget(ctx, cwd, target.ConfigFiles, target.Context, target.Namespace)
	if err != nil {
		return err
	}
	if current.Server != target.Server || current.Context != target.Context || current.Namespace != target.Namespace {
		return errors.New("Kubernetes target changed since capture")
	}
	// Read and validate the ENTIRE snapshot before the first remote write.
	// Passing these verified bytes on stdin also avoids a later path reread.
	var manifests [][]byte
	seen := map[string]bool{}
	remaining := int64(maxKubernetesBackupBytes)
	for _, rel := range target.Manifests {
		if err := ctx.Err(); err != nil {
			return err
		}
		local := filepath.FromSlash(rel)
		if !filepath.IsLocal(local) || strings.Contains(rel, "\\") || seen[rel] {
			return errors.New("invalid or duplicate Kubernetes manifest path")
		}
		seen[rel] = true
		full := filepath.Join(data.RollbackPath, local)
		if err := ensureNoSymlinkParents(data.RollbackPath, full); err != nil {
			return err
		}
		limit := remaining
		if limit > maxKubernetesObjectBytes {
			limit = maxKubernetesObjectBytes
		}
		contents, err := readKubernetesFile(full, limit)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(contents)
		if hex.EncodeToString(digest[:]) != target.ManifestSHA256[rel] {
			return errors.New("Kubernetes manifest integrity check failed")
		}
		manifest, err := normalizeKubernetesManifest(contents, target.Namespace, "")
		if err != nil {
			return err
		}
		remaining -= int64(len(manifest))
		if remaining < 0 {
			return errors.New("Kubernetes recovery snapshot exceeds max size")
		}
		manifests = append(manifests, manifest)
	}
	verb := "create"
	if opts.Force {
		verb = "apply"
	}
	for i, manifest := range manifests {
		if err := verifyKubernetesConfigFiles(target.ConfigFiles); err != nil {
			return err
		}
		if _, err := runKubernetesCommand(ctx, cwd, target.ConfigFiles, manifest, maxCapturedOutputBytes,
			kubernetesTargetArgs(target, verb, "-f", "-")...); err != nil {
			return fmt.Errorf("restoring Kubernetes manifest %d using %s (overwriting requires --force): %w", i+1, verb, err)
		}
	}
	return nil
}

func runCmdString(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = os.Environ()
	if strings.TrimSpace(dir) != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func sanitizeFilename(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, string(os.PathSeparator), "_")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, " ", "_")
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

func bytesTrimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}
