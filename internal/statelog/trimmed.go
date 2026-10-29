package statelog

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// THE TRIMMED RECORD — ADR-0050.
//
// Every arbitrated record a node consumes raises its subject's anchor
// ([tables.advanceAnchor]), and the anchor is what a write's expectation is
// formed from. Below the trim floor an anchor names a record the log no longer
// holds: a write that forms its expectation from it is refused by the broker,
// whatever it says, before the discriminator takes the zero path. So an anchor
// below the floor costs one doomed append on the next write to its object and
// a row on every data node for the life of the deployment — and the code host
// mints subjects for every throwaway repository a seat creates.
//
// # Removed by a record, never by a sweep beside the applier
//
// The anchors are replicated-estate state, and a maintenance job deleting them
// would be a second writer of the replicated estate (ADR-0002): every node
// deleting on its own schedule, from its own reading of a floor. So the trim
// appends a FRAMEWORK record naming what the log no longer holds, and every
// node's applier removes this log's anchors below it, in the transaction that
// applies it, at the same position on every node ([SweepPlan.Sweep] writes it,
// the applier's own step removes them).
//
// # Why removing one is safe at any bound
//
// An anchor is never what prevents a lost update — the broker's arbitration
// is. It is the expectation a write sends, and an absent one is a PROBE: the
// write asks the log for the subject's last sequence, and a subject that holds
// nothing reaches the expectation-zero fence and its floor theorem by that
// road, exactly as one whose anchor named a trimmed record reaches it after
// the broker refuses the anchor (the package doc's "both roads to an
// expectation of zero"). So removal moves a write between the two roads and
// never off the fence. Which anchors to remove is therefore chosen for COST:
// below the log's first surviving sequence an anchor can only send a doomed
// append, while one at or above it names a record the log still holds and
// saves the next write a leader read.

// TrimmedKind is the subject kind of the trim's record that removes the
// arbitration anchors below what a log no longer holds.
const TrimmedKind = "trimmed"

// TrimmedVersion is 1 from the first release AND FOR EVER, for
// [BarrierVersion]'s reason: a trimmed record a build cannot decode would be
// deferred — its anchors kept and a deferral row added — and the record has no
// payload a later version could extend.
const TrimmedVersion = 1

// AnchorsPerTrimmedRecord bounds the anchors one trimmed record's apply
// removes.
//
// BOUNDED AT ALL because the apply runs on the store's ONE writer, on every
// node at once, and [ApplyTxTimeBudget] is checked only between records: one
// record that removed a week's backlog — the code host's model mints about
// seventy thousand subjects a day — would hold every domain's applier for tens
// of seconds.
//
// TWO HUNDRED AND FIFTY, from the tail rather than the mean, because what the
// budget bounds is the slowest transaction a waiter sits behind.
// BenchmarkATrimmedRecordsSweep times one record's committed transaction on
// the pinned driver under the store's own pragmas, on a 4-vCPU container's
// virtio disk, over a table laid out as production lays it out — the order
// anchors are removed in unrelated to the order the table stores them in, and
// another log's anchors interleaved — and at 250 a record's 99th percentile is
// 21 ms from twenty thousand anchors, 56 ms from three hundred thousand and
// 75 ms from a million, twice the half million the code host's model holds on
// one log alone: under a third of the budget at the largest. At a thousand it
// was 49, 137 and 159 ms — 159 is most of the budget, the slowest of two
// hundred records took 273 ms, and a delete's cost grows with its row count.
// Smaller buys little: at 250 a record's commit is already a large share of
// its cost, and every record is one more on the log and one more append for
// the sweep to wait on. The rows count toward [ApplyTxRowBudget] too, so a
// transaction closes after sixteen of these records at most.
const AnchorsPerTrimmedRecord = 250

