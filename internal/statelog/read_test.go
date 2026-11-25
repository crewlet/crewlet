package statelog_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/statelog"
)

// stubStore counts the transactions a read opens over a real, empty estate.
//
// REAL, because every SERVED read opens one and its first statement is the
// coverage probe: a stub handing the reader nothing would panic in a case that
// wrongly thought its read was refused first. The local refusals never reach
// it, which the counter shows.
type stubStore struct {
	inner interface {
		Read(context.Context, func(*sql.Tx) error) error
	}
	reads atomic.Int64
	err   error
}

// newStubStore is a whole estate (a migrated store and the probe domain's
// tables), so a case builds ONE for every reader it makes: the rows are only
// read, and a case counts a fresh wrapper's reads ([stubStore.again]).
func newStubStore(t *testing.T) *stubStore {
	t.Helper()
	return &stubStore{inner: newApplyHarness(t, probeDomain{}).estate}
}

// again is the same estate behind a fresh read count, for a case that asks
// whether one reader opened a transaction after another already had.
func (s *stubStore) again() *stubStore { return &stubStore{inner: s.inner} }

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

func (w *stubWaiter) Reached() statelog.Position { return w.at }

func (w *stubWaiter) WaitReached(_ context.Context, p statelog.Position) error {
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
		Domain: probeDomain{}, Spec: specOf(probeDomain{}),
		Mode:   statelog.ModeNormal,
		DB:     db,
		Index:  index,
		Waiter: waiter,
		Health: readable(h),
		Drain:  func() float64 { return 2000 },
	})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	return r
}

// readable is a health provider that always manages to read the health it
// reports — every case here but the one about a health nobody could read.
func readable(h func() statelog.Health) func() (statelog.Health, error) {
	return func() (statelog.Health, error) { return h(), nil }
}

func pointQuery(level statelog.ReadLevel) statelog.Query {
	return statelog.Query{
		Level: level,
		Scope: statelog.ScopeSet{Paths: []string{"object/a"}},
	}
}

// A DOOMED READ NEVER APPENDS, and the refusals run cheapest first.
//
// Each is answered from what this node already knows and says its rows are
// WRONG rather than old, so no freshness makes the answer serveable: a barrier
// would spend a quorum round trip on a read that can never be certified.
func TestADoomedReadRefusesBeforeItAppends(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		health func() statelog.Health
		want   statelog.ReadRefusal

		// absent is what the refusal must NOT say: its detail reaches
		// every caller, and an applier's error holds a driver's message
		// or a database path.
		absent string
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
		"a halted applier": {
			health: func() statelog.Health {
				h := healthy()
				h.Err = "apply CREWLET_PROBE_LOG@1:41: open " +
					"/var/lib/crewlet/replicated.db: disk I/O error"
				return h
			},
			want:   statelog.RefuseStalled,
			absent: "/var/lib",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var appends atomic.Int64
			h := newHarness(t)
			index, err := statelog.NewReadIndex(probeDomain{}, specOf(probeDomain{}),
				&countingAppends{inner: h.log, n: &appends}, h.log.Members, testSigner(t, probeDomain{}), noCeiling(t), probeEncode, h.gen.Load, nil)
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
			if tc.absent != "" && strings.Contains(err.Error(), tc.absent) {
				t.Errorf("the refusal carries the applier's own error to the "+
					"caller: %q", err)
			}
		})
	}
}

// A NODE REPLAYING UP TO THE FLOOR TELLS A READER TO COME BACK, and when.
//
// The floor is published before the purge it licenses, so a node below it may
// still find every record it lacks in the log: a wait, not a fault. So
// `behind`, retried here with a hint from this node's drain and a detail
// naming the floor, never `below_floor` (which sends a caller elsewhere and an
// operator to a needless restore), and decided locally, before a barrier.
func TestANodeReplayingUpToTheFloorIsTheOneToComeBackTo(t *testing.T) {
	t.Parallel()
	var appends atomic.Int64
	h := newHarness(t)
	index, err := statelog.NewReadIndex(probeDomain{}, specOf(probeDomain{}),
		&countingAppends{inner: h.log, n: &appends}, h.log.Members, testSigner(t, probeDomain{}), noCeiling(t), probeEncode, h.gen.Load, nil)
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
// A frozen prefix still answers a consistent-prefix read at or above the
// floor. Below it [statelog.Health.Refusal] reports `stalled` ahead of the
// replay (a replay not moving is not one to wait on), and the stall exemption
// must not then serve rows below the floor every node must hold to answer.
func TestAStallBelowTheFloorServesNoRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	index, err := statelog.NewReadIndex(probeDomain{}, specOf(probeDomain{}), h.log, h.log.Members, testSigner(t, probeDomain{}), noCeiling(t), probeEncode, h.gen.Load, nil)
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

	db = db.again()
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
// A full log refuses appends, so a linearizable read's barrier cannot be
// written, but a level taking no broker call is unaffected: a full log does
// not stop reads.
//
// THE REFUSAL IS WHAT A BROKER SENDS, its own store failure, which the read
// index must classify as `log_full`, not `no_quorum` (retryable, with an
// election's hint, from a node refusing the next barrier identically). So the
// fake answers as a broker does, and a second case fills a REAL log.
func TestAFullLogCostsTheLevelsThatAppendAndNoOthers(t *testing.T) {
	t.Parallel()

	t.Run("the broker's own answer", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		assertFullLogRead(t, h, fullLog(), noCeiling(t))
	})

	t.Run("a real log at its ceiling", func(t *testing.T) {
		t.Parallel()
		h := newHarnessFor(t, tinyLogDomain{})
		// FILLED TO THE BYTE, in shrinking records: a barrier is a few
		// hundred bytes, so a log refused one large record can still take
		// one, and it is the room a barrier needs that has to be gone.
		written := 0
		for _, size := range []int{4 << 10, 256, 16} {
			for {
				_, err := h.write(probeSubject(fmt.Sprintf("o%d", written)),
					fmt.Sprintf("op-%d", written), strings.Repeat("x", size))
				if err != nil {
					break
				}
				written++
			}
		}
		// THROUGH THE LOG'S OWN RESERVE, as in production: a barrier
		// admitted past it would land in the room kept for gate records,
		// serving the read from a log refusing every ordinary write.
		assertFullLogRead(t, h, h.log, h.reserve)
	})
}

