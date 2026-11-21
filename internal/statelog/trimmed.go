package statelog

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/words"
)

// The trimmed record (ADR-0080) removes a log's arbitration anchors below its
// trim floor, each of which can only send a doomed append. It is a framework
// record every node's applier applies, never a sweep beside the applier (a
// second writer of the replicated estate, ADR-0002), and safe at any bound: an
// absent anchor moves a write to the other road to expectation zero, never off
// the fence (package doc, "An anchor below the floor is removed by a record").

// TrimmedKind is the subject kind of the trim's record that removes the
// arbitration anchors below what a log no longer holds.
const TrimmedKind = "trimmed"

// TrimmedVersion is 1 for ever, for [BarrierVersion]'s reason: a build that
// cannot decode a trimmed record defers it, keeping its anchors, and the record
// has no payload a later version could extend.
const TrimmedVersion = 1

// AnchorsPerTrimmedRecord bounds the anchors one trimmed record's apply
// removes, because the apply runs on the store's one writer on every node and
// [ApplyTxTimeBudget] is checked only between records: a record removing a
// week's backlog (Gitlet's model mints ~70,000 subjects a day) would hold every
// domain's applier for tens of seconds.
//
// 250 is set from the p99, since the budget bounds the slowest transaction a
// waiter sits behind: BenchmarkATrimmedRecordsSweep measures 21, 56 and 75 ms
// at 20k, 300k and 1M anchors, under a third of the budget; 1,000 reached
// 159 ms (max 273 ms). Smaller buys little: the commit is already a large share
// and each record is one more append. The rows count toward [ApplyTxRowBudget]
// too: a transaction closes after sixteen of these records at most.
const AnchorsPerTrimmedRecord = 250

// TrimmedRecordsPerSweep bounds the trimmed records one [SweepPlan] appends,
// per log per trim tick (the engine's quarter hour), beside the time budget
// [SweepPlan.Sweep] is handed; which binds depends on how fast a record applies.
//
// 128 is 32,000 anchors a tick, about three million a day: the cap binds where
// records apply fast (10 ms median at 20k anchors), the budget on a large table
// (43 ms at 1M). The three anchor-writing logs draining at once share the
// engine's ten seconds, 1.7 million a day each, over twenty times Gitlet's
// modelled rate of new subjects, so a week's backlog or a reanchor's clears in a
// day. The cap stays beside the budget because the budget bounds this node's
// time while every node applies the records: the cap bounds, in records, the
// queue a slower node builds in front of its writes, and the count's index walk.
const TrimmedRecordsPerSweep = 128

// TrimmedSubject is the subject of the trimmed record whose floor is floor:
// the generation and the sequence, in decimal, joined by a dot.
//
// The floor travels in the subject because the framework never decodes a
// payload (each domain's wire shape is its own) and the subject is what every
// [Domain.Envelope] hands back verbatim. One spelling ([TrimmedFloor] refuses
// any other), so one floor is one broker subject.
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

// TrimmedEnvelope is the envelope of the trimmed record writer appends to
// remove the anchors below floor, the one value a domain's encoder renders
// ([CheckTrimmed]).
//
// Unlike a barrier's it stamps the writer: it removes rows, so an eviction
// gate must be able to drop an evicted node's. The generation is the floor's,
// the one the trim read the log in. No operation id: it would become a message
// id and a ledger row nothing resolves, and identical trimmed records are each
// meant to land, each removing the next [AnchorsPerTrimmedRecord].
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
// The engine's gate and the conformance suite call it over the domain's
// production encoder, and [PlanSweep] checks its plan's body the same way, so
// the framework never appends a record it would not recognise.
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
		return fmt.Errorf("statelog: %s's trimmed record declares scope %s, not "+
			"the framework's %q — anything else makes the reads and writes it "+
			"intersects wait behind a record that changes none of their rows",
			name, words.And(env.Scope.Paths), FrameworkScope)
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

