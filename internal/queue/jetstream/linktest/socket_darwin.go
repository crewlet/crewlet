//go:build darwin

package linktest

import "golang.org/x/sys/unix"

// unreadOn is SO_NREAD: the bytes in the socket fd's receive buffer that its
// reader has not yet read.
func unreadOn(fd uintptr) (int, error) {
	return unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_NREAD)
}
