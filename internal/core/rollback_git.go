package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

// GitRollbackData describes a restorable HEAD -> index -> worktree snapshot.
// Patches are binary-capable and integrity checked before any destructive step.
// Capture requires a quiescent repository; it is not a filesystem transaction.
type GitRollbackData struct {
	RepoRoot         string                         `json:"repo_root"`
	GitDir           string                         `json:"git_dir,omitempty"`
	Head             string                         `json:"head"`
	Branch           string                         `json:"branch"`
	StatusFile       string                         `json:"status_file"`
	DiffFile         string                         `json:"diff_file"`
	CachedFile       string                         `json:"cached_file"`
	UntrackedFile    string                         `json:"untracked_file"`
	UntrackedArchive string                         `json:"untracked_archive,omitempty"`
	Artifacts        map[string]GitRollbackArtifact `json:"artifacts,omitempty"`
}

type GitRollbackArtifact struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// rollbackBudgetWriter fails rather than silently truncating recovery data.
// The budget is shared across the sequential artifacts of a capture attempt.
type rollbackBudgetWriter struct {
	writer    io.Writer
	remaining int64
	limited   bool
}

func (w *rollbackBudgetWriter) Write(p []byte) (int, error) {
	if w.limited && int64(len(p)) > w.remaining {
		return 0, fmt.Errorf("Git rollback capture exceeds max size")
	}
	n, err := w.writer.Write(p)
	if w.limited {
		w.remaining -= int64(n)
	}
	return n, err
}

func captureGitRollback(ctx context.Context, rollbackDir string, req *db.Request, tokens []string, opts RollbackCaptureOptions) (*GitRollbackData, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultRollbackCmdTimeout)
	defer cancel()
	if err := validateGitRollbackEnvironment(); err != nil {
		return nil, err
	}
	cwd := req.Command.Cwd
	if cwd == "" {
		cwd = req.ProjectPath
	}
	// Honor repeated -C options, but never guess at an unrecognized global
	// option that could select another repository or index.
	prefix, err := gitRollbackPrefix(tokens)
	if err != nil {
		return nil, err
	}
	root, err := gitSnapshotOutput(ctx, cwd, append(prefix, "rev-parse", "--show-toplevel")...)
	if err != nil {
		return nil, fmt.Errorf("git repo detection failed: %w", err)
	}
	root = strings.TrimSuffix(root, "\n")
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	gitDir, err := gitSnapshotOutput(ctx, root, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	gitDir = strings.TrimSuffix(gitDir, "\n")
	if unmerged, err := gitSnapshotOutput(ctx, root, "ls-files", "--unmerged", "-z"); err != nil || unmerged != "" {
		return nil, fmt.Errorf("cannot snapshot an unmerged Git index: %w", errors.Join(err, errors.New("resolve conflicts before capture")))
	}
	state, err := gitSnapshotState(ctx, root)
	if err != nil {
		return nil, err
	}
	gitPath := filepath.Join(rollbackDir, rollbackGitDirName)
	if err := os.MkdirAll(gitPath, 0700); err != nil {
		return nil, err
	}
	data := &GitRollbackData{
		RepoRoot: root, GitDir: gitDir, Head: state[0], Branch: state[1],
		StatusFile:    filepath.ToSlash(filepath.Join(rollbackGitDirName, rollbackGitStatusFilename)),
		DiffFile:      filepath.ToSlash(filepath.Join(rollbackGitDirName, rollbackGitDiffFilename)),
		CachedFile:    filepath.ToSlash(filepath.Join(rollbackGitDirName, rollbackGitCachedFilename)),
		UntrackedFile: filepath.ToSlash(filepath.Join(rollbackGitDirName, rollbackGitUntrackedFilename)),
		Artifacts:     make(map[string]GitRollbackArtifact),
	}
	budget := &rollbackBudgetWriter{remaining: opts.MaxSizeBytes, limited: opts.MaxSizeBytes > 0}
	untrackedArgs := []string{"ls-files", "--others", "-z"}
	if !gitCleanRemovesIgnored(tokens) {
		untrackedArgs = append(untrackedArgs, "--exclude-standard")
	}
	diffArgs := []string{"diff", "--binary", "--full-index", "--no-ext-diff", "--no-textconv", "--no-color", "--no-renames", "--no-relative", "--src-prefix=a/", "--dst-prefix=b/"}
	for _, item := range []struct {
		path string
		args []string
	}{
		{data.StatusFile, []string{"status", "--porcelain=v1", "-z"}},
		{data.CachedFile, append(append([]string(nil), diffArgs...), "--cached", state[0], "--")},
		{data.DiffFile, append(append([]string(nil), diffArgs...), "--")},
		{data.UntrackedFile, untrackedArgs},
	} {
		artifact, err := writeGitSnapshotArtifact(ctx, root, filepath.Join(rollbackDir, filepath.FromSlash(item.path)), budget, item.args...)
		if err != nil {
			return nil, fmt.Errorf("capturing %s: %w", item.path, err)
		}
		data.Artifacts[item.path] = artifact
	}
	if err := captureGitUntracked(ctx, rollbackDir, data, budget); err != nil {
		return nil, err
	}
	for name, value := range map[string]string{rollbackGitHeadFilename: state[0], rollbackGitBranchFilename: state[1]} {
		if err := os.WriteFile(filepath.Join(gitPath, name), []byte(value+"\n"), 0600); err != nil {
			return nil, fmt.Errorf("writing Git snapshot metadata: %w", err)
		}
	}
	// Refuse a snapshot spanning a concurrent HEAD/index transition. File
	// writes without index changes still require caller coordination.
	latest, err := gitSnapshotState(ctx, root)
	if err != nil {
		return nil, err
	}
	if state != latest {
		return nil, fmt.Errorf("Git HEAD or index changed during rollback capture")
	}
	return data, nil
}

