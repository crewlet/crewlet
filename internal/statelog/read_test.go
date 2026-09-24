package statelog_test

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

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
	return newReaderDraining(t, db, h, waiter, index, 2000)
}

// newReaderDraining is [newReader] over a node measured draining drain records
// a second, for the cases about what a backlog is stated as.
func newReaderDraining(t *testing.T, db interface {
	Read(context.Context, func(*sql.Tx) error) error
}, h func() statelog.Health, waiter statelog.Waiter, index *statelog.ReadIndex,
	drain float64) *statelog.Reader {

	t.Helper()
	r, err := statelog.NewReader(statelog.ReaderDeps{
		Domain: probeDomain{},
		DB:     db,
		Index:  index,
		Waiter: waiter,
		Health: h,
		Drain:  func() float64 { return drain },
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
		// says is what the detail must carry, where the refusal code alone
		// does not say what to do.
		says string
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
		// A HEALTH READ THAT FAILED BEFORE IT REACHED THE FLOOR says why in
		// its own words: an unanswered coordination read and a floor at a
		// generation this node has left refuse alike and are fixed apart.
		"a health read that failed before it established the floor": {
			health: func() statelog.Health {
				return statelog.Health{Err: "the published floor for probe is at " +
					"generation 3 and this node is on 1"}
			},
			want: statelog.RefuseFloorUnknown,
			says: "generation 3",
		},
		// A BROKER THAT DID NOT ANSWER THE HEALTH READ is named as the
		// broker, not as the floor that went unread behind it: the one is
		// retried and the other is not.
		"a health read the broker did not answer": {
			health: func() statelog.Health {
				return statelog.Health{BrokerErr: "nats: timeout"}
			},
			want: statelog.RefuseBrokerUnreachable,
			says: "nats: timeout",
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
			if !strings.Contains(refusal.Detail, tc.says) {
				t.Errorf("the detail %q does not say %q", refusal.Detail, tc.says)
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

// A STALE READ SURVIVES A FULL LOG, and a linearizable one is told the log is
// full.
//
// A full log refuses appends rather than dropping records, so the barrier a
// linearizable read rests on cannot be written — but a level that takes no
// broker call at all is unaffected. That asymmetry is worth stating, because
// the tempting reading is that a full log stops reads.
//
// OVER A REAL LOG AT ITS BYTE CEILING, because what the broker answers there
// is its own API error naming "maximum bytes exceeded", and no refusal this
// package made. Read any other way it is a majority that did not agree: a read
// told to wait out an election, about a log only an operator can empty.
//
// Mutation: hand the barrier's append error to the reader unclassified and the
// code reads no_quorum, with the election's retry hint.
func TestAFullLogCostsTheLevelsThatAppendAndNoOthers(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// A CEILING OF ONE BYTE on an empty log, so the barrier is the append
	// that meets it.
	updateProbeStream(t, h, "set its max_bytes", func(cfg *jetstream.StreamConfig) {
		cfg.MaxBytes = 1
	})
	index, err := statelog.NewReadIndex(probeDomain{}, h.log, probeEncode,
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
		t.Fatalf("code = %q, want %q: %v", refusal.Code, statelog.RefuseLogFull, err)
	}
	if !strings.Contains(refusal.Detail, "maximum bytes exceeded") {
		t.Errorf("the refusal does not carry the broker's words: %v", err)
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

// A BARRIER THE BROKER REFUSES FOR A REASON OF ITS OWN IS NAMED FOR IT.
//
// A sealed stream refuses the barrier's append with neither a full log's
// error nor a quorum's silence, and each of those codes would send somebody to
// the wrong place: a byte ceiling that is not the limit, or an election that is
// not happening. The broker's words are the detail, and waiting on this node
// does not unseal a stream, so there is no hint.
//
// Mutation: map a broker refusal to no_quorum and the code reads it, with the
// election's retry hint.
func TestABarrierTheBrokerRefusesIsRefusedInItsWords(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sealProbeStream(t, h)
	index, err := statelog.NewReadIndex(probeDomain{}, h.log, probeEncode,
		h.gen.Load, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}
	r := newReader(t, &stubStore{}, healthy, &stubWaiter{at: healthy().Position}, index)

	_, err = r.Read(t.Context(), pointQuery(statelog.ReadLinearizable),
		func(*sql.Tx) error { return nil })
	var refusal *statelog.Refused
	if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseBarrierRefused {
		t.Fatalf("a linearizable read on a sealed log = %v, want %s", err,
			statelog.RefuseBarrierRefused)
	}
	if !strings.Contains(refusal.Detail, "sealed") {
		t.Errorf("the refusal does not carry the broker's words: %v", err)
	}
	if refusal.RetryAfter != 0 {
		t.Errorf("a refused barrier carries a retry hint of %s — waiting does not "+
			"change a stream's setting", refusal.RetryAfter)
	}
	if _, err := r.Read(t.Context(), pointQuery(statelog.ReadStale),
		func(*sql.Tx) error { return nil }); err != nil {
		t.Errorf("a stale read on a sealed log = %v, want it served", err)
	}
}

// A BARRIER A FULL INGEST QUEUE REFUSED IS WORTH COMING BACK FOR, AND SAYS WHEN.
//
// The queue stored nothing and drains on its own, so this is the one broker
// refusal of a barrier that waiting clears. Filed with the refusals that name a
// setting it carries no hint and sends somebody to change a stream nothing is
// wrong with; filed with a quorum that did not agree it sends them to look for
// an election.
//
// Mutation: map the queue's answer to barrier_refused and the refusal carries
// no hint.
func TestABarrierAFullQueueRefusedIsRetryableWithAHint(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	index, err := statelog.NewReadIndex(probeDomain{}, &scripted{log: h.log,
		always: tooManyRequests}, probeEncode, h.gen.Load, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}
	r := newReader(t, &stubStore{}, healthy, &stubWaiter{at: healthy().Position}, index)

	_, err = r.Read(t.Context(), pointQuery(statelog.ReadLinearizable),
		func(*sql.Tx) error { return nil })
	var refusal *statelog.Refused
	if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseBrokerBusy {
		t.Fatalf("a linearizable read the queue refused = %v, want %s", err,
			statelog.RefuseBrokerBusy)
	}
	if !refusal.Code.Retryable() {
		t.Errorf("%s is not retryable, and a drained queue takes the same barrier",
			refusal.Code)
	}
	if refusal.RetryAfter != statelog.BusyRetryHint {
		t.Errorf("the refusal's hint is %s, want %s", refusal.RetryAfter,
			statelog.BusyRetryHint)
	}
	if !strings.Contains(refusal.Detail, "ingest queue") {
		t.Errorf("the refusal does not name the full queue: %v", err)
	}
	if _, err := r.Read(t.Context(), pointQuery(statelog.ReadStale),
		func(*sql.Tx) error { return nil }); err != nil {
		t.Errorf("a stale read while the queue is full = %v, want it served", err)
	}
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

// A RETRY HINT IS THE BACKLOG OVER THE DRAIN AS MEASURED, fraction and all.
//
// Through the one conversion every backlog stated as a time takes
// ([statelog.BacklogTime]), so a node's hint and its staleness bound can never
// state one backlog as two times. The floor stands in for a rate NOBODY
// MEASURED and for nothing else: a measured rate below one record a second is
// the rate this node is applying at, and floored it would send the caller back
// before the backlog has had time to clear.
//
// Mutation: floor a measured rate and the quarter-a-second case reads 3 s;
// truncate the rate and the fractional ones overstate; drop the round-up and
// the 1.2 s case reads a fraction of a second.
func TestARetryHintIsTheBacklogOverTheDrainAsMeasured(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		lag   uint64
		drain float64
		want  time.Duration
	}{
		{name: "a fractional drain between one and two", lag: 3, drain: 1.5,
			want: 2 * time.Second},
		{name: "a measured drain below the floor is used as measured", lag: 3,
			drain: 0.25, want: 12 * time.Second},
		{name: "an unmeasured drain reads at the floor", lag: 3, drain: 0,
			want: 3 * time.Second},
		{name: "rounded up to a whole second", lag: 3, drain: 2.5,
			want: 2 * time.Second},
		{name: "nothing to catch up on", lag: 0, drain: 1.5,
			want: statelog.ElectionRetryHint},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := statelog.RetryHint(statelog.RefuseBehind, c.lag, c.drain); got != c.want {
				t.Errorf("a %d-record backlog at %v records a second hints %v, "+
					"want %v", c.lag, c.drain, got, c.want)
			}
		})
	}

	// AND A HINT PAST WHAT A DURATION HOLDS IS STILL A HINT: a drain of a
	// tiny fraction of a record a second over the largest backlog there is
	// passes the range, and converted unguarded it is a negative duration —
	// a retry already due.
	hint := statelog.RetryHint(statelog.RefuseBehind, math.MaxUint64, 1e-12)
	if hint <= 0 || hint%time.Second != 0 {
		t.Errorf("the largest backlog at a vanishing rate hints %v, want the "+
			"largest whole number of seconds rather than a wrapped duration", hint)
	}
}

// A STALENESS BOUND IN SECONDS IS THE RECORD LAG OVER THE DRAIN AS MEASURED.
//
// A caller's `max_lag_seconds` is checked against the record lag stated as a
// time, and the statement is [statelog.BacklogTime]'s. Three records behind at
// one and a half a second is two seconds behind, which a caller accepting two
// and a half is served — where a rate truncated to a whole one states it as
// three and refuses a read inside its bound. And a measured rate below the
// floor is not raised to it: half a record a second is six seconds for three
// records, which a caller accepting five is refused.
//
// Mutation: truncate the drain and the first case refuses; floor a measured
// rate and the second is served past its bound.
func TestAStalenessBoundInSecondsDividesByTheDrainAsMeasured(t *testing.T) {
	t.Parallel()
	lagged := func(records uint64) func() statelog.Health {
		return func() statelog.Health {
			h := healthy()
			h.Lag = &records
			return h
		}
	}
	for _, c := range []struct {
		name   string
		lag    uint64
		drain  float64
		maxLag time.Duration
		served bool
	}{
		{name: "two seconds behind, accepting two and a half", lag: 3, drain: 1.5,
			maxLag: 2500 * time.Millisecond, served: true},
		{name: "six seconds behind at a measured half a record a second", lag: 3,
			drain: 0.5, maxLag: 5 * time.Second, served: false},
		{name: "three seconds behind at the floor, unmeasured", lag: 3, drain: 0,
			maxLag: 2500 * time.Millisecond, served: false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newReaderDraining(t, &stubStore{}, lagged(c.lag),
				&stubWaiter{at: healthy().Position}, nil, c.drain)
			q := pointQuery(statelog.ReadStale)
			q.MaxLag = c.maxLag
			_, err := r.Read(t.Context(), q, func(*sql.Tx) error { return nil })
			if c.served {
				if err != nil {
					t.Errorf("a read inside its bound was refused: %v", err)
				}
				return
			}
			var refusal *statelog.Refused
			if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseTooStale {
				t.Errorf("a read past its bound answered %v, want too_stale", err)
			}
		})
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

// A RECORD HELD BACK BEHIND ONE THIS BUILD CANNOT DECODE IS NAMED AS HELD BACK,
// AND A SET READ COUNTS EVERY RETAINED RECORD IT MEETS.
//
// The apply loop retains a record it could decode when its scope meets one it
// cannot, so the retained set is not the undecodable set. A refusal calling the
// held-back record "a record at version 1 this build cannot decode" sends an
// operator looking for a build that reads a version this one already reads,
// and an incomplete block that counts one record where two meet the read
// understates the gap it exists to state.
//
// Mutation: name every retained record by its version and the refusal claims
// version 1 is undecodable; count the earliest alone and the block says 1.
func TestARetainedRecordIsNamedForWhyItIsHeldAndCounted(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	// VERSION 9 THIS BUILD CANNOT DECODE, and version 1 it can — retained
	// all the same, because its scope meets the first one's.
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 9, "project/ENG/object/a"))
	h.fetch.offer(2, env(2, "edit", "c", "op-2", 1,
		"project/ENG/object/a", "project/ENG/object/c"))
	if err := h.run(2); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, held := h.runner.Deferred(); held != 2 {
		t.Fatalf("the runner retains %d record(s), want both — the second is held "+
			"back behind the first, so this case is not the shape it names", held)
	}
	retaining := func() statelog.Health {
		s := healthy()
		s.Deferred, s.DeferredFrom = 2, 1
		return s
	}
	r := newReader(t, h.db.Replicated(), retaining, &stubWaiter{at: healthy().Position}, nil)

	// A POINT READ THAT ONLY THE HELD-BACK RECORD MEETS.
	_, err := r.Read(t.Context(), statelog.Query{
		Level: statelog.ReadStale,
		Scope: statelog.ScopeSet{Paths: []string{"project/ENG/object/c"}},
	}, func(*sql.Tx) error { return nil })
	var refusal *statelog.Refused
	if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseDeferred {
		t.Fatalf("a point read the held-back record meets = %v, want deferred", err)
	}
	if !strings.Contains(refusal.Detail, "held back") {
		t.Errorf("the refusal does not say the record is held back: %q", refusal.Detail)
	}
	if strings.Contains(refusal.Detail, "version 1") {
		t.Errorf("the refusal names version 1 as undecodable, and this build reads "+
			"it: %q", refusal.Detail)
	}

	// A SET READ OVER THE CONTAINER BOTH OF THEM ARE IN.
	answer, err := r.Read(t.Context(), statelog.Query{
		Level: statelog.ReadStale, Set: true,
		Scope: statelog.ScopeSet{Paths: []string{"project/ENG"}},
	}, func(*sql.Tx) error { return nil })
	if err != nil {
		t.Fatalf("a set read over the container = %v, want it served", err)
	}
	if answer.Incomplete == nil {
		t.Fatal("a set read two retained records meet claimed completeness")
	}
	if got := answer.Incomplete.Records; got != 2 {
		t.Errorf("the incomplete block counts %d record(s), want 2", got)
	}
	if got := answer.Incomplete.From.Seq; got != 1 {
		t.Errorf("the incomplete block starts at %d, want the earliest, 1", got)
	}
	if got := answer.Incomplete.Version; got != 9 {
		t.Errorf("the incomplete block names version %d, want the earliest "+
			"record's 9", got)
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
	index, err := statelog.NewReadIndex(probeDomain{},
		&countingAppends{inner: h.log, n: &appends}, probeEncode, h.gen.Load, nil)
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
	index, err := statelog.NewReadIndex(probeDomain{}, h.log, probeEncode, h.gen.Load, nil)
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
