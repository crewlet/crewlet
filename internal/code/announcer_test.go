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
	case <-time.After(10 * time.Second): //fixedwait:liveness
		t.Fatal("no announcement reached the stream in 10 s")
	}
}

// heldCommitter is a committer on n announcing on stream, whose announcer
// holds at most announcements unpublished, at the harness's clock unless tune
// says otherwise. The stream is released before the committer is stopped, so
// the stop never waits on a held publish.
func heldCommitter(t *testing.T, n *node, stream *heldStream, announcements int,
	tune ...func(*code.CommitterOptions)) *code.Committer {

	t.Helper()
	o := code.CommitterOptions{
		Publisher: n.pub, DB: n.db.Reader(), NodeID: n.id, Limits: code.DefaultLimits,
		Retention: func() time.Duration { return code.DefaultDeletedRetention },
		SeatRules: func() (code.SeatRules, bool) { return code.SeatRules{Roster: testChart(), Zone: time.UTC}, true },
		Now:       func() time.Time { return harnessAt }, Events: stream, AgentIDs: agentIDs,
	}
	for _, f := range tune {
		f(&o)
	}
	c, err := code.NewCommitterAnnouncing(o, announcements)
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
	case <-time.After(30 * time.Second): //fixedwait:liveness
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
// one more takes the second open's and drops the third's — whose repository's
// next write is answered all the same — and, released, the stream takes the
// first two, the second stamped the instant its record landed, by the
// committer's clock, rather than when it was published.
//
// Each handover is read off its repository's next write: a repository's
// goroutine hands what a write landed to the announcer before it takes the
// write after, so that write is answered only once the opening is held or
// dropped. What the stream took is read once the committer has stopped, which
// publishes everything its announcer holds. And the clock is the case's, moved
// an hour between the handovers and the release.
//
// Mutations: wait for room in the announcer (a plain send) — api's next push
// waits behind the held stream, red; stamp an announcement when it is
// published (drop the Timestamp it was handed over at, or read the clock
// then) — the second is stamped an hour late, red; hold one more than the
// announcer may — the stream takes three, red.
func TestAFullAnnouncerDropsWhatItCannotHoldAndNeverWaits(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	stream := newHeldStream()
	clock := &fakeClock{at: harnessAt}
	c := heldCommitter(t, n, stream, 1, func(o *code.CommitterOptions) { o.Now = clock.now })
	web, api := changeRepo(t, n, "web"), changeRepo(t, n, "api")

	requireOpened(t, openAsync(t.Context(), c, opening(web, featureRef, mainRef, dev)))
	stream.waitHeld(t)
	queued := opening(web, releaseRef, mainRef, dev)
	requireOpened(t, openAsync(t.Context(), c, queued))
	requireOK(t, await(t, proposeAsync(t.Context(), c, proposal(web, update("refs/heads/kept", code.ZeroOID,
		oidOf("K"))))))
	requireOpened(t, openAsync(t.Context(), c, opening(api, featureRef, mainRef, dev)))
	requireOK(t, await(t, proposeAsync(t.Context(), c, proposal(api, update("refs/heads/kept", code.ZeroOID,
		oidOf("K"))))))

	landed := clock.now()
	clock.advance(time.Hour)
	stream.release()
	opened := settled(t, c, &stream.announced, "change_opened")
	if len(opened) != 2 {
		t.Fatalf("the stream took %d openings once released, want the first two: the announcer holds one, and "+
			"drops the third", len(opened))
	}
	second := opened[1]
	if got := second.Data.(*types.ChangeOpened); got.ChangeID != queued.Open.ChangeID || got.RepoID != web {
		t.Fatalf("the stream's second opening is %+v, want web's second change, the one the announcer held", got)
	}
	if !second.Timestamp.Equal(landed) {
		t.Fatalf("the held opening is stamped %s, want %s: an announcement carries the instant its record "+
			"landed by the committer's clock, never the instant it was published (%s)", second.Timestamp, landed,
			clock.now())
	}
}

// A MERGE'S ANNOUNCEMENT HOLDS NO PUSH OF ITS REPOSITORY: the merge is where
// the defect began — mergeWrite answers its asker and then announces — so it
// is held to the announcer on its own: with the stream holding every publish,
// dev's merge of web's change is answered merged, a push to web proposed
// behind it is answered while the merge's change_merged is still held, and,
// released, the stream takes the merge once, at its commit — read once the
// committer has stopped, which publishes everything its announcer holds.
//
// Mutation: announce the merge inline in mergeWrite (announceMerged in place
// of announceLater) — the push waits behind a publish the stream never
// acknowledges, red.
func TestAMergesAnnouncementHoldsNoPushOfItsRepository(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	stream := newHeldStream()
	c := heldCommitter(t, n, stream, code.CommitterAnnouncements)
	w, _ := writerOn(t, n, n.pub)
	repo, change := mergeRepo(t, n, c, "web", code.DirectPersons, mergeGate())
	stream.waitHeld(t)
	approved(t, n, w, repo, change)
	converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, code.MergeClean))

	res, err := c.ProposeMerge(t.Context(), merging(repo, change, dev, code.MergeMethodMerge, oidOf("A"),
		oidOf("B"), oidOf("M")))
	requireMerged(t, res, err, oidOf("M"))

	requireOK(t, await(t, proposeAsync(t.Context(), c, proposal(repo,
		update("refs/heads/kept", code.ZeroOID, oidOf("K"))))))
	if got := len(stream.all()); got != 0 {
		t.Fatalf("the stream took %d events while it held the opening's, want none", got)
	}

	stream.release()
	merged := settled(t, c, &stream.announced, "change_merged")
	if len(merged) != 1 {
		t.Fatalf("the stream took %d merges once released, want the merge once", len(merged))
	}
	if got := merged[0].Data.(*types.ChangeMerged); got.ChangeID != change || got.Commit != oidOf("M") {
		t.Fatalf("the merge was announced as %+v, want web's change merged at its commit", got)
	}
}
