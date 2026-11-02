package statelogtest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// runTrimmed reports what [TrimmedAnchorsGoAndAZeroWriteIsFenced] found.
func runTrimmed(t *testing.T, new Factory) {
	t.Helper()
	t.Run("anchors below the trim floor go, and a write to one of their subjects passes the zero fence",
		func(t *testing.T) {
			if err := TrimmedAnchorsGoAndAZeroWriteIsFenced(t, new); err != nil {
				t.Fatal(err)
			}
		})
}

// suiteOtherStream is a log nothing in the suite writes, whose anchor row the
// trimmed-record case plants to show the record is about its own log alone.
const suiteOtherStream = "CREWLET_SUITE_OTHER_LOG"

// suiteTrimWriter is the node the suite's trim runs as.
const suiteTrimWriter = "suite-trim"

// TrimmedAnchorsGoAndAZeroWriteIsFenced certifies the trim's trimmed record
// (ADR-0073) end to end over the candidate's own applier, encoder and read
// seam: a domain whose log writes anchors supplies the record and the
// framework can use it, its applier takes it — through the framework's own
// runner — removing exactly this log's anchors below the floor and writing
// nothing of its own, and a write to a subject whose anchor went then reaches
// the expectation-zero fence and publishes at zero only once the fence clears
// it, while a subject that kept its anchor arbitrates on it as before. A
// domain whose log writes no anchor is certified to declare no such record.
//
// # Why the whole loop, and not the encoder alone
//
// Every half of this is a seam a domain can break while its encoder looks
// right: an applier with no case for the kind faults every node's copy at the
// first trim, one that writes a row for it is a record that changes every
// node's tables, an envelope reader that loses the subject's id leaves the
// framework a floor it cannot read, and a read seam that kept a removed anchor
// would send the write the broker refuses. So the case drives the production
// runner over the candidate's applier and the production publisher over the
// candidate's read seam, and holds both to what the rows and the broker say.
//
// EXPORTED AND RETURNING THE VERDICT, for [Declaration]'s reason: the suite's
// own tests hand it a domain that gets each half wrong.
func TrimmedAnchorsGoAndAZeroWriteIsFenced(t *testing.T, new Factory) error {
	t.Helper()
	c := new(t)
	name, spec := c.Domain.Name(), c.spec()
	if !statelog.WritesAnchors(spec) {
		if c.EncodeTrimmed != nil {
			return fmt.Errorf("%s's log arbitrates no kind, so it writes no anchor, "+
				"and it supplies a trimmed record — appended, that is one more "+
				"record on every node's log each tick, removing nothing", name)
		}
		return nil
	}
	if c.EncodeTrimmed == nil {
		return fmt.Errorf("%s's log writes anchors and the candidate supplies no "+
			"trimmed record, so its anchors grow with every subject it ever writes", name)
	}
	if err := statelog.CheckTrimmed(c.Domain, spec, c.EncodeTrimmed); err != nil {
		return err
	}
	if c.Rows == nil {
		return fmt.Errorf("%s supplies no read seam, so nothing can show that a "+
			"write to a subject whose anchor the trim removed is fenced", name)
	}
	records := suiteRecords(t, new)
	kind := firstArbitrated(c, spec)
	if kind == "" {
		return fmt.Errorf("%s's candidate declares no kind its log arbitrates, so "+
			"the case has no anchor to remove", name)
	}

	// THE STAMP EVERY RECORD THE CANDIDATE ENCODES CARRIES — its writer and
	// its generation — which is the node and the generation the case runs
	// as, so a decision built from the candidate's own records passes the
	// publisher's stamp check.
	probe, err := c.Encode(kind, "suite-probe", "suite-probe", 0)
	if err != nil {
		t.Fatalf("encode a %s %s record: %v", name, kind, err)
	}
	stamp, err := c.Domain.Envelope(probe)
	if err != nil {
		t.Fatalf("read the envelope of a %s %s record: %v", name, kind, err)
	}
	gen, writer := stamp.Gen, stamp.Writer

	node, db := openNodeEstate(t, c)
	log := newSuiteLog()
	// SIGNED AND VERIFIED UNDER THE SUITE'S KEYRING, as every record a
	// running node applies is: the runner opens each frame before the
	// candidate's applier reads a byte, so the case drives the path a
	// trimmed record takes on a node.
	signer, err := statelog.NewSigner(name, suiteRing())
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := statelog.NewVerifier(name, suiteRing())
	if err != nil {
		t.Fatal(err)
	}
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain: c.Domain, Spec: spec, Applier: c.Applier, Verifier: verifier,
		Fetch: log, Log: log, Node: node, DB: db,
		Checkpoint: statelog.Position{Stream: spec.Name, Generation: gen},
	})
	if err != nil {
		t.Fatalf("build %s's applier: %v", name, err)
	}
	loop := startLoop(t, runner)
	defer loop.stop()

	// ANCHORS THE LOG ITS OWN RECORDS WROTE, one subject per record.
	written := appendRecords(t, c, spec, log, signer, records)
	if err = loop.waitPast(log.last()); err != nil {
		return fmt.Errorf("%s's applier did not apply its own records: %w", name, err)
	}
	if len(written) < 2 {
		return fmt.Errorf("%s's suite records wrote %d arbitrated record(s), so the "+
			"case has no anchor both to remove and to keep", name, len(written))
	}
	kept, swept := written[len(written)-1], written[0]
	floor := statelog.Position{Stream: spec.Name, Generation: gen, Seq: kept.seq}

	// AND TWO THE LOG NEVER WROTE: one on this log below the floor, which
	// goes whatever wrote it, and one on another log, which stays.
	stale := subjectOf(spec, statelog.Subject{Kind: "suite", ID: "stale"})
	if err = plantAnchor(t.Context(), db, spec.Name, stale,
		statelog.Position{Stream: spec.Name, Generation: gen}); err != nil {
		t.Fatalf("plant a stale anchor: %v", err)
	}
	if err = plantAnchor(t.Context(), db, suiteOtherStream, "crewlet.suite.other.x",
		statelog.Position{Stream: suiteOtherStream, Generation: gen, Seq: 1}); err != nil {
		t.Fatalf("plant another log's anchor: %v", err)
	}

	// THE TRIM: purge below the floor, then count and sweep through the
	// production entry points.
	log.purge(floor.Seq)
	// THE DOMAIN'S TABLES WHOLE, compared by content: a count would pass
	// an applier that rewrote a row for the record without adding one.
	before := tableContents(t, db, c.Domain.Tables())
	deps := statelog.SweepDeps{
		Domain: c.Domain, Spec: spec, Log: log, Signer: signer, Ends: log.Ends, Runner: runner,
		Admission: admissionFor(t, c),
		Encode:    c.EncodeTrimmed, DB: db.Reader(), Writer: suiteTrimWriter,
	}
	result, err := suiteSweep(t.Context(), deps, floor)
	n := result.Records
	switch {
	case err != nil && n > 0:
		// THE RECORD LANDED AND THIS NODE NEVER COMMITTED IT: the sweep
		// waits for each record it appends before it answers.
		return fmt.Errorf("%s's applier did not apply the trimmed record: %w — an "+
			"applier with no case for the kind faults every node's copy at the "+
			"first trim", name, err)
	case err != nil:
		return fmt.Errorf("the trim could not sweep %s's anchors: %w", name, err)
	}
	if n != 1 {
		return fmt.Errorf("the trim appended %d trimmed records for %d anchors below "+
			"the floor, want 1", n, len(written))
	}
	if err = loop.waitPast(log.last()); err != nil {
		return fmt.Errorf("%s's applier did not apply the trimmed record: %w — an "+
			"applier with no case for the kind faults every node's copy at the "+
			"first trim", name, err)
	}
	anchors, err := readAnchors(t.Context(), db)
	if err != nil {
		t.Fatalf("read the anchors: %v", err)
	}
	for _, w := range written[:len(written)-1] {
		// A SUBJECT THE FLOOR'S OWN RECORD ALSO WROTE keeps its anchor, at
		// the floor, which the check below holds: an anchor is one row per
		// subject naming its LAST record, so a candidate whose suite records
		// share a subject — a reanchor's generation, a singleton — leaves
		// nothing below the floor on it to remove.
		if w.env.Subject == kept.env.Subject {
			continue
		}
		if _, ok := anchors[spec.Name][subjectOf(spec, w.env.Subject)]; ok {
			return fmt.Errorf("the anchor of %s at %d is still on %s after a trimmed "+
				"record naming %s applied — every anchor below the floor goes",
				w.env.Subject, w.seq, spec.Name, floor)
		}
	}
	if _, ok := anchors[spec.Name][stale]; ok {
		return fmt.Errorf("an anchor below the floor that no record of this log wrote " +
			"is still there — the record removes what is below its floor, whatever " +
			"wrote it")
	}
	if got := anchors[spec.Name][subjectOf(spec, kept.env.Subject)]; got != floor.Packed() {
		return fmt.Errorf("the anchor at the floor, %s, reads %d after the trimmed "+
			"record, want %d — an anchor at the floor names a record the log still "+
			"holds, and removing it costs its next write a leader read",
			kept.env.Subject, got, floor.Packed())
	}
	if len(anchors[suiteOtherStream]) != 1 {
		return fmt.Errorf("another log's anchors are %v after %s's trimmed record — "+
			"a trimmed record is about its own log alone", anchors[suiteOtherStream], name)
	}
	if after := tableContents(t, db, c.Domain.Tables()); !maps.Equal(before, after) {
		return fmt.Errorf("%s's applier wrote rows of its own for a trimmed record:\n%s\n"+
			"the framework removes the anchors and the domain writes nothing — a "+
			"row here is one every node writes for a record that is about no object",
			name, diffTables(before, after))
	}
	again, err := suiteSweep(t.Context(), deps, floor)
	if err != nil || again.Records != 0 {
		return fmt.Errorf("a second sweep with nothing below the floor appended %d "+
			"(%v), want none — an idle log gains nothing", again.Records, err)
	}

	// THE ZERO FENCE, through the production publisher over the candidate's
	// own read seam, with the applier stopped where it stands.
	loop.stop()
	committed := runner.Committed()
	rows, err := c.Rows(db.Reader(), spec)
	if err != nil {
		t.Fatalf("build %s's read seam: %v", name, err)
	}
	fence := &countingFence{zero: statelog.ZeroFence{
		Evicted:   func(context.Context) (bool, error) { return false, nil },
		Ends:      log.Ends,
		Committed: runner.Committed,
	}}
	var published atomic.Uint64
	fence.zero.Floor = func(context.Context, uint32) (uint64, error) { return published.Load(), nil }
	pub, err := statelog.NewPublisher(statelog.Deps{
		Domain: c.Domain, Spec: spec,
		Log: log, Signer: signer, Rows: rows,
		Fence: fence, Gates: openGates{},
		Records: statelog.VerifiedLog{Log: log, Verifier: verifier},
		Waiter:  suiteWaiter{at: committed}, Voids: suiteWaiter{at: committed},
		Identity:   suiteWaiter{at: committed},
		NodeID:     writer,
		Generation: func() uint32 { return gen },
		Admission:  admissionFor(t, c),
	})
	if err != nil {
		t.Fatalf("build a publisher over %s: %v", name, err)
	}
	write := func(w suiteApplied, op string) (statelog.Result, error) {
		return pub.Publish(t.Context(), statelog.Request{
			Subject: w.env.Subject, Scope: w.env.Scope, OpID: op,
			Pattern: statelog.PatternArbitrated,
			// THE CANDIDATE'S OWN RECORD, stamped with the writer and
			// generation the case runs as.
			Decide: func(_ *sql.Tx, _ statelog.Stamp) (statelog.Decision, error) {
				body, encodeErr := c.Encode(w.rec.kind, w.rec.id, op, 0)
				return statelog.Decision{Payload: body}, encodeErr
			},
		})
	}

	// A FLOOR PUBLISHED ABOVE WHAT THESE ROWS REACH: the subject whose
	// anchor went holds nothing, and the write may not take zero.
	published.Store(committed.Seq + 2)
	appends := len(log.expectations())
	_, err = write(swept, "suite-trimmed-behind")
	var refused *statelog.Unavailable
	switch {
	case !errors.As(err, &refused) || refused.Reason != statelog.ReasonBehind:
		return fmt.Errorf("a write to %s, whose anchor the trim removed, under a "+
			"floor above this node's rows = %v, want a `behind` refusal — publishing "+
			"at zero there is a lost update on a node below the floor", swept.env.Subject, err)
	case len(fence.asked()) != 1:
		return fmt.Errorf("the write was refused with the zero fence asked %d time(s), "+
			"want once — a write to a subject whose anchor the trim removed reaches "+
			"the fence by asking the log what it holds", len(fence.asked()))
	case len(log.expectations()) != appends:
		return fmt.Errorf("a write the zero fence refused appended a record anyway")
	}

	// THE FLOOR THE TRIM PUBLISHED: the fence clears, and the write goes out
	// at zero — decided from rows the sweep had already been applied to.
	published.Store(floor.Seq)
	res, err := write(swept, "suite-trimmed-zero")
	if err != nil || res.Outcome != statelog.OutcomePending {
		return fmt.Errorf("a write to %s once the zero fence clears = %+v, %v, want "+
			"it published (pending, with nothing applying it here)", swept.env.Subject, res, err)
	}
	asked := fence.asked()
	expects := log.expectations()
	switch {
	case len(asked) != 2:
		return fmt.Errorf("the write was published with the zero fence asked %d "+
			"time(s) in all, want twice — a write to a subject whose anchor the "+
			"trim removed was published without asking the zero fence, which is a "+
			"lost update on a node below the floor", len(asked))
	case asked[1].Seq < committed.Seq:
		return fmt.Errorf("the zero fence was asked about rows at %s, below the "+
			"trimmed record this node applied at %s", asked[1], committed)
	case len(expects) == 0 || expects[len(expects)-1] == nil || *expects[len(expects)-1] != 0:
		return fmt.Errorf("the write to %s went out with expectation %v, want zero — "+
			"its anchor is gone and the subject holds nothing", swept.env.Subject,
			derefLast(expects))
	}

	// AND A SUBJECT THAT KEPT ITS ANCHOR arbitrates on it, with no fence.
	res, err = write(kept, "suite-trimmed-kept")
	expects = log.expectations()
	switch {
	case err != nil || res.Outcome != statelog.OutcomePending:
		return fmt.Errorf("a write to %s, whose anchor is at the floor, = %+v, %v, "+
			"want it published", kept.env.Subject, res, err)
	case len(fence.asked()) != 2:
		return fmt.Errorf("a write to a subject whose anchor the trim kept asked the " +
			"zero fence — it arbitrates on the anchor the rows hold")
	case expects[len(expects)-1] == nil || *expects[len(expects)-1] != kept.seq:
		return fmt.Errorf("the write to %s went out with expectation %v, want its "+
			"anchor %d", kept.env.Subject, derefLast(expects), kept.seq)
	}
	return nil
}

