//go:build darwin || freebsd || openbsd || netbsd || dragonfly

package daemon

import "os"

// The private-group exception is Linux-only; other platforms retain the
// existing requirement that writable ancestors have the sticky bit.
func runtimeAncestorHasNoACL(string, os.FileInfo) bool { return false }
