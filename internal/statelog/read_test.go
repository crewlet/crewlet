package statelog_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
			index, err := statelog.NewReadIndex(probeDomain{}, &countingAppends{inner: h.log, n: &appends}, testSigner(t, probeDomain{}), probeEncode, h.gen.Load, nil)
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
	index, err := statelog.NewReadIndex(probeDomain{}, refusingAppender{}, testSigner(t, probeDomain{}), probeEncode,
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
	index, err := statelog.NewReadIndex(probeDomain{}, &countingAppends{inner: h.log, n: &appends}, testSigner(t, probeDomain{}), probeEncode, h.gen.Load, nil)
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
	index, err := statelog.NewReadIndex(probeDomain{}, h.log, testSigner(t, probeDomain{}), probeEncode, h.gen.Load, nil)
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

// A REFUSAL WAITING CANNOT CLEAR IS NEVER TOLD TO COME BACK, on the read side
// or the write side, and one that can is told when.
//
// [statelog.RetryAfter] is the one rule every surface answering a refusal in a
// status code reads its Retry-After from. Each surface wrote its own, and /chart
// and the work surface both turned a node holding a record it cannot decode —
// or an evicted one — into "retry in two seconds", which a client obeys for as
// long as nobody upgrades or readmits the node.
func TestARefusalWaitingCannotClearIsNeverToldToComeBack(t *testing.T) {
	t.Parallel()
	const otherwise = 2 * time.Second
	for _, code := range statelog.ReadRefusals {
		refused := &statelog.Refused{Code: code, Level: statelog.ReadSession,
			RetryAfter: statelog.RetryHint(code, 1_000, 100)}
		// WRAPPED, because a surface meets it inside whatever the domain
		// said about it.
		got := statelog.RetryAfter(fmt.Errorf("tracker: read: %w", refused), otherwise)
		switch {
		case !code.Retryable() && got != 0:
			t.Errorf("read refusal %q says come back in %s, and waiting on "+
				"this node cannot clear it", code, got)
		case code.Retryable() && got != refused.RetryAfter:
			t.Errorf("read refusal %q says %s, want its own derived %s",
				code, got, refused.RetryAfter)
		}
	}
	// A RETRYABLE REFUSAL THAT DERIVED NOTHING gets the caller's scale
	// rather than no header at all, which would read as a node down for
	// good.
	bare := &statelog.Refused{Code: statelog.RefuseBehind, Level: statelog.ReadSession}
	if got := statelog.RetryAfter(bare, otherwise); got != otherwise {
		t.Errorf("a retryable read refusal with no hint says %s, want %s",
			got, otherwise)
	}
	for _, c := range []struct {
		reason    statelog.Reason
		retryable bool
	}{
		{statelog.ReasonBehind, true},
		{statelog.ReasonBelowFloor, true},
		{statelog.ReasonFloorUnknown, true},
		{statelog.ReasonEvictionUnknown, true},
		{statelog.ReasonEvicted, false},
		{statelog.ReasonDeferred, false},
		{statelog.ReasonDeleted, false},
		{statelog.ReasonGated, false},
		{statelog.ReasonRetired, false},
		{statelog.ReasonLogFull, false},
		{statelog.ReasonSkew, false},
	} {
		want := time.Duration(0)
		if c.retryable {
			want = otherwise
		}
		refused := fmt.Errorf("chart: publish: %w",
			&statelog.Unavailable{Reason: c.reason, Detail: "a detail"})
		if got := statelog.RetryAfter(refused, otherwise); got != want {
			t.Errorf("write refusal %q says come back in %s, want %s", c.reason,
				got, want)
		}
		if c.reason.Retryable() != c.retryable {
			t.Errorf("%q reports retryable = %v, want %v", c.reason,
				c.reason.Retryable(), c.retryable)
		}
	}
	// AND AN ERROR THIS PACKAGE DID NOT MAKE is the caller's to judge.
	if got := statelog.RetryAfter(errors.New("a store blip"), otherwise); got != otherwise {
		t.Errorf("a foreign error says %s, want the caller's own %s", got, otherwise)
	}
}

// A WRITE REFUSAL AND ITS READ TWIN AGREE ON WHETHER WAITING HELPS.
//
// A [statelog.Reason] spelled like a [statelog.ReadRefusal] names the same
// state of the same node — behind, below the floor, a floor nobody could read,
// evicted, a record it cannot decode, a full log — so the two answers are one
// fact said twice. They disagreed: `floor_unknown` and `below_floor` told a
// writer to come back and a reader, on the same node at the same instant, that
// waiting could not clear it. Both clear on their own — the floor is read
// again, and a below-floor node rejoins by itself.
func TestAWriteRefusalAndItsReadTwinAgreeOnWaiting(t *testing.T) {
	t.Parallel()
	twins := 0
	for _, read := range statelog.ReadRefusals {
		write := statelog.Reason(read)
		switch write {
		case statelog.ReasonBehind, statelog.ReasonDeferred,
			statelog.ReasonBelowFloor, statelog.ReasonFloorUnknown,
			statelog.ReasonEvicted, statelog.ReasonLogFull:
		default:
			continue
		}
		twins++
		if write.Retryable() != read.Retryable() {
			t.Errorf("%q: a write refusal says retryable=%v and a read refusal "+
				"says %v, about one state of one node", read, write.Retryable(),
				read.Retryable())
		}
	}
	// THE SIX, so a twin renamed on one side stops being compared rather
	// than silently passing.
	if twins != 6 {
		t.Errorf("compared %d twins, want 6 — a reason and a refusal spelled "+
			"alike went missing from one side", twins)
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
	index, err := statelog.NewReadIndex(probeDomain{}, &countingAppends{inner: broker.log, n: &appends}, testSigner(t, probeDomain{}), probeEncode, broker.gen.Load, nil)
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
	index, err := statelog.NewReadIndex(probeDomain{}, &countingAppends{inner: h.log, n: &appends}, testSigner(t, probeDomain{}), probeEncode, h.gen.Load, nil)
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
		r := newReader(t, &stubStore{}, healthy, w, index)
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
	r := newReader(t, &stubStore{}, healthy, w, index)
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
	index, err := statelog.NewReadIndex(probeDomain{}, h.log, testSigner(t, probeDomain{}), probeEncode, h.gen.Load, nil)
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
			r := newReader(t, &stubStore{}, healthy, w, index)
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
	r := newReader(t, &stubStore{}, drifting, w, nil)

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