func gitRollbackPrefix(tokens []string) ([]string, error) {
	if len(tokens) < 2 || tokens[0] != "git" {
		return nil, fmt.Errorf("invalid Git rollback command")
	}
	var prefix []string
	for i := 1; i < len(tokens); i++ {
		arg := tokens[i]
		if arg == "-C" {
			if i+1 >= len(tokens) {
				return nil, fmt.Errorf("Git -C requires a directory")
			}
			prefix = append(prefix, arg, tokens[i+1])
			i++
		} else if strings.HasPrefix(arg, "-C") && len(arg) > 2 {
			prefix = append(prefix, "-C", arg[2:])
		} else if strings.HasPrefix(arg, "-") {
			return nil, fmt.Errorf("Git rollback does not support global option %q", arg)
		} else {
			return prefix, nil
		}
	}
	return nil, fmt.Errorf("Git rollback command is missing its operation")
}

func validateGitRollbackEnvironment() error {
	for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR"} {
		if _, present := os.LookupEnv(key); present {
			return fmt.Errorf("Git rollback does not support %s; select the repository with git -C", key)
		}
	}
	return nil
}

func gitSnapshotState(ctx context.Context, root string) ([3]string, error) {
	var state [3]string
	for i, args := range [][]string{
		{"rev-parse", "--verify", "HEAD^{commit}"},
		{"rev-parse", "--abbrev-ref", "HEAD"},
		{"write-tree"},
	} {
		value, err := gitSnapshotOutput(ctx, root, args...)
		if err != nil {
			return state, fmt.Errorf("reading Git snapshot state: %w", err)
		}
		state[i] = strings.TrimSpace(value)
	}
	return state, nil
}

func gitSnapshotOutput(ctx context.Context, dir string, args ...string) (string, error) {
	var out bytes.Buffer
	limit := &rollbackBudgetWriter{writer: &out, remaining: 1 << 20, limited: true}
	err := runGitSnapshotCommand(ctx, dir, limit, args...)
	return out.String(), err
}

