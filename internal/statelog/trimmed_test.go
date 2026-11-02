package statelog_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// ---- the trimmed record — ADR-0073 ------------------------------------ //

// trimmedEnv is the probe domain's trimmed record naming floor, written by
// node-a — the probe's envelope IS the framework's, so this is
// [statelog.TrimmedEnvelope] verbatim.
func trimmedEnv(floor statelog.Position) statelog.Envelope {
	return statelog.TrimmedEnvelope(floor, "node-a")
}

// seedAnchors writes anchors straight into the estate's table, as a node whose
// applier consumed records on those subjects at those positions would have.
func seedAnchors(t testing.TB, db store.ReplicatedHandle, stream string, anchors map[string]statelog.Position) {
	t.Helper()
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		for subject, at := range anchors {
			if _, err := tx.ExecContext(t.Context(),
				`INSERT INTO statelog_anchor (stream, subject, anchor) VALUES (?, ?, ?)`,
				stream, subject, at.Packed()); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the anchors: %v", err)
	}
}

// seedManyAnchors writes n anchors on stream at generation gen, sequences 1..n,
// in batches a statement can carry.
func seedManyAnchors(t testing.TB, db store.ReplicatedHandle, stream string, gen uint32, n int) {
	t.Helper()
	const batch = 500
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		for from := 1; from <= n; from += batch {
			to := min(from+batch-1, n)
			var q strings.Builder
			q.WriteString(`INSERT INTO statelog_anchor (stream, subject, anchor) VALUES `)
			args := make([]any, 0, 3*(to-from+1))
			for i := from; i <= to; i++ {
				if i > from {
					q.WriteString(", ")
				}
				q.WriteString("(?, ?, ?)")
				args = append(args, stream, fmt.Sprintf("%s.object.seeded-%07d", probePrefix, i),
					statelog.Position{Stream: stream, Generation: gen, Seq: uint64(i)}.Packed())
			}
			if _, err := tx.ExecContext(t.Context(), q.String(), args...); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed %d anchors: %v", n, err)
	}
}

// anchorsOn is every anchor the estate holds on stream, by subject.
func anchorsOn(t testing.TB, db store.ReplicatedHandle, stream string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(),
			`SELECT subject, anchor FROM statelog_anchor WHERE stream = ?`, stream)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var subject string
			var anchor int64
			if err := rows.Scan(&subject, &anchor); err != nil {
				return err
			}
			out[subject] = anchor
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the anchors on %s: %v", stream, err)
	}
	return out
}

// A TRIMMED SUBJECT NAMES ITS FLOOR IN ONE SPELLING, and nothing else parses.
//
// The floor travels in the subject because the framework never decodes a
// payload, so the subject is a wire format: one floor is one broker subject,
// and a spelling the writer could not have produced is refused rather than
// read as a floor nobody meant.
func TestATrimmedSubjectNamesItsFloorInOneSpelling(t *testing.T) {
	t.Parallel()
	for _, floor := range []statelog.Position{
		{Stream: probeStream, Generation: 0, Seq: 0},
		{Stream: probeStream, Generation: 1, Seq: 42},
		{Stream: probeStream, Generation: statelog.MaxGeneration, Seq: statelog.MaxSeq},
	} {
		subject := statelog.TrimmedSubject(floor)
		if subject.Kind != statelog.TrimmedKind {
			t.Errorf("the subject of %s is a %q, want %q", floor, subject.Kind, statelog.TrimmedKind)
		}
		got, err := statelog.TrimmedFloor(probeStream, subject)
		if err != nil || got != floor {
			t.Errorf("TrimmedFloor(%s) = %s, %v, want %s", subject, got, err, floor)
		}
	}
	for name, s := range map[string]statelog.Subject{
		"another kind":             {Kind: "barrier", ID: "1.2"},
		"no id":                    {Kind: statelog.TrimmedKind},
		"one number":               {Kind: statelog.TrimmedKind, ID: "1"},
		"three numbers":            {Kind: statelog.TrimmedKind, ID: "1.2.3"},
		"a leading zero":           {Kind: statelog.TrimmedKind, ID: "01.2"},
		"a sign":                   {Kind: statelog.TrimmedKind, ID: "+1.2"},
		"a word":                   {Kind: statelog.TrimmedKind, ID: "1.x"},
		"an empty half":            {Kind: statelog.TrimmedKind, ID: "1."},
		"a generation out of form": {Kind: statelog.TrimmedKind, ID: fmt.Sprintf("%d.1", uint64(statelog.MaxGeneration)+1)},
		"a sequence out of form":   {Kind: statelog.TrimmedKind, ID: fmt.Sprintf("1.%d", statelog.MaxSeq+1)},
	} {
		if got, err := statelog.TrimmedFloor(probeStream, s); err == nil {
			t.Errorf("%s: TrimmedFloor(%+v) = %s, want a refusal", name, s, got)
		}
	}
}

