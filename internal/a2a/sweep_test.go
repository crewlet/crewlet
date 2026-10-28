package a2a_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events/types"
)

// owedTo is a judge of which idle channels' answers are still owed
// ([a2a.Owed]): the channels it names, by id, get the answer it gives for each,
// and every other channel is owed nothing — the sweep closes it, as it closed
// every idle channel before there was a judge to ask.
type owedTo map[string]owedAnswer

type owedAnswer struct {
	owed bool
	err  error
}

func (o owedTo) AnswerOwed(_ context.Context, ch a2a.Channel) (bool, error) {
	a := o[ch.ID]
	return a.owed, a.err
}

// openAsks opens one channel from alice to each target, under the ids a2a-1,
// a2a-2, … in order, over st.
func openAsks(t *testing.T, st a2a.Store, targets ...string) {
	t.Helper()
	seats := dir{}
	for _, target := range targets {
		seats[target] = true
	}
	n := 0
	svc, err := a2a.New(st, &recorder{}, a2a.Options{
		Directory: seats,
		Now:       func() time.Time { return clock },
		NewID:     func() string { n++; return fmt.Sprintf("a2a-%d", n) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, target := range targets {
		if _, err := svc.Open(context.Background(), a2a.Ask{
			Requester: "alice", Target: target, Brief: "?",
		}); err != nil {
			t.Fatalf("Open: %v", err)
		}
	}
}

// isOpen reports whether channel id is still open in st.
func isOpen(t *testing.T, st a2a.Store, id string) bool {
	t.Helper()
	ch, err := st.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get %s: %v", id, err)
	}
	return ch.Open()
}

// TestTheSweepNeedsNoDirectory is the invariant the split exists for.
//
// The retention sweep runs on a node that has no directory to give — one is
// built from the running company, and the retention of a FLEET-WIDE record
// must not depend on whether this node has applied a configuration. While the
// sweep built a whole [a2a.Service], the directory becoming mandatory
// disarmed it: the construction failed, the engine logged a warning, and both
// channel jobs silently stopped existing.
func TestTheSweepNeedsNoDirectory(t *testing.T) {
	t.Parallel()
	st := a2a.NewCoordStore(memory.NewFleet())
	rec := &recorder{}
	sweeper, err := a2a.NewSweeper(st, rec, owedTo{}, func() time.Time { return clock })
	if err != nil {
		t.Fatalf("NewSweeper: %v", err)
	}

	// A CHANNEL OPENED BY THE ADDRESSING HALF and swept by this one, over
	// ONE store: the two capabilities are separate types, not separate
	// state. Its own publisher, so what the sweep announces is counted
	// without the open's records in the way.
	svc, err := a2a.New(st, &recorder{}, a2a.Options{
		Directory: dir{"bob": true},
		Now:       func() time.Time { return clock },
		NewID:     func() string { return "a2a-swept" },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if _, err := svc.Open(ctx, a2a.Ask{
		Requester: "alice", Target: "bob", Brief: "?",
	}); err != nil {
		t.Fatalf("Open: %v", err)
	}

	swept, err := sweeper.SweepIdle(ctx, clock.Add(time.Hour))
	if err != nil {
		t.Fatalf("SweepIdle: %v", err)
	}
	if swept != 1 {
		t.Fatalf("swept %d channels, want the one that was opened", swept)
	}
	if closes := decodeAll[*types.A2AChannelClosed](t, rec, "a2a_channel_closed"); len(closes) != 1 {
		t.Fatalf("close announcements = %d, want one — the sweep publishes "+
			"through its own queue, not the service's", len(closes))
	}
	// AND THE PURGE, on the same handle: a sweeper that could close and
	// not delete would leave the table growing with rows nothing reads.
	if _, err := sweeper.Purge(ctx, clock.Add(2*time.Hour)); err != nil {
		t.Fatalf("Purge: %v", err)
	}
}

// TestASweeperIsRefusedWithoutAStoreAPublisherOrAJudge — the two refusals
// [a2a.New] makes are the sweeper's own, because they are what it is built
// from; and so is a judge of whether an idle channel's answer is still owed,
// without which the sweep closes the ask a colleague's coding run is still
// working on.
func TestASweeperIsRefusedWithoutAStoreAPublisherOrAJudge(t *testing.T) {
	t.Parallel()
	if _, err := a2a.NewSweeper(nil, &recorder{}, owedTo{}, nil); err == nil {
		t.Error("a sweeper with no channel store was accepted")
	}
	if _, err := a2a.NewSweeper(a2a.NewCoordStore(memory.NewFleet()), nil, owedTo{}, nil); err == nil {
		t.Error("a sweeper with no publisher was accepted")
	}
	if _, err := a2a.NewSweeper(a2a.NewCoordStore(memory.NewFleet()), &recorder{}, nil, nil); err == nil {
		t.Error("a sweeper with no judge of an owed answer was accepted")
	}
	// A NIL CLOCK IS THE REAL PATH and is accepted, so the zero value of
	// the one optional argument is not a refusal.
	if _, err := a2a.NewSweeper(a2a.NewCoordStore(memory.NewFleet()), &recorder{}, owedTo{}, nil); err != nil {
		t.Errorf("a sweeper with the default clock was refused: %v", err)
	}
}

// AN IDLE CHANNEL WHOSE ANSWER IS STILL OWED IS SPARED, AND ONE NOTHING OWES IS
// CLOSED. A coding run has no clock bound and can wait days on a person's
// question, so an ask its turn took up is quiet for as long as the run works —
// and closed under it, the answer the run brings back is refused as a reply on
// a closed channel and the asker is never woken. A channel nothing owes is the
// sweep's to close, as every idle channel was.
//
// Mutation: close an idle channel without asking the judge, and the owed one
// is closed; never close one, and the other stays open.
func TestTheSweepSparesAChannelWhoseAnswerIsStillOwed(t *testing.T) {
	t.Parallel()
	st := a2a.NewCoordStore(memory.NewFleet())
	openAsks(t, st, "bob", "carol")
	rec := &recorder{}
	sweeper, err := a2a.NewSweeper(st, rec, owedTo{"a2a-1": {owed: true}}, func() time.Time { return clock })
	if err != nil {
		t.Fatalf("NewSweeper: %v", err)
	}

	swept, err := sweeper.SweepIdle(context.Background(), clock.Add(time.Hour))
	if err != nil {
		t.Fatalf("SweepIdle: %v", err)
	}
	if swept != 1 {
		t.Fatalf("swept %d channels, want the one nothing owes", swept)
	}
	if !isOpen(t, st, "a2a-1") {
		t.Error("the channel whose answer is still owed was closed: its answer will be refused")
	}
	if isOpen(t, st, "a2a-2") {
		t.Error("the channel nothing owes was left open")
	}
	closes := decodeAll[*types.A2AChannelClosed](t, rec, "a2a_channel_closed")
	if len(closes) != 1 || closes[0].ChannelID != "a2a-2" {
		t.Fatalf("announced closes %+v, want the one swept channel", closes)
	}
}

// A CHANNEL THE JUDGE CANNOT ANSWER FOR IS SPARED FOR THE TICK, and the rest are
// swept: closing is the step nothing can undo, and the next tick asks again.
// The error is the sweep's, so the duty reports the tick.
//
// Mutation: read an unanswered judge as "not owed", and the channel is closed
// on a store blip; stop at the error, and the next channel is never swept.
func TestTheSweepSparesAChannelItCannotJudge(t *testing.T) {
	t.Parallel()
	st := a2a.NewCoordStore(memory.NewFleet())
	openAsks(t, st, "bob", "carol")
	blip := errors.New("the run records could not be read")
	sweeper, err := a2a.NewSweeper(st, &recorder{}, owedTo{"a2a-1": {err: blip}},
		func() time.Time { return clock })
	if err != nil {
		t.Fatalf("NewSweeper: %v", err)
	}

	swept, err := sweeper.SweepIdle(context.Background(), clock.Add(time.Hour))
	if !errors.Is(err, blip) || !strings.Contains(err.Error(), "a2a-1") {
		t.Fatalf("SweepIdle = %v, want the judge's error, naming the channel", err)
	}
	if swept != 1 || isOpen(t, st, "a2a-2") {
		t.Fatalf("swept %d, want the channel after the one it could not judge closed", swept)
	}
	if !isOpen(t, st, "a2a-1") {
		t.Error("a channel the judge could not answer for was closed")
	}
}
