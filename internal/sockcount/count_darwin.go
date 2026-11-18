//go:build darwin

package sockcount

import "golang.org/x/sys/unix"

// socketUnacked is SO_NWRITE: the bytes in the socket's send buffer, which
// holds a byte until the far end has acknowledged it.
func socketUnacked(fd uintptr) (int, error) {
	return unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_NWRITE)
}

// socketAcked is written less what the send buffer holds ([Acked]): darwin
// keeps no cumulative count of acknowledged bytes (TCP_CONNECTION_INFO's
// tcpi_snd_sbbytes is SO_NWRITE again).
func socketAcked(fd uintptr, written uint64) (uint64, error) {
	held, err := socketUnacked(fd)
	if err != nil {
		return 0, err
	}
	if uint64(held) >= written {
		return 0, nil
	}
	return written - uint64(held), nil
}
