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
// [Link.Mend], and [Link.Blackhole] stops carrying them either way without
// closing them, until [Link.Mend]. A connection that binds no account — a
// client's CONNECT names none, and a TLS or websocket link shows the relay
// none — is never one of them. Every link the package builds does this, and
// for a leaf that dials again only behind a member that advertises the link
// ([Link]'s "The link is the only way").
package linktest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"slices"
	"sync"
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
// sent, each way, as a loaded broker answers every request a while late. Or
// every connection it carries may share one bottleneck, fair
// ([SharedEachWay]) or first in, first out ([SharedFIFO]).
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
// [SharedFIFO] alone pushes back, and only past a connection's inflight: that
// is the bottleneck it plays, whose senders' socket buffers fill once their
// window is out, and its doc says what a case keeps clear of.
//
// # The link is the only way
//
// only while the broker tells the node of no other. A member hands every leaf
// the leaf URLs it advertises — its own listener's address, unless told
// otherwise — and a leaf that dials again tries those BEFORE the URL it was
// configured with (nats-server's doUpdateLNURLs and pickNextURL), so behind
// an ordinary member every redial walks around the link onto loopback: a
// [Link.Cut] or [Link.Drop] then cuts nothing, and a link that closed for any
// reason is slow no more. A member skips an advertised URL that is the
// leaf's configured one, so a case whose leaf may dial again opens the link
// with no target, starts the member advertising the link's [Link.Addr]
// (nats-server's LeafNode.Advertise, the jetstream package's LeafAdvertise),
// and then names the member's listener with [Link.To] — on a real network
// the address a member advertises is reached over the same link. A member
// that advertises nothing (LeafNode.NoAdvertise) serves as well.
//
// It ends with the case that started it: its listener, every connection it
// carries and every goroutine it runs.
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
// it on to target until the case ends, every connection's bytes in each
// direction crossing ONE first-in, first-out queue at rate bytes a second:
// the link behind a bottleneck with no fair queueing, where a small write
// waits behind every byte the other connections queued ahead of it.
//
// INFLIGHT IS A CONNECTION'S CONGESTION WINDOW. The relay reads a connection
// only while it has fewer than inflight bytes queued in that direction, and
// never more than that, so each flow keeps at most inflight bytes in the
// bottleneck, as TCP's window does. Past that the sender's socket buffer
// fills and its writes block: a nats.go client then holds its connection's
// lock in its write, and a broker's write stalls. A member's leaf writes keep
// moving within its write deadline (10 s) only while inflight / rate stays
// under it, so a case keeps inflight below ten seconds of its rate.
// What the broker sends is held back for as long as a [Link.Delay] says.
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
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind the link: %v", err)
	}
	l := newLink(nil, toward, back)
	l.ln, l.target, l.dial = ln, target, l.dialTarget
	l.start(t)
	return l
}