// Committer is this node's own applier of one log, which the anchor sweep
// paces itself on: the log's [Runner] in the engine.
//
// The sweep waits on its reached position ([Runner.Reached]), never its
// committed checkpoint: a trimmed record writes rows, so it is reached only
// once committed, and the barriers at the log's end write none, so their
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

	// Signer signs every trimmed record the sweep appends. Required, for
	// [Deps.Signer]'s reason: every applier refuses an unsigned record, so
	// its anchors would never be removed.
	Signer *recordseal.Signer

	// Ends reads the log's two ends live: how far this node's copy must
	// have applied before the sweep counts from it.
	Ends func(context.Context) (LogEnds, error)

	// Runner is this node's own applier of the log, which the sweep waits
	// on for the log's end before it counts, and for each record before it
	// appends the next.
	Runner Committer

	// Admission is the log's gate reserve, required exactly when the domain
	// keeps one ([ReservationOf]), for [NewReadIndex]'s reason: the records
	// run as long as the trim does, and in the reserve they would spend the
	// room an eviction is kept.
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

// errSweepBudgetSpent is the cause a sweep's context ends with when its budget
// runs out: it tells the designed stop from a wait or a read that failed.
var errSweepBudgetSpent = errors.New("statelog: the anchor sweep's time budget ran out")

// Swept is what one [SweepPlan.Sweep] did.
type Swept struct {
	// Records is how many trimmed records landed on the log, whether or not
	// this node had applied the last when it stopped.
	Records int

	// Remain reports that it left anchors below the floor for the next
	// sweep to count: its budget ran out before it appended every record
	// its count called for, or the count reached its cap.
	Remain bool
}

// SweepPlan is one log's anchor sweep counted and not yet appended: how many
// trimmed records the anchors this node's copy holds below a floor call for,
// and the one record every append carries. [PlanSweep] counts it and
// [SweepPlan.Sweep] appends it within a time budget.
//
// Two steps, so a caller sweeping several logs inside one bound (the engine's
// trim) shares it, once every log is counted, among those with anything to
// sweep: an idle log costs its count and none of the time.
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
// [TrimmedRecordsPerSweep], and none on a log with none or on an evicted node.
func (p SweepPlan) Records() int { return p.records }

// PlanSweep counts what a sweep of d's log below below appends: one record per
// [AnchorsPerTrimmedRecord] anchors this node's copy holds below the floor, at
// most [TrimmedRecordsPerSweep]; the next plan counts what the cap leaves.
//
// It counts only from a copy that has applied the whole log: one behind (after
// a snapshot adoption, backlogged, stopped, or a holder the duty just moved to)
// counts anchors the records already on the log remove, and would append them
// again every tick. A copy that cannot commit the log's end within
// [DefaultResolveBudget], or before ctx (the engine's shared deadline) ends, is
// an error naming why, and nothing is appended. An evicted node plans nothing,
// since every applier would gate its records (`records_gated`); an unreadable
// eviction refuses, as the third value blocks. It checks the record it plans.
func PlanSweep(ctx context.Context, d SweepDeps, below Position) (SweepPlan, error) {
	if err := d.check(below); err != nil {
		return SweepPlan{}, err
	}
	// Through a signing log, so no path of the sweep appends an unsigned record.
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
		// A count that reached its limit may have more below it.
		capped:  n == limit,
		subject: wireSubject(d.Spec.SubjectPrefix, TrimmedSubject(below)),
	}, nil
}

// Sweep appends the plan's trimmed records within budget, and answers how many
// it appended and whether it left any for the next sweep.
//
// Each record is appended only once this node has committed the one before, so
// a write or a barrier behind the sweep on this node waits one record's apply;
// a wait failing within [DefaultResolveBudget], the bound a write's resolution
// uses, ends the sweep. The budget bounds its length: once spent no record is
// appended and no wait outlasts it, so the caller (the engine's trim, which
// evaluates alarms on the same goroutine) is held the budget plus one append,
// and a node applying k times slower is left about k − 1 budgets behind, never
// more than the cap. Running out is the designed stop, answered with Remain and
// no error; a budget of zero or less bounds nothing and is refused.
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
	// Appends run under the caller's context, everything else under the
	// budget's: an append the budget cut short has an unknown outcome, and
	// this answers how many landed. The budget is checked before each instead.
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
		// No message id and no expectation: identical records are each meant
		// to land, and a duplicate removes what is left below the floor, or
		// nothing.
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

// waitReached waits, for at most [DefaultResolveBudget] and never past ctx, for
// this node's applier to reach p. The cause stays on the error: the sweep's
// budget running out is its designed stop, while this wait's own bound passing
// is an applier that could not reach what was appended.
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