func runGitSnapshotCommand(ctx context.Context, dir string, output io.Writer, args ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	commandArgs := append([]string{"-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", commandArgs...)
	cmd.Dir, cmd.Env = dir, os.Environ()
	cmd.Stdout = output
	stderr := &outputCapture{limit: 64 << 10}
	cmd.Stderr = stderr
	cmd.WaitDelay = time.Second
	stopGroup := configureCommandCancellation(cmd, false)
	err := cmd.Run()
	if err != nil {
		if stopGroup != nil {
			if stopErr := stopGroup(); stopErr != nil && !errors.Is(stopErr, os.ErrProcessDone) {
				err = errors.Join(err, stopErr)
			}
		}
		if ctx.Err() != nil {
			err = errors.Join(ctx.Err(), err)
		}
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return nil
}

func writeGitSnapshotArtifact(ctx context.Context, root, path string, budget *rollbackBudgetWriter, args ...string) (artifact GitRollbackArtifact, err error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return artifact, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	hash := sha256.New()
	budget.writer = io.MultiWriter(file, hash)
	if err := runGitSnapshotCommand(ctx, root, budget, args...); err != nil {
		return artifact, err
	}
	if err := file.Sync(); err != nil {
		return artifact, err
	}
	stat, err := file.Stat()
	if err != nil {
		return artifact, err
	}
	return GitRollbackArtifact{Size: stat.Size(), SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

// validateGitArtifact rejects missing, corrupt, or redirected artifacts BEFORE
// rollback resets a branch or changes files. Hashes detect damage, not hostile
// edits to both artifacts and metadata by an actor who owns the project.
func validateGitArtifact(base, rel string, artifacts map[string]GitRollbackArtifact) (string, error) {
	if rel == "" || !filepath.IsLocal(filepath.FromSlash(rel)) {
		return "", fmt.Errorf("invalid Git rollback artifact path %q", rel)
	}
	path := filepath.Join(base, filepath.FromSlash(rel))
	if err := ensureNoSymlinkParents(base, path); err != nil {
		return "", err
	}
	expected, ok := artifacts[rel]
	if !ok || len(expected.SHA256) != 64 || expected.Size < 0 {
		return "", fmt.Errorf("Git rollback artifact %q has no integrity record; recapture before execution", rel)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("opening Git rollback artifact: %w", err)
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !stat.Mode().IsRegular() || stat.Size() != expected.Size {
		return "", fmt.Errorf("Git rollback artifact %q has changed size or type", rel)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected.SHA256 {
		return "", fmt.Errorf("Git rollback artifact %q failed its checksum", rel)
	}
	return path, nil
}

func restoreGitRollback(ctx context.Context, data *RollbackData, opts RollbackRestoreOptions) error {
	if data.Git == nil {
		return fmt.Errorf("git rollback data missing")
	}
	if !opts.Force {
		return fmt.Errorf("git rollback is destructive (use --force)")
	}
	if err := validateGitRollbackEnvironment(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*DefaultExecutionTimeout)
	defer cancel()
	root := data.Git.RepoRoot
	if root == "" || !filepath.IsAbs(root) {
		return fmt.Errorf("git repo root missing or not absolute")
	}
	actual, err := gitSnapshotOutput(ctx, root, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	if data.Git.GitDir == "" || strings.TrimSuffix(actual, "\n") != data.Git.GitDir {
		return fmt.Errorf("Git repository identity changed since capture")
	}
	if len(data.Git.Head) != 40 && len(data.Git.Head) != 64 {
		return fmt.Errorf("invalid saved Git HEAD")
	}
	if _, err := hex.DecodeString(data.Git.Head); err != nil {
		return fmt.Errorf("invalid saved Git HEAD: %w", err)
	}
	if _, err := gitSnapshotOutput(ctx, root, "cat-file", "-e", data.Git.Head+"^{commit}"); err != nil {
		return fmt.Errorf("saved Git commit is unavailable: %w", err)
	}
	cached, err := validateGitArtifact(data.RollbackPath, data.Git.CachedFile, data.Git.Artifacts)
	if err != nil {
		return err
	}
	diff, err := validateGitArtifact(data.RollbackPath, data.Git.DiffFile, data.Git.Artifacts)
	if err != nil {
		return err
	}
	untracked, err := validateGitUntracked(ctx, data)
	if err != nil {
		return err
	}
	branch := data.Git.Branch
	if branch == "" {
		return fmt.Errorf("saved Git branch is missing")
	}
	if branch != "HEAD" {
		if _, err := gitSnapshotOutput(ctx, root, "check-ref-format", "--branch", branch); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Restore the saved branch, not whichever branch happens to be active.
	// -B also recreates a branch removed by the approved command; Git refuses
	// to steal a branch that is checked out in a different linked worktree.
	args := []string{"checkout", "--force", "--detach", data.Git.Head}
	if branch != "HEAD" {
		args = []string{"checkout", "--force", "-B", branch, data.Git.Head}
	}
	if _, err := gitSnapshotOutput(ctx, root, args...); err != nil {
		return fmt.Errorf("restoring Git HEAD: %w", err)
	}
	if _, err := gitSnapshotOutput(ctx, root, "reset", "--hard", data.Git.Head); err != nil {
		return err
	}
	// First reconstitute the staged baseline in BOTH the index and worktree.
	// Applying only to --cached leaves the worktree at HEAD, so subsequent
	// unstaged patches either fail or silently lose the staged changes.
	if err := applyGitPatchIfPresent(ctx, root, cached, true); err != nil {
		return err
	}
	if err := applyGitPatchIfPresent(ctx, root, diff, false); err != nil {
		return err
	}
	return restoreGitUntracked(ctx, data, untracked)
}

func applyGitPatchIfPresent(ctx context.Context, root, patchPath string, cached bool) error {
	stat, err := os.Stat(patchPath)
	if os.IsNotExist(err) {
		return nil // Optional helper; restore validates mandatory artifacts first.
	}
	if err != nil {
		return err
	}
	if stat.Size() == 0 {
		return nil
	}
	// Preserve the optional helper's treatment of whitespace-only patches
	// without reading an arbitrarily large recovery artifact into memory.
	file, err := os.Open(patchPath)
	if err != nil {
		return err
	}
	defer file.Close()
	var prefix [4096]byte
	for {
		n, readErr := file.Read(prefix[:])
		if len(bytes.TrimSpace(prefix[:n])) != 0 {
			break
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
	args := []string{"apply", "--binary", "--whitespace=nowarn"}
	if cached {
		args = append(args, "--index")
	}
	args = append(args, "--", patchPath)
	if _, err := gitSnapshotOutput(ctx, root, args...); err != nil {
		return fmt.Errorf("git apply (%s): %w", filepath.Base(patchPath), err)
	}
	return nil
}
