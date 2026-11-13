package changefeed_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/jsapi"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/jetstream/linktest"
	"github.com/crewlet/crewlet/internal/runtoken"
)

// slowKeepAuth is the fleet key the case below's member and leaf present to
// each other: a literal placeholder, never a key anybody deploys.
var slowKeepAuth = sync.OnceValue(func() js.FleetAuth {
	auth, err := js.FleetAuthFrom(runtoken.OneKey("feed-leaf",
		"the wake feed's leaf case's placeholder key material"))
	if err != nil {
		panic(err)
	}
	return auth
})

// What the case below keeps across its slow link: a frame of 5 MiB, through a
// link carrying 128 KiB a second towards the member — about forty seconds,
// past the group's thirty-second window, and inside the keep's own budget at
// the slowest pace a link is allowed.
const (
	slowKeepFrame = 5 << 20
	slowKeepRate  = 128 << 10
)

// A KEEP ACROSS A SLOW LINK KEEPS ONE ENTRY, WHICHEVER NODE IS HANDED THE
// RECORD.
//
// The real broker and the real group, whose acknowledgement window is thirty
// seconds. A node whose connection to the broker crosses a link carrying 128
// KiB a second — a leaf of a member here, the link linktest slows, standing
// for a data node on an external broker, whose feed's keeps cross the network
// the same way — keeps a record's 5 MiB frame, which takes about forty
// seconds to cross. The record is past the age, so that node's feed keeps it
// and acks it once the keep returns; meanwhile a member's node pulls the same
// group, as every data node of a fleet does, so the record is handed to it
// once the first delivery's window lapses, and it keeps the record too.
// Nothing the first node sends while its keep crosses reaches the broker
// before the keep's own last byte, so nothing it could say would keep the
// record from the peer: the second keep is made harmless instead. Both name
// the record's identity, the dead letters hold ONE entry of it, and the
// record is settled.
//
// Mutation: send a keep without its identity's message id, and the broker
// keeps the peer's copy and the leaf's as two entries.
func TestAKeepAcrossASlowLinkKeepsOneEntryWhicheverNodeIsHandedTheRecord(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	member, err := js.StartServer(ctx, js.Config{ServerName: "member",
		LeafHost: "127.0.0.1", LeafPort: js.AnyPort, StoreDir: brokertest.TempDir(t),
		StoreMaxBytes: brokertest.StoreMaxBytes, FleetAuth: slowKeepAuth()})
	if err != nil {
		t.Fatalf("start a member serving leaves: %v", err)
	}
	t.Cleanup(member.Shutdown)
	link := linktest.Slow(t, fmt.Sprintf("127.0.0.1:%d", member.LeafPort()), slowKeepRate)
	leaf, err := js.StartServer(ctx, js.Config{ServerName: "leaf",
		LeafURLs: []string{"nats-leaf://" + link.Addr()}, FleetAuth: slowKeepAuth()})
	if err != nil {
		t.Fatalf("start a leaf across the slow link: %v", err)
	}
	t.Cleanup(leaf.Shutdown)
	client := func(s *js.Server) *js.Queue {
		q, err := s.Client(ctx)
		if err != nil {
			t.Fatalf("a client: %v", err)
		}
		t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(ctx)) })
		return q
	}
	onMember, onLeaf := client(member), client(leaf)

	// THE RECORD IS WRITTEN ON THE MEMBER, across nothing: only the leaf's
	// keep crosses the slow link.
	spec := js.DomainStream{Name: "CREWLET_SLOWKEEP_LOG", Subjects: []string{"crewlet.slowkeep.>"},
		MaxBytes: 32 << 20, MaxMessageBytes: 8 << 20, Duplicates: time.Minute}
	if err := onMember.EnsureDomainStream(ctx, spec); err != nil {
		t.Fatalf("create the log: %v", err)
	}
	written, err := onMember.DomainLog(ctx, spec.Name)
	if err != nil {
		t.Fatalf("open the log on the member: %v", err)
	}
	frame := make([]byte, slowKeepFrame)
	if _, err := rand.Read(frame); err != nil {
		t.Fatalf("random frame: %v", err)
	}
	seq, _, err := written.Append(ctx, "crewlet.slowkeep.item.1", "op-slow", nil, frame)
	if err != nil {
		t.Fatalf("append the record: %v", err)
	}

	// A NODE'S FEED, each over its own handle on the group and keeping
	// through its own client, as a node's does — AN HOUR AND A MINUTE ON, so
	// the record the broker stored a moment ago is past the age and kept.
	group := func(ctx context.Context, q *js.Queue) *js.DomainGroup {
		t.Helper()
		read, err := q.DomainLog(ctx, spec.Name)
		if err != nil {
			t.Fatalf("open the log: %v", err)
		}
		g, _, err := read.Group(ctx, "slow", 0)
		if err != nil {
			t.Fatalf("open the group: %v", err)
		}
		t.Cleanup(func() { _ = g.Stop() })
		return g
	}
	feed := func(q *js.Queue) *changefeed.Feed {
		t.Helper()
		f, err := changefeed.New(changefeed.Options{
			Opener: newEstate(), Publisher: &capture{}, Claims: newClaims(), Translator: newProbe(),
			DeadLetters: q,
			Now:         func() time.Time { return time.Now().UTC().Add(changefeed.DeadLetterAge + time.Minute) },
		})
		if err != nil {
			t.Fatalf("new feed: %v", err)
		}
		return f
	}
	handle := func(ctx context.Context, f *changefeed.Feed, d *js.DomainDelivery) {
		changefeed.Handle(ctx, f, &changefeed.Message{
			Record: changefeed.Record{ID: "op-slow", Key: d.Subject, Position: d.Seq,
				Stream: spec.Name, StoredAt: d.StoredAt, Delivered: d.Delivered},
			Frame: d.Payload,
			Deferred: &changefeed.Deferral{Reason: changefeed.DeferNewerEnvelope,
				Err: errors.New("an envelope a newer build writes")},
			Ack: d.Ack, Nak: d.Nak,
		})
	}

	onTheLeaf := group(ctx, onLeaf)
	first, err := onTheLeaf.Next(ctx)
	if err != nil || first == nil {
		t.Fatalf("the leaf's delivery = (%v, %v)", first, err)
	}

	// THE PEER pulls the same group for as long as the leaf keeps, and keeps
	// what it is handed: the record, once the leaf's window lapses.
	peerCtx, stopPeer := context.WithCancel(ctx)
	onThePeer, peerFeed := group(peerCtx, onMember), feed(onMember)
	var peer sync.WaitGroup
	peer.Go(func() {
		for {
			d, err := onThePeer.Next(peerCtx)
			if err != nil || d == nil {
				return
			}
			handle(peerCtx, peerFeed, d)
		}
	})
	handle(ctx, feed(onLeaf), first)
	stopPeer()
	peer.Wait()

	// THE BROKER'S OWN ACCOUNT, read on the member once the leaf's keep
	// returned: the entries the dead letters hold of the record, and the
	// group's, which owes the record to nobody once it is settled.
	kept, err := onMember.DeadLettersOn(ctx, changefeed.DeadLetterSubject(newProbe().Source()))
	if err != nil {
		t.Fatalf("list the dead letters: %v", err)
	}
	if len(kept) != 1 {
		t.Errorf("a keep of about forty seconds across the link, beside a peer pulling the group: "+
			"the dead letters hold %d entries of the record, want one", len(kept))
	}
	nc, err := member.Conn()
	if err != nil {
		t.Fatalf("connect to the member: %v", err)
	}
	t.Cleanup(nc.Close)
	inspect, err := jsapi.Embedded().Client(nc)
	if err != nil {
		t.Fatalf("a client of the member: %v", err)
	}
	consumer, err := inspect.Consumer(ctx, spec.Name, onTheLeaf.Name())
	if err != nil {
		t.Fatalf("read the group's consumer: %v", err)
	}
	info, err := consumer.Info(ctx)
	if err != nil {
		t.Fatalf("read the group's state: %v", err)
	}
	if info.NumAckPending != 0 || info.AckFloor.Stream != seq {
		t.Errorf("the group holds %d deliveries unacknowledged and is acknowledged through %d, "+
			"want the record, %d, settled", info.NumAckPending, info.AckFloor.Stream, seq)
	}
}
