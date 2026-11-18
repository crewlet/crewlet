package linktest

import (
	"net"
	"sync/atomic"
	"syscall"
	"testing"
)

// askedBuffer is what the relay asks the kernel for of each socket buffer:
// stated here rather than read off the package, so a relay that asks any
// other size is red.
const askedBuffer = 64 << 10

// target is a broker stand-in on a loopback port: it reads everything sent
// on each connection it takes, counting, and sends reply bytes back on each.
type target struct {
	ln       net.Listener
	received atomic.Int64
}

// newTarget listens until the case ends, sending reply bytes back on every
// connection it takes.
func newTarget(t *testing.T, reply int) *target {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind the target: %v", err)
	}
	tg := &target{ln: ln}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if reply > 0 {
				go func() { _, _ = c.Write(make([]byte, reply)) }()
			}
			go func() {
				defer func() { _ = c.Close() }()
				buf := make([]byte, 64<<10)
				for {
					n, err := c.Read(buf)
					tg.received.Add(int64(n))
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})
	return tg
}

// raw is c's descriptor, for a case to read its socket through.
func raw(t *testing.T, c net.Conn) syscall.RawConn {
	t.Helper()
	sc, ok := c.(syscall.Conn)
	if !ok {
		t.Fatalf("%T carries no socket", c)
	}
	r, err := sc.SyscallConn()
	if err != nil {
		t.Fatalf("read %T's socket: %v", c, err)
	}
	return r
}

// buffersOf is what the kernel holds c's socket's buffers to.
func buffersOf(t *testing.T, c net.Conn) Buffers {
	t.Helper()
	got, err := granted(raw(t, c))
	if err != nil {
		t.Fatalf("read the socket's buffers: %v", err)
	}
	return got
}

// queued is what c's socket holds that the far end has not acknowledged, and
// the window the far end last advertised.
func queued(t *testing.T, c net.Conn) (int, uint32) {
	t.Helper()
	var (
		unacked int
		window  uint32
		qerr    error
	)
	if err := raw(t, c).Control(func(fd uintptr) { unacked, window, qerr = queuedOn(fd) }); err != nil {
		t.Fatalf("read the sender's socket: %v", err)
	}
	if qerr != nil {
		t.Fatalf("read the sender's socket: %v", qerr)
	}
	return unacked, window
}

// onlyPair is the one connection a link carries, once it carries it.
func (l *Link) onlyPair(t *testing.T) *pair {
	t.Helper()
	var p *pair
	eventually(t, "the link carrying the connection", func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		for q := range l.pairs {
			p = q
		}
		return len(l.pairs) == 1
	})
	return p
}

// EVERY SOCKET A LINK CARRIES OVER HOLDS THE BUFFERS IT REPORTS, the one it
// accepted from the node and the one it dialled to the broker, each way, once
// a bulk has crossed each way (what an unpinned buffer grows with): what the
// kernel granted for the size the relay asks. Granted is at least the ask and
// at most twice it (Linux doubles it, darwin rounds up to whole segments,
// [Link.Buffers]).
//
// Red first: a relay that pinned nothing reported Linux's unpinned send buffer
// of 16 KiB, a quarter of the ask. Mutants: the dialled socket left unpinned;
// the receive buffer left unpinned, and the bulk grows it past what was
// granted (on every link that reads as fast as bytes come; a first-in,
// first-out link reads too slowly for it to grow).
func TestEverySocketALinkCarriesOverHoldsTheBuffersItReports(t *testing.T) {
	// bulk is what crosses each way before the sockets are read: enough for
	// an unpinned receive buffer to grow, as Linux's grows with the traffic
	// its reader keeps up with.
	const bulk = 4 << 20
	for _, kind := range kinds {
		t.Run(kind.name, func(t *testing.T) {
			tg := newTarget(t, bulk)
			l := kind.open(t, tg.ln.Addr().String())
			node, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", l.Addr())
			if err != nil {
				t.Fatalf("dial the link: %v", err)
			}
			t.Cleanup(func() { _ = node.Close() })
			back := tally(node)
			if _, err := node.Write(make([]byte, bulk)); err != nil {
				t.Fatalf("send the bulk across the link: %v", err)
			}
			eventually(t, "the bulk crossed each way", func() bool {
				return tg.received.Load() == bulk && back.Load() == bulk
			})
			p := l.onlyPair(t)
			got := l.Buffers()
			for way, n := range map[string]int{"receive": got.Receive, "send": got.Send} {
				if n < askedBuffer || n > 2*askedBuffer {
					t.Errorf("the link reports a %s buffer of %d bytes, want between %d and %d: granted for the %d it asks",
						way, n, askedBuffer, 2*askedBuffer, askedBuffer)
				}
			}
			for side, c := range map[string]net.Conn{"accepted from the node": p.in, "dialled to the broker": p.out} {
				if held := buffersOf(t, c); held != got {
					t.Errorf("the socket %s holds buffers of %+v, want the %+v the link reports", side, held, got)
				}
			}
		})
	}
}