// assertFullLogRead reads at every level through a read index appending to
// log, which is full: the levels that append are refused `log_full` with no
// hint, and the ones that do not are served.
func assertFullLogRead(t *testing.T, h *harness, log statelog.Appender,
	admission statelog.Admission) {
	t.Helper()
	index, err := statelog.NewReadIndex(probeDomain{}, specOf(probeDomain{}), log,
		members(1), testSigner(t, probeDomain{}),
		admission, probeEncode, h.gen.Load, nil)
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
		t.Fatalf("code = %q (%s), want %q", refusal.Code, refusal.Detail,
			statelog.RefuseLogFull)
	}
	if refusal.RetryAfter != 0 {
		t.Errorf("a full log carries a retry hint of %s — waiting does not empty "+
			"a log, an operator does", refusal.RetryAfter)
	}
	if !strings.Contains(refusal.Detail, "byte ceiling") {
		t.Errorf("the refusal does not name the ceiling to raise: %s", refusal.Detail)
	}

	// AND THE LEVELS THAT TAKE NO BROKER CALL KEEP ANSWERING.
	for _, level := range []statelog.ReadLevel{statelog.ReadStale, statelog.ReadSession} {
		if _, err := r.Read(t.Context(), pointQuery(level), func(*sql.Tx) error { return nil }); err != nil {
			t.Errorf("a %s read on a full log = %v, want it served", level, err)
		}
	}
}

// A BARRIER THE BROKER REFUSED FOR A REASON OF ITS OWN IS NOT A MISSED QUORUM.
//
// A sealed stream, a JetStream store out of resources: the broker answered and
// will answer the next barrier the same, so `no_quorum`'s election hint would
// be false. A barrier nobody answered IS the missed quorum, on a log several
// members store, which the control holds.
func TestABarrierTheBrokerRefusedIsNotAMissedQuorum(t *testing.T) {
	t.Parallel()
	// AND THE WORDS: a refusal the broker NAMED carries them, as the
	// remedy; an unconfirmed barrier is told in this package's words naming
	// its subject, since the transport's own error would reach the caller.
	// `words` is what the detail must say, `absent` what it must not.
	//
	// AND THE MEMBERS: on three, an unconfirmed barrier is a majority that
	// did not agree or no leader answering; on one there is no majority, so
	// never `no_quorum` but a broker that did not answer or, when its time
	// ran out, the member's own stalled write. What the broker NAMED is the
	// same at any count.
	timedOut := fmt.Errorf("append: %w", context.DeadlineExceeded)
	for _, c := range []struct {
		name    string
		members int
		err     error
		want    statelog.ReadRefusal
		words   string
		absent  string
	}{
		{"a sealed stream", 3, &jetstream.APIError{Code: 400, ErrorCode: 10109,
			Description: "invalid operation on sealed stream"}, statelog.RefuseBrokerRefused,
			"invalid operation on sealed stream", ""},
		{"a sealed stream, on one member", 1, &jetstream.APIError{Code: 400, ErrorCode: 10109,
			Description: "invalid operation on sealed stream"}, statelog.RefuseBrokerRefused,
			"invalid operation on sealed stream", ""},
		// A BARRIER THE CLIENT REFUSED AS TOO LARGE is a server whose
		// max_payload refuses every record, and the client's decision, so
		// it is the broker's refusal too — never an unanswered append.
		{"a barrier past the server's max_payload", 3,
			fmt.Errorf("append: %w", nats.ErrMaxPayload), statelog.RefuseBrokerRefused,
			"max_payload", ""},
		{"nobody answered", 3, errors.New("nats: timeout dialling 10.0.0.7:4222"),
			statelog.RefuseNoQuorum, "was not confirmed", "10.0.0.7"},
		{"nobody answered, on one member", 1, errors.New("nats: timeout dialling 10.0.0.7:4222"),
			statelog.RefuseBrokerUnreachable, "did not answer its append", "10.0.0.7"},
		{"the barrier's time ran out", 3, timedOut,
			statelog.RefuseNoQuorum, "a majority of its members did not agree", "deadline"},
		{"the barrier's time ran out, on one member", 1, timedOut,
			statelog.RefuseBrokerStalled, "its own write is stalled", "deadline"},
		// A STORE THAT CLOSED UNDER THE BARRIER is a stream restarting:
		// the broker answered, and what it said is that the entry may
		// yet land — which clears exactly as an unanswered append does.
		{"a store closed under it", 3, &jetstream.APIError{Code: 503, ErrorCode: 10077,
			Description: "store is closed"}, statelog.RefuseNoQuorum,
			"was not confirmed", "store is closed"},
		{"a store closed under it, on one member", 1, &jetstream.APIError{Code: 503, ErrorCode: 10077,
			Description: "store is closed"}, statelog.RefuseBrokerUnreachable,
			"was not confirmed", "store is closed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			index, err := statelog.NewReadIndex(probeDomain{}, specOf(probeDomain{}), refusingAppender{err: c.err},
				members(c.members), testSigner(t, probeDomain{}), noCeiling(t), probeEncode, h.gen.Load, nil)
			if err != nil {
				t.Fatalf("NewReadIndex: %v", err)
			}
			r := newReader(t, &stubStore{}, healthy, &stubWaiter{at: healthy().Position}, index)
			_, err = r.Read(t.Context(), pointQuery(statelog.ReadLinearizable),
				func(*sql.Tx) error { return nil })
			var refusal *statelog.Refused
			if !errors.As(err, &refusal) || refusal.Code != c.want {
				t.Fatalf("a barrier the broker answered %v was refused as %v, want %q",
					c.err, err, c.want)
			}
			if got, want := refusal.RetryAfter > 0, c.want.Retryable(); got != want {
				t.Errorf("%q carries a hint of %s", c.want, refusal.RetryAfter)
			}
			if !strings.Contains(refusal.Detail, c.words) {
				t.Errorf("detail = %q, want it to say %q", refusal.Detail, c.words)
			}
			if c.absent != "" && strings.Contains(err.Error(), c.absent) {
				t.Errorf("the refusal carries the transport's own words to the "+
					"caller: %q", err)
			}
			unconfirmed := c.want == statelog.RefuseNoQuorum ||
				c.want == statelog.RefuseBrokerStalled || c.want == statelog.RefuseBrokerUnreachable
			if unconfirmed && !strings.Contains(refusal.Detail, probePrefix+"."+statelog.BarrierKind) {
				t.Errorf("detail = %q, want it to name the barrier's subject",
					refusal.Detail)
			}
		})
	}
}

