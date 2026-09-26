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
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
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

// A PUBLISHED FLOOR AHEAD OF THIS NODE REFUSES IT, and says which of two
// things it is.
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
//
// # And the two states below it
//
// The trim publishes its floor BEFORE the purge it licenses, so until that
// purge lands — and for good, if it fails and no later tick licenses as much
// again — the log still holds what a node below the floor lacks. That node is
// replaying: its reads are told to come back, it admits no seats until it has
// caught up, and nothing sends it to adopt a snapshot of records it can read.
// Only once the purge lands is its next record gone, and only then is it below
// the log, refused as `below_floor`, shed, and sent to adopt.
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
	s := e.native.Load().log
	running := s.Domain(tracker.Domain{}.Name())
	if running == nil {
		t.Fatal("the tracker domain is not running")
	}
	waitUntil(t, 20*time.Second, "the node to admit seats", hydrated(t, e))
	// THIS NODE'S OWN TRIM DUTY PUBLISHES THE SAME FIELD, and it ticks
	// immediately at boot: its first conclusion is `blocked_by
	// backup_floor` at zero, which lands on top of the floor published
	// below and reads back as ok. Stop it first — it waits out an
	// in-flight tick — so the floor under test is the only one there is.
	e.stopRetention()

	// THE HEARTBEAT IS WATCHED RATHER THAN OBEYED: a rejoin it requests is
	// counted and does nothing, so what is read below is the heartbeat's
	// own conclusion rather than an adoption racing the next assertion.
	var rejoins atomic.Int64
	s.rejoinMu.Lock()
	s.rejoin = func(context.Context) error { rejoins.Add(1); return nil }
	s.rejoinMu.Unlock()
	rejoinRequested := func() bool {
		s.rejoinMu.Lock()
		defer s.rejoinMu.Unlock()
		return s.rejoining || rejoins.Load() > 0
	}
	at := running.runner.Committed()
	publish := func(floor uint64) {
		t.Helper()
		if err := back.Fleet.PutFloor(t.Context(), coord.TrimFloor{
			Domain: tracker.Domain{}.Name(), Generation: at.Generation,
			TrimTo: floor, Floor: floor, At: time.Now().UTC(), By: "peer",
		}); err != nil {
			t.Fatalf("publish a floor: %v", err)
		}
	}
	health := func() statelog.Health {
		t.Helper()
		h, err := s.health(t.Context(), running)
		if err != nil {
			t.Fatalf("health: %v", err)
		}
		return h
	}

	// A FLOOR EXACTLY AT THE NEXT RECORD IS NOT BELOW: the node has
	// consumed everything the trim may have removed.
	publish(at.Seq + 1)
	if got := health().Floor.State; got != statelog.FloorOK {
		t.Fatalf("health reads the floor as %s with a published floor at the next "+
			"record, want ok", got)
	}

	// THE TRIM LICENSES REMOVING RECORDS THIS NODE HAS NOT APPLIED — which
	// is what a node that was away finds when its peers were counted
	// without it — and publishes that before it purges them.
	s.haltAppliers()
	for range 5 {
		barrierOn(t, running)
	}
	publish(at.Seq + 5)
	replaying := health()
	var carried uint64
	if replaying.TrimFloor != nil {
		carried = *replaying.TrimFloor
	}
	if replaying.TrimFloor == nil || carried != at.Seq+5 {
		t.Fatalf("health carries a floor of %d (known=%v), want the published %d — "+
			"the positions minimum this replaces could never exceed this node's own row",
			carried, replaying.TrimFloor != nil, at.Seq+5)
	}
	if replaying.Floor.State != statelog.FloorReplaying {
		t.Fatalf("health reads the floor as %s with a published floor 5 past the "+
			"checkpoint and every record it licenses still on the log, want replaying",
			replaying.Floor.State)
	}
	if got := replaying.Refusal(time.Now()); got != statelog.RefuseBehind {
		t.Fatalf("a node replaying up to the floor refuses reads as %q, want %q — "+
			"the records are on the log and it clears on its own", got, statelog.RefuseBehind)
	}
	if e.NativeHydrated(t.Context()) {
		t.Fatal("the node admits seats while below the published floor")
	}
	// AND IT KEEPS THE SEATS IT HOLDS. Being behind is admission's concern
	// alone: every record this node lacks is on the log and it is reading
	// them, so moving its seats would drop work in hand for a state that
	// clears on its own. A replaying node is necessarily one with records
	// pending — the floor is at most one past the log's end — so this is
	// also the case a lag-derived term would get wrong.
	if ok, domain := e.SeatsServiceable(); !ok {
		t.Fatalf("a node replaying records the log still holds shed its seats "+
			"(domain %q) — that is being behind, not being wrong", domain)
	}
	s.publishPositions(t.Context())
	if rejoinRequested() {
		t.Fatal("the heartbeat asked the fleet for a snapshot for a node whose " +
			"missing records are all still on the log — it halts its appliers to " +
			"fetch what it could simply replay")
	}
	// AND A WRITE AT ZERO IS REFUSED WITH THE SAME WORD: the floor theorem
	// does not hold until the node has applied up to the floor, so it
	// refuses — but as a wait, through the fence as the engine wires it,
	// and never as the snapshot the node does not need.
	requireZeroRefused(t, e, "op-replaying", "node-replaying", statelog.ReasonBehind)

	// AND THE PURGE IT LICENSED LANDS: the next record this node needs is
	// gone, and only a snapshot can bring it back.
	if err := running.log.Purge(t.Context(), at.Seq+2); err != nil {
		t.Fatalf("purge: %v", err)
	}
	below := health()
	if below.Floor.State != statelog.FloorBelow {
		t.Fatalf("health reads the floor as %s with record %d purged and never "+
			"applied, want below", below.Floor.State, at.Seq+1)
	}
	if got := below.Refusal(time.Now()); got != statelog.RefuseBelowFloor {
		t.Fatalf("a node below the log refuses reads as %q, want %q", got,
			statelog.RefuseBelowFloor)
	}
	if e.NativeHydrated(t.Context()) {
		t.Fatal("the node admits seats while below the log")
	}
	if ok, _ := e.SeatsServiceable(); ok {
		t.Fatal("the node keeps its seats while below the log")
	}
	s.publishPositions(t.Context())
	if !rejoinRequested() {
		t.Fatalf("the heartbeat did not ask for a snapshot for a node whose next "+
			"record %d is gone", at.Seq+1)
	}
	requireZeroRefused(t, e, "op-below", "node-below", statelog.ReasonBelowFloor)
}

