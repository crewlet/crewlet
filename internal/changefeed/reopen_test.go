package changefeed_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/statelog"
)

// errNoHeartbeat is the read failure a broker's client reports when its pull
// heard nothing for two heartbeats: what a host that slept, a leader election
// or a reconnect leaves behind, and what ended every feed on a node for good
// before a feed reopened its consumer.
var errNoHeartbeat = errors.New("jetstream: read crewlet-chat-feed: nats: no heartbeat received")

// clock is a settable wall clock, for the instants an outage is measured from.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func newClock() *clock {
	return &clock{at: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
	return c.at
}

// running is one feed under a case: its consumer, what it published and how
// to stop it.
type running struct {
	feed *changefeed.Feed
	docs *estate
	pub  *capture
	stop func() bool
}

// start runs a feed over a fresh estate whose reopen schedule begins at base,
// with the clock its outages are measured on (nil for the wall clock).
func start(t *testing.T, base time.Duration, now func() time.Time,
	arm func(*estate)) running {
	t.Helper()
	docs, pub := newEstate(), &capture{}
	if arm != nil {
		arm(docs)
	}
	feed, err := changefeed.New(changefeed.Options{
		Opener: docs, Publisher: pub, Claims: newClaims(), Translator: newProbe(),
		Now: now,
	})
	if err != nil {
		t.Fatalf("new feed: %v", err)
	}
	changefeed.ReopenBase(feed, base)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		feed.Run(ctx)
	}()
	// stop cancels the feed and reports whether it returned within the
	// bound: the wall clock is the symptom, since a feed that waits out its
	// schedule before noticing a shutdown holds its node's stop for it.
	stop := sync.OnceValue(func() bool {
		cancel()
		select {
		case <-done:
			return true
		case <-time.After(5 * time.Second):
			return false
		}
	})
	t.Cleanup(func() {
		if !stop() {
			t.Error("the feed did not stop")
		}
	})
	return running{feed: feed, docs: docs, pub: pub, stop: stop}
}

// A READ THAT FAILS IS A CONSUMER LOST, NOT A FEED ENDED. The broker's client
// reports a missed heartbeat as a read error, and before a feed reopened its
// consumer that one error ended the feed for good: the node routed no wake
// until it restarted, while every write still landed.
//
// Mutation: return from Run on a read error, and the consumer is never opened
// again and the next change wakes nobody.
func TestAFeedWhoseReadFailsReopensItsGroupAndDeliversTheNextChange(t *testing.T) {
	t.Parallel()
	f := start(t, time.Millisecond, nil, nil)
	settle(t, func() bool { return f.docs.opened() == 1 }, "the feed never opened")

	f.docs.failNextRead(errNoHeartbeat)
	settle(t, func() bool { return f.docs.opened() == 2 },
		"a read that failed ended the feed rather than its consumer")
	// THE FAILED CONSUMER IS STOPPED BEFORE ANOTHER IS OPENED, so it hands
	// back what it held rather than keeping it from the group.
	if got := f.docs.stopped(); got < 1 {
		t.Errorf("the failed consumer was never stopped (%d stops)", got)
	}
	if got := f.docs.openedGroup(); got != testGroup {
		t.Errorf("the feed reopened %q, want its own durable group %q", got, testGroup)
	}

	writeChange(t, f.docs, "u1")
	settle(t, func() bool { return f.pub.count() == 1 },
		"the change after the failed read never became a wake")
	if _, down := f.feed.Outage(); down {
		t.Error("a feed that reopened its consumer still reports itself down")
	}
}

// AN OPEN THAT FAILS IS TRIED AGAIN until one succeeds, which is a broker
// that is still coming back: the feed's first open at boot, or every reopen
// while the broker is down.
//
// Mutation: return from Run on an open error, and the feed never opens.
func TestAFeedWhoseOpenFailsTriesAgainUntilItOpens(t *testing.T) {
	t.Parallel()
	const fails = 3
	f := start(t, time.Millisecond, nil, func(e *estate) {
		e.failOpens(fails, errors.New("jetstream: open the group: context deadline exceeded"))
	})
	settle(t, func() bool { return f.docs.opened() == fails+1 },
		"the feed stopped trying to open its consumer")

	writeChange(t, f.docs, "u1")
	settle(t, func() bool { return f.pub.count() == 1 },
		"the feed that finally opened delivered nothing")
	time.Sleep(20 * time.Millisecond)
	if got := f.docs.opened(); got != fails+1 {
		t.Errorf("%d opens for %d failures, want exactly one more: a feed that "+
			"opened went on opening", got, fails)
	}
}

