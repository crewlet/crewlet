package statelog_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	natsjs "github.com/nats-io/nats.go/jetstream"
)

// purgeBelow trims the log the way the retention duty does — by sequence,
// stream-wide — which is what leaves a quiet object's subject holding
// nothing.
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
// The broker is the only party that sees every writer, which is why the
// expectation is checked there and not here. What this asserts is the
// property that makes that safe: every write either lands exactly once or is
// told it did not land, and the log ends up holding every record exactly once
// in an order every node will replay identically.
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
// The server evaluates a per-subject expectation by loading that subject's
// last message and rescues only the case where the expectation is ZERO. So
// once a quiet object's only commit is trimmed, an expectation above zero is
// refusable FOR EVER: the writer re-reads its row, gets the same number,
// retries, and is refused again. A quiet object becomes permanently
// unwritable.
func TestWriteImmediatelyAfterTrimAdvances(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	// One commit on a quiet object, then plenty of traffic elsewhere, then
	// a trim past all of it — which is exactly the shape the retention
	// duty produces.
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

	// The quiet object's row still records its old anchor, and its
	// subject now holds nothing.
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
// A node that cannot establish that it is at or above the published trim
// floor must not retry at zero: below the floor its rows may be stale, and an
// expectation of zero on an emptied subject succeeds against a peer's write
// rather than losing to it.
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
	// ONE attempt — the refused one. The retry never reaches the broker,
	// which is the assertion, stated as the absence of an append rather
	// than as the presence of an error.
	if attempts := h.appends.appends.Load() - before; attempts != 1 {
		t.Fatalf("the write made %d append attempt(s) below the floor, want 1 — "+
			"the retry at zero must never reach the broker", attempts)
	}
}

// AN OLD GENERATION'S ANCHOR CANNOT EXIST ON A RECREATED STREAM, and the
// probe is what says so rather than a cheap pre-filter.
//
// After an operator rebuilds a lost broker estate, every stored sequence is a
// number in a space it does not belong to. A first-sequence pre-filter reads
// TRUE for every stale row on the new stream — the stream starts at zero and
// zero is below everything — so it would hand a writer an expectation the
// broker refuses for ever.
func TestOldVersionAgainstARecreatedStream(t *testing.T) {
	t.Parallel()

	t.Run("an empty subject publishes at zero, fenced", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		// The row was last written in generation 1; the estate is now
		// on generation 2 and its stream holds nothing for this object.
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
		peer, _, err := h.log.Append(t.Context(), probePrefix+".object.a", "peer-op", nil, []byte("peer"))
		if err != nil {
			t.Fatalf("the peer's write: %v", err)
		}
		// The row was last written in generation 1, and the subject is
		// occupied in generation 2 by a record this node has not
		// applied.
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
// depends on facts this node reads locally rather than on a guess.
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

		// The record LANDS and the acknowledgement does not, and this
		// node's applier never reaches it.
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

	t.Run("an operation minted before this node adopted answers unknown", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		first, err := h.write(probeSubject("a"), "op-0", "one")
		if err != nil {
			t.Fatalf("the first write: %v", err)
		}
		h.anchorAt(probeSubject("a"), first.Position.Seq)

		// The record lands, the acknowledgement is lost, this node
		// applies past it — and its operation ledger is EMPTY, because
		// it adopted a donated snapshot after this operation was
		// minted and the ledger is scrubbed out of every one.
		h.applier.mu.Lock()
		h.applier.auto = false
		h.applier.mu.Unlock()
		h.gates.adopted = time.Now().Add(time.Hour)
		h.applier.advance(statelog.Position{Stream: probeStream, Generation: 1, Seq: 1_000})
		h.appends.fail(errors.New("no response from stream"), false)

		res, err := h.write(probeSubject("a"), "op-1", "two")
		if err != nil {
			t.Fatalf("write across an adoption: %v", err)
		}
		if res.Outcome != statelog.OutcomeUnknown {
			t.Fatalf("outcome = %q, want unknown — reading the ledger's silence "+
				"as \"somebody else won\" would re-decide against a row that "+
				"moved because of this very write", res.Outcome)
		}
		if res.OpID != "op-1" {
			t.Error("unknown must carry the op id to retry under")
		}
	})
}

// A STREAM AND ITS REPLAY PROTOCOL MUST AGREE, and neither loop can detect
// its own mismatch.
//
// A strict loop over a compacted stream stalls for ever on the first ordinary
// write; a compacted loop over a log accepts a hole that is data loss. Both
// failures are quiet and both are permanent, so the pairing is refused at the
// seam rather than inferred by whichever loop got there first.
func TestAStreamSpecAndItsReplayProtocolMustAgree(t *testing.T) {
	t.Parallel()
	base := func() statelog.StreamSpec { return probeDomain{}.Stream() }
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
// The framework derives the snapshot's scrub list, the identity claim's
// membership and the local sweep from this one map — which is the fix for the
// failure the backup package names in its own words, a hardcoded list that
// "would silently omit whatever a deployment actually has".
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
	// AND A VALUE OFF THE END IS REFUSED rather than rendered as
	// whichever behaviour its number happened to fall into.
	if (statelog.TableClass(99)).Valid() {
		t.Error("TableClass(99) reports itself valid")
	}
}

// unusedCompileGuards keeps the seams honest: a fake that stops satisfying an
// interface must fail the build here rather than at whichever test happens to
// use it next.
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
// Without this arm every one of its writes reads its own permanently empty
// table as "somebody else won" and republishes — for sixteen rounds, and then
// tells the caller its object kept changing when nothing touched it.
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

	// AND A GATE STILL DROPS ITS RECORDS. Having no ledger says nothing
	// about eviction: a record from an evicted node produces rows on no
	// node whatever the domain declares.
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
// The broker acknowledged the record, this node applied past it, and no gate
// dropped it — so it applied and the ledger row is missing. Re-deciding there
// would republish a record that already landed, which is the one thing the
// ledger exists to prevent.
func TestAnAppliedRecordWithNoLedgerRowIsReported(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// The applier commits but records nothing, which is what a domain
	// that forgot its ledger write looks like from here.
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
