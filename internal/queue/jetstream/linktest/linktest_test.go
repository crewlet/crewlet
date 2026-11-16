package linktest

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// floorRate is the link a shared case paces at: 64 KiB a second, so a
// segment crosses in 22.3 ms (quantum below).
const floorRate = 64 << 10

// ethernetSegment is one Ethernet segment's payload, the [Quantum] a fair
// link serves a connection by: stated here rather than read off the package,
// so a case's bound is the design's and a relay that serves by any other size
// is red.
const ethernetSegment = 1460

// quantum is how long one segment takes to cross a link at floorRate, as the
// relay times it.
var quantum = crossing(ethernetSegment)

// crossing is how long n bytes take to cross a link at floorRate.
func crossing(n int) time.Duration {
	return time.Duration(n) * time.Second / floorRate
}

// pipes is the broker's side of a link a case builds inside a bubble: every
// connection the link dials is an in-memory pipe, whose far end the case
// reads and writes as the broker. A pipe blocks durably, where a loopback
// socket does not, so the bubble's clock moves only when every byte that can
// move has, and a crossing's time is the link's arithmetic, exactly.
type pipes struct {
	dialled chan net.Conn
}

// bubbleLink is a link carrying toward and back, started in the case's
// bubble, and the broker's side of it.
func bubbleLink(t *testing.T, toward, back carrier) (*Link, *pipes) {
	t.Helper()
	far := &pipes{dialled: make(chan net.Conn)}
	l := newLink(far.dial, toward, back)
	l.start(t)
	return l, far
}

func (p *pipes) dial(ctx context.Context) (net.Conn, error) {
	relay, broker := net.Pipe()
	select {
	case p.dialled <- broker:
		return relay, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// connect carries a new connection across l, and is its node's end and its
// broker's.
func (p *pipes) connect(t *testing.T, l *Link) (node, broker net.Conn) {
	t.Helper()
	node, relay := net.Pipe()
	l.wg.Go(func() { l.carry(t.Context(), relay) })
	broker = <-p.dialled
	t.Cleanup(func() {
		_ = node.Close()
		_ = broker.Close()
	})
	return node, broker
}

// write sends n bytes on c from a goroutine of its own, as a sender whose
// socket takes them does.
func write(c net.Conn, n int) {
	go func() { _, _ = c.Write(make([]byte, n)) }()
}

// whenRead is the instant r has given n bytes, read in a goroutine of its own
// that reads on after it until r ends.
func whenRead(r io.Reader, n int) <-chan time.Time {
	at := make(chan time.Time, 1)
	go func() {
		buf := make([]byte, 4096)
		total := 0
		for {
			k, err := r.Read(buf)
			total += k
			if total >= n && at != nil {
				at <- time.Now()
				at = nil
			}
			if err != nil {
				return
			}
		}
	}()
	return at
}

// tally counts what r gives, in a goroutine of its own, until r ends.
func tally(r io.Reader) *atomic.Int64 {
	var n atomic.Int64
	go func() {
		buf := make([]byte, 4096)
		for {
			k, err := r.Read(buf)
			n.Add(int64(k))
			if err != nil {
				return
			}
		}
	}()
	return &n
}

// TWO CONNECTIONS THAT BOTH HAVE BYTES TO SEND SHARE A FAIR LINK EVENLY: each
// is served a quantum in turn, so the two finish a quantum apart, together
// in the time their bytes take at the link's whole rate — never one after
// the other, as a first-in, first-out link would carry them, and never each
// at the whole rate, as a link that paced each connection alone would. Both
// ways, since a node's link is shared both ways.
//
// Red first: carried each connection at the whole rate on its own ([Slow]'s
// pacing), both finish at 64 quanta, half the time the shared link takes.
// Mutant: SharedEachWay paces first in, first out, and the first finishes at
// 64 quanta.
func TestTwoConnectionsAcrossAFairLinkEachGetAboutHalf(t *testing.T) {
	const bulk = 64 * ethernetSegment
	for _, way := range []string{"towards the broker", "back towards the node"} {
		t.Run(way, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				toward, back := sharedEachWay(floorRate)
				l, far := bubbleLink(t, toward, back)
				var done [2]<-chan time.Time
				start := time.Now()
				for i := range done {
					node, broker := far.connect(t, l)
					from, to := node, broker
					if way != "towards the broker" {
						from, to = broker, node
					}
					done[i] = whenRead(to, bulk)
					write(from, bulk)
				}
				first, last := <-done[0], <-done[1]
				if last.Before(first) {
					first, last = last, first
				}
				if got, want := last.Sub(start), 128*quantum; got != want {
					t.Errorf("both connections' %d bytes crossed in %v, want %v: the link's whole rate, shared",
						2*bulk, got, want)
				}
				if got, want := first.Sub(start), 127*quantum; got < want {
					t.Errorf("the first connection finished at %v, want no sooner than %v: a quantum before the other, each served in turn",
						got, want)
				}
			})
		})
	}
}

