package engine

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE RUNNING LAYOUT-0 RUNTIME IS TODAY'S ESTATE: its streams, its keys and its
// coordination records are the ones a fleet running the build before logs had
// layouts holds, byte for byte.
//
// Every surface the per-log runtime rekeyed has a layout-0 answer that must be
// the old one, or a node upgraded into a running fleet provisions a second,
// empty stream beside the real one, publishes a register row nobody else's
// trim reads, or writes a floor under a key the fleet's fences never look at.
// So this boots a node the way `crewlet run` does and reads each surface back:
// the logs it runs and their order, each log's stream on the broker, the
// consumer each is applied through, the row its heartbeat writes and the floor
// its trim publishes.
func TestTheRunningLayoutZeroRuntimeIsTodaysEstate(t *testing.T) {
	t.Parallel()
	e, js := aRunningNode(t)
	s := e.native.Load().log

	if got, want := s.layout.AllLogs(), LayoutZero().AllLogs(); !slices.Equal(got, want) {
		t.Fatalf("the node runs the logs %v, and layout 0 is %v", got, want)
	}
	today := map[string][3]string{
		tracker.Domain{}.Name(): {topics.TrackerLogStream, topics.TrackerLogPrefix, topics.TrackerLogWildcard},
		search.Domain{}.Name():  {topics.TrackerVectorsStream, topics.TrackerVectorsPrefix, topics.TrackerVectorsWildcard},
		pages.Domain{}.Name():   {topics.PagesLogStream, topics.PagesLogPrefix, topics.PagesLogWildcard},
	}
	var registered []string
	for _, d := range registeredDomains() {
		registered = append(registered, d.Name())
	}
	if got := s.held().order; !slices.Equal(got, registered) {
		t.Fatalf("the node keys its logs %v, and the register keys them %v", got, registered)
	}
	for _, running := range s.running() {
		name := running.domain.Name()
		want := today[name]
		if running.key != name {
			t.Errorf("the %s log is keyed %q; the register, the floors and every "+
				"manifest key it %q", name, running.key, name)
		}
		if running.spec.Name != want[0] || running.spec.SubjectPrefix != want[1] ||
			!slices.Equal(running.spec.Subjects, []string{want[2]}) {
			t.Errorf("the %s log runs on (%q, %q, %v); the fleet's stream is (%q, %q, %q)",
				name, running.spec.Name, running.spec.SubjectPrefix, running.spec.Subjects,
				want[0], want[1], want[2])
		}
		// THE CEILING TIER A SIZED THE DOMAIN AT, whole: one log carries
		// the domain's whole budget, as its one stream always did.
		if budget := s.ceilings[name].Bytes; running.spec.MaxBytes != budget {
			t.Errorf("the %s log's ceiling is %d, and Tier A sized its domain at %d",
				name, running.spec.MaxBytes, budget)
		}
		stream, err := js.Stream(t.Context(), want[0])
		if err != nil {
			t.Fatalf("the fleet's %s stream is not on the broker: %v", name, err)
		}
		info, err := stream.Info(t.Context())
		if err != nil {
			t.Fatalf("read %s: %v", want[0], err)
		}
		if info.Config.MaxBytes != running.spec.MaxBytes {
			t.Errorf("%s was created at %d bytes and its spec says %d",
				want[0], info.Config.MaxBytes, running.spec.MaxBytes)
		}
		if _, err := stream.Consumer(t.Context(), running.consumer.Name()); err != nil {
			t.Errorf("the %s log is applied through the consumer %q, which is not on "+
				"its stream: %v", name, running.consumer.Name(), err)
		}
	}
	// NO PARTITIONED STREAM: layout 0 names only the three.
	names := js.StreamNames(t.Context())
	for name := range names.Name() {
		if strings.HasPrefix(name, "CREWLET_L") && name != "CREWLET_LOG" {
			t.Errorf("the layout-0 node created %s, a partitioned layout's stream", name)
		}
	}
	if err := names.Err(); err != nil {
		t.Fatalf("list the broker's streams: %v", err)
	}

	// THE REGISTER ROW: layout 0, which the wire omits, keyed by the domains.
	s.publishPositions(t.Context())
	rows, err := e.backends.Fleet.Positions(t.Context())
	if err != nil {
		t.Fatalf("read the register: %v", err)
	}
	var mine *coord.NodePositions
	for i := range rows {
		if rows[i].NodeID == e.id {
			mine = &rows[i]
		}
	}
	if mine == nil {
		t.Fatalf("the heartbeat published no row for %s: %+v", e.id, rows)
	}
	if mine.Layout != 0 {
		t.Errorf("the layout-0 node's row says layout %d", mine.Layout)
	}
	var keys []string
	for key := range mine.Domains {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	sortedRegistered := slices.Sorted(slices.Values(registered))
	if !slices.Equal(keys, sortedRegistered) {
		t.Errorf("the row names the logs %v, and a fleet's rows name %v", keys, sortedRegistered)
	}

	// THE FLOOR THE TRIM PUBLISHES: under the domain's key, at layout 0.
	r := &retention{fleet: e.backends.Fleet, state: s, nodeID: e.id,
		cfg: config.TrackerRetention{MinAgeRaw: "1ns"}}
	shared, err := r.read(t.Context())
	if err != nil {
		t.Fatalf("read the tick's inputs: %v", err)
	}
	running := s.Log(tracker.Domain{}.Name())
	if err := r.domain(t.Context(), running, shared); err != nil {
		t.Fatalf("the tracker's tick: %v", err)
	}
	floors, err := e.backends.Fleet.Floors(t.Context())
	if err != nil {
		t.Fatalf("read the floors: %v", err)
	}
	var found bool
	for _, f := range floors {
		if f.Domain == (tracker.Domain{}).Name() {
			found = true
			if f.Layout != 0 {
				t.Errorf("the tracker's floor says layout %d", f.Layout)
			}
		}
	}
	if !found {
		t.Errorf("the tick published no floor under the key %q: %+v", tracker.Domain{}.Name(), floors)
	}
}

// partitionedTestLayout is a layout 1 with two partitions of the tracker's log
// and one of the pages' — three logs, two of them one domain's, which is the
// case a runtime keyed by domain could not hold.
//
// THREE LOGS, because the replicated store a node opens pins one writer per
// log of layout 0, and every log's runner pins one for the life of its loop.
func partitionedTestLayout() statelog.Layout {
	return statelog.Layout{Number: 1, Spaces: []statelog.SpaceLayout{
		{Space: statelog.SpaceTracker, Partitions: 2, Domains: []string{tracker.Domain{}.Name()}},
		{Space: statelog.SpacePages, Partitions: 1, Domains: []string{pages.Domain{}.Name()}},
	}}
}

// aPartitionedStateLog boots a node with no company — so it runs no state log
// of its own — and starts a state log at the partitioned test layout over its
// backends.
func aPartitionedStateLog(t *testing.T) (*Engine, *stateLog, natsjs.JetStream) {
	t.Helper()
	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	e, err := New(t.Context(), Options{Bootstrap: &b})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	if n := e.native.Load(); n != nil && n.log != nil {
		t.Fatal("a node with no company runs a state log of its own, so the " +
			"one below would contend with it for the store's writers")
	}
	s, err := e.startStateLogAt(t.Context(), &b, "node-p", nil, partitionedTestLayout())
	if err != nil {
		t.Fatalf("start the partitioned state log: %v", err)
	}
	t.Cleanup(s.Stop)
	q, ok := e.backends.Queue.(interface{ Conn() *nats.Conn })
	if !ok {
		t.Fatalf("the stream is %T, not the JetStream backend", e.backends.Queue)
	}
	js, err := natsjs.New(q.Conn())
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return e, s, js
}

// lastSeq is a stream's last sequence on the broker.
func lastSeq(t *testing.T, js natsjs.JetStream, stream string) uint64 {
	t.Helper()
	st, err := js.Stream(t.Context(), stream)
	if err != nil {
		t.Fatalf("the stream %s: %v", stream, err)
	}
	info, err := st.Info(t.Context())
	if err != nil {
		t.Fatalf("read %s: %v", stream, err)
	}
	return info.State.LastSeq
}

// linearizableRead is one linearizable read through a running log's own read
// authority — the path a seat's tool takes — returning the position it was
// answered at.
func linearizableRead(t *testing.T, running *runningLog) statelog.Position {
	t.Helper()
	var answer statelog.Answer
	waitUntil(t, 20*time.Second, running.key+" to answer a linearizable read", func() bool {
		var err error
		answer, err = running.reader.Read(t.Context(), statelog.Query{
			Level: statelog.ReadLinearizable,
			Scope: statelog.ScopeSet{Paths: []string{"t"}},
		}, func(*sql.Tx) error { return nil })
		return err == nil
	})
	return answer.Position
}

// A STATE LOG RUNS EVERY LOG OF ITS LAYOUT, AND EACH IS ITS OWN.
//
// Two logs of one domain are the case a runtime keyed by domain could not hold:
// each needs its own stream, its own consumer, its own runner and checkpoint,
// its own read index — a barrier proves where ONE stream ends — and its own
// key in the register. So a layout with two tracker partitions is started over
// a real broker and each of those is read back: the streams are the partition
// grammar's and no layout-0 stream is made, a linearizable read on one log
// appends its barrier to that log alone and is applied there alone, and the
// heartbeat names every log under its own key with the layout beside it and the
// record version its build reads.
func TestAStateLogRunsEveryLogOfItsLayoutEachOnItsOwn(t *testing.T) {
	t.Parallel()
	_, s, js := aPartitionedStateLog(t)
	layout := partitionedTestLayout()

	want := []string{"pages@pages.000", "tracker@tracker.000", "tracker@tracker.001"}
	if got := s.held().order; !slices.Equal(got, want) {
		t.Fatalf("the state log runs %v, want %v in the layout's order", got, want)
	}
	for _, running := range s.running() {
		stream, prefix := layout.Stream(running.id)
		if running.spec.Name != stream || running.spec.SubjectPrefix != prefix {
			t.Errorf("%s runs on (%q, %q); the grammar names it (%q, %q)",
				running.key, running.spec.Name, running.spec.SubjectPrefix, stream, prefix)
		}
		budget := s.ceilings[running.domain.Name()].Bytes
		if share := layout.LogShare(running.domain.Name(), budget); running.spec.MaxBytes != share {
			t.Errorf("%s reserves %d bytes; its share of its domain's %d is %d",
				running.key, running.spec.MaxBytes, budget, share)
		}
		if lastSeq(t, js, running.spec.Name) != 0 {
			t.Fatalf("%s holds records before anything wrote to it", running.spec.Name)
		}
	}
	for _, stale := range []string{topics.TrackerLogStream, topics.PagesLogStream, topics.TrackerVectorsStream} {
		if _, err := js.Stream(t.Context(), stale); !errors.Is(err, natsjs.ErrStreamNotFound) {
			t.Errorf("a layout-1 state log made layout 0's %s (err %v)", stale, err)
		}
	}

	// ONE BARRIER, ON ONE LOG. The read index is per log, so a read of
	// tracker.000 proves tracker.000's end and appends nothing to
	// tracker.001, whose own read then appends its own.
	first, second := s.Log("tracker@tracker.000"), s.Log("tracker@tracker.001")
	at := linearizableRead(t, first)
	if at.Stream != first.spec.Name || at.Seq != 1 {
		t.Fatalf("the read on %s was answered at %s, want its own barrier at 1", first.key, at)
	}
	if got := lastSeq(t, js, second.spec.Name); got != 0 {
		t.Fatalf("a read on %s appended to %s, which now ends at %d", first.key, second.key, got)
	}
	if got := first.runner.Committed(); got.Seq != 1 {
		t.Fatalf("%s's applier stands at %s after its own barrier was read at 1", first.key, got)
	}
	if got := second.runner.Committed(); got.Seq != 0 {
		t.Fatalf("%s's applier moved to %s on another log's barrier", second.key, got)
	}
	if at := linearizableRead(t, second); at.Stream != second.spec.Name || at.Seq != 1 {
		t.Fatalf("the read on %s was answered at %s, want its own barrier at 1", second.key, at)
	}

	// THE HEARTBEAT NAMES EVERY LOG, with the layout beside the keys and the
	// record version each log's build reads.
	s.publishPositions(t.Context())
	rows, err := s.fleet.Positions(t.Context())
	if err != nil {
		t.Fatalf("read the register: %v", err)
	}
	var mine *coord.NodePositions
	for i := range rows {
		if rows[i].NodeID == "node-p" {
			mine = &rows[i]
		}
	}
	if mine == nil {
		t.Fatalf("the heartbeat published no row: %+v", rows)
	}
	if mine.Layout != 1 {
		t.Errorf("the row says layout %d, and its keys are layout 1's", mine.Layout)
	}
	for _, running := range s.running() {
		pos, held := mine.Domains[running.key]
		if !held {
			t.Errorf("the row names no position for %s: %v", running.key, mine.Domains)
			continue
		}
		if pos.RecordVersion != running.domain.RecordVersion() {
			t.Errorf("the row advertises reading version %d of %s, and this build "+
				"reads %d", pos.RecordVersion, running.key, running.domain.RecordVersion())
		}
		if pos.Seq != running.runner.Committed().Seq {
			t.Errorf("the row puts %s at %d and its applier stands at %d",
				running.key, pos.Seq, running.runner.Committed().Seq)
		}
	}

	// THE TRIM READS ITS OWN LAYOUT'S POSITIONS AND PUBLISHES ITS FLOOR
	// WITH THE LAYOUT BESIDE THE KEY, so a node of another layout holding
	// a log of the same key never takes this floor for its own.
	r := &retention{fleet: s.fleet, state: s, nodeID: "node-p",
		cfg: config.TrackerRetention{MinAgeRaw: "1ns"}}
	shared, err := r.read(t.Context())
	if err != nil {
		t.Fatalf("read the tick's inputs: %v", err)
	}
	var counted bool
	for _, row := range shared.positions {
		if _, named := row.Domains[first.key]; named && row.NodeID == "node-p" {
			counted = true
		}
	}
	if !counted {
		t.Fatalf("the trim read no position of %s for this node: %+v", first.key, shared.positions)
	}
	if err := r.domain(t.Context(), first, shared); err != nil {
		t.Fatalf("the %s tick: %v", first.key, err)
	}
	floors, err := s.fleet.Floors(t.Context())
	if err != nil {
		t.Fatalf("read the floors: %v", err)
	}
	var published bool
	for _, f := range floors {
		if f.Domain == first.key {
			published = true
			if f.Layout != 1 {
				t.Errorf("%s's floor says layout %d", first.key, f.Layout)
			}
		}
	}
	if !published {
		t.Errorf("the tick published no floor under %q: %+v", first.key, floors)
	}
}

// A LOG IS STOPPED AND STARTED WHILE THE NODE RUNS.
//
// It is how a node leaves and joins a partition: the log goes out of every
// reader's set before its applier ends, so nothing picks it up half-stopped;
// the heartbeat stops naming it; and started again it resumes from the
// checkpoint its rows keep, applying what was written while it was away. A log
// the layout does not carry is refused rather than guessed a stream for.
func TestALogIsStoppedAndStartedWhileTheNodeRuns(t *testing.T) {
	t.Parallel()
	_, s, js := aPartitionedStateLog(t)
	leaving := statelog.LogID{Domain: tracker.Domain{}.Name(),
		Partition: statelog.PartitionID{Space: statelog.SpaceTracker, Index: 1}}
	running := s.Log(leaving.String())
	linearizableRead(t, running)

	s.stopLogs([]statelog.LogID{leaving})
	if s.Log(leaving.String()) != nil {
		t.Fatal("the stopped log is still in the set every reader walks")
	}
	s.applyMu.Lock()
	_, applying := s.applying[leaving.String()]
	s.applyMu.Unlock()
	if applying {
		t.Error("the stopped log's applier is still running")
	}
	s.publishPositions(t.Context())
	rows, err := s.positions(t.Context())
	if err != nil {
		t.Fatalf("read the register: %v", err)
	}
	for _, row := range rows {
		if _, named := row.Domains[leaving.String()]; named && row.NodeID == "node-p" {
			t.Error("the heartbeat still names the stopped log, so the trim still " +
				"counts this node on it")
		}
	}

	// WRITTEN TO WHILE IT WAS AWAY, by the fleet's other holders.
	other := s.Log("tracker@tracker.000")
	encode := barrierEncoder(running.domain)
	body, err := encode(statelog.Envelope{
		V: statelog.BarrierVersion, Kind: statelog.BarrierKind,
		Subject: statelog.Subject{Kind: statelog.BarrierKind},
		Gen:     running.runner.Committed().Generation,
		Scope:   statelog.ScopeSet{Paths: []string{statelog.BarrierScope}},
	})
	if err != nil {
		t.Fatalf("encode a barrier: %v", err)
	}
	if _, _, err := running.log.Append(t.Context(),
		topics.LogSubject(running.spec.SubjectPrefix, statelog.BarrierKind, ""), "", nil, body); err != nil {
		t.Fatalf("append while the log is stopped: %v", err)
	}

	if err := s.startLogs(t.Context(), []statelog.LogID{leaving}); err != nil {
		t.Fatalf("start the log again: %v", err)
	}
	back := s.Log(leaving.String())
	if back == nil || back == running {
		t.Fatalf("the started log is %p; want a new runtime in the set, not %p", back, running)
	}
	if got := s.held().order; !slices.Equal(got,
		[]string{"pages@pages.000", "tracker@tracker.000", "tracker@tracker.001"}) {
		t.Fatalf("after the restart the set is %v, out of the layout's order", got)
	}
	end := lastSeq(t, js, back.spec.Name)
	waitUntil(t, 20*time.Second, "the restarted log to apply what was written while it was away",
		func() bool { return back.runner.Committed().Seq == end })
	if other.runner.Committed().Seq != 0 {
		t.Errorf("the other tracker log moved to %s", other.runner.Committed())
	}
	// AND AGAIN IS NOTHING: a retry after a partial failure starts only
	// what is missing.
	if err := s.startLogs(t.Context(), []statelog.LogID{leaving}); err != nil {
		t.Fatalf("start a running log: %v", err)
	}
	if s.Log(leaving.String()) != back {
		t.Error("starting a running log replaced it")
	}

	absent := statelog.LogID{Domain: tracker.Domain{}.Name(),
		Partition: statelog.PartitionID{Space: statelog.SpaceTracker, Index: 2}}
	if err := s.startLogs(t.Context(), []statelog.LogID{absent}); err == nil ||
		!strings.Contains(err.Error(), "not a log of layout 1") {
		t.Errorf("starting %s, which layout 1 does not carry, answered %v", absent, err)
	}
}

// A RECOVERY LOCKS ITS PARTITIONS AND NO OTHERS, IN ONE ORDER.
//
// Work on one partition must never wait on work on another, and two callers
// naming overlapping sets in different orders must not each hold what the
// other waits for.
func TestARecoveryLocksItsPartitionsAndNoOthers(t *testing.T) {
	t.Parallel()
	var locks partitionLocks
	p := statelog.PartitionID{Space: statelog.SpaceTracker, Index: 0}
	q := statelog.PartitionID{Space: statelog.SpaceTracker, Index: 1}

	unlockP := locks.lock(p)
	took := make(chan struct{})
	go func() { locks.lock(q)(); close(took) }()
	select {
	case <-took:
	case <-time.After(10 * time.Second):
		t.Fatal("a recovery of one partition waited on another's")
	}
	blocked := make(chan struct{})
	go func() { locks.lock(p)(); close(blocked) }()
	select {
	case <-blocked:
		t.Fatal("two recoveries of one partition ran together")
	case <-time.After(100 * time.Millisecond):
	}
	unlockP()
	<-blocked

	// IN ONE ORDER, WHATEVER ORDER THEY WERE NAMED IN: a recovery naming q
	// then p, while p is held, waits for p holding nothing — so a
	// recovery of q alone is not stuck behind it. Taken in the order they
	// were named, it would hold q while it waits, and a second recovery
	// naming p then q would hold p waiting for q: the deadlock.
	unlockP = locks.lock(p)
	waiting := make(chan struct{})
	go func() { locks.lock(q, p)(); close(waiting) }()
	time.Sleep(100 * time.Millisecond)
	alone := make(chan struct{})
	go func() { locks.lock(q)(); close(alone) }()
	select {
	case <-alone:
	case <-time.After(10 * time.Second):
		t.Fatal("a recovery waiting for one partition held another it named " +
			"after it, which is the half of a deadlock a second recovery completes")
	}
	unlockP()
	<-waiting

	// A PARTITION NAMED TWICE IS TAKEN ONCE: two logs of one partition — a
	// domain's and its vectors' — name it twice, and a mutex is not
	// reentrant.
	twice := make(chan struct{})
	go func() { locks.lock(p, q, p)(); close(twice) }()
	select {
	case <-twice:
	case <-time.After(10 * time.Second):
		t.Fatal("a recovery naming one partition twice waited on itself")
	}
}
