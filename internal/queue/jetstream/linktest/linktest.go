// Package linktest is a slow link a test puts between a node and the broker it
// reaches across one — a leaf's link to its member, a client's dial to an
// external server — so a case can show what a write waits for when its bytes
// take a while to cross, on a loopback interface that otherwise carries them
// at once.
//
// ONE RELAY FOR EVERY PACKAGE whose writes cross a link: the queue's
// (internal/queue/jetstream) and the coordination store's (internal/coord/kv)
// reach the members over the same leaf link in production, and a relay
// written twice would pace them apart.
package linktest

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// Link is a TCP relay a case puts between a node and the broker it
// reaches across a link — a leaf and its member's leaf listener, a client and
// an external server — which carries what the NODE sends at rate bytes a
// second and what the broker sends back at once: the direction a write's
// bytes cross is the slow one, and its acknowledgement comes straight back.
//
// # It reads what the node sends as fast as it comes
//
// and holds what the link has not carried yet, so the node's own writes never
// wait on it: what is paced is the DELIVERY to the broker, as on a slow
// network the sender's buffers take its bytes and the receiver hears them at
// the link's pace. A relay that paced its reads instead would push back into
// the sender's socket, and a broker whose writes stop moving closes its link
// once its own write deadline passes — so a case meant to show a write waiting
// on a slow link would show a link torn down instead, and a node's client,
// whose library publishes from a buffer of its own, would block in its write
// rather than wait for its acknowledgement.
//
// It ends with the case that started it: its listener, every connection it
// carries and every goroutine it runs.
type Link struct {
	ln     net.Listener
	target string
	// rate is how many bytes a second the link carries towards the broker.
	rate int64
	// done is closed when the case ends, which ends every delivery waiting
	// on the link's pace.
	done chan struct{}
	wg   sync.WaitGroup

	mu    sync.Mutex
	shut  bool
	conns []net.Conn
}

// Slow listens on a loopback port and carries every connection made to it on
// to target, at rate bytes a second towards target, until the case ends.
func Slow(t testing.TB, target string, rate int64) *Link {
	t.Helper()
	if rate <= 0 {
		t.Fatalf("a slow link carries %d bytes a second, which carries nothing", rate)
	}
	// THE CASE'S CONTEXT, which ends as the case does, just before its
	// cleanups (this link's close among them): a connection it dialled
	// outlives it, as a dialled connection does its dial's context.
	ctx := t.Context()
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind the slow link: %v", err)
	}
	l := &Link{ln: ln, target: target, rate: rate, done: make(chan struct{})}
	l.wg.Go(func() { l.accept(ctx) })
	t.Cleanup(l.close)
	return l
}

// Addr is the host and port the node dials to cross the link.
func (l *Link) Addr() string { return l.ln.Addr().String() }

// accept carries every connection the listener takes, until it is closed,
// dialling the target for each in ctx.
func (l *Link) accept(ctx context.Context) {
	for {
		in, err := l.ln.Accept()
		if err != nil {
			return
		}
		if !l.track(in) {
			_ = in.Close()
			continue
		}
		l.wg.Go(func() { l.carry(ctx, in) })
	}
}

// track records c for the case's end to close, and refuses it once that end
// has begun, so no connection outlives the link.
func (l *Link) track(c net.Conn) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.shut {
		return false
	}
	l.conns = append(l.conns, c)
	return true
}

// carry relays in to a connection of its own to the target, dialled in ctx,
// both ways, until either side ends, and then closes both.
func (l *Link) carry(ctx context.Context, in net.Conn) {
	out, err := (&net.Dialer{}).DialContext(ctx, "tcp", l.target)
	if err != nil {
		_ = in.Close()
		return
	}
	if !l.track(out) {
		_ = in.Close()
		_ = out.Close()
		return
	}
	// EITHER SIDE ENDING ENDS THE OTHER: a closed connection fails the
	// read or the write each of the three is blocked in.
	closeBoth := func() {
		_ = in.Close()
		_ = out.Close()
	}
	held := &backlog{more: make(chan struct{}, 1)}
	var wg sync.WaitGroup
	wg.Go(func() {
		defer closeBoth()
		held.fill(in)
	})
	wg.Go(func() {
		defer closeBoth()
		held.deliver(out, l.rate, l.done)
	})
	wg.Go(func() {
		defer closeBoth()
		_, _ = io.Copy(in, out)
	})
	wg.Wait()
}

// close ends the link: no connection is taken or carried after it, and it
// returns once every goroutine the link ran has.
func (l *Link) close() {
	_ = l.ln.Close()
	l.mu.Lock()
	l.shut = true
	conns := l.conns
	l.mu.Unlock()
	close(l.done)
	for _, c := range conns {
		_ = c.Close()
	}
	l.wg.Wait()
}

// backlog is what the node has sent and the link has not carried yet, in the
// order it was sent.
type backlog struct {
	mu     sync.Mutex
	chunks [][]byte
	// ended is set once the node's side has nothing more to send.
	ended bool
	// more is signalled whenever a chunk is added or the node's side ends.
	more chan struct{}
}

// fill reads src as fast as it sends, holding each read, until src ends.
func (b *backlog) fill(src io.Reader) {
	defer b.push(nil, true)
	for {
		buf := make([]byte, 64<<10)
		n, err := src.Read(buf)
		if n > 0 {
			b.push(buf[:n], false)
		}
		if err != nil {
			return
		}
	}
}

func (b *backlog) push(chunk []byte, ended bool) {
	b.mu.Lock()
	if chunk != nil {
		b.chunks = append(b.chunks, chunk)
	}
	b.ended = b.ended || ended
	b.mu.Unlock()
	select {
	case b.more <- struct{}{}:
	default:
	}
}

// next is the oldest chunk not yet carried, waiting for one; false once the
// node's side has ended with nothing left, or done is closed.
func (b *backlog) next(done <-chan struct{}) ([]byte, bool) {
	for {
		b.mu.Lock()
		if len(b.chunks) > 0 {
			chunk := b.chunks[0]
			b.chunks = b.chunks[1:]
			b.mu.Unlock()
			return chunk, true
		}
		ended := b.ended
		b.mu.Unlock()
		if ended {
			return nil, false
		}
		select {
		case <-b.more:
		case <-done:
			return nil, false
		}
	}
}

// deliver writes each chunk to dst once a link carrying rate bytes a second
// would have carried it: a chunk's own time after the one before it was
// written, so the link never carries faster than rate, and an idle link lets
// no burst through at once.
func (b *backlog) deliver(dst io.Writer, rate int64, done <-chan struct{}) {
	for {
		chunk, ok := b.next(done)
		if !ok {
			return
		}
		crossing := time.NewTimer(time.Duration(len(chunk)) * time.Second / time.Duration(rate)) //fixedwait:simulated a link carrying the node's bytes at its rate
		select {
		case <-crossing.C:
		case <-done:
			crossing.Stop()
			return
		}
		if _, err := dst.Write(chunk); err != nil {
			return
		}
	}
}