// TrimmedRecordsPerSweep bounds the trimmed records one [SweepPlan] appends.
// The trim sweeps each log once a tick — the engine's, a quarter hour — so
// this is per log per tick. A sweep is bounded in TIME as well (the budget
// [SweepPlan.Sweep] is handed), and which bound binds depends on how fast a
// record applies: each is appended only once the holder has applied the one
// before, so a sweep spends a record's apply and its append per record.
//
// A HUNDRED AND TWENTY-EIGHT, which is 32 000 anchors a quarter hour — about
// three million a day — where records apply fast: at twenty thousand anchors
// a record's median apply is 10 ms, so the cap binds well inside any budget
// the engine sets. On a large table the budget can bind first: at a million
// anchors a record's 43 ms median apply and its append fit about seventy
// records into a third of the engine's ten seconds and the cap's 128 into the
// whole of them. The engine shares those ten seconds among the logs that have
// anchors below their floor THAT TICK — a log with none costs the others its
// count and nothing more — so a log draining a backlog on its own drains at
// the cap, three million anchors a day, and each of today's three logs that
// write anchors (the tracker's, the knowledge base's and the code host's)
// draining at once is given a third: 1.7 million a day each. That is over
// twenty times the code host's modelled rate of new subjects, so a backlog
// left by a week-long blocked trim, or by a reanchor that put every anchor of
// the generation before below the floor, clears within a day.
//
// THE CAP STILL EARNS ITS PLACE beside the budget, because the budget is a
// bound on THIS node's time and the records are applied on every node: a node
// that applies them slower than the holder builds the difference up as a
// queue in front of its own writes, and the cap bounds that queue in records
// whatever the budget allows. It also bounds the count's walk of the index. A
// smaller cap only drains slower.
const TrimmedRecordsPerSweep = 128

// TrimmedSubject is the subject of the trimmed record whose floor is floor:
// the generation and the sequence, in decimal, joined by a dot.
//
// THE FLOOR TRAVELS IN THE SUBJECT, and not in a payload, because the
// framework never decodes a payload — every domain's wire shape is its own —
// and the one per-record value every domain's [Domain.Envelope] already hands
// back verbatim is the subject. ONE SPELLING ([TrimmedFloor] refuses any
// other), so one floor is one broker subject.
func TrimmedSubject(floor Position) Subject {
	return Subject{
		Kind: TrimmedKind,
		ID: strconv.FormatUint(uint64(floor.Generation), 10) + "." +
			strconv.FormatUint(floor.Seq, 10),
	}
}

// TrimmedFloor recovers the floor a trimmed record's subject names, on stream.
//
// It refuses anything [TrimmedSubject] could not have produced: another kind,
// an id that is not exactly two decimal numbers, a spelling that is not the
// canonical one (a sign, a leading zero), and a position outside the packed
// form's range.
func TrimmedFloor(stream string, s Subject) (Position, error) {
	if s.Kind != TrimmedKind {
		return Position{}, fmt.Errorf("statelog: a %q subject is not a trimmed record's", s.Kind)
	}
	genText, seqText, ok := strings.Cut(s.ID, ".")
	if !ok {
		return Position{}, fmt.Errorf("statelog: trimmed subject id %q names no "+
			"floor — it is a generation and a sequence joined by a dot", s.ID)
	}
	gen, genErr := strconv.ParseUint(genText, 10, 32)
	seq, seqErr := strconv.ParseUint(seqText, 10, 64)
	if genErr != nil || seqErr != nil {
		return Position{}, fmt.Errorf("statelog: trimmed subject id %q is not a "+
			"generation and a sequence in decimal", s.ID)
	}
	floor := Position{Stream: stream, Generation: uint32(gen), Seq: seq}
	if TrimmedSubject(floor).ID != s.ID {
		return Position{}, fmt.Errorf("statelog: trimmed subject id %q is not the "+
			"one spelling of its floor, %q — one floor is one subject",
			s.ID, TrimmedSubject(floor).ID)
	}
	if err := floor.Valid(); err != nil {
		return Position{}, fmt.Errorf("statelog: trimmed subject id %q: %w", s.ID, err)
	}
	return floor, nil
}

