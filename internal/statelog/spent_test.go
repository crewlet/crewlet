package statelog_test

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// The positions these cases name: a barrier's on the probe log, the probe
// log's checkpoint, and one on another log.
var (
	barrierAt = statelog.Position{Stream: probeStream, Generation: 1, Seq: 7}
	elsewhere = statelog.Position{Stream: "CREWLET_OTHER_LOG", Generation: 1, Seq: 900}
)

// countingIndex is a read index over a broker that commits every barrier at
// once at barrierAt's sequence, counting what it was asked to append.
func countingIndex(t *testing.T, appends *atomic.Int64, members func() int) *statelog.ReadIndex {
	t.Helper()
	index, err := statelog.NewReadIndex(probeDomain{}, specOf(probeDomain{}),
		&countingAppends{inner: committingLog{seq: barrierAt.Seq}, n: appends},
		members, testSigner(t, probeDomain{}), noCeiling(t),
		probeEncode, func() uint32 { return barrierAt.Generation }, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}
	return index
}

// committingLog is a broker that commits every barrier at once at seq.
type committingLog struct{ seq uint64 }

func (l committingLog) Append(context.Context, string, string, *uint64, []byte) (uint64, bool, error) {
	return l.seq, false, nil
}

func (committingLog) LastSeq(context.Context, string) (uint64, bool, error) { return 0, false, nil }

// A LINEARIZABLE READ HANDED A READ INDEX WAITS FOR IT AND APPENDS NOTHING.
// The index is one a node asked for the same read before this one established;
// a barrier that started after the request reached that node and committed is
// a bound on every write acknowledged before the request began, whichever
// node's applier the read is then served from. What the read waits for is
// the record BEFORE the index, as it is before its own barrier — the barrier
// writes no row on any node — so the position the waiter is handed is that
// record's, spelled out here rather than derived, so a reader that waited for
// the barrier itself again is seen. The control appends: without it a reader
// that never appended anything would pass the case.
func TestAReadHandedAReadIndexWaitsForItAndAppendsNothing(t *testing.T) {
	t.Parallel()
	var appends atomic.Int64
	db := newStubStore(t)
	waiter := &stubWaiter{at: healthy().Position}
	r := newReader(t, db, healthy, waiter, countingIndex(t, &appends, members(3)))
	read := func(ctx context.Context) (statelog.Answer, error) {
		return r.Read(ctx, pointQuery(statelog.ReadLinearizable), func(*sql.Tx) error { return nil })
	}

	if _, err := read(t.Context()); err != nil {
		t.Fatalf("the control: a linearizable read = %v", err)
	}
	if got := appends.Load(); got != 1 {
		t.Fatalf("the control: a linearizable read appended %d barriers, want one", got)
	}

	held := statelog.Position{Stream: probeStream, Generation: 1, Seq: 42}
	ctx := statelog.WithSpent(t.Context(), statelog.Spent{Index: []statelog.Position{held, elsewhere}})
	if _, err := read(ctx); err != nil {
		t.Fatalf("a read handed a read index = %v, want it served", err)
	}
	if got := appends.Load(); got != 1 {
		t.Errorf("a read handed a read index appended %d more barrier(s)", got-1)
	}
	if want := held.At(held.Seq - 1); waiter.last.Load() != want.Packed() {
		t.Errorf("the read waited for %d, want the record before the index it was handed (%d)",
			waiter.last.Load(), want.Packed())
	}

	// NOT AN INDEX FROM A GENERATION THIS NODE'S LOG HAS PASSED: its
	// sequence is a number in another space.
	passed := statelog.Position{Stream: probeStream, Generation: 0, Seq: 1_000_000}
	ctx = statelog.WithSpent(t.Context(), statelog.Spent{Index: []statelog.Position{passed}})
	if _, err := read(ctx); err != nil {
		t.Fatalf("a read handed an index of a passed generation = %v", err)
	}
	if got := appends.Load(); got != 2 {
		t.Errorf("a read handed an index of a generation its log has passed appended %d "+
			"barrier(s) of its own, want one", got-1)
	}
	if want := barrierAt.At(barrierAt.Seq - 1); waiter.last.Load() != want.Packed() {
		t.Errorf("the read waited for %d, want the record before its own barrier (%d)",
			waiter.last.Load(), want.Packed())
	}
}

