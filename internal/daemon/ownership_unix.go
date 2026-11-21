//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package daemon

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"
)

func ownedByCurrentUser(_ string, info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func lstatOwnedPath(path string) (os.FileInfo, error) { return os.Lstat(path) }

func protectSocket(path string, original os.FileInfo) error {
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(original, current) || current.Mode()&os.ModeSocket == 0 {
		return errors.New("socket path changed before setting permissions")
	}
	return os.Chmod(path, 0600)
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	defer func() { _ = proc.Release() }()
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// openPIDFile never follows a symlink or blocks on a FIFO. Existing readable
// PID files remain compatible, but a foreign or writable-by-others file cannot
// be trusted to identify a daemon.
func openPIDFile(path string, flags int) (*os.File, os.FileInfo, error) {
	fd, err := syscall.Open(path, flags|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, nil, &os.PathError{Op: "open pid file", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || !ownedByCurrentUser(path, info) || info.Mode().Perm()&0022 != 0 || !ok || stat.Nlink != 1 {
			err = errors.New("pid file must be an owned regular file with one link and no group/other write permission")
		}
	}
	if err == nil {
		current, statErr := os.Lstat(path)
		if statErr != nil || !os.SameFile(info, current) {
			err = errors.New("pid file path changed while opening it")
		}
	}
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return file, info, nil
}

func openDiscoveryFile(path string, flags int) (*os.File, os.FileInfo, error) {
	file, info, err := openPIDFile(path, flags)
	if err != nil {
		return nil, nil, err
	}
	if err := validateRuntimePermissions(path, info, true); err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return file, info, nil
}

func validateRuntimePermissions(_ string, info os.FileInfo, private bool) error {
	mask := os.FileMode(0022)
	if private {
		mask = 0077
	}
	if info.Mode().Perm()&mask != 0 {
		return errors.New("unsafe daemon runtime permissions")
	}
	return nil
}

// Ancestors may be owned by this user or root. Shared writable directories
// require the sticky bit (e.g. /tmp). Linux user-private-group ancestors may
// also be group writable when the local account databases establish that the
// group belongs only to this user and no access ACL grants other identities
// access. The runtime files and .slb itself retain their stricter permissions.
// Resolve platform aliases such as /var before inspecting them; the socket
// itself is never followed as a symlink.
func validateRuntimeParents(directory string) error {
	remaining := 512
	// Read lazily, at most once for this traversal (including symlink targets).
	// Never cache membership across calls: an administrator can change it.
	privateGroup := sync.OnceValue(currentUserPrivateGroup)
	return validateRuntimeAncestors(directory, &remaining, privateGroup)
}

func validateRuntimeAncestors(directory string, remaining *int, privateGroup func() bool) error {
	for dir := filepath.Clean(directory); ; dir = filepath.Dir(dir) {
		*remaining--
		if *remaining < 0 {
			return errors.New("daemon runtime directory has too many links or ancestors")
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) {
			return errors.New("unsafe daemon runtime directory")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(dir)
			if err != nil {
				return err
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(dir), target)
			}
			// Check both the target chain and the lexical containing directory.
			// Resolving first alone could hide a writable alias directory.
			if err := validateRuntimeAncestors(target, remaining, privateGroup); err != nil {
				return err
			}
		} else if !info.IsDir() || !runtimeAncestorPermissions(dir, info, privateGroup) {
			return errors.New("unsafe daemon runtime directory")
		}
		if filepath.Dir(dir) == dir {
			return nil
		}
	}
}

func runtimeAncestorPermissions(path string, info os.FileInfo, privateGroup func() bool) bool {
	if info.Mode().Perm()&0022 == 0 || info.Mode()&os.ModeSticky != 0 {
		return true
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.Mode().Perm()&0002 == 0 &&
		stat.Uid == uint32(os.Geteuid()) && stat.Gid == uint32(os.Getegid()) &&
		privateGroup() && runtimeAncestorHasNoACL(path, info)
}

const maxLocalAccountBytes = 1 << 20

// currentUserPrivateGroup trusts the administrator's conventional UPG setup.
// It does not claim to enumerate credentials assigned directly by privileged
// processes or shadow-only group grants. Do not infer private membership from
// USER/LOGNAME, matching numeric IDs alone, or incomplete remote NSS results.
func currentUserPrivateGroup() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	nsswitch, err := readLocalAccountFile("/etc/nsswitch.conf")
	if err != nil {
		return false
	}
	passwd, err := readLocalAccountFile("/etc/passwd")
	if err != nil {
		return false
	}
	group, err := readLocalAccountFile("/etc/group")
	return err == nil && localUserPrivateGroup(uint32(os.Geteuid()), uint32(os.Getegid()), nsswitch, passwd, group)
}

func readLocalAccountFile(path string) (string, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return "", err
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !ok || !before.Mode().IsRegular() || stat.Uid != 0 || before.Mode().Perm()&0022 != 0 || before.Size() > maxLocalAccountBytes {
		return "", errors.New("unsafe local account database")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxLocalAccountBytes+1))
	if err != nil {
		return "", err
	}
	after, err := file.Stat()
	if err != nil {
		return "", err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !sameLocalAccountFile(before, after) || !sameLocalAccountFile(after, current) ||
		int64(len(data)) != after.Size() || len(data) > maxLocalAccountBytes ||
		!utf8.Valid(data) || strings.IndexByte(string(data), 0) >= 0 {
		return "", errors.New("local account database changed or is invalid")
	}
	return string(data), nil
}

func sameLocalAccountFile(before, after os.FileInfo) bool {
	beforeStat, beforeOK := before.Sys().(*syscall.Stat_t)
	afterStat, afterOK := after.Sys().(*syscall.Stat_t)
	return beforeOK && afterOK && os.SameFile(before, after) &&
		before.Mode() == after.Mode() && beforeStat.Uid == afterStat.Uid && beforeStat.Gid == afterStat.Gid &&
		before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
}

// localUserPrivateGroup mirrors _local_user_private_group in hook_runtime.py.
// Only files-only NSS can make these public account files authoritative. Other
// providers (including systemd userdb), NSS actions and ambiguous records fail
// closed instead of hiding additional primary or supplementary group members.
func localUserPrivateGroup(uid, gid uint32, nsswitch, passwd, group string) bool {
	for _, data := range []string{nsswitch, passwd, group} {
		if len(data) > maxLocalAccountBytes || !utf8.ValidString(data) || strings.IndexByte(data, 0) >= 0 {
			return false
		}
	}
	databases := make(map[string]bool)
	for _, line := range strings.Split(nsswitch, "\n") {
		line, _, _ = strings.Cut(line, "#")
		line = strings.Trim(line, " \t\r\v\f")
		// libc also recognizes database names terminated by whitespace. Do
		// not ignore a colonless later rule that can override a files entry.
		end := strings.IndexAny(line, ": \t\r\v\f")
		if end < 0 {
			end = len(line)
		}
		name := line[:end]
		if name != "passwd" && name != "group" && name != "initgroups" {
			continue
		}
		rest := strings.TrimLeft(line[end:], " \t\r\v\f")
		if !strings.HasPrefix(rest, ":") || databases[name] || strings.Trim(rest[1:], " \t\r\v\f") != "files" {
			return false
		}
		databases[name] = true
	}
	if !databases["passwd"] || !databases["group"] {
		return false
	}
	username := ""
	names := make(map[string]bool)
	primaryMembers := 0
	for _, line := range strings.Split(passwd, "\n") {
		line = strings.Trim(line, " \t\r\v\f")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) != 7 || !localAccountName(fields[0]) || names[fields[0]] {
			return false
		}
		names[fields[0]] = true
		userID, userOK := localAccountID(fields[2])
		groupID, groupOK := localAccountID(fields[3])
		if !userOK || !groupOK {
			return false
		}
		if groupID == gid {
			primaryMembers++
			if userID != uid {
				return false
			}
		}
		if userID == uid {
			if username != "" || groupID != gid {
				return false
			}
			username = fields[0]
		}
	}
	if username == "" || primaryMembers != 1 {
		return false
	}
	clear(names)
	foundGroup := false
	for _, line := range strings.Split(group, "\n") {
		line = strings.Trim(line, " \t\r\v\f")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) != 4 || !localAccountName(fields[0]) || names[fields[0]] {
			return false
		}
		names[fields[0]] = true
		groupID, ok := localAccountID(fields[2])
		if !ok {
			return false
		}
		var members []string
		if fields[3] != "" {
			members = strings.Split(fields[3], ",")
			for _, member := range members {
				if !localAccountName(member) {
					return false
				}
			}
		}
		if groupID != gid {
			if fields[0] == username {
				return false
			}
			continue
		}
		if foundGroup || fields[0] != username {
			return false
		}
		foundGroup = true
		password := fields[1]
		if password != "" && password != "x" && !strings.HasPrefix(password, "!") && !strings.HasPrefix(password, "*") {
			return false
		}
		for _, member := range members {
			if member != username {
				return false
			}
		}
	}
	return foundGroup
}

func localAccountName(name string) bool {
	return name != "" && name[0] != '+' && name[0] != '-' && !strings.ContainsAny(name, " \t\r\n\v\f,")
}

func localAccountID(value string) (uint32, bool) {
	if value == "" {
		return 0, false
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0, false
		}
	}
	id, err := strconv.ParseUint(value, 10, 32)
	return uint32(id), err == nil
}

func acquireFileLease(path string) (*fileLease, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("daemon lease path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	// NOFOLLOW rejects symlinks. NONBLOCK prevents a hostile FIFO from
	// hanging before we can check its type. Never truncate a contender's file.
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, fmt.Errorf("opening daemon lease: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	keep := false
	defer func() {
		if !keep {
			_ = file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ownedByCurrentUser(path, info) || info.Mode().Perm()&0077 != 0 || !ok || stat.Nlink != 1 {
		return nil, errors.New("daemon lease must be a private, owned regular file with one link")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("%w: %s", ErrDaemonOwned, path)
		}
		return nil, fmt.Errorf("locking daemon lease: %w", err)
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		return nil, errors.New("daemon lease path changed while acquiring ownership")
	}
	keep = true
	return &fileLease{file: file}, nil
}
