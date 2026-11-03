package engine

import (
	"context"
	"errors"
	"maps"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// countingQueue answers Waiting from a table, failing the seats it names, and
// records how many reads it had in flight at once.
type countingQueue struct {
	queue.EventQueue
	counts  map[uuid.UUID]queue.Waiting
	fail    map[uuid.UUID]bool
	running atomic.Int32
	peak    atomic.Int32
}

func (q *countingQueue) Waiting(_ context.Context, topic, _ string) (queue.Waiting, error) {
	now := q.running.Add(1)
	defer q.running.Add(-1)
	for {
		peak := q.peak.Load()
		if now <= peak || q.peak.CompareAndSwap(peak, now) {
			break
		}
	}
	time.Sleep(2 * time.Millisecond)
	id, _ := topics.SeatFromInbox(topic)
	if q.fail[id] {
		return queue.Waiting{}, errors.New("the broker did not answer")
	}
	return q.counts[id], nil
}

// A READ THAT FAILS KEEPS THE SEAT'S LAST COUNT, never reports it empty: a
// broker that did not answer says nothing about the seat's inbox, and a zero
// would tell a person nobody is waiting. A seat never read is left out.
func TestAFailedInboxReadKeepsTheLastCount(t *testing.T) {
	t.Parallel()
	known, fresh, never := uuid.New(), uuid.New(), uuid.New()
	q := &countingQueue{
		counts: map[uuid.UUID]queue.Waiting{fresh: {Background: 5}},
		fail:   map[uuid.UUID]bool{known: true, never: true},
	}
	r := &inboxReporter{last: map[uuid.UUID]queue.Waiting{known: {Priority: 1, Background: 2}}}

	got := r.read(t.Context(), q, []uuid.UUID{known, fresh, never})
	want := map[uuid.UUID]queue.Waiting{known: {Priority: 1, Background: 2}, fresh: {Background: 5}}
	if !maps.Equal(got, want) {
		t.Fatalf("read %v, want %v", got, want)
	}
}

// ONE TICK'S READS ARE BOUNDED: two consumer-info requests per held seat, and a
// node holding thirty seats must not put all of them on the broker at once.
func TestAnInboxTickReadsWithBoundedConcurrency(t *testing.T) {
	t.Parallel()
	q := &countingQueue{counts: map[uuid.UUID]queue.Waiting{}}
	held := make([]uuid.UUID, 30)
	for i := range held {
		held[i] = uuid.New()
	}
	r := &inboxReporter{}
	if got := r.read(t.Context(), q, held); len(got) != len(held) {
		t.Fatalf("read %d seats, want %d", len(got), len(held))
	}
	if peak := q.peak.Load(); peak > inboxReadConcurrency {
		t.Fatalf("%d reads were in flight at once, want at most %d", peak, inboxReadConcurrency)
	}
}

// A REPORT GOES OUT WHEN A COUNT MOVED, A SEAT CAME OR WENT, OR THE HEARTBEAT
// IS DUE, and not otherwise: an empty node that never reported stays silent,
// and one that released its last seat says so once.
func TestAnInboxReportIsDueOnAChangeOrTheHeartbeat(t *testing.T) {
	t.Parallel()
	a := uuid.New()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	same := map[uuid.UUID]queue.Waiting{a: {Background: 2}}
	r := &inboxReporter{sent: maps.Clone(same), sentAt: now, reported: true}
	for _, tc := range []struct {
		name string
		read map[uuid.UUID]queue.Waiting
		at   time.Time
		want bool
	}{
		{"unchanged inside the heartbeat", same, now.Add(InboxReportInterval), false},
		{"a count moved", map[uuid.UUID]queue.Waiting{a: {Background: 3}}, now.Add(time.Second), true},
		{"the heartbeat is due", same, now.Add(InboxReportHeartbeat), true},
		{"the last seat was released", map[uuid.UUID]queue.Waiting{}, now.Add(time.Second), true},
	} {
		if got := r.due(tc.read, tc.at); got != tc.want {
			t.Errorf("%s: due = %v, want %v", tc.name, got, tc.want)
		}
	}
	silent := &inboxReporter{}
	if silent.due(map[uuid.UUID]queue.Waiting{}, now) {
		t.Error("a node that holds nothing and never reported published a report")
	}
}