// ONE CONNECTION ALONE CROSSES A SHARED LINK AT ITS WHOLE RATE, an idle one
// beside it taking no turn: a fair link divides its rate among the
// connections with bytes waiting, not among every connection it carries.
//
// Mutant: the pacer serves an idle connection's turn as though it had a
// quantum to send, and the bulk finishes at 128 quanta.
func TestOneConnectionAloneGetsTheWholeRate(t *testing.T) {
	const bulk = 64 * ethernetSegment
	synctest.Test(t, func(t *testing.T) {
		toward, back := sharedEachWay(floorRate)
		l, far := bubbleLink(t, toward, back)
		node, broker := far.connect(t, l)
		_, _ = far.connect(t, l)
		start := time.Now()
		done := whenRead(broker, bulk)
		write(node, bulk)
		if got, want := (<-done).Sub(start), 64*quantum; got != want {
			t.Errorf("%d bytes crossed in %v, want %v: the link's whole rate", bulk, got, want)
		}
	})
}

// A SMALL WRITE CROSSES A FAIR LINK WITHIN A QUANTUM OF THE BULK BESIDE IT:
// sent while another connection has 64 quanta queued, it waits only for the
// rest of the quantum crossing as it arrives, and then its own bytes — the
// bound a case derives from [Quantum], whatever the bulk has queued.
//
// Mutants: SharedEachWay paces first in, first out, and the write waits behind
// the whole bulk; a 64 KiB quantum, and it waits behind most of a second of
// it.
func TestASmallWriteCrossesAFairLinkWithinAQuantumOfTheBulkBesideIt(t *testing.T) {
	const bulk, small = 64 * ethernetSegment, 100
	synctest.Test(t, func(t *testing.T) {
		toward, back := sharedEachWay(floorRate)
		l, far := bubbleLink(t, toward, back)
		bulkNode, _ := far.connect(t, l)
		smallNode, smallBroker := far.connect(t, l)
		write(bulkNode, bulk)
		// HALFWAY THROUGH THE BULK'S SECOND QUANTUM, so the write meets a
		// quantum already crossing, the worst it can.
		time.Sleep(quantum + quantum/2)
		sent := time.Now()
		done := whenRead(smallBroker, small)
		write(smallNode, small)
		if got, bound := (<-done).Sub(sent), quantum+crossing(small); got > bound {
			t.Errorf("a %d-byte write beside a bulk crossed in %v, want at most %v: one quantum of the bulk and its own bytes",
				small, got, bound)
		}
	})
}

