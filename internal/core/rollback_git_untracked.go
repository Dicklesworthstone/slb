package core

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Dicklesworthstone/slb/internal/db"
)

// gitRollbackStorage survives both reset --hard and clean -fdx, including in
// linked worktrees where .git is a pointer file rather than a directory.
func gitRollbackStorage(ctx context.Context, req *db.Request, tokens []string) (string, error) {
	if err := validateGitRollbackEnvironment(); err != nil {
		return "", err
	}
	prefix, err := gitRollbackPrefix(tokens)
	if err != nil {
		return "", err
	}
	cwd := req.Command.Cwd
	if cwd == "" {
		cwd = req.ProjectPath
	}
	ctx, cancel := context.WithTimeout(ctx, defaultRollbackCmdTimeout)
	defer cancel()
	gitDir, err := gitSnapshotOutput(ctx, cwd, append(prefix, "rev-parse", "--absolute-git-dir")...)
	if err != nil {
		return "", err
	}
	gitDir = strings.TrimSuffix(gitDir, "\n")
	if !filepath.IsAbs(gitDir) {
		return "", fmt.Errorf("Git recovery storage is not absolute")
	}
	return filepath.Join(gitDir, "slb-rollback"), nil
}

func gitCleanRemovesIgnored(tokens []string) bool {
	for i := 1; i < len(tokens); i++ {
		arg := tokens[i]
		if arg == "-C" {
			i++
			continue
		}
		if strings.HasPrefix(arg, "-C") {
			continue
		}
		if arg != "clean" {
			return false
		}
		for j := i + 1; j < len(tokens); j++ {
			a := tokens[j]
			if a == "--" {
				break
			}
			if a == "-e" || a == "--exclude" {
				j++
				continue
			}
			if strings.HasPrefix(a, "--") || !strings.HasPrefix(a, "-") {
				continue
			}
			// A bundled -e consumes the rest of its token as the pattern.
			for k := 1; k < len(a); k++ {
				if a[k] == 'e' {
					if k == len(a)-1 {
						j++
					}
					break
				}
				if a[k] == 'x' || a[k] == 'X' {
					return true
				}
			}
		}
		return false
	}
	return false
}