// A BARRIER WHOSE LOG'S USAGE CANNOT BE READ IS NEVER APPENDED, AND SAYS SO.
//
// Admission reads the log's usage first; here the broker does not answer it.
// The read is refused as an unconfirmed barrier, by member count, in this
// package's words, never claiming an append that was not made. (A usage read
// answered leaderless is waited out first: election_test.go.)
//
// Mutation: drop barrierRefusal's usage_unknown arm and the detail goes red.
func TestABarrierWhoseLogUsageCannotBeReadIsNeverAppended(t *testing.T) {
	t.Parallel()
	unanswered := fmt.Errorf("read the log's state: %w", nats.ErrTimeout)
	for _, c := range []struct {
		members int
		want    statelog.ReadRefusal
	}{{3, statelog.RefuseNoQuorum}, {1, statelog.RefuseBrokerUnreachable}} {
		t.Run(fmt.Sprintf("on %d members", c.members), func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			unread, err := statelog.NewReserve(probeDomain{}.Stream(),
				func(context.Context) (statelog.Usage, error) { return statelog.Usage{}, unanswered })
			if err != nil {
				t.Fatalf("NewReserve: %v", err)
			}
			var appends atomic.Int64
			index, err := statelog.NewReadIndex(probeDomain{}, specOf(probeDomain{}),
				&countingAppends{inner: h.log, n: &appends}, members(c.members),
				testSigner(t, probeDomain{}), unread, probeEncode, h.gen.Load, nil)
			if err != nil {
				t.Fatalf("NewReadIndex: %v", err)
			}
			r := newReader(t, &stubStore{}, healthy, &stubWaiter{at: healthy().Position}, index)
			_, err = r.Read(t.Context(), pointQuery(statelog.ReadLinearizable),
				func(*sql.Tx) error { return nil })
			var refusal *statelog.Refused
			if !errors.As(err, &refusal) || refusal.Code != c.want {
				t.Fatalf("a barrier whose usage could not be read was refused as %v, want %q",
					err, c.want)
			}
			if !strings.Contains(refusal.Detail, "never appended") ||
				!strings.Contains(refusal.Detail, probePrefix+"."+statelog.BarrierKind) {
				t.Errorf("detail = %q, want it to say the barrier on its subject was never "+
					"appended", refusal.Detail)
			}
			if strings.Contains(err.Error(), nats.ErrTimeout.Error()) {
				t.Errorf("the refusal carries the broker's own words to the caller: %q", err)
			}
			if n := appends.Load(); n != 0 {
				t.Errorf("the barrier reached the broker %d times, want none", n)
			}
		})
	}
}

// A CALLER THAT GAVE UP IS NOT A REFUSAL.
//
// The read index answers a caller's cancellation or deadline with the
// context's error, as does the coverage probe's store read. A refusal there
// would be a false `no_quorum` (counted, with an election's hint) or
// `deferred_scope_unknown` (logged as an unreadable store), so the read
// answers the context's own error, as the floor wait does.
func TestACallerThatGaveUpIsNotARefusal(t *testing.T) {
	t.Parallel()
	gone, cancel := context.WithCancel(t.Context())
	cancel()

	t.Run("waiting on a barrier", func(t *testing.T) {
		t.Parallel()
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		index, err := statelog.NewReadIndex(probeDomain{}, specOf(probeDomain{}), blockingAppender{release: release},
			members(1), testSigner(t, probeDomain{}), noCeiling(t), probeEncode, func() uint32 { return 1 }, nil)
		if err != nil {
			t.Fatalf("NewReadIndex: %v", err)
		}
		r := newReader(t, &stubStore{}, healthy, &stubWaiter{at: healthy().Position}, index)
		_, err = r.Read(gone, pointQuery(statelog.ReadLinearizable),
			func(*sql.Tx) error { return nil })
		if !errors.Is(err, context.Canceled) || errors.Is(err, statelog.ErrUnavailable) {
			t.Errorf("a linearizable read whose caller gave up = %v, want %v and "+
				"no refusal", err, context.Canceled)
		}
	})

	t.Run("probing a deferred scope", func(t *testing.T) {
		t.Parallel()
		deferredHealth := func() statelog.Health {
			s := healthy()
			s.Deferred = 1
			s.DeferredFrom = 1
			return s
		}
		r := newReader(t, ctxStore{}, deferredHealth, &stubWaiter{at: healthy().Position}, nil)
		_, err := r.Read(gone, pointQuery(statelog.ReadStale),
			func(*sql.Tx) error { return nil })
		if !errors.Is(err, context.Canceled) || errors.Is(err, statelog.ErrUnavailable) {
			t.Errorf("a read whose caller gave up during the coverage probe = %v, "+
				"want %v and no refusal", err, context.Canceled)
		}
	})
}

// blockingAppender is a broker whose append does not return until release is
// closed, so a caller that gives up is the only way out of a read.
type blockingAppender struct{ release chan struct{} }

func (a blockingAppender) Append(context.Context, string, string, *uint64, []byte) (uint64, bool, error) {
	<-a.release
	return 0, false, errors.New("released")
}

func (blockingAppender) LastSeq(context.Context, string) (uint64, bool, error) {
	return 0, false, nil
}

// ctxStore is a store whose every read fails the way a real one does under a
// context that is done: with that context's error.
type ctxStore struct{}

func (ctxStore) Read(ctx context.Context, fn func(*sql.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("begin read: %w", err)
	}
	return fn(nil)
}

// refusingAppender is a broker that answers every append with err.
type refusingAppender struct{ err error }

// fullLog is a broker whose stream is at its byte ceiling: every append is
// answered what JetStream answers one past it.
func fullLog() refusingAppender {
	return refusingAppender{err: &jetstream.APIError{
		Code: 503, ErrorCode: 10077, Description: "maximum bytes exceeded"}}
}

