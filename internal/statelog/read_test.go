package statelog_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// stubStore counts the transactions a read opens over a real, empty estate.
//
// REAL rather than a nil transaction, because every read that is SERVED opens
// one and its first statement is the coverage probe: a stub that handed the
// reader nothing could only stand in for a read that is refused before it gets
// there, and a case that thought it was one of those and was not would panic
// rather than say so. The local refusals still never reach it, which is what
// the counter shows.
type stubStore struct {
	inner interface {
		Read(context.Context, func(*sql.Tx) error) error
	}
	reads atomic.Int64
	err   error
}

func newStubStore(t *testing.T) *stubStore {
	t.Helper()
	return &stubStore{inner: newApplyHarness(t, probeDomain{}).db.Replicated()}
}

func (s *stubStore) Read(ctx context.Context, fn func(*sql.Tx) error) error {
	s.reads.Add(1)
	if s.err != nil {
		return s.err
	}
	return s.inner.Read(ctx, fn)
}

// stubWaiter reports a position and never blocks, so a case about WHICH
// target a level picks is not also a case about waiting for it.
type stubWaiter struct {
	at     statelog.Position
	waited atomic.Int64
	last   atomic.Int64
	err    error
}

func (w *stubWaiter) Committed() statelog.Position { return w.at }

func (w *stubWaiter) WaitCommitted(_ context.Context, p statelog.Position) error {
	w.waited.Add(1)
	w.last.Store(p.Packed())
	return w.err
}

func (w *stubWaiter) WaitApplied(context.Context, statelog.ScopeSet, statelog.Position) error {
	return nil
}

func healthy() statelog.Health {
	lag := uint64(0)
	first := uint64(1)
	floor := uint64(1)
	return statelog.Health{
		Position:  statelog.Position{Stream: probeStream, Generation: 1, Seq: 100},
		Drained:   true,
		Floor:     statelog.Floor{State: statelog.FloorOK, ReadAt: time.Now()},
		Lag:       &lag,
		FirstSeq:  &first,
		TrimFloor: &floor,
	}
}

func newReader(t *testing.T, db interface {
	Read(context.Context, func(*sql.Tx) error) error
}, h func() statelog.Health, waiter statelog.Waiter, index *statelog.ReadIndex) *statelog.Reader {
	t.Helper()
	r, err := statelog.NewReader(statelog.ReaderDeps{
		Domain: probeDomain{},
		DB:     db,
		Index:  index,
		Waiter: waiter,
		Health: h,
		Drain:  func() float64 { return 2000 },
	})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	return r
}

func pointQuery(level statelog.ReadLevel) statelog.Query {
	return statelog.Query{
		Level: level,
		Scope: statelog.ScopeSet{Paths: []string{"object/a"}},
	}
}

// A DOOMED READ NEVER APPENDS, and the refusals run cheapest first.
//
// Each of these is answered from what this node already knows, for nothing —
// and each says this node's rows are WRONG rather than old, so no amount of
// freshness would make the answer serveable. Appending a barrier to discover
// that spends a quorum round trip on a read that could never be certified.
func TestADoomedReadRefusesBeforeItAppends(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		health func() statelog.Health
		want   statelog.ReadRefusal
	}{
		"an evicted node": {
			health: func() statelog.Health {
				h := healthy()
				h.Evicted = true
				return h
			},
			want: statelog.RefuseEvicted,
		},
		"a node below the trim floor": {
			health: func() statelog.Health {
				h := healthy()
				h.Floor.State = statelog.FloorBelow
				return h
			},
			want: statelog.RefuseBelowFloor,
		},
		"a floor nobody could read": {
			health: func() statelog.Health {
				h := healthy()
				h.Floor.ReadAt = time.Now().Add(-2 * time.Minute)
				return h
			},
			want: statelog.RefuseFloorUnknown,
		},
		"a stalled applier": {
			health: func() statelog.Health {
				h := healthy()
				h.Stalled = true
				return h
			},
			want: statelog.RefuseStalled,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var appends atomic.Int64
			h := newHarness(t)
			index, err := statelog.NewReadIndex(probeDomain{},
				&countingAppends{inner: h.log, n: &appends}, noCeiling(t), probeEncode, h.gen.Load, nil)
			if err != nil {
				t.Fatalf("NewReadIndex: %v", err)
			}
			db := newStubStore(t)
			r := newReader(t, db, tc.health, &stubWaiter{at: healthy().Position}, index)

			_, err = r.Read(t.Context(), pointQuery(statelog.ReadLinearizable),
				func(*sql.Tx) error { return nil })
			var refusal *statelog.Refused
			if !errors.As(err, &refusal) {
				t.Fatalf("Read = %v, want a Refused", err)
			}
			if refusal.Code != tc.want {
				t.Fatalf("code = %q, want %q", refusal.Code, tc.want)
			}
			if got := appends.Load(); got != 0 {
				t.Fatalf("a doomed read appended %d barrier(s) — a read that "+
					"could never be certified must not spend a quorum round trip "+
					"finding that out", got)
			}
			if got := db.reads.Load(); got != 0 {
				t.Fatalf("a doomed read opened %d transaction(s)", got)
			}
			// AND IT REFUSES WITH A REASON A PERSON CAN ACT ON.
			if refusal.Detail == "" {
				t.Error("the refusal names nothing to do about it")
			}
		})
	}
}