// A FIRST-IN, FIRST-OUT LINK HOLDS A WRITE BEHIND THE BYTES OTHERS QUEUED
// AHEAD OF IT: sent while another connection has 16 quanta queued, it
// crosses once all 16 have, and no later — what a renewal meets at a
// bottleneck with no fair queueing.
//
// Mutant: SharedFIFO serves its connections in turn, and the write crosses a
// quantum and a half after it was sent.
func TestAFIFOLinkHoldsAWriteBehindTheOthersQueuedBytes(t *testing.T) {
	const queued, small = 16 * ethernetSegment, 100
	synctest.Test(t, func(t *testing.T) {
		toward, back := sharedFIFO(floorRate, 64*ethernetSegment)
		l, far := bubbleLink(t, toward, back)
		bulkNode, _ := far.connect(t, l)
		smallNode, smallBroker := far.connect(t, l)
		start := time.Now()
		write(bulkNode, queued)
		time.Sleep(quantum + quantum/2)
		done := whenRead(smallBroker, small)
		write(smallNode, small)
		if got, want := (<-done).Sub(start), crossing(queued)+crossing(small); got != want {
			t.Errorf("a %d-byte write sent behind %d queued bytes crossed %v after them, want %v: behind every one, and then its own",
				small, queued, got, want)
		}
	})
}

// A FIRST-IN, FIRST-OUT LINK QUEUES NO MORE THAN ITS INFLIGHT OF ONE
// CONNECTION: a sender of 64 quanta through an inflight of 8 has its last
// byte taken only once all but 8 quanta have crossed, so its write blocks as
// a sender's does once its window is out; while another connection, which
// has nothing queued, has its write taken at once.
//
// Mutant: SharedFIFO without its cap, and the bulk's write returns before a
// byte has crossed.
func TestAFIFOLinkQueuesNoMoreThanItsInflightOfOneConnection(t *testing.T) {
	const bulk, inflight, small = 64 * ethernetSegment, 8 * ethernetSegment, 100
	synctest.Test(t, func(t *testing.T) {
		toward, back := sharedFIFO(floorRate, inflight)
		l, far := bubbleLink(t, toward, back)
		bulkNode, bulkBroker := far.connect(t, l)
		smallNode, _ := far.connect(t, l)
		crossed := tally(bulkBroker)
		start := time.Now()
		written := make(chan time.Time, 1)
		go func() {
			_, _ = bulkNode.Write(make([]byte, bulk))
			written <- time.Now()
		}()
		time.Sleep(crossing(bulk) / 2)
		before := time.Now()
		if _, err := smallNode.Write(make([]byte, small)); err != nil {
			t.Fatalf("write beside the bulk: %v", err)
		}
		if waited := time.Since(before); waited != 0 {
			t.Errorf("a %d-byte write beside a bulk at its inflight was taken after %v, want at once: the cap is each connection's own",
				small, waited)
		}
		at := <-written
		synctest.Wait()
		if got, least := crossed.Load(), int64(bulk-inflight); got < least {
			t.Errorf("when the bulk's write of %d bytes returned, %v after it began, %d had crossed, want at least %d: no more than its inflight of %d queued",
				bulk, at.Sub(start), got, least, inflight)
		}
	})
}

