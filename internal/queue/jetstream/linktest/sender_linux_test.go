//go:build linux

package linktest

import "golang.org/x/sys/unix"

// queuedOn is the bytes the socket fd holds that the far end has not
// acknowledged, sent or not (SIOCOUTQ), and the window the far end last
// advertised (TCP_INFO's tcpi_snd_wnd): 0 once its receive buffer is full.
func queuedOn(fd uintptr) (unacked int, window uint32, err error) {
	if unacked, err = unix.IoctlGetInt(int(fd), unix.SIOCOUTQ); err != nil {
		return 0, 0, err
	}
	info, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
	if err != nil {
		return 0, 0, err
	}
	return unacked, info.Snd_wnd, nil
}
