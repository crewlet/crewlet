package statelog_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// AN OPERATION THIS NODE'S LEDGER CANNOT VOUCH FOR IS NEVER DECIDED A SECOND
// TIME.
//
// An adoption installs a donated snapshot whose operation ledger was scrubbed,
// so a retry of an operation minted before it — a turn re-run, a caller
// repeating an `unknown` under the same id — finds no row, takes a snapshot
// whose rows already hold the first application, and decides again on top of
// it. The broker has no reason to refuse that: the expectation is current, and
// the duplicate window is two minutes wide. So the refusal has to come from
// here, before the append, and it has to read the instant the operation was
// MINTED — which is the id's own, because every retry reuses the id and a
// retry's own clock is always after the adoption.
func TestAnOperationTheLedgerCannotVouchForIsNeverDecidedTwice(t *testing.T) {
	t.Parallel()

	t.Run("minted before an adoption and retried after it, it answers unknown and publishes nothing", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		minted := time.Now().Add(-time.Hour)
		op := statelog.NewOpID(minted, "write-a")
		first, err := h.write(probeSubject("a"), op, "one")
		if err != nil || first.Outcome != statelog.OutcomeApplied {
			t.Fatalf("the first application = (%+v, %v), want applied", first, err)
		}
		// THE ADOPTION, after the mint and before the retry: the
		// ledger arrives scrubbed.
		h.adopt(minted.Add(time.Minute))
		appended := h.appends.appends.Load()

		res, err := h.write(probeSubject("a"), op, "one")
		if err != nil {
			t.Fatalf("the retry: %v", err)
		}
		if res.Outcome != statelog.OutcomeUnknown {
			t.Fatalf("outcome = %q, want unknown — the ledger that would say "+
				"whether the first application landed was scrubbed by the "+
				"adoption, and deciding again on rows that already hold it "+
				"applies the operation twice", res.Outcome)
		}
		if got := h.appends.appends.Load() - appended; got != 0 {
			t.Fatalf("the retry appended %d record(s) — an operation the "+
				"ledger cannot vouch for must never reach the broker again", got)
		}
		if res.OpID != op {
			t.Errorf("unknown carries op id %q, want %q to retry under", res.OpID, op)
		}
	})

	t.Run("minted after the adoption, an absent row is conclusive and it publishes", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		adopted := time.Now().Add(-time.Hour)
		h.adopt(adopted)
		res, err := h.write(probeSubject("a"), statelog.NewOpID(adopted.Add(time.Second), "write-a"), "one")
		if err != nil || res.Outcome != statelog.OutcomeApplied {
			t.Fatalf("a first write minted after the adoption = (%+v, %v), want "+
				"applied — every copy of it lands above the artefact, so this "+
				"node's own applier writes its row and absence means \"not yet\"",
				res, err)
		}
	})

	t.Run("minted before the adoption, a row the ledger does hold still answers", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		minted := time.Now().Add(-time.Hour)
		op := statelog.NewOpID(minted, "write-a")
		first, err := h.write(probeSubject("a"), op, "one")
		if err != nil || first.Outcome != statelog.OutcomeApplied {
			t.Fatalf("the first application = (%+v, %v), want applied", first, err)
		}
		// An adoption the operation predates, whose artefact did NOT
		// cover the first copy: the copy landed above it, so this node's
		// own applier applied it and its row is here. The row speaks
		// for itself, and refusing it would strand a retry that this
		// node can answer.
		h.gates.mu.Lock()
		h.gates.adopted = minted.Add(time.Minute)
		h.gates.mu.Unlock()

		res, err := h.write(probeSubject("a"), op, "one")
		if err != nil {
			t.Fatalf("the retry: %v", err)
		}
		if res.Outcome != statelog.OutcomeApplied || res.Position != first.Position {
			t.Fatalf("the retry = %q at %s, want applied at the first "+
				"application's %s — the ledger holds the operation's row, so "+
				"it vouches whatever the operation predates", res.Outcome,
				res.Position, first.Position)
		}
	})

	// PAST THE BROKER'S DUPLICATE WINDOW, which is where the second
	// application stops being a claim and becomes a record on the log. The
	// window is the one thing that could collapse a retry without the
	// ledger, and it is an optimisation rather than a mechanism: a turn is
	// re-run after a crash or a redelivery backoff, or an operator repeats
	// an `unknown` from a terminal, long after two minutes.
	t.Run("past the duplicate window a retry is still not published again", func(t *testing.T) {
		t.Parallel()
		h := newHarnessFor(t, shortWindowDomain{})
		minted := time.Now().Add(-time.Hour)
		op := statelog.NewOpID(minted, "write-a")
		first, err := h.write(probeSubject("a"), op, "one")
		if err != nil || first.Outcome != statelog.OutcomeApplied {
			t.Fatalf("the first application = (%+v, %v), want applied", first, err)
		}
		h.adopt(minted.Add(time.Minute))
		time.Sleep(2 * shortWindow)

		res, err := h.write(probeSubject("a"), op, "one")
		if err != nil {
			t.Fatalf("the retry past the window: %v", err)
		}
		last, found, err := h.log.LastSeq(t.Context(), probePrefix+".object.a")
		if err != nil || !found {
			t.Fatalf("read the subject's last record: (%d, %v, %v)", last, found, err)
		}
		if last != first.Position.Seq || res.Outcome != statelog.OutcomeUnknown {
			t.Fatalf("the retry answered %q and the subject's last record is %d, "+
				"want unknown and still %d — the broker no longer remembers the "+
				"operation, so a retry this node decides again lands as a SECOND "+
				"record every node applies", res.Outcome, last, first.Position.Seq)
		}
	})

	t.Run("an id that carries no instant is vouched for only where nothing was scrubbed", func(t *testing.T) {
		t.Parallel()
		never := newHarness(t)
		res, err := never.write(probeSubject("a"), "caller-chosen", "one")
		if err != nil || res.Outcome != statelog.OutcomeApplied {
			t.Fatalf("an id with no instant on a node that never adopted = "+
				"(%+v, %v), want applied — that ledger has lost nothing", res, err)
		}

		adopted := newHarness(t)
		adopted.adopt(time.Now())
		res, err = adopted.write(probeSubject("a"), "caller-chosen", "one")
		if err != nil {
			t.Fatalf("an id with no instant on a node that adopted: %v", err)
		}
		if res.Outcome != statelog.OutcomeUnknown {
			t.Fatalf("outcome = %q, want unknown — an id whose age nobody can "+
				"read may predate the adoption, and reading it as recent is the "+
				"one reading that can re-decide it", res.Outcome)
		}
	})
}

