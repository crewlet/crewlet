//go:build darwin

package linktest

import "golang.org/x/sys/unix"

// queuedOn is the bytes the socket fd holds that the far end has not
// acknowledged, sent or not (SO_NWRITE), and the window the far end last
// advertised (TCP_CONNECTION_INFO's tcpi_snd_wnd): 0 once its receive buffer
// is full.
func queuedOn(fd uintptr) (unacked int, window uint32, err error) {
	if unacked, err = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_NWRITE); err != nil {
		return 0, 0, err
	}
	info, err := unix.GetsockoptTCPConnectionInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_CONNECTION_INFO)
	if err != nil {
		return 0, 0, err
	}
	return unacked, info.Snd_wnd, nil
}