// A WATCHER IS HANDED EACH PIECE A LINK CARRIES AS IT CARRIES IT: across a
// first-in, first-out link, every quantum the far side is written is one
// crossing, a quantum's time after the one before, its count of what was
// carried one quantum more, and what was read never more than the
// connection's inflight ahead of it — what a case reads a node's socket
// against at the instant the link has carried a known count. The count
// keeps a connection's bytes once it has ended.
//
// Red first: with no count of what the link carried, a case could read a
// node's socket only against what it had itself sent. Mutants: the link
// counts as carried what it reads, and the first crossing reports all of it;
// the watcher is told once a read rather than once a piece, and the
// crossings are not one a quantum.
func TestAWatcherIsHandedEachPieceALinkCarriesAsItCarriesIt(t *testing.T) {
	const pieces, inflight = 16, 8 * ethernetSegment
	synctest.Test(t, func(t *testing.T) {
		toward, back := sharedFIFO(floorRate, inflight)
		l, far := bubbleLink(t, toward, back)
		var seen []Crossing
		var at []time.Duration
		start := time.Now()
		l.Watch(func(c Crossing) {
			seen = append(seen, c)
			at = append(at, time.Since(start))
		})
		node, broker := far.connect(t, l)
		done := whenRead(broker, pieces*ethernetSegment)
		write(node, pieces*ethernetSegment)
		<-done
		synctest.Wait()
		if len(seen) != pieces {
			t.Fatalf("the watcher was handed %d crossings, want %d: one a quantum", len(seen), pieces)
		}
		for i, c := range seen {
			if c.Account != "" || c.Way != ToBroker || c.Bytes != ethernetSegment {
				t.Errorf("crossing %d is %+v, want %d bytes of no account %s", i, c, ethernetSegment, ToBroker)
			}
			if want := int64((i + 1) * ethernetSegment); c.Count.Carried != want {
				t.Errorf("crossing %d counts %d bytes carried, want %d", i, c.Count.Carried, want)
			}
			if ahead := c.Count.Read - c.Count.Carried; ahead < 0 || ahead > inflight {
				t.Errorf("crossing %d counts %d bytes read ahead of what was carried, want at most the inflight of %d",
					i, ahead, inflight)
			}
			if want := time.Duration(i+1) * quantum; at[i] != want {
				t.Errorf("crossing %d came at %v, want %v: as the piece crossed", i, at[i], want)
			}
		}
		_ = node.Close()
		synctest.Wait()
		if got, want := l.Count("", ToBroker), (Count{Read: pieces * ethernetSegment, Carried: pieces * ethernetSegment}); got != want {
			t.Errorf("the link counts %+v once the connection ended, want %+v: kept", got, want)
		}
	})
}

// fastRate is the rate a case carries a real leaf's links at, where what it
// is about is not the pace: a handshake crosses in microseconds.
const fastRate = 8 << 20

// kinds are every link the package builds, opened to target.
var kinds = []struct {
	name string
	open func(t testing.TB, target string) *Link
}{
	{"slow", func(t testing.TB, target string) *Link { return Slow(t, target, fastRate) }},
	{"slow each way", func(t testing.TB, target string) *Link { return SlowEachWay(t, target, fastRate) }},
	{"lagging", func(t testing.TB, target string) *Link { return Lagging(t, target) }},
	{"shared each way", func(t testing.TB, target string) *Link { return SharedEachWay(t, target, fastRate) }},
	{"shared first in, first out", func(t testing.TB, target string) *Link {
		return SharedFIFO(t, target, fastRate, 64*ethernetSegment)
	}},
}

// leases is the second local account the leaf joins the member from, as a
// node joins its lease link's.
const leases = "LEASES"

// accounts are the leaf's two local accounts: nats-server's global one and
// leases.
var accounts = []string{server.DEFAULT_GLOBAL_ACCOUNT, leases}

// leafFleet is a member serving leaves, a link in front of its leaf listener,
// and a leaf joined to it across that link from two local accounts, with a
// client of the leaf in each and a responder on the member answering them.
type leafFleet struct {
	leaf    *server.Server
	link    *Link
	clients map[string]*nats.Conn
}