// A RETRY OF AN OPERATION THIS NODE'S LEDGER HOLDS IS ANSWERED, NOT DECIDED
// AGAIN — however long after the first copy it comes.
//
// The broker's duplicate window is the only other thing that could collapse
// it, and it is two minutes wide. Past it, a retry used to take a snapshot
// that already held the first application, decide again on top of it and
// publish a second copy every node applied: a second comment, a counter moved
// twice, an update conditioned on the version its own first copy moved refused
// as stale. The ledger is read in the snapshot's own transaction, before the
// domain is asked anything.
func TestARetryTheLedgerHoldsIsAnsweredNotDecided(t *testing.T) {
	t.Parallel()

	t.Run("past the duplicate window", func(t *testing.T) {
		t.Parallel()
		h := newHarnessFor(t, shortWindowDomain{})
		op := statelog.NewOpID(time.Now(), "write-a")
		first, err := h.write(probeSubject("a"), op, "one")
		if err != nil || first.Outcome != statelog.OutcomeApplied {
			t.Fatalf("the first application = (%+v, %v), want applied", first, err)
		}
		time.Sleep(2 * shortWindow)
		appended := h.appends.appends.Load()

		decided := 0
		res, err := h.pub.Publish(t.Context(), sessionWrite(probeSubject("a"),
			op, statelog.Position{}, func() { decided++ }))
		if err != nil {
			t.Fatalf("the retry: %v", err)
		}
		if res.Outcome != statelog.OutcomeApplied || res.Position != first.Position {
			t.Fatalf("the retry = %q at %s, want applied at the first copy's %s",
				res.Outcome, res.Position, first.Position)
		}
		if !res.Collapsed {
			t.Error("the retry does not say its decision was never taken — a " +
				"caller computing its answer inside the decision would build " +
				"on one nothing published")
		}
		if decided != 0 {
			t.Errorf("the retry decided %d time(s) — on rows that already hold "+
				"the first application, which is how a counter moves twice", decided)
		}
		if got := h.appends.appends.Load() - appended; got != 0 {
			t.Errorf("the retry reached the broker %d time(s) past its duplicate "+
				"window, which lands a second copy every node applies", got)
		}
	})

	// AND A DUPLICATE ACKNOWLEDGEMENT SAYS THE SAME: the broker collapsed
	// this append onto an earlier copy inside its window — here one this
	// node has not applied yet, so its ledger could not answer first — and
	// the position is that copy's, not a record this call stored.
	t.Run("a duplicate acknowledgement", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.applier.mu.Lock()
		h.applier.auto = false
		h.applier.mu.Unlock()
		op := statelog.NewOpID(time.Now(), "write-a")
		// The first copy lands on one subject and this node applies none
		// of it; the retry is decided on another, where its expectation
		// is current, so only the broker's dedupe can collapse it.
		seq, _, err := h.log.Append(t.Context(), probePrefix+".object.a", op, nil, []byte("one"))
		if err != nil {
			t.Fatalf("the first copy: %v", err)
		}
		res, err := h.write(probeSubject("b"), op, "one")
		if err != nil {
			t.Fatalf("the retry: %v", err)
		}
		if res.Position.Seq != seq || !res.Collapsed {
			t.Fatalf("the retry = %+v, want the first copy's sequence %d and "+
				"collapsed — the broker stored nothing for this call", res, seq)
		}
	})
}

// shortWindow is the duplicate window the case above runs its broker with —
// short enough to be waited out, where the shipped two minutes is not.
const shortWindow = 200 * time.Millisecond

// shortWindowDomain is the probe domain with a broker that forgets an
// operation id after [shortWindow].
type shortWindowDomain struct{ probeDomain }

func (shortWindowDomain) Stream() statelog.StreamSpec {
	spec := probeDomain{}.Stream()
	spec.Duplicates = shortWindow
	return spec
}
