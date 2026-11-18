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
//
// # Each connection alone, or one bottleneck for all of them
//
// [Slow], [SlowEachWay] and [Lagging] carry each connection at its rate or
// lag as though it had the link to itself: two connections across a [Slow]
// link each cross at the whole rate. That is the link a case about one
// connection's own bytes wants, and it says nothing about two connections
// that share a narrow link, which is what a node's connections do in
// production: its queue's, its coordination store's and, on a leaf, every
// leaf link it opens to the members' one port all cross the node's one
// uplink. [SharedEachWay] and [SharedFIFO] carry every connection's bytes in
// each direction through ONE pacer at the link's rate, as that uplink does,
// in the two disciplines a real bottleneck queues by:
//
//   - FAIR ([SharedEachWay]): fq_codel or CAKE, the default queue discipline
//     of most Linux distributions. Every connection with bytes waiting is
//     served in turn, a [Quantum] at a time, so a small write waits for at
//     most one quantum of each other connection and then its own bytes
//     (Quantum / rate per other connection: 22.3 ms at 64 KiB/s), however
//     much they have queued.
//   - FIRST IN, FIRST OUT ([SharedFIFO]): one queue per direction, in the
//     order the bytes were sent, so a small write waits behind every byte the
//     others queued ahead of it. What a connection may have queued is capped
//     at its inflight, which stands in for its congestion window: TCP grows a
//     flow's bytes in flight until they fill the bottleneck's buffer, and no
//     further.
//
// A QUANTUM IS ONE ETHERNET SEGMENT, 1,460 bytes, and a shared link moves at
// most that much of a connection before it turns to the next. Pacing whole
// reads instead (up to 64 KiB each) would let a connection that queued a
// megabyte hold the link for a second at 64 KiB/s per turn, and a case would
// derive its bound from a relay's buffer size rather than from the link.
//
// # Per account, on every link
//
// A node on a leaf opens one leaf link per local account it joins the members
// from, every one dialling the same URLs, so a case cannot give each link a
// relay of its own without a production seam to name a second address. The
// relay instead reads the account each leaf link binds from the
// `remote_account` field of the CONNECT the leaf sends through it — out of an
// s2 stream where the link negotiated compression, which nats-server does by
// default — and acts on one account's links alone: [Link.Drop] closes them
// and lets them redial, [Link.Cut] closes them and refuses them until
// [Link.Mend], and [Link.Blackhole] stops reading them either way without
// closing them, until [Link.Mend]. A connection that binds no account — a
// client's CONNECT names none, and a TLS or websocket link shows the relay
// none — is never one of them. Every link the package builds does this, and
// for a leaf that dials again only behind a member that advertises the link
// ([Link.To] says why).
//
// # What a node's socket says, against what the link carried
//
// A case that reads a node's own socket — how much of what it wrote the far
// end has acknowledged — reads it against the link: [Link.Count] is what the
// link has read and carried of an account's connections each way,
// [Link.Watch] hands a case each piece the link writes to the far side as it
// writes it, and [Link.Unread] is what waits in the relay's receive buffers.
// Those buffers are PINNED to [SocketBuffer], and [Link.Buffers] is what the
// kernel granted: a sender's kernel counts a byte as delivered once the
// relay's kernel has taken it, so a node's acknowledged bytes run ahead of
// what the link has read by at most [Buffers.Receive] — an upper bound the
// window never reaches, since the buffer counts the kernel's overhead of
// each segment as well as its bytes — and a blackholed account's run ahead
// of it by that at most and then stand still, because a blackhole stops
// reading rather than read and hold.
//
// A NODE'S ACKNOWLEDGED BYTES MOVE IN STEPS, NOT A CROSSING AT A TIME: the
// relay's kernel reopens a window it has shut only once a good part of its
// receive space is free again (Linux's receive-side silly window avoidance,
// __tcp_select_window), so behind a link that reads slowly — a first-in,
// first-out one at its inflight, a mended blackhole — what a node's socket
// says it delivered stands still across many crossings and then jumps by
// tens of kilobytes. A case bounds a node's acknowledged bytes by what the
// link has read and the pinned buffer; it never expects them to advance
// with each piece the link carries.
package linktest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/klauspost/compress/s2"
)

// Quantum is how many bytes a shared link ([SharedEachWay], [SharedFIFO])
// moves of one connection at a time: one Ethernet segment's payload (a
// 1,500-byte frame less 40 bytes of IPv4 and TCP header), the unit a fair
// queue serves a flow by. A case derives its bounds from it.
const Quantum = 1460

// readChunk is the most a relay reads from a side at once. It decides nothing
// a case derives from: a shared link moves a [Quantum] at a time, whatever a
// read held, and a link that paces each connection alone carries a read at
// its rate.
const readChunk = 64 << 10

// SocketBuffer is the receive buffer and the send buffer the relay asks the
// kernel for on every socket it carries over ([Buffers] says what a case
// allows for; [Link.Buffers] is what the kernel granted).
//
// 64 KiB, THE SMALLEST THAT TAKES A WHOLE LOOPBACK SEGMENT: loopback's MTU
// makes a segment 65,483 bytes, and Linux grants twice the ask but advertises
// only the part its per-segment overhead accounting leaves (since 6.6 about
// nine tenths on loopback), so 32 KiB would advertise a window short of one
// segment and slow every link the package builds. No larger, because every
// byte of it is one a sender's kernel counts as delivered that the relay never
// carried: a case reading a node's acknowledged bytes allows for it whole.
const SocketBuffer = 64 << 10

// Buffers is what the kernel holds a socket the relay carries over to, in
// bytes: what it takes in that the relay has not read, and what it keeps of
// what the relay wrote that the far end has not taken.
//
// The relay pins both to [SocketBuffer], its listener's before it listens and
// every socket it dials before it connects. A sender's kernel counts a byte as
// delivered once the RELAY'S kernel has taken it, so a node's delivered bytes
// run ahead of what the link carried by up to the relay's receive buffer,
// which unpinned grows from 128 KiB to 6 MiB: pinned, a node's acknowledged
// bytes are at most [Link.Count]'s read plus Receive. What the relay carried
// runs ahead of what a slow reader has read by up to Send plus that side's own
// receive buffer, which autotunes, so read what a side has taken at that side.
type Buffers struct {
	Receive int
	Send    int
}

