package engine

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// heldServeStore is a run store whose hooks' serves wait at gate, entered
// closed once the first is held, and then land whatever their caller's
// context says: a write the store took before its caller gave up on it.
type heldServeStore struct {
	sandbox.PendingStore
	entered, gate chan struct{}
	once          sync.Once
}

func newHeldServeStore(store sandbox.PendingStore) *heldServeStore {
	return &heldServeStore{PendingStore: store, entered: make(chan struct{}), gate: make(chan struct{})}
}

func (s *heldServeStore) ServeHook(ctx context.Context, turnID string, serve sandbox.HookServe) (
	[]sandbox.RunNote, error) {
	s.once.Do(func() { close(s.entered) })
	<-s.gate
	return s.PendingStore.ServeHook(context.WithoutCancel(ctx), turnID, serve)
}

// lostTo waits until note id on turnID's record has no live serve and its
// serves hold one lost as why, or none at all where why is empty.
func lostTo(t *testing.T, store sandbox.PendingStore, turnID, id string, why sandbox.ServeLoss) {
	t.Helper()
	eventually(t, "note "+id+"'s serve settled", func() bool {
		n := noteOn(t, store, turnID, id)
		if _, live := n.LiveServe(); live || !n.Waiting() {
			return false
		}
		if why == "" {
			return len(n.Serves) == 0
		}
		return slices.ContainsFunc(n.Serves, func(h sandbox.HookServed) bool { return h.Lost == why })
	})
}

// A CANCEL OF AN ANSWERED TAKE SERVES ITS NOTE AT THE NEXT BOUNDARY. The harness
// gave up a hook's call after its take served a note, so its session never
// took it: the inbox keeps the call, and the job's next take serves the note
// again, its own write losing the cancelled serve, whatever the inbox's view
// or the run's settle (held here) has caught up with.
//
// Mutation: offer no live-served note whatever the ring says, or write the
// serve without the cancelled calls (ServeHook ignoring Cancelled), and the
// next boundary passes the note over.
func TestACancelOfAnAnsweredTakeServesItsNoteAtTheNextBoundary(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	held := &lostServeStore{PendingStore: store, held: make(chan struct{})}
	t.Cleanup(func() { close(held.held) })
	runs, _ := steersOver(t, steerRing, held)
	runs.stopped = ctx.Done()
	launch := workingRun(t, store, runs, "run-1")
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
	inbox := boundInbox(t, runs, store, "run-1", launch)
	viewHolds(t, inbox, 1)
	call := hookCall()
	first := takeOf(t, inbox.Take(ctx, mcpbridge.SteerEventToolBatch, call))
	inbox.Cancelled(ctx, *call)
	answer := inbox.Take(ctx, mcpbridge.SteerEventToolBatch, hookCall())
	if !strings.Contains(answer, "use staging") {
		t.Fatalf("the next boundary was answered %q, want the note a cancel lost served again", answer)
	}
	again := takeOf(t, answer)
	servedNotRead(t, store, "run-1", "n-1", launch, again)
	if serves := noteOn(t, store, "run-1", "n-1").Serves; len(serves) != 2 || serves[0].Take != first ||
		serves[0].Lost != sandbox.ServeCancelled {
		t.Errorf("the note holds serves %+v, want the first lost to its cancel and the second live", serves)
	}
}

// A CANCEL THAT OVERTAKES ITS CALL SERVES NOTHING: the hook gave the call up
// before it reached the inbox, so its take answers nothing and serves nothing;
// the next call serves the note.
//
// Mutation: ignore the ring for a late call, and the call serves a note its
// harness has already given up.
func TestACancelThatOvertakesItsCallServesNothing(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, _ := steersOver(t, steerRing, store)
	launch := workingRun(t, store, runs, "run-1")
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
	inbox := boundInbox(t, runs, store, "run-1", launch)
	call := hookCall()
	inbox.Cancelled(ctx, *call)
	if got := inbox.Take(ctx, mcpbridge.SteerEventFinish, call); got != "" {
		t.Errorf("a call cancelled before it arrived was answered %q", got)
	}
	if n := noteOn(t, store, "run-1", "n-1"); len(n.Serves) != 0 {
		t.Errorf("a call cancelled before it arrived served %+v", n.Serves)
	}
	if got := inbox.Take(ctx, mcpbridge.SteerEventFinish, hookCall()); !strings.Contains(got, "use staging") {
		t.Errorf("the next call was answered %q, want the note", got)
	}
}

