package statelog_test

import (
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// AN OPERATION THIS NODE'S LEDGER CANNOT VOUCH FOR IS NEVER DECIDED A SECOND
// TIME.
//
// A ledger that LOST ROWS — to its retention sweep, or to a snapshot adopted
// from a donor that scrubbed its ledger — holds none for an operation minted
// before the loss, so a retry of one — a turn re-run, a caller repeating an
// `unknown` under the same id — finds no row, takes a snapshot whose rows
// already hold the first application, and decides again on top of it. The broker has no reason to refuse that: the expectation is current, and
// the duplicate window is two minutes wide. So the refusal has to come from
// here, before the append, and it has to read the instant the operation was
// MINTED — which is the id's own, because every retry reuses the id and a
// retry's own clock is always after the loss.
func TestAnOperationTheLedgerCannotVouchForIsNeverDecidedTwice(t *testing.T) {
	t.Parallel()

	t.Run("minted before an adoption from a scrubbing donor and retried after it, it answers unknown and publishes nothing", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		minted := time.Now().Add(-time.Hour)
		op := statelog.NewOpID(minted, "write-a")
		first, err := h.write(probeSubject("a"), op, "one")
		if err != nil || first.Outcome != statelog.OutcomeApplied {
			t.Fatalf("the first application = (%+v, %v), want applied", first, err)
		}
		// THE ADOPTION, after the mint and before the retry, from a
		// donor that scrubbed its ledger: it arrives empty, with the
		// join's start as its watermark.
		h.adoptFromAScrubbingDonor(minted.Add(time.Minute))
		appended := h.appends.appends.Load()

		res, err := h.write(probeSubject("a"), op, "one")
		if err != nil {
			t.Fatalf("the retry: %v", err)
		}
		if res.Outcome != statelog.OutcomeUnknown {
			t.Fatalf("outcome = %q, want unknown — the ledger that would say "+
				"whether the first application landed was scrubbed by the "+
				"donor, and deciding again on rows that already hold it "+
				"applies the operation twice", res.Outcome)
		}
		if got := h.appends.appends.Load() - appended; got != 0 {
			t.Fatalf("the retry appended %d record(s) — an operation the "+
				"ledger cannot vouch for must never reach the broker again", got)
		}
		if res.OpID != op {
			t.Errorf("unknown carries op id %q, want %q to retry under", res.OpID, op)
		}
		// AND SAYS WHY, because the retry it needs is not this node's:
		// the same id repeated here meets the same scrubbed ledger.
		if !res.Unvouched {
			t.Error("an unknown the ledger could not vouch for is not marked " +
				"unvouched — a caller cannot tell it from a lost " +
				"acknowledgement, which the same id retried here resolves")
		}
	})

	t.Run("minted after the adoption, an absent row is conclusive and it publishes", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		adopted := time.Now().Add(-time.Hour)
		h.adoptFromAScrubbingDonor(adopted)
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
		// A loss the operation predates, which did NOT take the first
		// copy's row: the copy landed above it, so this node's own
		// applier applied it and its row is here. The row speaks for
		// itself, and refusing it would strand a retry that this node
		// can answer.
		h.applier.mu.Lock()
		h.applier.lost = minted.Add(time.Minute)
		h.applier.mu.Unlock()

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
		h.adoptFromAScrubbingDonor(minted.Add(time.Minute))
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

	// THE SWEEP IS THE LEDGER'S OTHER LOSS. A row applied more than the
	// ledger's retention ago is deleted by this node's own sweep, and a
	// retry of its operation — an operator repeating an `unknown` a month
	// on, a seat carrying an id across a long pause — finds the same
	// silence an adoption leaves, over rows that already hold the first
	// application.
	t.Run("minted before the ledger's sweep and retried after it, it answers unknown and publishes nothing", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		minted := time.Now().Add(-time.Hour)
		op := statelog.NewOpID(minted, "write-a")
		first, err := h.write(probeSubject("a"), op, "one")
		if err != nil || first.Outcome != statelog.OutcomeApplied {
			t.Fatalf("the first application = (%+v, %v), want applied", first, err)
		}
		h.sweep(minted.Add(time.Minute))
		appended := h.appends.appends.Load()

		res, err := h.write(probeSubject("a"), op, "one")
		if err != nil {
			t.Fatalf("the retry: %v", err)
		}
		if res.Outcome != statelog.OutcomeUnknown || !res.Unvouched {
			t.Fatalf("outcome = %q (unvouched %v), want an unvouched unknown — "+
				"the row that would say whether the first application landed "+
				"was swept, and deciding again applies the operation twice",
				res.Outcome, res.Unvouched)
		}
		if got := h.appends.appends.Load() - appended; got != 0 {
			t.Fatalf("the retry appended %d record(s) past the sweep", got)
		}
	})

	t.Run("minted after the sweep's cutoff, an absent row is conclusive and it publishes", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		swept := time.Now().Add(-time.Hour)
		h.sweep(swept)
		res, err := h.write(probeSubject("a"), statelog.NewOpID(swept.Add(time.Second), "write-a"), "one")
		if err != nil || res.Outcome != statelog.OutcomeApplied {
			t.Fatalf("a first write minted after the sweep's cutoff = (%+v, %v), "+
				"want applied — every copy of it is applied after its mint, so "+
				"no sweep so far can have deleted its row", res, err)
		}
	})

	t.Run("an id that carries no instant is vouched for only where nothing was lost", func(t *testing.T) {
		t.Parallel()
		never := newHarness(t)
		res, err := never.write(probeSubject("a"), "caller-chosen", "one")
		if err != nil || res.Outcome != statelog.OutcomeApplied {
			t.Fatalf("an id with no instant on a node that never adopted = "+
				"(%+v, %v), want applied — that ledger has lost nothing", res, err)
		}

		for name, lose := range map[string]func(*harness){
			"adopted from a scrubbing donor": func(h *harness) {
				h.adoptFromAScrubbingDonor(time.Now())
			},
			"swept": func(h *harness) { h.sweep(time.Now()) },
		} {
			h := newHarness(t)
			lose(h)
			res, err = h.write(probeSubject("a"), "caller-chosen", "one")
			if err != nil {
				t.Fatalf("an id with no instant on a node that %s: %v", name, err)
			}
			if res.Outcome != statelog.OutcomeUnknown {
				t.Fatalf("on a node that %s, outcome = %q, want unknown — an "+
					"id whose age nobody can read may predate the loss, and "+
					"reading it as recent is the one reading that can "+
					"re-decide it", name, res.Outcome)
			}
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

// A REFUSAL IS NOT RETURNED FOR AN OPERATION THE LEDGER CANNOT VOUCH FOR —
// unless it is about this node rather than the operation.
//
// A retry's rows already hold what its first copy did, and that is exactly
// what makes a decision refuse: a move's root is already in the target, a
// create's guarding row is already there, a version condition finds the
// object moved by its own first copy. A domain whose decision only PROBES the
// ledger refuses whenever it runs at all. So on a node whose ledger may have
// lost the operation's row, the refusal is the ledger's silence read as an
// answer, and it was returned before the ledger was ever asked.
func TestARefusalTheLedgerCannotVouchForIsUnknown(t *testing.T) {
	t.Parallel()

	refused := errors.New("the rows say somebody else did it")
	probe := func(h *harness, op string) (statelog.Result, error) {
		return h.pub.Publish(h.t.Context(), statelog.Request{
			Subject: probeSubject("a"),
			Scope:   statelog.ScopeSet{Paths: []string{"object.a"}},
			OpID:    op,
			Pattern: statelog.PatternArbitrated,
			Decide: func(*sql.Tx, statelog.Stamp) (statelog.Decision, error) {
				return statelog.Decision{}, refused
			},
		})
	}
	create := func(h *harness, op string) (statelog.Result, error) {
		return h.pub.Publish(h.t.Context(), statelog.Request{
			Subject: probeSubject("a"),
			Scope:   statelog.ScopeSet{Paths: []string{"object.a"}},
			OpID:    op,
			Pattern: statelog.PatternCreate,
			Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
				return statelog.Decision{Payload: probeRecord(stamp, op, "x")}, nil
			},
		})
	}
	minted := time.Now().Add(-time.Hour)

	t.Run("a decision's refusal, minted before the sweep, answers unknown", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.sweep(minted.Add(time.Minute))
		op := statelog.NewOpID(minted, "probe")
		res, err := probe(h, op)
		if err != nil || res.Outcome != statelog.OutcomeUnknown || res.OpID != op ||
			!res.Unvouched {
			t.Fatalf("a refusing decision on an operation the ledger cannot "+
				"vouch for = (%+v, %v), want an unvouched unknown under %s",
				res, err, op)
		}
		if got := h.appends.appends.Load(); got != 0 {
			t.Fatalf("appended %d record(s)", got)
		}
	})

	t.Run("a create's guarding row, minted before the sweep, answers unknown", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.sweep(minted.Add(time.Minute))
		h.rows.set(func(s *statelog.Snap) { s.Guard = true })
		res, err := create(h, statelog.NewOpID(minted, "create"))
		if err != nil || res.Outcome != statelog.OutcomeUnknown {
			t.Fatalf("a create over a guarding row its own first copy may have "+
				"written = (%+v, %v), want unknown — \"it already exists\" "+
				"is what a re-run of a create that landed always finds", res, err)
		}
	})

	t.Run("the same refusals stand where the ledger vouches", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.sweep(minted.Add(time.Minute))
		after := minted.Add(2 * time.Minute)
		if _, err := probe(h, statelog.NewOpID(after, "probe")); !errors.Is(err, refused) {
			t.Fatalf("a refusing decision minted after the sweep = %v, want "+
				"its refusal — nothing that far back was lost, so the ledger's "+
				"silence is an answer", err)
		}
		h.rows.set(func(s *statelog.Snap) { s.Guard = true })
		if _, err := create(h, statelog.NewOpID(after, "create")); !errors.Is(err, statelog.ErrExists) {
			t.Fatalf("a create minted after the sweep over a guarding row = %v, "+
				"want ErrExists", err)
		}
	})

	t.Run("a refusal about this node stands either way", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.sweep(minted.Add(time.Minute))
		h.rows.set(func(s *statelog.Snap) { s.Deleted = true })
		_, err := create(h, statelog.NewOpID(minted, "create"))
		var refusal *statelog.Unavailable
		if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonDeleted {
			t.Fatalf("a create over a deletion marker on an operation the "+
				"ledger cannot vouch for = %v, want the deleted refusal — it "+
				"is true whether or not the operation applied, and names the "+
				"remedy an unknown would hide", err)
		}
	})
}