func (a refusingAppender) Append(context.Context, string, string, *uint64, []byte) (uint64, bool, error) {
	return 0, false, a.err
}

func (refusingAppender) LastSeq(context.Context, string) (uint64, bool, error) {
	return 0, false, nil
}

// A SESSION READ WITH NOTHING TO WAIT FOR TAKES NO BROKER CALL.
//
// A caller that has written nothing has nothing to be behind; a barrier for it
// would make the cheap level cost what the expensive one does.
func TestASessionReadWithNoHighWaterMarkWaitsForNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var appends atomic.Int64
	index, err := statelog.NewReadIndex(probeDomain{}, specOf(probeDomain{}),
		&countingAppends{inner: h.log, n: &appends}, h.log.Members, testSigner(t, probeDomain{}), noCeiling(t), probeEncode, h.gen.Load, nil)
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

// A LINEARIZABLE READ WAITS FOR THE RECORD BEFORE ITS BARRIER, never the
// barrier: a barrier writes no row, so its commit is a checkpoint and an fsync
// changing nothing the read reads. A read handed the barrier by a node asked
// before waits for that same record and appends nothing, and a refusal still
// carries the BARRIER as what the read paid for, so every node a walk asks
// derives one position from one index.
func TestALinearizableReadWaitsForTheRecordBeforeItsBarrier(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var appends atomic.Int64
	index, err := statelog.NewReadIndex(probeDomain{}, specOf(probeDomain{}),
		&countingAppends{inner: h.log, n: &appends}, h.log.Members, testSigner(t, probeDomain{}),
		noCeiling(t), probeEncode, h.gen.Load, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}
	estate := newStubStore(t)
	// lastBarrier is the barrier the reads have appended so far: nothing else
	// appends to this log.
	lastBarrier := func() statelog.Position {
		t.Helper()
		_, last, err := h.log.Bounds(t.Context())
		if err != nil {
			t.Fatalf("read the log's end: %v", err)
		}
		return statelog.Position{Stream: probeStream, Generation: h.gen.Load(), Seq: last}
	}
	before := func(p statelog.Position) statelog.Position { return p.At(p.Seq - 1) }
	read := func(ctx context.Context, w *stubWaiter) error {
		_, err := newReader(t, estate, healthy, w, index).Read(ctx,
			pointQuery(statelog.ReadLinearizable), func(*sql.Tx) error { return nil })
		return err
	}

	own := &stubWaiter{at: healthy().Position}
	if err := read(t.Context(), own); err != nil {
		t.Fatalf("Read: %v", err)
	}
	barrier := lastBarrier()
	if got := appends.Load(); got != 1 {
		t.Fatalf("the read appended %d barrier(s), want 1", got)
	}
	if got, want := own.last.Load(), before(barrier).Packed(); got != want {
		t.Fatalf("the read waited for packed %d, want the record before its barrier "+
			"at %s (packed %d) — the barrier itself writes no row, and waiting for "+
			"its commit is waiting for an fsync that changes nothing the read reads",
			got, barrier, want)
	}

	// HANDED THE BARRIER, a node waits for the same record and appends nothing.
	handed := statelog.WithSpent(t.Context(), statelog.Spent{Index: []statelog.Position{barrier}})
	peer := &stubWaiter{at: healthy().Position}
	if err := read(handed, peer); err != nil {
		t.Fatalf("a read handed the barrier: %v", err)
	}
	if got := appends.Load(); got != 1 {
		t.Fatalf("a read handed the barrier appended one of its own (%d appends)", got)
	}
	if got, want := peer.last.Load(), before(barrier).Packed(); got != want {
		t.Fatalf("a read handed the barrier at %s waited for packed %d, want %d — "+
			"every node derives one position from one read index", barrier, got, want)
	}

	// AND A REFUSAL STILL CARRIES THE BARRIER as what the read paid for, beside
	// the record it waited its whole bound for.
	behind := &stubWaiter{at: healthy().Position, err: context.DeadlineExceeded}
	err = read(t.Context(), behind)
	var refusal *statelog.Refused
	if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseBehind {
		t.Fatalf("a read whose wait ran out = %v, want refused %s", err, statelog.RefuseBehind)
	}
	refused := lastBarrier()
	if !slices.Equal(refusal.Spent.Index, []statelog.Position{refused}) {
		t.Errorf("the refusal carries the read index %v, want the barrier %s — a node "+
			"asked next derives its own wait from the barrier", refusal.Spent.Index, refused)
	}
	if !slices.Equal(refusal.Spent.Waited, []statelog.Position{before(refused)}) {
		t.Errorf("the refusal says it waited for %v, want the record before its "+
			"barrier, %s", refusal.Spent.Waited, before(refused))
	}
	if !strings.Contains(refusal.Detail, before(refused).String()) ||
		!strings.Contains(refusal.Detail, refused.String()) {
		t.Errorf("the refusal's detail %q names neither the record the read waited "+
			"for nor the barrier it appended", refusal.Detail)
	}
}

