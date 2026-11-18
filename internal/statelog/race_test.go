package statelog_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/statelog"
)

// purgeBelow trims the log as the retention duty does, by sequence and
// stream-wide, which leaves a quiet object's subject holding nothing.
func (h *harness) purgeBelow(seq uint64) {
	h.t.Helper()
	ctx := h.t.Context()
	j, err := natsjs.New(h.q.Conn())
	if err != nil {
		h.t.Fatalf("open a jetstream context: %v", err)
	}
	s, err := j.Stream(ctx, probeStream)
	if err != nil {
		h.t.Fatalf("open the log: %v", err)
	}
	if err := s.Purge(ctx, natsjs.WithPurgeSequence(seq)); err != nil {
		h.t.Fatalf("purge below %d: %v", seq, err)
	}
}

// CONCURRENT WRITERS RACE ONE OBJECT AND EXACTLY ONE WINS EACH ROUND.
//
// Only the broker sees every writer, so it checks the expectation; this
// asserts what makes that safe: every write lands exactly once or is told it
// did not, each at its own position in the one replay order.
func TestConcurrentWritersRaceOneObject(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	const writers = 8
	var wg sync.WaitGroup
	results := make([]statelog.Result, writers)
	errs := make([]error, writers)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = h.write(probeSubject("a"), fmt.Sprintf("op-%d", i),
				fmt.Sprintf("body-%d", i))
		}()
	}
	wg.Wait()

	seen := map[uint64]int{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
		if results[i].Outcome != statelog.OutcomeApplied {
			t.Fatalf("writer %d: outcome = %q, want applied", i, results[i].Outcome)
		}
		if prev, dup := seen[results[i].Position.Seq]; dup {
			t.Fatalf("writers %d and %d both landed at sequence %d — the whole "+
				"concurrency control is that they cannot", prev, i,
				results[i].Position.Seq)
		}
		seen[results[i].Position.Seq] = i
	}
	if len(seen) != writers {
		t.Fatalf("%d writers produced %d distinct positions", writers, len(seen))
	}
}

// A TRIMMED ANCHOR RETRIES ONCE AT ZERO, AND ONLY THROUGH THE FENCE.
//
// The server rescues an empty subject only for an expectation of ZERO, so once
// a quiet object's only commit is trimmed, its row's anchor is refused FOR EVER
// without the retry (package doc, "THE FLOOR THEOREM").
func TestWriteImmediatelyAfterTrimAdvances(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	// One commit on a quiet object, traffic elsewhere, then a trim past all
	// of it: the retention duty's shape.
	quiet, err := h.write(probeSubject("quiet"), "op-quiet", "once")
	if err != nil {
		t.Fatalf("the quiet object's only write: %v", err)
	}
	for i := range 5 {
		if _, err := h.write(probeSubject("busy"), fmt.Sprintf("op-busy-%d", i), "x"); err != nil {
			t.Fatalf("busy write %d: %v", i, err)
		}
	}
	h.purgeBelow(quiet.Position.Seq + 3)

	// The row keeps its old anchor; the subject holds nothing.
	h.anchorAt(probeSubject("quiet"), quiet.Position.Seq)
	h.fence.zeroes.Store(0)
	before := h.appends.appends.Load()

	res, err := h.write(probeSubject("quiet"), "op-quiet-2", "again")
	if err != nil {
		t.Fatalf("the write after the trim: %v — a quiet object whose anchor was "+
			"trimmed must stay writable", err)
	}
	if res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("outcome = %q, want applied", res.Outcome)
	}
	if got := h.fence.zeroes.Load(); got != 1 {
		t.Fatalf("the expectation-zero fence ran %d time(s), want exactly 1 — the "+
			"retry-at-zero branch is safe only because it is verified WITHIN the "+
			"call, and a cadence that checks it later leaves a window that admits "+
			"a lost update", got)
	}
	if attempts := h.appends.appends.Load() - before; attempts != 2 {
		t.Fatalf("the write made %d append attempt(s), want 2: one refused under "+
			"the trimmed anchor and one retried at zero", attempts)
	}
}

