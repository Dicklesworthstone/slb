package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const nativeHookMarker = "# SLB native Git hook v1\n"

// HookStatus describes the effective hook, including an existing foreign hook.
type HookStatus struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Installed  bool   `json:"installed"`
	Managed    bool   `json:"managed"`
	Executable bool   `json:"executable"`
	Backup     string `json:"backup,omitempty"`
}

func hookNames(names []string) ([]string, error) {
	if len(names) == 0 {
		names = []string{"pre-commit", "pre-push", "pre-rebase"}
	}
	seen := make(map[string]bool)
	for _, name := range names {
		if name != "pre-commit" && name != "pre-push" && name != "pre-rebase" {
			return nil, fmt.Errorf("unsupported Git hook %q; supported: pre-commit, pre-push, pre-rebase", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate Git hook %q", name)
		}
		seen[name] = true
	}
	return names, nil
}

// NativeHookDirectory uses Git's own path resolution: worktrees use a .git
// file, and core.hooksPath may redirect hooks outside .git/hooks entirely.
func NativeHookDirectory(ctx context.Context, repo string) (string, error) {
	rootCmd := exec.CommandContext(ctx, "git", "-C", repo, "rev-parse", "--show-toplevel")
	root, err := rootCmd.Output()
	if err != nil {
		return "", fmt.Errorf("finding Git worktree: %w", err)
	}
	pathCmd := exec.CommandContext(ctx, "git", "-C", strings.TrimSuffix(string(root), "\n"), "rev-parse", "--path-format=absolute", "--git-path", "hooks")
	path, err := pathCmd.Output()
	if err != nil {
		return "", fmt.Errorf("finding effective Git hooks directory: %w", err)
	}
	directory := strings.TrimSuffix(string(path), "\n")
	if !filepath.IsAbs(directory) {
		return "", errors.New("Git returned a non-absolute hooks directory")
	}
	return directory, nil
}

func nativeHookScript(name string) string {
	// Preserve argv and (for pre-push) stdin. A missing binary is never permission.
	return "#!/bin/sh\n" + nativeHookMarker +
		"if ! command -v slb >/dev/null 2>&1; then\n" +
		"  echo 'SLB: binary unavailable; Git operation blocked. Restore slb on PATH.' >&2\n" +
		"  exit 1\nfi\nexec slb hook " + name + " -- \"$@\"\n"
}

func readNativeHook(path, name string) (HookStatus, error) {
	status := HookStatus{Name: name, Path: path}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return status, nil
	}
	if err != nil {
		return status, err
	}
	status.Installed = true
	// Never read through a symlink, FIFO, socket or directory to find a marker.
	if !info.Mode().IsRegular() {
		return status, nil
	}
	status.Executable = info.Mode().Perm()&0111 != 0
	if info.Size() > 4096 {
		return status, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return status, err
	}
	status.Managed = string(data) == nativeHookScript(name)
	return status, nil
}

// NativeHookStatus reports the effective hooks without creating directories.
func NativeHookStatus(ctx context.Context, repo string, names []string) ([]HookStatus, error) {
	names, err := hookNames(names)
	if err != nil {
		return nil, err
	}
	directory, err := NativeHookDirectory(ctx, repo)
	if err != nil {
		return nil, err
	}
	result := make([]HookStatus, 0, len(names))
	for _, name := range names {
		status, err := readNativeHook(filepath.Join(directory, name), name)
		if err != nil {
			return nil, err
		}
		result = append(result, status)
	}
	return result, nil
}

// InstallNativeHooks does not replace existing hooks, even when --force might
// otherwise be convenient. Validate the whole selection before making changes.
// Atomic no-replace publication prevents Git observing a half-written script.
func InstallNativeHooks(ctx context.Context, repo string, names []string) ([]HookStatus, error) {
	statuses, err := NativeHookStatus(ctx, repo, names)
	if err != nil {
		return nil, err
	}
	for _, status := range statuses {
		if status.Installed && !status.Managed {
			return nil, fmt.Errorf("preserving existing hook %s; integrate slb hook %s manually", status.Path, status.Name)
		}
	}
	for _, status := range statuses {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if status.Installed {
			if !status.Executable {
				if err := os.Chmod(status.Path, 0755); err != nil {
					return nil, err
				}
			}
			continue
		}
		if err := publishNativeHook(status.Path, nativeHookScript(status.Name)); err != nil {
			return nil, err
		}
	}
	return NativeHookStatus(ctx, repo, names)
}

func publishNativeHook(path, script string) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0755); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".slb-hook-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := file.WriteString(script); err != nil {
		return err
	}
	if err := file.Chmod(0755); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	// Link is atomic and, unlike Rename, refuses to clobber a racing installer.
	if err := os.Link(file.Name(), path); err != nil {
		return fmt.Errorf("publishing hook without overwriting %s: %w", path, err)
	}
	return nil
}

// UninstallNativeHooks preserves scripts as disabled backups, and refuses to
// move foreign or modified hooks. A backup is never a Git hook entrypoint.
func UninstallNativeHooks(ctx context.Context, repo string, names []string) ([]HookStatus, error) {
	statuses, err := NativeHookStatus(ctx, repo, names)
	if err != nil {
		return nil, err
	}
	for _, status := range statuses {
		if status.Installed && !status.Managed {
			return nil, fmt.Errorf("refusing to uninstall unmanaged hook %s", status.Path)
		}
	}
	for i, status := range statuses {
		if !status.Installed {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Reserve a unique backup name in the same directory before renaming.
		backup, err := os.CreateTemp(filepath.Dir(status.Path), ".slb-disabled-"+status.Name+"-*")
		if err != nil {
			return nil, err
		}
		if err := backup.Close(); err != nil {
			return nil, err
		}
		if err := os.Rename(status.Path, backup.Name()); err != nil {
			return nil, err
		}
		statuses[i].Installed = false
		statuses[i].Executable = false
		statuses[i].Backup = backup.Name()
	}
	return statuses, nil
}

// NativeProtectedBranches always protects main, plus configured additions.
// Git config supports repeated values: git config --add slb.protectedBranch release.
func NativeProtectedBranches(ctx context.Context, repo string) ([]string, error) {
	command := exec.CommandContext(ctx, "git", "-C", repo, "config", "--get-all", "slb.protectedBranch")
	data, err := command.Output()
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
		return nil, fmt.Errorf("reading protected Git branches: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	branches := []string{"refs/heads/main"}
	if len(data) == 0 {
		return branches, nil
	}
	for _, ref := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if !strings.HasPrefix(ref, "refs/") {
			ref = "refs/heads/" + ref
		}
		if err := exec.CommandContext(ctx, "git", "-C", repo, "check-ref-format", ref).Run(); err != nil {
			return nil, fmt.Errorf("invalid slb.protectedBranch setting")
		}
		branches = append(branches, ref)
	}
	return branches, nil
}