// A CONSUMER THAT CLOSES UNDER A RUNNING FEED IS A FAILURE, because only the
// feed's own context ends it: a close the feed did not ask for is a consumer
// it still has to read, and taken as a shutdown it ended the feed without a
// line said.
//
// Mutation: return on a nil message whatever the context, and the second open
// never happens.
func TestAConsumerThatClosesUnderARunningFeedIsOpenedAgain(t *testing.T) {
	t.Parallel()
	f := start(t, time.Millisecond, nil, nil)
	settle(t, func() bool { return f.docs.opened() == 1 }, "the feed never opened")

	f.docs.closeNextRead()
	settle(t, func() bool { return f.docs.opened() == 2 },
		"a consumer that closed under the feed ended the feed")
	writeChange(t, f.docs, "u1")
	settle(t, func() bool { return f.pub.count() == 1 },
		"the change after the close never became a wake")
}

// A FEED TOLD TO STOP WHILE IT WAITS STOPS AT ONCE. Its wait can be half a
// minute, and a node shutting down joins every feed: a wait that ran out
// before the context was read would hold the whole stop for it.
//
// Mutation: sleep without watching the context, and the stop waits an hour.
func TestAFeedStopsAtOnceWhenToldToWhileItWaitsToReopen(t *testing.T) {
	t.Parallel()
	f := start(t, time.Hour, nil, func(e *estate) {
		e.failOpens(0, errors.New("jetstream: open the group: no responders"))
	})
	settle(t, func() bool {
		o, down := f.feed.Outage()
		return down && o.Failures == 1
	}, "the feed's first open never failed")

	began := time.Now()
	if !f.stop() {
		t.Fatal("a feed waiting an hour to reopen did not stop when its context ended")
	}
	if waited := time.Since(began); waited > time.Second {
		t.Errorf("the feed took %s to stop, want it at once", waited)
	}
	if got := f.docs.opened(); got != 1 {
		t.Errorf("%d opens: the feed tried again after its context ended", got)
	}
}

// A FEED THAT IS DOWN SAYS SO, from the moment it lost its consumer, and stops
// saying so the moment it holds one again. A feed with no consumer publishes
// no wake, so nothing that judges published wakes can see it; this is what
// `feed_stopped` is raised from.
//
// Mutation: start the outage at the latest failure rather than the first, and
// its age never passes the grace while the broker stays down.
func TestAFeedThatIsDownSaysSinceWhenUntilItOpens(t *testing.T) {
	t.Parallel()
	clk := newClock()
	booted := clk.now()
	f := start(t, time.Millisecond, clk.now, func(e *estate) {
		e.failOpens(0, errors.New("jetstream: open the group: context deadline exceeded"))
	})

	// DOWN SINCE IT BEGAN, for a feed that has never held a consumer.
	settle(t, func() bool {
		o, down := f.feed.Outage()
		return down && o.Failures >= 2
	}, "a feed that cannot open does not report itself down")
	o, _ := f.feed.Outage()
	if o.Source != "work" || !o.Since.Equal(booted) ||
		!strings.Contains(o.Cause, "context deadline exceeded") {
		t.Errorf("a feed that never opened reports %+v, want the work feed down "+
			"since %s for the open's own error", o, booted)
	}

	f.docs.failOpens(0, nil)
	settle(t, func() bool { _, down := f.feed.Outage(); return !down },
		"a feed that opened still reports itself down")

	// DOWN SINCE THE READ THAT ENDED ITS CONSUMER, however many opens fail
	// after it.
	lost := clk.advance(10 * time.Minute)
	f.docs.failOpens(0, errors.New("jetstream: open the group: no responders"))
	f.docs.failNextRead(errNoHeartbeat)
	settle(t, func() bool {
		o, down := f.feed.Outage()
		return down && o.Failures >= 3
	}, "a feed whose consumer failed does not report itself down")
	clk.advance(3 * time.Minute)
	o, _ = f.feed.Outage()
	if !o.Since.Equal(lost) {
		t.Errorf("the outage began at %s, want the read that ended the consumer at %s",
			o.Since, lost)
	}
	if !strings.Contains(o.Cause, "no responders") {
		t.Errorf("the outage says %q, want the latest failure's own words", o.Cause)
	}

	f.docs.failOpens(0, nil)
	settle(t, func() bool { _, down := f.feed.Outage(); return !down },
		"a feed that reopened still reports itself down")

	// AND A STOPPED FEED IS NOT DOWN: nothing is meant to be consuming.
	if !f.stop() {
		t.Fatal("the feed did not stop")
	}
	if o, down := f.feed.Outage(); down {
		t.Errorf("a stopped feed reports an outage %+v", o)
	}
}

