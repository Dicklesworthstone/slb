package daemon

import (
	"errors"
	"net"
	"unsafe"

	"golang.org/x/sys/windows"
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
	const sioAFUnixGetPeerPID = 0x58000100 // _WSAIOR(IOC_VENDOR, 256), Windows SDK shared/afunix.h.
	var pid, received uint32
	var peerErr error
	if err := raw.Control(func(fd uintptr) {
		peerErr = windows.WSAIoctl(windows.Handle(fd), sioAFUnixGetPeerPID, nil, 0,
			(*byte)(unsafe.Pointer(&pid)), uint32(unsafe.Sizeof(pid)), &received, nil, 0)
	}); err != nil {
		return 0, err
	}
	if peerErr != nil {
		return 0, peerErr
	}
	if received != uint32(unsafe.Sizeof(pid)) || pid == 0 {
		return 0, errors.New("invalid daemon peer PID")
	}
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0, err
	}
	defer func() { _ = windows.CloseHandle(process) }()
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return 0, err
	}
	defer func() { _ = token.Close() }()
	peer, err := token.GetTokenUser()
	if err != nil {
		return 0, err
	}
	current, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return 0, err
	}
	if !peer.User.Sid.Equals(current.User.Sid) {
		return 0, errors.New("daemon peer is not the current OS user")
	}
	return int(pid), nil
}
