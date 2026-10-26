package testutil

import (
	"path/filepath"
	"strings"
	"testing"
)

// protectedSystemRoots mirrors the system directories whose recursive
// deletion the default patterns classify as CRITICAL (rmSystemPath in
// internal/config/defaults.go).
var protectedSystemRoots = []string{
	"bin", "boot", "dev", "etc", "lib", "lib64", "media", "mnt",
	"opt", "proc", "run", "sbin", "srv", "sys", "usr", "var",
}

// NonSystemTempDir returns a temporary directory that the deletion classifier
// treats as ordinary project or home data. t.TempDir() can live below a
// protected system directory: macOS puts the per-user temporary directory
// under /var/folders, and every recursive deletion below /var is CRITICAL by
// design. In that case the symlink-resolved path (/private/var/folders on
// macOS) is used; if that is protected too, the test is skipped rather than
// asserting a tier the classifier must not return there.
func NonSystemTempDir(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	if !underProtectedSystemRoot(dir) {
		return dir
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil && !underProtectedSystemRoot(resolved) {
		return resolved
	}
	t.Skipf("temporary directory %s is below a protected system directory", dir)
	return ""
}

func underProtectedSystemRoot(path string) bool {
	first, _, _ := strings.Cut(strings.TrimLeft(filepath.ToSlash(path), "/"), "/")
	for _, root := range protectedSystemRoots {
		if first == root {
			return true
		}
	}
	return false
}
