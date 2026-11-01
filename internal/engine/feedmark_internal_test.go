package engine

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

// markedLog is a real broker's domain log holding n records, and a feed position
// over it in a fresh mark register, at generation zero.
func markedLog(t *testing.T, stream, prefix string, n int) (*jetstream.Queue, feedPosition) {
	t.Helper()
	q, err := jetstream.Open(t.Context(), jetstream.Config{
		StoreDir: t.TempDir(), StoreMaxBytes: brokertest.StoreMaxBytes,
	})
	if err != nil {
		t.Fatalf("open the broker: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	if err := q.EnsureDomainStream(t.Context(), jetstream.DomainStream{
		Name: stream, Subjects: []string{prefix + ".>"}, MaxBytes: 16 << 20, Duplicates: time.Minute,
	}); err != nil {
		t.Fatalf("create the log: %v", err)
	}
	domain, err := q.DomainLog(t.Context(), stream)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	for i := range n {
		if _, _, err := domain.Append(t.Context(), prefix+".record", "", nil,
			[]byte{byte('a' + i)}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	return q, feedPosition{log: domain, marks: memory.NewFleet(), stream: stream,
		group: "crewlet-chat-feed", generation: func() (uint32, bool) { return 0, true }}
}

// handle reads and acknowledges n deliveries, and waits for the broker's floor
// to reach the last of them.
func handle(t *testing.T, at feedPosition, g *jetstream.DomainGroup, n int) uint64 {
	t.Helper()
	var last uint64
	for range n {
		d, err := g.Next(t.Context())
		if err != nil || d == nil {
			t.Fatalf("read: %v, %v", d, err)
		}
		if err := d.Ack(); err != nil {
			t.Fatalf("ack: %v", err)
		}
		last = d.Seq
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		floor, _, err := at.log.GroupAckFloor(t.Context(), at.group)
		if err == nil && floor >= last {
			return last
		}
		if time.Now().After(deadline) {
			t.Fatalf("the group's floor never reached %d: %d, %v", last, floor, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A WAKE FEED'S GROUP CREATED AGAIN STARTS AFTER THE FLOOR THE FLEET RECORDED.
// The group was consumed to a floor, its consumer went (the broker lost it, an
// operator deleted it) and the log stayed. Created again with nothing to go
// on, it started at the log's first retained record, and on a company's chat
// log that was a week of messages woken again. The open that consumed it
// recorded its floor, and the open that creates it again starts after it.
//
// Mutation: start a created group at zero whatever the mark says, and the
// first delivery after the recreation is the log's first record again.
func TestAWakeFeedGroupCreatedAgainStartsAfterTheRecordedFloor(t *testing.T) {
	t.Parallel()
	q, at := markedLog(t, "CREWLET_FEEDMARK_LOG", "crewlet.feedmark", 9)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	if from := at.start(ctx); from != 0 {
		t.Fatalf("a group with no mark starts at %d, want the log's first record", from)
	}
	first, created, err := at.log.Group(ctx, at.group, 0)
	if err != nil || !created {
		t.Fatalf("create the group: created %v, %v", created, err)
	}
	handled := handle(t, at, first, 5)
	marker := startFeedMarker(ctx, at)
	if err := first.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	marker.close()
	mark, found, err := at.marks.FeedMark(ctx, at.stream, at.group)
	if err != nil || !found || mark.Floor != handled {
		t.Fatalf("the stopped open left the mark %+v (found %v, %v), want floor %d",
			mark, found, err, handled)
	}

	if err := q.JetStream().DeleteConsumer(ctx, at.stream, first.Name()); err != nil {
		t.Fatalf("delete the group's consumer: %v", err)
	}
	from := at.start(ctx)
	again, created, err := at.log.Group(ctx, at.group, from)
	if err != nil || !created {
		t.Fatalf("create the group again from %d: created %v, %v", from, created, err)
	}
	defer func() { _ = again.Stop() }()
	d, err := again.Next(ctx)
	if err != nil || d == nil {
		t.Fatalf("read the recreated group: %v, %v", d, err)
	}
	if d.Seq != handled+1 {
		t.Fatalf("the recreated group delivered %d first, want %d, the record after the floor",
			d.Seq, handled+1)
	}
}

// A MARK THIS LOG CANNOT USE STARTS A CREATED GROUP AT ITS FIRST RECORD, as
// before: one from another generation names another number space, and one
// past the log's last record is from a longer log than this one. Started from
// either, the group would skip records nobody was woken for.
func TestAMarkFromAnotherLogStartsAtTheFirstRecord(t *testing.T) {
	t.Parallel()
	_, at := markedLog(t, "CREWLET_FEEDMARK_OTHER", "crewlet.feedmarkother", 4)
	ctx := t.Context()
	for name, mark := range map[string]coord.FeedMark{
		"another generation": {Stream: at.stream, Group: at.group, Generation: 3, Floor: 2},
		"past the log's end": {Stream: at.stream, Group: at.group, Generation: 0, Floor: 40},
	} {
		at := at
		at.marks = memory.NewFleet()
		if _, err := at.marks.RaiseFeedMark(ctx, mark); err != nil {
			t.Fatalf("%s: raise: %v", name, err)
		}
		if from := at.start(ctx); from != 0 {
			t.Errorf("%s: a created group would start at %d, want the log's first record", name, from)
		}
	}
	unknown := at
	unknown.generation = func() (uint32, bool) { return 0, false }
	if _, err := unknown.marks.RaiseFeedMark(ctx, coord.FeedMark{Stream: at.stream, Group: at.group,
		Floor: 2}); err != nil {
		t.Fatalf("raise: %v", err)
	}
	if from := unknown.start(ctx); from != 0 {
		t.Errorf("a node that knows no generation would start a created group at %d", from)
	}
}
