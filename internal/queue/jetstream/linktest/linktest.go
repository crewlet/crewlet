// Package linktest is a slow link a test puts between a node and the broker it
// reaches across one — a leaf's link to its member, a client's dial to an
// external server — so a case can show what a write waits for when its bytes
// take a while to cross, and what a read waits for when its reply does, on a
// loopback interface that otherwise carries them at once.
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
// second, and what the broker sends back either at once ([Slow]) or at a rate
// of its own ([SlowEachWay]): a write's bytes cross towards the broker, and a
// read's reply — a stream leader's message of megabytes, base64-encoded a
// third larger — crosses back, on a real link as slowly. A case may also hold
// the broker's bytes back for a while ([Link.Delay]), as a reply is held on a
// slow link behind what was sent ahead of it. A link may instead LAG
// ([Lagging]): carry every byte at once but each a fixed while after it was
// sent, each way, as a loaded broker answers every request a while late.
//
// # It reads what each side sends as fast as it comes
//
// and holds what the link has not carried yet, so neither side's writes wait
// on it: what is paced is the DELIVERY to the other side, as on a slow network
// the sender's buffers take its bytes and the receiver hears them at the
// link's pace. A relay that paced its reads instead would push back into the
// sender's socket, and a broker whose writes stop moving closes its link once
// its own write deadline passes — so a case meant to show a write, or a read's
// reply, waiting on a slow link would show a link torn down instead, and a
// node's client, whose library publishes from a buffer of its own, would block
// in its write rather than wait for its acknowledgement.
//
// It ends with the case that started it: its listener, every connection it
// carries and every goroutine it runs.
type Link struct {
	ln     net.Listener
	target string
	// rate is how many bytes a second the link carries towards the broker.
	rate int64
	// back is how many bytes a second it carries back towards the node:
	// none for a link that carries them at once ([Slow]).
	back int64
	// lagging is a link that carries both ways at once but each chunk
	// [Link.Lag] after it was sent ([Lagging]); rate and back are then 0.
	lagging bool
	// done is closed when the case ends, which ends every delivery waiting
	// on the link's pace.
	done chan struct{}
	wg   sync.WaitGroup

	mu    sync.Mutex
	shut  bool
	conns []net.Conn
	// heldUntil is when what the broker sends stops being held back
	// ([Link.Delay]); zero for never held.
	heldUntil time.Time
	// lag is how long after it was sent a lagging link delivers a chunk
	// ([Link.Lag]).
	lag time.Duration
}

// Slow listens on a loopback port and carries every connection made to it on
// to target, at rate bytes a second towards target and what target sends back
// at once, until the case ends.
func Slow(t testing.TB, target string, rate int64) *Link {
	t.Helper()
	return open(t, target, rate, 0)
}

// SlowEachWay is [Slow] whose link carries what target sends back at rate
// bytes a second too: a link that is slow both ways, as a real one is, so a
// read whose reply carries a value of megabytes waits for it to cross as a
// write of the value does.
func SlowEachWay(t testing.TB, target string, rate int64) *Link {
	t.Helper()
	return open(t, target, rate, rate)
}

// Lagging listens on a loopback port and carries every connection made to it
// on to target, both ways, until the case ends: at once until the case sets a
// lag ([Link.Lag]), and from then on every chunk either side sends that lag
// after it was sent.
//
// A LAG, NOT A RATE, so what it plays is a loaded broker rather than a narrow
// link: every round trip costs twice the lag, every time, however much the
// two sides send, where a rate lets a stream of small long-poll requests
// build a backlog that grows. Set once the node has linked, since a link's
// own handshake keeps deadlines a lag of hundreds of milliseconds each way
// already misses, and what a lagging link is for is the traffic after it.
//
// AND SIZE EVERY WINDOW THE NODE WAITS IN FOR AN ANSWER TO THE ROUND TRIP: a
// window that ends before its answer crosses back does not wait for it, it
// loses it, so a case whose windows suit a loopback that answers at once
// shows answers lost, not answers late (jetstream's lagging leaf says which
// windows, and what it measured when they were not).
func Lagging(t testing.TB, target string) *Link {
	t.Helper()
	l := listen(t, target)
	l.lagging = true
	l.start(t)
	return l
}