// A TRIMMED RECORD IS READ BY EVERY BUILD THERE WILL EVER BE, and declares the
// framework's own scope.
//
// Deferred by an older build, it would keep every anchor it names and add a
// deferral row; scoped anywhere else, every read and write it intersected
// would wait behind a record that changes none of their rows.
func TestATrimmedRecordsVersionIsPinnedAtOne(t *testing.T) {
	t.Parallel()
	if statelog.TrimmedVersion != 1 {
		t.Fatalf("TrimmedVersion = %d, want 1 for ever", statelog.TrimmedVersion)
	}
	env := statelog.TrimmedEnvelope(statelog.Position{Stream: probeStream, Generation: 2, Seq: 9}, "node-a")
	if env.V != 1 || env.OpID != "" || env.Writer != "node-a" || env.Gen != 2 ||
		len(env.Scope.Paths) != 1 || env.Scope.Paths[0] != statelog.FrameworkScope {
		t.Fatalf("the trimmed envelope is %+v, want version 1, no operation, the "+
			"writer and the floor's generation stamped, and the framework's scope", env)
	}
}

// A TRIMMED RECORD REMOVES THIS LOG'S ANCHORS BELOW ITS FLOOR — every one, of
// whatever generation and whatever wrote it — and nothing at or above it, and
// nothing on another log.
func TestATrimmedRecordRemovesOnlyTheAnchorsBelowItsFloor(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	const other = "CREWLET_OTHER_LOG"
	old := probePrefix + ".object.old"
	elsewhere := "crewlet.other.log.object.x"
	seedAnchors(t, h.estate, probeStream, map[string]statelog.Position{
		// A REANCHOR'S LEFTOVER: an anchor from the generation before,
		// at a sequence above everything this generation holds.
		old: {Stream: probeStream, Generation: 0, Seq: 500},
	})
	seedAnchors(t, h.estate, other, map[string]statelog.Position{
		elsewhere: {Stream: other, Generation: 1, Seq: 1},
	})
	for seq, id := range map[uint64]string{1: "a", 2: "b", 3: "c"} {
		h.fetch.offer(seq, env(seq, "edit", id, "op-"+id, 1))
	}
	h.fetch.offer(4, trimmedEnv(statelog.Position{Stream: probeStream, Generation: 1, Seq: 3}))
	if err := h.run(4); err != nil {
		t.Fatalf("run: %v", err)
	}

	got := anchorsOn(t, h.estate, probeStream)
	want := map[string]int64{
		probePrefix + ".object.c": statelog.Position{Stream: probeStream, Generation: 1, Seq: 3}.Packed(),
	}
	if len(got) != len(want) || got[probePrefix+".object.c"] != want[probePrefix+".object.c"] {
		t.Fatalf("the anchors on %s after the trimmed record are %v, want only %v — "+
			"every anchor below the floor goes (the earlier generation's too), "+
			"the one at the floor stays, and the record writes none of its own",
			probeStream, got, want)
	}
	if kept := anchorsOn(t, h.estate, other); len(kept) != 1 {
		t.Fatalf("the anchors on another log are %v — a trimmed record is about its "+
			"own log alone", kept)
	}
	if n := h.counter(metrics.StatelogAnchorsSwept); n != 3 {
		t.Fatalf("%s counted %d, want 3", metrics.StatelogAnchorsSwept, n)
	}
}

// A TRIMMED RECORD REMOVES AT MOST ITS BOUND, LOWEST FIRST — so one record never
// holds every node's only writer for a backlog's length, and which anchors one
// removes is the same on every node holding the same table.
func TestATrimmedRecordRemovesAtMostItsBoundLowestFirst(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	extra := 5
	// THE GENERATION BEFORE, so the floor — the first sequence of this
	// one — is above every one of them while the records sit where a
	// fresh log puts them.
	seedManyAnchors(t, h.estate, probeStream, 0, statelog.AnchorsPerTrimmedRecord+extra)
	floor := statelog.Position{Stream: probeStream, Generation: 1, Seq: 1}

	h.fetch.offer(1, trimmedEnv(floor))
	if err := h.run(1); err != nil {
		t.Fatalf("run the first record: %v", err)
	}
	left := anchorsOn(t, h.estate, probeStream)
	if len(left) != extra {
		t.Fatalf("one trimmed record left %d anchors of %d, want %d — it removes at "+
			"most %d", len(left), statelog.AnchorsPerTrimmedRecord+extra, extra,
			statelog.AnchorsPerTrimmedRecord)
	}
	for i := statelog.AnchorsPerTrimmedRecord + 1; i <= statelog.AnchorsPerTrimmedRecord+extra; i++ {
		if _, ok := left[fmt.Sprintf("%s.object.seeded-%07d", probePrefix, i)]; !ok {
			t.Fatalf("the anchors left are %v, want the %d highest — the lowest go "+
				"first", left, extra)
		}
	}

	h.fetch.offer(2, trimmedEnv(floor))
	if err := h.run(2); err != nil {
		t.Fatalf("run the second record: %v", err)
	}
	if left := anchorsOn(t, h.estate, probeStream); len(left) != 0 {
		t.Fatalf("a second record left %v, want none", left)
	}
	if n := h.counter(metrics.StatelogAnchorsSwept); n != uint64(statelog.AnchorsPerTrimmedRecord+extra) {
		t.Fatalf("%s counted %d, want %d", metrics.StatelogAnchorsSwept, n,
			statelog.AnchorsPerTrimmedRecord+extra)
	}
}