// AND THE FENCE IS WHAT STANDS BETWEEN THAT RETRY AND A LOST UPDATE.
//
// Below the published trim floor a node's rows may be stale, and an
// expectation of zero on an emptied subject succeeds over a peer's write
// rather than losing to it, so such a node is refused instead.
func TestABelowFloorNodeIsRefusedRatherThanRetryingAtZero(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	quiet, err := h.write(probeSubject("quiet"), "op-quiet", "once")
	if err != nil {
		t.Fatalf("the quiet object's only write: %v", err)
	}
	h.purgeBelow(quiet.Position.Seq + 1)
	h.anchorAt(probeSubject("quiet"), quiet.Position.Seq)
	h.fence.zeroErr = &statelog.Unavailable{
		Reason: statelog.ReasonBelowFloor,
		Detail: "this node is below the published trim floor",
	}
	before := h.appends.appends.Load()

	_, err = h.write(probeSubject("quiet"), "op-quiet-2", "again")
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonBelowFloor {
		t.Fatalf("write = %v, want a below_floor refusal", err)
	}
	// ONE attempt, the refused one: the retry never reaches the broker.
	if attempts := h.appends.appends.Load() - before; attempts != 1 {
		t.Fatalf("the write made %d append attempt(s) below the floor, want 1 — "+
			"the retry at zero must never reach the broker", attempts)
	}
}

// THE ZERO FENCE IS ASKED ABOUT THE CHECKPOINT THE DECISION WAS MADE AT, on
// both roads to an expectation of zero.
//
// The C the fence verifies F <= C+1 against must be the position the decided
// rows were at (package doc, "THE FLOOR THEOREM"). The applier moves forward
// while a write runs, so its live position is too permissive: a record applied
// after the snapshot and trimmed before the check passes, and the decision
// published at zero overwrites it. The staging applies one in between.
func TestTheZeroFenceIsAskedAboutTheCheckpointTheDecisionRead(t *testing.T) {
	t.Parallel()
	for name, stage := range map[string]func(h *harness) statelog.Subject{
		// No anchor and nothing on the broker: the expectation is zero,
		// fenced before the first append.
		"a subject nobody has written": func(h *harness) statelog.Subject {
			if _, err := h.write(probeSubject("busy"), "op-busy", "x"); err != nil {
				h.t.Fatalf("a write elsewhere: %v", err)
			}
			return probeSubject("fresh")
		},
		// An anchor the trim removed: the append at it is refused, and the
		// retry at zero publishes the same snapshot's decision.
		"an anchor the trim removed": func(h *harness) statelog.Subject {
			quiet, err := h.write(probeSubject("quiet"), "op-quiet", "once")
			if err != nil {
				h.t.Fatalf("the quiet object's only write: %v", err)
			}
			for i := range 3 {
				if _, err := h.write(probeSubject("busy"), fmt.Sprintf("op-busy-%d", i), "x"); err != nil {
					h.t.Fatalf("busy write %d: %v", i, err)
				}
			}
			h.purgeBelow(quiet.Position.Seq + 2)
			h.anchorAt(probeSubject("quiet"), quiet.Position.Seq)
			return probeSubject("quiet")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			subject := stage(h)

			decided := h.applier.Committed()
			moved := statelog.Position{
				Stream: probeStream, Generation: decided.Generation, Seq: decided.Seq + 7,
			}
			var once sync.Once
			h.rows.mu.Lock()
			h.rows.afterSnapshot = func() { once.Do(func() { h.applier.advance(moved) }) }
			h.rows.mu.Unlock()

			before := len(h.fence.asked())
			if _, err := h.write(subject, "op-after", "again"); err != nil {
				t.Fatalf("the write: %v", err)
			}
			asked := h.fence.asked()[before:]
			if len(asked) == 0 {
				t.Fatal("the write never reached the expectation-zero fence, so this " +
					"case stages nothing")
			}
			if asked[0] != decided {
				t.Fatalf("the fence was asked about %s, want %s — the checkpoint the "+
					"decision's rows were at; %s is where the applier moved after "+
					"the snapshot, and a record in between is one the decision "+
					"published at zero never saw", asked[0], decided, moved)
			}
		})
	}
}