// Link is a TCP relay a case puts between a node and the broker it reaches (a
// leaf and its member's leaf listener, a client and an external server). It
// carries what the NODE sends at rate bytes a second and what the broker sends
// back at once ([Slow]) or at its own rate ([SlowEachWay]), as a real link
// does a read's reply of megabytes; it may hold the broker's bytes
// ([Link.Delay]), LAG ([Lagging]) or share one fair ([SharedEachWay]) or FIFO
// ([SharedFIFO]) bottleneck. It ends, with every connection and goroutine,
// with the case. It reads each side as fast as it comes and holds what it has
// not carried, so only DELIVERY is paced: pacing reads would push back into
// the sender's socket, and a broker whose writes stall closes its link, tearing
// down what a case meant to show waiting. [SharedFIFO] alone pushes back.
type Link struct {
	ln net.Listener
	// dial opens the relay's own connection to the broker for each one it
	// carries.
	dial func(context.Context) (net.Conn, error)
	// toward carries what the node sends towards the broker, and back what
	// the broker sends back.
	toward, back carrier
	// done is closed when the case ends, which ends every delivery waiting
	// on the link's pace.
	done chan struct{}
	wg   sync.WaitGroup

	mu    sync.Mutex
	shut  bool
	pairs map[*pair]struct{}
	// buffers is the most the kernel granted any of the relay's sockets
	// ([pin]): its listener's, and every one it has carried over once
	// connected ([Link.Buffers]).
	buffers Buffers
	// target is the broker's address, empty until a case names it
	// ([Link.To]).
	target string
	// heldUntil is when what the broker sends stops being held back
	// ([Link.Delay]); zero for never held.
	heldUntil time.Time
	// lag is how long after it was sent a lagging link delivers a chunk
	// ([Link.Lag]).
	lag time.Duration
	// accounts is what a case has done to each account's links: cut,
	// blackholed, and how many of its connections the link refused.
	accounts map[string]*accountState
	// watch is called each time the link writes a piece of a connection's
	// bytes to the far side ([Link.Watch]); nil for no one watching.
	watch func(Crossing)
}

// carrier is how a link carries one direction's bytes.
type carrier struct {
	// rate is how many bytes a second each connection crosses at, alone;
	// 0 for at once. Unused where shared is set.
	rate int64
	// lagging delivers each chunk [Link.Lag] after it was read.
	lagging bool
	// held holds the direction back for as long as a [Link.Delay] says:
	// what the broker sends, on every link that does not lag.
	held bool
	// shared is the one bottleneck every connection's bytes in this
	// direction cross; nil where each connection crosses alone.
	shared *bottleneck
}

// accountState is what a case has done to the links of one account.
type accountState struct {
	// cut refuses the account's connections ([Link.Cut]).
	cut bool
	// silent carries nothing of them, either way ([Link.Blackhole]).
	silent bool
	// refused counts the connections the link closed because the account
	// was cut when they bound it.
	refused int
	// ended is what the account's connections that have ended read and
	// carried, each way.
	ended map[Way]Count
}

// Way is a direction across a link.
type Way string

const (
	// ToBroker is what the node sends, carried to the broker.
	ToBroker Way = "to the broker"
	// ToNode is what the broker sends back, carried to the node.
	ToNode Way = "to the node"
)

// Valid reports whether w is one of the two ways.
func (w Way) Valid() bool { return w == ToBroker || w == ToNode }

// Count is what a link has done with the bytes of some connections in one
// way, in bytes.
type Count struct {
	// Read is what the link has taken off the sender's socket.
	Read int64
	// Carried is what it has written to the far side's socket, once the
	// link's pace let it cross.
	Carried int64
}

// Crossing is a piece of a connection's bytes the link has just written to
// the far side, and the count of its account's connections in its way once it
// has ([Link.Watch]).
type Crossing struct {
	// Account is the one the connection binds, the empty name for none.
	Account string
	Way     Way
	// Bytes is how many the piece held.
	Bytes int
	Count Count
}

// Slow listens on a loopback port and carries every connection made to it on
// to target, at rate bytes a second towards target and what target sends back
// at once, until the case ends. Each connection crosses at the whole rate, as
// though it had the link to itself.
func Slow(t testing.TB, target string, rate int64) *Link {
	t.Helper()
	positive(t, rate)
	return open(t, target, carrier{rate: rate}, carrier{held: true})
}

// SlowEachWay is [Slow] whose link carries what target sends back at rate
// bytes a second too: a link that is slow both ways, as a real one is, so a
// read whose reply carries a value of megabytes waits for it to cross as a
// write of the value does.
func SlowEachWay(t testing.TB, target string, rate int64) *Link {
	t.Helper()
	positive(t, rate)
	return open(t, target, carrier{rate: rate}, carrier{rate: rate, held: true})
}

// Distant is [SlowEachWay] whose link also delivers every chunk either side
// sends no sooner than [Link.Lag] after it was sent: a link with a rate and a
// round trip, as a distant one is, where a transfer that moves one window a
// round trip runs at the window over the round trip, whatever the rate. Each
// connection crosses at the whole rate. Set the lag once the node has linked,
// as for [Lagging].
func Distant(t testing.TB, target string, rate int64) *Link {
	t.Helper()
	positive(t, rate)
	return open(t, target, carrier{rate: rate, lagging: true}, carrier{rate: rate, lagging: true})
}

// Lagging listens on a loopback port and carries every connection made to it
// on to target, both ways, until the case ends: at once until the case sets a
// lag ([Link.Lag]), and from then on every chunk either side sends that lag
// after it was sent.
//
// A LAG, NOT A RATE: it plays a loaded broker, where every round trip costs
// twice the lag however much is sent and a rate would let small long-poll
// requests build a growing backlog. Set it once the node has linked, since the
// handshake keeps deadlines a lag of hundreds of milliseconds already misses.
// SIZE EVERY WINDOW THE NODE WAITS IN TO THE ROUND TRIP: a window that ends
// before its answer crosses back loses it (jetstream's lagging leaf says which).
func Lagging(t testing.TB, target string) *Link {
	t.Helper()
	return open(t, target, carrier{lagging: true}, carrier{lagging: true})
}