// A NODE REPLAYING UP TO THE FLOOR TELLS A READER TO COME BACK, and when.
//
// The floor is published before the purge it licenses, so a node can sit
// below it while the log still holds every record it lacks. That is a wait,
// not a fault: the answer is `behind`, which a caller retries here, with a
// hint from this node's own drain and a detail naming the floor — never
// `below_floor`, which sends a caller elsewhere and an operator to a restore
// the node does not need. And it is still decided locally, before a barrier.
func TestANodeReplayingUpToTheFloorIsTheOneToComeBackTo(t *testing.T) {
	t.Parallel()
	var appends atomic.Int64
	h := newHarness(t)
	index, err := statelog.NewReadIndex(probeDomain{},
		&countingAppends{inner: h.log, n: &appends}, noCeiling(t), probeEncode, h.gen.Load, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}
	replaying := func() statelog.Health {
		h := healthy()
		floor, lag := uint64(500), uint64(800)
		h.Floor.State = statelog.FloorReplaying
		h.TrimFloor, h.Lag = &floor, &lag
		return h
	}
	db := newStubStore(t)
	r := newReader(t, db, replaying, &stubWaiter{at: healthy().Position}, index)

	for _, level := range []statelog.ReadLevel{
		statelog.ReadLinearizable, statelog.ReadStale, statelog.ReadConsistentPrefix,
	} {
		_, err = r.Read(t.Context(), pointQuery(level), func(*sql.Tx) error { return nil })
		var refusal *statelog.Refused
		if !errors.As(err, &refusal) {
			t.Fatalf("%s: Read = %v, want a Refused", level, err)
		}
		if refusal.Code != statelog.RefuseBehind || !refusal.Code.Retryable() {
			t.Fatalf("%s: code = %q, want %q, which a caller comes back for",
				level, refusal.Code, statelog.RefuseBehind)
		}
		if refusal.RetryAfter <= 0 {
			t.Errorf("%s: the refusal carries no retry hint", level)
		}
		if !strings.Contains(refusal.Detail, "published trim floor 500") {
			t.Errorf("%s: the detail does not name the floor it is replaying to: %q",
				level, refusal.Detail)
		}
	}
	if got := appends.Load(); got != 0 {
		t.Fatalf("a read below the floor appended %d barrier(s)", got)
	}
	if got := db.reads.Load(); got != 0 {
		t.Fatalf("a read below the floor opened %d transaction(s)", got)
	}
}

// A STALL BELOW THE FLOOR SERVES NOTHING, not even the one read a stall
// otherwise survives.
//
// A consistent-prefix read asks only for a coherent point in the log's order,
// so a frozen prefix still answers it — at or above the floor. Below it, a
// stall outranks the replay ([statelog.Health.Refusal] reports `stalled`,
// since a replay that is not moving is not one to wait on), and that order
// alone let the stall exemption serve rows below the floor every node must
// hold before it answers — while the same node with a first sequence one lower
// reported `below_floor` and served nothing.
func TestAStallBelowTheFloorServesNoRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	index, err := statelog.NewReadIndex(probeDomain{}, h.log, noCeiling(t), probeEncode, h.gen.Load, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}
	stalled := func(floor statelog.FloorState) func() statelog.Health {
		return func() statelog.Health {
			hp := healthy()
			hp.Stalled = true
			hp.Floor.State = floor
			return hp
		}
	}

	// THE CONTROL: at the floor the exemption holds, or the case below
	// would pass on a reader that refuses every stalled read.
	db := newStubStore(t)
	r := newReader(t, db, stalled(statelog.FloorOK), &stubWaiter{at: healthy().Position}, index)
	if _, err := r.Read(t.Context(), pointQuery(statelog.ReadConsistentPrefix),
		func(*sql.Tx) error { return nil }); err != nil {
		t.Fatalf("a stalled node at the floor refused a consistent-prefix read: %v", err)
	}

	db = newStubStore(t)
	r = newReader(t, db, stalled(statelog.FloorReplaying), &stubWaiter{at: healthy().Position}, index)
	_, err = r.Read(t.Context(), pointQuery(statelog.ReadConsistentPrefix),
		func(*sql.Tx) error { return nil })
	var refusal *statelog.Refused
	if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseStalled {
		t.Fatalf("a stalled node below the floor answered a consistent-prefix read "+
			"with %v, want a %q refusal — its rows are below the floor every node "+
			"must hold before it answers", err, statelog.RefuseStalled)
	}
	if got := db.reads.Load(); got != 0 {
		t.Fatalf("a stalled node below the floor opened %d read transaction(s)", got)
	}
}

// countingAppends counts barriers so "never appends" is an assertion rather
// than a hope.
type countingAppends struct {
	inner statelog.Appender
	n     *atomic.Int64
}

func (c *countingAppends) Append(ctx context.Context, subject, msgID string, expect *uint64, body []byte) (uint64, bool, error) {
	c.n.Add(1)
	return c.inner.Append(ctx, subject, msgID, expect, body)
}

