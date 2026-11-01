package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
)

// broker is a publisher a case turns off and on, counting what reached it and
// keeping what it took.
type broker struct {
	mu       sync.Mutex
	down     bool
	attempts int
	took     []*events.Event
}

func (b *broker) publish(_ context.Context, ev *events.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.attempts++
	if b.down {
		return fmt.Errorf("publish crewlet.events.%s: %w: context deadline exceeded",
			ev.Type, queue.ErrUnavailable)
	}
	b.took = append(b.took, ev)
	return nil
}

func (b *broker) set(down bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.down = down
}

func (b *broker) tried() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.attempts
}

// taken is every notice the broker took, as its seat and payload.
func (b *broker) taken() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.took))
	for _, ev := range b.took {
		out = append(out, noticeOf(ev))
	}
	return out
}

// noticeOf is a notice in a case's words: whose, which and, for a park, until
// when.
func noticeOf(ev *events.Event) string {
	if p, ok := events.DataAs[*types.SeatProviderParked](ev); ok && p != nil {
		return p.AgentHandle + " parked until " + p.RetryAt.Format("15:04")
	}
	if p, ok := events.DataAs[*types.SeatProviderReleased](ev); ok && p != nil {
		return p.AgentHandle + " released: " + p.Reason
	}
	return ev.Type
}

func parkedNotice(handle string, retry time.Time) *events.Event {
	return events.NewFrom(types.SeatProviderParked{AgentHandle: handle, RetryAt: retry,
		ProviderKeys: []string{"default"}}, events.TraceContext{})
}

func releasedNotice(handle, why string) *events.Event {
	return events.NewFrom(types.SeatProviderReleased{AgentHandle: handle, Reason: why},
		events.TraceContext{})
}