// THE ZERO FENCE READS ITS BOUNDS ONLY AFTER THE LOG HAS SAID THE SUBJECT IS
// EMPTY, on both roads to an expectation of zero.
//
// A node applies past its own snapshot while a write runs, so a peer's record
// R above the snapshot's checkpoint can be applied here, licensed (the applied
// term) and purged before the write learns the subject is empty. The licensing
// floor is published before the purge, so a fence reading the floor and the
// log's first sequence after LastSeq refuses; reading them before, it clears
// and the write lands at zero over R. Each case purges R as the publisher asks for LastSeq,
// the one instant that tells the two orders apart.
func TestTheZeroFenceReadsItsBoundsOnlyAfterTheLogSaysTheSubjectIsEmpty(t *testing.T) {
	t.Parallel()
	subject := probeSubject("contended")
	wire := probePrefix + "." + subject.String()
	for name, stage := range map[string]func(t *testing.T, h *harness){
		// Nothing applied on the subject: LastSeq forms the zero.
		"a subject this node has never written": func(*testing.T, *harness) {},
		// The row names this node's own record and R lands above it, so the
		// append is refused and LastSeq tells a trimmed anchor from a lost race.
		"an anchor the broker refuses": func(t *testing.T, h *harness) {
			own, err := h.write(subject, "op-own", "mine")
			if err != nil {
				t.Fatalf("this node's own write: %v", err)
			}
			h.anchorAt(subject, own.Position.Seq)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			stage(t, h)
			decided := h.applier.Committed()

			// A PEER'S RECORD this node has not applied when it decides.
			r, _, err := h.log.Append(t.Context(), wire, "peer-op", nil,
				signedByPeer(t, h, probeRecord(statelog.Stamp{Gen: h.gen.Load(), Writer: "node-b"}, subject, statelog.ScopeSet{Paths: []string{subject.String()}}, "peer-op", "peer")))
			if err != nil {
				t.Fatalf("the peer's write: %v", err)
			}
			if r <= decided.Seq {
				t.Fatalf("the peer's record landed at %d, not above the decision's "+
					"checkpoint %d", r, decided.Seq)
			}

			// THE REAL ZERO FENCE, over a published floor and the log's ends
			// as they stand when it reads them.
			var floor atomic.Uint64
			h.fence.zero = &statelog.ZeroFence{
				Evicted: func(context.Context) (bool, error) { return false, nil },
				Floor: func(context.Context, uint32) (uint64, error) {
					return floor.Load(), nil
				},
				Ends: func(ctx context.Context) (statelog.LogEnds, error) {
					first, last, err := h.log.Bounds(ctx)
					return statelog.LogEnds{First: first, Last: last}, err
				},
				Committed: h.applier.Committed,
			}

			// AND THE TRIM, as the write asks what the subject holds: this node
			// applies R, a tick publishes a floor past R, then purges below it.
			var once sync.Once
			h.appends.mu.Lock()
			h.appends.beforeLastSeq = func() {
				once.Do(func() {
					h.applier.advance(statelog.Position{
						Stream: probeStream, Generation: decided.Generation, Seq: r,
					})
					floor.Store(r + 1)
					h.purgeBelow(r + 1)
				})
			}
			h.appends.mu.Unlock()

			before := len(h.appends.expectations())
			_, err = h.write(subject, "op-after", "decided without R")
			if !errors.Is(err, statelog.ErrUnavailable) || errors.Is(err, statelog.ErrConflict) {
				t.Fatalf("write = %v, want the zero fence's refusal — R was purged "+
					"after this node's snapshot, and the floor that licensed it "+
					"was published first", err)
			}
			for _, expect := range h.appends.expectations()[before:] {
				if expect != nil && *expect == 0 {
					t.Fatalf("appended at an expectation of zero over R at %d, which "+
						"the decision never read", r)
				}
			}
			first, last, err := h.log.Bounds(t.Context())
			if err != nil {
				t.Fatalf("read the log's bounds: %v", err)
			}
			if first != r+1 {
				t.Fatalf("the log starts at %d, want %d — the trim never ran as the "+
					"write asked for the subject, so this case staged nothing", first, r+1)
			}
			if last != r {
				t.Fatalf("the log ends at %d, want %d — a record landed over R", last, r)
			}
		})
	}
}