func (c *countingAppends) LastSeq(ctx context.Context, subject string) (uint64, bool, error) {
	return c.inner.LastSeq(ctx, subject)
}

// A STALE READ SURVIVES A FULL LOG, and a linearizable one does not.
//
// A full log refuses appends rather than dropping records, so the barrier a
// linearizable read rests on cannot be written — but a level that takes no
// broker call at all is unaffected. That asymmetry is worth stating, because
// the tempting reading is that a full log stops reads.
func TestAFullLogCostsTheLevelsThatAppendAndNoOthers(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	index, err := statelog.NewReadIndex(probeDomain{}, refusingAppender{}, noCeiling(t), probeEncode,
		h.gen.Load, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}
	r := newReader(t, newStubStore(t), healthy, &stubWaiter{at: healthy().Position}, index)

	_, err = r.Read(t.Context(), pointQuery(statelog.ReadLinearizable),
		func(*sql.Tx) error { return nil })
	var refusal *statelog.Refused
	if !errors.As(err, &refusal) {
		t.Fatalf("a linearizable read on a full log = %v, want a Refused", err)
	}
	if refusal.Code != statelog.RefuseLogFull {
		t.Fatalf("code = %q, want %q", refusal.Code, statelog.RefuseLogFull)
	}
	if refusal.RetryAfter != 0 {
		t.Errorf("a full log carries a retry hint of %s — waiting does not empty "+
			"a log, an operator does", refusal.RetryAfter)
	}

	// AND THE LEVELS THAT TAKE NO BROKER CALL KEEP ANSWERING.
	for _, level := range []statelog.ReadLevel{statelog.ReadStale, statelog.ReadSession} {
		if _, err := r.Read(t.Context(), pointQuery(level), func(*sql.Tx) error { return nil }); err != nil {
			t.Errorf("a %s read on a full log = %v, want it served", level, err)
		}
	}
}

// refusingAppender is a broker whose log is at its ceiling.
type refusingAppender struct{}

func (refusingAppender) Append(context.Context, string, string, *uint64, []byte) (uint64, bool, error) {
	return 0, false, &statelog.Unavailable{
		Reason: statelog.ReasonLogFull,
		Detail: "maximum bytes exceeded",
	}
}

func (refusingAppender) LastSeq(context.Context, string) (uint64, bool, error) {
	return 0, false, nil
}

// A SESSION READ WITH NOTHING TO WAIT FOR TAKES NO BROKER CALL.
//
// It is what makes the level free in the common case: a caller that has
// written nothing has nothing to be behind, and appending a barrier for it
// would make the cheap level cost the same as the expensive one.
func TestASessionReadWithNoHighWaterMarkWaitsForNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var appends atomic.Int64
	index, err := statelog.NewReadIndex(probeDomain{},
		&countingAppends{inner: h.log, n: &appends}, noCeiling(t), probeEncode, h.gen.Load, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}
	w := &stubWaiter{at: healthy().Position}
	r := newReader(t, newStubStore(t), healthy, w, index)

	if _, err := r.Read(t.Context(), pointQuery(statelog.ReadSession),
		func(*sql.Tx) error { return nil }); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := appends.Load(); got != 0 {
		t.Fatalf("a session read appended %d barrier(s)", got)
	}
	if got := w.waited.Load(); got != 0 {
		t.Fatalf("a session read waited %d time(s) for nothing", got)
	}

	// AND ONE WITH A HIGH-WATER MARK WAITS FOR EXACTLY THAT.
	q := pointQuery(statelog.ReadSession)
	q.Session = statelog.Position{Stream: probeStream, Generation: 1, Seq: 42}
	if _, err := r.Read(t.Context(), q, func(*sql.Tx) error { return nil }); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := w.last.Load(); got != q.Session.Packed() {
		t.Fatalf("the read waited for packed %d, want the caller's own %d",
			got, q.Session.Packed())
	}
	if got := appends.Load(); got != 0 {
		t.Fatalf("a session read with a high-water mark appended %d barrier(s) "+
			"— it waits for what the caller already knows, not for a new "+
			"position", got)
	}
}

// A LINEARIZABLE READ WAITS FOR ITS OWN BARRIER.
func TestALinearizableReadWaitsForThePositionItsBarrierEstablished(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	index, err := statelog.NewReadIndex(probeDomain{}, h.log, noCeiling(t), probeEncode, h.gen.Load, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}
	w := &stubWaiter{at: healthy().Position}
	r := newReader(t, newStubStore(t), healthy, w, index)

	if _, err := r.Read(t.Context(), pointQuery(statelog.ReadLinearizable),
		func(*sql.Tx) error { return nil }); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := w.waited.Load(); got != 1 {
		t.Fatalf("a linearizable read waited %d time(s), want exactly 1 — a "+
			"barrier creates a record this node has not applied, so it ALWAYS "+
			"waits for one", got)
	}
	if w.last.Load() == 0 {
		t.Fatal("the read waited for the zero position")
	}
}

