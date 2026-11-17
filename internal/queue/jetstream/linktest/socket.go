//go:build linux || darwin

package linktest

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// pin asks the kernel for a receive and a send buffer of [SocketBuffer] on the
// socket c is the descriptor of: a listener's before it listens, so every
// connection it accepts negotiates its window for it, and a dialled socket's
// before it connects. Setting either turns off its autotuning (Linux's
// SOCK_RCVBUF_LOCK and SOCK_SNDBUF_LOCK), which is the point.
func pin(_, _ string, c syscall.RawConn) error {
	var err error
	if cerr := c.Control(func(fd uintptr) {
		if err = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, SocketBuffer); err != nil {
			return
		}
		err = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_SNDBUF, SocketBuffer)
	}); cerr != nil {
		return cerr
	}
	return err
}

// granted is the receive and send buffers the kernel holds the socket c is
// the descriptor of to: Linux doubles what was asked, for its own bookkeeping
// of each buffer's bytes, and holds the socket to that from then on; darwin
// grants what was asked and then, on a connected socket, rounds each buffer
// up to a whole number of the connection's segments as its handshake settles
// them (XNU's tcp_mss: 65,536 asked becomes about 81.7 kB on loopback, whose
// MTU is 16,384), on the socket accepted and the one dialled alike, never on
// a listener ([Link.Buffers]).
func granted(c syscall.RawConn) (Buffers, error) {
	var (
		got Buffers
		err error
	)
	if cerr := c.Control(func(fd uintptr) {
		if got.Receive, err = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF); err != nil {
			return
		}
		got.Send, err = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_SNDBUF)
	}); cerr != nil {
		return Buffers{}, cerr
	}
	return got, err
}