// open is a link to target carrying rate bytes a second towards it and back
// bytes a second from it, none for at once.
func open(t testing.TB, target string, rate, back int64) *Link {
	t.Helper()
	if rate <= 0 {
		t.Fatalf("a slow link carries %d bytes a second, which carries nothing", rate)
	}
	l := listen(t, target)
	l.rate, l.back = rate, back
	l.start(t)
	return l
}

// listen binds a link to target on a loopback port, carrying nothing yet.
func listen(t testing.TB, target string) *Link {
	t.Helper()
	// THE CASE'S CONTEXT, which ends as the case does, just before its
	// cleanups (this link's close among them): a connection it dialled
	// outlives it, as a dialled connection does its dial's context.
	ctx := t.Context()
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind the link: %v", err)
	}
	return &Link{ln: ln, target: target, done: make(chan struct{})}
}

// start carries every connection the link takes, dialling the target for each
// in the case's context, until the case ends.
func (l *Link) start(t testing.TB) {
	ctx := t.Context()
	l.wg.Go(func() { l.accept(ctx) })
	t.Cleanup(l.close)
}

// Addr is the host and port the node dials to cross the link.
func (l *Link) Addr() string { return l.ln.Addr().String() }

// Delay holds back every byte the broker sends towards the node from now until
// d has passed, and then carries them on at once and in order: what a reply
// meets on a slow link behind a whole message the broker sent ahead of it —
// it was sent, and arrives late. What reached the link before Delay was
// called is carried at once, as before.
func (l *Link) Delay(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.heldUntil = time.Now().Add(d)
}

// Lag makes a lagging link ([Lagging]) deliver every chunk either side sends
// from now on d after it was sent; what was sent before is delivered at the
// lag it was sent under.
func (l *Link) Lag(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lag = d
}

// lagNow is the lag a chunk sent now is delivered at.
func (l *Link) lagNow() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lag
}

// held is how long what the broker sends now is still to be held back
// ([Link.Delay]).
func (l *Link) held() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return time.Until(l.heldUntil) //fixedwait:measure the instant a case's Delay ends, which the relay holds bytes to rather than decides by
}

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
	if l.lagging {
		// BOTH WAYS THROUGH A BACKLOG, each chunk stamped as it is read
		// and delivered at its lag, with no rate to cross at.
		replies := &backlog{more: make(chan struct{}, 1)}
		for _, way := range []struct {
			held     *backlog
			from, to net.Conn
		}{{held, in, out}, {replies, out, in}} {
			wg.Go(func() {
				defer closeBoth()
				way.held.fillLagging(way.from, l.lagNow)
			})
			wg.Go(func() {
				defer closeBoth()
				way.held.deliver(way.to, 0, l.done, nil)
			})
		}
		wg.Wait()
		return
	}
	wg.Go(func() {
		defer closeBoth()
		held.fill(in)
	})
	wg.Go(func() {
		defer closeBoth()
		held.deliver(out, l.rate, l.done, nil)
	})
	if l.back == 0 {
		wg.Go(func() {
			defer closeBoth()
			l.answer(in, out)
		})
		wg.Wait()
		return
	}
	// PACED BACK AS WELL: what the broker sends is read as fast as it comes
	// and delivered to the node at the link's pace, held back first for as
	// long as a [Link.Delay] says.
	replies := &backlog{more: make(chan struct{}, 1)}
	wg.Go(func() {
		defer closeBoth()
		replies.fill(out)
	})
	wg.Go(func() {
		defer closeBoth()
		replies.deliver(in, l.back, l.done, l.held)
	})
	wg.Wait()
}