// TrimmedEnvelope is the envelope of the trimmed record writer appends to remove
// the anchors below floor — the one value a domain's encoder renders
// ([CheckTrimmed]).
//
// THE WRITER IS STAMPED, unlike a barrier's: a trimmed record removes rows, so
// an eviction gate must be able to drop an evicted node's. The generation is
// the floor's, which is the generation the trim read the log in. NO OPERATION
// ID: one becomes a message id and a ledger row nothing resolves, and two
// identical trimmed records are each meant to land — each removes the next
// [AnchorsPerTrimmedRecord].
func TrimmedEnvelope(floor Position, writer string) Envelope {
	return Envelope{
		V:       TrimmedVersion,
		Kind:    TrimmedKind,
		Subject: TrimmedSubject(floor),
		Gen:     floor.Generation,
		Writer:  writer,
		Scope:   ScopeSet{Paths: []string{FrameworkScope}},
	}
}

// WritesAnchors reports whether a log's records write arbitration anchors —
// whether it arbitrates any kind — and so whether the trim has anchors on it
// to remove. A compacted log arbitrates nothing, writes no anchor and has no
// trimmed record.
func WritesAnchors(spec StreamSpec) bool { return len(spec.ArbitratedKinds) > 0 }

// frameworkRecord reports a kind whose whole effect is the framework's own: a
// record that writes no domain row on any node, whatever its history.
func frameworkRecord(kind string) bool { return kind == BarrierKind || kind == TrimmedKind }

// CheckTrimmed refuses a domain whose trimmed record, as encode renders it, the
// framework could not use — and a log that writes no anchor, which has nothing
// for one to remove.
//
// THE ENGINE'S GATE AND THE CONFORMANCE SUITE BOTH CALL IT, over the domain's
// own production encoder, and [PlanSweep] runs the same check on the body
// every append of its plan carries — so the framework never appends a record
// it would not recognise.
func CheckTrimmed(d Domain, spec StreamSpec, encode func(Envelope) ([]byte, error)) error {
	if !WritesAnchors(spec) {
		return fmt.Errorf("statelog: %s's log arbitrates no kind, so it writes no "+
			"anchor and a trimmed record has nothing to remove — appended, it "+
			"would be one more record on every node's log", d.Name())
	}
	if encode == nil {
		return fmt.Errorf("statelog: %s writes anchors and has no trimmed-record "+
			"encoder, so its anchors grow with every subject it ever writes", d.Name())
	}
	floor := Position{Stream: spec.Name, Generation: 1, Seq: 2}
	const writer = "check-writer"
	body, err := encode(TrimmedEnvelope(floor, writer))
	if err != nil {
		return fmt.Errorf("statelog: %s refuses to encode a trimmed record: %w", d.Name(), err)
	}
	return checkTrimmed(d, spec, floor, writer, body)
}

// checkTrimmed refuses a trimmed record body the framework could not use, with
// a sentence naming what each defect would cost.
func checkTrimmed(d Domain, spec StreamSpec, floor Position, writer string, body []byte) error {
	name := d.Name()
	env, err := d.Envelope(body)
	switch {
	case err != nil:
		return fmt.Errorf("statelog: %s's trimmed record does not decode through "+
			"its own envelope reader, so every applier would stop on it: %w", name, err)
	case !env.ReadableBy(TrimmedVersion):
		return fmt.Errorf("statelog: %s's trimmed record is at version %d — an "+
			"older build would defer it and every anchor it names would stay",
			name, env.V)
	case env.Kind != TrimmedKind || env.Subject != TrimmedSubject(floor):
		return fmt.Errorf("statelog: %s's trimmed record reads back as a %q record "+
			"on %q, not as %q — the framework would not recognise it and would "+
			"remove nothing", name, env.Kind, env.Subject, TrimmedSubject(floor))
	case env.Gen != floor.Generation || env.Writer != writer:
		return fmt.Errorf("statelog: %s's trimmed record names writer %q at "+
			"generation %d, and was written as %q at %d — the eviction gate reads "+
			"the writer, and the generation is what the identity fence reads",
			name, env.Writer, env.Gen, writer, floor.Generation)
	case env.OpID != "":
		return fmt.Errorf("statelog: %s's trimmed record carries operation %q — "+
			"an operation id becomes a message id and a ledger row nothing "+
			"resolves", name, env.OpID)
	case !slices.Equal(env.Scope.Paths, []string{FrameworkScope}):
		return fmt.Errorf("statelog: %s's trimmed record declares scope %v, not "+
			"the framework's %q — anything else makes the reads and writes it "+
			"intersects wait behind a record that changes none of their rows",
			name, env.Scope.Paths, FrameworkScope)
	case d.InstallsGate(env) || d.NodeGate(env):
		return fmt.Errorf("statelog: %s declares its trimmed record a gate — a "+
			"gate an older build cannot read stops its applier, and a node gate "+
			"is excused the fences and the reserve", name)
	case slices.Contains(spec.ArbitratedKinds, TrimmedKind):
		return fmt.Errorf("statelog: %s's log arbitrates the trimmed kind, so "+
			"every trimmed record would write an anchor of its own and need an "+
			"expectation to append", name)
	}
	return nil
}