// A CALL CANCELLED WHILE ITS TAKE RAN IS ANSWERED NOTHING: the cancel finds the
// take running, so it is the take's to settle; its write lands after, and the
// take, finding its call in the ring, answers nothing and has its serve
// removed, so the note waits unserved.
//
// Mutation: have the take ignore the ring once its write lands, and it hands
// over notes its harness gave up.
func TestACallCancelledWhileItsTakeRanIsAnsweredNothing(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	held := newHeldServeStore(store)
	runs, _ := steersOver(t, steerRing, held)
	runs.stopped = ctx.Done()
	launch := workingRun(t, store, runs, "run-1")
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
	inbox := boundInbox(t, runs, store, "run-1", launch)
	call := hookCall()
	answered := make(chan string, 1)
	go func() { answered <- inbox.Take(ctx, mcpbridge.SteerEventFinish, call) }()
	<-held.entered
	inbox.Cancelled(ctx, *call)
	close(held.gate)
	if got := <-answered; got != "" {
		t.Errorf("a call cancelled while its take ran was answered %q", got)
	}
	lostTo(t, store, "run-1", "n-1", "")
}

// A REQUEST THE HARNESS ABANDONED IS A CANCEL: a hook's request that ends
// cancelled while its take's write is held (the harness aborting it, the
// newer protocol's way of giving a call up) answers nothing, and the serve is
// removed. A take whose own budget ends is no cancel: the hook still waits,
// and is answered its notes.
//
// Mutation: ignore the request's cancellation, or read the budget's end as
// one, and a case goes red.
func TestARequestTheHarnessAbandonedIsACancel(t *testing.T) {
	t.Parallel()
	t.Run("its request cancelled", func(t *testing.T) {
		t.Parallel()
		store := sandbox.NewCoordStore(coordmem.NewFleet())
		held := newHeldServeStore(store)
		runs, _ := steersOver(t, steerRing, held)
		runs.stopped = t.Context().Done()
		launch := workingRun(t, store, runs, "run-1")
		fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
		inbox := boundInbox(t, runs, store, "run-1", launch)
		ctx, abandon := context.WithCancel(t.Context())
		answered := make(chan string, 1)
		go func() { answered <- inbox.Take(ctx, mcpbridge.SteerEventFinish, hookCall()) }()
		<-held.entered
		abandon()
		close(held.gate)
		if got := <-answered; got != "" {
			t.Errorf("an abandoned request was answered %q", got)
		}
		lostTo(t, store, "run-1", "n-1", "")
	})
	t.Run("its own budget spent", func(t *testing.T) {
		t.Parallel()
		store := sandbox.NewCoordStore(coordmem.NewFleet())
		held := newHeldServeStore(store)
		close(held.gate)
		runs, _ := steersOver(t, steerRing, held)
		launch := workingRun(t, store, runs, "run-1")
		fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
		inbox := boundInbox(t, runs, store, "run-1", launch)
		viewHolds(t, inbox, 1)
		spent, done := context.WithDeadline(t.Context(), time.Unix(0, 0))
		defer done()
		answer := inbox.Take(spent, mcpbridge.SteerEventToolBatch, hookCall())
		if !strings.Contains(answer, "use staging") {
			t.Fatalf("a take whose own budget was spent as its write landed answered %q, want its note", answer)
		}
		servedNotRead(t, store, "run-1", "n-1", launch, takeOf(t, answer))
	})
}