// newLeafFleet starts a leaf fleet across a link open builds, its two remotes
// compressing as mode says, and waits until a request from each account is
// answered across it.
//
// THE MEMBER ADVERTISES THE LINK, which is opened first with no target and
// named the member's listener once it has one ([Link]'s "The link is the only
// way"): advertising its own listener, the member handed the leaf a URL the
// leaf dialled first on every redial, and a cut account's link came straight
// back on loopback with the link refusing nothing.
func newLeafFleet(t *testing.T, open func(testing.TB, string) *Link, mode string) leafFleet {
	t.Helper()
	link := open(t, "")
	member := startServer(t, &server.Options{ServerName: "member", Host: "127.0.0.1", Port: -1,
		NoLog: true, NoSigs: true,
		LeafNode: server.LeafNodeOpts{Host: "127.0.0.1", Port: -1, Advertise: link.Addr()}})
	varz, err := member.Varz(nil)
	if err != nil {
		t.Fatalf("read the member's leaf port: %v", err)
	}
	link.To(fmt.Sprintf("127.0.0.1:%d", varz.LeafNode.Port))
	u, err := url.Parse("nats-leaf://" + link.Addr())
	if err != nil {
		t.Fatal(err)
	}
	leaseAccount := server.NewAccount(leases)
	var remotes []*server.RemoteLeafOpts
	for _, name := range accounts {
		remotes = append(remotes, &server.RemoteLeafOpts{LocalAccount: name, URLs: []*url.URL{u},
			Compression: server.CompressionOpts{Mode: mode}})
	}
	leaf := startServer(t, &server.Options{ServerName: "leaf", Host: "127.0.0.1", Port: -1,
		NoLog: true, NoSigs: true, Accounts: []*server.Account{leaseAccount},
		Users: []*server.User{{Username: "global", Password: "global"},
			{Username: "leases", Password: "leases", Account: leaseAccount}},
		LeafNode: server.LeafNodeOpts{Remotes: remotes, ReconnectInterval: 20 * time.Millisecond}})

	responder, err := nats.Connect(member.ClientURL())
	if err != nil {
		t.Fatalf("connect to the member: %v", err)
	}
	t.Cleanup(responder.Close)
	if _, err := responder.Subscribe("echo", func(m *nats.Msg) { _ = m.Respond(m.Data) }); err != nil {
		t.Fatalf("answer on the member: %v", err)
	}
	if err := responder.Flush(); err != nil {
		t.Fatal(err)
	}
	f := leafFleet{leaf: leaf, link: link, clients: map[string]*nats.Conn{}}
	for name, user := range map[string]string{server.DEFAULT_GLOBAL_ACCOUNT: "global", leases: "leases"} {
		nc, err := nats.Connect(leaf.ClientURL(), nats.UserInfo(user, user))
		if err != nil {
			t.Fatalf("connect to the leaf in %s: %v", name, err)
		}
		t.Cleanup(nc.Close)
		f.clients[name] = nc
	}
	for _, name := range accounts {
		eventually(t, name+"'s request answered across the link", func() bool {
			_, err := f.clients[name].Request("echo", []byte(name), 200*time.Millisecond)
			return err == nil
		})
	}
	return f
}

// startServer starts a nats-server on opts that ends with the case.
func startServer(t *testing.T, opts *server.Options) *server.Server {
	t.Helper()
	s, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("build %s: %v", opts.ServerName, err)
	}
	s.Start()
	t.Cleanup(func() {
		s.Shutdown()
		s.WaitForShutdown()
	})
	if !s.ReadyForConnections(10 * time.Second) {
		t.Fatalf("%s did not start", opts.ServerName)
	}
	return s
}

// links is the leaf's link to the member in each local account, by the
// connection's id: a link closed and dialled again has a new one.
func (f leafFleet) links(t *testing.T) map[string]uint64 {
	t.Helper()
	leafz, err := f.leaf.Leafz(nil)
	if err != nil {
		t.Fatalf("read the leaf's links: %v", err)
	}
	ids := map[string]uint64{}
	for _, l := range leafz.Leafs {
		ids[l.Account] = l.ID
	}
	return ids
}

// answers fails the case unless a request from the leaf's client in account
// is answered across the link.
func (f leafFleet) answers(t *testing.T, account string) {
	t.Helper()
	if _, err := f.clients[account].Request("echo", []byte(account), 10*time.Second); err != nil {
		t.Fatalf("a request from %s across the link: %v", account, err)
	}
}

// eventually waits for cond, failing the case once a bound far past any it
// should take has passed.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	bound := time.NewTimer(30 * time.Second) //fixedwait:liveness
	defer bound.Stop()
	poll := time.NewTicker(5 * time.Millisecond) //fixedwait:liveness
	defer poll.Stop()
	for !cond() {
		select {
		case <-bound.C:
			t.Fatalf("still waiting for %s", what)
		case <-poll.C:
		}
	}
}

