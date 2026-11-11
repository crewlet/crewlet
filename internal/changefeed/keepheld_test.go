package changefeed_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/queue"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/jetstream/linktest"
	"github.com/crewlet/crewlet/internal/runtoken"
)

// slowKeeper keeps a dead letter only once takes has passed — a keep waiting
// as long as a large frame takes to cross a slow link — and counts the keeps
// it was asked for. It holds nothing a replay would read.
type slowKeeper struct {
	takes time.Duration

	mu    sync.Mutex
	keeps int
}

func (k *slowKeeper) KeepDeadLetter(context.Context, string, []byte, []byte) error {
	k.mu.Lock()
	k.keeps++
	k.mu.Unlock()
	time.Sleep(k.takes) //fixedwait:simulated a keep waiting while its frame crosses a slow link
	return nil
}

func (k *slowKeeper) DeadLettersOn(context.Context, string) ([]queue.Kept, error) {
	return nil, nil
}

func (k *slowKeeper) DeadLetterFrame(context.Context, string, uint64) ([]byte, bool, error) {
	return nil, false, nil
}

func (k *slowKeeper) DropDeadLetter(context.Context, string, uint64) error { return nil }

func (k *slowKeeper) kept() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.keeps
}

// windowBroker delivers a record to a feed as the feed's group does: a
// delivery that goes its acknowledgement window without an ack, a nak or a
// progress signal is handed back — delivered again, its count one higher —
// and every delivery and ack is counted. It delivers a record at most limit
// times, so a case whose feed lets its deliveries lapse still ends.
type windowBroker struct {
	feed   *changefeed.Feed
	window time.Duration
	limit  int

	mu         sync.Mutex
	deliveries int
	acks       int
	handlers   sync.WaitGroup
}

// deliver hands rec to the feed as one delivery, settled on this goroutine,
// and hands it back on a goroutine of its own if its window lapses first.
func (b *windowBroker) deliver(ctx context.Context, rec changefeed.Record) {
	b.mu.Lock()
	b.deliveries++
	b.mu.Unlock()
	var (
		mu    sync.Mutex
		over  bool
		lapse *time.Timer
	)
	settle := func() bool {
		mu.Lock()
		defer mu.Unlock()
		if over {
			return false
		}
		over = true
		lapse.Stop()
		return true
	}
	mu.Lock()
	lapse = time.AfterFunc(b.window, func() { //fixedwait:simulated the group's acknowledgement window, which hands a delivery back
		mu.Lock()
		handedBack := !over
		over = true
		mu.Unlock()
		b.mu.Lock()
		again := handedBack && b.deliveries < b.limit
		b.mu.Unlock()
		if again {
			next := rec
			next.Delivered++
			b.handlers.Go(func() { b.deliver(ctx, next) })
		}
	})
	mu.Unlock()
	changefeed.Handle(ctx, b.feed, &changefeed.Message{
		Record: rec, Frame: aFrame,
		Deferred: &changefeed.Deferral{Reason: changefeed.DeferNewerEnvelope,
			Err: errors.New("an envelope a newer build writes")},
		Ack: func() error {
			if settle() {
				b.mu.Lock()
				b.acks++
				b.mu.Unlock()
			}
			return nil
		},
		Nak: func(time.Duration) error {
			settle()
			return nil
		},
		Progress: func() error {
			mu.Lock()
			defer mu.Unlock()
			if !over {
				lapse.Reset(b.window)
			}
			return nil
		},
		ProgressEvery: b.window / 3,
	})
}

func (b *windowBroker) counts() (deliveries, acks int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.deliveries, b.acks
}

// A KEEP LONGER THAN ITS DELIVERY'S ACKNOWLEDGEMENT WINDOW HOLDS THE DELIVERY,
// and the record is kept once.
//
// A dead letter's keep waits as long as its frame takes to cross the node's
// link to the broker, and the feed acks the record only once the keep returns:
// a keep that outlasted the window had its record handed back mid-keep, and
// the redelivery, still past the age, was kept again — two entries of one
// record in the dead letters, and a second copy across the link. So while a
// keep runs the feed sends the delivery's progress signal at the cadence the
// delivery states. Here a broker of the case's own hands the record back once
// its window passes with no word, and the keep takes two windows: delivered
// once, kept once, acked once. In a bubble, so the windows are the clock's and
// not a loaded runner's.
//
// Mutation: keep without sending the delivery's progress signal, and the
// record is handed back mid-keep and kept twice.
func TestAKeepLongerThanItsAckWindowHoldsItsDelivery(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const window = 30 * time.Second
		keeper := &slowKeeper{takes: 2 * window}
		clock := newClock()
		feed, err := changefeed.New(changefeed.Options{
			Opener: newEstate(), Publisher: &capture{}, Claims: newClaims(), Translator: newProbe(),
			DeadLetters: keeper, Now: clock.now,
		})
		if err != nil {
			t.Fatalf("new feed: %v", err)
		}
		broker := &windowBroker{feed: feed, window: window, limit: 2}
		broker.deliver(t.Context(), aRecord(clock.now().Add(-changefeed.DeadLetterAge-time.Minute), 1))
		broker.handlers.Wait()
		synctest.Wait()
		deliveries, acks := broker.counts()
		if kept := keeper.kept(); kept != 1 || deliveries != 1 || acks != 1 {
			t.Errorf("a keep of two windows: delivered %d times, kept %d and acked %d, want the "+
				"one delivery kept once and acked once", deliveries, kept, acks)
		}
	})
}

