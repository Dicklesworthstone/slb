//go:build windows

package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows FileInfo does not contain the owner SID. Open the object itself,
// without following reparse points, and verify that it is still the snapshot
// the caller inspected. Native Unix sockets are themselves reparse points.
func ownedByCurrentUser(path string, info os.FileInfo) bool {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	handle, err := windows.CreateFile(name, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return false
	}
	file := os.NewFile(uintptr(handle), path)
	defer file.Close()
	current, err := file.Stat()
	if err != nil || !os.SameFile(info, current) {
		return false
	}
	user, defaultOwner, err := windowsDaemonIdentity()
	if err != nil {
		return false
	}
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false
	}
	owner, _, err := sd.Owner()
	return err == nil && owner != nil && (owner.Equals(user) || owner.Equals(defaultOwner))
}

func protectSocket(path string, original os.FileInfo) error {
	user, defaultOwner, err := windowsDaemonIdentity()
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(name, windows.READ_CONTROL|windows.WRITE_DAC|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(handle), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 || !os.SameFile(original, info) {
		return errors.New("socket path changed before setting permissions")
	}
	existing, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := existing.Owner()
	if err != nil || owner == nil || (!owner.Equals(user) && !owner.Equals(defaultOwner)) {
		return errors.New("socket must be owned by the current user")
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.String() + ")(A;;FA;;;SY)")
	if err != nil {
		return err
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	// Chmod only changes the read-only attribute on Windows. Restrict the
	// actual ACL used by AF_UNIX to authorize local client connections.
	if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		return err
	}
	current, err := lstatOwnedPath(path)
	if err != nil || !os.SameFile(original, current) {
		return errors.New("socket path changed while setting permissions")
	}
	return nil
}

func processAlive(pid int) bool {
	if pid <= 0 || uint64(pid) > uint64(^uint32(0)) {
		return false
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		// Access denial and other ambiguous failures do not prove the PID is
		// stale. Windows uses INVALID_PARAMETER for a nonexistent process.
		return !errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	// Signal(0) is unsupported on Windows. A zero-time process wait tests
	// liveness without confusing a legitimate exit code 259 with STILL_ACTIVE.
	state, err := windows.WaitForSingleObject(handle, 0)
	return err != nil || state != windows.WAIT_OBJECT_0
}

func openPIDFile(path string, flags int) (*os.File, os.FileInfo, error) {
	return openWindowsDaemonFile(path, flags, false)
}

// os.Lstat on Windows can defer fetching a regular file's identity until
// os.SameFile, using an exclusive handle incompatible with an open PID file.
// Capture its identity now through a no-follow, metadata-only shared handle.
func lstatOwnedPath(path string) (os.FileInfo, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, &os.PathError{Op: "stat daemon file", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(handle), path)
	defer file.Close()
	return file.Stat()
}

func acquireFileLease(path string) (*fileLease, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("daemon lease path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	file, info, err := openWindowsDaemonFile(path, os.O_CREATE|os.O_RDWR, true)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = file.Close()
		}
	}()
	// Locking beyond EOF is supported, so the persistent lease can stay empty.
	// The non-inheritable handle owns this lock until fileLease.Close or exit.
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &windows.Overlapped{}); err != nil {
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, fmt.Errorf("%w: %s", ErrDaemonOwned, path)
		}
		return nil, fmt.Errorf("locking daemon lease: %w", err)
	}
	current, err := lstatOwnedPath(path)
	if err != nil || !os.SameFile(info, current) {
		return nil, errors.New("daemon lease path changed while acquiring ownership")
	}
	keep = true
	return &fileLease{file: file}, nil
}