// refused is how many connections binding name the link closed because the
// account was cut.
func (l *Link) refused(name string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.account(name).refused
}

// boundAccounts is the account each connection the link carries binds, by
// how many bind it.
func (l *Link) boundAccounts() map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	names := map[string]int{}
	for p := range l.pairs {
		if p.bound {
			names[p.account]++
		}
	}
	return names
}

// A LINK CUTS ONE LEAF ACCOUNT'S LINK AND CARRIES THE OTHER'S: a leaf joined
// to its member from two local accounts dials the same address for both, and
// Cut of the global account closes that link and refuses every redial of it,
// while the other account's link — the same connection throughout — still
// answers; Mend lets the global account link again. On every link the
// package builds.
//
// Mutant: Cut matches every connection, and the other account's link is
// closed and dialled again.
func TestALinkCutsOneLeafAccountAndCarriesTheOther(t *testing.T) {
	for _, kind := range kinds {
		t.Run(kind.name, func(t *testing.T) {
			f := newLeafFleet(t, kind.open, server.CompressionS2Auto)
			before := f.links(t)
			f.link.Cut(server.DEFAULT_GLOBAL_ACCOUNT)
			eventually(t, "a redial of the cut account refused", func() bool {
				return f.link.refused(server.DEFAULT_GLOBAL_ACCOUNT) >= 2
			})
			eventually(t, "the cut account's link gone from the leaf", func() bool {
				_, up := f.links(t)[server.DEFAULT_GLOBAL_ACCOUNT]
				return !up
			})
			if got, want := f.links(t)[leases], before[leases]; got != want {
				t.Fatalf("the %s link is connection %d after the cut, want %d: cut with the other account",
					leases, got, want)
			}
			f.answers(t, leases)
			f.link.Mend(server.DEFAULT_GLOBAL_ACCOUNT)
			eventually(t, "the mended account linked again", func() bool {
				_, up := f.links(t)[server.DEFAULT_GLOBAL_ACCOUNT]
				return up
			})
			f.answers(t, server.DEFAULT_GLOBAL_ACCOUNT)
		})
	}
}

// A DROPPED ACCOUNT'S LINK DIALS AGAIN, AND THE OTHER STAYS: Drop closes the
// global account's link once, which the leaf dials again at once, while the
// other account's link is the same connection throughout.
//
// Mutant: Drop matches every connection, and the other account's link is
// dialled again too.
func TestADroppedAccountsLinkDialsAgainAndTheOtherStays(t *testing.T) {
	for _, kind := range kinds {
		t.Run(kind.name, func(t *testing.T) {
			f := newLeafFleet(t, kind.open, server.CompressionS2Auto)
			before := f.links(t)
			f.link.Drop(server.DEFAULT_GLOBAL_ACCOUNT)
			eventually(t, "the dropped account linked again", func() bool {
				id, up := f.links(t)[server.DEFAULT_GLOBAL_ACCOUNT]
				return up && id != before[server.DEFAULT_GLOBAL_ACCOUNT]
			})
			if got, want := f.links(t)[leases], before[leases]; got != want {
				t.Fatalf("the %s link is connection %d after the drop, want %d: dropped with the other account",
					leases, got, want)
			}
			f.answers(t, server.DEFAULT_GLOBAL_ACCOUNT)
			f.answers(t, leases)
		})
	}
}

