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

// stubStore is a database for the cases that never open a transaction: the
// local refusals, which are answered from what this node already knows.
type stubStore struct {
	reads atomic.Int64
	err   error
}

func (s *stubStore) Read(_ context.Context, fn func(*sql.Tx) error) error {
	s.reads.Add(1)
	if s.err != nil {
		return s.err
	}
	return fn(nil)
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
		CaughtUp:  true,
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
				&countingAppends{inner: h.log, n: &appends}, probeEncode, h.gen.Load, nil)
			if err != nil {
				t.Fatalf("NewReadIndex: %v", err)
			}
			db := &stubStore{}
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
	index, err := statelog.NewReadIndex(probeDomain{}, refusingAppender{}, probeEncode,
		h.gen.Load, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}
	r := newReader(t, &stubStore{}, healthy, &stubWaiter{at: healthy().Position}, index)

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
		&countingAppends{inner: h.log, n: &appends}, probeEncode, h.gen.Load, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}
	w := &stubWaiter{at: healthy().Position}
	r := newReader(t, &stubStore{}, healthy, w, index)

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
	index, err := statelog.NewReadIndex(probeDomain{}, h.log, probeEncode, h.gen.Load, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}
	w := &stubWaiter{at: healthy().Position}
	r := newReader(t, &stubStore{}, healthy, w, index)

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
	r := newReader(t, &stubStore{}, lagged, w, nil)

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
		&countingAppends{inner: broker.log, n: &appends}, probeEncode, broker.gen.Load, nil)
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
	r := newReader(t, &stubStore{}, lagged, &stubWaiter{at: healthy().Position}, nil)

	q := pointQuery(statelog.ReadStale)
	q.MaxLag = time.Second
	_, err := r.Read(t.Context(), q, func(*sql.Tx) error { return nil })
	var refusal *statelog.Refused
	if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseTooStale {
		t.Fatalf("Read = %v, want a too_stale refusal", err)
	}

	// AND AN UNREADABLE LAG REFUSES RATHER THAN SERVING UNBOUNDED. A
	// zero-because-unknown lag answers "not behind at all" to a read that
	// asked exactly that question.
	unknown := func() statelog.Health {
		h := healthy()
		h.Lag = nil
		return h
	}
	r = newReader(t, &stubStore{}, unknown, &stubWaiter{at: healthy().Position}, nil)
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
	r := newReader(t, &stubStore{}, healthy, &stubWaiter{at: healthy().Position}, nil)
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
