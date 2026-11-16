package natsobj_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"testing"
	"time"

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

// A WINDOW ACROSS A LINK CROSSES WITHIN A REQUEST'S TERM.
//
// Whatever a node sends after a put has filled its window waits, on a link
// that carries what it is sent in order, behind the window's bytes: a read, a
// small write or a lease renewal on the connection the bucket rides, the
// coordination store's, and at a bottleneck that queues first in first out
// whatever another connection sends across the same link. Such a request is
// not answered within its term ([jsapi.RequestTerm]) unless the window
// crosses the floor ([objstore.MiBPace]) in what that term leaves after the
// request's own round trip, one [jsprovision.ReadTerm]: four seconds,
// whatever lease the node holds. So the window across a link is the widest
// that crosses within that, and at least the one message a put needs to move
// at all; a member's own client, which crosses no link, keeps the window its
// throughput was measured at ([natsobj.MemberWindow]). Read off backends
// opened both ways, so a window Open did not take from these figures fails
// too.
//
// Mutation: size the window across a link by the shipped lease heartbeat
// again (four messages, fifteen seconds at the floor), and this goes red.
func TestAWindowAcrossALinkCrossesWithinARequestsTerm(t *testing.T) {
	t.Parallel()
	if natsobj.MessageCrossing != crossing(1) {
		t.Fatalf("one message crosses a link at the floor in %v by natsobj's figure and %v by the "+
			"floor's own", natsobj.MessageCrossing, crossing(1))
	}
	window := natsobj.LinkWindow
	if window < 1 {
		t.Fatalf("the window across a link is %d messages; a put needs one out to move at all", window)
	}
	if c := crossing(window); c > requestBudget {
		t.Errorf("the window across a link is %d messages, which cross the floor in %v: a request "+
			"sent behind it is answered past the %v its %v term leaves after its own round trip",
			window, c, requestBudget, jsapi.RequestTerm)
	}
	if window < natsobj.MemberWindow && crossing(window+1) <= requestBudget {
		t.Errorf("the window across a link is %d messages, and %d still cross the floor within %v: "+
			"throughput thrown away", window, window+1, requestBudget)
	}
	if c := crossing(window); c > objstore.Stall {
		t.Errorf("a message waits up to %v behind a full window at the floor, past the %v stall",
			c, objstore.Stall)
	}
	c := memberClient(t)
	across := openWith(t, c, natsobj.Config{MaxBytes: testCeiling, Replicas: 1, AcrossLink: true})
	if got := across.Window(); got != window {
		t.Errorf("a bucket opened across a link has a window of %d messages, want %d", got, window)
	}
	own := openWith(t, c, natsobj.Config{MaxBytes: testCeiling, Replicas: 1})
	if got := own.Window(); got != natsobj.MemberWindow {
		t.Errorf("a bucket opened on a member's own client has a window of %d messages, want %d",
			got, natsobj.MemberWindow)
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
// DECIDED BY ORDER, NEVER BY THE CLOCK. A subscription on the member counts
// the upload's messages as they arrive there, and the read is answered with
// the count of those that crossed while it waited: every one crossed ahead of
// the read's request, which the link carries behind them, and a message sent
// after it needs the floor's 3.75 seconds to cross where the answer needs
// milliseconds. Those messages must cross the floor within what the read's
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
	b := openWith(t, client(t, across.LeafBroker), natsobj.Config{MaxBytes: testCeiling, Replicas: 1,
		AcrossLink: true})
	sent := watchPieces(t, client(t, across.LeafBroker).Conn())
	arrived := watchPieces(t, client(t, across.MemberBroker).Conn())
	window := b.Window()

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

	before := len(arrived.all())
	// A LIVENESS BOUND, deciding nothing: the claim is the count below.
	ctx, cancel := context.WithTimeout(t.Context(), objstore.Stall)
	defer cancel()
	if _, _, err := across.Leaf.Channel(ctx, "absent"); err != nil {
		t.Fatalf("a fleet-store read sent behind a full window of %d messages on a link at the floor "+
			"was not answered within the %v the case waits for it: %v", window, objstore.Stall, err)
	}
	ahead := len(arrived.all()) - before
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
