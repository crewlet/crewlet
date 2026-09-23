package engine

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE FLOOR IS THE TRIM'S PUBLISHED FLOOR, NOT ITS LAST CONCLUSION, and every
// branch of reading it is reachable without a fleet.
//
// The two differ exactly where a write fence needs the floor most. A blocked
// tick concludes zero, and a tick whose lowest counted node is below the log —
// readmitted, restored from an old backup, up on its own history — concludes
// that node's own position; records earlier ticks licensed removing are gone
// all the same. Read as the floor, either conclusion cleared that node to
// publish at an expectation of zero over records it never applied.
func TestTheFloorIsThePublishedFloorRatherThanTheTicksConclusion(t *testing.T) {
	t.Parallel()
	floors := []coord.TrimFloor{
		{Domain: "tracker", Generation: 3, TrimTo: 4_200, Floor: 4_200},
		{Domain: "pages", Generation: 3, TrimTo: 0, Floor: 3_000, BlockedBy: "backup_floor"},
		{Domain: "readmitted", Generation: 3, TrimTo: 700, Floor: 3_000},
		{Domain: "vectors", Generation: 5, TrimTo: 90, Floor: 90},
	}
	for name, tc := range map[string]struct {
		domain string
		gen    uint32
		want   uint64
		err    bool
	}{
		"the trim's own floor":                     {domain: "tracker", gen: 3, want: 4_200},
		"a blocked trim keeps what it licensed":    {domain: "pages", gen: 3, want: 3_000},
		"a conclusion the counted minimum dragged": {domain: "readmitted", gen: 3, want: 3_000},
		"a domain the trim never reached":          {domain: "other", gen: 3, want: 0},
		"a floor from a previous generation":       {domain: "tracker", gen: 4, want: 0},
		"a floor from a generation ahead":          {domain: "vectors", gen: 3, err: true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := floorFor(floors, tc.domain, tc.gen)
			if (err != nil) != tc.err {
				t.Fatalf("err = %v, want error=%v", err, tc.err)
			}
			if got != tc.want {
				t.Fatalf("floor = %d, want %d", got, tc.want)
			}
		})
	}
}

// A PUBLISHED FLOOR AHEAD OF THIS NODE REFUSES IT.
//
// # The vacuous comparand this replaces
//
// The floor every fence, the readiness gate and the join compared against was
// the minimum over every node's published position — this node's own row
// included. A minimum that includes the reader can never exceed the reader, so
// the comparison was decided before it was made: a node genuinely below the
// fleet's floor kept serving, kept admitting seats, and kept publishing at an
// expectation of zero over records the trim had removed. This publishes a
// floor the way the trim does and requires the node to notice.
func TestANodeBelowThePublishedFloorRefusesToServe(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	back, err := OpenBackends(t.Context(), &b, cfg)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	s := e.native.log
	running := s.Domain(tracker.Domain{}.Name())
	if running == nil {
		t.Fatal("the tracker domain is not running")
	}
	waitUntil(t, 20*time.Second, "the node to admit seats", e.NativeHydrated)
	// THIS NODE'S OWN TRIM DUTY PUBLISHES THE SAME FIELD, and it ticks
	// immediately at boot: its first conclusion is `blocked_by
	// backup_floor` at zero, which lands on top of the floor published
	// below and reads back as ok. Stop it first — it waits out an
	// in-flight tick — so the floor under test is the only one there is.
	e.stopRetention()

	// THE TRIM CONCLUDES the fleet may remove everything below a point this
	// node has not reached — which is what happens to a node that was away
	// while its peers moved on and were counted without it.
	at := running.runner.Committed()
	if err := back.Fleet.PutFloor(t.Context(), coord.TrimFloor{
		Domain: tracker.Domain{}.Name(), Generation: at.Generation,
		TrimTo: at.Seq + 5, Floor: at.Seq + 5, At: time.Now().UTC(), By: "peer",
	}); err != nil {
		t.Fatalf("publish a floor: %v", err)
	}
	health, err := s.health(t.Context(), running)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	var carried uint64
	if health.TrimFloor != nil {
		carried = *health.TrimFloor
	}
	if health.TrimFloor == nil || carried != at.Seq+5 {
		t.Fatalf("health carries a floor of %d (known=%v), want the published %d — "+
			"the positions minimum this replaces could never exceed this node's own row",
			carried, health.TrimFloor != nil, at.Seq+5)
	}
	if health.Floor.State != statelog.FloorBelow {
		t.Fatalf("health reads the floor as %s with a published floor 5 past the "+
			"checkpoint, want below", health.Floor.State)
	}
	if e.NativeHydrated() {
		t.Fatal("the node admits seats while below the published floor")
	}
	if ok, _ := e.SeatsServiceable(); ok {
		t.Fatal("the node keeps its seats while below the published floor")
	}

	// AND A FLOOR EXACTLY AT THE NEXT RECORD IS NOT BELOW: the node has
	// consumed everything the trim may have removed.
	if err := back.Fleet.PutFloor(t.Context(), coord.TrimFloor{
		Domain: tracker.Domain{}.Name(), Generation: at.Generation,
		TrimTo: at.Seq + 1, Floor: at.Seq + 1, At: time.Now().UTC(), By: "peer",
	}); err != nil {
		t.Fatalf("publish a floor: %v", err)
	}
	health, err = s.health(t.Context(), running)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Floor.State != statelog.FloorOK {
		t.Fatalf("health reads the floor as %s with a published floor at the next "+
			"record, want ok", health.Floor.State)
	}
}