// SharedEachWay listens on a loopback port and carries every connection made
// to it on to target until the case ends, every connection's bytes in each
// direction crossing ONE fair pacer at rate bytes a second: whichever
// connections have bytes waiting are served in turn, a [Quantum] each, and
// each connection's backlog is its own, read as fast as it comes.
//
// So N connections that all have bytes to send each cross at rate / N, one
// alone crosses at the whole rate, and a small write waits for at most one
// quantum of each other connection with bytes waiting, Quantum / rate apiece,
// before its own bytes cross: the link behind a fair queue discipline
// (fq_codel, CAKE), where a renewal is not queued behind a bulk transfer.
// What the broker sends is held back for as long as a [Link.Delay] says.
func SharedEachWay(t testing.TB, target string, rate int64) *Link {
	t.Helper()
	positive(t, rate)
	toward, back := sharedEachWay(rate)
	return open(t, target, toward, back)
}

// sharedEachWay is the two directions of a [SharedEachWay] link.
func sharedEachWay(rate int64) (toward, back carrier) {
	return carrier{shared: &bottleneck{rate: rate}},
		carrier{held: true, shared: &bottleneck{rate: rate}}
}

// SharedFIFO listens on a loopback port and carries every connection made to
// it on to target until the case ends, all bytes in each direction crossing
// ONE first-in, first-out queue at rate bytes a second: a bottleneck with no
// fair queueing, where a small write waits behind every queued byte.
//
// INFLIGHT IS A CONNECTION'S CONGESTION WINDOW: the relay reads a connection
// only while fewer than inflight of its bytes in that direction await writing
// to the far side (queued, or held by a [Link.Blackhole]), so what it read
// runs ahead of [Link.Count]'s carried by at most inflight. Past that the
// sender's socket buffer fills and its writes block. A member's leaf writes
// keep within their 10 s deadline only while inflight / rate stays under it:
// keep inflight below ten seconds of rate. [Link.Delay] holds the broker's bytes.
func SharedFIFO(t testing.TB, target string, rate int64, inflight int) *Link {
	t.Helper()
	positive(t, rate)
	if inflight <= 0 {
		t.Fatalf("a first-in, first-out link lets a connection queue %d bytes, which carries nothing", inflight)
	}
	toward, back := sharedFIFO(rate, inflight)
	return open(t, target, toward, back)
}

// sharedFIFO is the two directions of a [SharedFIFO] link.
func sharedFIFO(rate int64, inflight int) (toward, back carrier) {
	return carrier{shared: &bottleneck{rate: rate, fifo: true, inflight: inflight}},
		carrier{held: true, shared: &bottleneck{rate: rate, fifo: true, inflight: inflight}}
}

// positive fails the case for a rate that carries nothing.
func positive(t testing.TB, rate int64) {
	t.Helper()
	if rate <= 0 {
		t.Fatalf("a slow link carries %d bytes a second, which carries nothing", rate)
	}
}

// open binds a link to target on a loopback port, carrying toward and back
// as each says, until the case ends.
func open(t testing.TB, target string, toward, back carrier) *Link {
	t.Helper()
	// THE CASE'S CONTEXT, which ends as the case does, just before its
	// cleanups (this link's close among them): a connection it dialled
	// outlives it, as a dialled connection does its dial's context.
	ctx := t.Context()
	// PINNED BEFORE IT LISTENS, so the window every connection it accepts
	// negotiates is the pinned buffer's, and each accepted socket takes the
	// listener's buffers (Linux's sk_clone_lock, darwin's sonewconn).
	ln, err := (&net.ListenConfig{Control: pin}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind the link: %v", err)
	}
	raw, err := ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		_ = ln.Close()
		t.Fatalf("read the link's socket: %v", err)
	}
	got, err := granted(raw)
	if err != nil {
		_ = ln.Close()
		t.Fatalf("read the link's socket buffers: %v", err)
	}
	l := newLink(nil, toward, back)
	l.ln, l.target, l.dial = ln, target, l.dialTarget
	l.buffers = got
	l.start(t)
	return l
}

// dialTarget dials the broker the link carries to, in ctx, its socket's
// buffers pinned as the listener's are.
func (l *Link) dialTarget(ctx context.Context) (net.Conn, error) {
	l.mu.Lock()
	target := l.target
	l.mu.Unlock()
	if target == "" {
		return nil, errNoTarget
	}
	return (&net.Dialer{Control: pin}).DialContext(ctx, "tcp", target)
}

// errNoTarget refuses a connection made to a link before the case named the
// broker it carries to ([Link.To]).
var errNoTarget = errors.New("the link carries to no broker yet: name one with To")

// newLink is a link that dials the broker with dial and carries toward and
// back as each says, taking no connection until it is started.
func newLink(dial func(context.Context) (net.Conn, error), toward, back carrier) *Link {
	if toward.shared != nil {
		toward.shared.more = make(chan struct{}, 1)
	}
	if back.shared != nil {
		back.shared.more = make(chan struct{}, 1)
	}
	return &Link{
		dial: dial, toward: toward, back: back,
		done:     make(chan struct{}),
		pairs:    make(map[*pair]struct{}),
		accounts: make(map[string]*accountState),
	}
}

// start runs the link's shared pacers and carries every connection its
// listener takes, dialling the target for each in the case's context, until
// the case ends.
func (l *Link) start(t testing.TB) {
	ctx := t.Context()
	if b := l.toward.shared; b != nil {
		l.wg.Go(func() { b.run(l.done, nil) })
	}
	if b := l.back.shared; b != nil {
		l.wg.Go(func() { b.run(l.done, l.held) })
	}
	if l.ln != nil {
		l.wg.Go(func() { l.accept(ctx) })
	}
	t.Cleanup(l.close)
}

// Addr is the host and port the node dials to cross the link.
func (l *Link) Addr() string { return l.ln.Addr().String() }

// Buffers is the most the kernel granted any socket the relay has carried
// over, each way, for [SocketBuffer] asked of each ([Buffers] says what a case
// allows for it).
//
// READ IT ONCE THE CONNECTION A CASE ASSERTS ABOUT IS CARRIED: Linux grants on
// the ask, but darwin rounds each buffer of an accepted or dialled socket up
// to whole segments as the handshake settles them (XNU's tcp_mss), never on
// the listener, so there a connection's sockets hold more than the listener
// was granted, and the link reports it from when it carries them.
func (l *Link) Buffers() Buffers {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffers
}