// A CANCEL ON THE SEAT'S NEXT HOLDER LOSES THE SERVE. The cancel is relayed to
// the node holding the run's seat now, whose inbox served nothing and so can
// rule nothing out: it asks the store, which loses the serve by its call on
// the fleet-wide record, and the new holder's next take serves the note.
//
// Mutation: have an inbox that served nothing ask nothing, and the serve
// stands.
func TestACancelOnTheSeatsNextHolderLosesTheServe(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	first, _ := steersOver(t, steerRing, store)
	next, _ := steersOver(t, steerRing, store)
	next.stopped = ctx.Done()
	launch := workingRun(t, store, first, "run-1")
	fileSealed(t, first, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
	call := hookCall()
	takeOf(t, boundInbox(t, first, store, "run-1", launch).Take(ctx, mcpbridge.SteerEventFinish, call))
	holder := boundInbox(t, next, store, "run-1", launch)
	holder.Cancelled(ctx, *call)
	lostTo(t, store, "run-1", "n-1", sandbox.ServeCancelled)
	if got := holder.Take(ctx, mcpbridge.SteerEventFinish, hookCall()); !strings.Contains(got, "use staging") {
		t.Errorf("the new holder's next boundary was answered %q, want the note served again", got)
	}
}

// A CANCEL HEARD AS THE OPENING ENDS STILL LOSES THE SERVE: the inbox an
// opening's end took off it still hears a cancel whose POST reached the
// opening, and acts on the job it was bound to, closed or not.
//
// Mutation: return from Cancelled on a closed inbox, and the serve stands.
func TestACancelHeardAsTheOpeningEndsStillLosesTheServe(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, _ := steersOver(t, steerRing, store)
	runs.stopped = ctx.Done()
	launch := workingRun(t, store, runs, "run-1")
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
	inbox := boundInbox(t, runs, store, "run-1", launch)
	call := hookCall()
	takeOf(t, inbox.Take(ctx, mcpbridge.SteerEventFinish, call))
	inbox.Close()
	inbox.Cancelled(ctx, *call)
	lostTo(t, store, "run-1", "n-1", sandbox.ServeCancelled)
}

// CANCELS ASK THE STORE ONCE A ROUND, AND ONLY FOR CALLS NOT RULED OUT. An inbox
// that has served knows its calls, so a burst of cancels of calls it never
// took (a model's calls given up at their timeout) asks the store nothing; a
// new holder, which has served nothing, asks for every cancel it hears, merged
// into the run's one settle, so a burst is one ask a round, not one each.
//
// Mutation: ask the store per cancel, or for a call the inbox never took, and
// a case goes red.
func TestCancelsAskTheStoreOnceARoundAndOnlyForCallsNotRuledOut(t *testing.T) {
	t.Parallel()
	t.Run("an inbox that served", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		store := sandbox.NewCoordStore(coordmem.NewFleet())
		runs, _ := steersOver(t, steerRing, store)
		launch := workingRun(t, store, runs, "run-1")
		fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
		inbox := boundInbox(t, runs, store, "run-1", launch)
		takeOf(t, inbox.Take(ctx, mcpbridge.SteerEventFinish, hookCall()))
		for range 20 {
			inbox.Cancelled(ctx, *hookCall())
		}
		runs.settleMu.Lock()
		asked := len(runs.settling)
		runs.settleMu.Unlock()
		if asked != 0 {
			t.Errorf("cancels of calls the inbox never took asked the store to settle %d runs", asked)
		}
	})
	t.Run("a new holder", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		store := sandbox.NewCoordStore(coordmem.NewFleet())
		held := &lostServeStore{PendingStore: store, held: make(chan struct{})}
		runs, _ := steersOver(t, steerRing, held)
		runs.stopped = ctx.Done()
		launch := workingRun(t, store, runs, "run-1")
		inbox := boundInbox(t, runs, store, "run-1", launch)
		var calls []sandbox.NoteCall
		for range 5 {
			call := *hookCall()
			calls = append(calls, call)
			inbox.Cancelled(ctx, call)
		}
		close(held.held)
		var asked []sandbox.HookSettle
		eventually(t, "every cancel asked", func() bool {
			asked = held.asked()
			var got []sandbox.NoteCall
			for _, a := range asked {
				got = append(got, a.Cancelled...)
			}
			return len(got) >= len(calls)
		})
		if len(asked) > 2 {
			t.Errorf("a burst of %d cancels asked the store %d times, want one ask a round: %+v", len(calls),
				len(asked), asked)
		}
	})
	t.Run("a cancel heard again", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		store := sandbox.NewCoordStore(coordmem.NewFleet())
		held := &lostServeStore{PendingStore: store}
		runs, _ := steersOver(t, steerRing, held)
		runs.stopped = ctx.Done()
		launch := workingRun(t, store, runs, "run-1")
		fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
		inbox := boundInbox(t, runs, store, "run-1", launch)
		call := hookCall()
		takeOf(t, inbox.Take(ctx, mcpbridge.SteerEventFinish, call))
		inbox.Cancelled(ctx, *call)
		lostTo(t, store, "run-1", "n-1", sandbox.ServeCancelled)
		eventually(t, "the first hearing's loop ended", func() bool {
			runs.settleMu.Lock()
			defer runs.settleMu.Unlock()
			return len(runs.settling) == 0
		})
		// HELD FROM HERE, so a loop a repeat started stays in settling.
		held.mu.Lock()
		held.held = make(chan struct{})
		held.mu.Unlock()
		t.Cleanup(func() { close(held.held) })
		for range 5 {
			inbox.Cancelled(ctx, *call)
		}
		runs.settleMu.Lock()
		asked := len(runs.settling)
		runs.settleMu.Unlock()
		if asked != 0 || len(held.asked()) != 1 {
			t.Errorf("a cancel POSTed again started %d loops and the store was asked %d settles, want "+
				"nothing past its first hearing's one", asked, len(held.asked()))
		}
	})
}

