//go:build linux

package sockcount

import "golang.org/x/sys/unix"

// socketUnacked is SIOCOUTQ: the bytes the socket holds that the far end has
// not acknowledged — sent and unacknowledged, and not yet sent.
func socketUnacked(fd uintptr) (int, error) { return unix.IoctlGetInt(int(fd), unix.SIOCOUTQ) }

// socketAcked is TCP_INFO's tcpi_bytes_acked (Linux 4.1 and later): the bytes
// the far end has acknowledged since the connection opened, counted by the
// kernel. written is not needed here.
func socketAcked(fd uintptr, _ uint64) (uint64, error) {
	info, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
	if err != nil {
		return 0, err
	}
	return info.Bytes_acked, nil
}