// Count is what the link has done with the bytes of every connection that
// binds account in way, those that have ended included: the empty name counts
// the connections that bind none.
func (l *Link) Count(account string, way Way) Count {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.countLocked(account, way)
}

// countLocked is [Link.Count]; l.mu held.
func (l *Link) countLocked(account string, way Way) Count {
	c := l.account(account).ended[way]
	for p := range l.pairs {
		if p.account != account {
			continue
		}
		for _, g := range p.legs {
			if g.way == way {
				c.Read += g.taken.Load()
				c.Carried += g.carried.Load()
			}
		}
	}
	return c
}

// Unread is how many bytes of every connection that binds account wait in
// way's receive buffers of the relay's sockets, taken in by the kernel and
// not yet read by the relay: what a blackholed account's senders have sent it
// since ([Link.Blackhole]), at most [Buffers.Receive] a connection. A
// connection carried over no socket (a case's in-memory pipe) holds none.
func (l *Link) Unread(account string, way Way) (int, error) {
	var srcs []net.Conn
	l.mu.Lock()
	for p := range l.pairs {
		if p.account != account {
			continue
		}
		for _, g := range p.legs {
			if g.way == way {
				srcs = append(srcs, g.src)
			}
		}
	}
	l.mu.Unlock()
	total := 0
	for _, c := range srcs {
		sc, ok := c.(syscall.Conn)
		if !ok {
			continue
		}
		raw, err := sc.SyscallConn()
		if err != nil {
			return 0, err
		}
		var (
			n    int
			rerr error
		)
		// THROUGH THE CONNECTION'S OWN DESCRIPTOR, held open for the
		// call, so a read racing the connection's close fails rather than
		// reads another socket.
		if err := raw.Control(func(fd uintptr) { n, rerr = unreadOn(fd) }); err != nil {
			if errors.Is(err, net.ErrClosed) {
				continue
			}
			return 0, err
		}
		if rerr != nil {
			return 0, rerr
		}
		total += n
	}
	return total, nil
}

// Watch calls f each time the link writes a piece of a connection's bytes to
// the far side, from then until the case ends, with the count of its
// account's connections in its way once it has: so a case reads what a node's
// socket says at the moment the link has carried a known count.
//
// F RUNS ON THE GOROUTINE THAT WROTE THE PIECE, one per connection and way, so
// it is called concurrently and must be safe for that; one connection's pieces
// in one way arrive in carried order, and nothing orders the rest. That
// connection's way waits for f to return, every other carries on; a
// first-in, first-out link reads no more of it than its inflight ahead of
// what f has been handed ([SharedFIFO]).
func (l *Link) Watch(f func(Crossing)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.watch = f
}

// To names target as the broker every connection the link takes from now on
// is carried to: for a link opened with no target, which refuses every
// connection until it has one.
//
// THE LINK IS THE ONLY WAY only while the broker tells the node of no other. A
// member hands every leaf the leaf URLs it advertises, its own listener's
// unless told otherwise, and a redialling leaf tries those BEFORE its
// configured URL (nats-server's doUpdateLNURLs and pickNextURL), so behind an
// ordinary member a [Link.Cut] or [Link.Drop] cuts nothing. So open the link
// with no target, start the member advertising [Link.Addr] (LeafNode.Advertise,
// the jetstream package's LeafAdvertise; LeafNode.NoAdvertise serves too), then
// name the member's listener here.
func (l *Link) To(target string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.target = target
}

// Delay holds back every byte the broker sends towards the node from now until
// d has passed, and then carries them on in order — at once, or at the link's
// pace where it paces them: what a reply meets on a slow link behind a whole
// message the broker sent ahead of it — it was sent, and arrives late. What
// the link has already delivered before Delay was called is not held. A
// lagging link ([Lagging]) is held by its lag alone.
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

// Drop closes every connection the link carries that binds account (a leaf's
// local account, [Link]'s "Per account"), and lets them dial again: a link
// that went down once.
func (l *Link) Drop(account string) {
	for _, p := range l.bound(account, func(*accountState) {}) {
		p.close()
	}
}

// Cut closes every connection the link carries that binds account, and closes
// every one that binds it from now on as soon as it says so, until
// [Link.Mend]: a link that is down and stays down. The other accounts' links
// are carried as before.
func (l *Link) Cut(account string) {
	for _, p := range l.bound(account, func(a *accountState) { a.cut = true }) {
		p.close()
	}
}

// Blackhole stops reading what every connection that binds account sends,
// either way, now and for every one that binds it later (a leaf's redial),
// until [Link.Mend], without closing any: a path that drops every packet. It
// returns once the link reads none, so [Link.Count] stands still, a later
// connection's count including what was read to learn its account (INFO, CONNECT).
//
// IT STOPS READING, RATHER THAN READ AND HOLD: a sender's kernel counts what
// the relay's kernel takes as delivered, so reading on would let a node say it
// delivered everything across a link that carries nothing. The relay's kernel
// takes what fits its pinned receive buffer ([Link.Unread], at most
// [Buffers.Receive]) and shuts its window. A read in progress is held and
// [Link.Mend] carries it on first; whatever liveness the ends keep notices.
func (l *Link) Blackhole(account string) {
	ps := l.bound(account, func(a *accountState) { a.silent = true })
	for _, p := range ps {
		p.hush()
	}
	for _, p := range ps {
		p.quiet()
	}
}

// Mend undoes [Link.Cut] and [Link.Blackhole] for account: its links are
// taken again, and a blackholed one carries on what it held and reads on, in
// order.
func (l *Link) Mend(account string) {
	for _, p := range l.bound(account, func(a *accountState) { a.cut, a.silent = false, false }) {
		p.unhush()
	}
}

// bound records what a case did to name, under the link's lock, and is the
// connections the link carries that bind it.
func (l *Link) bound(name string, did func(*accountState)) []*pair {
	l.mu.Lock()
	defer l.mu.Unlock()
	did(l.account(name))
	var ps []*pair
	for p := range l.pairs {
		if p.bound && p.account == name {
			ps = append(ps, p)
		}
	}
	return ps
}

