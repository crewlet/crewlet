package statelog_test

import (
	"database/sql"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// readerOn is a reader of d's own log over db, healthy at a position on that
// log, waiting through waiter: one half of a read that spans two logs, each
// half a reader of its own.
func readerOn(t *testing.T, db *stubStore, d statelog.Domain, waiter statelog.Waiter) *statelog.Reader {
	t.Helper()
	stream := d.Stream().Name
	health := func() statelog.Health {
		h := healthy()
		h.Position.Stream = stream
		return h
	}
	r, err := statelog.NewReader(statelog.ReaderDeps{
		Domain: d, Spec: specOf(d),
		Mode:   statelog.ModeNormal,
		DB:     db,
		Waiter: waiter,
		Health: readable(health),
		Drain:  func() float64 { return 2000 },
	})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	return r
}

// A READ THAT SPANS TWO LOGS WAITS ON THE CALLER'S FLOOR IN EACH, AND ON NO
// OTHER LOG'S.
//
// One answer read from two logs — the company feed's work and pages — was
// held up by one floor, on one of them: a caller that wrote both logs could
// not read its writes back, because the half the floor did not name was
// served from before the write. Each half takes its own log's floor
// ([Freshness.On]) and waits for exactly that: a half handed the whole ask
// would refuse the other log's floor rather than wait on it.
//
// AND A HALF WHOSE LOG THE CALLER NAMED NO FLOOR ON IS READ `stale`: a
// `session` ask there would wait for the zero position and serve this node's
// prefix labelled `session`, the wrong label every grammar refuses a bare
// `session` over. Each half's answer says the level it was actually read at.
//
// Mutation: hand each half the whole ask instead of [Freshness.On]'s, or keep
// every floor in On, and both halves refuse; keep `session` on a half with no
// floor, and the second case reads it under the stronger name.
func TestAComposedReadWaitsOnEachLogsFloor(t *testing.T) {
	t.Parallel()
	first := statelog.Position{Stream: probeStream, Generation: 1, Seq: 1 << 30}
	second := statelog.Position{Stream: secondProbeStream, Generation: 1, Seq: 1 << 29}
	scope := statelog.ScopeSet{Paths: []string{"object/a"}}
	db := newStubStore(t)

	for _, level := range []statelog.ReadLevel{
		statelog.ReadSession, statelog.ReadStale, statelog.ReadConsistentPrefix,
	} {
		ask := statelog.Freshness{Level: level, Floors: []statelog.Position{first, second}}
		for _, half := range []struct {
			domain statelog.Domain
			floor  statelog.Position
		}{{probeDomain{}, first}, {secondProbeDomain{}, second}} {
			w := &stubWaiter{at: healthy().Position}
			r := readerOn(t, db, half.domain, w)
			answer, err := r.Read(t.Context(), ask.On(half.floor.Stream).Query(scope, true),
				func(*sql.Tx) error { return nil })
			if err != nil {
				t.Fatalf("a %s read of %s with a floor on each log: %v",
					level, half.floor.Stream, err)
			}
			if got := w.last.Load(); w.waited.Load() != 1 || got != half.floor.Packed() {
				t.Errorf("the %s half of a %s read waited %d time(s), last for packed %d — "+
					"want once, for its own log's floor %s", half.floor.Stream, level,
					w.waited.Load(), got, half.floor)
			}
			if answer.Level != level {
				t.Errorf("the %s half of a %s read with its own floor answered at %s",
					half.floor.Stream, level, answer.Level)
			}
		}
	}

	// A FLOOR ON ONE LOG ONLY: the other half has nothing of the caller's to
	// wait for, and is read at the dashboard's own level rather than labelled
	// `session` over a wait for nothing.
	ask := statelog.Freshness{Level: statelog.ReadSession, Floors: []statelog.Position{first}}
	w := &stubWaiter{at: healthy().Position}
	answer, err := readerOn(t, db, secondProbeDomain{}, w).Read(t.Context(),
		ask.On(secondProbeStream).Query(scope, true), func(*sql.Tx) error { return nil })
	if err != nil {
		t.Fatalf("the half of a session read whose log carries no floor: %v", err)
	}
	if answer.Level != statelog.ReadStale {
		t.Errorf("the half whose log the caller named no floor on answered at %s, want %s — "+
			"a session read with nothing to wait for is a stale read under a stronger name",
			answer.Level, statelog.ReadStale)
	}
	if got := w.waited.Load(); got != 0 {
		t.Errorf("the half with no floor of its own waited %d time(s), want none", got)
	}

	// THE CONTROL: the whole ask handed to one half is refused, naming the
	// other log, rather than waited on or dropped — which is why a composer
	// splits it.
	_, err = readerOn(t, db, probeDomain{}, &stubWaiter{at: healthy().Position}).Read(t.Context(),
		statelog.Freshness{Level: statelog.ReadSession,
			Floors: []statelog.Position{first, second}}.Query(scope, true),
		func(*sql.Tx) error { return nil })
	if !errors.Is(err, statelog.ErrForeignPosition) || !strings.Contains(err.Error(), secondProbeStream) {
		t.Errorf("one half handed both logs' floors answered %v, want %v naming %s",
			err, statelog.ErrForeignPosition, secondProbeStream)
	}
}

// A HALF OF A COMPOSED READ IS HANDED THE WHOLE ASK, ITS FLOORS NARROWED TO ITS
// OWN LOG AND NOTHING ELSE CHANGED.
//
// The half is read on its own, so a part of the ask [statelog.Freshness.On]
// leaves out is one nothing after it can put back: `max_lag_seq=250` on the
// company feed would read both of its halves unbounded and still answer
// `stale`. On built the half field by field, which carried a part only for
// as long as its literal named it, and every case of a composed read asked
// with no bound, so dropping both bounds passed them all. The half is
// compared WHOLE here, against an ask that sets every part, so a part added
// to the ask is held from the day it lands: the guard below fails until the
// ask sets it, and the comparison then fails if a half drops it.
//
// AND THE ASK ITSELF IS LEFT AS IT WAS: each half is cut from the same ask,
// so a half narrowing the ask's own slice in place would hand the next half
// floors the first one cut.
//
// Mutation: build the half as `Freshness{Level: f.Level}`, or leave out any
// one part, and the comparison goes red; narrow `out.Floors` in place, and
// the ask's floors do.
func TestAHalfOfAComposedReadIsHandedTheWholeAsk(t *testing.T) {
	t.Parallel()
	first := statelog.Position{Stream: probeStream, Generation: 1, Seq: 1 << 30}
	second := statelog.Position{Stream: secondProbeStream, Generation: 1, Seq: 1 << 29}
	ask := statelog.Freshness{
		Level: statelog.ReadStale, MaxLag: 30 * time.Second, MaxLagSeq: 250,
		Floors: []statelog.Position{first, second},
	}
	asked := slices.Clone(ask.Floors)
	whole := reflect.ValueOf(ask)
	for i := range whole.NumField() {
		if whole.Field(i).IsZero() {
			t.Fatalf("the ask leaves %s unset: set it, or a half that drops it passes",
				whole.Type().Field(i).Name)
		}
	}
	for _, half := range []struct {
		stream string
		floors []statelog.Position
	}{
		{probeStream, []statelog.Position{first}},
		{secondProbeStream, []statelog.Position{second}},
		// A LOG THE CALLER NAMED NO FLOOR ON: the half has none, and keeps
		// the rest of the ask, its bound included.
		{"CREWLET_THIRD_PROBE_LOG", nil},
	} {
		want := ask
		want.Floors = half.floors
		if got := ask.On(half.stream); !reflect.DeepEqual(got, want) {
			t.Errorf("the %s half of %+v is %+v, want %+v: the whole ask, its floors on "+
				"that log alone", half.stream, ask, got, want)
		}
	}
	if !slices.Equal(ask.Floors, asked) {
		t.Errorf("cutting the halves left the ask's floors %v, want them as asked, %v",
			ask.Floors, asked)
	}
}

// A READ OF ONE LOG REFUSES A FLOOR ON ANOTHER BESIDE ITS OWN, AT EVERY LEVEL,
// NAMING THE OTHER LOG — AND WAITS FOR NOTHING FIRST.
//
// With one floor per log a caller can name several, and a reader that waited
// on its own and dropped the rest would answer a caller who believes every
// floor was honoured: the other log's half of their write simply missing from
// an answer that never reads it. Refused instead, as the request's mistake
// ([statelog.ErrForeignPosition]) that every node answers the same, naming the
// log the floor is on so the caller can see which floor it misplaced. The
// rule is [statelog.Freshness.OnlyOn]'s, which a reader of one log outside
// this framework holds a caller's floors to as well.
//
// Mutation: hold only the floor on the read's own log to it (the first one,
// or the one it waits for) and the second floor is dropped: red.
func TestASingleLogReadRefusesAnotherLogsFloor(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	index, err := statelog.NewReadIndex(probeDomain{}, specOf(probeDomain{}), h.log,
		h.log.Members, testSigner(t, probeDomain{}), noCeiling(t), probeEncode, h.gen.Load, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}
	own := statelog.Position{Stream: probeStream, Generation: 1, Seq: 5}
	other := statelog.Position{Stream: secondProbeStream, Generation: 1, Seq: 9}
	db := newStubStore(t)
	for _, floors := range [][]statelog.Position{{own, other}, {other, own}} {
		for _, level := range statelog.ReadLevels {
			w := &stubWaiter{at: healthy().Position}
			r := newReader(t, db, healthy, w, index)
			q := pointQuery(level)
			q.Floors = floors
			_, err := r.Read(t.Context(), q, func(*sql.Tx) error { return nil })
			if !errors.Is(err, statelog.ErrForeignPosition) {
				t.Errorf("a %s read floored at %v answered %v, want %v", level, floors,
					err, statelog.ErrForeignPosition)
				continue
			}
			if !strings.Contains(err.Error(), secondProbeStream) {
				t.Errorf("a %s read's refusal of another log's floor does not name the "+
					"log it is on: %v", level, err)
			}
			if got := w.waited.Load(); got != 0 {
				t.Errorf("a %s read floored at %v waited %d time(s) before refusing", level,
					floors, got)
			}
		}
	}
	// THE RULE ITSELF, for a reader of one log outside the framework, and its
	// control: a floor on the log asked of, and one that names nothing, pass.
	fresh := statelog.Freshness{Floors: []statelog.Position{own, other}}
	if err := fresh.OnlyOn(probeStream); !errors.Is(err, statelog.ErrForeignPosition) {
		t.Errorf("OnlyOn(%s) over a floor on %s = %v, want %v", probeStream, secondProbeStream,
			err, statelog.ErrForeignPosition)
	}
	fresh.Floors = []statelog.Position{own, {}}
	if err := fresh.OnlyOn(probeStream); err != nil {
		t.Errorf("OnlyOn(%s) over its own floor and a zero one = %v, want nil", probeStream, err)
	}
	// AND THE CONTROL: a floor on this read's own log alone is waited for.
	w := &stubWaiter{at: healthy().Position}
	q := pointQuery(statelog.ReadStale)
	q.Floors = []statelog.Position{own}
	if _, err := newReader(t, db, healthy, w, index).Read(t.Context(), q,
		func(*sql.Tx) error { return nil }); err != nil {
		t.Fatalf("a stale read floored on its own log: %v", err)
	}
	if got := w.last.Load(); got != own.Packed() {
		t.Errorf("a stale read floored on its own log waited for packed %d, want %d", got,
			own.Packed())
	}
}

// AN ANSWER COMPOSED OF HALVES IS AS FRESH AS ITS STALEST HALF.
//
// A half of a `session` read whose log carries no floor of the caller's is
// read `stale`, so the composed answer's level is the weakest of its halves'
// in the order the levels are listed: labelled by its other half it would
// claim for these rows a level they were never read at. A half that read
// nothing (an empty level) says nothing. Mutation: answer the first level, or
// the strongest, and the mixed cases go red.
func TestAComposedAnswerIsAsFreshAsItsStalestHalf(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		levels []statelog.ReadLevel
		want   statelog.ReadLevel
	}{
		{nil, ""},
		{[]statelog.ReadLevel{"", ""}, ""},
		{[]statelog.ReadLevel{statelog.ReadSession, statelog.ReadSession}, statelog.ReadSession},
		{[]statelog.ReadLevel{statelog.ReadSession, statelog.ReadStale}, statelog.ReadStale},
		{[]statelog.ReadLevel{statelog.ReadStale, statelog.ReadSession}, statelog.ReadStale},
		{[]statelog.ReadLevel{"", statelog.ReadLinearizable}, statelog.ReadLinearizable},
		{[]statelog.ReadLevel{statelog.ReadConsistentPrefix, statelog.ReadLinearizable},
			statelog.ReadConsistentPrefix},
	} {
		if got := statelog.Weakest(c.levels...); got != c.want {
			t.Errorf("Weakest(%v) = %q, want %q", c.levels, got, c.want)
		}
	}
}

