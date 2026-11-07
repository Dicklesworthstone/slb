//go:build !linux && !darwin && !freebsd && !windows

package daemon

import (
	"errors"
	"net"
)

func authenticatedPeerPID(net.Conn) (int, error) {
	return 0, errors.New("authenticated project socket discovery is unsupported on this platform")
}