// account is what a case has done to name's links; l.mu held.
func (l *Link) account(name string) *accountState {
	a, ok := l.accounts[name]
	if !ok {
		a = &accountState{}
		l.accounts[name] = a
	}
	return a
}

// bind records that p binds the account name, and holds it to what a case
// has done to that account: false, with p closed, for a cut account.
func (l *Link) bind(p *pair, name string) bool {
	l.mu.Lock()
	a := l.account(name)
	p.account, p.bound = name, true
	if a.cut {
		a.refused++
		l.mu.Unlock()
		p.close()
		return false
	}
	// UNDER THE LINK'S LOCK, so a Mend of the account either finds the
	// pair bound and silenced or leaves it never silenced.
	if a.silent {
		p.hush()
	}
	l.mu.Unlock()
	return true
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
		l.wg.Go(func() { l.carry(ctx, in) })
	}
}

// track records p for the case's end to close, and refuses it once that end
// has begun, so no connection outlives the link; and takes the buffers its
// sockets hold into what the link reports, before a case can find it.
func (l *Link) track(p *pair, held Buffers) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.shut {
		return false
	}
	l.pairs[p] = struct{}{}
	l.buffers.Receive = max(l.buffers.Receive, held.Receive)
	l.buffers.Send = max(l.buffers.Send, held.Send)
	return true
}

// untrack forgets p once it has ended, keeping what it read and carried in
// its account's count.
func (l *Link) untrack(p *pair) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.pairs, p)
	a := l.account(p.account)
	if a.ended == nil {
		a.ended = map[Way]Count{}
	}
	for _, g := range p.legs {
		c := a.ended[g.way]
		c.Read += g.taken.Load()
		c.Carried += g.carried.Load()
		a.ended[g.way] = c
	}
}

// carry relays in to a connection of its own to the target, dialled in ctx,
// both ways, until either side ends, and then closes both.
func (l *Link) carry(ctx context.Context, in net.Conn) {
	out, err := l.dial(ctx)
	if err != nil {
		_ = in.Close()
		return
	}
	// ONCE BOTH ARE CONNECTED, which is when darwin settles their buffers
	// ([Link.Buffers]). A link that cannot say what its sockets hold
	// carries nothing over them.
	held, err := heldBy(in, out)
	if err != nil {
		_ = in.Close()
		_ = out.Close()
		return
	}
	p := newPair(in, out)
	// EITHER SIDE ENDING ENDS THE OTHER: closing the pair fails the read or
	// the write each of its goroutines is blocked in, and stops every wait
	// on the link's pace. Its legs are whole before the link tracks it, so
	// a count reads them without the pair's lock.
	toward := l.leg(p, ToBroker, in, out, l.toward)
	toward.sniff = &sniffer{}
	back := l.leg(p, ToNode, out, in, l.back)
	if !l.track(p, held) {
		p.close()
		return
	}
	defer l.untrack(p)
	var wg sync.WaitGroup
	for _, g := range []*leg{toward, back} {
		wg.Go(func() {
			defer p.close()
			l.read(g)
		})
		wg.Go(func() {
			defer p.close()
			g.deliver(l, p.stop)
		})
	}
	wg.Wait()
}

// heldBy is the most the kernel holds any of conns' sockets' buffers to, each
// way; a connection carried over no socket (a case's in-memory pipe) holds
// none.
func heldBy(conns ...net.Conn) (Buffers, error) {
	var most Buffers
	for _, c := range conns {
		sc, ok := c.(syscall.Conn)
		if !ok {
			continue
		}
		raw, err := sc.SyscallConn()
		if err != nil {
			return Buffers{}, err
		}
		got, err := granted(raw)
		if err != nil {
			return Buffers{}, err
		}
		most.Receive, most.Send = max(most.Receive, got.Receive), max(most.Send, got.Send)
	}
	return most, nil
}

// pair is one connection the link carries: the node's, and the relay's own to
// the broker.
type pair struct {
	in, out net.Conn
	// stop is closed when the pair ends, by either side or the case.
	stop chan struct{}
	once sync.Once

	// account is the one the connection binds, once bound is set; both
	// written under the link's lock.
	account string
	bound   bool

	mu sync.Mutex
	// silent stops either side being read ([Link.Blackhole]); audible is
	// closed once it is not, for a reader waiting on it.
	silent  bool
	audible chan struct{}
	// reading is how many of the pair's directions are in a read of their
	// source, and quieted is signalled, under mu, as each leaves it.
	reading int
	quieted *sync.Cond
	// legs are the pair's two directions, whole before the link tracks
	// the pair.
	legs []*leg
}

// newPair is the pair carrying in, the node's connection, over out, the
// relay's own to the broker.
func newPair(in, out net.Conn) *pair {
	p := &pair{in: in, out: out, stop: make(chan struct{}), audible: make(chan struct{})}
	close(p.audible)
	p.quieted = sync.NewCond(&p.mu)
	return p
}

// close ends the pair: both its connections, every wait on its behalf, and its
// place on a shared pacer.
func (p *pair) close() {
	p.once.Do(func() {
		close(p.stop)
		_ = p.in.Close()
		_ = p.out.Close()
		p.mu.Lock()
		legs := p.legs
		p.mu.Unlock()
		for _, g := range legs {
			if g.flow != nil {
				g.flow.leave()
			}
		}
	})
}

// longAgo is a read deadline already passed, which ends a read a source is
// blocked in at once.
var longAgo = time.Unix(1, 0)

// hush stops both of the pair's sources being read: a read either is in ends
// at once, through a deadline already passed, and none starts until
// [pair.unhush].
func (p *pair) hush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.silent {
		return
	}
	p.silent, p.audible = true, make(chan struct{})
	for _, g := range p.legs {
		_ = g.src.SetReadDeadline(longAgo)
	}
}

// quiet returns once neither of the pair's directions is in a read of its
// source, which after [pair.hush] none starts again: what the pair has read is
// then what it will have read until it is unhushed.
func (p *pair) quiet() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.reading > 0 {
		p.quieted.Wait()
	}
}

