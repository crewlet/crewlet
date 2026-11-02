package engine

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// EVERY LOG THAT WRITES ANCHORS HAS A TRIMMED RECORD, AND ONLY THOSE — ADR-0073.
//
// The trim removes a log's anchors below what it no longer holds by appending
// the domain's own trimmed record, which [trimmedEncoder] is the switch for. A
// domain registered with anchors and no case there keeps an anchor for every
// subject it ever wrote, on every data node, for the life of the deployment;
// one with a case and no anchors appends a record each tick that removes
// nothing. And the encoder each case names is the one the framework can use.
func TestEveryLogThatWritesAnchorsHasATrimmedRecord(t *testing.T) {
	t.Parallel()
	for _, d := range registeredDomains() {
		encode := trimmedEncoder(d)
		if statelog.WritesAnchors(d.Stream()) != (encode != nil) {
			t.Errorf("%s writes anchors=%v and has a trimmed-record encoder=%v — "+
				"the two must agree", d.Name(), statelog.WritesAnchors(d.Stream()), encode != nil)
			continue
		}
		if encode == nil {
			continue
		}
		if err := statelog.CheckTrimmed(d, d.Stream(), encode); err != nil {
			t.Errorf("%s's trimmed record is one the framework cannot use: %v", d.Name(), err)
		}
	}
}

// A TICK'S ANCHOR SWEEPS SHARE ONE ALARM INTERVAL — ADR-0073.
//
// The sweeps run on the trim's own goroutine ([retention.run]), and each is
// paced on this node's applier, so an applier slow but still moving would
// otherwise hold that goroutine — and hand every slower data node a backlog —
// for the cap times every wait, log after log. So the logs that have anything
// to sweep share one [statelog.AlarmInterval] ([anchorSweepDeadline],
// [runSweeps]): each is handed an equal share of what is left among the
// sweeps still to run, and together they hold the goroutine for at most one
// interval, however many logs write anchors. Every log that writes anchors is
// handed a sweep, and a log that writes none is handed none.
func TestATicksAnchorSweepsShareOneAlarmInterval(t *testing.T) {
	t.Parallel()
	r := &retention{state: everyRegisteredLog(), nodeID: "node-a",
		estate: func() statelog.ReadEstate { return nil }}
	writers := 0
	for _, running := range r.state.running() {
		_, sweeps := r.sweepDeps(running)
		if writes := statelog.WritesAnchors(running.spec); sweeps != writes {
			t.Errorf("%s writes anchors=%v and is handed a sweep=%v — the two must agree",
				running.domain.Name(), writes, sweeps)
		}
		if sweeps {
			writers++
		}
	}
	if writers < 2 {
		t.Fatalf("%d registered log(s) write anchors — the case needs the ones that "+
			"share the interval today", writers)
	}

	// EVERY LOG BUSY AND EVERY SWEEP SPENDING ITS WHOLE BUDGET: equal shares,
	// and together exactly the interval.
	clock := &sweepClock{at: time.Unix(0, 0)}
	sweeps := make([]*fakeSweep, writers)
	counted := make([]countedSweep, writers)
	for i := range sweeps {
		sweeps[i] = &fakeSweep{records: 3, clock: clock, spends: time.Hour}
		counted[i] = countedSweep{domain: fmt.Sprint("log-", i), plan: sweeps[i]}
	}
	runSweeps(t.Context(), clock.at.Add(anchorSweepDeadline), clock.now, counted)
	var total time.Duration
	for i, s := range sweeps {
		// AN EQUAL SHARE OF WHAT IS LEFT, so the nanoseconds a division
		// drops go to the last.
		if want := (statelog.AlarmInterval - total) / time.Duration(writers-i); s.budget != want {
			t.Errorf("busy log %d was handed %v, want %v — an equal share of one alarm "+
				"interval among the %d logs with anything to sweep", i, s.budget, want, writers)
		}
		total += s.budget
	}
	if total > statelog.AlarmInterval || clock.at.Sub(time.Unix(0, 0)) > statelog.AlarmInterval {
		t.Errorf("a tick's sweeps were handed %v together and took %v, past one alarm "+
			"interval (%v) — the holder's alarms would go unevaluated for longer",
			total, clock.at.Sub(time.Unix(0, 0)), statelog.AlarmInterval)
	}

	// A SWEEP THAT FINISHES EARLY LEAVES WHAT IT DID NOT SPEND to the sweeps
	// after it, and the deadline still holds every one.
	clock = &sweepClock{at: time.Unix(0, 0)}
	quick := &fakeSweep{records: 1, clock: clock, spends: time.Second}
	slow := &fakeSweep{records: 9, clock: clock, spends: time.Hour}
	runSweeps(t.Context(), clock.at.Add(anchorSweepDeadline), clock.now, []countedSweep{
		{domain: "quick", plan: quick}, {domain: "slow", plan: slow},
	})
	if quick.budget != statelog.AlarmInterval/2 || slow.budget != statelog.AlarmInterval-time.Second {
		t.Errorf("a sweep done in a second was handed %v and the one after it %v, want "+
			"%v and %v — what one sweep leaves goes to the sweeps after it",
			quick.budget, slow.budget, statelog.AlarmInterval/2, statelog.AlarmInterval-time.Second)
	}

	// COUNTS THAT SPENT THE INTERVAL leave nothing to append in: no sweep is
	// handed a budget that bounds nothing.
	clock = &sweepClock{at: time.Unix(0, 0)}
	late := &fakeSweep{records: 2, clock: clock}
	runSweeps(t.Context(), clock.at, clock.now, []countedSweep{{domain: "late", plan: late}})
	if late.called {
		t.Errorf("a sweep was run with %v left of the interval", late.budget)
	}
}