// A NODE THAT NEVER REACHES THE TARGET REFUSES `behind`, WITH A DERIVED HINT.
//
// A flat hint is wrong in both directions on the same fleet: too early on a
// node grinding through a bulk apply, too late on one that caught up in
// milliseconds. The hint divides the lag by the drain this node has actually
// been observed at.
func TestAReadThatCannotReachItsTargetRefusesWithADerivedHint(t *testing.T) {
	t.Parallel()
	lagged := func() statelog.Health {
		h := healthy()
		lag := uint64(8_000)
		h.Lag = &lag
		return h
	}
	w := &stubWaiter{at: healthy().Position, err: context.DeadlineExceeded}
	r := newReader(t, newStubStore(t), lagged, w, nil)

	q := pointQuery(statelog.ReadSession)
	q.Session = statelog.Position{Stream: probeStream, Generation: 1, Seq: 9_000}
	_, err := r.Read(t.Context(), q, func(*sql.Tx) error { return nil })
	var refusal *statelog.Refused
	if !errors.As(err, &refusal) {
		t.Fatalf("Read = %v, want a Refused", err)
	}
	if refusal.Code != statelog.RefuseBehind {
		t.Fatalf("code = %q, want %q", refusal.Code, statelog.RefuseBehind)
	}
	// 8 000 records behind at 2 000 a second is four seconds, and it is a
	// derivation rather than a constant: change either input and it moves.
	if want := 4 * time.Second; refusal.RetryAfter != want {
		t.Fatalf("the hint is %s, want %s — 8 000 records behind at the 2 000 "+
			"a second this node was measured draining", refusal.RetryAfter, want)
	}
}

// A REFUSAL WAITING CANNOT CLEAR CARRIES NO HINT.
//
// It is what separates a hint from a redirect. A node holding a record it
// cannot decode will still be holding it however long the caller waits, and a
// hint there sends a caller round a loop that cannot terminate.
func TestOnlyARefusalWaitingCanClearCarriesAHint(t *testing.T) {
	t.Parallel()
	for _, code := range statelog.ReadRefusals {
		hint := statelog.RetryHint(code, 1_000, 100)
		if code.Retryable() && hint == 0 {
			t.Errorf("%q is retryable and carries no hint", code)
		}
		if !code.Retryable() && hint != 0 {
			t.Errorf("%q carries a hint of %s and waiting on this node cannot "+
				"clear it", code, hint)
		}
	}
	// AND THE COUNT IS DERIVED FROM THE SET. A number in a comment and a
	// set in code are two lists, and the comment is the one nobody
	// updates.
	seen := map[statelog.ReadRefusal]struct{}{}
	for _, code := range statelog.ReadRefusals {
		if !code.Valid() {
			t.Errorf("%q is in the set and reports itself invalid", code)
		}
		if _, dup := seen[code]; dup {
			t.Errorf("%q appears twice in the set", code)
		}
		seen[code] = struct{}{}
	}
	if statelog.ReadRefusal("made_up").Valid() {
		t.Error("an unknown refusal code reports itself valid")
	}
}

// A POINT READ REFUSES ON A DEFERRED SCOPE AND A SET READ CLAIMS NOTHING.
//
// The two are different in kind. A point read is about an object whose row may
// be stale, so it refuses. A set read cannot enumerate what would have ENTERED
// the set — a deferred create is an absence with no local row at all — so it
// is served at the level asked for, for the rows it does return, and makes no
// completeness claim.
func TestADeferredScopeRefusesAPointReadAndUncertifiesASetRead(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 9, "project/ENG"))
	if err := h.run(1); err != nil {
		t.Fatalf("run: %v", err)
	}

	deferredHealth := func() statelog.Health {
		s := healthy()
		s.Deferred = 1
		s.DeferredFrom = 1
		return s
	}
	var appends atomic.Int64
	broker := newHarness(t)
	index, err := statelog.NewReadIndex(probeDomain{},
		&countingAppends{inner: broker.log, n: &appends}, noCeiling(t), probeEncode, broker.gen.Load, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}
	r := newReader(t, h.db.Replicated(), deferredHealth,
		&stubWaiter{at: healthy().Position}, index)

	// A POINT READ about an object inside the deferred scope.
	point := statelog.Query{
		Level: statelog.ReadLinearizable,
		Scope: statelog.ScopeSet{Paths: []string{"project/ENG/object/b"}},
	}
	_, err = r.Read(t.Context(), point, func(*sql.Tx) error { return nil })
	var refusal *statelog.Refused
	if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseDeferred {
		t.Fatalf("a point read inside a deferred scope = %v, want a deferred "+
			"refusal", err)
	}
	if !strings.Contains(refusal.Detail, "9") {
		t.Errorf("the refusal does not name the record version an operator has "+
			"to run a build for: %q", refusal.Detail)
	}
	if got := appends.Load(); got != 0 {
		t.Fatalf("a read that could never be certified appended %d barrier(s)", got)
	}

	// A SET READ over the same scope is served, and claims nothing.
	set := point
	set.Set = true
	set.Level = statelog.ReadStale
	answer, err := r.Read(t.Context(), set, func(*sql.Tx) error { return nil })
	if err != nil {
		t.Fatalf("a set read inside a deferred scope = %v, want it served", err)
	}
	if answer.Complete {
		t.Fatal("a set read over a deferred scope claimed completeness — it " +
			"cannot enumerate what would have entered the set, because a " +
			"deferred create is an absence with no local row")
	}
	if answer.Incomplete == nil || answer.Incomplete.Direction != "unknown" {
		t.Fatalf("the incomplete block is %+v — the direction is a FIELD rather "+
			"than an omission, so a reader meets the fact rather than inferring "+
			"it", answer.Incomplete)
	}
	if answer.Level != statelog.ReadStale {
		t.Errorf("the answer is at level %q — coverage and freshness are two "+
			"facts, and an incomplete answer is still as fresh as it was asked "+
			"to be", answer.Level)
	}

	// AND A READ OUTSIDE THE DEFERRED SCOPE IS SERVED WHOLE.
	outside := statelog.Query{
		Level: statelog.ReadStale,
		Scope: statelog.ScopeSet{Paths: []string{"project/OPS/object/c"}},
	}
	answer, err = r.Read(t.Context(), outside, func(*sql.Tx) error { return nil })
	if err != nil || !answer.Complete {
		t.Fatalf("a read outside every deferred scope = (%+v, %v), want a "+
			"complete answer", answer, err)
	}
}

