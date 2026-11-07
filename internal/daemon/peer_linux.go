package daemon

import (
	"errors"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// SO_PEERCRED authenticates the connected process through the kernel, rather
// than trusting the socket pathname or the daemon's self-reported status PID.
func authenticatedPeerPID(conn net.Conn) (int, error) {
	local, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, errors.New("project daemon requires a Unix connection")
	}
	raw, err := local.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, credErr
	}
	if cred.Uid != uint32(os.Geteuid()) || cred.Pid <= 0 {
		return 0, errors.New("daemon peer is not the current OS user")
	}
	return int(cred.Pid), nil
}