// AN OLD GENERATION'S ANCHOR CANNOT EXIST ON A RECREATED STREAM, and the
// probe says so, not a pre-filter.
//
// On a rebuilt broker estate every stored sequence is from another space, and
// a first-sequence pre-filter reads TRUE for every stale row (the new stream
// starts at zero), handing a writer an expectation refused for ever.
func TestOldVersionAgainstARecreatedStream(t *testing.T) {
	t.Parallel()

	t.Run("an empty subject publishes at zero, fenced", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		// The row is from generation 1; generation 2's stream holds
		// nothing for this object.
		h.gen.Store(2)
		h.rows.stage(probeSubject("a"),
			statelog.Position{Stream: probeStream, Generation: 1, Seq: 4_000})

		res, err := h.write(probeSubject("a"), "op-1", "hello")
		if err != nil {
			t.Fatalf("the first write after a reanchor: %v", err)
		}
		if res.Outcome != statelog.OutcomeApplied {
			t.Fatalf("outcome = %q, want applied", res.Outcome)
		}
		if res.Position.Generation != 2 {
			t.Fatalf("the record landed at generation %d, want 2 — a successful "+
				"commit stamps the CURRENT generation onto the row, which is what "+
				"makes the migration lazy and free", res.Position.Generation)
		}
		if got := h.fence.zeroes.Load(); got != 1 {
			t.Fatalf("the expectation-zero fence ran %d time(s), want exactly 1", got)
		}
	})

	t.Run("a peer's write in this generation is waited for, never overwritten", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.gen.Store(2)
		peer, _, err := h.log.Append(t.Context(), probePrefix+".object.a", "peer-op", nil,
			signedByPeer(t, h, probeRecord(statelog.Stamp{Gen: h.gen.Load(), Writer: "node-b"}, probeSubject("a"), statelog.ScopeSet{Paths: []string{"object.a"}}, "peer-op", "peer")))
		if err != nil {
			t.Fatalf("the peer's write: %v", err)
		}
		// The row is from generation 1, and a record this node has not
		// applied occupies the subject in generation 2.
		h.rows.stage(probeSubject("a"),
			statelog.Position{Stream: probeStream, Generation: 1, Seq: 4_000})
		h.applier.advance(statelog.Position{Stream: probeStream, Generation: 2})

		res, err := h.write(probeSubject("a"), "op-1", "mine")
		if err != nil {
			t.Fatalf("write against a peer's record in this generation: %v", err)
		}
		if res.Position.Seq <= peer {
			t.Fatalf("this write landed at %d, at or below the peer's %d — a "+
				"stale-generation row whose subject IS occupied must wait for "+
				"the peer rather than publish at zero", res.Position.Seq, peer)
		}
		if got := h.fence.zeroes.Load(); got != 0 {
			t.Fatalf("published at zero %d time(s) over an occupied subject", got)
		}
	})
}

