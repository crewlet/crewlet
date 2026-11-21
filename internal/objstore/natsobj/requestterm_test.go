package natsobj_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/coord/kv/kvtest"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/natsobj"
	"github.com/crewlet/crewlet/internal/queue/jetstream/linktest"
)

// floorRate is the slowest pace the engine allows a link, in bytes a second:
// a mebibyte in [objstore.MiBPace], rounded down, so a link carrying it is no
// faster than the floor.
const floorRate = int64(objstore.MiB) * int64(time.Second) / int64(objstore.MiBPace)

// requestBudget is what a request's term leaves the bytes sent ahead of it to
// cross in: [jsapi.RequestTerm] less one [jsprovision.ReadTerm] for the
// request's own round trip. Spelled from the two terms here rather than read
// from the package that sizes the window by it, so a window sized by any other
// figure fails these cases.
const requestBudget = jsapi.RequestTerm - jsprovision.ReadTerm

// crossing is how long messages of an object take to cross a link at the
// floor, spelled from the floor's own pace rather than natsobj's figure.
func crossing(messages int) time.Duration {
	return time.Duration(int64(messages)*natsobj.MessageBytes) * objstore.MiBPace / objstore.MiB
}

// A NODE'S TRANSFERS ACROSS A LINK TAKE ITS ONE WINDOW, ONE PIECE AT A TIME.
//
// A request on a link waits behind the node's transfers both ways, so a
// bucket opened across a link takes the link's ONE window it was handed
// (jsapi.Window), never one of its own; a put's piece and a download's each
// fit it and two do not, so one crosses at a time whichever way it goes; and
// a member's own client keeps [natsobj.MemberWindow] messages.
//
// Mutation: open a bucket across a link on a window of its own, and this goes
// red.
func TestAWindowAcrossALinkCrossesWithinARequestsTerm(t *testing.T) {
	t.Parallel()
	if natsobj.MessageCrossing != crossing(1) {
		t.Fatalf("one message crosses a link at the floor in %v by natsobj's figure and %v by the "+
			"floor's own", natsobj.MessageCrossing, crossing(1))
	}
	link := jsapi.NewLinkWindow()
	window := link.Size()
	if c := time.Duration(int64(window) * int64(objstore.MiBPace) / objstore.MiB); c > requestBudget {
		t.Errorf("the link's window of %d bytes crosses the floor in %v, past the %v a request's %v "+
			"term leaves after its own round trip", window, c, requestBudget, jsapi.RequestTerm)
	}
	c := memberClient(t)
	cfg := natsobj.Config{MaxBytes: testCeiling, Replicas: 1, AcrossLink: true, Window: link}
	across := openWith(t, c, cfg)
	if got := across.Window(); got != link {
		t.Errorf("a bucket opened across a link transfers inside %p, want the link's window %p", got, link)
	}
	up, down := natsobj.PieceTake(across, natsobj.MessageBytes), natsobj.PieceBound(natsobj.MessageBytes)
	if up > window || down > window || up+down <= window || 2*down <= window {
		t.Errorf("a put's piece takes %d bytes and a download's %d of a window of %d: each must fit "+
			"alone and no two together", up, down, window)
	}
	member := openWith(t, c, natsobj.Config{MaxBytes: testCeiling, Replicas: 1})
	if got, one := member.Window().Size(), natsobj.PieceTake(member, 1); got != natsobj.MemberWindow ||
		one != 1 || natsobj.PieceTake(member, natsobj.MessageBytes) != 1 {
		t.Errorf("a bucket opened on a member's own client has a window of %d and a message takes %d of "+
			"it, want %d messages, one a message whatever its size", got, one, natsobj.MemberWindow)
	}
	for _, bad := range []natsobj.Config{
		{MaxBytes: testCeiling, Replicas: 1, AcrossLink: true},
		{MaxBytes: testCeiling, Replicas: 1, Window: jsapi.NewLinkWindow()},
	} {
		if _, err := natsobj.Open(t.Context(), c, bad); err == nil || !strings.Contains(err.Error(), "Config.Window") {
			t.Errorf("a bucket opened with %+v = %v, want it refused naming Config.Window", bad, err)
		}
	}
}