// A TRIMMED RECORD A GATE DROPS REMOVES NOTHING, like every gated record: a
// gate's whole promise is that the record produces no rows on any node.
func TestAGatedTrimmedRecordRemovesNoAnchor(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.fetch.offer(1, env(1, "edit", "a", "op-a", 1))
	h.fetch.offer(2, trimmedEnv(statelog.Position{Stream: probeStream, Generation: 1, Seq: 2}))
	h.applier.gate, h.applier.gated[2] = statelog.ReasonEvicted, true
	if err := h.run(2); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := anchorsOn(t, h.estate, probeStream); len(got) != 1 {
		t.Fatalf("the anchors after a gated trimmed record are %v, want a's still "+
			"there", got)
	}
	if n := h.counter(metrics.StatelogAnchorsSwept); n != 0 {
		t.Fatalf("%s counted %d for a record a gate dropped", metrics.StatelogAnchorsSwept, n)
	}
}

// A TRIMMED RECORD THAT NAMES NO FLOOR FAULTS, as a malformed record of a kind
// a domain declares does: its one writer checks every body before appending
// it, so this one came from somewhere this build cannot explain, and applying
// it as nothing would hide that.
func TestATrimmedRecordThatNamesNoFloorFaults(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	bad := trimmedEnv(statelog.Position{Stream: probeStream, Generation: 1, Seq: 1})
	bad.Subject.ID = "x"
	h.fetch.offer(1, bad)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	errs := make(chan error, 1)
	go func() { errs <- h.runner.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, faulted := h.runner.Fault(time.Now().Add(statelog.ApplyRetryBudget)); faulted {
			break
		}
		select {
		case err := <-errs:
			t.Fatalf("Run returned %v — a record that names no floor is a fault, not a stop", err)
		case <-time.After(2 * time.Millisecond):
		}
	}
	msg, faulted := h.runner.Fault(time.Now().Add(statelog.ApplyRetryBudget))
	cancel()
	<-errs
	if !faulted || !strings.Contains(msg, "trimmed") {
		t.Fatalf("Fault = (%q, %v), want a fault naming the trimmed record", msg, faulted)
	}
	if got := h.runner.Committed().Seq; got != 0 {
		t.Fatalf("the checkpoint moved to %d past a trimmed record that names no floor", got)
	}
	if err := h.runner.Stopped(); err != nil {
		t.Fatalf("a malformed trimmed record stopped the applier: %v", err)
	}
}

// THE UNHELD TAIL STEPS OVER A TRIMMED RECORD, as it steps over a barrier: it
// writes no domain row and carries no operation id, so no ledger could ever
// say these rows hold it — and a reanchor of a restored log would refuse over
// a record that wrote nothing until an operator discarded it.
func TestTheUnheldTailStepsOverATrimmedRecord(t *testing.T) {
	t.Parallel()
	h := appliedThrough(t, 3)
	h.fetch.offerStored(4, otherHistory,
		trimmedEnv(statelog.Position{Stream: probeStream, Generation: 1, Seq: 2}))
	if got := unheld(t, h, 4); got != nil {
		t.Fatalf("a trimmed record written after the restore reads as unheld: %v", got)
	}
}

// ---- the sweep that writes them --------------------------------------- //

// sweepLog is an appender that keeps every append, failing from one on. The
// log already holds held records when the sweep begins, and the records it
// appends follow them.
type sweepLog struct {
	mu      sync.Mutex
	held    uint64
	appends []sweepAppend
	failAt  int
	failErr error

	// runner is this node's applier, read at each append: an append made
	// while the record before it is not committed there is counted early.
	runner *sweepRunner
	early  int
}

type sweepAppend struct {
	subject, msgID string
	expect         *uint64
	body           []byte
}

func (l *sweepLog) Append(_ context.Context, subject, msgID string, expect *uint64, body []byte) (uint64, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failErr != nil && len(l.appends)+1 >= l.failAt {
		return 0, false, l.failErr
	}
	if l.runner != nil && len(l.appends) > 0 && l.runner.Reached().Seq < l.last() {
		l.early++
	}
	l.appends = append(l.appends, sweepAppend{subject, msgID, expect, append([]byte(nil), body...)})
	return l.last(), false, nil
}

// last is the log's last sequence; l.mu is held.
func (l *sweepLog) last() uint64 { return l.held + uint64(len(l.appends)) }

func (l *sweepLog) LastSeq(context.Context, string) (uint64, bool, error) { return 0, false, nil }

// Ends is the log's two ends, as the sweep reads them.
func (l *sweepLog) Ends(context.Context) (statelog.LogEnds, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return statelog.LogEnds{First: 1, Last: l.last()}, nil
}