// heldKeepAuth is the fleet key the case below's member and leaf present to
// each other: a literal placeholder, never a key anybody deploys.
var heldKeepAuth = sync.OnceValue(func() js.FleetAuth {
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
	heldKeepFrame = 5 << 20
	heldKeepRate  = 128 << 10
)

// A KEEP ACROSS A SLOW LINK HOLDS ITS DELIVERY, AND THE RECORD IS KEPT ONCE.
//
// The real broker and the real group, whose acknowledgement window is thirty
// seconds: a node whose link to its member carries 128 KiB a second keeps a
// record's 5 MiB frame, which takes about forty seconds to cross. The record
// is past the age, so the feed keeps it and acks it once the keep returns, and
// while the keep crosses the feed signals progress on the delivery: the broker
// delivered it once — its own count — and the dead letters hold one entry.
//
// Mutation: keep without the progress signal, and the broker hands the record
// back mid-keep, the node keeps what it is handed again, and the dead letters
// hold two entries of one record.
func TestAKeepAcrossASlowLinkHoldsItsDeliveryAndKeepsOneEntry(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	member, err := js.StartServer(ctx, js.Config{ServerName: "member",
		LeafHost: "127.0.0.1", LeafPort: js.AnyPort, StoreDir: t.TempDir(),
		StoreMaxBytes: brokertest.StoreMaxBytes, FleetAuth: heldKeepAuth()})
	if err != nil {
		t.Fatalf("start a member serving leaves: %v", err)
	}
	t.Cleanup(member.Shutdown)
	link := linktest.Slow(t, fmt.Sprintf("127.0.0.1:%d", member.LeafPort()), heldKeepRate)
	leaf, err := js.StartServer(ctx, js.Config{ServerName: "leaf",
		LeafURLs: []string{"nats-leaf://" + link.Addr()}, FleetAuth: heldKeepAuth()})
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

	// THE RECORD IS WRITTEN ON THE MEMBER, across nothing: only the keep
	// crosses the slow link.
	spec := js.DomainStream{Name: "CREWLET_HELDKEEP_LOG", Subjects: []string{"crewlet.heldkeep.>"},
		MaxBytes: 32 << 20, MaxMessageBytes: 8 << 20, Duplicates: time.Minute}
	if err := onMember.EnsureDomainStream(ctx, spec); err != nil {
		t.Fatalf("create the log: %v", err)
	}
	written, err := onMember.DomainLog(ctx, spec.Name)
	if err != nil {
		t.Fatalf("open the log on the member: %v", err)
	}
	frame := make([]byte, heldKeepFrame)
	if _, err := rand.Read(frame); err != nil {
		t.Fatalf("random frame: %v", err)
	}
	if _, _, err := written.Append(ctx, "crewlet.heldkeep.item.1", "op-held", nil, frame); err != nil {
		t.Fatalf("append the record: %v", err)
	}

	read, err := onLeaf.DomainLog(ctx, spec.Name)
	if err != nil {
		t.Fatalf("open the log on the leaf: %v", err)
	}
	group, _, err := read.Group(ctx, "held", 0)
	if err != nil {
		t.Fatalf("open the group on the leaf: %v", err)
	}
	t.Cleanup(func() { _ = group.Stop() })
	feed, err := changefeed.New(changefeed.Options{
		Opener: newEstate(), Publisher: &capture{}, Claims: newClaims(), Translator: newProbe(),
		DeadLetters: onLeaf,
		// AN HOUR AND A MINUTE ON, so the record the broker stored a
		// moment ago is past the age and kept.
		Now: func() time.Time { return time.Now().UTC().Add(changefeed.DeadLetterAge + time.Minute) },
	})
	if err != nil {
		t.Fatalf("new feed: %v", err)
	}
	handle := func() {
		t.Helper()
		d, err := group.Next(ctx)
		if err != nil || d == nil {
			t.Fatalf("the group's next delivery = (%v, %v)", d, err)
		}
		changefeed.Handle(ctx, feed, &changefeed.Message{
			Record: changefeed.Record{ID: "op-held", Key: d.Subject, Position: d.Seq,
				Stream: spec.Name, StoredAt: d.StoredAt, Delivered: d.Delivered},
			Frame: d.Payload,
			Deferred: &changefeed.Deferral{Reason: changefeed.DeferNewerEnvelope,
				Err: errors.New("an envelope a newer build writes")},
			Ack: d.Ack, Nak: d.Nak, Progress: d.Progress, ProgressEvery: d.ProgressEvery,
		})
	}
	handle()

	// THE BROKER'S OWN COUNT of the deliveries it made, read once the keep
	// returned: a delivery handed back mid-keep is a second one, already on
	// its way to this node, which keeps what it is handed as a node does.
	nc, err := member.Conn()
	if err != nil {
		t.Fatalf("connect to the member: %v", err)
	}
	t.Cleanup(nc.Close)
	inspect, err := jsapi.Embedded().Client(nc)
	if err != nil {
		t.Fatalf("a client of the member: %v", err)
	}
	consumer, err := inspect.Consumer(ctx, spec.Name, group.Name())
	if err != nil {
		t.Fatalf("read the group's consumer: %v", err)
	}
	info, err := consumer.Info(ctx)
	if err != nil {
		t.Fatalf("read the group's deliveries: %v", err)
	}
	delivered := info.Delivered.Consumer
	if delivered > 1 {
		handle()
	}
	kept, err := onMember.DeadLettersOn(ctx, changefeed.DeadLetterSubject(newProbe().Source()))
	if err != nil {
		t.Fatalf("list the dead letters: %v", err)
	}
	if delivered != 1 || len(kept) != 1 {
		t.Errorf("a keep of about forty seconds across the link: the broker delivered the record "+
			"%d times and the dead letters hold %d entries of it, want one of each", delivered, len(kept))
	}
}