// A READ SENT BEHIND A FULL WINDOW IS ANSWERED INSIDE ITS TERM.
//
// A node without `data` sends its uploads and fleet-store reads over one leaf
// link, which the relay carries both ways at the floor and in the order it
// takes them off the leaf ([linktest.SlowEachWay]), as a first-in-first-out
// link does; here they go on two connections of the leaf's broker. An upload
// fills its window with bytes no compressing link shrinks, and a read is sent
// behind them. Mutation: size the window across a link by the shipped lease
// heartbeat again, and four messages, fifteen seconds at the floor, cross ahead.
func TestAReadSentBehindAFullWindowIsAnsweredInsideItsTerm(t *testing.T) {
	t.Parallel()
	// DECIDED BY ORDER, NEVER BY THE CLOCK. The member counts the upload's
	// messages as the link delivers them, again where the read's request
	// arrives ([watchReadArrival]): those arriving after the read was sent and
	// before it are what it crossed behind, however late the case reads the
	// count. They must cross the floor within what the term leaves after its
	// round trip ([requestBudget]) for the read to be answered inside
	// [jsapi.RequestTerm], on a link a loaded machine paces late too. The
	// read's minute only ends a case never answered.
	across := kvtest.SlowLeaf(t, floorRate)
	full := watchLeafLink(t, across.Link)
	b := openWith(t, client(t, across.LeafBroker), acrossLink())
	arrived := watchReadArrival(t, client(t, across.MemberBroker).Conn())
	window := b.Window().Size() / natsobj.PieceTake(b, natsobj.MessageBytes)
	account := dataLink
	taken := across.Link.Count(account, linktest.ToBroker).Read
	full.arm()

	body := make([]byte, (window+4)*natsobj.MessageBytes)
	if _, err := rand.Read(body); err != nil {
		t.Fatalf("random bytes: %v", err)
	}
	putCtx, stopPut := context.WithCancel(t.Context())
	put := make(chan error, 1)
	go func() { put <- b.Put(putCtx, "window", bytes.NewReader(body), objstore.PutMeta{}) }()
	// THE PUT IS STOPPED AND WAITED FOR before the leaf goes: it drains the
	// one window it has out and purges what it stored, once the link carries
	// again.
	defer func() {
		full.release()
		stopPut()
		<-put
	}()

	// THE WINDOW IS FULL ON THE LINK BY CONSTRUCTION: the link holds the
	// upload's bytes once part of its first message has crossed, so none of
	// the window reaches the member until the read is queued behind it; and
	// the read is sent only once the link has taken the whole window off the
	// leaf. A subscriber on the leaf seeing a message routed says nothing of
	// when the link takes it: nothing orders two clients' messages on the one
	// link, and the leaf put a read sent after it on the link first.
	ctx, cancel := context.WithTimeout(t.Context(), objstore.Stall)
	defer cancel()
	full.held(ctx, t)
	full.untilRead(ctx, t, account, taken+int64(window*natsobj.MessageBytes))
	before := arrived.pieces.Load()
	sent := across.Link.Count(account, linktest.ToBroker).Read
	read := make(chan error, 1)
	go func() {
		_, _, err := across.Leaf.Channel(ctx, "absent")
		read <- err
	}()
	// THE READ IS ON THE LINK, behind the window, once the link has taken
	// anything more off the leaf: the put waits for its window and the leaf's
	// pings are minutes apart, so what it takes now is the read.
	full.untilRead(ctx, t, account, sent+1)
	full.release()

	// A LIVENESS BOUND, deciding nothing: the claim is the count below.
	if err := <-read; err != nil {
		t.Fatalf("a fleet-store read sent behind a full window of %d messages on a link at the floor "+
			"was not answered within the %v the case waits for it: %v", window, objstore.Stall, err)
	}
	var atRead int64
	select {
	case atRead = <-arrived.atRead:
	case <-ctx.Done():
		t.Fatal("the read was answered, and the member's watch never saw its request arrive")
	}
	ahead := int(atRead - before)
	if ahead < 1 {
		t.Fatalf("none of the upload's messages crossed the link while the read waited, though the "+
			"link held its window of %d messages until the read was behind it", window)
	}
	if c := crossing(ahead); c > requestBudget {
		t.Errorf("%d of the upload's messages crossed the link ahead of a fleet-store read sent behind "+
			"its full window, %v at the floor: the read is answered past the %v its %v term leaves "+
			"after its own round trip", ahead, c, requestBudget, jsapi.RequestTerm)
	}
}

// leafLink watches what a leaf sends across its link, and once armed holds
// it back with an upload's bytes on it ([watchLeafLink]).
type leafLink struct {
	link  *linktest.Link
	armed atomic.Bool
	// heldAt is closed once the link holds what the leaf sends.
	heldAt chan struct{}
	// open lets the link carry again once closed ([leafLink.release]).
	open    chan struct{}
	opening sync.Once
}

