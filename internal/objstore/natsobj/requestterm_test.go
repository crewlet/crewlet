package natsobj_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/coord/kv/kvtest"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/natsobj"
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
// A node without `data` reaches the members across one leaf link, and sends
// its uploads and its fleet-store reads on one connection of it, the
// coordination store's. The relay in front of the members' leaf listener
// carries that whole link, both ways, at the floor and in the order it was
// sent ([linktest.SlowEachWay]), as a link that queues first in first out
// does; here the upload and the read go out on two connections of the leaf's
// broker, which its one link carries in the order the leaf sends them, as it
// would one connection's. An upload fills its window with bytes no
// compressing link shrinks, and once the leaf has taken the window's every
// message onto its link (a subscription on the leaf sees each as the leaf
// routes it), a fleet-store read is sent behind them.
//
// DECIDED BY ORDER, NEVER BY THE CLOCK. The member counts the upload's
// messages as the link delivers them, and the count is taken again where the
// read's request arrives among them ([watchReadArrival]): the messages that
// arrived after the read was sent and before it are those it crossed behind,
// however late the case reads the count, and a message sent after the request
// arrives after it. Those messages must cross the floor within what the read's
// term leaves after its own round trip ([requestBudget]), which is what
// decides whether a read sent behind a full window is answered inside
// [jsapi.RequestTerm] at the floor, and is so on a link whose pacing a loaded
// machine runs late. The read is given a minute only so a case that is never
// answered ends.
//
// Mutation: size the window across a link by the shipped lease heartbeat
// again, and four messages, fifteen seconds at the floor, cross ahead of the
// read.
func TestAReadSentBehindAFullWindowIsAnsweredInsideItsTerm(t *testing.T) {
	t.Parallel()
	across := kvtest.SlowLeaf(t, floorRate)
	b := openWith(t, client(t, across.LeafBroker), acrossLink())
	sent := watchPieces(t, client(t, across.LeafBroker).Conn())
	arrived := watchReadArrival(t, client(t, across.MemberBroker).Conn())
	window := b.Window().Size() / natsobj.PieceTake(b, natsobj.MessageBytes)

	// FOUR MESSAGES MORE THAN A WINDOW, so the put still holds its window
	// full when the read is sent, whatever the window is, on a machine slow
	// to send it.
	body := make([]byte, (window+4)*natsobj.MessageBytes)
	if _, err := rand.Read(body); err != nil {
		t.Fatalf("random bytes: %v", err)
	}
	putCtx, stopPut := context.WithCancel(t.Context())
	put := make(chan error, 1)
	go func() { put <- b.Put(putCtx, "window", bytes.NewReader(body), objstore.PutMeta{}) }()
	// THE PUT IS STOPPED AND WAITED FOR before the leaf goes: it drains the
	// one window it has out and purges what it stored.
	defer func() {
		stopPut()
		<-put
	}()
	sent.await(t, window)

	before := arrived.pieces.Load()
	// A LIVENESS BOUND, deciding nothing: the claim is the count below.
	ctx, cancel := context.WithTimeout(t.Context(), objstore.Stall)
	defer cancel()
	if _, _, err := across.Leaf.Channel(ctx, "absent"); err != nil {
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
		t.Fatalf("none of the upload's messages crossed the link while the read waited: the case sent "+
			"it behind nothing, its window of %d messages already across", window)
	}
	if c := crossing(ahead); c > requestBudget {
		t.Errorf("%d of the upload's messages crossed the link ahead of a fleet-store read sent behind "+
			"its full window, %v at the floor: the read is answered past the %v its %v term leaves "+
			"after its own round trip", ahead, c, requestBudget, jsapi.RequestTerm)
	}
}

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