func (l *sweepLog) made() []sweepAppend {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]sweepAppend(nil), l.appends...)
}

// appendedEarly is how many appends were made while the record before them
// was not committed on this node.
func (l *sweepLog) appendedEarly() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.early
}

// sweepRunner is this node's applier of the probe log, as the sweep waits on
// it: a wait commits through the position it names — the applier reaching it
// — unless the position is at or past stuckAt, where the applier never gets
// and the wait fails as its deadline would. Every wait's deadline is noted.
type sweepRunner struct {
	mu        sync.Mutex
	committed uint64
	stuckAt   uint64
	budgets   []time.Duration
	deadlines []time.Time

	// slow is how long the applier takes to reach a position it has not
	// committed: a wait for one succeeds that long after it began, unless
	// its context ends first — or whatever its context says, where late,
	// an applier whose answer arrives after the wait's deadline. Zero
	// reaches it at once.
	slow time.Duration
	late bool
}

func (r *sweepRunner) Reached() statelog.Position {
	r.mu.Lock()
	defer r.mu.Unlock()
	return statelog.Position{Stream: probeStream, Generation: 1, Seq: r.committed}
}

func (r *sweepRunner) WaitReached(ctx context.Context, p statelog.Position) error {
	r.mu.Lock()
	budget := time.Duration(-1)
	deadline, ok := ctx.Deadline()
	if ok {
		budget = time.Until(deadline)
	}
	r.budgets = append(r.budgets, budget)
	r.deadlines = append(r.deadlines, deadline)
	if r.stuckAt != 0 && p.Seq >= r.stuckAt {
		r.mu.Unlock()
		return context.DeadlineExceeded
	}
	slow := r.slow > 0 && p.Seq > r.committed
	late := r.late
	r.mu.Unlock()
	switch {
	case slow && late:
		time.Sleep(r.slow)
	case slow:
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.slow):
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.committed = max(r.committed, p.Seq)
	return nil
}

// waits is the time each wait was handed until its deadline, -1 for none.
func (r *sweepRunner) waits() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.budgets...)
}

// waitDeadlines is the deadline each wait was handed, the zero time for none.
func (r *sweepRunner) waitDeadlines() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.deadlines...)
}

// sweepEstate is a fresh replicated estate with no domain tables — the sweep
// reads only the framework's.
func sweepEstate(t testing.TB) store.ReplicatedHandle {
	t.Helper()
	db, estate := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	return estate
}

// encodeProbe is the probe domain's encoder: its envelope is the framework's.
func encodeProbe(env statelog.Envelope) ([]byte, error) { return json.Marshal(env) }

// sweepDeps is a sweep of the probe log over estate, appending to log, on an
// applier that keeps up with it unless log names one of its own.
func sweepDeps(t testing.TB, estate store.ReplicatedHandle, log *sweepLog) statelog.SweepDeps {
	if log.runner == nil {
		log.runner = &sweepRunner{}
	}
	signer, err := statelog.NewSigner(probeDomain{}.Name(), testRing())
	if err != nil {
		t.Fatal(err)
	}
	return statelog.SweepDeps{
		Domain: probeDomain{}, Spec: specOf(probeDomain{}),
		Log: log, Signer: signer, Ends: log.Ends, Runner: log.runner,
		Admission: noCeiling(t), Encode: encodeProbe,
		DB: estate.Reader(), Writer: "node-a",
	}
}

// noBound is a budget no case reaches but the ones that set their own.
const noBound = time.Hour

// sweep counts the anchors deps' copy holds below floor and appends the
// records they call for — the two steps the engine's trim takes — counting
// under a context bounded by budget, as the engine counts every log under the
// deadline their sweeps share, and appending within budget.
func sweep(t testing.TB, deps statelog.SweepDeps, floor statelog.Position,
	budget time.Duration) (statelog.Swept, error) {

	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), budget)
	plan, err := statelog.PlanSweep(ctx, deps, floor)
	cancel()
	if err != nil {
		return statelog.Swept{}, err
	}
	return plan.Sweep(t.Context(), budget)
}