// openWindowsDaemonFile never truncates or follows the final path component.
// Lease handles deny delete sharing, keeping one stable lock file for all
// contenders; PID handles allow ownership-checked publication and cleanup.
func openWindowsDaemonFile(path string, flags int, private bool) (*os.File, os.FileInfo, error) {
	if flags & ^(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_EXCL) != 0 {
		return nil, nil, errors.New("unsupported daemon file open flags")
	}
	user, defaultOwner, err := windowsDaemonIdentity()
	if err != nil {
		return nil, nil, err
	}
	// Do not inherit a permissive directory ACL when creating a new file.
	// Explicit ownership also avoids an elevated token's default group owner.
	sd, err := windows.SecurityDescriptorFromString("O:" + user.String() + "D:P(A;;FA;;;" + user.String() + ")(A;;FA;;;SY)")
	if err != nil {
		return nil, nil, err
	}
	security := windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, nil, err
	}
	access := uint32(windows.READ_CONTROL)
	switch flags & (os.O_WRONLY | os.O_RDWR) {
	case os.O_WRONLY:
		access |= windows.GENERIC_WRITE
	case os.O_RDWR:
		access |= windows.GENERIC_READ | windows.GENERIC_WRITE
	case os.O_RDONLY:
		access |= windows.GENERIC_READ
	default:
		return nil, nil, errors.New("invalid daemon file access mode")
	}
	creation := uint32(windows.OPEN_EXISTING)
	if flags&os.O_CREATE != 0 {
		creation = windows.OPEN_ALWAYS
		if flags&os.O_EXCL != 0 {
			creation = windows.CREATE_NEW
		}
	}
	sharing := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE)
	if !private {
		sharing |= windows.FILE_SHARE_DELETE
	}
	handle, err := windows.CreateFile(name, access, sharing, &security, creation,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, nil, &os.PathError{Op: "open daemon file", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(handle), path)
	keep := false
	defer func() {
		if !keep {
			_ = file.Close()
		}
	}()
	kind, err := windows.GetFileType(handle)
	if err != nil {
		return nil, nil, err
	}
	if kind != windows.FILE_TYPE_DISK {
		return nil, nil, errors.New("daemon file must be a regular disk file")
	}
	var metadata windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &metadata); err != nil {
		return nil, nil, err
	}
	if metadata.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 || metadata.NumberOfLinks != 1 {
		return nil, nil, errors.New("daemon file must be a regular file with one link and no reparse point")
	}
	info, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, errors.New("daemon file must be a regular file")
	}
	existing, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, nil, err
	}
	if err := validateWindowsDaemonSecurity(existing, user, defaultOwner, private); err != nil {
		return nil, nil, err
	}
	current, err := lstatOwnedPath(path)
	if err != nil || !os.SameFile(info, current) {
		return nil, nil, errors.New("daemon file path changed while opening it")
	}
	keep = true
	return file, info, nil
}

func validateWindowsDaemonSecurity(sd *windows.SECURITY_DESCRIPTOR, user, defaultOwner *windows.SID, private bool) error {
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || (!owner.Equals(user) && !owner.Equals(defaultOwner)) {
		return errors.New("daemon file must be owned by the current user")
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil {
		return errors.New("daemon file must have a restrictive access control list")
	}
	const unsafeAccess = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_WRITE_EA |
		windows.FILE_WRITE_ATTRIBUTES | windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER |
		windows.GENERIC_WRITE | windows.GENERIC_ALL | windows.MAXIMUM_ALLOWED
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 || ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return errors.New("daemon file has an unsupported access control entry")
		}
		// GetSecurityInfo returns a validated descriptor. Still bound the SID
		// to this ACE before inspecting its variable-length subauthorities.
		const sidOffset = unsafe.Offsetof(windows.ACCESS_ALLOWED_ACE{}.SidStart)
		if uintptr(ace.Header.AceSize) < sidOffset+8 {
			return errors.New("daemon file has a malformed access control entry")
		}
		sidBytes := (*[8]byte)(unsafe.Pointer(&ace.SidStart))
		if sidOffset+8+uintptr(sidBytes[1])*4 > uintptr(ace.Header.AceSize) {
			return errors.New("daemon file has a malformed access control SID")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() {
			return errors.New("daemon file has an invalid access control SID")
		}
		// SYSTEM and administrators already have privileged filesystem access,
		// like root on Unix. Ordinary users may only read a compatible PID file.
		if sid.Equals(user) || sid.Equals(defaultOwner) || sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
			continue
		}
		if (private && ace.Mask != 0) || uint32(ace.Mask)&unsafeAccess != 0 {
			return errors.New("daemon file grants unsafe access to another user")
		}
	}
	return nil
}

func windowsDaemonIdentity() (*windows.SID, *windows.SID, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, nil, err
	}
	// Existing files made by elevated processes can use the token's default
	// owner group. Accept that exact owner, not arbitrary foreign groups.
	var size uint32
	if err := windows.GetTokenInformation(token, windows.TokenOwner, nil, 0, &size); !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
		return nil, nil, fmt.Errorf("querying daemon token owner: %w", err)
	}
	wordSize := uint32(unsafe.Sizeof(uintptr(0)))
	if size < wordSize {
		return nil, nil, errors.New("invalid daemon token owner size")
	}
	buffer := make([]uintptr, (size+wordSize-1)/wordSize)
	if err := windows.GetTokenInformation(token, windows.TokenOwner, (*byte)(unsafe.Pointer(&buffer[0])), size, &size); err != nil {
		return nil, nil, err
	}
	owner := *(**windows.SID)(unsafe.Pointer(&buffer[0]))
	if owner == nil || !owner.IsValid() {
		return nil, nil, errors.New("invalid daemon token owner")
	}
	owner, err = owner.Copy()
	if err != nil {
		return nil, nil, err
	}
	return user.User.Sid, owner, nil
}