// dialTarget dials the broker the link carries to, in ctx.
func (l *Link) dialTarget(ctx context.Context) (net.Conn, error) {
	l.mu.Lock()
	target := l.target
	l.mu.Unlock()
	if target == "" {
		return nil, errNoTarget
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", target)
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

// To names target as the broker every connection the link takes from now on
// is carried to: for a link opened with no target, which refuses every
// connection until it has one. Open the link first, start the broker
// advertising its [Link.Addr], and then name the broker's own listener here
// ([Link]'s "The link is the only way").
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

// Blackhole stops carrying what every connection that binds account sends,
// either way, from now on and for every one that binds it later, until
// [Link.Mend], without closing any of them: a path that drops every packet.
// What they send is read as fast as it comes and held, and [Link.Mend]
// carries it on in order, as the senders' retransmissions arrive once a path
// heals; the link answers nothing meanwhile, so whatever liveness the two
// ends keep is what notices.
func (l *Link) Blackhole(account string) {
	for _, p := range l.bound(account, func(a *accountState) { a.silent = true }) {
		p.silence(true)
	}
}

// Mend undoes [Link.Cut] and [Link.Blackhole] for account: its links are
// taken again, and what a blackholed one held is carried on, in order.
func (l *Link) Mend(account string) {
	for _, p := range l.bound(account, func(a *accountState) { a.cut, a.silent = false, false }) {
		p.silence(false)
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
	if a.silent {
		p.silence(true)
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
// has begun, so no connection outlives the link.
func (l *Link) track(p *pair) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.shut {
		return false
	}
	l.pairs[p] = struct{}{}
	return true
}

// untrack forgets p once it has ended.
func (l *Link) untrack(p *pair) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.pairs, p)
}

// carry relays in to a connection of its own to the target, dialled in ctx,
// both ways, until either side ends, and then closes both.
func (l *Link) carry(ctx context.Context, in net.Conn) {
	out, err := l.dial(ctx)
	if err != nil {
		_ = in.Close()
		return
	}
	p := &pair{in: in, out: out, stop: make(chan struct{})}
	if !l.track(p) {
		p.close()
		return
	}
	defer l.untrack(p)
	// EITHER SIDE ENDING ENDS THE OTHER: closing the pair fails the read or
	// the write each of its goroutines is blocked in, and stops every wait
	// on the link's pace.
	toward := l.leg(p, in, out, l.toward)
	toward.sniff = &sniffer{}
	back := l.leg(p, out, in, l.back)
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
	// silent holds what either side sends rather than carrying it
	// ([Link.Blackhole]).
	silent bool
	// legs are the pair's two directions, for silence to carry on what they
	// held and close to take off the shared pacers.
	legs []*leg
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

// silence holds what the pair carries, or carries on what it held, in the
// order it was sent.
func (p *pair) silence(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.silent = on
	if on {
		return
	}
	for _, g := range p.legs {
		for _, c := range g.parked {
			g.send(c)
		}
		g.parked = nil
	}
}

// leg is one direction of a carried connection: what src sends, carried to
// dst.
type leg struct {
	p        *pair
	src, dst net.Conn
	how      carrier
	// sniff reads the account the connection binds out of what the node
	// sends; nil once that is decided, and on the broker's direction.
	sniff *sniffer
	// own is the leg's own backlog, delivered at its own pace, where the
	// direction is not shared.
	own *backlog
	// flow is the leg's place on the shared pacer, where it is.
	flow *flow
	// parked is what the leg read while the pair was silent, in order;
	// written under the pair's lock.
	parked []chunk
}

// leg is the direction from src to dst of p, carried as how says.
func (l *Link) leg(p *pair, src, dst net.Conn, how carrier) *leg {
	g := &leg{p: p, src: src, dst: dst, how: how}
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
// ends or the pair does.
func (l *Link) read(g *leg) {
	for {
		n := readChunk
		if g.flow != nil {
			var ok bool
			if n, ok = g.flow.room(g.p.stop); !ok {
				return
			}
		}
		buf := make([]byte, n)
		k, err := g.src.Read(buf)
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
		if err != nil {
			return
		}
	}
}

// admit carries c on, or holds it while the pair is silent.
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
		g.flow.out.deliver(g.dst, 0, stop, nil)
		return
	}
	g.own.deliver(g.dst, g.how.rate, stop, hold)
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
	// queued is how many of the connection's bytes a fifo pacer has still
	// to carry.
	queued int
	// freed is signalled whenever a fifo pacer has carried some of them.
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
// left, waiting while that is none; false once stop is closed.
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
			f.queued += n
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

// carried hands what crossed to its connection, and makes room for more of it
// on a fifo pacer.
func (b *bottleneck) carried(f *flow, piece []byte) {
	b.mu.Lock()
	if f.left {
		b.mu.Unlock()
		return
	}
	if b.fifo {
		f.queued -= len(piece)
	}
	b.mu.Unlock()
	f.out.push(chunk{bytes: piece})
	select {
	case f.freed <- struct{}{}:
	default:
	}
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
// ([Link.Lag]). It returns once stop is closed or a write fails.
func (b *backlog) deliver(dst io.Writer, rate int64, stop <-chan struct{}, hold func() time.Duration) {
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
