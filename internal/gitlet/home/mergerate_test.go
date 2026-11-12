package home_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/home"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
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
	l, err := home.New(home.Options{Chart: noChart,
		Estate: rows, Placement: asNode{placement: &placement{}, node: "git-a"},
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
			CodeMergeRateWindow: gitlet.MergeRateWindow}
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
	if len(since) == 0 || !since[0].Equal(start.Add(-gitlet.MergeRateWindow)) {
		t.Fatalf("the queue work asked enqueues since %v, want an hour before the turn, %v", since,
			start.Add(-gitlet.MergeRateWindow))
	}

	c.advance(10 * time.Minute)
	turn(t, l, rows)
	over("a train ten minutes in", 12, 10*time.Minute)

	train, _ := rows.snapshot()
	for _, cand := range train {
		rows.ended(cand.TipOID, "unit", gitlet.CheckStatePassed)
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
// long-lived Gitlet node holds a figure for the queues it is home to now, never
// one for every queue it ever was.
//
// Mutation: never forget a target → it is still held a window on, red;
// forget a target whose figure lapsed though its train stands → the first
// reading drops the round under way, held 0, red.
func TestAQueueThisNodeNoLongerTurnsIsForgotten(t *testing.T) {
	t.Parallel()
	c := &clock{now: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)}
	rows := newQueueRows(oid("a"), 2, 1)
	l, err := home.New(home.Options{Chart: noChart,
		Estate: rows, Placement: asNode{placement: &placement{}, node: "git-a"},
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

	c.advance(gitlet.MergeRateWindow + time.Minute)
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

// THE QUEUE COUNTS ITS ROUNDS, ITS LANDINGS AND ITS EJECTIONS (M5 §2.10):
// what an operator reads of a queue before code_merge_rate_over_capacity
// fires is the home's own instruments, each recorded where the round it
// counts ends —
//
//   - a train of two landed green, ten minutes after it was built: two
//     changes landed, one round `landed`, timed at ten minutes;
//   - a train of two whose tip failed, bisected to its first entry: that
//     entry ejected `check_failed`, one round `bisected`;
//   - the train built of what was left, given up under a person's push:
//     one round `abandoned`, counted once although the turn after builds
//     again;
//   - the train rebuilt on the pushed tip landed by a land answered PENDING
//     — durable, every node applies it — while another change waits: its
//     change counted landed and its round `landed` where it was appended,
//     never read as abandoned by the turn after.
//
// Mutation: drop the landed count → no change landed, red; time a round from
// the land rather than its build → not ten minutes, red; drop the ejected
// count → no check_failed, red; never count a replaced train abandoned → no
// abandoned round, red; count only a land answered applied → the pending
// one's change uncounted and its train abandoned, red.
func TestTheQueueCountsItsRoundsLandingsAndEjections(t *testing.T) {
	t.Parallel()
	c := &clock{now: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)}
	rec, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	rows := newQueueRows(oid("a"), 1, 2)
	l, err := home.New(home.Options{Chart: noChart,
		Estate: rows, Placement: asNode{placement: &placement{}, node: "git-a"},
		Nudges: newNudges(), Cache: unmerged{t: t}, Git: unmerged{t: t}, Chains: &queueChains{},
		Tasks: &queueTasks{}, NodeID: "git-a", Share: units(2), Now: c.Now, Metrics: rec})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rounds := func(outcome string) uint64 {
		return counted(rec, metrics.CodeQueueRounds, metrics.Attrs{"outcome": outcome})
	}

	rows.enqueue(t, 2)
	turn(t, l, rows)
	train, _ := rows.snapshot()
	if len(train) != 1 {
		t.Fatalf("the premise: two changes at depth 1 and batch 2 built %d candidates, want 1", len(train))
	}
	rows.ended(train[0].TipOID, "unit", gitlet.CheckStatePassed)
	c.advance(10 * time.Minute)
	turn(t, l, rows)
	if got := counted(rec, metrics.CodeQueueLanded, nil); got != 2 {
		t.Fatalf("the green train counted %d changes landed, want 2", got)
	}
	if n, sum := observed(rec, metrics.CodeQueueRoundDuration, nil); n != 1 ||
		sum != float64(10*time.Minute/time.Millisecond) {
		t.Fatalf("the landed round was timed %d times at %v ms in all, want once at ten minutes", n, sum)
	}
	if got := rounds("landed"); got != 1 {
		t.Fatalf("the green train counted %d rounds landed, want 1", got)
	}

	entries := rows.enqueue(t, 2)
	turn(t, l, rows)
	train, _ = rows.snapshot()
	if len(train) != 1 || len(train[0].Commits) != 2 {
		t.Fatalf("the premise: the second train is %+v, want one candidate of two", train)
	}
	commits := train[0].Commits
	rows.ended(commits[1].Commit, "unit", gitlet.CheckStateFailed)
	turn(t, l, rows)
	rows.ended(commits[0].Commit, "unit", gitlet.CheckStateFailed)
	turn(t, l, rows)
	if got := counted(rec, metrics.CodeQueueEjected,
		metrics.Attrs{"reason": string(gitlet.EjectCheckFailed)}); got != 1 {
		t.Fatalf("the bisect counted %d ejections check_failed, want its culprit's 1", got)
	}
	if got := rounds("bisected"); got != 1 {
		t.Fatalf("the red train counted %d rounds bisected, want 1", got)
	}
	if _, live := rows.snapshot(); len(live) != 1 || live[0].EntryID != entries[1].EntryID {
		t.Fatalf("the premise: %+v are live after the bisect, want the second entry alone", live)
	}

	turn(t, l, rows)
	rows.mu.Lock()
	rows.tip = oid("d")
	rows.mu.Unlock()
	turn(t, l, rows)
	turn(t, l, rows)
	if got := rounds("abandoned"); got != 1 {
		t.Fatalf("a train given up under a person's push counted %d rounds abandoned, want 1", got)
	}
	if got := rounds("landed") + rounds("bisected") + rounds("ejected"); got != 2 {
		t.Fatalf("the other rounds count %d after the push, want the 2 before it", got)
	}

	rows.mu.Lock()
	rows.pending = true
	rows.mu.Unlock()
	rows.enqueue(t, 1)
	train, _ = rows.snapshot()
	if len(train) != 1 || len(train[0].Commits) != 1 || train[0].Commits[0].EntryID != entries[1].EntryID {
		t.Fatalf("the premise: the train on the pushed tip is %+v, want the second entry's alone", train)
	}
	rows.ended(train[0].TipOID, "unit", gitlet.CheckStatePassed)
	turn(t, l, rows)
	turn(t, l, rows)
	if got := counted(rec, metrics.CodeQueueLanded, nil); got != 3 {
		t.Fatalf("a landing answered pending counted %d changes landed in all, want its 1 beside the 2 before",
			got)
	}
	if got := rounds("landed"); got != 2 {
		t.Fatalf("a landing answered pending counted %d rounds landed in all, want 2", got)
	}
	if got := rounds("abandoned"); got != 1 {
		t.Fatalf("a train landed by a landing answered pending counted %d rounds abandoned in all, want the "+
			"push's 1", got)
	}
}

// A TRAIN ENDED BY AN EJECTION IS COUNTED EJECTED, AND ONCE (M5 §2.10): the
// two ends the queue worker appends an eject for that are neither a landing
// nor a bisect each count one round `ejected` and nothing else, and the turn
// after does not count the same train `abandoned` again —
//
//   - a green train whose landing the rows refuse, its first entry
//     gate_unmet: that entry ejected, the train ended there, and the next
//     train, of the entry left, built over no abandoned round;
//   - a train whose base's checks file defines none of its required checks:
//     every entry ejected check_failed, and the next train, of a change
//     queued after, built over no abandoned round.
//
// Mutation: spell the outcome as another's (roundEjected = "bisected") → no
// round ejected, red; end neither train (nil in place of endOf at the land's
// refusal, or at trainDefs) → no round ejected, and the next train reads the
// old one abandoned, red.
func TestATrainEndedByAnEjectionCountsItsRoundEjected(t *testing.T) {
	t.Parallel()
	loop := func(rows *queueRows) (*home.Loop, func(string) uint64) {
		t.Helper()
		c := &clock{now: time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)}
		rec, err := metrics.New()
		if err != nil {
			t.Fatal(err)
		}
		l, err := home.New(home.Options{Chart: noChart,
			Estate: rows, Placement: asNode{placement: &placement{}, node: "git-a"},
			Nudges: newNudges(), Cache: unmerged{t: t}, Git: unmerged{t: t}, Chains: &queueChains{},
			Tasks: &queueTasks{}, NodeID: "git-a", Share: units(2), Now: c.Now, Metrics: rec})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return l, func(outcome string) uint64 {
			return counted(rec, metrics.CodeQueueRounds, metrics.Attrs{"outcome": outcome})
		}
	}
	only := func(step string, rounds func(string) uint64) {
		t.Helper()
		for outcome, want := range map[string]uint64{"ejected": 1, "landed": 0, "bisected": 0, "abandoned": 0} {
			if got := rounds(outcome); got != want {
				t.Errorf("%s: %d rounds %s, want %d", step, got, outcome, want)
			}
		}
	}

	// A LANDING REFUSED: depth 2 at batch 1 is a train of two candidates,
	// both green; the rows refuse the first entry's landing gate_unmet.
	refused := newQueueRows(oid("a"), 2, 1)
	entries := refused.enqueue(t, 2)
	l, rounds := loop(refused)
	turn(t, l, refused)
	train, _ := refused.snapshot()
	if len(train) != 2 {
		t.Fatalf("the premise: two changes at depth 2 and batch 1 built %d candidates, want 2", len(train))
	}
	for _, cand := range train {
		refused.ended(cand.TipOID, "unit", gitlet.CheckStatePassed)
	}
	refused.mu.Lock()
	refused.refuse = func(gitlet.Land) *gitlet.QueueResult {
		return &gitlet.QueueResult{Refusal: gitlet.MergeRefusalGateUnmet, Detail: "would not land",
			Refused: []gitlet.RefusedEntry{{EntryID: entries[0].EntryID, Refusal: gitlet.MergeRefusalGateUnmet}}}
	}
	refused.mu.Unlock()
	turn(t, l, refused)
	if _, live := refused.snapshot(); len(live) != 1 || live[0].EntryID != entries[1].EntryID {
		t.Fatalf("the premise: %+v are live after the refused landing, want the second entry alone", live)
	}
	refused.mu.Lock()
	refused.refuse = nil
	refused.mu.Unlock()
	turn(t, l, refused)
	if train, _ = refused.snapshot(); len(train) != 1 || train[0].Commits[0].EntryID != entries[1].EntryID {
		t.Fatalf("the premise: the train after the ejection is %+v, want the second entry's alone", train)
	}
	only("a landing refused gate_unmet", rounds)

	// A REQUIRED CHECK THE BASE DEFINES NOT: unit is required and the train
	// base's checks file defines lint alone.
	undefined := newQueueRows(oid("f"), 2, 1)
	undefined.file = checksFile("lint")
	undefined.enqueue(t, 2)
	l, rounds = loop(undefined)
	turn(t, l, undefined)
	turn(t, l, undefined)
	if _, live := undefined.snapshot(); len(live) != 0 {
		t.Fatalf("the premise: %+v are live after a train whose base defines no unit, want both ejected", live)
	}
	undefined.enqueue(t, 1)
	turn(t, l, undefined)
	if train, _ := undefined.snapshot(); len(train) != 1 {
		t.Fatalf("the premise: the change queued after the ejection built %d candidates, want 1", len(train))
	}
	only("a required check the train base defines not", rounds)
}