// A REFUSED READ SAYS WHAT IT PAID FOR, AND A NODE ASKED NEXT PAYS FOR NONE OF
// IT. The first node appends its barrier and waits its whole bound for the
// record before it: refused `behind`, carrying the read index (the barrier)
// and the position it waited out (that record).
// Handed both, the next node appends nothing and waits for nothing — refused
// `behind` at once, its applier being no further on. Asked afresh, it
// appended a barrier of its own and waited another whole read budget.
func TestARefusedReadSaysWhatItPaidForAndTheNextNodePaysForNone(t *testing.T) {
	t.Parallel()
	var appends atomic.Int64
	r := newReader(t, newStubStore(t), healthy, unreachedWaiter{at: healthy().Position},
		countingIndex(t, &appends, members(3)))
	read := func(ctx context.Context) error {
		_, err := r.Read(ctx, pointQuery(statelog.ReadLinearizable), func(*sql.Tx) error { return nil })
		return err
	}

	begun := time.Now()
	err := read(t.Context())
	var first *statelog.Refused
	if !errors.As(err, &first) || first.Code != statelog.RefuseBehind {
		t.Fatalf("a read that never reaches its barrier = %v, want refused %q", err, statelog.RefuseBehind)
	}
	if waited := time.Since(begun); waited < statelog.ReadBudget {
		t.Fatalf("the first refusal came after %v, before the read budget (%v) it waits",
			waited, statelog.ReadBudget)
	}
	if waited := barrierAt.At(barrierAt.Seq - 1); !slices.Equal(first.Spent.Index, []statelog.Position{barrierAt}) ||
		!slices.Equal(first.Spent.Waited, []statelog.Position{waited}) {
		t.Fatalf("the refusal says it paid for %+v, want the read index %v and the position it "+
			"waited out, the record before it, %v", first.Spent, barrierAt, waited)
	}

	begun = time.Now()
	err = read(statelog.WithSpent(t.Context(), first.Spent))
	var next *statelog.Refused
	if !errors.As(err, &next) || next.Code != statelog.RefuseBehind {
		t.Fatalf("the next node = %v, want refused %q", err, statelog.RefuseBehind)
	}
	if waited := time.Since(begun); waited >= statelog.ReadBudget/4 {
		t.Errorf("the next node took %v to refuse a position the read already waited out", waited)
	}
	if got := appends.Load(); got != 1 {
		t.Errorf("two nodes appended %d barriers for one read, want one", got)
	}
	if !strings.Contains(next.Detail, "already waited a whole bound") {
		t.Errorf("the next refusal reads %q, which does not say the wait was spent elsewhere", next.Detail)
	}
	if !slices.Equal(next.Spent.Index, first.Spent.Index) || !slices.Equal(next.Spent.Waited, first.Spent.Waited) {
		t.Errorf("the next refusal carries %+v, want what the walk had paid for (%+v)", next.Spent, first.Spent)
	}
}