// A STALE READ WITH A BOUND REFUSES WHEN THIS NODE IS PAST IT.
func TestAStaleReadRefusesPastTheBoundItAsksFor(t *testing.T) {
	t.Parallel()
	lagged := func() statelog.Health {
		h := healthy()
		lag := uint64(60_000)
		h.Lag = &lag
		return h
	}
	r := newReader(t, newStubStore(t), lagged, &stubWaiter{at: healthy().Position}, nil)

	q := pointQuery(statelog.ReadStale)
	q.MaxLag = time.Second
	_, err := r.Read(t.Context(), q, func(*sql.Tx) error { return nil })
	var refusal *statelog.Refused
	if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseTooStale {
		t.Fatalf("Read = %v, want a too_stale refusal", err)
	}

	// AND THE SAME BOUND IN RECORDS, which is the reading the broker
	// actually gives: the duration above is DERIVED from it through this
	// node's drain rate, and a bound stated in records that was validated
	// and then ignored is a bound that never bounded anything.
	byRecords := pointQuery(statelog.ReadStale)
	byRecords.MaxLagSeq = 100
	_, err = r.Read(t.Context(), byRecords, func(*sql.Tx) error { return nil })
	if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseTooStale {
		t.Fatalf("a stale read accepting 100 records behind, on a node 60000 "+
			"behind, = %v, want a too_stale refusal", err)
	}
	// AND A NODE INSIDE THAT BOUND IS SERVED, so the case above is not
	// passing against a reader that refuses every record bound it is given.
	within := pointQuery(statelog.ReadStale)
	within.MaxLagSeq = 60_000
	if _, err := r.Read(t.Context(), within, func(*sql.Tx) error { return nil }); err != nil {
		t.Fatalf("a stale read accepting 60000 records behind, on a node "+
			"exactly 60000 behind, = %v, want it served", err)
	}

	// AND AN UNREADABLE LAG REFUSES RATHER THAN SERVING UNBOUNDED. A
	// zero-because-unknown lag answers "not behind at all" to a read that
	// asked exactly that question.
	unknown := func() statelog.Health {
		h := healthy()
		h.Lag = nil
		return h
	}
	r = newReader(t, newStubStore(t), unknown, &stubWaiter{at: healthy().Position}, nil)
	_, err = r.Read(t.Context(), q, func(*sql.Tx) error { return nil })
	if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseBrokerUnreachable {
		t.Fatalf("a bounded stale read with an unknown lag = %v, want "+
			"broker_unreachable", err)
	}
}

// EVERY READ LEVEL IS ONE THIS BUILD KNOWS, and consistent-prefix is never a
// default.
//
// It is weaker than session in a way that is invisible in the answer, so a
// surface defaulting to it would silently downgrade every reader that did not
// know to ask for more.
func TestTheReadLevelsAreAClosedSet(t *testing.T) {
	t.Parallel()
	if len(statelog.ReadLevels) != 4 {
		t.Fatalf("%d read levels, want 4", len(statelog.ReadLevels))
	}
	for _, level := range statelog.ReadLevels {
		if !level.Valid() {
			t.Errorf("%q is in the set and reports itself invalid", level)
		}
	}
	if statelog.ReadLevel("eventual").Valid() {
		t.Error("an unknown level reports itself valid")
	}
	r := newReader(t, newStubStore(t), healthy, &stubWaiter{at: healthy().Position}, nil)
	if _, err := r.Read(t.Context(), pointQuery("eventual"),
		func(*sql.Tx) error { return nil }); err == nil {
		t.Fatal("a read at an unknown level was served")
	}
}