// suiteApplied is one of the suite's records as the trimmed-record case wrote
// it: what it is, its envelope, and where it landed.
type suiteApplied struct {
	rec suiteRecord
	env statelog.Envelope
	seq uint64
}

// appendRecords appends records to log in the candidate's own encoding,
// signed by signer as a publisher would have signed them, and answers the ones
// on a subject the log arbitrates — the ones that write an anchor — in order.
func appendRecords(t *testing.T, c Candidate, spec statelog.StreamSpec, log *suiteLog,
	signer *statelog.Signer, records []suiteRecord) []suiteApplied {

	t.Helper()
	var out []suiteApplied
	for _, r := range records {
		body, err := c.Encode(r.kind, r.id, r.op, c.Domain.RecordVersion())
		if err != nil {
			t.Fatalf("encode %s/%s: %v", r.kind, r.id, err)
		}
		env, err := c.Domain.Envelope(body)
		if err != nil {
			t.Fatalf("envelope %s/%s: %v", r.kind, r.id, err)
		}
		seq, _, err := log.Append(t.Context(), subjectOf(spec, env.Subject), r.op, nil, signer.Seal(body))
		if err != nil {
			t.Fatalf("append %s/%s: %v", r.kind, r.id, err)
		}
		if slices.Contains(spec.ArbitratedKinds, env.Subject.Kind) {
			out = append(out, suiteApplied{rec: r, env: env, seq: seq})
		}
	}
	return out
}