// THE SCHEDULE DOUBLES FROM ONE SECOND TO A CEILING INSIDE THE GRACE the
// alarm fires at, so a broker that answers again is answered within the grace
// however long it was gone, and a feed down for a day asks twice a minute.
func TestTheReopenScheduleDoublesToACeilingInsideTheStallGrace(t *testing.T) {
	t.Parallel()
	feed, err := changefeed.New(changefeed.Options{
		Opener: newEstate(), Publisher: &capture{}, Translator: newProbe(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ceiling := changefeed.Ceiling(feed)
	if ceiling != statelog.FeedReopenCeiling || ceiling >= statelog.StallGrace {
		t.Fatalf("the ceiling is %s, want the declared %s inside the %s grace",
			ceiling, statelog.FeedReopenCeiling, statelog.StallGrace)
	}
	for n, want := range map[int]time.Duration{
		1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 5: 16 * time.Second,
		6: ceiling, 50: ceiling,
	} {
		got := changefeed.Wait(feed, n)
		// A FIFTH EITHER WAY, so the feeds one outage stopped together do
		// not ask the broker again in the same second.
		if lo, hi := want*8/10, want*12/10; got < lo || got > hi {
			t.Errorf("attempt %d waits %s, want %s spread by a fifth", n, got, want)
		}
	}
}

// ONLY A CONSUMER THAT STAYED OPEN FOR THE CEILING BEGINS A NEW SERIES. One
// that opens and fails again at once is a broker flapping, which the doubling
// exists to back off from; one that held for the ceiling was healthy, and its
// failure is the first of a new outage.
//
// Mutation: begin a new series at every open, and a flapping broker is asked
// again every second for ever.
func TestOnlyAConsumerThatStayedOpenBeginsTheScheduleAgain(t *testing.T) {
	t.Parallel()
	clk := newClock()
	f := start(t, time.Millisecond, clk.now, nil)
	settle(t, func() bool { return f.docs.opened() == 1 }, "the feed never opened")

	f.docs.failNextRead(errNoHeartbeat)
	settle(t, func() bool { return f.docs.opened() == 2 }, "the feed never reopened")
	f.docs.failNextRead(errNoHeartbeat)
	settle(t, func() bool { return f.docs.opened() == 3 }, "the feed never reopened")
	if got := changefeed.Series(f.feed); got != 2 {
		t.Errorf("a consumer that failed at once left the schedule at attempt %d, want 2", got)
	}

	// HOLDING ITS CONSUMER, not merely opened: the instant it opened is read
	// after the open returns, and a clock moved before it would date the
	// consumer after the hold it is meant to have lasted.
	settle(t, func() bool { _, down := f.feed.Outage(); return !down },
		"the reopened feed never held its consumer")
	clk.advance(changefeed.Ceiling(f.feed))
	f.docs.failNextRead(errNoHeartbeat)
	settle(t, func() bool { return f.docs.opened() == 4 }, "the feed never reopened")
	if got := changefeed.Series(f.feed); got != 1 {
		t.Errorf("a consumer that held for the ceiling left the schedule at attempt "+
			"%d, want a new series at 1", got)
	}
}