// AN IDLE LOG LEAVES ITS SHARE TO THE OTHERS — ADR-0073. A log with nothing
// below its floor this tick is counted and never handed a share, wherever it
// sits in the order: an equal share for every log that writes anchors spent a
// share on each idle one, so registering the code log cut the tracker's sweep
// from five seconds a tick to three and a third on every node, a company with
// no repositories at all included.
func TestAnIdleLogLeavesItsShareToTheOthers(t *testing.T) {
	t.Parallel()
	for name, order := range map[string][]int{
		"idle before":        {0, 0, 5},
		"idle after":         {5, 0, 0},
		"idle on both sides": {0, 5, 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			clock := &sweepClock{at: time.Unix(0, 0)}
			var counted []countedSweep
			var busy, idle []*fakeSweep
			for i, records := range order {
				s := &fakeSweep{records: records, clock: clock, spends: time.Hour}
				counted = append(counted, countedSweep{domain: fmt.Sprint("log-", i), plan: s})
				if records > 0 {
					busy = append(busy, s)
				} else {
					idle = append(idle, s)
				}
			}
			runSweeps(t.Context(), clock.at.Add(anchorSweepDeadline), clock.now, counted)
			if busy[0].budget != statelog.AlarmInterval {
				t.Errorf("the one busy log among %d was handed %v, want the whole %v — an "+
					"idle log costs the others nothing", len(order), busy[0].budget, statelog.AlarmInterval)
			}
			for _, s := range idle {
				if s.called {
					t.Errorf("an idle log was handed a sweep of %v", s.budget)
				}
			}
		})
	}
}

// sweepClock is the time a fake sweep spends, as [runSweeps] reads it.
type sweepClock struct{ at time.Time }

func (c *sweepClock) now() time.Time { return c.at }

// fakeSweep is a counted sweep of records records that takes spends of the
// budget it is handed, or all of it where spends is more, and remembers the
// budget.
type fakeSweep struct {
	records int
	spends  time.Duration
	clock   *sweepClock
	called  bool
	budget  time.Duration
}

func (f *fakeSweep) Records() int { return f.records }

func (f *fakeSweep) Sweep(_ context.Context, budget time.Duration) (statelog.Swept, error) {
	f.called, f.budget = true, budget
	f.clock.at = f.clock.at.Add(min(f.spends, budget))
	return statelog.Swept{Records: f.records, Remain: f.spends > budget}, nil
}

// trimLog is the tick's two steps for one log — its trim, then its anchor
// sweep — as [retention.tick] takes them for every log.
func trimLog(ctx context.Context, r *retention, running *runningLog, shared fleetInputs) error {
	below, err := r.domain(ctx, running, shared)
	if err != nil {
		return err
	}
	r.sweepAnchors(ctx, []sweepTarget{{running: running, below: below}})
	return nil
}