// firstArbitrated is the first of the candidate's kinds its log arbitrates.
func firstArbitrated(c Candidate, spec statelog.StreamSpec) string {
	for _, k := range c.Kinds {
		if slices.Contains(spec.ArbitratedKinds, k) {
			return k
		}
	}
	return ""
}

// subjectOf is a subject as spec's log carries it — the grammar's one spelling,
// which the framework's anchors are keyed on.
func subjectOf(spec statelog.StreamSpec, s statelog.Subject) string {
	return topics.LogSubject(spec.SubjectPrefix, s.Kind, s.ID)
}

// plantAnchor writes an anchor row directly, as a node whose applier consumed a
// record on subject at at would have — the suite's one write of a framework
// table, to give the trimmed record something no record of the log wrote.
func plantAnchor(ctx context.Context, db store.ReplicatedHandle, stream, subject string,
	at statelog.Position) error {

	return db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO statelog_anchor (stream, subject, anchor) VALUES (?, ?, ?)`,
			stream, subject, at.Packed())
		return err
	})
}

// readAnchors is every anchor the estate holds, by stream and subject.
func readAnchors(ctx context.Context, db store.ReplicatedHandle) (map[string]map[string]int64, error) {
	out := map[string]map[string]int64{}
	err := db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT stream, subject, anchor FROM statelog_anchor`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var stream, subject string
			var anchor int64
			if err := rows.Scan(&stream, &subject, &anchor); err != nil {
				return err
			}
			if out[stream] == nil {
				out[stream] = map[string]int64{}
			}
			out[stream][subject] = anchor
		}
		return rows.Err()
	})
	return out, err
}

