// Package core implements rollback state capture and restoration.
package core

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
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
	// MaxSizeBytes limits filesystem capture. 0 disables the limit.
	MaxSizeBytes int64
	// Retention controls cleanup of old rollback captures. 0 uses the default.
	Retention time.Duration
	// Now overrides time.Now for tests.
	Now func() time.Time
}

type RollbackRestoreOptions struct {
	// Force allows overwriting existing files and running destructive git restores.
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

type GitRollbackData struct {
	RepoRoot      string `json:"repo_root"`
	Head          string `json:"head"`
	Branch        string `json:"branch"`
	StatusFile    string `json:"status_file"`
	DiffFile      string `json:"diff_file"`
	CachedFile    string `json:"cached_file"`
	UntrackedFile string `json:"untracked_file"`
}

type KubernetesRollbackData struct {
	Namespace string   `json:"namespace,omitempty"`
	Manifests []string `json:"manifests"`
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

	opts = normalizeRollbackCaptureOptions(opts)

	normalized := NormalizeCommand(req.Command.Raw)
	cmd := strings.TrimSpace(normalized.Primary)
	if cmd == "" {
		cmd = strings.TrimSpace(req.Command.Raw)
	}
	tokens := parseShellTokens(cmd)
	if len(tokens) == 0 {
		return nil, fmt.Errorf("empty command")
	}

	kind := detectRollbackKind(tokens)
	if kind == "" {
		return nil, nil
	}

	baseDir := filepath.Join(req.ProjectPath, ".slb", "rollback")
	_ = cleanupOldRollbackCaptures(baseDir, opts.Retention, opts.Now())

	rollbackDir := filepath.Join(baseDir, "req-"+req.ID)
	if err := os.MkdirAll(rollbackDir, 0700); err != nil {
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
		gitData, err := captureGitRollback(ctx, rollbackDir, req, tokens)
		if err != nil {
			return nil, err
		}
		data.Git = gitData
	case rollbackKindKubernetes:
		k8sData, err := captureKubernetesRollback(ctx, rollbackDir, req, tokens)
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
		return restoreFilesystemRollback(data, opts)
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

func detectRollbackKind(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}
	switch tokens[0] {
	case "rm":
		paths := rmTargets(tokens[1:])
		if len(paths) == 0 {
			return ""
		}
		return rollbackKindFilesystem
	case "git":
		return rollbackKindGit
	case "kubectl":
		if len(tokens) >= 2 && tokens[1] == "delete" {
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
	targets := rmTargets(tokens[1:])
	if len(targets) == 0 {
		return nil, fmt.Errorf("no rm targets found")
	}

	cwd := req.Command.Cwd
	if strings.TrimSpace(cwd) == "" {
		cwd = req.ProjectPath
	}

	paths, missing := resolvePaths(cwd, targets)
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

func writeTarGz(outPath string, roots []FilesystemRoot) error {
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("creating tar.gz: %w", err)
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	for _, root := range roots {
		if err := addRootToTar(tw, root.ID, root.Path); err != nil {
			return err
		}
	}
	return nil
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

func restoreFilesystemRollback(data *RollbackData, opts RollbackRestoreOptions) error {
	if data.Filesystem == nil {
		return fmt.Errorf("filesystem rollback data missing")
	}
	rootMap := make(map[string]string, len(data.Filesystem.Roots))
	for _, r := range data.Filesystem.Roots {
		if r.ID != "" && r.Path != "" {
			rootMap[r.ID] = r.Path
		}
	}
	if len(rootMap) == 0 {
		return fmt.Errorf("filesystem rollback roots missing")
	}

	tarPath := filepath.Join(data.RollbackPath, data.Filesystem.TarGz)
	f, err := os.Open(tarPath)
	if err != nil {
		return fmt.Errorf("opening rollback tar.gz: %w", err)
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("opening gzip: %w", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar: %w", err)
		}

		clean := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if clean == "." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "\\") {
			return fmt.Errorf("invalid tar entry name: %q", hdr.Name)
		}
		parts := strings.Split(strings.TrimSuffix(clean, "/"), "/")
		if len(parts) == 0 || parts[0] == "" {
			continue
		}
		rootID := parts[0]
		rootPath, ok := rootMap[rootID]
		if !ok {
			return fmt.Errorf("unknown rollback root id: %s", rootID)
		}

		rel := strings.Join(parts[1:], "/")
		if strings.HasPrefix(rel, "../") || rel == ".." {
			return fmt.Errorf("invalid rollback relative path: %q", rel)
		}
		relOS := filepath.FromSlash(rel)
		if filepath.IsAbs(relOS) || filepath.VolumeName(relOS) != "" {
			return fmt.Errorf("invalid rollback relative path: %q", rel)
		}

		target := rootPath
		if rel != "" && rel != "." {
			target = filepath.Join(rootPath, relOS)
		}

		mode := os.FileMode(hdr.Mode) & os.ModePerm

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := ensureNoSymlinkParents(rootPath, target); err != nil {
				return err
			}
			if err := os.MkdirAll(target, mode); err != nil {
				return fmt.Errorf("creating dir %s: %w", target, err)
			}
		case tar.TypeSymlink:
			parent := filepath.Dir(target)
			if filepath.Clean(target) != filepath.Clean(rootPath) {
				if err := ensureNoSymlinkParents(rootPath, parent); err != nil {
					return err
				}
			}
			if info, err := os.Lstat(target); err == nil {
				if !opts.Force {
					return fmt.Errorf("path exists: %s (use --force to overwrite)", target)
				}
				if info.IsDir() {
					if err := os.RemoveAll(target); err != nil {
						return fmt.Errorf("removing %s: %w", target, err)
					}
				} else {
					if err := os.Remove(target); err != nil {
						return fmt.Errorf("removing %s: %w", target, err)
					}
				}
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("lstat %s: %w", target, err)
			}
			if err := os.MkdirAll(parent, 0755); err != nil {
				return fmt.Errorf("creating parent dir: %w", err)
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("creating symlink %s: %w", target, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			parent := filepath.Dir(target)
			if filepath.Clean(target) != filepath.Clean(rootPath) {
				if err := ensureNoSymlinkParents(rootPath, parent); err != nil {
					return err
				}
			}
			if info, err := os.Lstat(target); err == nil {
				if !opts.Force {
					return fmt.Errorf("path exists: %s (use --force to overwrite)", target)
				}
				if info.IsDir() {
					if err := os.RemoveAll(target); err != nil {
						return fmt.Errorf("removing %s: %w", target, err)
					}
				} else {
					if err := os.Remove(target); err != nil {
						return fmt.Errorf("removing %s: %w", target, err)
					}
				}
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("lstat %s: %w", target, err)
			}
			if err := os.MkdirAll(parent, 0755); err != nil {
				return fmt.Errorf("creating parent dir: %w", err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return fmt.Errorf("creating file %s: %w", target, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return fmt.Errorf("writing file %s: %w", target, err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("closing file %s: %w", target, err)
			}
			modTime := hdr.ModTime
			_ = os.Chtimes(target, modTime, modTime)
		default:
			// Skip unsupported types.
		}
	}

	return nil
}

func captureGitRollback(ctx context.Context, rollbackDir string, req *db.Request, tokens []string) (*GitRollbackData, error) {
	captureCtx, cancel := context.WithTimeout(ctx, defaultRollbackCmdTimeout)
	defer cancel()

	cwd := req.Command.Cwd
	if strings.TrimSpace(cwd) == "" {
		cwd = req.ProjectPath
	}

	repoRoot, err := runCmdString(captureCtx, cwd, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("git repo detection failed: %w", err)
	}
	repoRoot = strings.TrimSpace(repoRoot)

	head, err := runCmdString(captureCtx, repoRoot, "git", "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("git head: %w", err)
	}
	branch, _ := runCmdString(captureCtx, repoRoot, "git", "rev-parse", "--abbrev-ref", "HEAD")

	status, _ := runCmdString(captureCtx, repoRoot, "git", "status", "--porcelain=v1")
	diff, _ := runCmdString(captureCtx, repoRoot, "git", "diff")
	cached, _ := runCmdString(captureCtx, repoRoot, "git", "diff", "--cached")
	untracked, _ := runCmdString(captureCtx, repoRoot, "git", "ls-files", "--others", "--exclude-standard")

	gitDir := filepath.Join(rollbackDir, rollbackGitDirName)
	if err := os.MkdirAll(gitDir, 0700); err != nil {
		return nil, fmt.Errorf("creating git rollback dir: %w", err)
	}

	if err := os.WriteFile(filepath.Join(gitDir, rollbackGitHeadFilename), []byte(strings.TrimSpace(head)+"\n"), 0600); err != nil {
		return nil, fmt.Errorf("writing git head: %w", err)
	}
	_ = os.WriteFile(filepath.Join(gitDir, rollbackGitBranchFilename), []byte(strings.TrimSpace(branch)+"\n"), 0600)
	_ = os.WriteFile(filepath.Join(gitDir, rollbackGitStatusFilename), []byte(status), 0600)
	_ = os.WriteFile(filepath.Join(gitDir, rollbackGitDiffFilename), []byte(diff), 0600)
	_ = os.WriteFile(filepath.Join(gitDir, rollbackGitCachedFilename), []byte(cached), 0600)
	_ = os.WriteFile(filepath.Join(gitDir, rollbackGitUntrackedFilename), []byte(untracked), 0600)

	return &GitRollbackData{
		RepoRoot:      repoRoot,
		Head:          strings.TrimSpace(head),
		Branch:        strings.TrimSpace(branch),
		StatusFile:    filepath.ToSlash(filepath.Join(rollbackGitDirName, rollbackGitStatusFilename)),
		DiffFile:      filepath.ToSlash(filepath.Join(rollbackGitDirName, rollbackGitDiffFilename)),
		CachedFile:    filepath.ToSlash(filepath.Join(rollbackGitDirName, rollbackGitCachedFilename)),
		UntrackedFile: filepath.ToSlash(filepath.Join(rollbackGitDirName, rollbackGitUntrackedFilename)),
	}, nil
}

func restoreGitRollback(ctx context.Context, data *RollbackData, opts RollbackRestoreOptions) error {
	if data.Git == nil {
		return fmt.Errorf("git rollback data missing")
	}
	if !opts.Force {
		return fmt.Errorf("git rollback is destructive (use --force)")
	}
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git not found in PATH")
	}

	restoreCtx, cancel := context.WithTimeout(ctx, 2*DefaultExecutionTimeout)
	defer cancel()

	repoRoot := data.Git.RepoRoot
	if strings.TrimSpace(repoRoot) == "" {
		return fmt.Errorf("git repo root missing")
	}

	// Try to return to the original branch if it existed.
	if b := strings.TrimSpace(data.Git.Branch); b != "" && b != "HEAD" {
		_, _ = runCmdString(restoreCtx, repoRoot, "git", "checkout", b)
	}

	if _, err := runCmdString(restoreCtx, repoRoot, "git", "reset", "--hard", data.Git.Head); err != nil {
		return fmt.Errorf("git reset --hard: %w", err)
	}

	// Re-apply captured diffs (best-effort).
	if err := applyGitPatchIfPresent(restoreCtx, repoRoot, filepath.Join(data.RollbackPath, filepath.FromSlash(data.Git.CachedFile)), true); err != nil {
		return err
	}
	if err := applyGitPatchIfPresent(restoreCtx, repoRoot, filepath.Join(data.RollbackPath, filepath.FromSlash(data.Git.DiffFile)), false); err != nil {
		return err
	}

	return nil
}

func applyGitPatchIfPresent(ctx context.Context, repoRoot, patchPath string, cached bool) error {
	b, err := os.ReadFile(patchPath)
	if err != nil {
		return nil
	}
	if len(bytesTrimSpace(b)) == 0 {
		return nil
	}
	args := []string{"apply"}
	if cached {
		args = append(args, "--cached")
	}
	args = append(args, patchPath)
	if _, err := runCmdString(ctx, repoRoot, "git", args...); err != nil {
		return fmt.Errorf("git apply (%s): %w", filepath.Base(patchPath), err)
	}
	return nil
}

func captureKubernetesRollback(ctx context.Context, rollbackDir string, req *db.Request, tokens []string) (*KubernetesRollbackData, error) {
	if len(tokens) < 2 || tokens[1] != "delete" {
		return nil, fmt.Errorf("unsupported kubectl command")
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		return nil, fmt.Errorf("kubectl not found in PATH")
	}

	captureCtx, cancel := context.WithTimeout(ctx, defaultRollbackCmdTimeout)
	defer cancel()

	cwd := req.Command.Cwd
	if strings.TrimSpace(cwd) == "" {
		cwd = req.ProjectPath
	}

	ns, resources := parseKubectlDelete(tokens[2:])
	if len(resources) == 0 {
		return nil, fmt.Errorf("no kubectl delete targets found")
	}

	outDir := filepath.Join(rollbackDir, rollbackKubernetesDirName)
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return nil, fmt.Errorf("creating k8s rollback dir: %w", err)
	}

	var manifests []string
	for _, r := range resources {
		filename := fmt.Sprintf("%s_%s.yaml", sanitizeFilename(r.Kind), sanitizeFilename(r.Name))
		fullPath := filepath.Join(outDir, filename)

		args := []string{"get", r.Kind, r.Name}
		if ns != "" {
			args = append(args, "-n", ns)
		}
		args = append(args, "-o", "yaml")

		out, err := runCmdString(captureCtx, cwd, "kubectl", args...)
		if err != nil {
			return nil, fmt.Errorf("kubectl get %s/%s: %w", r.Kind, r.Name, err)
		}
		if err := os.WriteFile(fullPath, []byte(out), 0600); err != nil {
			return nil, fmt.Errorf("writing manifest: %w", err)
		}
		manifests = append(manifests, filepath.ToSlash(filepath.Join(rollbackKubernetesDirName, filename)))
	}

	return &KubernetesRollbackData{
		Namespace: ns,
		Manifests: manifests,
	}, nil
}

type kubectlResource struct {
	Kind string
	Name string
}

func parseKubectlDelete(args []string) (string, []kubectlResource) {
	var ns string
	var out []kubectlResource

	// Extract namespace flag.
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-n" || a == "--namespace" {
			if i+1 < len(args) {
				ns = args[i+1]
				i++
				continue
			}
		}
		if strings.HasPrefix(a, "--namespace=") {
			ns = strings.TrimPrefix(a, "--namespace=")
			continue
		}
		rest = append(rest, a)
	}

	// Parse resources: either kind/name or kind name1 name2.
	i := 0
	for i < len(rest) {
		tok := rest[i]
		if tok == "--" {
			i++
			continue
		}
		if strings.HasPrefix(tok, "-") {
			// Stop at flags; capturing selectors is out of scope.
			break
		}
		if strings.Contains(tok, "/") {
			parts := strings.SplitN(tok, "/", 2)
			if parts[0] != "" && parts[1] != "" {
				out = append(out, kubectlResource{Kind: parts[0], Name: parts[1]})
			}
			i++
			continue
		}

		kind := tok
		i++
		for i < len(rest) && !strings.HasPrefix(rest[i], "-") && rest[i] != "--" && !strings.Contains(rest[i], "/") {
			out = append(out, kubectlResource{Kind: kind, Name: rest[i]})
			i++
		}
		if len(out) == 0 || out[len(out)-1].Kind != kind {
			// No names captured for this kind; stop.
			break
		}
	}

	return ns, out
}

func restoreKubernetesRollback(ctx context.Context, data *RollbackData, _ RollbackRestoreOptions) error {
	if data.Kubernetes == nil {
		return fmt.Errorf("kubernetes rollback data missing")
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		return fmt.Errorf("kubectl not found in PATH")
	}

	restoreCtx, cancel := context.WithTimeout(ctx, 2*DefaultExecutionTimeout)
	defer cancel()

	cwd := data.CommandCwd
	if strings.TrimSpace(cwd) == "" {
		cwd = data.ProjectPath
	}

	for _, rel := range data.Kubernetes.Manifests {
		full := filepath.Join(data.RollbackPath, filepath.FromSlash(rel))
		args := []string{"apply", "-f", full}
		if _, err := runCmdString(restoreCtx, cwd, "kubectl", args...); err != nil {
			return fmt.Errorf("kubectl apply %s: %w", rel, err)
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