// Committer is this node's own applier of one log, as the anchor sweep paces
// itself on it — the log's [Runner] in the engine.
//
// ITS REACHED POSITION, never its committed checkpoint, is what the sweep
// waits on ([Runner.Reached]): a trimmed record writes rows, so it is reached
// only once committed, and the barriers at the log's end write none, so their
// checkpoint holds back no anchor the sweep counts.
type Committer interface {
	// Reached is the position this node's rows of the log are current
	// through.
	Reached() Position

	// WaitReached blocks until this node's applier has reached p.
	WaitReached(ctx context.Context, p Position) error
}

// SweepDeps is what the trim needs to remove one log's anchors below a floor.
type SweepDeps struct {
	// Domain and Spec are the log's: Spec is the running log's own
	// ([StreamSpec.Instantiates]).
	Domain Domain
	Spec   StreamSpec

	// Log is the live log the records are appended to.
	Log Appender

	// Signer signs every trimmed record the sweep appends. REQUIRED, for the
	// reason [Deps.Signer] is: every applier opens a record's frame before
	// it reads one, so an unsigned trimmed record is refused on every node
	// and the anchors it was appended to remove are never removed.
	Signer *Signer

	// Ends is the log's two ends, read live — what the sweep reads to learn
	// how far this node's copy must have applied before it is counted
	// from.
	Ends func(context.Context) (LogEnds, error)

	// Runner is this node's own applier of the log, which the sweep waits
	// on twice over: for the log's end before it counts, and for each
	// record it appends before it appends the next.
	Runner Committer

	// Admission is the log's gate reserve — required exactly when the
	// domain keeps one ([ReservationOf]), for [NewReadIndex]'s reason:
	// the records go on for as long as the trim does, and allowed into the
	// reserve they would spend the room an eviction is kept.
	Admission Admission

	// Encode is the domain's own encoder of the trimmed record.
	Encode func(Envelope) ([]byte, error)

	// DB is THIS node's copy of the replicated estate, resolved by the
	// caller per call.
	DB ReadEstate

	// Writer is this node's id, stamped on every record for the eviction
	// gate.
	Writer string

	// Evicted is this node's own eviction on this log. Nil for a domain
	// with no eviction gate, which answers "not evicted".
	Evicted func(context.Context) (bool, error)
}

// errSweepBudgetSpent is the cause a sweep's context ends with when the budget
// [SweepPlan.Sweep] was handed runs out — what tells the designed stop from a
// wait or a read that failed.
var errSweepBudgetSpent = errors.New("statelog: the anchor sweep's time budget ran out")

// Swept is what one [SweepPlan.Sweep] did.
type Swept struct {
	// Records is how many trimmed records it appended — each landed on the
	// log, whether or not this node had applied the last when it stopped.
	Records int

	// Remain reports that it left anchors below the floor for the next
	// sweep to count: its budget ran out before it appended every record
	// its count called for, or the count reached its cap.
	Remain bool
}