// A CANCELLED SERVE IS TOLD AGAIN UNDER THE HEADER. A note whose only serve a
// cancel lost may have reached the session all the same, its answer outrunning
// the cancel, so a launch that continues the session tells it as a note it may
// already show; one the session proved absent is told plainly.
//
// Mutation: tell a note a cancel alone lost plainly, and the case goes red.
func TestACancelledServeIsToldAgainUnderTheHeader(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, _ := steersOver(t, steerRing, store)
	runs.stopped = ctx.Done()
	launch := workingRun(t, store, runs, "run-1")
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
	inbox := boundInbox(t, runs, store, "run-1", launch)
	call := hookCall()
	takeOf(t, inbox.Take(ctx, mcpbridge.SteerEventFinish, call))
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-2", Text: "and tag it", By: "founder"})
	absent := takeOf(t, inbox.Take(ctx, mcpbridge.SteerEventFinish, hookCall()))
	inbox.Cancelled(ctx, *call)
	lostTo(t, store, "run-1", "n-1", sandbox.ServeCancelled)
	if _, err := store.SettleHook(ctx, "run-1", sandbox.HookSettle{Launch: launch,
		Absent: []string{absent}}); err != nil {
		t.Fatal(err)
	}
	if got := servedNotes(mustRow(t, store, "run-1")); !slices.Equal(got, []string{"n-1"}) {
		t.Errorf("a continuation is told %v under the header, want the note a cancel alone lost", got)
	}
}

// A JOB'S PENDING SETTLE KEEPS ITS NEWEST LOSSES. A run's loop merges every
// settle a job's inbox hands it into one, so a box that loses serves faster
// than its store answers would grow it without end: each list keeps its
// newest steerLossesKept, as the inbox's own rings do.
//
// Mutation: leave either list uncapped, and the case goes red.
func TestAJobsPendingSettleKeepsItsNewestLosses(t *testing.T) {
	t.Parallel()
	var held hookSettle
	const asked = steerLossesKept + 8
	for k := range asked {
		held.merge(sandbox.HookSettle{Launch: "job-1", Unanswered: []string{"take-" + strconv.Itoa(k)},
			Cancelled: []sandbox.NoteCall{{Session: "mcp-session-1", ID: strconv.Itoa(k)}}})
	}
	if got := held.settle.Unanswered; len(got) != steerLossesKept || got[0] != "take-8" ||
		got[len(got)-1] != "take-"+strconv.Itoa(asked-1) {
		t.Errorf("the pending settle holds %d unanswered takes, %v, want the newest %d", len(got), got,
			steerLossesKept)
	}
	if got := held.settle.Cancelled; len(got) != steerLossesKept || got[0].ID != "8" {
		t.Errorf("the pending settle holds %d cancelled calls, want the newest %d", len(got), steerLossesKept)
	}
}