// A LOST ACKNOWLEDGEMENT IS NEVER A SUCCESS, and which of the other two it is
// rests on facts this node reads locally, never a guess.
func TestLostPubAckIsUnknownNotSuccess(t *testing.T) {
	t.Parallel()

	t.Run("a lagging node answers pending, because the record is durable", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		first, err := h.write(probeSubject("a"), "op-0", "one")
		if err != nil {
			t.Fatalf("the first write: %v", err)
		}
		h.anchorAt(probeSubject("a"), first.Position.Seq)

		// The record LANDS, the acknowledgement does not, and this node's
		// applier never reaches it.
		h.applier.mu.Lock()
		h.applier.auto = false
		h.applier.mu.Unlock()
		h.appends.fail(errors.New("no response from stream"), false)

		res, err := h.write(probeSubject("a"), "op-1", "two")
		if err != nil {
			t.Fatalf("write with a lost acknowledgement: %v", err)
		}
		if res.Outcome != statelog.OutcomePending {
			t.Fatalf("outcome = %q, want pending — the record is on the stream "+
				"and every other node will apply it, so neither success nor "+
				"failure is the truth", res.Outcome)
		}
		if res.Position.Seq <= first.Position.Seq {
			t.Fatalf("pending named position %s, which is not the record's",
				res.Position)
		}
	})

	t.Run("an operation minted before a sweep during its own write answers unknown", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		first, err := h.write(probeSubject("a"), "op-0", "one")
		if err != nil {
			t.Fatalf("the first write: %v", err)
		}
		h.anchorAt(probeSubject("a"), first.Position.Seq)
		decided := h.rows.snapshots()

		// The record lands, the acknowledgement is lost, this node applies
		// past it, and its ledger is EMPTY: its sweep ran DURING the write,
		// after the decision's check, with a cutoff past the operation's
		// mint instant (an old-instant id: turn-derived, or a late retry).
		// And a PEER's record lands above it before the subject is asked,
		// so nothing on the log says whether this one landed below it.
		h.applier.mu.Lock()
		h.applier.auto = false
		h.applier.mu.Unlock()
		h.applier.advance(statelog.Position{Stream: probeStream, Generation: 1, Seq: 1_000})
		h.appends.fail(errors.New("no response from stream"), false)
		minted := time.Now()
		h.appends.mu.Lock()
		h.appends.beforeLastSeq = func() {
			h.sweep(minted.Add(time.Hour))
			if _, _, err := h.log.Append(t.Context(), probePrefix+".object.a", "peer-op", nil,
				signedByPeer(t, h, probeRecord(statelog.Stamp{Gen: 1, Writer: "node-b"}, probeSubject("a"), statelog.ScopeSet{Paths: []string{"object.a"}},
					"peer-op", "peer"))); err != nil {
				t.Errorf("the peer's write: %v", err)
			}
		}
		h.appends.mu.Unlock()

		op := statelog.NewOpID(minted, "write-a")
		res, err := h.write(probeSubject("a"), op, "two")
		if err != nil {
			t.Fatalf("write across a sweep: %v", err)
		}
		if res.Outcome != statelog.OutcomeUnknown {
			t.Fatalf("outcome = %q, want unknown — reading the ledger's silence "+
				"as \"somebody else won\" would re-decide against a row that "+
				"moved because of this very write", res.Outcome)
		}
		if got := h.rows.snapshots() - decided; got != 1 {
			t.Fatalf("the write decided %d time(s), want once — the resolution "+
				"of the landed record is what must answer, and a second decision "+
				"is the re-decide the unknown exists to prevent", got)
		}
		if res.Position != (statelog.Position{}) {
			t.Errorf("unknown named position %s — the newest record on the "+
				"subject is not known to be this write's, so an unknown names "+
				"none", res.Position)
		}
		if res.OpID != op {
			t.Error("unknown must carry the op id to retry under")
		}
		if !res.Position.IsZero() {
			t.Errorf("unknown named position %s: the record found there was "+
				"never acknowledged as this write's, and a caller reads a "+
				"position as where its own record landed", res.Position)
		}
	})

	t.Run("an operation minted before a sweep during its own write, held at another's record, answers unknown", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		first, err := h.write(probeSubject("a"), "op-0", "one")
		if err != nil {
			t.Fatalf("the first write: %v", err)
		}
		h.anchorAt(probeSubject("a"), first.Position.Seq)

		// As above, and THIS NODE'S EVICTION is below the peer's record.
		// A refusal would claim this write's append applies nowhere, which
		// is the ledger's silence read: an append landing below the
		// eviction applied, its row among those the sweep took.
		h.applier.mu.Lock()
		h.applier.auto = false
		h.applier.mu.Unlock()
		h.applier.advance(statelog.Position{Stream: probeStream, Generation: 1, Seq: 1_000})
		h.gates.holdWriter("node-a", statelog.ReasonEvicted)
		h.appends.fail(errors.New("no response from stream"), false)
		minted := time.Now()
		h.appends.mu.Lock()
		h.appends.beforeLastSeq = func() {
			h.sweep(minted.Add(time.Hour))
			if _, _, err := h.log.Append(t.Context(), probePrefix+".object.a", "peer-op", nil,
				signedByPeer(t, h, probeRecord(statelog.Stamp{Gen: 1, Writer: "node-b"},
					probeSubject("a"), statelog.ScopeSet{Paths: []string{"object.a"}},
					"peer-op", "peer"))); err != nil {
				t.Errorf("the peer's write: %v", err)
			}
		}
		h.appends.mu.Unlock()

		op := statelog.NewOpID(minted, "write-a")
		res, err := h.write(probeSubject("a"), op, "two")
		if err != nil {
			t.Fatalf("write = %v, want unknown — the ledger cannot vouch for this "+
				"operation, so nothing here can say its append applies nowhere; it "+
				"may have applied below this node's eviction", err)
		}
		if res.Outcome != statelog.OutcomeUnknown || !res.Unvouched || !res.Position.IsZero() ||
			res.OpID != op {
			t.Fatalf("result = %+v, want unknown, unvouched, under %s and naming no position",
				res, op)
		}
	})

	t.Run("a lost acknowledgement whose record is the newest answers applied from the record whatever the ledger lost", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		first, err := h.write(probeSubject("a"), "op-0", "one")
		if err != nil {
			t.Fatalf("the first write: %v", err)
		}
		h.anchorAt(probeSubject("a"), first.Position.Seq)
		decided := h.rows.snapshots()

		// As above, with no peer: the newest record on the subject carries
		// this operation's id, so the log itself says the write landed
		// there, whatever the ledger lost, and no gate dropped it.
		h.applier.mu.Lock()
		h.applier.auto = false
		h.applier.mu.Unlock()
		h.applier.advance(statelog.Position{Stream: probeStream, Generation: 1, Seq: 1_000})
		h.appends.fail(errors.New("no response from stream"), false)
		minted := time.Now()
		h.appends.mu.Lock()
		h.appends.beforeLastSeq = func() { h.sweep(minted.Add(time.Hour)) }
		h.appends.mu.Unlock()

		op := statelog.NewOpID(minted, "write-a")
		res, err := h.write(probeSubject("a"), op, "two")
		if err != nil {
			t.Fatalf("write across a sweep: %v", err)
		}
		landed := statelog.Position{Stream: probeStream, Generation: 1,
			Seq: uint64(h.appends.lastSeq.Load())}
		if res.Outcome != statelog.OutcomeApplied || res.Position != landed {
			t.Fatalf("result = %+v, want applied at %s — the record there carries "+
				"this operation's id, this node applied past it and no gate "+
				"dropped it", res, landed)
		}
		if !res.Collapsed {
			t.Error("the answer is not marked collapsed — a record this call " +
				"found rather than was acknowledged for is one it cannot prove " +
				"its own decision, and a caller computing its answer inside the " +
				"decision must not report it")
		}
		if got := h.rows.snapshots() - decided; got != 1 {
			t.Fatalf("the write decided %d time(s), want once", got)
		}
	})

	t.Run("an acknowledged record whose ledger row was lost is applied", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		// The loss shows from the SECOND watermark read on (the
		// resolution's): this node's sweep landing between the append and
		// its answer, after the decision's check.
		h.applier.mu.Lock()
		h.applier.lost = time.Now().Add(time.Hour)
		h.applier.lostFrom = 2
		h.applier.mu.Unlock()
		// This node's rows are past the record, and the sweep took its
		// ledger row.
		h.applier.mu.Lock()
		h.applier.auto = false
		h.applier.mu.Unlock()
		h.applier.advance(statelog.Position{Stream: probeStream, Generation: 1, Seq: 1_000})

		res, err := h.write(probeSubject("a"), statelog.NewOpID(time.Now(), "write-a"), "one")
		if err != nil {
			t.Fatalf("an acknowledged write whose row the ledger lost: %v — "+
				"the broker named this operation's own record and no gate dropped "+
				"it, so the missing row is the loss's doing and not an applier "+
				"that broke its contract", err)
		}
		if res.Outcome != statelog.OutcomeApplied || res.Position.Seq == 0 {
			t.Fatalf("outcome = %q at %s, want applied at the acknowledged position",
				res.Outcome, res.Position)
		}
	})
}

