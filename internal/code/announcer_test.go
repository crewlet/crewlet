package code_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A COMMITTER'S ANNOUNCEMENTS WAIT ON NONE OF ITS WRITES: the committer's
// goroutine for a repository is that repository's only writer, so what a
// landed record announces is handed to the committer's one announcer and
// published there, never in front of the repository's next write — dropped
// and logged past what the announcer holds, never waited for, and published
// before a stopping committer returns.

// heldStream is an event stream that takes no event until it is released —
// whatever the publisher's context says, as a stream that stopped
// acknowledging holds a publish — and then records each one; held signals
// every publish that reached it.
type heldStream struct {
	announced
	held chan struct{}
	gate chan struct{}
	once sync.Once
}

func newHeldStream() *heldStream {
	return &heldStream{held: make(chan struct{}, 16), gate: make(chan struct{})}
}

func (h *heldStream) Publish(ctx context.Context, subject string, ev *events.Event) error {
	select {
	case h.held <- struct{}{}:
	default:
	}
	<-h.gate
	return h.announced.Publish(ctx, subject, ev)
}

// release lets every publish held, and every one after, through.
func (h *heldStream) release() { h.once.Do(func() { close(h.gate) }) }

// waitHeld waits for a publish to reach the stream and be held there.
func (h *heldStream) waitHeld(t *testing.T) {
	t.Helper()
	select {
	case <-h.held:
	case <-time.After(10 * time.Second):
		t.Fatal("no announcement reached the stream in 10 s")
	}
}

// heldCommitter is a committer on n announcing on stream, whose announcer
// holds at most announcements unpublished. The stream is released before the
// committer is stopped, so the stop never waits on a held publish.
func heldCommitter(t *testing.T, n *node, stream *heldStream, announcements int) *code.Committer {
	t.Helper()
	c, err := code.NewCommitterAnnouncing(code.CommitterOptions{
		Publisher: n.pub, DB: n.db.Reader(), NodeID: n.id, Limits: code.DefaultLimits,
		Retention: func() time.Duration { return code.DefaultDeletedRetention },
		SeatRules: func() (code.SeatRules, bool) { return code.SeatRules{Roster: testChart(), Zone: time.UTC}, true },
		Now:       func() time.Time { return harnessAt }, Events: stream, AgentIDs: agentIDs,
	}, announcements)
	if err != nil {
		t.Fatalf("NewCommitterAnnouncing: %v", err)
	}
	t.Cleanup(func() {
		stream.release()
		c.Stop(context.WithoutCancel(t.Context()))
	})
	return c
}

// openAsync asks c for p on its own goroutine and hands back what became of
// it: nil for a change it opened.
func openAsync(ctx context.Context, c *code.Committer, p code.ChangeProposal) <-chan error {
	out := make(chan error, 1)
	go func() {
		res, err := c.ProposeChange(ctx, p)
		if err == nil && (res.Outcome != statelog.OutcomeApplied || res.Refusal != "" || res.Unchanged) {
			err = fmt.Errorf("the open answered %+v, want a change it opened", res)
		}
		out <- err
	}()
	return out
}

// requireOpened fails t unless the open answered on ch opened its change
// within 30 s.
func requireOpened(t *testing.T, ch <-chan error) {
	t.Helper()
	select {
	case err := <-ch:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the open was not answered in 30 s: it waited on an announcement")
	}
}

// A REPOSITORY'S NEXT PUSH WAITS ON NONE OF WHAT ITS LAST ONE ANNOUNCES: dev's
// push that opens a change hands its `change_opened` to the committer's
// announcer and the repository's goroutine goes on — so the repository's next
// push is answered while the stream still holds that announcement, which the
// stream takes once released.
//
// Mutation: publish the push batch's openings on the repository's goroutine
// (the inline call restored), or run every announcement where it is handed
// over — the next push waits behind a publish the stream never acknowledges,
// red.
func TestARepositorysNextPushWaitsOnNoneOfItsAnnouncements(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	stream := newHeldStream()
	c := heldCommitter(t, n, stream, code.CommitterAnnouncements)
	repo := changeRepo(t, n, "web")

	p, res := pushOpening(t, c, repo, dev, &code.ChangeOption{Title: "Retry the upload"},
		update(featureRef, oidOf("B"), oidOf("C")))
	requireOK(t, answer{res, nil})
	stream.waitHeld(t)

	next := await(t, proposeAsync(t.Context(), c, proposal(repo, update(releaseRef, oidOf("R"), oidOf("S")))))
	requireOK(t, next)
	if got := len(stream.all()); got != 0 {
		t.Fatalf("the stream took %d events while it held the opening's, want none", got)
	}

	stream.release()
	waitAnnouncedOf(t, &stream.announced, "change_opened", 1)
	if got := stream.of("change_opened")[0].Data.(*types.ChangeOpened); got.ChangeID != code.PushChangeID(p.PushID) {
		t.Fatalf("the stream took %+v once released, want the change dev's push opened", got)
	}
}

