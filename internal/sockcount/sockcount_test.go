//go:build linux || darwin

package sockcount_test

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sockcount"
)

// pair is a sender connected over loopback to a peer that reads only when the
// case says so.
func pair(t *testing.T) (sender, peer net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()
	sender, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sender.Close() })
	peer, ok := <-accepted
	if !ok {
		t.Fatal("the peer was never accepted")
	}
	t.Cleanup(func() { _ = peer.Close() })
	return sender, peer
}

// fill writes to sender as much as the two sockets take while the peer reads
// nothing, and answers how much: the write ends at its deadline with the rest
// unsent, every byte it wrote held in the sender's socket or taken by the
// peer's kernel and none read.
func fill(t *testing.T, sender net.Conn) int {
	t.Helper()
	if err := sender.SetWriteDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	sent, _ := sender.Write(make([]byte, 4<<20))
	if sent == 0 {
		t.Fatal("the sockets took none of four mebibytes")
	}
	return sent
}

// until polls cond until it holds, failing the case once a bound far past any
// a loopback socket takes has passed.
func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	bound := time.NewTimer(10 * time.Second) //fixedwait:liveness
	defer bound.Stop()
	poll := time.NewTicker(10 * time.Millisecond) //fixedwait:liveness
	defer poll.Stop()
	for !cond() {
		select {
		case <-bound.C:
			t.Fatalf("still waiting for %s", what)
		case <-poll.C:
		}
	}
}

// THE SOCKET'S UNACKNOWLEDGED BYTES ARE READ ON THIS PLATFORM — one of the two
// the engine builds for — because a write's watch that could not read them
// would end every write at the floor (internal/objstore/s3obj): bytes written
// to a peer that reads nothing sit in the socket unacknowledged, and once the
// peer has read them all the count falls.
//
// Mutant: the platform's reading answers [sockcount.ErrUncountable], or a
// constant: red at the first reading, or at the fall.
func TestTheSocketsUnacknowledgedBytesAreReadOnThisPlatform(t *testing.T) {
	t.Parallel()
	sender, peer := pair(t)
	sent := fill(t, sender)
	held, err := sockcount.Unacked(sender)
	if err != nil {
		t.Fatalf("the socket's unacknowledged bytes cannot be read on this platform: %v", err)
	}
	if held <= 0 {
		t.Fatalf("%d bytes sent to a peer reading nothing leave %d unacknowledged; want some", sent, held)
	}
	if _, err := io.ReadFull(peer, make([]byte, sent)); err != nil {
		t.Fatal(err)
	}
	until(t, "the unacknowledged count to fall once the peer read everything", func() bool {
		now, err := sockcount.Unacked(sender)
		return err == nil && now < held
	})
}

// THE BYTES THE FAR END ACKNOWLEDGED ARE READ ON THIS PLATFORM, and they are
// the far end's, never the writer's: while the peer reads nothing, what its
// kernel took is acknowledged and the rest of what was written is not — so
// the count stands below what was written — and once the peer has read it
// all, every byte written is acknowledged and none more. A link's meter
// (internal/queue/jetstream) reads this to tell a link that carries what it
// is handed from one that carries nothing, and a count of what was written
// would say a dead link carried it. Counted from a reading taken before
// anything was written, as every reader counts it.
//
// Mutant: Acked answers written (no send queue, no kernel count): red while
// the peer reads nothing. Mutant: Acked answers zero: red at the end.
func TestTheBytesTheFarEndAcknowledgedAreReadOnThisPlatform(t *testing.T) {
	t.Parallel()
	sender, peer := pair(t)
	base, err := sockcount.Acked(sender, 0)
	if err != nil {
		t.Fatalf("the socket's acknowledged bytes cannot be read on this platform: %v", err)
	}
	if base > 1 {
		t.Fatalf("a socket nothing was written to reads %d acknowledged, past its handshake's one", base)
	}
	sent := fill(t, sender)
	acked, err := sockcount.Acked(sender, uint64(sent))
	if err != nil {
		t.Fatalf("the socket's acknowledged bytes cannot be read on this platform: %v", err)
	}
	acked -= base
	if acked >= uint64(sent) {
		t.Fatalf("%d bytes sent to a peer that reads nothing, its window shut, read as %d acknowledged; "+
			"want fewer than were sent", sent, acked)
	}
	if _, err := io.ReadFull(peer, make([]byte, sent)); err != nil {
		t.Fatal(err)
	}
	until(t, "every byte written to be acknowledged once the peer read them all", func() bool {
		now, err := sockcount.Acked(sender, uint64(sent))
		if err != nil {
			t.Fatalf("read the acknowledged bytes: %v", err)
		}
		now -= base
		if now > uint64(sent) {
			t.Fatalf("%d bytes acknowledged of %d written", now, sent)
		}
		return now == uint64(sent)
	})
}

// A CONNECTION WITH NO SOCKET UNDER IT IS UNCOUNTABLE, never read as zero: a
// reader that took zero for a count would read an in-memory pipe as a link
// that carries nothing.
func TestAConnectionWithNoSocketIsUncountable(t *testing.T) {
	t.Parallel()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if _, err := sockcount.Unacked(a); !errors.Is(err, sockcount.ErrUncountable) {
		t.Errorf("Unacked on a pipe = %v, want ErrUncountable", err)
	}
	if _, err := sockcount.Acked(a, 0); !errors.Is(err, sockcount.ErrUncountable) {
		t.Errorf("Acked on a pipe = %v, want ErrUncountable", err)
	}
}
