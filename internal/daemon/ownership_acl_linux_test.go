package daemon

import (
	"encoding/binary"
	"errors"
	"os"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRuntimeParentsRejectPrivateGroupWithAccessACL(t *testing.T) {
	path := t.TempDir()
	// Linux POSIX ACL xattr with a named-user entry. Use the current UID so
	// the fixture also works inside a namespace with only one mapped user.
	// All extended access ACLs are rejected, even this redundant grant; the
	// resulting 0775 mode alone cannot establish which identities may write.
	acl := make([]byte, 4+5*8)
	binary.LittleEndian.PutUint32(acl, 2)
	entries := []struct {
		tag, permissions uint16
		id               uint32
	}{
		{0x01, 7, ^uint32(0)},
		{0x02, 7, uint32(os.Geteuid())},
		{0x04, 5, ^uint32(0)},
		{0x10, 7, ^uint32(0)},
		{0x20, 5, ^uint32(0)},
	}
	for i, entry := range entries {
		offset := 4 + 8*i
		binary.LittleEndian.PutUint16(acl[offset:], entry.tag)
		binary.LittleEndian.PutUint16(acl[offset+2:], entry.permissions)
		binary.LittleEndian.PutUint32(acl[offset+4:], entry.id)
	}
	if err := unix.Setxattr(path, "system.posix_acl_access", acl, 0); err != nil {
		if errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EPERM) {
			t.Skipf("filesystem cannot install an access ACL: %v", err)
		}
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != 0775 {
		t.Fatalf("ACL fixture must have mode 0775: %v, %v", info, err)
	}
	if runtimeAncestorPermissions(path, info, func() bool { return true }) {
		t.Fatal("private-group mode bits hid an extended access ACL")
	}
}