// A BARRIER AT THE START OF A LOG STILL TAKES THE WAIT'S PATH. The code log's
// first record is @0:1, so a read whose barrier lands there waits for the zero
// position. The path after the wait, which stamps the answer from this node's
// applier and holds its staleness bound to a health read AFTER the wait, runs
// whatever the barrier's sequence, never only for a non-zero target.
func TestALinearizableReadAtTheStartOfALogIsStampedAndBounded(t *testing.T) {
	t.Parallel()
	// The waiter's position, which the health's is not, so an answer stamped
	// from the health read before the wait is told apart.
	stamped := statelog.Position{Stream: probeStream, Seq: 7}
	for name, tc := range map[string]struct {
		maxLagSeq uint64
		refused   bool
	}{
		"served, stamped from the applier":           {},
		"bounded, against the health after the wait": {maxLagSeq: 100, refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.gen.Store(0)
			index, err := statelog.NewReadIndex(probeDomain{}, specOf(probeDomain{}), h.log,
				h.log.Members, testSigner(t, probeDomain{}), noCeiling(t), probeEncode, h.gen.Load, nil)
			if err != nil {
				t.Fatalf("NewReadIndex: %v", err)
			}
			// LEVEL BEFORE THE WAIT AND FIVE HUNDRED BEHIND AFTER IT: the
			// log ran on while this node caught up.
			var healths atomic.Int64
			health := func() statelog.Health {
				hl := healthy()
				hl.Position = statelog.Position{Stream: probeStream}
				if healths.Add(1) > 1 {
					lag := uint64(500)
					hl.Lag = &lag
				}
				return hl
			}
			w := &stubWaiter{at: stamped}
			q := pointQuery(statelog.ReadLinearizable)
			q.MaxLagSeq = tc.maxLagSeq
			answer, err := newReader(t, newStubStore(t), health, w, index).Read(t.Context(), q,
				func(*sql.Tx) error { return nil })
			if _, last, berr := h.log.Bounds(t.Context()); berr != nil || last != 1 {
				t.Fatalf("the read's barrier is at %d (%v), want the log's first record", last, berr)
			}
			if got := w.waited.Load(); got != 1 {
				t.Fatalf("the read waited %d time(s), want 1 — for the zero position "+
					"before its barrier, which is the step that stamps and bounds it", got)
			}
			if tc.refused {
				var refusal *statelog.Refused
				if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseTooStale {
					t.Fatalf("a read bounded at %d records over a health 500 behind after "+
						"its wait = %v, want refused %s", tc.maxLagSeq, err, statelog.RefuseTooStale)
				}
				return
			}
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if answer.Position != stamped {
				t.Errorf("the answer is stamped %s, want the applier's %s — not the "+
					"position the health read before the wait reported", answer.Position, stamped)
			}
			if answer.Lag == nil || *answer.Lag != 500 {
				t.Errorf("the answer reports lag %v, want the 500 read after the wait", answer.Lag)
			}
		})
	}
}

// A NODE THAT NEVER REACHES THE TARGET REFUSES `behind`, WITH A DERIVED HINT.
//
// A flat hint is wrong in both directions on the same fleet: too early on a
// node grinding through a long backlog, too late on one that caught up in
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
	// AND THE SET ITSELF IS CHECKED, never a count in a comment, which
	// nobody updates.
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

// EVERY REFUSAL ABOUT THIS NODE IS ONE ANOTHER NODE MAY SERVE; only the four
// facts about the whole fleet are not. Every node appends to the one stream, so
// a full log and a broker's own refusal (a sealed stream, a store out of
// resources) refuse every node's barrier; a log one member stores takes every
// barrier through that member's write, so its stall holds every node; and a
// capacity window holds every node. Each code is named, so a new code is a
// decision this table must make, and a newer build's code is final.
func TestOnlyAFleetWideRefusalIsFinalForEveryNode(t *testing.T) {
	t.Parallel()
	want := map[statelog.ReadRefusal]bool{
		statelog.RefuseBehind:               true,
		statelog.RefuseDeferred:             true,
		statelog.RefuseDeferredScopeUnknown: true,
		statelog.RefuseStalled:              true,
		statelog.RefuseBelowFloor:           true,
		statelog.RefuseFloorUnknown:         true,
		statelog.RefuseEvicted:              true,
		statelog.RefuseBrokerUnreachable:    true,
		statelog.RefuseNoQuorum:             true,
		statelog.RefuseWrongStream:          true,
		statelog.RefuseTooStale:             true,
		statelog.RefuseLogFull:              false,
		statelog.RefuseBrokerRefused:        false,
		statelog.RefuseBrokerStalled:        false,
		statelog.RefuseMaintenance:          false,
	}
	for _, code := range statelog.ReadRefusals {
		elsewhere, judged := want[code]
		if !judged {
			t.Errorf("%q is a refusal this table does not judge: say whether another node may serve it", code)
			continue
		}
		if got := code.Elsewhere(); got != elsewhere {
			t.Errorf("%q says another node may serve it: %v, want %v", code, got, elsewhere)
		}
	}
	if statelog.ReadRefusal("made_up").Elsewhere() {
		t.Error("a refusal code this build does not know says another node may serve it")
	}
}