// SweepPlan is one log's anchor sweep COUNTED AND NOT YET APPENDED: how many
// trimmed records the anchors this node's copy holds below a floor call for,
// and the one record every append carries. [PlanSweep] counts it and
// [SweepPlan.Sweep] appends it within a time budget.
//
// TWO STEPS, because a caller that sweeps several logs inside one bound — the
// engine's trim, every log that writes anchors inside one alarm interval —
// can share that bound only among the logs that have anything to sweep once
// every log has been counted: a log with nothing below its floor costs its
// count and none of the time, and a log with a backlog is handed the time
// the idle ones did not need, wherever they sit in the order.
type SweepPlan struct {
	deps    SweepDeps
	below   Position
	records int
	capped  bool
	subject string
	body    []byte
}

// Records is how many trimmed records the plan appends: one per
// [AnchorsPerTrimmedRecord] anchors below its floor, at most
// [TrimmedRecordsPerSweep], and none on a log with none — an idle fleet's,
// a node evicted from the log, or a tick whose purge removed only barriers.
func (p SweepPlan) Records() int { return p.records }

// PlanSweep counts what a sweep of d's log below below appends: the anchors
// this node's copy holds below the floor, one record per
// [AnchorsPerTrimmedRecord] of them and at most [TrimmedRecordsPerSweep], and
// none when there are none. A backlog the cap leaves is counted again by the
// next plan, without waiting for another purge.
//
// # Counted only from a copy that has applied the whole log
//
// The count is of THIS NODE'S rows, and the records already on the log remove
// anchors from those rows only once this node applies them. Counted from a
// copy behind them — a holder catching up after adopting a snapshot, one with
// a backlog, one whose applier has stopped, or a holder the duty just moved
// to while the last holder's records were in flight — the count includes
// anchors those records already remove, and every tick it stayed behind would
// append up to the cap again — records that remove nothing on a node that has
// applied the ones before them. So the plan first reads the log's end and
// waits for this node to commit it, and a copy that cannot within
// [DefaultResolveBudget] — or before ctx ends, where that is sooner — is a
// plan that could not count, and an error naming why: nothing is appended,
// the reason is the one an operator needs, and the next tick counts again.
//
// BOUNDED BY ctx, which is the caller's: the engine counts every log under
// the one deadline its sweeps share, so a copy slow to catch up spends that
// bound and never more.
//
// AN EVICTED NODE PLANS NOTHING and answers no error — every applier would
// drop its records under the eviction gate and count them toward the
// `records_gated` alarm; an eviction that cannot be read refuses, because the
// third value blocks.
//
// THE RECORD IS CHECKED HERE, before anything is appended: the framework
// never appends a record its own applier would not recognise.
func PlanSweep(ctx context.Context, d SweepDeps, below Position) (SweepPlan, error) {
	if err := d.check(below); err != nil {
		return SweepPlan{}, err
	}
	// THE PLAN APPENDS THROUGH A SIGNING LOG, so no path of the sweep can
	// put an unsigned record on it.
	signed, err := signing(d.Log, d.Signer, d.Domain.Name())
	if err != nil {
		return SweepPlan{}, err
	}
	d.Log = signed
	if d.Evicted != nil {
		var evicted bool
		if evicted, err = d.Evicted(ctx); err != nil {
			return SweepPlan{}, fmt.Errorf("statelog: read this node's eviction on %s "+
				"before sweeping its anchors: %w", d.Spec.Name, err)
		}
		if evicted {
			return SweepPlan{}, nil
		}
	}
	ends, err := d.Ends(ctx)
	if err != nil {
		return SweepPlan{}, fmt.Errorf("statelog: read the ends of %s before sweeping "+
			"its anchors: %w", d.Spec.Name, err)
	}
	if err = d.waitReached(ctx, below.At(ends.Last)); err != nil {
		return SweepPlan{}, fmt.Errorf("statelog: this node's copy of %s has not "+
			"applied the log's end, %d, so its anchors are not counted — counted, "+
			"they would include anchors the records already on the log remove: %w",
			d.Spec.Name, ends.Last, err)
	}
	t, err := newTables(d.Domain, d.Spec)
	if err != nil {
		return SweepPlan{}, err
	}
	const limit = TrimmedRecordsPerSweep * AnchorsPerTrimmedRecord
	n, err := t.anchorsBelow(ctx, d.DB, below, limit)
	if err != nil || n == 0 {
		return SweepPlan{}, err
	}
	body, err := d.Encode(TrimmedEnvelope(below, d.Writer))
	if err != nil {
		return SweepPlan{}, fmt.Errorf("statelog: encode %s's trimmed record: %w",
			d.Domain.Name(), err)
	}
	if err = checkTrimmed(d.Domain, d.Spec, below, d.Writer, body); err != nil {
		return SweepPlan{}, err
	}
	return SweepPlan{
		deps: d, below: below, body: body,
		records: (n + AnchorsPerTrimmedRecord - 1) / AnchorsPerTrimmedRecord,
		// A COUNT THAT REACHED ITS LIMIT may have more below it, which the
		// next plan counts.
		capped:  n == limit,
		subject: wireSubject(d.Spec.SubjectPrefix, TrimmedSubject(below)),
	}, nil
}