// A DEFERRED RECORD LANDING WHILE A READ WAITS IS THE SAME HOLE THROUGH
// ANOTHER DOOR.
//
// Coverage is probed before the barrier, because a read that could never be
// certified must not append one. But the wait that follows can be seconds, and
// a record this node cannot decode arriving inside it would be invisible to
// that probe — so the answer's own transaction opens with the same question.
func TestADeferralLandingWhileAReadWaitsIsCaughtByTheSecondProbe(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	// Nothing is deferred yet, so the probe before the barrier passes.
	deferredHealth := func() statelog.Health {
		s := healthy()
		s.Deferred = 1
		return s
	}
	landing := &landsBetween{
		inner: h.db.Replicated(),
		land: func() {
			h.fetch.offer(1, env(1, "edit", "a", "op-1", 9, "project/ENG"))
			if err := h.run(1); err != nil {
				t.Errorf("land the deferred record: %v", err)
			}
		},
	}
	r := newReader(t, landing, deferredHealth, &stubWaiter{at: healthy().Position}, nil)

	q := statelog.Query{
		Level: statelog.ReadSession,
		Scope: statelog.ScopeSet{Paths: []string{"project/ENG/object/b"}},
	}
	_, err := r.Read(t.Context(), q, func(*sql.Tx) error { return nil })
	var refusal *statelog.Refused
	if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseDeferred {
		t.Fatalf("Read = %v, want a deferred refusal — a record that landed "+
			"while the read waited is the same hole the first probe exists for",
			err)
	}
}

// landsBetween lets a record arrive between the coverage probe and the
// answer's own transaction, which is the interval the second probe covers.
type landsBetween struct {
	inner interface {
		Read(context.Context, func(*sql.Tx) error) error
	}
	land  func()
	calls atomic.Int64
}

func (l *landsBetween) Read(ctx context.Context, fn func(*sql.Tx) error) error {
	err := l.inner.Read(ctx, fn)
	if l.calls.Add(1) == 1 && l.land != nil {
		l.land()
	}
	return err
}

// THE CALLER'S FLOOR IS HONOURED AT EVERY LEVEL, not only at `session`.
//
// A write answers with the position its record landed at, and a caller that
// hands it back as [statelog.Query.MinPosition] is served nothing from before
// it — whatever else it asked for. It used to reach the reader at `session`
// only: a `stale` read carrying a floor waited for nothing and served rows
// from before the write the caller had just been told about, and a
// `linearizable` one waited for its own barrier, which on the honest path is
// past the floor and on the pasted-position path is not.
func TestTheCallersFloorIsHonouredAtEveryLevel(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var appends atomic.Int64
	index, err := statelog.NewReadIndex(probeDomain{},
		&countingAppends{inner: h.log, n: &appends}, noCeiling(t), probeEncode, h.gen.Load, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}
	// FAR PAST ANYTHING A BARRIER ESTABLISHES, so a level that waited for
	// its own target instead of the floor is told apart from one that
	// waited for the later of the two.
	floor := statelog.Position{Stream: probeStream, Generation: 1, Seq: 1 << 30}

	for _, level := range []statelog.ReadLevel{
		statelog.ReadLinearizable, statelog.ReadSession,
		statelog.ReadStale, statelog.ReadConsistentPrefix,
	} {
		w := &stubWaiter{at: healthy().Position}
		r := newReader(t, newStubStore(t), healthy, w, index)
		q := pointQuery(level)
		q.MinPosition = floor
		if _, err := r.Read(t.Context(), q, func(*sql.Tx) error { return nil }); err != nil {
			t.Fatalf("%s read with a floor: %v", level, err)
		}
		if got := w.last.Load(); got != floor.Packed() {
			t.Errorf("a %s read with a floor waited for packed %d, want the "+
				"floor's %d — a caller handed a position by its own write was "+
				"served rows from before it", level, got, floor.Packed())
		}
	}
	// AND ONLY THE LINEARIZABLE READ APPENDED: the floor is a wait, never
	// a barrier, so the three cheap levels stay cheap.
	if got := appends.Load(); got != 1 {
		t.Errorf("four floored reads appended %d barrier(s), want the "+
			"linearizable one's alone", got)
	}

	// A FLOOR ON ANOTHER STREAM IS REFUSED, NOT WAITED FOR, at the level
	// that never appends — the one where a wait for a foreign sequence
	// would otherwise run out the whole budget.
	w := &stubWaiter{at: healthy().Position}
	r := newReader(t, newStubStore(t), healthy, w, index)
	q := pointQuery(statelog.ReadStale)
	q.MinPosition = statelog.Position{Stream: "SOME_OTHER_LOG", Generation: 1, Seq: 5}
	_, err = r.Read(t.Context(), q, func(*sql.Tx) error { return nil })
	var refusal *statelog.Refused
	if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseWrongStream {
		t.Fatalf("a stale read with a floor on another stream = %v, want "+
			"wrong_stream", err)
	}
	if got := w.waited.Load(); got != 0 {
		t.Errorf("the wrong-stream read waited %d time(s) before refusing", got)
	}
}