// A BLACKHOLED CONNECTION'S SENDER HAS NO MORE ACKNOWLEDGED THAN THE LINK READ
// AND ITS PINNED RECEIVE BUFFER: a node's kernel counts a byte as delivered
// once the relay's kernel takes it, so what a sender's socket says it
// delivered (written less what it holds) is at most what the link read plus
// [Buffers.Receive], and once the relay's window has shut, the link's read plus
// what waits unread in its socket. The link reads none of it while blackholed
// and carries every byte on once mended, after a bulk has crossed first.
//
// Red first: a Blackhole that read and held read on, megabytes past where it
// began. Mutants: the listener left unpinned, so the grown receive buffer
// passes the reported one (not on a first-in, first-out link, which reads too
// slowly for it to grow); Blackhole reads and holds, and the link's read moves.
func TestABlackholedSenderHasNoMoreAcknowledgedThanTheLinkReadAndItsBuffer(t *testing.T) {
	const (
		account = "BULK"
		// warm is what crosses before the blackhole: enough for an
		// unpinned receive buffer to grow, as Linux's grows with the
		// traffic it reads.
		warm = 4 << 20
		// bulk is what the sender writes into the blackhole: more than
		// its own send buffer, the relay's receive buffer and the most
		// an unpinned one grows to (6 MiB, Linux's tcp_rmem), so its
		// window shuts whatever the relay's kernel would take.
		bulk = 8 << 20
		// piece is how much the sender writes at a time.
		piece = 16 << 10
	)
	connect := []byte(`CONNECT {"remote_account":"` + account + `"}` + "\r\n")
	for _, kind := range kinds {
		t.Run(kind.name, func(t *testing.T) {
			tg := newTarget(t, 0)
			l := kind.open(t, tg.ln.Addr().String())
			// THE SENDER'S OWN BUFFERS ARE PINNED TOO, so its socket fills
			// at a size it states rather than one it grows to.
			node, err := (&net.Dialer{Control: pin}).DialContext(t.Context(), "tcp", l.Addr())
			if err != nil {
				t.Fatalf("dial the link: %v", err)
			}
			t.Cleanup(func() { _ = node.Close() })
			if _, err := node.Write(connect); err != nil {
				t.Fatalf("bind the connection: %v", err)
			}
			if _, err := node.Write(make([]byte, warm)); err != nil {
				t.Fatalf("send the bulk ahead of the blackhole: %v", err)
			}
			before := int64(len(connect) + warm)
			eventually(t, "the link reading what crossed ahead of the blackhole", func() bool {
				return l.Count(account, ToBroker).Read == before
			})
			l.Blackhole(account)
			read := l.Count(account, ToBroker).Read
			if read != before {
				t.Fatalf("the link read %d bytes ahead of the blackhole, want %d", read, before)
			}

			var written atomic.Int64
			wrote := make(chan error, 1)
			go func() {
				buf := make([]byte, piece)
				for sent := 0; sent < bulk; sent += piece {
					if _, err := node.Write(buf); err != nil {
						wrote <- err
						return
					}
					written.Add(piece)
				}
				wrote <- nil
			}()
			most := l.Buffers().Receive
			eventually(t, "the blackholed link's window shut", func() bool {
				// WRITTEN FIRST, then the socket: a piece written between
				// the two readings is in the socket and not yet counted,
				// so delivered is never more than the sender's truth.
				w := written.Load()
				unacked, window := queued(t, node)
				delivered := before + w - int64(unacked)
				if got := l.Count(account, ToBroker).Read; got != read {
					t.Fatalf("the blackholed link read %d bytes, %d when the blackhole began: it reads none", got, read)
				}
				if delivered > read+int64(most) {
					t.Fatalf("the sender's socket says it delivered %d bytes, want at most the %d the link read and its pinned receive buffer's %d",
						delivered, read, most)
				}
				return window == 0
			})
			// SHUT, the relay's kernel has taken what it will: every byte
			// the sender's socket says it delivered is one the link read
			// or one waiting unread in the link's socket, which holds
			// some and no more than its pinned buffer.
			unread, err := l.Unread(account, ToBroker)
			if err != nil {
				t.Fatalf("read the link's socket: %v", err)
			}
			if unread == 0 || unread > most {
				t.Fatalf("%d bytes wait unread in the link's socket once its window shut, want some and at most its pinned receive buffer's %d",
					unread, most)
			}
			// WRITTEN FIRST here too: the writer may still be filling its
			// own send buffer, and a piece it finishes between the two
			// readings must not count as delivered.
			w := written.Load()
			unacked, _ := queued(t, node)
			if delivered := before + w - int64(unacked); delivered > read+int64(unread) {
				t.Fatalf("the sender's socket says it delivered %d bytes, want at most the %d the link read and the %d waiting unread",
					delivered, read, unread)
			}

			l.Mend(account)
			total := before + bulk
			// ON THE LINK'S COUNT AS WELL AS THE TARGET'S: the relay counts
			// a piece carried once its write returns, which may be after
			// the target has read it. And before the writer's own end,
			// which a mend that never read again would leave waiting.
			eventually(t, "every byte carried on once mended", func() bool {
				return tg.received.Load() == total && l.Count(account, ToBroker).Carried == total
			})
			if err := <-wrote; err != nil {
				t.Fatalf("send into the mended link: %v", err)
			}
			if got := l.Count(account, ToBroker); got.Read != total || got.Carried != total {
				t.Fatalf("the mended link read and carried %+v, want %d of each: every byte", got, total)
			}
		})
	}
}