// A STREAM AND ITS REPLAY PROTOCOL MUST AGREE, and neither loop can detect
// its own mismatch.
//
// A strict loop over a compacted stream stalls for ever on the first ordinary
// write; a compacted loop over a log accepts a hole that is data loss. Both are
// quiet and permanent, so [statelog.StreamSpec.Validate] refuses the pairing.
func TestAStreamSpecAndItsReplayProtocolMustAgree(t *testing.T) {
	t.Parallel()
	base := func() statelog.StreamSpec { return specOf(probeDomain{}) }
	for name, tc := range map[string]struct {
		spec statelog.StreamSpec
		want string
	}{
		"a strict log is fine": {spec: base()},
		"a strict stream may not keep one per subject": {
			spec: func() statelog.StreamSpec { s := base(); s.MaxPerSubject = 1; return s }(),
			want: "per subject",
		},
		"a strict stream may not bound its age": {
			spec: func() statelog.StreamSpec { s := base(); s.MaxAge = time.Hour; return s }(),
			want: "max_age",
		},
		"a compacted stream must keep exactly one per subject": {
			spec: func() statelog.StreamSpec {
				s := base()
				s.Replay = statelog.ReplayCompacted
				return s
			}(),
			want: "keyed table",
		},
		"a compacted stream that does is fine": {
			spec: func() statelog.StreamSpec {
				s := base()
				s.Replay, s.MaxPerSubject, s.MaxAge = statelog.ReplayCompacted, 1, time.Hour
				return s
			}(),
		},
		"a protocol this build does not know is refused": {
			spec: func() statelog.StreamSpec { s := base(); s.Replay = "eventual"; return s }(),
			want: "declares replay",
		},
		"a log with no ceiling fills the volume": {
			spec: func() statelog.StreamSpec { s := base(); s.MaxBytes = 0; return s }(),
			want: "byte ceiling",
		},
		"and a prefix is declared, never derived": {
			spec: func() statelog.StreamSpec { s := base(); s.SubjectPrefix = ""; return s }(),
			want: "subject prefix",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.spec.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() accepted %+v", tc.spec)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
		})
	}
}