// unhush carries on what the pair held of the reads it was in when hushed, in
// the order they were sent, and lets its sources be read again.
func (p *pair) unhush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.silent {
		return
	}
	p.silent = false
	for _, g := range p.legs {
		for _, c := range g.parked {
			g.send(c)
		}
		g.parked = nil
		_ = g.src.SetReadDeadline(time.Time{})
	}
	close(p.audible)
}

// startRead waits until the pair's sources may be read and counts one more
// direction reading; false once stop is closed.
func (p *pair) startRead(stop <-chan struct{}) bool {
	for {
		p.mu.Lock()
		if !p.silent {
			p.reading++
			p.mu.Unlock()
			return true
		}
		audible := p.audible
		p.mu.Unlock()
		select {
		case <-audible:
		case <-stop:
			return false
		}
	}
}

// endRead counts a direction's read of its source ended.
func (p *pair) endRead() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reading--
	p.quieted.Broadcast()
}

// leg is one direction of a carried connection: what src sends, carried to
// dst.
type leg struct {
	p        *pair
	way      Way
	src, dst net.Conn
	how      carrier
	// taken is what the leg has read of its source, and carried what it
	// has written to its destination.
	taken, carried atomic.Int64
	// sniff reads the account the connection binds out of what the node
	// sends; nil once that is decided, and on the broker's direction.
	sniff *sniffer
	// own is the leg's own backlog, delivered at its own pace, where the
	// direction is not shared.
	own *backlog
	// flow is the leg's place on the shared pacer, where it is.
	flow *flow
	// parked is what the leg's read returned once the pair was silenced
	// while the leg was in it, in order: at most the one read; written
	// under the pair's lock.
	parked []chunk
}

// leg is the direction from src to dst of p, the way way, carried as how
// says.
func (l *Link) leg(p *pair, way Way, src, dst net.Conn, how carrier) *leg {
	g := &leg{p: p, way: way, src: src, dst: dst, how: how}
	if how.shared != nil {
		g.flow = how.shared.join()
	} else {
		g.own = &backlog{more: make(chan struct{}, 1)}
	}
	p.mu.Lock()
	p.legs = append(p.legs, g)
	p.mu.Unlock()
	return g
}

// read takes what the leg's source sends, as fast as it comes, or as fast as a
// first-in, first-out link lets it, and hands each read on, until the source
// ends or the pair does; while the pair is silent it reads nothing.
func (l *Link) read(g *leg) {
	for {
		n := readChunk
		if g.flow != nil {
			var ok bool
			if n, ok = g.flow.room(g.p.stop); !ok {
				return
			}
		}
		if !g.p.startRead(g.p.stop) {
			return
		}
		buf := make([]byte, n)
		k, err := g.src.Read(buf)
		// COUNTED AGAINST ITS INFLIGHT AS IT IS READ, before a blackhole
		// can park it, so a parked read is never outside the bound.
		if g.flow != nil {
			g.flow.took(k)
		}
		g.taken.Add(int64(k))
		g.p.endRead()
		if k > 0 {
			c := chunk{bytes: buf[:k]}
			if g.how.lagging {
				if d := l.lagNow(); d > 0 {
					c.due = time.Now().Add(d)
				}
			}
			if g.sniff != nil {
				if name, decided := g.sniff.feed(c.bytes); decided {
					g.sniff = nil
					if name != "" && !l.bind(g.p, name) {
						return
					}
				}
			}
			g.admit(c)
		}
		// A DEADLINE IS ONLY EVER THE PAIR'S HUSH, which the next read
		// waits out; anything else ends the source.
		if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
			return
		}
	}
}

// admit carries c on, or holds it while the pair is silent: the read the leg
// was in when the pair was silenced.
func (g *leg) admit(c chunk) {
	g.p.mu.Lock()
	defer g.p.mu.Unlock()
	if g.p.silent {
		g.parked = append(g.parked, c)
		return
	}
	g.send(c)
}

// send hands c to whatever carries the leg's bytes.
func (g *leg) send(c chunk) {
	if g.flow != nil {
		g.flow.enqueue(c.bytes)
		return
	}
	g.own.push(c)
}

// deliver writes what the leg carries to its destination, at the leg's pace,
// until stop is closed or a write fails.
func (g *leg) deliver(l *Link, stop <-chan struct{}) {
	var hold func() time.Duration
	if g.how.held {
		hold = l.held
	}
	if g.flow != nil {
		// THE SHARED PACER DECIDES WHEN; this writes what it has carried
		// at once, so a destination slow to read holds back its own
		// connection and none of the others.
		g.flow.out.deliver(g.dst, 0, stop, nil, func(n int) { g.wrote(l, n) })
		return
	}
	g.own.deliver(g.dst, g.how.rate, stop, hold, func(n int) { g.wrote(l, n) })
}

// wrote counts n bytes the leg has written to its destination, makes room
// for as many more on a fifo pacer, and hands the crossing to whoever watches
// the link ([Link.Watch]), waiting for it. Counted carried BEFORE the room is
// made, so what the link has read never runs more than inflight ahead of
// what it carried, not even between the two.
func (g *leg) wrote(l *Link, n int) {
	g.carried.Add(int64(n))
	if g.flow != nil {
		g.flow.release(n)
	}
	l.mu.Lock()
	watch := l.watch
	var c Crossing
	if watch != nil {
		c = Crossing{Account: g.p.account, Way: g.way, Bytes: n, Count: l.countLocked(g.p.account, g.way)}
	}
	l.mu.Unlock()
	if watch != nil {
		watch(c)
	}
}

// close ends the link: no connection is taken or carried after it, and it
// returns once every goroutine the link ran has.
func (l *Link) close() {
	if l.ln != nil {
		_ = l.ln.Close()
	}
	l.mu.Lock()
	l.shut = true
	pairs := make([]*pair, 0, len(l.pairs))
	for p := range l.pairs {
		pairs = append(pairs, p)
	}
	l.mu.Unlock()
	close(l.done)
	for _, p := range pairs {
		p.close()
	}
	l.wg.Wait()
}

