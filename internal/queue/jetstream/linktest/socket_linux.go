//go:build linux

package linktest

import "golang.org/x/sys/unix"

// unreadOn is SIOCINQ: the bytes the socket fd has taken in order that its
// reader has not yet read.
func unreadOn(fd uintptr) (int, error) { return unix.IoctlGetInt(int(fd), unix.SIOCINQ) }
