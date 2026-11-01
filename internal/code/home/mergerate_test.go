package home_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/home"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE QUEUE PRICES ITS MERGE RATE AT THE ROUNDS IT TIMED (M5-D28, ADR-0015):
// main's queue at depth 2 and batch 1, asked thirty enqueues within the
// window by its rows, is judged against what its own trains land —
// config.LandingCapacity at the mean round this node timed — and nothing of
// the alarm's own:
//
//   - before any train, no round is timed: nothing is said;
//   - a train ten minutes into testing is one round of ten so far: twelve an
//     hour, thirty past it — main of web over, at its window, by the read
//     that asked from an hour back;
//   - landed at twenty minutes, the round is timed: six an hour, and five
//     asked is within it — silent;
//   - a new train a minute old does not shorten the mean of twenty: thirty
//     asked is past six again;
//   - drained, the queue's figure goes with its last entry.
//
// Mutation: price every target at the standing train's age alone, or never
// record a landing's round → the young train's minute prices 120, silent,
// red; count the standing train even where it is younger than the mean → a
// mean of ten and a half, eleven, red; keep a drained target's figure → it
// still fires, red.
func TestTheQueuePricesItsMergeRateAtTheRoundsItTimed(t *testing.T) {
	t.Parallel()
	c := &clock{now: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)}
	rows := newQueueRows(oid("a"), 2, 1)
	l, err := home.New(home.Options{Estate: rows, Placement: asNode{placement: &placement{}, node: "git-a"},
		Nudges: newNudges(), Cache: unmerged{t: t}, Git: unmerged{t: t}, Chains: &queueChains{},
		Tasks: &queueTasks{}, NodeID: "git-a", Share: units(2), Now: c.Now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reading := func() statelog.Reading {
		var r statelog.Reading
		l.AlarmReading(&r)
		return r
	}
	set := func(enqueued int) {
		rows.mu.Lock()
		rows.enqueued = enqueued
		rows.mu.Unlock()
	}
	over := func(step string, capacity int, round time.Duration) {
		t.Helper()
		want := statelog.Reading{CodeMergeRateOver: 1, CodeMergeRateOverTarget: "main of web",
			CodeMergeRateOverAsked: 30, CodeMergeRateOverCapacity: capacity, CodeMergeRateOverRound: round,
			CodeMergeRateWindow: code.MergeRateWindow}
		if got := reading(); merged(got) != merged(want) {
			t.Fatalf("%s: the reading is %+v, want main of web's 30 against %d at a round of %s", step,
				merged(got), capacity, round)
		}
	}
	silent := func(step string) {
		t.Helper()
		if got := reading(); merged(got) != merged(statelog.Reading{}) {
			t.Fatalf("%s: the reading is %+v, want nothing", step, merged(got))
		}
	}

	rows.enqueue(t, 2)
	set(30)
	start := c.Now()
	turn(t, l, rows)
	silent("no round timed")
	rows.mu.Lock()
	since := append([]time.Time(nil), rows.since...)
	rows.mu.Unlock()
	if len(since) == 0 || !since[0].Equal(start.Add(-code.MergeRateWindow)) {
		t.Fatalf("the queue work asked enqueues since %v, want an hour before the turn, %v", since,
			start.Add(-code.MergeRateWindow))
	}

	c.advance(10 * time.Minute)
	turn(t, l, rows)
	over("a train ten minutes in", 12, 10*time.Minute)

	train, _ := rows.snapshot()
	for _, cand := range train {
		rows.ended(cand.TipOID, "unit", code.CheckStatePassed)
	}
	c.advance(10 * time.Minute)
	turn(t, l, rows)
	if _, live := rows.snapshot(); len(live) != 0 {
		t.Fatalf("the premise: %d entries are live after the green train's turn, want it landed", len(live))
	}

	c.advance(time.Minute)
	rows.enqueue(t, 2)
	set(5)
	turn(t, l, rows)
	silent("five asked within a timed round's six")

	c.advance(time.Minute)
	set(30)
	turn(t, l, rows)
	over("a young train beside a timed round", 6, 20*time.Minute)

	rows.mu.Lock()
	rows.entries, rows.train = nil, nil
	rows.mu.Unlock()
	turn(t, l, rows)
	silent("drained")
}

// A QUEUE THIS NODE NO LONGER TURNS IS FORGOTTEN: a target's merge rate is
// kept while its train stands or its figure is within the window — so a
// reading taken before the train's first measurement keeps the round it is
// timing — and dropped by the first reading past a window with neither, as a
// queue whose home moved to another node, or whose repository was deleted,
// is never turned here again and so never reached by a drained turn. A
// long-lived git node holds a figure for the queues it is home to now, never
// one for every queue it ever was.
//
// Mutation: never forget a target → it is still held a window on, red;
// forget a target whose figure lapsed though its train stands → the first
// reading drops the round under way, held 0, red.
func TestAQueueThisNodeNoLongerTurnsIsForgotten(t *testing.T) {
	t.Parallel()
	c := &clock{now: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)}
	rows := newQueueRows(oid("a"), 2, 1)
	l, err := home.New(home.Options{Estate: rows, Placement: asNode{placement: &placement{}, node: "git-a"},
		Nudges: newNudges(), Cache: unmerged{t: t}, Git: unmerged{t: t}, Chains: &queueChains{},
		Tasks: &queueTasks{}, NodeID: "git-a", Share: units(2), Now: c.Now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reading := func() statelog.Reading {
		var r statelog.Reading
		l.AlarmReading(&r)
		return r
	}
	rows.enqueue(t, 2)
	rows.mu.Lock()
	rows.enqueued = 30
	rows.mu.Unlock()

	turn(t, l, rows)
	reading()
	if n := home.MergeRateTargets(l); n != 1 {
		t.Fatalf("a train just built: %d target(s) held, want main's, whose round is under way", n)
	}
	c.advance(10 * time.Minute)
	turn(t, l, rows)
	if r := reading(); r.CodeMergeRateOver != 1 {
		t.Fatalf("the premise: a train ten minutes in reads %d queue(s) over, want main's", r.CodeMergeRateOver)
	}

	c.advance(code.MergeRateWindow + time.Minute)
	if r := reading(); r.CodeMergeRateOver != 0 {
		t.Fatalf("a window on with no turn reads %d queue(s) over, want none", r.CodeMergeRateOver)
	}
	if n := home.MergeRateTargets(l); n != 0 {
		t.Fatalf("a window on with no turn of its repository, %d target(s) held, want main's forgotten", n)
	}
}

// rateHalf is a reading's merge-rate half: what the queue fills, compared
// whole.
type rateHalf struct {
	Over, Asked, Capacity int
	Target                string
	Round, Window         time.Duration
}

// merged is r's merge-rate half.
func merged(r statelog.Reading) rateHalf {
	return rateHalf{r.CodeMergeRateOver, r.CodeMergeRateOverAsked, r.CodeMergeRateOverCapacity,
		r.CodeMergeRateOverTarget, r.CodeMergeRateOverRound, r.CodeMergeRateWindow}
}