// bottleneck is the one pacer every connection's bytes in one direction of a
// shared link cross, at rate bytes a second: in turn, a [Quantum] of each
// connection with bytes waiting, or, fifo, in the order they were read.
type bottleneck struct {
	rate int64
	fifo bool
	// inflight is the most a connection may have queued, fifo; a
	// connection is read only while it has less.
	inflight int

	mu sync.Mutex
	// flows are the connections on the pacer, in the order a fair pacer
	// serves them; turn is the next to be served, the one after the last
	// served.
	flows []*flow
	turn  int
	// queue is what a fifo pacer has to carry, in the order it was read.
	queue []segment
	// more is signalled whenever bytes are queued.
	more chan struct{}
}

// flow is one connection's direction on a shared pacer.
type flow struct {
	b *bottleneck
	// pending is what a fair pacer has still to carry of the connection,
	// in order.
	pending []byte
	// queued is how many of the bytes the link has read of the connection
	// a fifo link has still to write to the far side: from the read that
	// took them, a read a [Link.Blackhole] holds included, until the leg
	// has written them, so what the link has read of the connection runs
	// ahead of what it has carried by queued, and queued never passes
	// inflight.
	queued int
	// freed is signalled whenever a fifo link has written some of them.
	freed chan struct{}
	// out is what the pacer has carried, written to the destination by
	// the leg's own goroutine.
	out *backlog
	// left is set once the connection has ended.
	left bool
}

// segment is a piece of one connection's bytes on a fifo pacer's queue.
type segment struct {
	f     *flow
	bytes []byte
}

// join puts a connection's direction on the pacer, NEXT IN TURN: right after
// the connection served last, as a fair queue serves a new flow ahead of the
// round it joins (fq_codel's new flows). Put at the end instead, a connection
// that joined while the only other was being served waited a second quantum
// of it, so two connections sent at once did not alternate.
func (b *bottleneck) join() *flow {
	f := &flow{b: b, freed: make(chan struct{}, 1), out: &backlog{more: make(chan struct{}, 1)}}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.flows = slices.Insert(b.flows, b.turn, f)
	return f
}

// leave takes an ended connection off the pacer, with whatever it had still
// to carry.
func (f *flow) leave() {
	b := f.b
	b.mu.Lock()
	defer b.mu.Unlock()
	f.left = true
	f.pending = nil
	for i, o := range b.flows {
		if o == f {
			b.flows = append(b.flows[:i], b.flows[i+1:]...)
			if i < b.turn {
				b.turn--
			}
			break
		}
	}
	if b.turn >= len(b.flows) {
		b.turn = 0
	}
}

// room is how many bytes the connection's source may be read for now: as many
// as a read takes on a fair pacer, and on a fifo pacer what its inflight has
// left, waiting while that is none; false once stop is closed. ONE READER A
// FLOW, so what room grants is still free when the read it sizes returns:
// only that reader adds to queued ([flow.took]).
func (f *flow) room(stop <-chan struct{}) (int, bool) {
	b := f.b
	if !b.fifo {
		return readChunk, true
	}
	for {
		b.mu.Lock()
		left := b.inflight - f.queued
		b.mu.Unlock()
		if left > 0 {
			return min(left, readChunk), true
		}
		select {
		case <-f.freed:
		case <-stop:
			return 0, false
		}
	}
}