// A SESSION READ'S POSITION IS WAITED OUT ONCE TOO, and a position past the
// one waited out is still waited for: what was paid for is a position, and a
// later one is a wait nobody made.
func TestAPositionWaitedOutIsNotWaitedForAgainAndALaterOneIs(t *testing.T) {
	t.Parallel()
	r := newReader(t, newStubStore(t), healthy, unreachedWaiter{at: healthy().Position}, nil)
	q := pointQuery(statelog.ReadSession)
	q.Session = statelog.Position{Stream: probeStream, Generation: 1, Seq: 9_000}
	read := func(spent statelog.Spent) (time.Duration, error) {
		begun := time.Now()
		_, err := r.Read(statelog.WithSpent(t.Context(), spent), q, func(*sql.Tx) error { return nil })
		return time.Since(begun), err
	}

	for name, tc := range map[string]struct {
		waited statelog.Position
		waits  bool
	}{
		"the position itself": {waited: q.Session},
		"one past it":         {waited: statelog.Position{Stream: probeStream, Generation: 1, Seq: 9_001}},
		"one before it":       {waited: statelog.Position{Stream: probeStream, Generation: 1, Seq: 8_999}, waits: true},
		"another log's":       {waited: statelog.Position{Stream: elsewhere.Stream, Generation: 1, Seq: 9_001}, waits: true},
	} {
		took, err := read(statelog.Spent{Waited: []statelog.Position{tc.waited}})
		var refusal *statelog.Refused
		if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseBehind {
			t.Fatalf("%s: = %v, want refused %q", name, err, statelog.RefuseBehind)
		}
		if waited := took >= statelog.ReadBudget; waited != tc.waits {
			t.Errorf("%s waited out: the read took %v, and waiting its %v read budget is %v, want %v",
				name, took, statelog.ReadBudget, waited, tc.waits)
		}
	}
}

// A SINGLE MEMBER'S BARRIER THAT RUNS OUT OF ITS BUDGET IS ITS OWN WRITE
// STALLED, NEVER A MISSED QUORUM.
//
// One member stores the log — the count read off the harness's own broker, as
// the engine reads its stream's — and the member holds the barrier's write
// past the barrier's whole budget, as a disk holding the fsync `stream.sync:
// always` makes of every append holds it. No majority was asked and no
// election is coming, and refused `no_quorum` with an election's hint, a
// single node on a stalling disk sent its operator looking for peers it does
// not have. It is refused `broker_stalled`: one budget's hint, the stall named,
// and no node asked next appends a barrier behind it.
func TestASingleMembersStalledBarrierIsNeverAMissedQuorum(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if got := h.log.Members(); got != 1 {
		t.Fatalf("the premise: the harness's log is stored by %d members, want one", got)
	}
	read := func(ctx context.Context, r *statelog.Reader) error {
		_, err := r.Read(ctx, pointQuery(statelog.ReadLinearizable), func(*sql.Tx) error { return nil })
		return err
	}
	stalled := func(t *testing.T, what string, err error) *statelog.Refused {
		t.Helper()
		var refusal *statelog.Refused
		if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseBrokerStalled {
			t.Fatalf("%s = %v, want refused %q", what, err, statelog.RefuseBrokerStalled)
		}
		if refusal.RetryAfter != statelog.StalledRetryHint {
			t.Errorf("%s says come back in %v, want one barrier budget (%v) — there is "+
				"no election to wait out", what, refusal.RetryAfter, statelog.StalledRetryHint)
		}
		if !slices.Equal(refusal.Spent.Unconfirmed, []string{probeStream}) {
			t.Errorf("%s says it paid for %+v, want the log its barrier ran out on", what, refusal.Spent)
		}
		for _, wrong := range []string{"quorum", "majority"} {
			if strings.Contains(refusal.Detail, wrong) {
				t.Errorf("%s reads %q, which speaks of a %s one member does not have",
					what, refusal.Detail, wrong)
			}
		}
		if !strings.Contains(refusal.Detail, "one member") {
			t.Errorf("%s reads %q, which does not say the log's one member is what stalled",
				what, refusal.Detail)
		}
		return refusal
	}

	t.Run("the barrier's own budget ran out", func(t *testing.T) {
		t.Parallel()
		r := newReader(t, newStubStore(t), healthy, &stubWaiter{at: healthy().Position},
			heldIndex(t, newHeldAppender(t), h.log.Members))
		began := time.Now()
		stalled(t, "a barrier the member held past its budget", read(t.Context(), r))
		if took := time.Since(began); took < statelog.DefaultBarrierBudget {
			t.Errorf("refused after %v, before the barrier's own %v budget was spent",
				took, statelog.DefaultBarrierBudget)
		}
	})
	t.Run("the asker's attempt ran out", func(t *testing.T) {
		t.Parallel()
		r := newReader(t, newStubStore(t), healthy, &stubWaiter{at: healthy().Position},
			heldIndex(t, newHeldAppender(t), h.log.Members))
		ctx, deadline := asked(t)
		refusal := stalled(t, "a barrier its asker's attempt ended", read(ctx, r))
		heard(t, "a read whose barrier the member held", deadline)
		if !strings.Contains(refusal.Detail, "all its asker's attempt left") {
			t.Errorf("the refusal reads %q, which does not say the asker's time is what ran out",
				refusal.Detail)
		}
	})
	t.Run("the next node", func(t *testing.T) {
		t.Parallel()
		var appends atomic.Int64
		r := newReader(t, newStubStore(t), healthy, &stubWaiter{at: healthy().Position},
			countingIndex(t, &appends, h.log.Members))
		begun := time.Now()
		refusal := stalled(t, "a node asked after the member's write stalled",
			read(statelog.WithSpent(t.Context(), statelog.Spent{Unconfirmed: []string{probeStream}}), r))
		if took := time.Since(begun); took >= statelog.ReadBudget/4 {
			t.Errorf("it took %v to refuse", took)
		}
		if got := appends.Load(); got != 0 {
			t.Errorf("it appended %d barrier(s) behind a member whose write is stalled", got)
		}
		if !strings.Contains(refusal.Detail, "appends no second barrier") {
			t.Errorf("the refusal reads %q, which does not say why nothing was appended", refusal.Detail)
		}
	})
}