// A FLOOR ON ANOTHER STREAM IS REFUSED AT EVERY LEVEL, INCLUDING WHEN IT
// SORTS LOW.
//
// [statelog.Position.Packed] deliberately carries the generation and the
// sequence and NOT the stream, so a foreign floor compared against a local
// target is two coordinates from two number spaces. Selecting the maximum
// first therefore discarded a foreign floor whenever it happened to sort below
// the level's own target — and the guard that names `wrong_stream` then
// inspected a purely local position and waved it through. The read was served
// as though no floor had been named, under the level the caller asked for.
//
// The two cases here are the two sides of that comparison: a foreign floor
// BELOW the barrier (silently dropped before) and one above it (refused
// before). They must answer the same way, because which side of a local
// barrier a foreign sequence happens to fall on says nothing about anything.
func TestAFloorOnAnotherStreamIsRefusedAtEveryLevel(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	index, err := statelog.NewReadIndex(probeDomain{}, h.log, noCeiling(t), probeEncode, h.gen.Load, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}
	for _, floor := range []statelog.Position{
		{Stream: "SOME_OTHER_LOG", Generation: 1, Seq: 1},
		{Stream: "SOME_OTHER_LOG", Generation: 1, Seq: 1 << 30},
	} {
		for _, level := range []statelog.ReadLevel{
			statelog.ReadLinearizable, statelog.ReadSession,
			statelog.ReadStale, statelog.ReadConsistentPrefix,
		} {
			w := &stubWaiter{at: healthy().Position}
			r := newReader(t, newStubStore(t), healthy, w, index)
			q := pointQuery(level)
			q.MinPosition = floor
			_, err := r.Read(t.Context(), q, func(*sql.Tx) error { return nil })
			var refusal *statelog.Refused
			if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseWrongStream {
				t.Errorf("a %s read floored at %s answered %v, want a "+
					"wrong_stream refusal", level, floor, err)
				continue
			}
			if got := w.waited.Load(); got != 0 {
				t.Errorf("a %s read floored at %s waited %d time(s) before "+
					"refusing — a sequence from another log is a caller bug "+
					"rather than a position this node can reach", level, floor, got)
			}
		}
	}
}

// THE STALENESS BOUND IS ENFORCED AGAINST THE ANSWER'S OWN MOMENT, NOT A
// SNAPSHOT FROM BEFORE THE WAIT.
//
// A read may now wait for the caller's own floor for up to the read budget,
// and what ends that wait is this node reaching a position — which says
// nothing about how far the log ran on meanwhile. Checking the bound only
// before the wait served answers past it and printed the pre-wait lag beside
// the post-wait rows, so the number and the rows described different moments.
func TestAStalenessBoundIsRecheckedAfterTheFloorWait(t *testing.T) {
	t.Parallel()
	// The node is close behind when the read arrives and far behind by
	// the time its floor is reached.
	var reads atomic.Int64
	drifting := func() statelog.Health {
		hp := healthy()
		lag := uint64(1)
		if reads.Add(1) > 1 {
			lag = 5_000
		}
		hp.Lag = &lag
		return hp
	}
	w := &stubWaiter{at: healthy().Position}
	r := newReader(t, newStubStore(t), drifting, w, nil)

	q := pointQuery(statelog.ReadStale)
	q.MinPosition = statelog.Position{Stream: probeStream, Generation: 1, Seq: 42}
	q.MaxLagSeq = 250
	_, err := r.Read(t.Context(), q, func(*sql.Tx) error { return nil })
	var refusal *statelog.Refused
	if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseTooStale {
		t.Fatalf("a floored read that fell 5000 records behind while it waited "+
			"answered %v, want too_stale — the bound was checked against a "+
			"snapshot from before the wait", err)
	}
	if got := w.waited.Load(); got != 1 {
		t.Errorf("the read waited %d time(s), want 1 — the refusal must come "+
			"from re-reading health after the wait, not from skipping it", got)
	}

	// AND THE ANSWER REPORTS THE POST-WAIT LAG when it is served, so the
	// figure beside the rows describes the moment they were read.
	reads.Store(0)
	within := pointQuery(statelog.ReadStale)
	within.MinPosition = q.MinPosition
	answer, err := r.Read(t.Context(), within, func(*sql.Tx) error { return nil })
	if err != nil {
		t.Fatalf("an unbounded floored read: %v", err)
	}
	if answer.Lag == nil || *answer.Lag != 5_000 {
		t.Errorf("the answer reports lag %v, want the post-wait 5000 — a lag "+
			"from before the wait describes a different moment from the rows "+
			"it is printed beside", answer.Lag)
	}
}

// landingWaiter delivers a record while a read waits for its target, which is
// the interval a health snapshot taken before the wait cannot see into.
type landingWaiter struct {
	stubWaiter
	land func()
}

func (w *landingWaiter) WaitCommitted(ctx context.Context, p statelog.Position) error {
	if w.land != nil {
		w.land()
	}
	return w.stubWaiter.WaitCommitted(ctx, p)
}

