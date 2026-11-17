// Package sockcount reads what a TCP socket's kernel has done with the bytes
// written to it: how many it still holds that the far end has not
// acknowledged ([Unacked]), and how many the far end has acknowledged since
// the connection opened ([Acked]).
//
// # Why a node reads its own socket
//
// Every counter a client library or a broker offers moves only when a whole
// message has crossed: nats-server adds a connection's bytes in once its read
// loop has parsed a message whole and counts them out when it queues one, and
// nats.go counts a message's bytes as it hands it on. A message of megabytes
// crossing a link at the slowest pace the engine allows takes minutes, and
// for all of them those counters stand still. The kernel's do not: it
// acknowledges a segment at a time. So the two places that have to tell a
// link that is carrying something from one that carries nothing read the
// socket — the S3 backend's watch on an upload (internal/objstore/s3obj) and
// the queue's meter on a node's data link (internal/queue/jetstream).
//
// ONE COPY, because the two had been one function each, and a platform's
// reading fixed in one would have stayed wrong in the other.
//
// # What it cannot see
//
// A proxy that terminates TCP in front of the far end — a service mesh's
// sidecar, a load balancer that buffers — acknowledges what it has taken, not
// what the far end has. From the node's socket a write it buffers moves at
// once and then stands still while the proxy drains it, and a far end that
// stopped reading looks, for as long as the proxy's buffer takes, like one
// that reads. Each reader states what that costs it.
//
// # Where it reads
//
// Through the connection's own descriptor, held open for the call by
// SyscallConn().Control, so a reading racing the connection's close fails
// rather than reads another socket's count; a TLS connection, and any
// wrapper that names the connection under it (NetConn), is read through the
// TCP socket beneath. On Linux and darwin, the two platforms the engine
// builds for (internal/store/platform.go); elsewhere every reading is
// [ErrUncountable], so a build for another platform reaches the store's own
// message naming the ones it supports.
package sockcount

import (
	"errors"
	"net"
	"syscall"
)

// ErrUncountable is a connection whose counts this node cannot read: no TCP
// socket under it, or a platform without the call.
var ErrUncountable = errors.New("sockcount: this connection's socket counts cannot be read")

// Unacked is how many bytes c's socket holds that the far end has not
// acknowledged — those sent and unacknowledged, and those not yet sent:
// `SIOCOUTQ` on Linux, the send buffer's `SO_NWRITE` on darwin, each holding
// a byte until the far end's kernel takes it.
func Unacked(c net.Conn) (int, error) {
	var n int
	err := control(c, func(fd uintptr) (err error) {
		n, err = socketUnacked(fd)
		return err
	})
	return n, err
}

// Acked is how many bytes the far end of c's socket has acknowledged, in the
// kernel's reckoning, written being how many bytes have been written to c
// (beneath any TLS, so the count of the socket's own bytes).
//
// A COUNT FROM A BASELINE, NEVER FROM ZERO: Linux counts the sequence space
// the far end acknowledged, and a connection this end OPENED had its SYN
// acknowledged too, one more than its bytes, while one it accepted did not
// (measured: 2,633,836 acknowledged of 2,633,835 written on a dialled
// socket). So a reader takes a reading before it writes anything and counts
// from it.
//
// LINUX COUNTS IT ITSELF: TCP_INFO's tcpi_bytes_acked, the kernel's
// cumulative count, read in one call, so it never steps back and written is
// not read. darwin keeps no such count, so there it is written less what the
// send buffer holds (`SO_NWRITE`): two readings at two instants, which can
// step back by what a write added between them — a caller that holds the
// count over time keeps the largest it has read. And it lags by every byte of
// a Write still in the socket: the send buffer holds them before the Write
// returns, and a caller counts them into written only once it has, so a
// caller that hands its socket one Write of megabytes reads nothing
// acknowledged until that Write ends — it writes in chunks it counts as each
// returns, and lags by one chunk at most.
func Acked(c net.Conn, written uint64) (uint64, error) {
	var n uint64
	err := control(c, func(fd uintptr) (err error) {
		n, err = socketAcked(fd, written)
		return err
	})
	return n, err
}

// control runs read on the descriptor of the TCP socket under c, held open
// for the call.
func control(c net.Conn, read func(fd uintptr) error) error {
	for {
		inner, ok := c.(interface{ NetConn() net.Conn })
		if !ok {
			break
		}
		c = inner.NetConn()
	}
	sc, ok := c.(syscall.Conn)
	if !ok {
		return ErrUncountable
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return err
	}
	var rerr error
	if err := raw.Control(func(fd uintptr) { rerr = read(fd) }); err != nil {
		return err
	}
	return rerr
}