// Plain tar keeps the budget meaningful for incompressible and highly
// compressible files alike. Every payload byte and archive header is counted.
func captureGitUntracked(ctx context.Context, base string, data *GitRollbackData, budget *rollbackBudgetWriter) (err error) {
	list, err := os.Open(filepath.Join(base, filepath.FromSlash(data.UntrackedFile)))
	if err != nil {
		return err
	}
	defer list.Close()
	rel := filepath.ToSlash(filepath.Join(rollbackGitDirName, "untracked.tar"))
	file, err := os.OpenFile(filepath.Join(base, filepath.FromSlash(rel)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	hash := sha256.New()
	budget.writer = io.MultiWriter(file, hash)
	tw := tar.NewWriter(budget)
	defer func() { err = errors.Join(err, tw.Close()) }()
	reader := bufio.NewReader(list)
	for {
		name, readErr := reader.ReadString(0)
		if errors.Is(readErr, io.EOF) && name == "" {
			break
		}
		if readErr != nil {
			return fmt.Errorf("reading untracked file list: %w", readErr)
		}
		name = strings.TrimSuffix(name, "\x00")
		path, err := gitUntrackedTarget(data.RepoRoot, name)
		if err != nil {
			return err
		}
		if err := ensureNoSymlinkParents(data.RepoRoot, filepath.Dir(path)); err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("untracked file changed during capture: %w", err)
		}
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("cannot recover untracked special file or nested repository %q", name)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = name
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			if err := copyGitUntrackedFile(ctx, tw, path, info); err != nil {
				return err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	stat, err := file.Stat()
	if err != nil {
		return err
	}
	data.UntrackedArchive = rel
	data.Artifacts[rel] = GitRollbackArtifact{Size: stat.Size(), SHA256: hex.EncodeToString(hash.Sum(nil))}
	return nil
}

func copyGitUntrackedFile(ctx context.Context, out io.Writer, path string, before os.FileInfo) (err error) {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(before, opened) || !opened.Mode().IsRegular() {
		return fmt.Errorf("untracked file replaced during capture: %s", path)
	}
	if _, err := io.CopyN(out, gitRecoveryReader{ctx, file}, before.Size()); err != nil {
		return err
	}
	after, err := file.Stat()
	if err != nil {
		return err
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return fmt.Errorf("untracked file changed during capture: %s", path)
	}
	return nil
}

type gitRecoveryReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r gitRecoveryReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func gitUntrackedTarget(root, name string) (string, error) {
	local := filepath.FromSlash(name)
	if name == "" || strings.ContainsRune(name, 0) || !filepath.IsLocal(local) || filepath.ToSlash(filepath.Clean(local)) != name {
		return "", fmt.Errorf("invalid untracked recovery path %q", name)
	}
	for _, part := range strings.Split(name, "/") {
		if strings.EqualFold(part, ".git") {
			return "", fmt.Errorf("untracked recovery cannot replace Git metadata")
		}
	}
	return filepath.Join(root, local), nil
}

// Validate the entire archive and every destination before tracked restoration
// starts. Untracked symlinks may be restored as links, but never traversed.
func validateGitUntracked(ctx context.Context, data *RollbackData) (string, error) {
	if data.Git.UntrackedArchive == "" {
		return "", nil
	}
	path, err := validateGitArtifact(data.RollbackPath, data.Git.UntrackedArchive, data.Git.Artifacts)
	if err != nil {
		return "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	tr := tar.NewReader(gitRecoveryReader{ctx, file})
	seen := make(map[string]bool)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA && hdr.Typeflag != tar.TypeSymlink {
			return "", fmt.Errorf("unsupported untracked recovery entry %q", hdr.Name)
		}
		target, err := gitUntrackedTarget(data.Git.RepoRoot, hdr.Name)
		if err != nil {
			return "", err
		}
		if seen[hdr.Name] {
			return "", fmt.Errorf("duplicate untracked recovery entry %q", hdr.Name)
		}
		seen[hdr.Name] = true
		if err := ensureNoSymlinkParents(data.Git.RepoRoot, filepath.Dir(target)); err != nil {
			return "", err
		}
		if info, err := os.Lstat(target); err == nil && info.IsDir() {
			return "", fmt.Errorf("untracked recovery would replace an existing directory: %s", target)
		} else if err != nil && !os.IsNotExist(err) {
			return "", err
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return "", err
		}
	}
	for name := range seen {
		for parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(name))); parent != "."; parent = filepath.ToSlash(filepath.Dir(filepath.FromSlash(parent))) {
			if seen[parent] {
				return "", fmt.Errorf("untracked recovery entry has a file or symlink parent: %s", name)
			}
		}
	}
	return path, nil
}

func restoreGitUntracked(ctx context.Context, data *RollbackData, archive string) error {
	if archive == "" {
		return nil
	}
	file, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer file.Close()
	tr := tar.NewReader(gitRecoveryReader{ctx, file})
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := gitUntrackedTarget(data.Git.RepoRoot, hdr.Name)
		if err != nil {
			return err
		}
		parent := filepath.Dir(target)
		if err := ensureNoSymlinkParents(data.Git.RepoRoot, parent); err != nil {
			return err
		}
		if err := os.MkdirAll(parent, 0700); err != nil {
			return err
		}
		if err := ensureNoSymlinkParents(data.Git.RepoRoot, parent); err != nil {
			return err
		}
		if err := restoreGitUntrackedEntry(ctx, tr, hdr, target); err != nil {
			return err
		}
	}
}

func restoreGitUntrackedEntry(ctx context.Context, reader io.Reader, hdr *tar.Header, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if hdr.Typeflag == tar.TypeSymlink {
		dir, err := os.MkdirTemp(filepath.Dir(target), ".slb-link-*")
		if err != nil {
			return err
		}
		defer os.Remove(dir) // Only this call's empty temporary directory.
		link := filepath.Join(dir, "link")
		if err := os.Symlink(hdr.Linkname, link); err != nil {
			return err
		}
		defer os.Remove(link) // Owned temporary link, never its destination.
		return os.Rename(link, target)
	}
	if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
		return fmt.Errorf("unsupported untracked entry type")
	}
	file, err := os.CreateTemp(filepath.Dir(target), ".slb-recover-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name()) // Remove only our incomplete staging file.
	_, copyErr := io.Copy(file, gitRecoveryReader{ctx, reader})
	chmodErr := file.Chmod(os.FileMode(hdr.Mode) & os.ModePerm)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(copyErr, chmodErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), target); err != nil {
		return err
	}
	return os.Chtimes(target, hdr.ModTime, hdr.ModTime)
}