// A LEDGER LOSS BETWEEN THE DECISION AND ITS RESOLUTION IS UNVOUCHED TOO, AND
// A LOST ACKNOWLEDGEMENT IS NOT.
//
// The resolution of an ambiguous publish is the third place the ledger's
// silence is weighed: something landed on the subject, the ledger holds no row
// for this operation, and the ledger has lost rows since the operation was
// minted — so "somebody else won" cannot be concluded. That is an unvouched
// unknown, like the other two. A publish nobody could probe at all is a plain
// lost acknowledgement, which the same id retried here does resolve, and it
// must not be marked.
func TestAnUnknownSaysWhetherTheLedgerCouldVouch(t *testing.T) {
	t.Parallel()

	t.Run("a loss between the decision and its resolution", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		first, err := h.write(probeSubject("a"), "op-0", "one")
		if err != nil {
			t.Fatalf("the first write: %v", err)
		}
		h.anchorAt(probeSubject("a"), first.Position.Seq)
		minted := time.Now().Add(-time.Hour)
		op := statelog.NewOpID(minted, "write-a")
		var once sync.Once
		h.appends.mu.Lock()
		h.appends.beforeLastSeq = func() {
			once.Do(func() {
				// A PEER'S RECORD LANDS ABOVE THE ANCHOR, so the
				// resolution has something to weigh...
				seq, _, err := h.log.Append(t.Context(), probePrefix+".object.a",
					"peer-op", nil, probeRecord(statelog.Stamp{
						Gen: h.gen.Load(), Writer: "node-b"}, "peer-op", "theirs"))
				if err != nil {
					t.Errorf("land the peer's record: %v", err)
					return
				}
				// THIS NODE APPLIES IT, as it would a peer's.
				h.applier.advance(statelog.Position{
					Stream: probeStream, Generation: h.gen.Load(), Seq: seq})
				// ...and the ledger loses everything minted before
				// now, this operation included.
				h.sweep(minted.Add(time.Minute))
			})
		}
		h.appends.mu.Unlock()
		// THIS CALL'S APPEND NEVER LANDS, its answer lost.
		h.appends.fail(errors.New("no response from stream"), true)

		res, err := h.write(probeSubject("a"), op, "two")
		if err != nil {
			t.Fatalf("the write: %v", err)
		}
		if res.Outcome != statelog.OutcomeUnknown || !res.Unvouched {
			t.Fatalf("the resolution answered %+v, want an unvouched unknown", res)
		}
	})

	t.Run("a publish nobody could probe", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		first, err := h.write(probeSubject("a"), "op-0", "one")
		if err != nil {
			t.Fatalf("the first write: %v", err)
		}
		h.anchorAt(probeSubject("a"), first.Position.Seq)
		h.appends.mu.Lock()
		h.appends.lastSeqErr = errors.New("nats: no responders")
		h.appends.mu.Unlock()
		h.appends.fail(errors.New("no response from stream"), true)

		res, err := h.write(probeSubject("a"), statelog.NewOpID(time.Now(), "write-a"), "two")
		if err != nil {
			t.Fatalf("the write: %v", err)
		}
		if res.Outcome != statelog.OutcomeUnknown {
			t.Fatalf("a publish nobody could probe answered %q, want unknown", res.Outcome)
		}
		if res.Unvouched {
			t.Error("a lost acknowledgement is marked unvouched — the same id " +
				"retried here resolves it, and the mark says it never will")
		}
	})
}