// requireZeroRefused publishes an eviction of a node nobody has ever evicted —
// a subject with no anchor, so the write reaches the expectation-zero fence —
// and requires the fence's refusal under want, with nothing appended.
func requireZeroRefused(t *testing.T, e *Engine, opID, nodeID string, want statelog.Reason) {
	t.Helper()
	running := e.native.Load().log.Domain(tracker.Domain{}.Name())
	_, before, err := running.log.Bounds(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	_, err = e.native.Load().writer.EvictNode(t.Context(), opID, nodeID)
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) || refusal.Reason != want {
		t.Fatalf("a write at an expectation of zero answered %v, want an "+
			"Unavailable naming %q", err, want)
	}
	if _, after, err := running.log.Bounds(t.Context()); err != nil || after != before {
		t.Fatalf("the log's last record is %d (err %v), want %d — a write the "+
			"fence refused was appended", after, err, before)
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
	waitUntil(t, 20*time.Second, "the node to admit seats", hydrated(t, e))
	// The trim is the other thing that purges this log; stopped, every
	// purge below is this test's own.
	e.stopRetention()

	s := e.native.Load().log
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
		behind, _, _, err := s.replayable(t.Context(), logs)
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
	s := e.native.Load().log
	writer := e.native.Load().writer
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
	floor, err := s.trimFloor(t.Context(), running.domain.Name(), at.Generation)
	if err != nil {
		t.Fatalf("read the published floor: %v", err)
	}
	if !statelog.Replayable(at.Seq, floor) {
		t.Fatalf("the published floor %d already refuses checkpoint %d, so this "+
			"case would not show which bound the fence reads", floor, at.Seq)
	}

	_, err = writer.EvictNode(t.Context(), "op-below", "node-below")
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonBelowFloor {
		t.Fatalf("a node at %d against a log whose first record is %d wrote at "+
			"an expectation of zero (err %v), want a %s refusal — record %d is "+
			"gone and it never applied it", at.Seq, missed+1, err,
			statelog.ReasonBelowFloor, missed)
	}
	if _, last, err := running.log.Bounds(t.Context()); err != nil || last != missed {
		t.Fatalf("the log's last record is %d (err %v), want %d — an append "+
			"landed from a node the fence should have refused", last, err, missed)
	}
}

// A NODE BELOW THE TRIM FLOOR IS NOT READMITTED — through the node gate the
// readmission route calls, judged against the register, the published floors
// and the logs as the engine wires them, with nothing written to either log on
// a refusal.
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
	s := e.native.Load().log
	gate := e.native.Load().gate
	if gate == nil {
		t.Fatal("the node runs no node gate")
	}
	wiki := s.Domain(pages.Domain{}.Name())
	vectors := s.Domain(search.Domain{}.Name())
	if wiki == nil || vectors == nil {
		t.Fatal("the pages or the vector domain is not running")
	}

	const away = "node-away"
	if res, err := gate.Evict(t.Context(), GateRequest{
		Node: away, OpID: "op-evict", By: "operator"}); err != nil || !res.Complete() {
		t.Fatalf("evict %s: %v (%+v)", away, err, res)
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
	// BOTH LOGS' ENDS, because a refusal writes nothing to EITHER.
	end := func() [2]uint64 {
		t.Helper()
		var out [2]uint64
		for i, r := range []*runningDomain{running, wiki} {
			_, last, err := r.log.Bounds(t.Context())
			if err != nil {
				t.Fatalf("read %s's end: %v", r.domain.Name(), err)
			}
			out[i] = last
		}
		return out
	}
	readmit := GateRequest{Node: away, OpID: "op-readmit", By: "operator"}
	refusedIn := func(domain string, seq, floorWant, firstWant uint64) {
		t.Helper()
		before := end()
		_, err := gate.Readmit(t.Context(), readmit)
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
			t.Fatalf("the logs moved from %v to %v on a refused readmission", before, last)
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
	res, err := gate.Readmit(t.Context(), readmit)
	if err != nil {
		t.Fatalf("a node one record short of every floor was refused: %v", err)
	}
	for _, d := range res.Domains {
		if d.Err != nil || d.Outcome != statelog.OutcomeApplied {
			t.Fatalf("the readmission answered %+v on %s, want applied", d, d.Domain)
		}
	}
	for _, lister := range []evictionLister{tracker.Domain{}, pages.Domain{}} {
		rows, err := lister.Evictions(t.Context(), back.Store)
		if err != nil {
			t.Fatalf("read the evictions: %v", err)
		}
		if len(rows) != 1 || rows[0].NodeID != away || !rows[0].Back {
			t.Fatalf("evictions = %+v, want %s readmitted on every log", rows, away)
		}
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

// A WRITE FENCE READS THE PUBLISHED FLOOR AT THE GENERATION IT NAMES, for the
// domain it was built for, and at nothing else.
//
// The fence names the generation of the checkpoint its write's snapshot read,
// and [stateLog.floorOf] is the one line between that name and the register.
// Read a generation up, the floor published at the fence's own reads as a dead
// number space's — zero — and the published-floor witness drops out of both
// write fences; read a generation down, every current floor is a refusal; bound
// to anything but its argument, it answers for a sequence space the cursor is
// not in. One floor, published at one generation and asked about at three,
// tells each of those apart.
func TestAWriteFenceReadsTheFloorAtTheGenerationItNames(t *testing.T) {
	t.Parallel()
	fleet := coordmem.NewFleet()
	s := &stateLog{fleet: fleet}
	const generation = 4
	if err := fleet.PutFloor(t.Context(), coord.TrimFloor{
		Domain: tracker.Domain{}.Name(), Generation: generation, TrimTo: 700,
		Floor: 700, At: time.Now().UTC(), By: "peer",
	}); err != nil {
		t.Fatalf("publish a floor: %v", err)
	}
	read := s.floorOf(tracker.Domain{}.Name())

	if got, err := read(t.Context(), generation); err != nil || got != 700 {
		t.Fatalf("the floor at the generation it was published at reads %d (err "+
			"%v), want 700", got, err)
	}
	if got, err := read(t.Context(), generation+1); err != nil || got != 0 {
		t.Fatalf("a floor from the generation before reads %d (err %v), want 0 — "+
			"it names a dead number space", got, err)
	}
	if got, err := read(t.Context(), generation-1); err == nil {
		t.Fatalf("a floor from the generation ahead reads %d, want a refusal — "+
			"the cursor is the one on the dead number space", got)
	}
	if got, err := s.floorOf(pages.Domain{}.Name())(t.Context(), generation); err != nil || got != 0 {
		t.Fatalf("the pages fence reads the tracker's floor as %d (err %v), want 0",
			got, err)
	}
}

// THE WRITE FENCES AS THE ENGINE WIRES THEM REFUSE ON THE PUBLISHED FLOOR
// ALONE, in both identity-claiming domains, while the log still holds
// everything.
//
// The fence takes the higher of two witnesses, and every other case here that
// refuses a write at zero does it with the stream's first sequence — a purge
// that has landed. The floor is the witness to one that has NOT: the trim
// publishes it before the purge it licenses, so until that purge lands it is
// the only thing that says a record this node never applied may be about to
// go. Each domain's fence reaches it through its own wiring line, so each is
// asked here with a floor one past its next record and a log that has lost
// nothing, which the floor alone refuses, and then with the floor AT its next
// record, which clears — the boundary [statelog.Replayable] draws.
func TestTheWiredWriteFencesRefuseOnThePublishedFloorAlone(t *testing.T) {
	t.Parallel()
	e, back, _ := trimmedTracker(t)
	s := e.native.Load().log
	if e.native.Load().writer == nil || e.native.Load().pages == nil {
		t.Fatal("the node runs no tracker writer or no page store")
	}
	// THE HEARTBEAT IS WATCHED RATHER THAN OBEYED: a node below a
	// published floor may ask the fleet for a snapshot, and an adoption
	// racing the writes below would be what they measured.
	s.rejoinMu.Lock()
	s.rejoin = func(context.Context) error { return nil }
	s.rejoinMu.Unlock()

	for _, tc := range []struct {
		domain string
		// zero is a write on a subject nobody has written, which is a
		// write at an expectation of zero.
		zero func(ctx context.Context, key string) error
	}{
		{domain: tracker.Domain{}.Name(), zero: func(ctx context.Context, key string) error {
			_, err := e.native.Load().writer.EvictNode(ctx, "op-"+key, "node-"+key)
			return err
		}},
		{domain: pages.Domain{}.Name(), zero: func(ctx context.Context, key string) error {
			_, _, err := e.native.Load().pages.EnsureContainer(ctx, testActivation, key, key, "")
			return err
		}},
	} {
		running := s.Domain(tc.domain)
		if running == nil {
			t.Fatalf("the %s domain is not running", tc.domain)
		}
		// CAUGHT UP, so the checkpoint a write's snapshot reads is the
		// log's end, and a floor one past it is one record ahead.
		first, last, err := running.log.Bounds(t.Context())
		if err != nil {
			t.Fatalf("read %s's bounds: %v", tc.domain, err)
		}
		waitUntil(t, 20*time.Second, tc.domain+" to apply its whole log", func() bool {
			return running.runner.Committed().Seq >= last
		})
		at := running.runner.Committed()
		if !statelog.Replayable(at.Seq, first) {
			t.Fatalf("%s's log starts at %d past this node's %d, so the stream "+
				"would refuse on its own and this case would not show the floor",
				tc.domain, first, at.Seq)
		}
		publish := func(floor uint64) {
			t.Helper()
			if err := back.Fleet.PutFloor(t.Context(), coord.TrimFloor{
				Domain: tc.domain, Generation: at.Generation,
				TrimTo: floor, Floor: floor, At: time.Now().UTC(), By: "peer",
			}); err != nil {
				t.Fatalf("publish %s's floor: %v", tc.domain, err)
			}
		}

		publish(at.Seq + 2)
		if err := tc.zero(t.Context(), "REFUSED"); !errors.Is(err, statelog.ErrUnavailable) {
			t.Fatalf("%s: a write at zero from a node one record short of the "+
				"published floor answered %v, want %v — the log still holds that "+
				"record, so the floor is the only witness", tc.domain, err,
				statelog.ErrUnavailable)
		}
		if _, after, err := running.log.Bounds(t.Context()); err != nil || after != last {
			t.Fatalf("%s's log ends at %d (err %v), want %d — the refused write "+
				"was appended", tc.domain, after, err, last)
		}

		publish(at.Seq + 1)
		if err := tc.zero(t.Context(), "CLEARED"); err != nil {
			t.Fatalf("%s: a write at zero from a node holding everything the "+
				"published floor licenses removing was refused: %v", tc.domain, err)
		}
	}
}