// THE RECOVERY PATH DRAWS ITS LINE AT THE NEXT RECORD, on a real stream: the
// join's behind test, the running node's heartbeat that requests a rejoin, and
// the re-check after a transfer.
//
// Both sides of the boundary cost something nothing else reports. A node
// wrongly judged behind asks a fleet for a snapshot of a log it could simply
// read — halting its appliers to do it, when the heartbeat is the one asking —
// and on a lone node that ask finds no responders and returns in milliseconds,
// so the boot's wall clock cannot see it. A node wrongly judged able to replay
// replays over a hole and reports itself caught up. So each question is put
// directly, against what JetStream itself reports rather than against numbers
// typed here: a stream a purge emptied says first = last+1, the zero case the
// join's own comment once confused with a never-written stream's.
func TestTheRecoveryPathDrawsItsLineAtTheNextRecord(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	back, err := OpenBackends(t.Context(), &b, cfg)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	waitUntil(t, 20*time.Second, "the node to admit seats", e.NativeHydrated)
	// The trim is the other thing that purges this log; stopped, every
	// purge below is this test's own.
	e.stopRetention()

	s := e.native.log
	name := tracker.Domain{}.Name()
	spec := tracker.Domain{}.Stream()
	running := s.Domain(name)
	if running == nil {
		t.Fatal("the tracker domain is not running")
	}
	logs := map[string]*jetstream.DomainLog{}
	for _, domain := range registeredDomains() {
		r := s.Domain(domain.Name())
		if r == nil {
			t.Fatalf("%s is not running", domain.Name())
		}
		logs[domain.Name()] = r.log
	}
	behindNow := func() []string {
		t.Helper()
		behind, _, err := s.replayable(t.Context(), logs)
		if err != nil {
			t.Fatalf("replayable: %v", err)
		}
		return behind
	}
	// THE HEARTBEAT IS WATCHED RATHER THAN OBEYED: a rejoin it requests is
	// counted and does nothing, so what is read below is the heartbeat's
	// own conclusion rather than an adoption racing the next assertion.
	// Installed while the node is healthy, so no rejoin is in flight to be
	// holding the old one.
	var rejoins atomic.Int64
	s.rejoinMu.Lock()
	s.rejoin = func(context.Context) error { rejoins.Add(1); return nil }
	s.rejoinMu.Unlock()
	rejoinRequested := func() bool {
		s.rejoinMu.Lock()
		defer s.rejoinMu.Unlock()
		return s.rejoining || rejoins.Load() > 0
	}
	body, err := tracker.EncodeBarrier(statelog.Envelope{
		V: statelog.BarrierVersion, Kind: statelog.BarrierKind,
		Subject: statelog.Subject{Kind: statelog.BarrierKind},
		Gen:     running.runner.Committed().Generation,
		Scope:   statelog.ScopeSet{Paths: []string{statelog.BarrierScope}},
	})
	if err != nil {
		t.Fatalf("encode a barrier: %v", err)
	}
	appendBarrier := func() uint64 {
		t.Helper()
		seq, _, err := running.log.Append(t.Context(),
			spec.SubjectPrefix+"."+statelog.BarrierKind, "", nil, body)
		if err != nil {
			t.Fatalf("append a barrier: %v", err)
		}
		return seq
	}
	purgeTo := func(upTo, wantLast uint64) {
		t.Helper()
		if err := running.log.Purge(t.Context(), upTo); err != nil {
			t.Fatalf("purge: %v", err)
		}
		first, last, err := running.log.Bounds(t.Context())
		if err != nil {
			t.Fatalf("bounds: %v", err)
		}
		if first != last+1 || last != wantLast {
			t.Fatalf("the purged log reports first %d and last %d, want %d and %d "+
				"— the case under test is a stream a purge emptied",
				first, last, wantLast+1, wantLast)
		}
	}

	// THE HEALTH READ asks of the higher of the published floor and the
	// stream's first sequence, and it is the one that decides whether the
	// node keeps its seats. With the trim stopped the published floor
	// cannot move, so the case checks it sits at or below the next record
	// — and whatever the health read concludes past that is the stream's
	// `first` alone, the half of the comparison a published floor never
	// exercises.
	floorOf := func(checkpoint uint64) statelog.FloorState {
		t.Helper()
		h, err := s.health(t.Context(), running)
		if err != nil {
			t.Fatalf("health: %v", err)
		}
		if h.TrimFloor == nil {
			t.Fatal("the health read carries no published floor")
		}
		if *h.TrimFloor > checkpoint+1 {
			t.Fatalf("the published floor is %d with the trim stopped, want at "+
				"most %d — a floor past the next record would decide the case "+
				"on its own, and the stream's first sequence would go untested",
				*h.TrimFloor, checkpoint+1)
		}
		return h.Floor.State
	}

	// A FRESH NODE IS NOT BEHIND, whatever the stream's zero case is.
	if behind := behindNow(); len(behind) != 0 {
		t.Fatalf("a freshly booted node judged itself behind on %v — it has the "+
			"whole log ahead of it", behind)
	}

	// ONE RECORD, APPLIED, AND THEN PURGED: the log holds nothing and says
	// first = last+1, exactly the next record this node needs.
	applied := appendBarrier()
	waitUntil(t, 20*time.Second, "the node to apply the barrier", func() bool {
		return running.runner.Committed().Seq == applied
	})
	purgeTo(applied+1, applied)
	if behind := behindNow(); len(behind) != 0 {
		t.Fatalf("a node at %d against a log purged empty after it (first %d) "+
			"judged itself behind on %v — it applied every record that was removed",
			applied, applied+1, behind)
	}
	s.publishPositions(t.Context())
	if rejoinRequested() {
		t.Fatalf("the heartbeat requested a rejoin for a node at %d against a log "+
			"whose first record is %d — it applied every record that was removed, "+
			"and a rejoin halts its appliers to ask for nothing", applied, applied+1)
	}
	if state := floorOf(applied); state != statelog.FloorOK {
		t.Fatalf("the health read judged a node at %d against a log whose first "+
			"record is %d %s, want ok — it applied every record that was "+
			"removed, and a node read as below gives up its seats", applied,
			applied+1, state)
	}

	// AND ONE THIS NODE NEVER APPLIED, PURGED TOO: the next record it needs
	// is gone, and only a snapshot can bring it back.
	s.haltAppliers()
	missed := appendBarrier()
	if missed != applied+1 {
		t.Fatalf("the unapplied barrier landed at %d, want %d", missed, applied+1)
	}
	purgeTo(missed+1, missed)
	if behind := behindNow(); !slices.Equal(behind, []string{name}) {
		t.Fatalf("a node at %d against a log whose first record is %d judged "+
			"itself behind on %v, want exactly [%s] — record %d is gone and it "+
			"never applied it", applied, missed+1, behind, name, missed)
	}
	s.publishPositions(t.Context())
	if !rejoinRequested() {
		t.Fatalf("the heartbeat did not request a rejoin for a node at %d against "+
			"a log whose first record is %d — record %d is gone, and a running "+
			"node that does not notice serves over the hole", applied, missed+1, missed)
	}
	if state := floorOf(applied); state != statelog.FloorBelow {
		t.Fatalf("the health read judged a node at %d against a log whose first "+
			"record is %d %s, want below — record %d is gone, and a node read "+
			"as serving keeps seats it cannot answer for", applied, missed+1,
			state, missed)
	}

	// AND THE RE-CHECK AFTER A TRANSFER DRAWS THE SAME LINE: an artefact
	// through the last record removed leaves nothing missing, and one a
	// record short of it would be installed over a hole.
	artefactAt := func(seq uint64) statelog.Manifest {
		m := statelog.Manifest{Domains: map[string]statelog.DomainPosition{}}
		for _, domain := range registeredDomains() {
			m.Domains[domain.Name()] = statelog.DomainPosition{Stream: domain.Stream().Name}
		}
		m.Domains[name] = statelog.DomainPosition{Stream: spec.Name, Seq: seq}
		return m
	}
	if err := s.stillUsable(t.Context(), logs, artefactAt(missed)); err != nil {
		t.Fatalf("an artefact at %d against a log whose first record is %d was "+
			"refused: %v", missed, missed+1, err)
	}
	if err := s.stillUsable(t.Context(), logs, artefactAt(applied)); err == nil {
		t.Fatalf("an artefact at %d against a log whose first record is %d was "+
			"accepted — record %d is gone and it does not hold it",
			applied, missed+1, missed)
	}
}