// THE TRIM APPENDS ONE TRIMMED RECORD PER BOUND OF ANCHORS BELOW THE FLOOR, AT
// MOST ITS CAP, AND NONE WHEN THERE ARE NONE — so an idle fleet's log gains
// nothing, and a backlog drains over the ticks that follow without waiting for
// another purge.
func TestASweepAppendsOneRecordPerBoundAndNoneWhenNothingIsBelow(t *testing.T) {
	t.Parallel()
	floor := statelog.Position{Stream: probeStream, Generation: 1, Seq: 1}
	per := statelog.AnchorsPerTrimmedRecord
	// A COUNT THAT REACHED THE CAP REPORTS THAT MORE REMAIN, and one below it
	// that none do — what the trim's log says of a backlog still draining.
	for _, tc := range []struct {
		anchors, want int
		remain        bool
	}{
		{0, 0, false},
		{1, 1, false},
		{per, 1, false},
		{per + 1, 2, false},
		{statelog.TrimmedRecordsPerSweep*per - 1, statelog.TrimmedRecordsPerSweep, false},
		{statelog.TrimmedRecordsPerSweep*per + 1, statelog.TrimmedRecordsPerSweep, true},
	} {
		t.Run(fmt.Sprintf("%d anchors", tc.anchors), func(t *testing.T) {
			t.Parallel()
			estate := sweepEstate(t)
			seedManyAnchors(t, estate, probeStream, 0, tc.anchors)
			// ONE AT THE FLOOR, which is not below it.
			seedAnchors(t, estate, probeStream, map[string]statelog.Position{
				probePrefix + ".object.kept": floor,
			})
			log := &sweepLog{}
			plan, err := statelog.PlanSweep(t.Context(), sweepDeps(t, estate, log), floor)
			if err != nil {
				t.Fatalf("PlanSweep: %v", err)
			}
			if plan.Records() != tc.want || len(log.made()) != 0 {
				t.Fatalf("the plan over %d anchors below the floor counts %d records and "+
					"appended %d, want %d counted and nothing appended until it sweeps",
					tc.anchors, plan.Records(), len(log.made()), tc.want)
			}
			swept, err := plan.Sweep(t.Context(), noBound)
			if err != nil {
				t.Fatalf("Sweep: %v", err)
			}
			made := log.made()
			if swept.Records != tc.want || len(made) != tc.want {
				t.Fatalf("the sweep answered %d and appended %d over %d anchors "+
					"below the floor, want %d", swept.Records, len(made), tc.anchors, tc.want)
			}
			if swept.Remain != tc.remain {
				t.Fatalf("a sweep over %d anchors below the floor said more remain=%v, "+
					"want %v", tc.anchors, swept.Remain, tc.remain)
			}
			for _, a := range made {
				if a.msgID != "" || a.expect != nil {
					t.Fatalf("a trimmed record was appended with message id %q and "+
						"expectation %v — identical records are each meant to land",
						a.msgID, a.expect)
				}
				if want := probePrefix + "." + statelog.TrimmedSubject(floor).String(); a.subject != want {
					t.Fatalf("a trimmed record went to %q, want %q", a.subject, want)
				}
				// SIGNED, as every record on the log is: the applier
				// opens the frame before it reads the envelope.
				body, verdict := testVerifier(t, probeDomain{}).Open(a.body)
				if verdict != statelog.Verified {
					t.Fatalf("a trimmed record's frame opens as %s, want %s",
						verdict, statelog.Verified)
				}
				env, err := probeDomain{}.Envelope(body)
				if err != nil || env.Subject != statelog.TrimmedSubject(floor) ||
					env.Writer != "node-a" || env.Gen != floor.Generation {
					t.Fatalf("a trimmed record reads back as %+v (%v)", env, err)
				}
			}
		})
	}
}

// THE SWEEP REFUSES WHAT IT COULD NOT DO RIGHT, before it appends anything —
// the plan what it could not count or would not recognise, the sweep a budget
// that bounds nothing.
func TestASweepRefusesBeforeItAppends(t *testing.T) {
	t.Parallel()
	floor := statelog.Position{Stream: probeStream, Generation: 1, Seq: 1}
	estate := sweepEstate(t)
	seedManyAnchors(t, estate, probeStream, 0, 3)

	for name, tc := range map[string]struct {
		mutate func(*statelog.SweepDeps)
		floor  statelog.Position
		// unbounded hands the sweep a budget of zero.
		unbounded bool
	}{
		"a body the framework would not recognise": {mutate: func(d *statelog.SweepDeps) {
			d.Encode = func(env statelog.Envelope) ([]byte, error) {
				env.Subject.ID = ""
				return json.Marshal(env)
			}
		}},
		"a body carrying an operation": {mutate: func(d *statelog.SweepDeps) {
			d.Encode = func(env statelog.Envelope) ([]byte, error) {
				env.OpID = "op-1"
				return json.Marshal(env)
			}
		}},
		"a body no build at version 1 reads": {mutate: func(d *statelog.SweepDeps) {
			d.Encode = func(env statelog.Envelope) ([]byte, error) {
				env.V = 2
				return json.Marshal(env)
			}
		}},
		"no admission on a log that keeps a reserve": {mutate: func(d *statelog.SweepDeps) {
			d.Admission = nil
		}},
		"no signer, so its records would be unsigned": {mutate: func(d *statelog.SweepDeps) {
			d.Signer = nil
		}},
		"no writer":                    {mutate: func(d *statelog.SweepDeps) { d.Writer = "" }},
		"no time budget":               {unbounded: true},
		"no reading of the log's ends": {mutate: func(d *statelog.SweepDeps) { d.Ends = nil }},
		"no applier to wait on":        {mutate: func(d *statelog.SweepDeps) { d.Runner = nil }},
		"a floor on another log":       {floor: statelog.Position{Stream: "CREWLET_OTHER_LOG", Generation: 1, Seq: 1}},
		"an eviction that cannot be read": {mutate: func(d *statelog.SweepDeps) {
			d.Evicted = func(context.Context) (bool, error) { return false, errors.New("unreadable") }
		}},
		"a log that writes no anchor": {mutate: func(d *statelog.SweepDeps) {
			d.Domain = unarbitrated{}
			d.Spec = unarbitrated{}.Stream()
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			log := &sweepLog{}
			deps := sweepDeps(t, estate, log)
			if tc.mutate != nil {
				tc.mutate(&deps)
			}
			at := floor
			if tc.floor != (statelog.Position{}) {
				at = tc.floor
			}
			budget := noBound
			if tc.unbounded {
				budget = 0
			}
			// COUNTED UNDER NO BOUND OF THE BUDGET'S, so a budget of zero
			// reaches the sweep that refuses it.
			plan, err := statelog.PlanSweep(t.Context(), deps, at)
			if err == nil {
				var swept statelog.Swept
				if swept, err = plan.Sweep(t.Context(), budget); err == nil {
					t.Fatalf("the sweep answered %d with no error", swept.Records)
				}
			}
			if made := log.made(); len(made) != 0 {
				t.Fatalf("a refused sweep appended %d records", len(made))
			}
		})
	}
}