// ONE READ NAMES ONE FLOOR PER LOG, AND AT MOST FOUR.
//
// Two floors on one log are two answers to how far the caller has written
// there, and the read waits on one: the grammar refuses the pair, naming the
// log, rather than choose. And a list longer than [statelog.MaxFloors] is a
// client sending every floor it holds rather than the ones this answer reads.
// Mutation: drop either check and the case naming it goes red.
func TestOneReadNamesOneFloorPerLogAndAtMostFour(t *testing.T) {
	t.Parallel()
	at := func(stream string, seq uint64) statelog.Position {
		return statelog.Position{Stream: stream, Generation: 1, Seq: seq}
	}
	for _, c := range []struct {
		name   string
		floors []statelog.Position
		want   string
	}{
		{"none", nil, ""},
		{"one per log", []statelog.Position{at("A", 1), at("B", 2), at("C", 3), at("D", 4)}, ""},
		{"two on one log", []statelog.Position{at("A", 1), at("B", 2), at("A", 7)}, "two floors on A"},
		{"more than four", []statelog.Position{at("A", 1), at("B", 2), at("C", 3), at("D", 4),
			at("E", 5)}, "at most 4"},
		{"no stream", []statelog.Position{{Generation: 1, Seq: 1}}, "no stream"},
	} {
		err := statelog.ValidFloors(c.floors)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: ValidFloors = %v, want nil", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: ValidFloors = %v, want a refusal saying %q", c.name, err, c.want)
		}
	}
}