// A DEFERRAL LANDING DURING THE WAIT IS CAUGHT EVEN WHEN NOTHING WAS DEFERRED
// WHEN THE READ BEGAN.
//
// The wait is where such a record arrives: the applier has to cross it to reach
// the position the read is waiting for. The answer's own transaction probed only
// when the health read BEFORE the wait already reported a deferral, so on a node
// holding none when the read began — every healthy node, which is the only kind
// that meets a newer peer's record for the first time — the probe was skipped on
// the one path it exists for, and a point read certified rows the record had
// already made wrong.
func TestADeferralLandingDuringTheWaitIsCaughtWhenNoneWasDeferredBefore(t *testing.T) {
	t.Parallel()
	for _, set := range []bool{false, true} {
		t.Run(map[bool]string{false: "point", true: "set"}[set], func(t *testing.T) {
			t.Parallel()
			h := newApplyHarness(t, probeDomain{})
			waiter := &landingWaiter{
				stubWaiter: stubWaiter{at: healthy().Position},
				land: func() {
					h.fetch.offer(1, env(1, "edit", "a", "op-1", 9, "project/ENG"))
					if err := h.run(1); err != nil {
						t.Errorf("land the deferred record: %v", err)
					}
				},
			}
			// Nothing is deferred as far as the snapshot before the wait
			// knows, which is what makes the probe before the barrier
			// pass and leaves the answer's transaction as the only door.
			r := newReader(t, h.db.Replicated(), healthy, waiter, nil)
			q := statelog.Query{
				Level:       statelog.ReadSession,
				Scope:       statelog.ScopeSet{Paths: []string{"project/ENG/object/b"}},
				MinPosition: healthy().Position,
				Set:         set,
			}
			answer, err := r.Read(t.Context(), q, func(*sql.Tx) error { return nil })
			if waiter.waited.Load() == 0 {
				t.Fatal("the read did not wait, so this case is not about the wait")
			}
			if !set {
				var refusal *statelog.Refused
				if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseDeferred {
					t.Fatalf("Read = %v, want a deferred refusal — the record "+
						"landed while the read waited, and the rows it is about "+
						"may already be wrong", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("a set read = %v, want it served", err)
			}
			if answer.Complete || answer.Incomplete == nil {
				t.Fatalf("a set read that waited across a deferral claimed "+
					"completeness: %+v", answer)
			}
		})
	}
}

// A POINT READ NAMED BY REFERENCE IS PROBED ON WHERE THE REFERENCE RESOLVES.
//
// A path formed from the reference itself — a key read as though it were an
// id, an object with no container — is filed under nothing, so the probe
// passed on every deferral there is. The resolver reads the rows, in the
// answer's own transaction, and the probe asks about what it found.
func TestAPointReadNamedByReferenceIsProbedWhereItResolves(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 9, "project/ENG"))
	if err := h.run(1); err != nil {
		t.Fatalf("run: %v", err)
	}
	deferred := func() statelog.Health {
		s := healthy()
		s.Deferred = 1
		s.DeferredFrom = 1
		return s
	}
	r := newReader(t, h.db.Replicated(), deferred, &stubWaiter{at: healthy().Position}, nil)
	fresh := statelog.Freshness{Level: statelog.ReadStale}

	var resolved atomic.Int64
	inside := func(context.Context, *sql.Tx) (statelog.ScopeSet, error) {
		resolved.Add(1)
		return statelog.ScopeSet{Paths: []string{"project/ENG/object/b"}}, nil
	}
	_, err := r.Read(t.Context(), fresh.Resolved(inside, false), func(*sql.Tx) error { return nil })
	var refusal *statelog.Refused
	if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseDeferred {
		t.Fatalf("a reference resolving inside the deferred scope = %v, want a "+
			"deferred refusal", err)
	}
	if resolved.Load() == 0 {
		t.Fatal("the resolver was never asked, so the probe was about nothing")
	}

	// THE CONTROL: the same reader serves a reference that resolves
	// elsewhere, or the case above passes on a reader that refuses every
	// point read while anything is deferred.
	outside := func(context.Context, *sql.Tx) (statelog.ScopeSet, error) {
		return statelog.ScopeSet{Paths: []string{"project/OPS/object/c"}}, nil
	}
	answer, err := r.Read(t.Context(), fresh.Resolved(outside, false), func(*sql.Tx) error { return nil })
	if err != nil || !answer.Complete {
		t.Fatalf("a reference resolving outside every deferral = (%+v, %v), want "+
			"a complete answer", answer, err)
	}

	// A READ THAT REPORTS ITS COVERAGE is served over the same resolution,
	// and says what it could not account for rather than refusing.
	answer, err = r.Read(t.Context(), fresh.Resolved(inside, true), func(*sql.Tx) error { return nil })
	if err != nil {
		t.Fatalf("a reporting read resolving inside the deferral = %v, want it "+
			"served", err)
	}
	if answer.Complete || answer.Incomplete == nil {
		t.Fatalf("a reporting read resolving inside the deferral claimed "+
			"completeness: %+v", answer)
	}

	// AND THE CONTRACT IS EXACTLY ONE OF THE TWO: an object named by
	// reference has no scope until its rows are read.
	both := fresh.Resolved(outside, false)
	both.Scope = statelog.ScopeSet{Paths: []string{"project/OPS"}}
	if _, err := r.Read(t.Context(), both, func(*sql.Tx) error { return nil }); err == nil {
		t.Error("a read that declared a scope AND resolved one was served")
	}
}