// unarbitrated is the probe domain on a log that arbitrates nothing.
type unarbitrated struct{ probeDomain }

func (unarbitrated) Stream() statelog.StreamSpec {
	s := probeDomain{}.Stream()
	s.ArbitratedKinds = nil
	return s
}

// AN EVICTED NODE APPENDS NO TRIMMED RECORD, and says so with no error: every
// applier would drop it under the eviction gate and count it toward the
// `records_gated` alarm.
func TestAnEvictedNodeSweepsNothing(t *testing.T) {
	t.Parallel()
	estate := sweepEstate(t)
	seedManyAnchors(t, estate, probeStream, 0, 3)
	log := &sweepLog{}
	deps := sweepDeps(t, estate, log)
	deps.Evicted = func(context.Context) (bool, error) { return true, nil }
	swept, err := sweep(t, deps, statelog.Position{Stream: probeStream, Generation: 1, Seq: 1}, noBound)
	if err != nil || swept.Records != 0 || len(log.made()) != 0 {
		t.Fatalf("an evicted node's sweep = %d, %v, appending %d", swept.Records, err, len(log.made()))
	}
}

// A FULL LOG IS NAMED AS ONE, and the records already appended are counted.
func TestASweepOnAFullLogSaysSoAndCountsWhatLanded(t *testing.T) {
	t.Parallel()
	estate := sweepEstate(t)
	seedManyAnchors(t, estate, probeStream, 0, 3*statelog.AnchorsPerTrimmedRecord)
	log := &sweepLog{failAt: 2, failErr: fullLogError()}
	swept, err := sweep(t, sweepDeps(t, estate, log),
		statelog.Position{Stream: probeStream, Generation: 1, Seq: 1}, noBound)
	var refused *statelog.Unavailable
	if !errors.As(err, &refused) || refused.Reason != statelog.ReasonLogFull {
		t.Fatalf("a sweep on a full log = %v, want a log_full refusal", err)
	}
	if swept.Records != 1 {
		t.Fatalf("the sweep counted %d records appended, want the 1 that landed", swept.Records)
	}
}

// EACH TRIMMED RECORD IS APPENDED ONLY ONCE THIS NODE HAS COMMITTED THE ONE
// BEFORE IT — so a write or a barrier appended behind a sweep waits one
// record's apply, never a run of up to the cap of them, on every node.
func TestASweepAppendsEachRecordOnlyOnceTheOneBeforeHasCommittedHere(t *testing.T) {
	t.Parallel()
	estate := sweepEstate(t)
	seedManyAnchors(t, estate, probeStream, 0, 3*statelog.AnchorsPerTrimmedRecord)
	log := &sweepLog{held: 7}
	deps := sweepDeps(t, estate, log)
	swept, err := sweep(t, deps, statelog.Position{Stream: probeStream, Generation: 1, Seq: 1}, noBound)
	if err != nil || swept.Records != 3 || swept.Remain {
		t.Fatalf("the sweep = %+v, %v, want 3 records and none remaining", swept, err)
	}
	if early := log.appendedEarly(); early != 0 {
		t.Fatalf("%d trimmed record(s) were appended while the one before was not "+
			"committed on this node — a write behind the sweep waits for every "+
			"record in the run", early)
	}
	if got := log.runner.Reached().Seq; got != 10 {
		t.Fatalf("the sweep returned with this node committed through %d, want 10 — "+
			"its last record applied here", got)
	}
}