// servesLostSaid is the count of notes each steer_serve_lost line logs holds
// for turnID says.
func servesLostSaid(logs *bundleLog, turnID string) []int {
	var out []int
	for _, line := range strings.Split(logs.String(), "\n") {
		var rec struct {
			Msg    string `json:"msg"`
			TurnID string `json:"turn_id"`
			Notes  int    `json:"notes"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "steer_serve_lost" && rec.TurnID == turnID {
			out = append(out, rec.Notes)
		}
	}
	return out
}

// A SERVE LOST TO A CANCEL IS SAID WHERE IT IS LOST, AND ONLY IT: by the settle
// loop, counting the notes its write lost to a cancel and never those a take
// answered nothing merged into the same settle; and by a serve's own write
// that loses one, which no loop then says. NOT PARALLEL: it reads the
// process's log.
//
// Mutation: count every note the merged settle answered, or say nothing where
// a serve's write lost one, and a case goes red.
func TestAServeLostToACancelIsSaidWhereItIsLost(t *testing.T) {
	logs := captureSteerLogs(t)
	ctx := t.Context()
	t.Run("by the settle loop", func(t *testing.T) {
		store := sandbox.NewCoordStore(coordmem.NewFleet())
		runs, _ := steersOver(t, steerRing, store)
		runs.stopped = ctx.Done()
		launch := workingRun(t, store, runs, "run-lost-1")
		calls := map[string]*sandbox.NoteCall{}
		for _, id := range []string{"n-1", "n-2"} {
			fileSealed(t, runs, "run-lost-1", steer.Note{ID: id, Text: "the note " + id, By: "founder"})
			calls[id] = hookCall()
			if _, err := store.ServeHook(ctx, "run-lost-1", sandbox.HookServe{IDs: []string{id}, Launch: launch,
				Statuses: steerTakeStatuses, Serve: sandbox.HookServed{Take: "take-" + id,
					Boundary: string(types.SteerBoundaryFinish), Call: calls[id]}}); err != nil {
				t.Fatal(err)
			}
		}
		runs.settleHook(ctx, "run-lost-1", sandbox.HookSettle{Launch: launch, Unanswered: []string{"take-n-1"},
			Cancelled: []sandbox.NoteCall{*calls["n-2"]}})
		eventually(t, "the settle answered", func() bool {
			runs.settleMu.Lock()
			defer runs.settleMu.Unlock()
			return len(runs.settling) == 0
		})
		if said := servesLostSaid(logs, "run-lost-1"); !slices.Equal(said, []int{1}) {
			t.Errorf("steer_serve_lost said %v notes, want once, of the one note a cancel lost", said)
		}
	})
	t.Run("by a serve's own write", func(t *testing.T) {
		store := sandbox.NewCoordStore(coordmem.NewFleet())
		held := &lostServeStore{PendingStore: store, held: make(chan struct{})}
		t.Cleanup(func() { close(held.held) })
		runs, _ := steersOver(t, steerRing, held)
		runs.stopped = ctx.Done()
		launch := workingRun(t, store, runs, "run-lost-2")
		fileSealed(t, runs, "run-lost-2", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
		inbox := boundInbox(t, runs, store, "run-lost-2", launch)
		call := hookCall()
		takeOf(t, inbox.Take(ctx, mcpbridge.SteerEventFinish, call))
		inbox.Cancelled(ctx, *call)
		takeOf(t, inbox.Take(ctx, mcpbridge.SteerEventFinish, hookCall()))
		if said := servesLostSaid(logs, "run-lost-2"); !slices.Equal(said, []int{1}) {
			t.Errorf("steer_serve_lost said %v notes as the serve's write lost one, want once, of one", said)
		}
	})
}