// A NODE BELOW THE LOG IS NOT CLEARED TO PUBLISH AT ZERO, WHATEVER THE
// PUBLISHED FLOOR SAYS — the write fence as the engine wires it, on a real
// stream.
//
// The floor is one of two witnesses to what the log has lost, and it cannot
// see everything: a floor published at another generation reads as zero, and
// so does one no trim ever wrote. The stream's own first sequence is the other
// witness, and the fence asks of the higher. Wired with the floor alone —
// which is how it stood — this node, whose next record had been purged,
// published an eviction at an expectation of zero over a subject whose history
// it had never applied: the retry-at-zero the floor theorem forbids below the
// log.
func TestANodeBelowTheLogIsRefusedZeroWhateverThePublishedFloorSays(t *testing.T) {
	t.Parallel()
	e, _, running := trimmedTracker(t)
	s := e.native.log
	writer := e.native.writer
	if writer == nil {
		t.Fatal("the node runs no tracker writer")
	}

	// THE CONTROL: at the log, a write at zero goes through. Without it a
	// fence that refused everything would pass the case below.
	res, err := writer.EvictNode(t.Context(), "op-control", "node-control")
	if err != nil {
		t.Fatalf("a node at the log was refused a write at zero: %v", err)
	}
	if res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the control write answered %q, want applied", res.Outcome)
	}

	// THE NODE MISSES A RECORD AND THE LOG LOSES IT: appliers halted, one
	// record appended past this node's checkpoint, and the log purged
	// through it — the next record this node needs is gone.
	s.haltAppliers()
	at := running.runner.Committed()
	missed := barrierOn(t, running)
	if missed != at.Seq+1 {
		t.Fatalf("the unapplied record landed at %d, want %d", missed, at.Seq+1)
	}
	if err := running.log.Purge(t.Context(), missed+1); err != nil {
		t.Fatalf("purge: %v", err)
	}

	// AND THE PUBLISHED FLOOR CANNOT SEE IT: the one this node's own trim
	// wrote at boot, before any of this, still clears the checkpoint.
	floor, err := s.trimFloor(running.domain.Name(),
		func() uint32 { return at.Generation })(t.Context())
	if err != nil {
		t.Fatalf("read the published floor: %v", err)
	}
	if !statelog.Replayable(at.Seq, floor) {
		t.Fatalf("the published floor %d already refuses checkpoint %d, so this "+
			"case would not show which bound the fence reads", floor, at.Seq)
	}

	_, err = writer.EvictNode(t.Context(), "op-below", "node-below")
	if !errors.Is(err, statelog.ErrUnavailable) {
		t.Fatalf("a node at %d against a log whose first record is %d wrote at "+
			"an expectation of zero (err %v), want %v — record %d is gone and "+
			"it never applied it", at.Seq, missed+1, err, statelog.ErrUnavailable,
			missed)
	}
	if _, last, err := running.log.Bounds(t.Context()); err != nil || last != missed {
		t.Fatalf("the log's last record is %d (err %v), want %d — an append "+
			"landed from a node the fence should have refused", last, err, missed)
	}
}

