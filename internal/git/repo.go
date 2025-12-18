package git

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	defaultHistoryAuthorName  = "slb"
	defaultHistoryAuthorEmail = "slb@localhost"
)

func expandUserPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("path is required")
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", fmt.Errorf("resolving home directory: %w", err)
		}
		suffix := strings.TrimPrefix(p, "~")
		suffix = strings.TrimPrefix(suffix, "/")
		suffix = strings.TrimPrefix(suffix, "\\")
		if suffix == "" {
			return home, nil
		}
		return filepath.Join(home, suffix), nil
	}
	return p, nil
}

func runGit(repoPath string, args ...string) (string, error) {
	if repoPath == "" {
		return "", fmt.Errorf("repoPath is required")
	}

	cmd := exec.Command("git", append([]string{"-C", repoPath}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg != "" {
			return strings.TrimSpace(stdout.String()), fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), msg, err)
		}
		return strings.TrimSpace(stdout.String()), fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}

	return strings.TrimSpace(stdout.String()), nil
}

func ensureGitRepo(repoPath string) error {
	if err := os.MkdirAll(repoPath, 0700); err != nil {
		return fmt.Errorf("creating repo directory: %w", err)
	}

	if IsRepo(repoPath) {
		return nil
	}
	_, err := runGit(repoPath, "init")
	return err
}

func ensureGitIdentity(repoPath string) error {
	// Ensure commits always work even if user-level git config is absent.
	if _, err := runGit(repoPath, "config", "user.name", defaultHistoryAuthorName); err != nil {
		return err
	}
	if _, err := runGit(repoPath, "config", "user.email", defaultHistoryAuthorEmail); err != nil {
		return err
	}
	return nil
}

func gitAdd(repoPath string, relPaths ...string) error {
	if len(relPaths) == 0 {
		return nil
	}
	args := append([]string{"add", "--"}, relPaths...)
	_, err := runGit(repoPath, args...)
	return err
}

func stagedChangesExist(repoPath string) (bool, error) {
	out, err := runGit(repoPath, "diff", "--cached", "--name-only")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

func gitCommitIfNeeded(repoPath string, message string) (bool, error) {
	if strings.TrimSpace(message) == "" {
		return false, fmt.Errorf("commit message is required")
	}

	hasChanges, err := stagedChangesExist(repoPath)
	if err != nil {
		return false, err
	}
	if !hasChanges {
		return false, nil
	}

	_, err = runGit(repoPath, "commit", "-m", message)
	if err != nil {
		// Handle "nothing to commit" edge cases gracefully.
		if strings.Contains(err.Error(), "nothing to commit") {
			return false, nil
		}
		return false, err
	}

	return true, nil
}