// Sweep appends the plan's trimmed records within budget, and answers how many
// it appended and whether it left any for the next sweep. A plan with no
// records appends nothing and answers so.
//
// # Paced on this node's applier, one record at a time
//
// Every record on the log is applied in order, on every node, and so is every
// write and every barrier appended after it. Appended back to back, a sweep's
// records would put up to the cap of them in front of whatever came next — a
// seat's write waiting for its own record, a linearizable read's barrier —
// and that wait is the whole run's apply time on every node. So each record is
// appended only once this node has committed the one before it, and a wait
// that fails within [DefaultResolveBudget] ends the sweep with what it
// appended: a write or a read behind the sweep ON THIS NODE waits at most ONE
// record's apply, and the sweep's own pace is what this node's applier keeps
// — the cost lands on the duty's tick rather than on anybody's request.
//
// [DefaultResolveBudget] is the bound for both waits — this one and the
// plan's — because it is the same quantity a write's resolution waits on:
// this node's applier reaching what was just appended. A node that cannot
// inside it has a health fault waiting does not fix, and the alarms name that.
//
// # And bounded in time
//
// Pacing sets the sweep's pace and nothing in it bounds the sweep's LENGTH:
// that is the cap times whatever each wait takes, and on an applier slow but
// still moving — every wait succeeding just inside [DefaultResolveBudget] — it
// is minutes, all of them on the caller's goroutine (the engine's trim, which
// evaluates the holder's alarms on the same one). Nor does pacing on THIS node
// bound what another node queues: one that applies the records slower than
// this node builds the difference up in front of its own writes and barriers.
// So the sweep is bounded in time too, by budget — no record is appended once
// it is spent and no wait outlasts it — and that one bound limits both costs.
// The caller is held for at most the budget and the append in flight when it
// ran out; and a node that applies a record k times slower than this one is
// left at most about k − 1 budgets of records behind when the sweep ends, and
// never more than the cap's. A budget of zero or less bounds nothing the
// caller chose, and is refused.
//
// RUNNING OUT IS THE DESIGNED STOP, not a failure: the sweep answers what it
// appended and that some remain, with no error, and the next one counts what
// is left.
func (p SweepPlan) Sweep(ctx context.Context, budget time.Duration) (Swept, error) {
	if p.records == 0 {
		return Swept{}, nil
	}
	d := p.deps
	if budget <= 0 {
		return Swept{}, fmt.Errorf("statelog: the anchor sweep of %s was handed a "+
			"time budget of %v — it needs a positive one, or nothing bounds how "+
			"long it holds its caller", d.Domain.Name(), budget)
	}
	// THE APPENDS RUN UNDER THE CALLER'S CONTEXT and everything else under
	// the budget's: an append the budget cut short would be one whose outcome
	// nobody knows, and what this answers is how many landed. The budget is
	// checked before each one instead.
	appendCtx := ctx
	ctx, cancel := context.WithTimeoutCause(ctx, budget,
		fmt.Errorf("%w after %v", errSweepBudgetSpent, budget))
	defer cancel()
	for i := range p.records {
		if spent(ctx) {
			return Swept{Records: i, Remain: true}, nil
		}
		release := func() {}
		if d.Admission != nil {
			var err error
			if release, err = d.Admission.Admit(ctx, appendBytes(p.body)); err != nil {
				if spent(ctx) {
					return Swept{Records: i, Remain: true}, nil
				}
				return Swept{Records: i}, err
			}
		}
		// NO MESSAGE ID AND NO EXPECTATION: identical records are each
		// meant to land, and a duplicate is harmless — it removes what is
		// left below the floor, or nothing.
		seq, _, err := d.Log.Append(appendCtx, p.subject, "", nil, p.body)
		release()
		if err != nil {
			return Swept{Records: i}, frameworkAppendError(TrimmedKind, p.subject, err)
		}
		if err = d.waitReached(ctx, p.below.At(seq)); err != nil {
			if errors.Is(err, errSweepBudgetSpent) {
				return Swept{Records: i + 1, Remain: i+1 < p.records || p.capped}, nil
			}
			return Swept{Records: i + 1}, fmt.Errorf("statelog: this node has not "+
				"applied the trimmed record at %d on %s, so the sweep appends no "+
				"more behind it: %w", seq, d.Spec.Name, err)
		}
	}
	return Swept{Records: p.records, Remain: p.capped}, nil
}

