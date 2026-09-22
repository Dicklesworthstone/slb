package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func hookTestGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s: %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}
func hookTestRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	repo := t.TempDir()
	hookTestGit(t, repo, "init", "-b", "main")
	hookTestGit(t, repo, "config", "user.name", "Hook Test")
	hookTestGit(t, repo, "config", "user.email", "hooks@example.invalid")
	hookTestGit(t, repo, "config", "commit.gpgsign", "false")
	return repo
}
func TestNativeHooksLifecycle(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "custom-hooks-path"}[custom], func(t *testing.T) {
			repo := hookTestRepo(t)
			if custom {
				hookTestGit(t, repo, "config", "core.hooksPath", "custom hooks")
			}
			statuses, err := InstallNativeHooks(context.Background(), repo, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, status := range statuses {
				if !status.Installed || !status.Managed || !status.Executable {
					t.Fatalf("invalid status: %+v", status)
				}
				if custom && !strings.Contains(status.Path, "custom hooks") {
					t.Fatal(status.Path)
				}
			}
			if _, err := InstallNativeHooks(context.Background(), repo, nil); err != nil {
				t.Fatal("idempotent install:", err)
			}
			statuses, err = UninstallNativeHooks(context.Background(), repo, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, status := range statuses {
				if status.Installed || status.Backup == "" {
					t.Fatalf("invalid uninstall: %+v", status)
				}
				data, err := os.ReadFile(status.Backup)
				if err != nil || string(data) != nativeHookScript(status.Name) {
					t.Fatalf("backup lost: %s %v", data, err)
				}
			}
		})
	}
}
func TestNativeHooksWorktree(t *testing.T) {
	repo := hookTestRepo(t)
	hookTestGit(t, repo, "commit", "--allow-empty", "-m", "initial")
	worktree := filepath.Join(t.TempDir(), "linked")
	hookTestGit(t, repo, "worktree", "add", "-b", "linked", worktree)
	info, err := os.Stat(filepath.Join(worktree, ".git"))
	if err != nil || !info.Mode().IsRegular() {
		t.Fatal("worktree .git file missing")
	}
	statuses, err := InstallNativeHooks(context.Background(), worktree, []string{"pre-push"})
	if err != nil || len(statuses) != 1 || !statuses[0].Managed {
		t.Fatalf("worktree install: %+v %v", statuses, err)
	}
}
func TestNativeHooksPreserveForeignAndSymlink(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "foreign", true: "symlink"}[symlink], func(t *testing.T) {
			repo := hookTestRepo(t)
			directory, err := NativeHookDirectory(context.Background(), repo)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, "pre-push")
			target := filepath.Join(t.TempDir(), "target")
			if symlink {
				if err := os.WriteFile(target, []byte(nativeHookScript("pre-push")), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Skip(err)
				}
			} else if err := os.WriteFile(path, []byte("#!/bin/sh\necho foreign\n"), 0755); err != nil {
				t.Fatal(err)
			}
			if _, err := InstallNativeHooks(context.Background(), repo, nil); err == nil {
				t.Fatal("replaced foreign hook")
			}
			if _, err := os.Stat(filepath.Join(directory, "pre-commit")); !os.IsNotExist(err) {
				t.Fatal("partial installation before conflict")
			}
			if _, err := UninstallNativeHooks(context.Background(), repo, nil); err == nil {
				t.Fatal("uninstalled foreign hook")
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("foreign hook lost", err)
			}
		})
	}
	for _, names := range [][]string{{"../escape"}, {"pre-rebase"}, {"pre-push", "pre-push"}} {
		if _, err := InstallNativeHooks(context.Background(), hookTestRepo(t), names); err == nil {
			t.Fatal("accepted invalid hooks", names)
		}
	}
}
func TestNativeHookProtocolAndFailure(t *testing.T) {
	repo := hookTestRepo(t)
	statuses, err := InstallNativeHooks(context.Background(), repo, []string{"pre-push"})
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	slb := filepath.Join(bin, "slb")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\"\nwhile IFS= read -r line; do printf '%s\\n' \"$line\"; done\nexit 7\n"
	if err := os.WriteFile(slb, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/bin/sh", statuses[0].Path, "origin", "remote location with spaces")
	command.Env = append(os.Environ(), "PATH="+bin)
	command.Stdin = strings.NewReader("ref oid refs/heads/dev old\n")
	data, err := command.CombinedOutput()
	if err == nil || command.ProcessState.ExitCode() != 7 {
		t.Fatalf("exit status lost: %s %v", data, err)
	}
	if string(data) != "hook\npre-push\n--\norigin\nremote location with spaces\nref oid refs/heads/dev old\n" {
		t.Fatalf("argv/stdin changed: %q", data)
	}
	command = exec.Command("/bin/sh", statuses[0].Path, "origin", "location")
	command.Env = append(os.Environ(), "PATH="+t.TempDir())
	data, err = command.CombinedOutput()
	if err == nil || !strings.Contains(string(data), "blocked") {
		t.Fatalf("missing binary failed open: %s %v", data, err)
	}
}

func TestNativeProtectedBranches(t *testing.T) {
	repo := hookTestRepo(t)
	refs, err := NativeProtectedBranches(context.Background(), repo)
	if err != nil || len(refs) != 1 || refs[0] != "refs/heads/main" {
		t.Fatalf("default protection: %v %v", refs, err)
	}
	hookTestGit(t, repo, "config", "--add", "slb.protectedBranch", "release")
	hookTestGit(t, repo, "config", "--add", "slb.protectedBranch", "refs/heads/production")
	refs, err = NativeProtectedBranches(context.Background(), repo)
	if err != nil || strings.Join(refs, ",") != "refs/heads/main,refs/heads/release,refs/heads/production" {
		t.Fatalf("configured protection: %v %v", refs, err)
	}
	hookTestGit(t, repo, "config", "--add", "slb.protectedBranch", "invalid branch")
	if _, err := NativeProtectedBranches(context.Background(), repo); err == nil {
		t.Fatal("invalid protected branch ignored")
	}
}
