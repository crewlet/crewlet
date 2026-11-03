package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/statelog"
)

// refusingOpener is a broker that answers no open.
type refusingOpener struct{}

func (refusingOpener) Open(context.Context, string) (changefeed.Records, error) {
	return nil, errors.New("jetstream: open the group crewlet-chat-feed: nats: no responders available for request")
}

// silentPublisher publishes nothing; no record reaches it here.
type silentPublisher struct{}

func (silentPublisher) Publish(context.Context, string, *events.Event) error { return nil }

// chatSource is a translator naming the chat estate, which no record reaches.
type chatSource struct{}

func (chatSource) Source() changefeed.Source {
	return changefeed.Source{Name: "chat", Group: "crewlet-test-chat-feed"}
}

func (chatSource) Translate(context.Context, changefeed.Record) (changefeed.Delivery, bool, error) {
	return changefeed.Delivery{}, false, nil
}

// A FEED THIS NODE STARTED THAT IS DOWN REACHES THE ALARM READING, and through
// it `feed_stopped` once it has been down for the stall grace. A feed with no
// consumer publishes no wake, so nothing that judges the published wakes sees
// it: the reading is the only surface it has, and before it a node whose feeds
// had all stopped looked as healthy as one whose company was quiet.
//
// Mutation: drop the feeds from the reading, and a feed down for three minutes
// raises nothing.
func TestAFeedThatIsDownReachesTheAlarmReading(t *testing.T) {
	t.Parallel()
	began := time.Date(2026, 11, 1, 0, 1, 14, 0, time.UTC)
	feed, err := changefeed.New(changefeed.Options{
		Opener: refusingOpener{}, Publisher: silentPublisher{}, Translator: chatSource{},
		Now: func() time.Time { return began },
	})
	if err != nil {
		t.Fatalf("new feed: %v", err)
	}
	n := &native{}
	n.feeds.Store(&[]*changefeed.Feed{feed})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		feed.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if o, down := feed.Outage(); down && o.Failures > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a feed whose broker refuses every open never reported itself down")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// THROUGH THE RETENTION'S OWN READING, the one every alarm beat takes.
	r := &retention{feeds: n.feedsReading}
	reading := r.reading(t.Context(), began.Add(3*time.Minute), nil, nil)
	if len(reading.FeedsDown) != 1 {
		t.Fatalf("the reading carries %+v, want the one chat feed that is down", reading.FeedsDown)
	}
	got := reading.FeedsDown[0]
	if got.Source != "chat" || got.For != 3*time.Minute || got.Failures < 1 ||
		!strings.Contains(got.Cause, "no responders") {
		t.Errorf("the reading carries %+v, want the chat feed down 3m for its "+
			"broker's refusal", got)
	}
	fired := func(r statelog.Reading) bool {
		for _, a := range statelog.Evaluate(r) {
			if a.Kind == statelog.KindFeedStopped {
				return true
			}
		}
		return false
	}
	if !fired(reading) {
		t.Error("a feed down for three minutes raised no feed_stopped")
	}
	// INSIDE THE GRACE IT IS CARRIED AND NOT RAISED: the feed may be
	// reopening from a blip the broker has already ended.
	if early := r.reading(t.Context(), began.Add(30*time.Second), nil, nil); fired(early) {
		t.Error("a feed down for half a minute raised feed_stopped")
	}

	// A NODE WITH NO FEEDS, OR NO NATIVE HALF AT ALL, reads none.
	var none statelog.Reading
	(&native{}).feedsReading(began, &none)
	(*native)(nil).feedsReading(began, &none)
	if len(none.FeedsDown) != 0 {
		t.Errorf("a node that started no feed carries %+v", none.FeedsDown)
	}
}