// A COMMIT WAIT THAT FAILS ENDS THE SWEEP WITH WHAT IT APPENDED: the next record
// would queue behind one this node has not applied, and the count says how many
// landed.
func TestASweepWhoseCommitWaitFailsStopsWithWhatItAppended(t *testing.T) {
	t.Parallel()
	estate := sweepEstate(t)
	seedManyAnchors(t, estate, probeStream, 0, 3*statelog.AnchorsPerTrimmedRecord)
	log := &sweepLog{runner: &sweepRunner{stuckAt: 2}}
	swept, err := sweep(t, sweepDeps(t, estate, log),
		statelog.Position{Stream: probeStream, Generation: 1, Seq: 1}, noBound)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a sweep whose second record never applied here = %v, want the "+
			"wait's own failure", err)
	}
	if made := len(log.made()); swept.Records != 2 || made != 2 {
		t.Fatalf("the sweep answered %d and appended %d, want 2 of 3 — it stops at "+
			"the record this node did not apply", swept.Records, made)
	}
}

// A SWEEP COUNTS ONLY FROM A COPY THAT HAS APPLIED THE WHOLE LOG. Counted from a
// copy behind the records already on the log — its own last sweep's, or the
// last duty holder's — it would append them again, every tick it stayed
// behind, for records that remove nothing anywhere else.
func TestASweepFromACopyBehindTheLogAppendsNothing(t *testing.T) {
	t.Parallel()
	estate := sweepEstate(t)
	seedManyAnchors(t, estate, probeStream, 0, 2*statelog.AnchorsPerTrimmedRecord)
	// THE LAST SWEEP'S TWO RECORDS ARE ON THE LOG, at 9 and 10, and this
	// node's applier stopped at 8.
	log := &sweepLog{held: 10, runner: &sweepRunner{committed: 8, stuckAt: 9}}
	swept, err := sweep(t, sweepDeps(t, estate, log),
		statelog.Position{Stream: probeStream, Generation: 1, Seq: 1}, noBound)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a sweep from a copy behind the log = %d, %v, want a refusal naming "+
			"the wait", swept.Records, err)
	}
	if swept.Records != 0 || len(log.made()) != 0 {
		t.Fatalf("a sweep from a copy behind the log answered %d and appended %d, "+
			"want none", swept.Records, len(log.made()))
	}
}

// EVERY WAIT THE SWEEP MAKES IS BOUNDED BY THE RESOLVE BUDGET, the bound a write
// waits for its own applier by: the trim's tick has other logs to sweep, and a
// node whose applier cannot reach what it just appended has a fault that
// waiting does not fix. And by the caller's bound where that is shorter — the
// plan's context, the sweep's budget — so no wait outlasts either.
func TestEveryWaitASweepMakesIsBounded(t *testing.T) {
	t.Parallel()
	for _, budget := range []time.Duration{time.Hour, statelog.DefaultResolveBudget / 2} {
		t.Run(budget.String(), func(t *testing.T) {
			t.Parallel()
			estate := sweepEstate(t)
			seedManyAnchors(t, estate, probeStream, 0, 2*statelog.AnchorsPerTrimmedRecord)
			log := &sweepLog{}
			deps := sweepDeps(t, estate, log)
			if _, err := sweep(t, deps,
				statelog.Position{Stream: probeStream, Generation: 1, Seq: 1}, budget); err != nil {
				t.Fatalf("the sweep: %v", err)
			}
			waits := log.runner.waits()
			if len(waits) != 3 {
				t.Fatalf("the sweep waited %d time(s), want 3 — once for the log's end "+
					"and once for each of its two records", len(waits))
			}
			bound := min(budget, statelog.DefaultResolveBudget)
			for i, left := range waits {
				if left <= 0 || left > bound {
					t.Fatalf("wait %d was handed %v, want a deadline within %v", i, left, bound)
				}
			}
		})
	}
}