// logLines counts the log lines named event in buf.
func logLines(buf *syncBuffer, event string) int {
	return strings.Count(buf.String(), "msg="+event)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newNotices is a publisher on a schedule of base, logging into a buffer.
func newNotices(t *testing.T, base time.Duration) (*parkNotices, *syncBuffer) {
	t.Helper()
	buf := &syncBuffer{}
	n := &parkNotices{base: base,
		logger: slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	t.Cleanup(n.stop)
	return n, buf
}

// A BROKER OUTAGE COSTS ONE ATTEMPT PER STEP OF THE BACKOFF, HOWEVER MANY PARKS
// IT SPANS, AND ONE LINE EACH WAY. One node logged 36 000
// `seat_provider_park_not_published` lines in a three-minute outage: every
// renewal of every parked seat was published on the spot, failed and said so.
// Held, the notices wait for the broker on one schedule, the newest per seat,
// and land once it answers, each seat's latest state exactly once.
//
// Mutation: publish every notice on the spot, as before, and the broker is
// asked six thousand times.
func TestABrokerOutageCostsOneAttemptPerStepAndOneLineEachWay(t *testing.T) {
	t.Parallel()
	n, logs := newNotices(t, 5*time.Millisecond)
	b := &broker{down: true}
	at := time.Date(2026, 11, 1, 0, 49, 0, 0, time.UTC)

	// THE STORM: three parked seats renewed two thousand times each, as the
	// probe that was re-armed for an instant already past renewed them.
	const renewals = 2000
	seats := []string{"pm", "eng", "qa"}
	for i := range renewals {
		for _, seat := range seats {
			n.post(t.Context(), b.publish, seat, parkedNotice(seat, at.Add(time.Duration(i)*time.Minute)))
		}
	}
	time.Sleep(60 * time.Millisecond)
	// AT MOST ONE ATTEMPT PER STEP: the first post's own, and one per step
	// of a 5ms schedule in the time slept (5, 10, 20, 40ms), with room for
	// a step's spread. A loaded machine fires fewer, never more.
	if got := b.tried(); got > 8 {
		t.Errorf("the broker was asked %d times for %d notices during the outage, want "+
			"one attempt per step of the backoff", got, renewals*len(seats))
	}
	if got := logLines(logs, "seat_provider_park_notices_held"); got != 1 {
		t.Errorf("the outage was said %d times, want once:\n%s", got, logs)
	}

	b.set(false)
	eventually(t, "the held notices to land once the broker answered",
		func() bool { return len(b.taken()) == len(seats) })
	time.Sleep(20 * time.Millisecond)
	last := at.Add((renewals - 1) * time.Minute).Format("15:04")
	want := []string{"eng parked until " + last, "pm parked until " + last, "qa parked until " + last}
	if got := b.taken(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("the broker took %q, want each seat's newest notice once: %q", got, want)
	}
	if got := logLines(logs, "seat_provider_park_notices_published"); got != 1 {
		t.Errorf("the recovery was said %d times, want once:\n%s", got, logs)
	}

	// AND A NOTICE AFTER IT IS PUBLISHED ON THE SPOT, by its caller.
	n.post(t.Context(), b.publish, "pm", releasedNotice("pm", "its models answered"))
	if got := b.taken(); len(got) != 4 || got[3] != "pm released: its models answered" {
		t.Errorf("a notice after the outage was not published by its caller: %q", got)
	}
}

// A NOTICE IS PUBLISHED ONCE PER CHANGE. A renewal that names what the last
// notice named is no news, and a screen hears nothing new from it; one that
// moves anything is published.
//
// Mutation: drop the comparison, and the repeated notice is published again.
func TestAParkNoticeIsPublishedOncePerChange(t *testing.T) {
	t.Parallel()
	n, _ := newNotices(t, time.Millisecond)
	b := &broker{}
	at := time.Date(2026, 11, 1, 5, 43, 0, 0, time.UTC)

	n.post(t.Context(), b.publish, "pm", parkedNotice("pm", at))
	n.post(t.Context(), b.publish, "pm", parkedNotice("pm", at))
	n.post(t.Context(), b.publish, "eng", parkedNotice("eng", at))
	n.post(t.Context(), b.publish, "pm", parkedNotice("pm", at.Add(time.Hour)))
	n.post(t.Context(), b.publish, "pm", parkedNotice("pm", at.Add(time.Hour)))
	want := []string{"pm parked until 05:43", "eng parked until 05:43", "pm parked until 06:43"}
	if got := b.taken(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("the broker took %q, want one notice per change: %q", got, want)
	}
}

// THE NEWEST NOTICE WINS, and one that returns the seat to what the broker
// already has replaces nothing. A seat parked and released during an outage is
// published released, once, never parked after it; one released and parked
// again as it was is not published at all, since a screen still draws it
// parked as it was.
//
// Mutation: publish the held notices in arrival order rather than the newest,
// and the released seat is drawn parked again.
func TestTheNewestParkNoticeWinsAnOutage(t *testing.T) {
	t.Parallel()
	n, _ := newNotices(t, time.Millisecond)
	b := &broker{}
	at := time.Date(2026, 11, 1, 0, 49, 0, 0, time.UTC)
	n.post(t.Context(), b.publish, "eng", parkedNotice("eng", at))

	b.set(true)
	n.post(t.Context(), b.publish, "pm", parkedNotice("pm", at))
	n.post(t.Context(), b.publish, "pm", releasedNotice("pm", "its models answered"))
	n.post(t.Context(), b.publish, "eng", releasedNotice("eng", "the seat's models changed"))
	n.post(t.Context(), b.publish, "eng", parkedNotice("eng", at))
	b.set(false)

	eventually(t, "the held notices to land once the broker answered",
		func() bool { return len(b.taken()) == 2 })
	time.Sleep(20 * time.Millisecond)
	want := []string{"eng parked until 00:49", "pm released: its models answered"}
	if got := b.taken(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("the broker took %q, want the newest notice of the seat that changed: %q",
			got, want)
	}
}

// A STOPPING NODE DROPS WHAT IT HELD AND ENDS ITS RETRY, and returns once the
// retry has: no publish this node started outlives its teardown, and nothing
// is published after it.
func TestAStoppedNodeDropsItsHeldParkNotices(t *testing.T) {
	t.Parallel()
	n, _ := newNotices(t, time.Millisecond)
	stuck := make(chan struct{})
	var once sync.Once
	var after int
	var mu sync.Mutex
	publish := func(ctx context.Context, _ *events.Event) error {
		mu.Lock()
		after++
		mu.Unlock()
		once.Do(func() { close(stuck) })
		<-ctx.Done()
		return errors.New("the broker did not answer")
	}
	at := time.Date(2026, 11, 1, 0, 49, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-stuck
		cancel()
	}()
	// The caller's own publish is ended by its context, which holds the
	// notice for the retry; the retry's is ended by the stop.
	n.post(ctx, publish, "pm", parkedNotice("pm", at))
	eventually(t, "the held notice to be retried",
		func() bool { mu.Lock(); defer mu.Unlock(); return after >= 2 })

	done := make(chan struct{})
	go func() {
		n.stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stop did not end the retry's publish")
	}
	mu.Lock()
	tried := after
	mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	n.post(t.Context(), publish, "pm", releasedNotice("pm", "the node stopped"))
	mu.Lock()
	defer mu.Unlock()
	if after != tried {
		t.Errorf("%d publishes after the stop, want none", after-tried)
	}
}
