//go:build darwin || freebsd

package daemon

import (
	"errors"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

func authenticatedPeerPID(conn net.Conn) (int, error) {
	local, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, errors.New("project daemon requires a Unix connection")
	}
	raw, err := local.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Xucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, credErr
	}
	if cred.Version != 0 || cred.Uid != uint32(os.Geteuid()) {
		return 0, errors.New("daemon peer is not the current OS user")
	}
	// These credentials authenticate UID, but do not include a portable PID.
	// The protected discovery record still binds the expected daemon PID.
	return 0, nil
}