// A SWEEP STOPS APPENDING ONCE ITS BUDGET IS SPENT, and answers what it appended
// with no error — the designed stop, whose remainder the next sweep counts.
//
// On an applier that is slow but still moving, every wait succeeds inside the
// resolve budget, so pacing alone would hold the sweep's caller for the cap
// times each wait: the engine's trim, on the goroutine that also evaluates the
// holder's alarms. Here a record takes a fifth of the budget to apply and the
// backlog is twenty records, so a sweep held to its budget appends a handful
// and one held to its cap all twenty. EVERY WAIT ENDS AT ONE DEADLINE, the
// sweep's, so no wait outlasts it whatever the resolve budget allows.
//
// THE BUDGET BOUNDS THE APPENDS ALONE: the count of five thousand anchors is
// PlanSweep's, under the caller's context, and over before the budget begins.
// It is still SECONDS, with a record a fifth of it, so the store work each
// record does beside its apply — under the race detector, beside this
// package's other parallel cases — stays a small share of a record, and a
// stall of a scheduler's making cannot spend the budget before the first
// record lands.
func TestASweepStopsAppendingOnceItsBudgetIsSpent(t *testing.T) {
	t.Parallel()
	const records = 20
	const budget, slow = 2 * time.Second, 400 * time.Millisecond
	estate := sweepEstate(t)
	seedManyAnchors(t, estate, probeStream, 0, records*statelog.AnchorsPerTrimmedRecord)
	// THE LOG'S END IS ALREADY COMMITTED HERE, so planning waits on nothing
	// slow and every slow wait below is a record's.
	log := &sweepLog{held: 4, runner: &sweepRunner{committed: 4, slow: slow}}
	plan, err := statelog.PlanSweep(t.Context(), sweepDeps(t, estate, log),
		statelog.Position{Stream: probeStream, Generation: 1, Seq: 1})
	if err != nil {
		t.Fatalf("PlanSweep: %v", err)
	}
	counted := len(log.runner.waitDeadlines())
	started := time.Now()
	swept, err := plan.Sweep(t.Context(), budget)
	returned := time.Now()
	if err != nil || !swept.Remain {
		t.Fatalf("a sweep whose budget ran out = %+v, %v, want no error and more "+
			"remaining — running out is the stop the budget exists for, and the "+
			"next sweep goes on", swept, err)
	}
	if n, made := swept.Records, len(log.made()); n != made || n == 0 || n >= records {
		t.Fatalf("the sweep answered %d and appended %d of a %d-record backlog, want "+
			"the same handful — at %v a record, a %v budget is not the whole backlog",
			n, made, records, slow, budget)
	}
	deadlines := log.runner.waitDeadlines()[counted:]
	for i, deadline := range deadlines {
		if !deadline.Equal(deadlines[0]) {
			t.Fatalf("wait %d ended at %v and the first at %v — every wait ends at "+
				"the sweep's one deadline, or a wait begun late in the budget "+
				"outlasts it", i, deadline, deadlines[0])
		}
	}
	if end := deadlines[0]; end.Before(started.Add(budget)) || end.After(returned) {
		t.Fatalf("the sweep's waits ended %v after it began and it returned %v after, "+
			"want a %v budget that it ran until", end.Sub(started), returned.Sub(started),
			budget)
	}
}

// NO RECORD IS APPENDED ONCE THE BUDGET IS SPENT, even after a wait that
// succeeded: an applier whose answer arrives as the budget runs out has
// committed the record before, and the next record would still begin past the
// budget. On a log that keeps a gate reserve the admission would refuse it;
// on one that keeps none, nothing between the wait and the append would.
func TestASweepAppendsNoRecordPastItsBudget(t *testing.T) {
	t.Parallel()
	for name, domain := range map[string]statelog.Domain{
		"a log that keeps a gate reserve": probeDomain{},
		"a log that keeps none":           unreservedDomain{},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			estate := sweepEstate(t)
			seedManyAnchors(t, estate, probeStream, 0, 5*statelog.AnchorsPerTrimmedRecord)
			log := &sweepLog{held: 4, runner: &sweepRunner{committed: 4,
				slow: 300 * time.Millisecond, late: true}}
			deps := sweepDeps(t, estate, log)
			deps.Domain, deps.Spec = domain, specOf(domain)
			if !statelog.ReservationOf(domain).Kept {
				deps.Admission = nil
			}
			swept, err := sweep(t, deps,
				statelog.Position{Stream: probeStream, Generation: 1, Seq: 1}, 100*time.Millisecond)
			if err != nil || swept.Records != 1 || !swept.Remain || len(log.made()) != 1 {
				t.Fatalf("a sweep whose first record committed past its budget = %+v, "+
					"%v, appending %d — want that one record, no other and more "+
					"remaining", swept, err, len(log.made()))
			}
		})
	}
}

// A RECORD WHOSE ADMISSION IS STILL WAITING WHEN THE BUDGET RUNS OUT IS NOT
// APPENDED, and the sweep stops with no error: the admission waits on this
// node's other appends, and how long the sweep may wait on anything is the
// budget's to decide.
func TestASweepWhoseAdmissionOutlastsItsBudgetStops(t *testing.T) {
	t.Parallel()
	estate := sweepEstate(t)
	seedManyAnchors(t, estate, probeStream, 0, 2*statelog.AnchorsPerTrimmedRecord)
	log := &sweepLog{}
	deps := sweepDeps(t, estate, log)
	deps.Admission = waitingAdmission{}
	swept, err := sweep(t, deps,
		statelog.Position{Stream: probeStream, Generation: 1, Seq: 1}, 50*time.Millisecond)
	if err != nil || swept.Records != 0 || !swept.Remain || len(log.made()) != 0 {
		t.Fatalf("a sweep whose admission outlasted its budget = %+v, %v, appending "+
			"%d — want nothing appended, no error and more remaining", swept, err,
			len(log.made()))
	}
}

// waitingAdmission admits nothing until its context ends: a log whose room is
// held by appends that have not been answered.
type waitingAdmission struct{}

func (waitingAdmission) Admit(ctx context.Context, _ int64) (func(), error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// fullLogError is the broker's refusal of an append past the stream's byte
// ceiling, as the client hands it back.
func fullLogError() error {
	return &jetstream.APIError{ErrorCode: 10077, Description: "maximum bytes exceeded"}
}
