package daemon

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func runtimeAncestorHasNoACL(path string, original os.FileInfo) bool {
	// With an extended POSIX ACL, the group mode bits describe the ACL mask;
	// a named user outside the private group could have write access too.
	_, err := unix.Lgetxattr(path, "system.posix_acl_access", nil)
	if !errors.Is(err, syscall.ENODATA) && !errors.Is(err, syscall.ENOTSUP) {
		return false
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(original, current) || original.Mode() != current.Mode() {
		return false
	}
	before, beforeOK := original.Sys().(*syscall.Stat_t)
	after, afterOK := current.Sys().(*syscall.Stat_t)
	return beforeOK && afterOK && before.Uid == after.Uid && before.Gid == after.Gid
}
