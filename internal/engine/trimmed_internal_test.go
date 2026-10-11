package engine

import (
	"database/sql"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// EVERY LOG THAT WRITES ANCHORS HAS A TRIMMED RECORD, AND ONLY THOSE — ADR-0032.
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

// A TICK'S ANCHOR SWEEPS SHARE ONE ALARM INTERVAL — ADR-0032.
//
// The sweeps run on the goroutine that evaluates this node's alarms
// ([retention.run]), and each is paced on this node's applier, so an applier
// slow but still moving would otherwise hold that goroutine for the cap times
// every wait, log after log. So each log that writes anchors is handed an
// equal share of [AlarmInterval] as its sweep's budget: together they hold the
// goroutine for at most one interval, however many logs write anchors. A log
// that writes none is handed no sweep.
func TestATicksAnchorSweepsShareOneAlarmInterval(t *testing.T) {
	t.Parallel()
	r := &retention{state: everyLog(), nodeID: "node-a",
		estate: func() statelog.ReadEstate { return nil }}
	var budgets []time.Duration
	for _, running := range r.state.running() {
		deps, sweeps := r.sweepDeps(running)
		if writes := statelog.WritesAnchors(running.spec); sweeps != writes {
			t.Errorf("%s writes anchors=%v and is handed a sweep=%v — the two must agree",
				running.domain.Name(), writes, sweeps)
		}
		if sweeps {
			budgets = append(budgets, deps.Budget)
		}
	}
	if len(budgets) < 2 {
		t.Fatalf("%d registered log(s) write anchors — the case needs the two that "+
			"share the interval today", len(budgets))
	}
	var total time.Duration
	for _, budget := range budgets {
		if want := AlarmInterval / time.Duration(len(budgets)); budget != want {
			t.Errorf("a log's sweep was handed %v, want %v — an equal share of one "+
				"alarm interval among the %d logs that write anchors", budget, want,
				len(budgets))
		}
		total += budget
	}
	if total > AlarmInterval {
		t.Errorf("a tick's sweeps may take %v together, past one alarm interval (%v) "+
			"— the holder's alarms would go unevaluated for longer", total, AlarmInterval)
	}
}

// anchorsOf is every anchor this node's copy holds on stream, by subject.
func anchorsOf(t *testing.T, e *Engine, stream string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	if err := e.native.Load().log.estate().Reader().Read(t.Context(), func(tx *sql.Tx) error {
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
	s := e.native.Load().log
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
		mustApply(t, "the project "+key, func() (tracker.WriteResult, error) {
			return e.native.Load().writer.WriteDocument(t.Context(), "op-"+key,
				tracker.ProjectSubject(key), "", tracker.Project{
					V: tracker.DocumentVersion, Key: key, Name: "Project " + key,
					CreatedAt: at, UpdatedAt: at,
				}, tracker.ChangeProjectCreated, nil)
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
	floors, err := back.Fleet.Floors(t.Context())
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
	if err := r.domain(t.Context(), running, shared); err != nil {
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
	mustApply(t, "the project", func() (tracker.WriteResult, error) {
		return e.native.Load().writer.WriteDocument(t.Context(), "op-ENG",
			tracker.ProjectSubject("ENG"), "", tracker.Project{
				V: tracker.DocumentVersion, Key: "ENG", Name: "Engineering",
				CreatedAt: at, UpdatedAt: at,
			}, tracker.ChangeProjectCreated, nil)
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
	if err := r.domain(t.Context(), running, shared); err != nil {
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
	r.sweepAnchors(t.Context(), running, statelog.Position{
		Stream: running.spec.Name, Generation: gen, Seq: after,
	})
	if _, end, err := running.log.Bounds(t.Context()); err != nil || end != last {
		t.Fatalf("the log ends at %d (err %v) after a sweep with nothing below the "+
			"floor, want %d — it appended a record that removes nothing", end, err, last)
	}
}