// spent reports whether a sweep's context ended because its budget ran out,
// and not because its caller's did.
func spent(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errSweepBudgetSpent)
}

// waitReached waits, for at most [DefaultResolveBudget] and never past ctx —
// the caller's bound for a plan, the budget's for a sweep — for this node's
// applier to reach p: see [PlanSweep] and [SweepPlan.Sweep], its two callers.
//
// THE CAUSE IS KEPT on the error, because the two bounds mean different
// things: the budget running out is the sweep's designed stop, and this wait's
// own bound passing is an applier that could not reach what was appended.
func (d SweepDeps) waitReached(ctx context.Context, p Position) error {
	ctx, cancel := context.WithTimeout(ctx, DefaultResolveBudget)
	defer cancel()
	if err := d.Runner.WaitReached(ctx, p); err != nil {
		if cause := context.Cause(ctx); cause != nil && !errors.Is(err, cause) {
			err = fmt.Errorf("%w: %w", err, cause)
		}
		return fmt.Errorf("its applier, at %s, did not reach %s: %w",
			d.Runner.Reached(), p, err)
	}
	return nil
}

// check refuses a dependency set that cannot sweep correctly.
func (d SweepDeps) check(below Position) error {
	switch {
	case d.Domain == nil:
		return fmt.Errorf("statelog: an anchor sweep has no domain")
	case d.Log == nil || d.DB == nil || d.Encode == nil:
		return fmt.Errorf("statelog: the anchor sweep of %s is missing its log, "+
			"its estate or its encoder", d.Domain.Name())
	case d.Ends == nil || d.Runner == nil:
		return fmt.Errorf("statelog: the anchor sweep of %s is missing the log's "+
			"ends or this node's applier, so it can neither count from a copy "+
			"that has applied the log nor pace its records on one", d.Domain.Name())
	case d.Writer == "":
		return fmt.Errorf("statelog: the anchor sweep of %s names no writer, so "+
			"no eviction gate could drop its records", d.Domain.Name())
	case below.Stream != d.Spec.Name:
		return fmt.Errorf("%w: the anchor sweep of %s on %s was handed a floor on %s",
			ErrWrongStream, d.Domain.Name(), d.Spec.Name, below.Stream)
	case ReservationOf(d.Domain).Kept && d.Admission == nil:
		return fmt.Errorf("statelog: the anchor sweep of %s has no admission, so "+
			"its records would fill the gate reserve an eviction needs", d.Domain.Name())
	case !ReservationOf(d.Domain).Kept && d.Admission != nil:
		return fmt.Errorf("statelog: the anchor sweep of %s was given an "+
			"admission, and its log keeps no gate reserve", d.Domain.Name())
	}
	if err := d.Spec.Instantiates(d.Domain); err != nil {
		return err
	}
	if !WritesAnchors(d.Spec) {
		return fmt.Errorf("statelog: %s's log writes no anchor, so it has none "+
			"to sweep", d.Domain.Name())
	}
	return below.Valid()
}
