package changefeed_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

// groupOpener opens a domain log's fleet-wide group as the feed's consumer,
// over a real broker, and remembers the consumer the latest open made so a
// case can take it away.
type groupOpener struct {
	log *jetstream.DomainLog

	mu     sync.Mutex
	name   string
	opened int
}

func (o *groupOpener) Open(ctx context.Context, group string) (changefeed.Records, error) {
	g, err := o.log.Group(ctx, group)
	if err != nil {
		return nil, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.name, o.opened = g.Name(), o.opened+1
	return groupRecords{g}, nil
}

func (o *groupOpener) consumer() (string, int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.name, o.opened
}

// groupRecords is a group's deliveries as the feed reads them: the subject as
// the key, which the probe translator reads its id from.
type groupRecords struct{ group *jetstream.DomainGroup }

func (r groupRecords) Next(ctx context.Context) (*changefeed.Message, error) {
	d, err := r.group.Next(ctx)
	if err != nil || d == nil {
		return nil, err
	}
	return &changefeed.Message{
		Record: changefeed.Record{ID: d.Subject, Key: d.Subject, Position: d.Seq,
			StoredAt: d.StoredAt, Payload: d.Payload},
		Ack: d.Ack, Nak: d.Nak,
	}, nil
}

func (r groupRecords) Stop() error { return r.group.Stop() }

// keys is every published wake's record key, in order.
func (c *capture) keys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.sent))
	for _, ev := range c.sent {
		w, _ := events.DataAs[*types.RawWebhook](ev)
		if w == nil {
			out = append(out, "")
			continue
		}
		key, _ := w.Body["key"].(string)
		out = append(out, key)
	}
	return out
}

// A FEED WHOSE CONSUMER THE BROKER TOOK AWAY OPENS ITS GROUP AGAIN AND GOES ON
// WAKING, over a real broker: the consumer is deleted under the running feed,
// the broker ends its read, and the next record committed to the log still
// wakes somebody. The record the lost consumer had already handled is handed
// to the new consumer too (a fresh consumer starts from the log's first
// record) and its claim collapses it, so nobody is woken for it twice.
//
// Mutation: return from Run on a read error, and the record committed after
// the deletion wakes nobody.
func TestAFeedWhoseConsumerTheBrokerTookAwayGoesOnWaking(t *testing.T) {
	t.Parallel()
	const stream, prefix = "CREWLET_FEEDREOPEN_LOG", "crewlet.feedreopen"
	q, err := jetstream.Open(t.Context(), jetstream.Config{
		StoreDir: t.TempDir(), StoreMaxBytes: brokertest.StoreMaxBytes,
	})
	if err != nil {
		t.Fatalf("open the broker: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	if err := q.EnsureDomainStream(t.Context(), jetstream.DomainStream{
		Name: stream, Subjects: []string{prefix + ".>"},
		MaxBytes: 16 << 20, Duplicates: time.Minute,
	}); err != nil {
		t.Fatalf("create the log: %v", err)
	}
	domain, err := q.DomainLog(t.Context(), stream)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}

	opener, pub := &groupOpener{log: domain}, &capture{}
	feed, err := changefeed.New(changefeed.Options{
		Opener: opener, Publisher: pub, Claims: newClaims(), Translator: newProbe(),
	})
	if err != nil {
		t.Fatalf("new feed: %v", err)
	}
	changefeed.ReopenBase(feed, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		feed.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the feed did not stop")
		}
	})

	commit := func(name string) {
		t.Helper()
		if _, _, err := domain.Append(t.Context(), prefix+"."+name, "", nil, []byte(`{}`)); err != nil {
			t.Fatalf("commit %s: %v", name, err)
		}
	}
	commit("one")
	settle(t, func() bool { return pub.count() == 1 }, "the first record never woke anybody")

	lost, _ := opener.consumer()
	if err := q.JetStream().DeleteConsumer(t.Context(), stream, lost); err != nil {
		t.Fatalf("delete the feed's consumer: %v", err)
	}
	settle(t, func() bool { _, opened := opener.consumer(); return opened >= 2 },
		"the feed never opened its group again after the broker took its consumer")

	commit("two")
	deadline := time.Now().Add(15 * time.Second)
	for pub.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// A SETTLING PAUSE for a duplicate wake to show itself: the new consumer
	// redelivers the first record, and its claim is what must hold it back.
	time.Sleep(200 * time.Millisecond)
	got := pub.keys()
	if len(got) != 2 || got[0] != prefix+".one" || got[1] != prefix+".two" {
		t.Fatalf("the feed woke %v, want the first record once and the one "+
			"committed after its consumer was taken", got)
	}
	if o, down := feed.Outage(); down {
		t.Errorf("a feed that is waking again reports itself down: %+v", o)
	}
}
