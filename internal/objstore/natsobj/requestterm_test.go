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

// A WINDOW ACROSS A LINK CROSSES WITHIN A REQUEST'S TERM.
//
// Whatever a node's connection sends after a put has filled its window waits,
// on a link that carries what it is sent in order, behind the window's bytes:
// a read or a small write of the node's own, and at a bottleneck that queues
// first in first out, the lease renewals of a connection beside it. Such a
// request is not answered within its term ([jsapi.RequestTerm]) unless the
// window crosses the floor ([objstore.MiBPace]) in what that term leaves after
// the request's own round trip, one [jsprovision.ReadTerm]: four seconds,
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
	crossing := func(messages int) time.Duration {
		return time.Duration(int64(messages)*natsobj.MessageBytes) * objstore.MiBPace / objstore.MiB
	}
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
// A node without `data` reaches the members across one leaf link, which its
// queue's connection, where its uploads go, and its coordination connection,
// where its fleet-store reads go, share: the relay in front of the members'
// leaf listener carries that whole link, both ways, at the floor and in the
// order it was sent ([linktest.SlowEachWay]), as a link that queues first in
// first out does. An upload fills its window with bytes no compressing link
// shrinks, and once the leaf has taken the window's every message onto its
// link ([piecesSeen], a subscription on the leaf, sees each as the leaf routes
// it), a fleet-store read is sent on the other connection: it is answered
// within the term it is held to, [jsapi.RequestTerm].
//
// Mutation: size the window across a link by the shipped lease heartbeat
// again, four messages that take fifteen seconds at the floor, and the read
// misses its term.
func TestAReadSentBehindAFullWindowIsAnsweredInsideItsTerm(t *testing.T) {
	t.Parallel()
	across := kvtest.SlowLeaf(t, floorRate)
	b := openWith(t, client(t, across.LeafBroker), natsobj.Config{MaxBytes: testCeiling, Replicas: 1,
		AcrossLink: true})
	nc, err := across.LeafBroker.Conn()
	if err != nil {
		t.Fatalf("connect to the leaf: %v", err)
	}
	t.Cleanup(nc.Close)
	pieces := watchPieces(t, nc)
	window := b.Window()

	// MORE THAN A WINDOW, so the put holds its window full while the read
	// is sent, whatever the window is.
	body := make([]byte, (window+1)*natsobj.MessageBytes)
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
	pieces.await(t, window)

	ctx, cancel := context.WithTimeout(t.Context(), jsapi.RequestTerm)
	defer cancel()
	if _, _, err := across.Leaf.Channel(ctx, "absent"); err != nil {
		t.Fatalf("a fleet-store read sent behind a full window of %d messages on a link at the floor "+
			"was not answered within its %v term: %v", window, jsapi.RequestTerm, err)
	}
}