func derefLast(expects []*uint64) any {
	if len(expects) == 0 || expects[len(expects)-1] == nil {
		return nil
	}
	return *expects[len(expects)-1]
}

// countingFence is the production zero fence, counting what it is asked.
type countingFence struct {
	zero statelog.ZeroFence

	mu      sync.Mutex
	cursors []statelog.Position
}

func (f *countingFence) Evicted(ctx context.Context) (bool, error) { return f.zero.Evicted(ctx) }

func (f *countingFence) ClearForZero(ctx context.Context, cursor statelog.Position) error {
	f.mu.Lock()
	f.cursors = append(f.cursors, cursor)
	f.mu.Unlock()
	return f.zero.ClearForZero(ctx, cursor)
}

func (f *countingFence) asked() []statelog.Position {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]statelog.Position(nil), f.cursors...)
}

// loop is one run of a domain's applier, ended by stop — on every path out of
// a case, so no goroutine outlives the test that started it.
type loop struct {
	runner *statelog.Runner
	cancel context.CancelFunc
	done   chan error
	once   sync.Once
}

func startLoop(t *testing.T, runner *statelog.Runner) *loop {
	ctx, cancel := context.WithCancel(t.Context())
	l := &loop{runner: runner, cancel: cancel, done: make(chan error, 1)}
	go func() { l.done <- runner.Run(ctx) }()
	t.Cleanup(l.stop)
	return l
}