// anchorsOf is every anchor this node's copy holds on stream, by subject.
func anchorsOf(t *testing.T, e *Engine, stream string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	if err := e.core.Load().log.estate().Reader().Read(t.Context(), func(tx *sql.Tx) error {
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

// sweepingRetention is the trim of e's runtime, as [Engine.startRetention]
// builds it, over the given register.
func sweepingRetention(e *Engine, fleet coord.Fleet) *retention {
	s := e.core.Load().log
	return &retention{
		fleet: fleet, state: s, nodeID: s.nodeID,
		estate: func() statelog.ReadEstate { return s.estate().Reader() },
	}
}

// A TICK REMOVES THE ANCHORS BELOW WHAT IT PURGED, on a real stream, through
// the tick's own per-log step ([retention.domain]): every term permits the
// purge to the log's newest record, so the tick publishes, purges, and then
// appends the trimmed record its own copy's count calls for BELOW WHAT IT JUST
// PURGED TO — and every anchor below the log's first surviving record goes
// once the node applies it.
//
// The anchors are of records the log held when the tick began, so a tick that
// handed the sweep the log's first record from BEFORE its purge would find
// nothing below it, append nothing, and leave them for the next tick.
func TestATickRemovesTheAnchorsBelowWhatItPurged(t *testing.T) {
	t.Parallel()
	e, back, running := trimmedTracker(t)
	at := time.Now().UTC()
	for _, key := range []string{"ENG", "OPS"} {
		mustApply(t, "the project "+key, func() (worklet.WriteResult, error) {
			return e.native.Load().writer.WriteDocument(t.Context(), "op-"+key,
				worklet.ProjectSubject(key), "", worklet.Project{
					V: worklet.DocumentVersion, Key: key, Name: "Project " + key,
					CreatedAt: at, UpdatedAt: at,
				}, worklet.ChangeProjectCreated, nil)
		})
	}
	// THE NEWEST RECORD IS NOT AN OBJECT'S, so the purge to it removes the
	// records of every anchor this node holds.
	to := barrierOn(t, running)
	stream, name := running.spec.Name, running.domain.Name()
	held := anchorsOf(t, e, stream)
	if len(held) < 2 {
		t.Fatalf("the log's writes left %d anchors, want at least the two projects'", len(held))
	}
	waitUntil(t, 20*time.Second, "the node to apply its own log", func() bool {
		return running.runner.Committed().Seq >= to
	})
	group := running.domain.FeedGroup()
	waitUntil(t, 20*time.Second, "the log's own feed to acknowledge its newest record",
		func() bool {
			floor, exists, err := running.log.GroupAckFloor(t.Context(), group)
			return err == nil && exists && floor >= to
		})
	before, _, err := running.log.Bounds(t.Context())
	if err != nil {
		t.Fatalf("bounds: %v", err)
	}
	committed := running.runner.Committed()
	floor := statelog.Position{Stream: stream, Generation: committed.Generation, Seq: to}.Packed()
	below := 0
	for _, anchor := range held {
		if anchor < floor {
			below++
		}
	}
	if before >= to || below == 0 {
		t.Fatalf("the log starts at %d with %d anchors below its newest record %d — "+
			"the case needs records to purge and anchors of them", before, below, to)
	}

	// EVERY TERM PERMITS THE PURGE TO THE NEWEST RECORD: the age term at a
	// nanosecond, this node counted at its committed position, a verified
	// backup covering the record, no holds, and the feed acknowledged above.
	r := sweepingRetention(e, back.Fleet)
	r.cfg = config.TrackerRetention{MinAgeRaw: "1ns"}
	floors, err := fleetFloors(t.Context(), back.Fleet)
	if err != nil {
		t.Fatalf("floors: %v", err)
	}
	previous := map[string]coord.TrimFloor{}
	for _, f := range floors {
		previous[f.Domain] = f
	}
	now := time.Now().UTC()
	shared := fleetInputs{
		at: now, readable: true, previous: previous,
		positions: []coord.NodePositions{{
			NodeID: r.nodeID, At: now,
			Domains: map[string]coord.DomainPosition{name: {
				Generation: committed.Generation, Seq: committed.Seq,
			}},
		}},
		backups: []coord.BackupPoint{{
			Owner: r.nodeID, At: now, Verified: true,
			Streams: map[string]coord.Position{stream: {
				Stream: stream, Generation: committed.Generation, Seq: to,
			}},
		}},
	}
	if err := trimLog(t.Context(), r, running, shared); err != nil {
		t.Fatalf("the tick: %v", err)
	}

	start, end, err := running.log.Bounds(t.Context())
	if err != nil {
		t.Fatalf("bounds: %v", err)
	}
	if start != to {
		t.Fatalf("after the tick the log starts at %d, want the %d it purged to", start, to)
	}
	if want := to + uint64((below+statelog.AnchorsPerTrimmedRecord-1)/statelog.AnchorsPerTrimmedRecord); end != want {
		t.Fatalf("the log ends at %d after the tick, want %d — one trimmed record per "+
			"%d anchors below what the tick purged to, appended in the same tick",
			end, want, statelog.AnchorsPerTrimmedRecord)
	}
	waitUntil(t, 20*time.Second, "the node to apply the trimmed record", func() bool {
		return running.runner.Committed().Seq >= end
	})
	for subject, anchor := range anchorsOf(t, e, stream) {
		if anchor < floor {
			t.Errorf("%s's anchor %d is below what the tick purged to, %d, after its "+
				"trimmed record applied", subject, anchor, floor)
		}
	}
}

// A TICK THAT PURGES NOTHING STILL SWEEPS BELOW THE LOG'S OWN FIRST RECORD, so
// a backlog an earlier tick's cap left — or anchors whose sweep failed after
// their purge — go on the next tick without waiting for another purge.
//
// THROUGH THE TICK'S OWN PER-LOG STEP ([retention.domain]), whose terms block
// the purge here — every record is younger than the minimum age — while the
// log, purged by an earlier tick, no longer holds the projects' records.
func TestATickThatPurgesNothingSweepsBelowTheLogsFirstRecord(t *testing.T) {
	t.Parallel()
	e, back, running := trimmedTracker(t)
	at := time.Now().UTC()
	mustApply(t, "the project", func() (worklet.WriteResult, error) {
		return e.native.Load().writer.WriteDocument(t.Context(), "op-ENG",
			worklet.ProjectSubject("ENG"), "", worklet.Project{
				V: worklet.DocumentVersion, Key: "ENG", Name: "Engineering",
				CreatedAt: at, UpdatedAt: at,
			}, worklet.ChangeProjectCreated, nil)
	})
	last := barrierOn(t, running)
	waitUntil(t, 20*time.Second, "the node to apply its own log", func() bool {
		return running.runner.Committed().Seq >= last
	})
	if err := running.log.Purge(t.Context(), last); err != nil {
		t.Fatalf("an earlier tick's purge: %v", err)
	}

	r := sweepingRetention(e, back.Fleet)
	shared, err := r.read(t.Context())
	if err != nil {
		t.Fatalf("read the fleet's inputs: %v", err)
	}
	if err := trimLog(t.Context(), r, running, shared); err != nil {
		t.Fatalf("the tick: %v", err)
	}
	_, end, err := running.log.Bounds(t.Context())
	if err != nil {
		t.Fatalf("bounds: %v", err)
	}
	if end != last+1 {
		t.Fatalf("the log ends at %d after a tick over anchors below its first "+
			"record, want %d — one trimmed record", end, last+1)
	}
	waitUntil(t, 20*time.Second, "the node to apply the trimmed record", func() bool {
		return running.runner.Committed().Seq >= end
	})
	gen := running.runner.Committed().Generation
	floor := statelog.Position{Stream: running.spec.Name, Generation: gen, Seq: last}.Packed()
	for subject, anchor := range anchorsOf(t, e, running.spec.Name) {
		if anchor < floor {
			t.Errorf("%s's anchor %d is below the log's first record %d after the tick",
				subject, anchor, floor)
		}
	}
}

// THE TRIM'S OWN DEPS SIGN WHAT THE SWEEP APPENDS — ADR-0031 beside ADR-0073.
//
// Every applier opens a record's frame before it reads one, so a trimmed
// record that is not a frame this fleet's keyring opens is refused on every
// node, and the anchors it was appended to remove stay for good.
// [statelog.SweepDeps] makes its signer REQUIRED for that reason, so a
// [retention.sweepDeps] that handed it none swept no anchor on any log: the
// count refused (`ErrUnsigned`) and every tick logged a failed sweep. This is
// the sweep the tick runs, through the deps the tick builds, and the record it
// appends is read back off the log and opened by the running log's own
// verifier — the one its applier opens it with.
func TestTheTrimsOwnDepsSignTheTrimmedRecord(t *testing.T) {
	t.Parallel()
	e, back, running := trimmedTracker(t)
	at := time.Now().UTC()
	mustApply(t, "the project", func() (worklet.WriteResult, error) {
		return e.native.Load().writer.WriteDocument(t.Context(), "op-ENG",
			worklet.ProjectSubject("ENG"), "", worklet.Project{
				V: worklet.DocumentVersion, Key: "ENG", Name: "Engineering",
				CreatedAt: at, UpdatedAt: at,
			}, worklet.ChangeProjectCreated, nil)
	})
	last := barrierOn(t, running)
	waitUntil(t, 20*time.Second, "the node to apply its own log", func() bool {
		return running.runner.Committed().Seq >= last
	})
	if err := running.log.Purge(t.Context(), last); err != nil {
		t.Fatalf("purge the project's record: %v", err)
	}

	r := sweepingRetention(e, back.Fleet)
	deps, sweeps := r.sweepDeps(running)
	if !sweeps {
		t.Fatal("the trim hands the tracker's log no anchor sweep")
	}
	below := statelog.Position{
		Stream: running.spec.Name, Generation: running.runner.Committed().Generation, Seq: last,
	}
	plan, err := statelog.PlanSweep(t.Context(), deps, below)
	if err != nil {
		t.Fatalf("count the anchors below the log's first record through the trim's own "+
			"deps: %v", err)
	}
	if plan.Records() != 1 {
		t.Fatalf("the count calls for %d trimmed record(s), want the 1 the project's "+
			"anchor below the log's first record needs", plan.Records())
	}
	swept, err := plan.Sweep(t.Context(), anchorSweepDeadline)
	if err != nil || swept.Records != 1 {
		t.Fatalf("the sweep appended %d record(s) (%v), want 1", swept.Records, err)
	}

	wire, payload, _, held, err := running.log.At(t.Context(), last+1)
	if err != nil || !held {
		t.Fatalf("read the record the sweep appended at %d: held %v, %v", last+1, held, err)
	}
	body, verdict := running.verifier.Open(payload)
	if verdict != statelog.Verified {
		t.Fatalf("the trimmed record on %s opens as %v under the running log's own "+
			"verifier, want verified — every applier refuses it, and the anchors it "+
			"names stay on every node", wire, verdict)
	}
	env, err := running.domain.Envelope(body)
	if err != nil {
		t.Fatalf("decode the trimmed record's envelope: %v", err)
	}
	if env.Subject != statelog.TrimmedSubject(below) {
		t.Errorf("the sweep appended a %v record, want the trimmed record of %v",
			env.Subject, below)
	}
}

// A TICK WITH NO ANCHOR BELOW THE LOG APPENDS NOTHING, so an idle fleet's log
// gains no record a quarter hour.
func TestATickWithNoAnchorBelowTheLogAppendsNothing(t *testing.T) {
	t.Parallel()
	e, back, running := trimmedTracker(t)
	first, last, err := running.log.Bounds(t.Context())
	if err != nil {
		t.Fatalf("bounds: %v", err)
	}
	r := sweepingRetention(e, back.Fleet)
	gen := running.runner.Committed().Generation
	// A BLOCKED TICK, which purges nothing: the floor is the log's own first
	// record, and nothing is below it.
	after, err := r.apply(t.Context(), running.log, running.domain.Name(), gen,
		statelog.TrimDecision{
			BlockedBy: statelog.TermApplied,
			Terms:     []statelog.Term{{Name: statelog.TermApplied, Known: true}},
		}, first, fleetInputs{at: time.Now().UTC(), previous: map[string]coord.TrimFloor{}})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if after != first {
		t.Fatalf("a blocked tick answered %d as the first surviving record, want %d", after, first)
	}
	r.sweepAnchors(t.Context(), []sweepTarget{{running: running, below: statelog.Position{
		Stream: running.spec.Name, Generation: gen, Seq: after,
	}}})
	if _, end, err := running.log.Bounds(t.Context()); err != nil || end != last {
		t.Fatalf("the log ends at %d (err %v) after a sweep with nothing below the "+
			"floor, want %d — it appended a record that removes nothing", end, err, last)
	}
}