// A BARRIER THAT RUNS ITS WHOLE BUDGET IS A LOG WITHOUT A QUORUM, where several
// members store it, and no node asked after it appends another: every node's
// barrier goes to the one log's leader. The first node's refusal says so —
// whether its asker's attempt or the barrier's own budget is what ran out —
// and the next node is refused `no_quorum` at once, appending nothing. A
// barrier refused at once by a full log spent nothing of the kind, and the
// next node appends its own.
func TestNoNodeAppendsASecondBarrierToALogWithoutAQuorum(t *testing.T) {
	t.Parallel()
	read := func(ctx context.Context, r *statelog.Reader) error {
		_, err := r.Read(ctx, pointQuery(statelog.ReadLinearizable), func(*sql.Tx) error { return nil })
		return err
	}
	noQuorum := func(t *testing.T, what string, err error) *statelog.Refused {
		t.Helper()
		var refusal *statelog.Refused
		if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseNoQuorum {
			t.Fatalf("%s = %v, want refused %q", what, err, statelog.RefuseNoQuorum)
		}
		if !slices.Equal(refusal.Spent.Unconfirmed, []string{probeStream}) {
			t.Errorf("%s says it paid for %+v, want the log its barrier ran out on", what, refusal.Spent)
		}
		return refusal
	}

	t.Run("the asker's attempt ran out", func(t *testing.T) {
		t.Parallel()
		r := newReader(t, newStubStore(t), healthy, &stubWaiter{at: healthy().Position},
			heldIndex(t, newHeldAppender(t), members(3)))
		ctx, _ := asked(t)
		noQuorum(t, "a barrier its asker's attempt ended", read(ctx, r))
	})
	t.Run("the barrier's own budget ran out", func(t *testing.T) {
		t.Parallel()
		r := newReader(t, newStubStore(t), healthy, &stubWaiter{at: healthy().Position},
			heldIndex(t, newHeldAppender(t), members(3)))
		noQuorum(t, "a barrier that ran its own budget", read(t.Context(), r))
	})
	t.Run("the next node", func(t *testing.T) {
		t.Parallel()
		var appends atomic.Int64
		r := newReader(t, newStubStore(t), healthy, &stubWaiter{at: healthy().Position},
			countingIndex(t, &appends, members(3)))
		begun := time.Now()
		refusal := noQuorum(t, "a node asked after a barrier ran out",
			read(statelog.WithSpent(t.Context(), statelog.Spent{Unconfirmed: []string{probeStream}}), r))
		if took := time.Since(begun); took >= statelog.ReadBudget/4 {
			t.Errorf("it took %v to refuse", took)
		}
		if got := appends.Load(); got != 0 {
			t.Errorf("it appended %d barrier(s) to a log a node already found without a quorum", got)
		}
		if !strings.Contains(refusal.Detail, "appends no second barrier") {
			t.Errorf("the refusal reads %q, which does not say why nothing was appended", refusal.Detail)
		}
		// ANOTHER LOG'S RUN-OUT barrier says nothing about this one.
		if err := read(statelog.WithSpent(t.Context(), statelog.Spent{Unconfirmed: []string{elsewhere.Stream}}), r); err != nil {
			t.Fatalf("a read after another log's barrier ran out = %v, want it served", err)
		}
		if got := appends.Load(); got != 1 {
			t.Errorf("a read after another log's barrier ran out appended %d barriers, want one", got)
		}
	})
	t.Run("a full log spends nothing", func(t *testing.T) {
		t.Parallel()
		index, err := statelog.NewReadIndex(probeDomain{}, specOf(probeDomain{}), fullLog(),
			members(1), testSigner(t, probeDomain{}), noCeiling(t), probeEncode, func() uint32 { return 1 }, nil)
		if err != nil {
			t.Fatalf("NewReadIndex: %v", err)
		}
		r := newReader(t, newStubStore(t), healthy, &stubWaiter{at: healthy().Position}, index)
		var refusal *statelog.Refused
		if err := read(t.Context(), r); !errors.As(err, &refusal) || refusal.Code != statelog.RefuseLogFull {
			t.Fatalf("a read on a full log = %v, want refused %q", err, statelog.RefuseLogFull)
		}
		if !refusal.Spent.IsZero() {
			t.Errorf("a barrier refused at once says it paid for %+v", refusal.Spent)
		}
	})
}