// answer carries what the broker sends to the node as it comes, each read held
// back first for as long as a [Link.Delay] says, until either side ends.
func (l *Link) answer(node io.Writer, broker io.Reader) {
	buf := make([]byte, 64<<10)
	for {
		n, err := broker.Read(buf)
		if n > 0 {
			if wait := l.held(); wait > 0 {
				late := time.NewTimer(wait) //fixedwait:simulated bytes queued on the link behind others the broker sent first
				select {
				case <-late.C:
				case <-l.done:
					late.Stop()
					return
				}
			}
			if _, werr := node.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
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
	chunks []chunk
	// ended is set once the node's side has nothing more to send.
	ended bool
	// more is signalled whenever a chunk is added or the node's side ends.
	more chan struct{}
}

// chunk is one read of what a side sent, and when a lagging link delivers it:
// zero for as soon as the link's rate lets it.
type chunk struct {
	bytes []byte
	due   time.Time
}

// fill reads src as fast as it sends, holding each read, until src ends.
func (b *backlog) fill(src io.Reader) {
	b.fillLagging(src, nil)
}

// fillLagging is fill whose every read is due lag() after it was read; nil for
// no lag.
func (b *backlog) fillLagging(src io.Reader, lag func() time.Duration) {
	defer b.push(chunk{}, true)
	for {
		buf := make([]byte, 64<<10)
		n, err := src.Read(buf)
		if n > 0 {
			read := chunk{bytes: buf[:n]}
			if lag != nil {
				if d := lag(); d > 0 {
					read.due = time.Now().Add(d)
				}
			}
			b.push(read, false)
		}
		if err != nil {
			return
		}
	}
}

func (b *backlog) push(c chunk, ended bool) {
	b.mu.Lock()
	if c.bytes != nil {
		b.chunks = append(b.chunks, c)
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
func (b *backlog) next(done <-chan struct{}) (chunk, bool) {
	for {
		b.mu.Lock()
		if len(b.chunks) > 0 {
			c := b.chunks[0]
			b.chunks = b.chunks[1:]
			b.mu.Unlock()
			return c, true
		}
		ended := b.ended
		b.mu.Unlock()
		if ended {
			return chunk{}, false
		}
		select {
		case <-b.more:
		case <-done:
			return chunk{}, false
		}
	}
}

// deliver writes each chunk to dst once a link carrying rate bytes a second
// would have carried it: a chunk's own time after the one before it was
// written, so the link never carries faster than rate, and an idle link lets
// no burst through at once; a rate of 0 carries it at once. hold, where it is
// not nil, is how long a chunk is still to be held back before it starts to
// cross ([Link.Delay]), and a chunk with a due instant is held until it
// ([Link.Lag]).
func (b *backlog) deliver(dst io.Writer, rate int64, done <-chan struct{}, hold func() time.Duration) {
	for {
		c, ok := b.next(done)
		if !ok {
			return
		}
		if !c.due.IsZero() {
			wait := time.Until(c.due) //fixedwait:measure the instant a lagging link delivers a chunk at, which the relay holds bytes to rather than decides by
			if wait > 0 {
				late := time.NewTimer(wait) //fixedwait:simulated a loaded broker answering every request a while late
				select {
				case <-late.C:
				case <-done:
					late.Stop()
					return
				}
			}
		}
		if hold != nil {
			if wait := hold(); wait > 0 {
				late := time.NewTimer(wait) //fixedwait:simulated bytes queued on the link behind others the broker sent first
				select {
				case <-late.C:
				case <-done:
					late.Stop()
					return
				}
			}
		}
		if rate > 0 {
			crossing := time.NewTimer(time.Duration(len(c.bytes)) * time.Second / time.Duration(rate)) //fixedwait:simulated a link carrying a side's bytes at its rate
			select {
			case <-crossing.C:
			case <-done:
				crossing.Stop()
				return
			}
		}
		if _, err := dst.Write(c.bytes); err != nil {
			return
		}
	}
}
