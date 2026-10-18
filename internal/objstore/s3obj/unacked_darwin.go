//go:build darwin

package s3obj

import "golang.org/x/sys/unix"

// socketUnacked is SO_NWRITE: the bytes in the socket's send buffer, which
// holds a byte until the far end has acknowledged it.
func socketUnacked(fd uintptr) (int, error) {
	return unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_NWRITE)
}