// WHAT A WALK HAS PAID FOR IS KEPT PER LOG: the first read index on each, the
// furthest position waited out on each, and every log a barrier ran out on —
// so what was paid on one log is never read as paid on another.
func TestWhatAWalkPaidForIsKeptPerLog(t *testing.T) {
	t.Parallel()
	at := func(stream string, seq uint64) statelog.Position {
		return statelog.Position{Stream: stream, Generation: 1, Seq: seq}
	}
	got := statelog.Spent{
		Index:  []statelog.Position{at(probeStream, 7)},
		Waited: []statelog.Position{at(probeStream, 7)},
	}.Merge(statelog.Spent{
		Index:       []statelog.Position{at(probeStream, 9), at(elsewhere.Stream, 3)},
		Waited:      []statelog.Position{at(probeStream, 9), at(elsewhere.Stream, 3)},
		Unconfirmed: []string{elsewhere.Stream},
	}).Merge(statelog.Spent{
		Waited:      []statelog.Position{at(probeStream, 8)},
		Unconfirmed: []string{elsewhere.Stream},
	})
	if idx, _ := got.IndexOn(probeStream); idx != at(probeStream, 7) {
		t.Errorf("the probe log's index is %v, want the first one established", idx)
	}
	if idx, _ := got.IndexOn(elsewhere.Stream); idx != at(elsewhere.Stream, 3) {
		t.Errorf("the other log's index is %v", idx)
	}
	if !got.WaitedFor(at(probeStream, 9)) || got.WaitedFor(at(probeStream, 10)) {
		t.Errorf("waited out %v, want the probe log's furthest (9)", got.Waited)
	}
	if got.WaitedFor(at(elsewhere.Stream, 4)) || !got.WaitedFor(at(elsewhere.Stream, 3)) {
		t.Errorf("waited out %v, want the other log's own (3)", got.Waited)
	}
	if !slices.Equal(got.Unconfirmed, []string{elsewhere.Stream}) || got.UnconfirmedOn(probeStream) {
		t.Errorf("unconfirmed on %v, want the other log alone, once", got.Unconfirmed)
	}
	if (statelog.Spent{}).Merge(statelog.Spent{}).IsZero() != true {
		t.Error("nothing merged with nothing paid for something")
	}
}