// EVERY TABLE A DOMAIN WRITES CARRIES A CLASS, and the four are exhaustive.
//
// The snapshot's scrub list, the identity claim's membership and the local
// sweep all derive from this one map, never a hardcoded list, which "would
// silently omit whatever a deployment actually has" (the backup package).
func TestEveryTableClassIsOneOfTheFour(t *testing.T) {
	t.Parallel()
	for table, class := range (probeDomain{}).Tables() {
		if !class.Valid() {
			t.Errorf("%s is classed %v, which is not one of the four", table, class)
		}
		if strings.HasPrefix(class.String(), "TableClass(") {
			t.Errorf("%s is classed %s, which has no name", table, class)
		}
	}
	// AND A VALUE OFF THE END IS REFUSED, not given whatever behaviour its
	// number falls into.
	if (statelog.TableClass(99)).Valid() {
		t.Error("TableClass(99) reports itself valid")
	}
}

// unusedCompileGuards fails the build here when a fake stops satisfying its
// interface, not at whichever test uses it next.
var (
	_ statelog.Domain   = probeDomain{}
	_ statelog.Rows     = (*fakeRows)(nil)
	_ statelog.Fence    = (*fakeFence)(nil)
	_ statelog.Gates    = (*fakeGates)(nil)
	_ statelog.Waiter   = (*applier)(nil)
	_ statelog.Appender = (*countingAppender)(nil)
)

// ledgerless is the one domain shape that may carry no operation ledger: an
// apply that is a total function under a monotone version guard, whose writer
// never reports a committed position to a caller.
type ledgerless struct{ probeDomain }

func (ledgerless) OpsTable() string { return "" }

// A DOMAIN WITH NO LEDGER IS ANSWERED FROM THE WAIT AND THE GATES ALONE.
//
// Otherwise every write reads its permanently empty table as "somebody else
// won" and republishes for sixteen rounds, then reports the object kept
// changing when nothing touched it.
func TestALedgerlessDomainResolvesFromItsOwnPosition(t *testing.T) {
	t.Parallel()
	h := newHarnessFor(t, ledgerless{})
	res, err := h.write(probeSubject("a"), "op-1", "hello")
	if err != nil {
		t.Fatalf("write on a ledgerless domain: %v", err)
	}
	if res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("outcome = %q, want applied", res.Outcome)
	}
	if res.Rounds != 1 {
		t.Fatalf("took %d rounds, want 1 — a ledgerless domain that re-decides "+
			"is one reading its own empty table as a lost race", res.Rounds)
	}

	// AND A GATE STILL DROPS ITS RECORDS: an evicted node's record
	// produces rows on no node, ledger or not.
	h.gates.gated, h.gates.reason = true, statelog.ReasonEvicted
	_, err = h.write(probeSubject("b"), "op-2", "hello")
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonEvicted {
		t.Fatalf("a gated write on a ledgerless domain = %v, want an evicted "+
			"refusal", err)
	}
}

// AN APPLIER THAT WRITES NO LEDGER ROW FOR AN ACKNOWLEDGED RECORD IS
// REPORTED, not guessed at.
//
// Acknowledged, applied past and not gated, the record applied and its ledger
// row is missing; re-deciding would republish a record that already landed,
// which the ledger exists to prevent.
func TestAnAppliedRecordWithNoLedgerRowIsReported(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// The applier commits but records nothing: a domain that forgot its
	// ledger write.
	h.applier.mu.Lock()
	h.applier.auto = false
	h.applier.mu.Unlock()
	h.applier.advance(statelog.Position{Stream: probeStream, Generation: 1, Seq: 1_000})

	_, err := h.write(probeSubject("a"), "op-1", "hello")
	if err == nil {
		t.Fatal("an applied record with no ledger row was accepted")
	}
	if !strings.Contains(err.Error(), "probe_ops") {
		t.Errorf("the error does not name the ledger table an operator has to "+
			"look in: %v", err)
	}
}