func (l *loop) stop() {
	l.once.Do(func() {
		l.cancel()
		<-l.done
	})
}

// waitPast waits until the applier has committed through seq, and refuses a
// stop, a fault or a loop that never gets there.
func (l *loop) waitPast(seq uint64) error {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if l.runner.Committed().Seq >= seq {
			return nil
		}
		if err := l.runner.Stopped(); err != nil {
			return fmt.Errorf("the applier stopped: %w", err)
		}
		// THE FAULT, as soon as there is one: a record the applier
		// refuses is retried for the whole budget before it is reported,
		// and a case that waited that long for the obvious would be slow
		// to say so.
		if msg, faulted := l.runner.Fault(time.Now().Add(statelog.ApplyRetryBudget)); faulted {
			return fmt.Errorf("the applier faulted: %s", msg)
		}
		time.Sleep(2 * time.Millisecond)
	}
	return fmt.Errorf("the applier reached %s, want sequence %d", l.runner.Committed(), seq)
}

// suiteSweep counts the anchors below floor and appends the records they call
// for — the two steps the engine's trim takes — under a budget no wait here
// reaches, so an applier that never commits the record is named by the wait's
// own bound rather than by the budget's designed stop: the case is about what
// the domain does.
func suiteSweep(ctx context.Context, deps statelog.SweepDeps, floor statelog.Position) (statelog.Swept, error) {
	plan, err := statelog.PlanSweep(ctx, deps, floor)
	if err != nil {
		return statelog.Swept{}, err
	}
	return plan.Sweep(ctx, time.Minute)
}