// heldAfter is how much of what the leaf sends the link carries once armed
// before it holds the rest: a quarter of a message, which the put's own
// requests ahead of its first message, a few hundred bytes, never reach, and
// which the link's reads of at most 64 KiB pass inside the first message, so
// at least half of it is still to cross when the link holds.
const heldAfter = natsobj.MessageBytes / 4

// watchLeafLink watches link from now on. Once [leafLink.arm]ed, and once it
// has carried [heldAfter] of what the leaf sends from then, it holds the rest
// until [leafLink.release]: the way a crossing waits for its watch to return
// ([linktest.Link.Watch]).
func watchLeafLink(t *testing.T, link *linktest.Link) *leafLink {
	t.Helper()
	w := &leafLink{link: link, heldAt: make(chan struct{}), open: make(chan struct{})}
	t.Cleanup(w.release)
	var held sync.Once
	start := int64(-1)
	link.Watch(func(c linktest.Crossing) {
		if c.Way != linktest.ToBroker || c.Account != dataLink {
			return
		}
		if !w.armed.Load() {
			return
		}
		// ONE GOROUTINE CARRIES ONE CONNECTION'S WAY, and the leaf's link is
		// one connection, so start is never read and written at once.
		if start < 0 {
			start = c.Count.Carried - int64(c.Bytes)
		}
		if c.Count.Carried-start < heldAfter {
			return
		}
		held.Do(func() { close(w.heldAt) })
		<-w.open
	})
	return w
}

// dataLink is the account the leaf's DATA LINK binds, the one its uploads
// cross; the lease link beside it binds its own.
const dataLink = server.DEFAULT_GLOBAL_ACCOUNT

// arm makes the link hold what the leaf sends once [heldAfter] of it has
// crossed.
func (w *leafLink) arm() { w.armed.Store(true) }

// held waits until the link holds what the leaf sends.
func (w *leafLink) held(ctx context.Context, t *testing.T) {
	t.Helper()
	select {
	case <-w.heldAt:
	case <-ctx.Done():
		t.Fatalf("the link never carried %d bytes of the upload to hold the rest behind", heldAfter)
	}
}

// untilRead waits until the link has taken n bytes off the connections of
// account the leaf sends on, counted from when the link began: a poll whose
// own count decides, at a pace that only spends the processor.
func (w *leafLink) untilRead(ctx context.Context, t *testing.T, account string, n int64) {
	t.Helper()
	poll := time.NewTicker(time.Millisecond) //fixedwait:liveness
	defer poll.Stop()
	for w.link.Count(account, linktest.ToBroker).Read < n {
		select {
		case <-poll.C:
		case <-ctx.Done():
			t.Fatalf("the link took %d bytes off the leaf, waiting for %d",
				w.link.Count(account, linktest.ToBroker).Read, n)
		}
	}
}

// release lets the link carry again.
func (w *leafLink) release() { w.opening.Do(func() { close(w.open) }) }

// readArrival is what [watchReadArrival] sees on the member: the upload's
// messages so far, and how many had arrived when the first leader read of a
// KV bucket did.
type readArrival struct {
	pieces atomic.Int64
	atRead chan int64
}

// watchReadArrival watches nc, a connection of the member, for the upload's
// messages and the fleet store's leader reads (the only KV bucket reads
// across the link while the case runs), in the order the link delivered them:
// every subscription feeds one channel, which nats.go fills from the
// connection's one read loop in the order the server sent, so the count at
// the read's arrival is the link's order and never when a goroutine ran. A
// leaf's read names the fleet's domain, which the member maps to its own API,
// so both spellings are watched.
func watchReadArrival(t *testing.T, nc *nats.Conn) *readArrival {
	t.Helper()
	w := &readArrival{atRead: make(chan int64, 1)}
	msgs := make(chan *nats.Msg, 1<<12)
	for _, subject := range []string{"$O." + natsobj.Bucket + ".C.>",
		"$JS.API.STREAM.MSG.GET.>", "$JS.*.API.STREAM.MSG.GET.>"} {
		sub, err := nc.ChanSubscribe(subject, msgs)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		read := false
		for {
			select {
			case m := <-msgs:
				switch {
				case strings.HasPrefix(m.Subject, "$O."):
					w.pieces.Add(1)
				case !read && strings.Contains(m.Subject, ".KV_"):
					read = true
					w.atRead <- w.pieces.Load()
				}
			case <-done:
				return
			}
		}
	}()
	return w
}