// took counts n bytes the leg has just read of the connection against its
// inflight, on a fifo pacer, until [flow.release] lets them go.
func (f *flow) took(n int) {
	b := f.b
	if !b.fifo {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	f.queued += n
}

// release lets go of n of the connection's bytes the leg has written to the
// far side, on a fifo pacer, making room for more of it.
func (f *flow) release(n int) {
	b := f.b
	if !b.fifo {
		return
	}
	b.mu.Lock()
	f.queued -= n
	b.mu.Unlock()
	select {
	case f.freed <- struct{}{}:
	default:
	}
}

// enqueue puts p on the pacer, behind what the connection queued before.
func (f *flow) enqueue(p []byte) {
	b := f.b
	b.mu.Lock()
	if f.left {
		b.mu.Unlock()
		return
	}
	if b.fifo {
		for len(p) > 0 {
			n := min(len(p), Quantum)
			b.queue = append(b.queue, segment{f: f, bytes: p[:n:n]})
			p = p[n:]
		}
	} else {
		f.pending = append(f.pending, p...)
	}
	b.mu.Unlock()
	select {
	case b.more <- struct{}{}:
	default:
	}
}

// take is the next piece the pacer carries and whose it is, waiting for one;
// false once done is closed.
func (b *bottleneck) take(done <-chan struct{}) (*flow, []byte, bool) {
	for {
		if f, piece := b.next(); f != nil {
			return f, piece, true
		}
		select {
		case <-b.more:
		case <-done:
			return nil, nil, false
		}
	}
}

// next is the piece the pacer carries now, nil for none waiting: the head of
// a fifo pacer's queue, or a [Quantum] of the next connection in turn that has
// bytes waiting.
func (b *bottleneck) next() (*flow, []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fifo {
		for len(b.queue) > 0 {
			s := b.queue[0]
			b.queue = b.queue[1:]
			if !s.f.left {
				return s.f, s.bytes
			}
		}
		return nil, nil
	}
	n := len(b.flows)
	for i := range n {
		f := b.flows[(b.turn+i)%n]
		if len(f.pending) == 0 {
			continue
		}
		k := min(len(f.pending), Quantum)
		piece := f.pending[:k:k]
		f.pending = f.pending[k:]
		if len(f.pending) == 0 {
			f.pending = nil
		}
		b.turn = (b.turn + i + 1) % n
		return f, piece
	}
	return nil, nil
}

// run carries what the pacer has queued, a piece at a time at its rate, each
// held back first for as long as hold says where it is not nil, until done is
// closed.
func (b *bottleneck) run(done <-chan struct{}, hold func() time.Duration) {
	for {
		f, piece, ok := b.take(done)
		if !ok {
			return
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
		crossing := time.NewTimer(time.Duration(len(piece)) * time.Second / time.Duration(b.rate)) //fixedwait:simulated a bottleneck carrying every connection's bytes at its rate
		select {
		case <-crossing.C:
		case <-done:
			crossing.Stop()
			return
		}
		b.carried(f, piece)
	}
}

// carried hands what crossed to its connection, for its leg to write to the
// far side. It makes no room on a fifo pacer: the leg does, once it has
// written the piece ([flow.release]), so a far side slow to read, or a
// watcher slow to return, holds its connection's reads back as a receiver's
// window holds a sender's.
func (b *bottleneck) carried(f *flow, piece []byte) {
	b.mu.Lock()
	left := f.left
	b.mu.Unlock()
	if left {
		return
	}
	f.out.push(chunk{bytes: piece})
}

// backlog is what a side has sent and the link has not carried yet, in the
// order it was sent.
type backlog struct {
	mu     sync.Mutex
	chunks []chunk
	// more is signalled whenever a chunk is added.
	more chan struct{}
}

// chunk is one read of what a side sent, and when a lagging link delivers it:
// zero for as soon as the link's rate lets it.
type chunk struct {
	bytes []byte
	due   time.Time
}

// push adds c behind what the backlog holds.
func (b *backlog) push(c chunk) {
	b.mu.Lock()
	b.chunks = append(b.chunks, c)
	b.mu.Unlock()
	select {
	case b.more <- struct{}{}:
	default:
	}
}

// next is the oldest chunk not yet carried, waiting for one; false once stop
// is closed.
func (b *backlog) next(stop <-chan struct{}) (chunk, bool) {
	for {
		b.mu.Lock()
		if len(b.chunks) > 0 {
			c := b.chunks[0]
			b.chunks = b.chunks[1:]
			b.mu.Unlock()
			return c, true
		}
		b.mu.Unlock()
		select {
		case <-b.more:
		case <-stop:
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
// ([Link.Lag]). wrote is told each chunk's size once it is written. It
// returns once stop is closed or a write fails.
func (b *backlog) deliver(dst io.Writer, rate int64, stop <-chan struct{}, hold func() time.Duration, wrote func(int)) {
	for {
		c, ok := b.next(stop)
		if !ok {
			return
		}
		if !c.due.IsZero() {
			wait := time.Until(c.due) //fixedwait:measure the instant a lagging link delivers a chunk at, which the relay holds bytes to rather than decides by
			if wait > 0 {
				late := time.NewTimer(wait) //fixedwait:simulated a loaded broker answering every request a while late
				select {
				case <-late.C:
				case <-stop:
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
				case <-stop:
					late.Stop()
					return
				}
			}
		}
		if rate > 0 {
			crossing := time.NewTimer(time.Duration(len(c.bytes)) * time.Second / time.Duration(rate)) //fixedwait:simulated a link carrying a side's bytes at its rate
			select {
			case <-crossing.C:
			case <-stop:
				crossing.Stop()
				return
			}
		}
		if _, err := dst.Write(c.bytes); err != nil {
			return
		}
		wrote(len(c.bytes))
	}
}

// sniffLimit is how much of what a node sends the relay reads for its CONNECT
// before deciding it binds no account: a CONNECT is a line of JSON of a few
// hundred bytes, a few kilobytes with a JWT, and is the first or second thing
// a node sends.
const sniffLimit = 64 << 10

var crlf = []byte("\r\n")

// sniffer reads the account a leaf connection binds out of the first things
// its node sends: the `remote_account` of its CONNECT (nats-server's
// leafConnectInfo), which a leaf sets to the local account the remote binds.
//
// A LEAF THAT NEGOTIATES COMPRESSION sends its INFO in the clear and every
// byte after it as s2 streams (nats-server's negotiateLeafCompression: one
// stream per flush, each with its own header), its CONNECT among them; one
// that does not sends its CONNECT in the clear. A client's CONNECT names no
// account, and anything else — a TLS handshake, a websocket upgrade — shows
// the relay none.
type sniffer struct {
	// plain is what the node sent in the clear that is not yet a whole
	// line.
	plain []byte
	// compressed is set once the node's INFO said everything after it is
	// an s2 stream, zipped what it has sent of that, and seen how much of
	// zipped is whole chunks, already read for a CONNECT.
	compressed bool
	zipped     []byte
	seen       int
}

// feed reads b, the next bytes the node sent, and is the account its CONNECT
// names once decided: the empty name for a connection that binds none.
func (s *sniffer) feed(b []byte) (name string, decided bool) {
	if s.compressed {
		s.zipped = append(s.zipped, b...)
		return s.fromZipped()
	}
	s.plain = append(s.plain, b...)
	i := bytes.Index(s.plain, crlf)
	if i < 0 {
		return "", len(s.plain) > sniffLimit
	}
	line, rest := s.plain[:i], s.plain[i+len(crlf):]
	switch {
	case bytes.HasPrefix(line, []byte("CONNECT ")):
		return remoteAccount(line), true
	case bytes.HasPrefix(line, []byte("INFO ")):
		s.compressed, s.zipped, s.plain = true, append([]byte(nil), rest...), nil
		return s.fromZipped()
	default:
		return "", true
	}
}

// fromZipped reads the CONNECT out of the whole chunks of the s2 stream the
// node has sent so far, waiting for more while there is none yet.
func (s *sniffer) fromZipped() (string, bool) {
	whole := wholeChunks(s.zipped)
	if whole == s.seen {
		return "", len(s.zipped) > sniffLimit
	}
	s.seen = whole
	text, err := io.ReadAll(s2.NewReader(bytes.NewReader(s.zipped[:whole])))
	if err != nil {
		return "", true
	}
	for len(text) > 0 {
		i := bytes.Index(text, crlf)
		if i < 0 {
			break
		}
		if line := text[:i]; bytes.HasPrefix(line, []byte("CONNECT ")) {
			return remoteAccount(line), true
		}
		text = text[i+len(crlf):]
	}
	return "", len(s.zipped) > sniffLimit
}

// wholeChunks is how many leading bytes of z are whole s2 chunks: each a
// one-byte type and a three-byte little-endian length, then that many bytes.
func wholeChunks(z []byte) int {
	n := 0
	for len(z)-n >= 4 {
		size := int(z[n+1]) | int(z[n+2])<<8 | int(z[n+3])<<16
		if len(z)-n-4 < size {
			break
		}
		n += 4 + size
	}
	return n
}

// remoteAccount is the account a CONNECT line names, the empty name for one
// that names none.
func remoteAccount(line []byte) string {
	var connect struct {
		RemoteAccount string `json:"remote_account"`
	}
	if json.Unmarshal(bytes.TrimSpace(line[len("CONNECT "):]), &connect) != nil {
		return ""
	}
	return connect.RemoteAccount
}