// A NODE BELOW THE TRIM FLOOR IS NOT READMITTED — through the writer the
// readmission route calls, judged against the register, the published floors
// and the logs as the engine wires them.
//
// The documentation promised this refusal; the writer wrote the inverse record
// for any node, so an operator readmitting a machine that was still offline
// put back exactly the pin the eviction had lifted and was told it had worked.
// Each witness is exercised on its own — the published floor, then the
// stream's first sequence with the floor at zero — because the fence the node
// becomes subject to reads the higher of the two, and a readmission judged
// against either alone clears a node that fence refuses. And every
// identity-claiming domain is judged while the compacted one is not.
func TestANodeBelowTheFloorIsNotReadmitted(t *testing.T) {
	t.Parallel()
	e, back, running := trimmedTracker(t)
	s := e.native.log
	writer := e.native.writer
	if writer == nil {
		t.Fatal("the node runs no tracker writer")
	}
	wiki := s.Domain(pages.Domain{}.Name())
	vectors := s.Domain(search.Domain{}.Name())
	if wiki == nil || vectors == nil {
		t.Fatal("the pages or the vector domain is not running")
	}

	const away = "node-away"
	if res, err := writer.EvictNode(t.Context(), "op-evict", away); err != nil ||
		res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("evict %s: %v (outcome %q)", away, err, res.Outcome)
	}
	at := running.runner.Committed()
	if at.Seq < 3 {
		t.Fatalf("this node is at %d, too near the start of the log for a floor "+
			"to pass anybody", at.Seq)
	}
	// THE ABSENT NODE'S LAST HEARTBEAT, from before the fleet moved on.
	report := func(trackerSeq uint64) {
		t.Helper()
		if err := back.Fleet.PutPositions(t.Context(), coord.NodePositions{
			NodeID: away, At: time.Now().UTC(),
			Domains: map[string]coord.DomainPosition{
				running.domain.Name(): {Generation: at.Generation, Seq: trackerSeq,
					AppliedThrough: trackerSeq},
				wiki.domain.Name(): {Generation: wiki.runner.Committed().Generation},
				vectors.domain.Name(): {
					Generation: vectors.runner.Committed().Generation},
			},
		}); err != nil {
			t.Fatalf("publish %s's position: %v", away, err)
		}
	}
	floor := func(r *runningDomain, f uint64) {
		t.Helper()
		if err := back.Fleet.PutFloor(t.Context(), coord.TrimFloor{
			Domain: r.domain.Name(), Generation: r.runner.Committed().Generation,
			TrimTo: f, Floor: f, At: time.Now().UTC(), By: "peer",
		}); err != nil {
			t.Fatalf("publish %s's floor: %v", r.domain.Name(), err)
		}
	}
	end := func() uint64 {
		t.Helper()
		_, last, err := running.log.Bounds(t.Context())
		if err != nil {
			t.Fatalf("read the log's end: %v", err)
		}
		return last
	}
	refusedIn := func(domain string, seq, floorWant, firstWant uint64) {
		t.Helper()
		before := end()
		_, err := writer.ReadmitNode(t.Context(), "op-readmit", away)
		var refusal *statelog.ReadmissionRefusal
		if !errors.As(err, &refusal) {
			t.Fatalf("readmitting %s answered %v, want a refusal naming %s", away,
				err, domain)
		}
		if refusal.Domain != domain || refusal.Seq != seq ||
			refusal.Bound.Floor != floorWant || refusal.Bound.First != firstWant {
			t.Fatalf("refused in %s at %d against floor %d / first %d, want %s at %d "+
				"against %d / %d", refusal.Domain, refusal.Seq, refusal.Bound.Floor,
				refusal.Bound.First, domain, seq, floorWant, firstWant)
		}
		if last := end(); last != before {
			t.Fatalf("the log moved from %d to %d on a refused readmission", before, last)
		}
	}

	// THE PUBLISHED FLOOR HAS PASSED IT — and the vectors' floor is further
	// still, which refuses nothing: a compacted domain forms no expectation
	// of zero, so a node behind in it is a coverage figure.
	report(1)
	floor(running, at.Seq)
	floor(vectors, 1_000_000)
	refusedIn(running.domain.Name(), 1, at.Seq, 1)

	// THE STREAM HAS LOST WHAT IT NEEDS while the published floor says
	// nothing — a floor at zero is what a floor from another generation
	// reads as.
	floor(running, 0)
	if err := running.log.Purge(t.Context(), at.Seq); err != nil {
		t.Fatalf("purge: %v", err)
	}
	refusedIn(running.domain.Name(), 1, 0, at.Seq)

	// CAUGHT UP ON THE TRACKER, BEHIND ON THE PAGES: a node is a replica
	// of every log it runs or of none.
	report(at.Seq - 1)
	floor(wiki, 5)
	refusedIn(wiki.domain.Name(), 0, 5, logFirstOf(t, wiki))

	// AND ONCE IT HOLDS EVERYTHING THAT MAY BE GONE, IT IS TAKEN BACK.
	floor(wiki, 0)
	res, err := writer.ReadmitNode(t.Context(), "op-readmit", away)
	if err != nil {
		t.Fatalf("a node one record short of every floor was refused: %v", err)
	}
	if res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the readmission answered %q, want applied", res.Outcome)
	}
	rows, err := tracker.Evictions(t.Context(), back.Store.Replicated(),
		running.domain.Stream().Name)
	if err != nil {
		t.Fatalf("read the evictions: %v", err)
	}
	if len(rows) != 1 || rows[0].NodeID != away || !rows[0].IsBack {
		t.Fatalf("evictions = %+v, want %s readmitted", rows, away)
	}
}

// logFirstOf is a domain log's own first surviving sequence.
func logFirstOf(t *testing.T, r *runningDomain) uint64 {
	t.Helper()
	first, _, err := r.log.Bounds(t.Context())
	if err != nil {
		t.Fatalf("read %s's bounds: %v", r.domain.Name(), err)
	}
	return first
}