// A BLACKHOLED ACCOUNT'S LINK CARRIES NOTHING UNTIL IT IS MENDED, AND IS NOT
// CLOSED: a request from the global account reaches the relay's socket and
// waits there unread, within the pinned buffer, while the link reads none of
// the account's bytes and the other account's requests are answered; once
// mended the request crosses and is answered over the same connection.
//
// Red first: a Blackhole that read and held what the leaf sent took the
// request off the socket, and nothing ever waited there unread. Mutant:
// Blackhole holds nothing, and the request crosses and nothing waits.
func TestABlackholedAccountCarriesNothingUntilMended(t *testing.T) {
	for _, kind := range kinds {
		t.Run(kind.name, func(t *testing.T) {
			f := newLeafFleet(t, kind.open, server.CompressionS2Auto)
			before := f.links(t)
			f.link.Blackhole(server.DEFAULT_GLOBAL_ACCOUNT)
			read := f.link.Count(server.DEFAULT_GLOBAL_ACCOUNT, ToBroker).Read
			answered := make(chan error, 1)
			go func() {
				_, err := f.clients[server.DEFAULT_GLOBAL_ACCOUNT].Request("echo", []byte("held"), 30*time.Second)
				answered <- err
			}()
			// WHAT CARRIES THE REQUEST WAITS IN THE RELAY'S SOCKET, the
			// interest in its reply first and then the request, unread.
			eventually(t, "the request waiting unread in the blackholed link's socket", func() bool {
				unread, err := f.link.Unread(server.DEFAULT_GLOBAL_ACCOUNT, ToBroker)
				if err != nil {
					t.Fatalf("read the link's socket: %v", err)
				}
				if most := f.link.Buffers().Receive; unread > most {
					t.Fatalf("%d bytes wait unread in the blackholed link's socket, want at most its pinned receive buffer's %d",
						unread, most)
				}
				return unread > len("held")
			})
			if got := f.link.Count(server.DEFAULT_GLOBAL_ACCOUNT, ToBroker).Read; got != read {
				t.Fatalf("the blackholed link read %d bytes of the account, %d when the blackhole began: it reads none",
					got, read)
			}
			f.answers(t, leases)
			select {
			case err := <-answered:
				t.Fatalf("the request across the blackholed link was answered before it was mended: %v", err)
			default:
			}
			f.link.Mend(server.DEFAULT_GLOBAL_ACCOUNT)
			if err := <-answered; err != nil {
				t.Fatalf("the request held on the blackholed link, once mended: %v", err)
			}
			if got, want := f.links(t), before; got[server.DEFAULT_GLOBAL_ACCOUNT] != want[server.DEFAULT_GLOBAL_ACCOUNT] ||
				got[leases] != want[leases] {
				t.Fatalf("the leaf's links are %v after the blackhole, want %v: held, never closed", got, want)
			}
		})
	}
}

// A LINK READS THE ACCOUNT A LEAF BINDS WHATEVER ITS COMPRESSION: a leaf that
// negotiates s2 sends its CONNECT inside an s2 stream, at every level a remote
// may name (s2_auto picking s2_uncompressed on a loopback's round trip), and
// one that does not sends it in the clear; the link binds both of the leaf's
// links to their accounts either way.
//
// Mutant: the relay reads the CONNECT in the clear alone, and no compressed
// link binds an account.
func TestALinkReadsTheAccountALeafBindsWhateverItsCompression(t *testing.T) {
	for _, mode := range []string{server.CompressionOff, server.CompressionS2Fast,
		server.CompressionS2Better, server.CompressionS2Best, server.CompressionS2Auto} {
		t.Run(mode, func(t *testing.T) {
			f := newLeafFleet(t, func(t testing.TB, target string) *Link {
				return SharedEachWay(t, target, fastRate)
			}, mode)
			leafz, err := f.leaf.Leafz(nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, l := range leafz.Leafs {
				if mode != server.CompressionS2Auto && !strings.HasPrefix(l.Compression, mode) {
					t.Fatalf("the leaf's %s link compresses %q, want %q: the case did not reach the mode it names",
						l.Account, l.Compression, mode)
				}
			}
			bound := f.link.boundAccounts()
			for _, name := range accounts {
				if bound[name] != 1 {
					t.Errorf("the link binds %v, want one connection in each of %v", bound, accounts)
					break
				}
			}
		})
	}
}