// AN ANNOUNCER THAT HOLDS ALL IT MAY DROPS THE NEXT, AND NEVER WAITS FOR ROOM:
// behind a stream holding the first opening's publish, an announcer that holds
// one more takes the second open's and drops the third's — whose open is
// answered at once all the same — and, released, the stream takes the first
// two, the second stamped the instant its record landed rather than when it
// was published.
//
// Mutations: wait for room in the announcer (a plain send) — the third open
// waits behind the held stream, red; stamp an announcement when it is
// published (drop the Timestamp it was handed over at) — the second is
// stamped after the release, red.
func TestAFullAnnouncerDropsWhatItCannotHoldAndNeverWaits(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	stream := newHeldStream()
	c := heldCommitter(t, n, stream, 1)
	web, api := changeRepo(t, n, "web"), changeRepo(t, n, "api")

	requireOpened(t, openAsync(t.Context(), c, opening(web, featureRef, mainRef, dev)))
	stream.waitHeld(t)
	queued := opening(web, releaseRef, mainRef, dev)
	requireOpened(t, openAsync(t.Context(), c, queued))
	requireOpened(t, openAsync(t.Context(), c, opening(api, featureRef, mainRef, dev)))

	time.Sleep(20 * time.Millisecond)
	released := time.Now().UTC()
	stream.release()
	waitAnnouncedOf(t, &stream.announced, "change_opened", 2)
	requireNoMore(t, &stream.announced, "change_opened", 2)
	second := stream.of("change_opened")[1]
	if got := second.Data.(*types.ChangeOpened); got.ChangeID != queued.Open.ChangeID || got.RepoID != web {
		t.Fatalf("the stream's second opening is %+v, want web's second change, the one the announcer held", got)
	}
	if !second.Timestamp.Before(released) {
		t.Fatalf("the held opening is stamped %s, at or after its release at %s: an announcement carries the "+
			"instant its record landed", second.Timestamp, released)
	}
}

// A STOPPING COMMITTER PUBLISHES WHAT IT LANDED BEFORE IT RETURNS: Stop waits
// for the announcer to drain what the repositories handed it, so a node that
// stops loses no announcement of a record it landed while its stream still
// takes them.
//
// Mutation: return from Stop once the repositories' goroutines are done,
// without waiting for the announcer — Stop returns while the stream still
// holds the opening, red.
func TestAStoppingCommitterPublishesWhatItLanded(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	stream := newHeldStream()
	c := heldCommitter(t, n, stream, code.CommitterAnnouncements)
	repo := changeRepo(t, n, "web")

	requireOpened(t, openAsync(t.Context(), c, opening(repo, featureRef, mainRef, dev)))
	stream.waitHeld(t)
	stopped := make(chan struct{})
	go func() {
		c.Stop(context.WithoutCancel(t.Context()))
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned while the stream still held the opening it landed")
	case <-time.After(200 * time.Millisecond):
	}
	stream.release()
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("Stop did not return in 30 s once the stream took the opening")
	}
	if got := len(stream.of("change_opened")); got != 1 {
		t.Fatalf("the stream holds %d openings once Stop returned, want the one the committer landed", got)
	}
}

// requireNoMore fails t if stream comes to hold more than n events of
// eventType once a late one would have arrived.
func requireNoMore(t *testing.T, stream *announced, eventType string, n int) {
	t.Helper()
	time.Sleep(20 * time.Millisecond)
	if got := len(stream.of(eventType)); got != n {
		t.Fatalf("the stream holds %d %s events, want %d", got, eventType, n)
	}
}