// A REFUSAL WAITING CANNOT CLEAR IS NEVER TOLD TO COME BACK, on the read side
// or the write side, and one that can is told when.
//
// [statelog.RetryAfter] is the one rule every surface reads its Retry-After
// from, so none tells a client to retry a node holding a record it cannot
// decode, or an evicted one, which only an upgrade or a readmission clears.
func TestARefusalWaitingCannotClearIsNeverToldToComeBack(t *testing.T) {
	t.Parallel()
	const otherwise = 2 * time.Second
	for _, code := range statelog.ReadRefusals {
		refused := &statelog.Refused{Code: code, Level: statelog.ReadSession,
			RetryAfter: statelog.RetryHint(code, 1_000, 100)}
		// WRAPPED, because a surface meets it inside whatever the domain
		// said about it.
		got := statelog.RetryAfter(fmt.Errorf("worklet: read: %w", refused), otherwise)
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
	covered := map[statelog.Reason]bool{}
	for _, c := range []struct {
		reason    statelog.Reason
		retryable bool
	}{
		{statelog.ReasonBehind, true},
		{statelog.ReasonBelowFloor, true},
		{statelog.ReasonFloorUnknown, true},
		{statelog.ReasonEvictionUnknown, true},
		{statelog.ReasonUsageUnknown, true},
		{statelog.ReasonEvicted, false},
		{statelog.ReasonDeferred, false},
		{statelog.ReasonDeleted, false},
		{statelog.ReasonAbandoned, false},
		{statelog.ReasonOvertaken, false},
		{statelog.ReasonLogFull, false},
		{statelog.ReasonRecordTooLarge, false},
		{statelog.ReasonBrokerRefused, false},
		{statelog.ReasonSkew, false},
		{statelog.ReasonOpReused, false},
		{statelog.ReasonLogTruncated, false},
		{statelog.ReasonWrongStream, false},
		{statelog.ReasonSuperseded, false},
	} {
		covered[c.reason] = true
		want := time.Duration(0)
		if c.retryable {
			want = otherwise
		}
		refused := fmt.Errorf("worklet: publish: %w",
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
	// EVERY REASON HAS A ROW, so a reason added without deciding whether
	// waiting clears it fails here rather than defaulting to "no".
	for _, reason := range statelog.Reasons() {
		if !reason.Valid() {
			t.Errorf("%q is listed and does not report itself valid", reason)
		}
		if !covered[reason] {
			t.Errorf("write refusal %q has no row above, so nothing decided "+
				"whether waiting clears it", reason)
		}
	}
	if statelog.Reason("log_fool").Valid() {
		t.Error("a reason this build never declared reports itself valid")
	}
	// AND AN ERROR THIS PACKAGE DID NOT MAKE is the caller's to judge.
	if got := statelog.RetryAfter(errors.New("a store blip"), otherwise); got != otherwise {
		t.Errorf("a foreign error says %s, want the caller's own %s", got, otherwise)
	}
}

// A WRITE REFUSAL AND ITS READ TWIN AGREE ON WHETHER WAITING HELPS.
//
// A [statelog.Reason] spelled like a [statelog.ReadRefusal] names the same
// state of the same node, so the two answers are one fact said twice: e.g.
// `floor_unknown` and `below_floor` clear on their own (the floor is read
// again, a below-floor node rejoins by itself), for a writer and a reader alike.
func TestAWriteRefusalAndItsReadTwinAgreeOnWaiting(t *testing.T) {
	t.Parallel()
	// THE PAIRS ARE DERIVED from the two vocabularies rather than listed, so
	// a reason added to one side and spelled like the other is compared the
	// day it lands instead of skipped by a list nobody extended.
	var twins []string
	for _, read := range statelog.ReadRefusals {
		write := statelog.Reason(read)
		if !write.Valid() {
			continue
		}
		twins = append(twins, string(read))
		if write.Retryable() != read.Retryable() {
			t.Errorf("%q: a write refusal says retryable=%v and a read refusal "+
				"says %v, about one state of one node", read, write.Retryable(),
				read.Retryable())
		}
	}
	// AND THE SET IS THE ONE THE CONSISTENCY GUIDE STATES, both ways: a twin
	// renamed on one side stops being compared, and a new one is a sentence
	// in that guide nobody has written yet.
	want := []string{"behind", "deferred", "below_floor", "floor_unknown",
		"evicted", "log_full", "broker_refused", "wrong_stream"}
	slices.Sort(twins)
	slices.Sort(want)
	if !slices.Equal(twins, want) {
		t.Errorf("the reasons spelled like a read refusal are %v, want %v — "+
			"docs/guides/consistency.md names which write reasons have a read "+
			"twin and why the rest have none", twins, want)
	}
}

// A DEFERRED SCOPE THIS NODE COULD NOT READ IS REFUSED IN THIS PACKAGE'S
// WORDS, NEVER THE STORE'S.
//
// Every surface sends a refusal's detail to the caller (the query registry's
// REST body and socket frame, `/worklet`'s own), so the probe's error in it
// would hand a driver's message or a database path to anyone holding the
// question's grant. The refusal says what could not be read; the reason goes to
// the log. The control, a read declaring no scope, is told in its own words.
func TestAnUnreadableDeferredScopeIsRefusedInThisPackagesWords(t *testing.T) {
	t.Parallel()
	deferredHealth := func() statelog.Health {
		s := healthy()
		s.Deferred = 1
		s.DeferredFrom = 1
		return s
	}
	storeWords := "open /var/lib/crewlet/replicated.db: database is locked"
	for _, c := range []struct {
		name   string
		store  *stubStore
		scope  statelog.ScopeSet
		want   string
		absent string
	}{
		{"a store that could not be read",
			&stubStore{err: errors.New(storeWords)},
			statelog.ScopeSet{Paths: []string{"object/a"}},
			"could not be read", "/var/lib"},
		// A REAL ESTATE, because the scope is resolved inside the
		// probe's own transaction — a read by reference has none until
		// its rows are read — so the no-scope answer is reached there.
		{"a read that names no objects, the control",
			newStubStore(t), statelog.ScopeSet{},
			"declares no scope", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newReader(t, c.store, deferredHealth,
				&stubWaiter{at: healthy().Position}, nil)
			_, err := r.Read(t.Context(), statelog.Query{
				Level: statelog.ReadStale, Scope: c.scope,
			}, func(*sql.Tx) error { return nil })
			var refusal *statelog.Refused
			if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseDeferredScopeUnknown {
				t.Fatalf("Read = %v, want a deferred_scope_unknown refusal", err)
			}
			if !strings.Contains(refusal.Detail, c.want) {
				t.Errorf("detail = %q, want it to say %q", refusal.Detail, c.want)
			}
			if c.absent != "" && strings.Contains(err.Error(), c.absent) {
				t.Errorf("the refusal carries the store's own words to the "+
					"caller: %q", err)
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
	index, err := statelog.NewReadIndex(probeDomain{}, specOf(probeDomain{}),
		&countingAppends{inner: broker.log, n: &appends}, broker.log.Members, testSigner(t, probeDomain{}), noCeiling(t), probeEncode, broker.gen.Load, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}
	r := newReader(t, h.estate, deferredHealth,
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

	// AND THE SAME BOUND IN RECORDS, the broker's own reading, from which
	// the duration above is derived through this node's drain rate: a
	// record bound validated and then ignored bounds nothing.
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

// A READINESS NOBODY COULD READ REFUSES `broker_unreachable`, NAMING WHY, at
// every level, and after a wait as well as before one.
//
// The provider's failure is its own return, not a sentence in
// [statelog.Health.Err]: folded into a zero health, the zero floor beside it
// would be refused first as a floor never read, sending an operator to
// coordination for a stream leader that stopped answering.
func TestAnUnreadableReadinessRefusesBrokerUnreachable(t *testing.T) {
	t.Parallel()
	const why = "the stream's leader does not answer"
	refusedFor := func(t *testing.T, err error, level statelog.ReadLevel) {
		t.Helper()
		var refusal *statelog.Refused
		if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseBrokerUnreachable ||
			!strings.Contains(refusal.Detail, why) {
			t.Fatalf("a %s read over an unreadable readiness = %v, want %q naming %q",
				level, err, statelog.RefuseBrokerUnreachable, why)
		}
	}
	unread := func() (statelog.Health, error) { return statelog.Health{}, errors.New(why) }
	db := newStubStore(t)
	r, err := statelog.NewReader(statelog.ReaderDeps{
		Domain: probeDomain{}, Spec: specOf(probeDomain{}), Mode: statelog.ModeNormal,
		DB: db, Waiter: &stubWaiter{at: healthy().Position}, Health: unread,
		Drain: func() float64 { return 2000 },
	})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	for _, level := range statelog.ReadLevels {
		_, err := r.Read(t.Context(), pointQuery(level), func(*sql.Tx) error { return nil })
		refusedFor(t, err, level)
	}
	if got := db.reads.Load(); got != 0 {
		t.Fatalf("reads over an unreadable readiness opened %d transaction(s)", got)
	}

	// AFTER A WAIT: the readiness answered before it and not after.
	var asked atomic.Int64
	waited := func() (statelog.Health, error) {
		if asked.Add(1) == 1 {
			return healthy(), nil
		}
		return unread()
	}
	w := &stubWaiter{at: healthy().Position}
	r, err = statelog.NewReader(statelog.ReaderDeps{
		Domain: probeDomain{}, Spec: specOf(probeDomain{}), Mode: statelog.ModeNormal,
		DB: db, Waiter: w, Health: waited, Drain: func() float64 { return 2000 },
	})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	q := pointQuery(statelog.ReadSession)
	q.Session = statelog.Position{Stream: probeStream, Generation: 1, Seq: 42}
	_, err = r.Read(t.Context(), q, func(*sql.Tx) error { return nil })
	if w.waited.Load() != 1 {
		t.Fatalf("the session read waited %d time(s), want once — the case is "+
			"about a readiness read after a wait", w.waited.Load())
	}
	refusedFor(t, err, statelog.ReadSession)
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
		inner: h.estate,
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
// A write answers with the position its record landed at, and a caller handing
// it back as [statelog.Query.Floors] is served nothing from before it at any
// level: a `stale` read waits for it, and a `linearizable` one too, since its
// own barrier is past the floor only when the position is honest.
func TestTheCallersFloorIsHonouredAtEveryLevel(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var appends atomic.Int64
	index, err := statelog.NewReadIndex(probeDomain{}, specOf(probeDomain{}),
		&countingAppends{inner: h.log, n: &appends}, h.log.Members, testSigner(t, probeDomain{}), noCeiling(t), probeEncode, h.gen.Load, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}
	// FAR PAST ANYTHING A BARRIER ESTABLISHES, so a level that waited for
	// its own target instead of the floor is told apart from one that
	// waited for the later of the two.
	floor := statelog.Position{Stream: probeStream, Generation: 1, Seq: 1 << 30}

	db := newStubStore(t)
	for _, level := range []statelog.ReadLevel{
		statelog.ReadLinearizable, statelog.ReadSession,
		statelog.ReadStale, statelog.ReadConsistentPrefix,
	} {
		w := &stubWaiter{at: healthy().Position}
		r := newReader(t, db, healthy, w, index)
		q := pointQuery(level)
		q.Floors = []statelog.Position{floor}
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
	r := newReader(t, db, healthy, w, index)
	q := pointQuery(statelog.ReadStale)
	q.Floors = []statelog.Position{{Stream: "SOME_OTHER_LOG", Generation: 1, Seq: 5}}
	_, err = r.Read(t.Context(), q, func(*sql.Tx) error { return nil })
	if !errors.Is(err, statelog.ErrForeignPosition) {
		t.Fatalf("a stale read with a floor on another stream = %v, want %v",
			err, statelog.ErrForeignPosition)
	}
	if got := w.waited.Load(); got != 0 {
		t.Errorf("the wrong-stream read waited %d time(s) before refusing", got)
	}
}

// A FLOOR ON ANOTHER STREAM IS REFUSED AT EVERY LEVEL, INCLUDING WHEN IT
// SORTS LOW, AND AS THE REQUEST'S MISTAKE, NOT AS A STATE OF THE NODE.
//
// [statelog.Position.Packed] carries no stream, so taking the maximum of a
// foreign floor and a local target first would drop a foreign floor sorting
// below it before the stream check saw it; the two floors sit either side of
// the barrier and must answer alike.
//
// The answer is [statelog.ErrForeignPosition], never a [statelog.Refused] such
// as `wrong_stream`, which sends a client to another node refusing it alike.
// Every node refuses it, so before this node's own state is consulted: an
// EVICTED node answers the request's mistake too.
func TestAFloorOnAnotherStreamIsRefusedAtEveryLevel(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	index, err := statelog.NewReadIndex(probeDomain{}, specOf(probeDomain{}), h.log, h.log.Members, testSigner(t, probeDomain{}), noCeiling(t), probeEncode, h.gen.Load, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}
	db := newStubStore(t)
	evicted := func() statelog.Health {
		out := healthy()
		out.Evicted = true
		return out
	}
	for _, health := range []struct {
		name string
		fn   func() statelog.Health
	}{{"healthy", healthy}, {"evicted", evicted}} {
		for _, floor := range []statelog.Position{
			{Stream: "SOME_OTHER_LOG", Generation: 1, Seq: 1},
			{Stream: "SOME_OTHER_LOG", Generation: 1, Seq: 1 << 30},
		} {
			for _, level := range []statelog.ReadLevel{
				statelog.ReadLinearizable, statelog.ReadSession,
				statelog.ReadStale, statelog.ReadConsistentPrefix,
			} {
				w := &stubWaiter{at: healthy().Position}
				r := newReader(t, db, health.fn, w, index)
				q := pointQuery(level)
				q.Floors = []statelog.Position{floor}
				_, err := r.Read(t.Context(), q, func(*sql.Tx) error { return nil })
				if !errors.Is(err, statelog.ErrForeignPosition) {
					t.Errorf("a %s read floored at %s on a node that is %s "+
						"answered %v, want %v", level, floor, health.name, err,
						statelog.ErrForeignPosition)
					continue
				}
				if errors.Is(err, statelog.ErrUnavailable) {
					t.Errorf("a %s read floored at %s answered %v, which is a "+
						"refusal — the caller's mistake reported as this "+
						"node's state", level, floor, err)
				}
				if got := w.waited.Load(); got != 0 {
					t.Errorf("a %s read floored at %s waited %d time(s) before "+
						"refusing — a sequence from another log is a caller bug "+
						"rather than a position this node can reach", level, floor, got)
				}
			}
		}
	}
	// THE CONTROL: a floor on this read's own log is no mistake, and the
	// evicted node answers it with its own refusal.
	r := newReader(t, &stubStore{}, evicted, &stubWaiter{at: healthy().Position}, index)
	q := pointQuery(statelog.ReadStale)
	q.Floors = []statelog.Position{{Stream: probeStream, Generation: 1, Seq: 1}}
	_, err = r.Read(t.Context(), q, func(*sql.Tx) error { return nil })
	var refusal *statelog.Refused
	if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseEvicted {
		t.Errorf("an evicted node's read floored on its own log answered %v, "+
			"want its evicted refusal", err)
	}
}

// THE STALENESS BOUND IS ENFORCED AGAINST THE ANSWER'S OWN MOMENT, NOT A
// SNAPSHOT FROM BEFORE THE WAIT.
//
// A read may wait up to the read budget for the caller's floor, and reaching a
// position says nothing about how far the log ran on meanwhile, so the bound
// is checked, and the lag reported, after the wait: the figure and the rows
// describe one moment.
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
	q.Floors = []statelog.Position{{Stream: probeStream, Generation: 1, Seq: 42}}
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
	within.Floors = q.Floors
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

func (w *landingWaiter) WaitReached(ctx context.Context, p statelog.Position) error {
	if w.land != nil {
		w.land()
	}
	return w.stubWaiter.WaitReached(ctx, p)
}

// A DEFERRAL LANDING DURING THE WAIT IS CAUGHT EVEN WHEN NOTHING WAS DEFERRED
// WHEN THE READ BEGAN.
//
// The applier crosses such a record to reach the position the read waits for,
// and a healthy node, holding no deferral when the read began, is the only kind
// that meets a newer peer's record first. So the answer's own transaction
// probes whatever the health read before the wait said, or a point read
// certifies rows the record already made wrong.
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
			r := newReader(t, h.estate, healthy, waiter, nil)
			q := statelog.Query{
				Level:  statelog.ReadSession,
				Scope:  statelog.ScopeSet{Paths: []string{"project/ENG/object/b"}},
				Floors: []statelog.Position{healthy().Position},
				Set:    set,
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
// A path formed from the reference itself (a key read as an id, an object with
// no container) is filed under nothing, so the probe would pass every
// deferral. The resolver reads the rows in the answer's own transaction, and
// the probe asks about what it found.
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
	r := newReader(t, h.estate, deferred, &stubWaiter{at: healthy().Position}, nil)
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

// A NODE THAT PUBLISHES NOTHING APPENDS NO BARRIER.
//
// A barrier is a record on the log, and a node in maintenance or seal mode
// exists so the logs hold still while a capacity window measures them (a
// seal-mode acknowledgement is evidence because nothing on that node writes).
// So a linearizable read there is refused `maintenance`, which neither a wait
// nor another node clears (every node is in the window), while every level
// appending nothing is answered.
// The reader cannot be built with a read index, nor without naming its mode.
func TestANodeThatPublishesNothingAppendsNoBarrier(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var appends atomic.Int64
	index, err := statelog.NewReadIndex(probeDomain{}, specOf(probeDomain{}),
		&countingAppends{inner: h.log, n: &appends}, h.log.Members, testSigner(t, probeDomain{}), noCeiling(t),
		probeEncode, h.gen.Load, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}
	noop := func(*sql.Tx) error { return nil }
	deps := func(mode statelog.MaintenanceMode, index *statelog.ReadIndex) statelog.ReaderDeps {
		return statelog.ReaderDeps{
			Domain: probeDomain{}, Spec: specOf(probeDomain{}), Mode: mode,
			DB: newStubStore(t), Index: index, Health: readable(healthy),
			Waiter: &stubWaiter{at: healthy().Position},
		}
	}
	for _, mode := range []statelog.MaintenanceMode{statelog.ModeMaintenance, statelog.ModeSeal} {
		if _, err := statelog.NewReader(deps(mode, index)); err == nil {
			t.Errorf("a reader in %s mode was built over a read index, which "+
				"appends", mode)
		}
		r, err := statelog.NewReader(deps(mode, nil))
		if err != nil {
			t.Fatalf("NewReader in %s mode: %v", mode, err)
		}
		_, err = r.Read(t.Context(), pointQuery(statelog.ReadLinearizable), noop)
		var refusal *statelog.Refused
		if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseMaintenance {
			t.Fatalf("a linearizable read in %s mode answered %v, want %q",
				mode, err, statelog.RefuseMaintenance)
		}
		if !refusal.Code.Valid() {
			t.Errorf("%q is not a code this build names, so a peer reading it "+
				"off the wire could not tell it from a newer build's", refusal.Code)
		}
		if refusal.Code.Retryable() || refusal.RetryAfter != 0 {
			t.Errorf("the refusal invites a retry (%v, %s) that only the fleet's "+
				"restart into normal mode can answer", refusal.Code.Retryable(),
				refusal.RetryAfter)
		}
		if !strings.Contains(refusal.Detail, string(mode)) {
			t.Errorf("the refusal does not name the mode: %q", refusal.Detail)
		}
		session := pointQuery(statelog.ReadSession)
		session.Session = healthy().Position
		for _, q := range []statelog.Query{
			pointQuery(statelog.ReadStale), pointQuery(statelog.ReadConsistentPrefix), session,
		} {
			if _, err := r.Read(t.Context(), q, noop); err != nil {
				t.Errorf("a %s read in %s mode, which appends nothing, answered %v",
					q.Level, mode, err)
			}
		}
	}
	if got := appends.Load(); got != 0 {
		t.Fatalf("a node that publishes nothing appended %d barrier(s)", got)
	}

	// THE CONTROL: the same index, in the mode that publishes, appends.
	r := newReader(t, newStubStore(t), healthy, &stubWaiter{at: healthy().Position}, index)
	if _, err := r.Read(t.Context(), pointQuery(statelog.ReadLinearizable), noop); err != nil {
		t.Fatalf("a linearizable read in normal mode: %v", err)
	}
	if got := appends.Load(); got != 1 {
		t.Fatalf("a linearizable read in normal mode appended %d barrier(s), want 1", got)
	}

	// AND A READER WITH NO MODE IS REFUSED rather than read as either.
	if _, err := statelog.NewReader(deps("", nil)); err == nil {
		t.Error("a reader was built without a mode")
	}
}
