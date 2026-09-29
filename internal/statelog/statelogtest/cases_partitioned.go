package statelogtest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
)

// PartitionedCandidate is a domain certified on a layout that DIVIDES it: the
// domain's log in more than one partition, several nodes holding each, and
// every write and every record put through the framework's own publisher and
// apply loop on real logs.
//
// # What it is for
//
// The floor theorem is stated over one stream, and a divided layout keeps that
// true — every log is one stream — while adding a way to break each clause
// without breaking any number. Three gates hold it per log (the statelog
// package doc's "who may write a log"), and each is a rule about a record or a
// write that no single-log case can reach: a scope naming another partition's
// object, a record on another partition's log, a node writing a partition it
// does not serve. A partition function that ships silently wrong under layout
// 0 — where every answer is the one partition — is wrong the day the layout
// divides, on every node at once. So a domain that is divided is certified
// here, on the divided layout, before it runs on one.
//
// Its CONTROL — a domain that files each object in the partition its id names
// — comes back clean, and a domain lying in either direction is caught: one
// whose write declares a scope in another partition (gate 1), and one whose
// writes are routed to a partition its own partition function does not place
// them in (gate 2). So is a partition function that places an object's path in
// no partition, which would otherwise disarm gate 1 silently.
type PartitionedCandidate struct {
	// Candidate is the domain as [Run] certifies it. Of it this family uses
	// the Domain, its Applier, Migrate, Rows and Layout — which must carry
	// the domain's log in at least two partitions. Log is not read: every
	// partition's log is run.
	Candidate

	// Gates is the publisher's side of the domain's gates over one
	// partition's rows: what answers a write whose record a gate dropped.
	Gates func(db store.PartitionReader) statelog.Gates

	// Object is an object the domain places in partition p, the n-th of as
	// many distinct ones as a case needs — what a router sends to p.
	Object func(p statelog.PartitionID, n int) string

	// WriteObject writes object id through the domain's OWN write path, on
	// pub and over the partition's rows db.
	WriteObject func(ctx context.Context, pub *statelog.Publisher, db store.PartitionReader,
		id, opID string) (statelog.Result, error)

	// Release is the domain's own write path for node's release of pub's
	// log — node is the one pub publishes as, since a release is only ever a
	// node's own — and Readmit for pub's node readmitting node to it.
	Release func(ctx context.Context, pub *statelog.Publisher, db store.PartitionReader,
		node, opID string) (statelog.Result, error)
	Readmit func(ctx context.Context, pub *statelog.Publisher, db store.PartitionReader,
		node, opID string) (statelog.Result, error)

	// Holds reports whether a partition's rows hold object id.
	Holds func(ctx context.Context, db store.PartitionReader, id string) (bool, error)

	// LogTerms are the scope paths the domain names its LOG by — its domain
	// term, its family terms, a node gate's term — each of which lies in
	// whichever log it is written to, so [statelog.Domain.ScopePartition]
	// answers false for it. At least one.
	//
	// DECLARED BESIDE THE DOMAIN rather than read from it, because the
	// partition function is what this family certifies, and a false from it
	// is exactly the answer that disarms gate 1: a path placed in no
	// partition is never refused, so a function that disclaimed an object's
	// path — an over-broad prefix taking a family's objects for its term —
	// would pass every write and every read-back while filing deferrals the
	// partition the path names never probes. So every other path a record
	// declares must be placed, and in its log's partition.
	LogTerms []string
}

// PartitionedFactory builds a fresh partitioned candidate for one run.
type PartitionedFactory func(t *testing.T) PartitionedCandidate

// RunPartitioned certifies a divided domain on the framework's three per-log
// gates — see [Partitioned].
func RunPartitioned(t *testing.T, new PartitionedFactory) {
	t.Helper()
	t.Run("the three gates hold every log of a divided layout", func(t *testing.T) {
		if err := Partitioned(t, new); err != nil {
			t.Fatal(err)
		}
	})
}

// The nodes of a partitioned run. Each holds and APPLIES every partition's log;
// partition i is SERVED by nodes i and i+1 (mod three) and the third is still
// joining it — counted and applying, and not yet writing — so every partition
// has two writers and one node that must not write.
var partitionedNodes = []string{"parted-node-a", "parted-node-b", "parted-node-c"}

// partitionedDeadline bounds every wait for the appliers to reach a log's end:
// long enough for three nodes' loops on a raced runner, short enough that a
// loop that stopped fails the run rather than hangs it.
const partitionedDeadline = 20 * time.Second

// Partitioned runs a divided domain through the framework on real logs and
// reports every place a gate did not hold, in order:
//
//  1. Every write a partition's serving node makes applies on every node that
//     applies that partition's log, identically, and in no other partition.
//  2. A node that does not serve a partition — one still joining it — writes
//     nothing to its log (gate 3), whatever it asks.
//  3. A write routed to a partition its object is not in is refused before the
//     broker, and a record appended to another partition's log is dropped on
//     every holder of that log, `wrong_partition` (gate 2).
//  4. A RELEASE RACING A WRITER: a serving node leaves a partition between one
//     of its writes' last question and that write's landing, so the write lands
//     above the node's release — and is dropped on every holder, its writer told
//     `released`; after the release the node writes nothing there.
//  5. A readmission by another serving node takes the leaver back: once it
//     serves again, what it writes applies on every holder.
//  6. Every record on every log declares a scope inside its log's partition
//     (gate 1) and belongs there (gate 2) — read back off the logs, so a
//     publisher that stopped refusing would not take the certification with
//     it — save the one record case 3 routed wrong on purpose. INSIDE means
//     placed there: a path the domain places in no partition is one of its
//     declared log terms or a path gate 1 cannot see ([PartitionedCandidate.LogTerms]).
//
// EXPORTED AND RETURNING THE VERDICT, for [Declaration]'s reason: the suite's
// own tests hand it domains and appliers that lie, and read the report back.
func Partitioned(t *testing.T, new PartitionedFactory) error {
	t.Helper()
	c := new(t)
	w := openPartitioned(t, c)
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	name := c.Domain.Name()
	ctx := t.Context()

	// 0. A LOG TERM IS PLACED NOWHERE — the declaration and the partition
	// function must agree before either can vouch for the other.
	for _, term := range c.LogTerms {
		if lies, placed := c.Domain.ScopePartition(w.layout, term); placed {
			add("%s declares %q a term naming its log, and its partition function "+
				"places it in %s", name, term, lies)
		}
	}

	// 1. EVERY WRITE APPLIES WHERE ITS OBJECT IS, AND ONLY THERE.
	written := map[statelog.PartitionID][]string{}
	for _, p := range w.partitions {
		for n, node := range w.servers(p) {
			id := c.Object(p, n)
			held := node.parts[p]
			if _, err := c.WriteObject(ctx, held.pub, held.db.Reader(), id,
				"parted-write-"+id); err != nil {
				// A PARTITION GATE'S REFUSAL HERE is the domain
				// disagreeing with itself: its router sent the object to
				// p, and its own partition function places the record,
				// or a path its write declares, elsewhere.
				verdict := "was refused"
				if errors.Is(err, statelog.ErrScopeCrossesPartitions) ||
					errors.Is(err, statelog.ErrWrongPartition) {
					verdict = "was refused by the partition gates"
				}
				add("%s's own write of %s, which its router sends to %s, through "+
					"%s's publisher for %s %s: %v", name, id, p, node.id, p, verdict, err)
				continue
			}
			written[p] = append(written[p], id)
		}
	}
	w.drain()
	for _, p := range w.partitions {
		for _, id := range written[p] {
			for _, node := range w.nodes {
				for _, q := range w.partitions {
					holds, err := c.Holds(ctx, node.parts[q].db.Reader(), id)
					switch {
					case err != nil:
						add("read %s on %s's copy of %s: %v", id, node.id, q, err)
					case q == p && !holds:
						add("%s's copy of %s does not hold %s, written to %s's log — "+
							"a holder that applies the log misses a write on it", node.id,
							q, id, p)
					case q != p && holds:
						add("%s's copy of %s holds %s, which was written to %s — an "+
							"object applied in a partition it is not in", node.id, q, id, p)
					}
				}
			}
		}
	}
	problems = append(problems, w.identical("after every server wrote")...)

	// 2. A NODE STILL JOINING A PARTITION WRITES NOTHING THERE.
	for _, p := range w.partitions {
		joiner := w.joiner(p)
		held := joiner.parts[p]
		end := w.end(p)
		id := c.Object(p, 90)
		_, err := c.WriteObject(ctx, held.pub, held.db.Reader(), id, "parted-joiner-"+id)
		if !errors.Is(err, statelog.ErrNotHolder) {
			add("%s, which applies %s and does not serve it yet, was answered %v "+
				"writing %s there, want %v — a writer the partition's trim may not "+
				"be counting", joiner.id, p, err, id, statelog.ErrNotHolder)
		}
		if after := w.end(p); after != end {
			add("%s's write to %s, which it does not serve, reached the log "+
				"(its end moved from %d to %d)", joiner.id, p, end, after)
		}
	}

	// 3. GATE 2: ROUTED TO THE WRONG PARTITION, AND APPENDED TO IT.
	p, q := w.partitions[0], w.partitions[1]
	server := w.servers(p)[0]
	stray := c.Object(q, 91)
	end := w.end(p)
	_, err := c.WriteObject(ctx, server.parts[p].pub, server.parts[p].db.Reader(), stray,
		"parted-stray-"+stray)
	// EITHER GATE MAY BE THE ONE: a write naming its own object in its scope
	// meets gate 1 before its record is decided, and one that does not meets
	// gate 2 on the record. What may not happen is an append.
	if !errors.Is(err, statelog.ErrWrongPartition) &&
		!errors.Is(err, statelog.ErrScopeCrossesPartitions) {
		add("%s's write of %s, which it places in %s, through %s's publisher "+
			"for %s was answered %v, want %v or %v", name, stray, q, server.id, p, err,
			statelog.ErrWrongPartition, statelog.ErrScopeCrossesPartitions)
	}
	if after := w.end(p); after != end {
		add("the write of %s routed to %s reached its log (its end moved from %d "+
			"to %d)", stray, p, end, after)
	}
	// AND A RECORD OF q ON p's LOG, as another writer's router would put one
	// there: decided and appended on q by one of q's servers, and the same
	// bytes appended to p's log beside it.
	misrouted := c.Object(q, 92)
	qServer := w.servers(q)[0]
	// COUNTED FROM BEFORE THE APPEND, because the appliers run while this
	// does and may drop the copy before the write below even returns.
	gatedBefore := w.gated(p, statelog.ReasonWrongPartition)
	qServer.parts[q].misroute.arm(w.specs[p].SubjectPrefix, w.logs[p])
	_, err = c.WriteObject(ctx, qServer.parts[q].pub, qServer.parts[q].db.Reader(), misrouted,
		"parted-misrouted-"+misrouted)
	if err != nil {
		add("%s's own write of %s through %s's publisher for %s was refused: %v",
			name, misrouted, qServer.id, q, err)
	}
	misroutedAt := qServer.parts[q].misroute.landed()
	if misroutedAt == 0 {
		add("the copy of %s meant for %s's log never reached it", misrouted, p)
	}
	w.drain()
	for _, node := range w.nodes {
		if holds, readErr := c.Holds(ctx, node.parts[p].db.Reader(), misrouted); readErr != nil || holds {
			add("%s's copy of %s holds %s — a record of %s on %s's log applied "+
				"there (%v)", node.id, p, misrouted, q, p, readErr)
		}
		if holds, readErr := c.Holds(ctx, node.parts[q].db.Reader(), misrouted); readErr != nil || !holds {
			add("%s's copy of %s does not hold %s, written there (%v)", node.id, q,
				misrouted, readErr)
		}
		if got := w.gated(p, statelog.ReasonWrongPartition)[node.id] - gatedBefore[node.id]; got != 1 {
			add("%s counted %d record(s) on %s's log gated %q, want the one routed "+
				"there wrongly — a dropped record's only witness is that counter",
				node.id, got, p, statelog.ReasonWrongPartition)
		}
	}

	// 4. A RELEASE RACING A WRITER.
	leaver, stayer := w.servers(p)[0], w.servers(p)[1]
	racing := c.Object(p, 93)
	var released bool
	leaving := leaver.parts[p]
	leaving.race.Before(func(subject string) bool { return strings.HasSuffix(subject, "."+racing) },
		func() {
			// THE LEAVE, between the in-flight write's last question and
			// its landing: the node stops serving, then releases the log.
			leaver.holding.Stop(p)
			if _, releaseErr := c.Release(ctx, leaving.pub, leaving.db.Reader(), leaver.id,
				"parted-release-"+leaver.id); releaseErr != nil {
				add("%s's release of %s's log was refused: %v", leaver.id, p, releaseErr)
				return
			}
			released = true
		})
	_, err = c.WriteObject(ctx, leaving.pub, leaving.db.Reader(), racing, "parted-racing-"+racing)
	var refusal *statelog.Unavailable
	if !released {
		add("the race never ran: the write of %s did not reach the broker through "+
			"%s's publisher", racing, leaver.id)
	} else if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonReleased {
		add("%s's write of %s, which landed after %s released %s's log, was "+
			"answered %v, want a refusal %q — it is on the log and applies nowhere",
			leaver.id, racing, leaver.id, p, err, statelog.ReasonReleased)
	}
	w.drain()
	for _, node := range w.nodes {
		if holds, err := c.Holds(ctx, node.parts[p].db.Reader(), racing); err != nil || holds {
			add("%s's copy of %s holds %s, which %s wrote after it released the "+
				"log (%v) — the release gate did not drop it", node.id, p, racing,
				leaver.id, err)
		}
	}
	after := c.Object(p, 94)
	end = w.end(p)
	if _, err := c.WriteObject(ctx, leaving.pub, leaving.db.Reader(), after,
		"parted-after-"+after); !errors.Is(err, statelog.ErrNotHolder) {
		add("%s, which left %s, was answered %v writing %s there, want %v",
			leaver.id, p, err, after, statelog.ErrNotHolder)
	}
	if got := w.end(p); got != end {
		add("a write asked of %s after it left %s reached the log (its end moved "+
			"from %d to %d)", leaver.id, p, end, got)
	}
	problems = append(problems, w.identical("after a release raced a writer")...)

	// 5. A READMISSION TAKES THE LEAVER BACK.
	kept := stayer.parts[p]
	if _, err := c.Readmit(ctx, kept.pub, kept.db.Reader(), leaver.id,
		"parted-readmit-"+leaver.id); err != nil {
		add("%s's readmission of %s to %s's log was refused: %v", stayer.id,
			leaver.id, p, err)
	}
	w.drain()
	leaver.holding.Serve(p)
	back := c.Object(p, 95)
	if _, err := c.WriteObject(ctx, leaving.pub, leaving.db.Reader(), back,
		"parted-back-"+back); err != nil {
		add("%s, readmitted to %s and serving it again, was refused writing %s: %v",
			leaver.id, p, back, err)
	}
	w.drain()
	for _, node := range w.nodes {
		if holds, err := c.Holds(ctx, node.parts[p].db.Reader(), back); err != nil || !holds {
			add("%s's copy of %s does not hold %s, written by %s after its "+
				"readmission (%v)", node.id, p, back, leaver.id, err)
		}
	}
	problems = append(problems, w.identical("after the readmission")...)

	// 6. EVERY RECORD ON EVERY LOG, READ BACK.
	problems = append(problems, w.onItsOwnPartition(p, misroutedAt)...)

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%s breaks a per-log gate on a divided layout:\n  %s", name,
		strings.Join(problems, "\n  "))
}

// partitionedWorld is one partitioned run: a broker, the domain's log in every
// partition of the layout, and the nodes applying them.
type partitionedWorld struct {
	t          *testing.T
	c          PartitionedCandidate
	broker     *js.Queue
	layout     statelog.Layout
	partitions []statelog.PartitionID
	logs       map[statelog.PartitionID]*js.DomainLog
	specs      map[statelog.PartitionID]statelog.StreamSpec
	nodes      []*partitionedNode
}

// partitionedNode is one node: its store, what it serves, and every partition
// it holds.
type partitionedNode struct {
	id      string
	holding *Holding
	parts   map[statelog.PartitionID]*partitionHeld
}

// partitionHeld is one partition on one node: its file, its log's applier and
// write authority, the two hands a case has on the broker that authority
// appends through, and the applier's instruments.
type partitionHeld struct {
	db       store.PartitionHandle
	runner   *statelog.Runner
	pub      *statelog.Publisher
	race     *Race
	misroute *misroutingLog
	metrics  *metrics.Recorder
}

// openPartitioned brings up the run's world: every log provisioned, every node
// holding every partition with its applier running, and each partition served
// by the two nodes [partitionedNodes] names.
func openPartitioned(t *testing.T, c PartitionedCandidate) *partitionedWorld {
	t.Helper()
	layout := c.layout()
	logs := layout.LogsOf(c.Domain.Name())
	switch {
	case c.Gates == nil || c.Object == nil || c.WriteObject == nil ||
		c.Release == nil || c.Readmit == nil || c.Holds == nil || c.Rows == nil ||
		len(c.LogTerms) == 0:
		t.Fatal("the partitioned candidate leaves a hook out, so the run cannot " +
			"write, read or gate what it certifies — FATAL rather than a skip, for " +
			"the reason requireKinds gives")
	case len(logs) < 2:
		t.Fatalf("layout %d carries %d log(s) of %s — a partitioned run needs the "+
			"domain divided, or no record has another partition to belong to",
			layout.Number, len(logs), c.Domain.Name())
	}
	w := &partitionedWorld{t: t, c: c, layout: layout,
		logs:  map[statelog.PartitionID]*js.DomainLog{},
		specs: map[statelog.PartitionID]statelog.StreamSpec{},
	}
	q, err := js.Open(t.Context(), js.Config{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open a broker: %v", err)
	}
	t.Cleanup(func() {
		if stopErr := q.Stop(context.WithoutCancel(t.Context())); stopErr != nil {
			t.Errorf("stop the broker: %v", stopErr)
		}
	})
	w.broker = q
	for _, log := range logs {
		spec := layout.StreamSpec(c.Domain, log)
		if ensureErr := q.EnsureDomainStream(t.Context(), js.DomainStream{
			Name: spec.Name, Subjects: spec.Subjects, MaxBytes: spec.MaxBytes,
			Duplicates: spec.Duplicates,
		}); ensureErr != nil {
			t.Fatalf("provision %s: %v", log, ensureErr)
		}
		if w.logs[log.Partition], err = q.DomainLog(t.Context(), spec.Name); err != nil {
			t.Fatalf("open %s: %v", log, err)
		}
		w.specs[log.Partition] = spec
		w.partitions = append(w.partitions, log.Partition)
	}

	// THE LOOPS END BEFORE ANYTHING THEY USE: cleanups run last-first, so
	// this one, registered after the broker's, stops every applier before
	// the stores close and the broker goes.
	run, stop := context.WithCancel(context.WithoutCancel(t.Context()))
	var loops sync.WaitGroup
	for i, id := range partitionedNodes {
		node := &partitionedNode{id: id, holding: NewHolding(),
			parts: map[statelog.PartitionID]*partitionHeld{}}
		for pi, p := range w.partitions {
			if pi%len(partitionedNodes) == i || (pi+1)%len(partitionedNodes) == i {
				node.holding.Serve(p)
			}
		}
		nodeDB, err := store.OpenNode(t.Context(), filepath.Join(t.TempDir(), id+".db"), store.Options{})
		if err != nil {
			t.Fatalf("open %s's store: %v", id, err)
		}
		t.Cleanup(func() {
			if err := nodeDB.Close(); err != nil {
				t.Errorf("close %s's store: %v", id, err)
			}
		})
		for _, p := range w.partitions {
			node.parts[p] = w.hold(t.Context(), run, &loops, node, nodeDB, p)
		}
		w.nodes = append(w.nodes, node)
	}
	t.Cleanup(func() {
		stop()
		loops.Wait()
	})
	return w
}

// hold opens partition p on node and brings up its log's applier and write
// authority there: set up under ctx, the applier's loop run under run, which
// outlives the call.
func (w *partitionedWorld) hold(ctx, run context.Context, loops *sync.WaitGroup,
	node *partitionedNode, nodeDB *store.DB, p statelog.PartitionID) *partitionHeld {

	t, c := w.t, w.c
	t.Helper()
	file, err := w.layout.File(p)
	if err != nil {
		t.Fatalf("the file of %s: %v", p, err)
	}
	if _, openErr := nodeDB.OpenPartition(ctx, file); openErr != nil {
		t.Fatalf("open %s on %s: %v", p, node.id, openErr)
	}
	db := nodeDB.PartitionHandle(file.Name)
	if c.Migrate != nil {
		if migrateErr := c.Migrate(ctx, db); migrateErr != nil {
			t.Fatalf("create %s's tables in %s on %s: %v", c.Domain.Name(), p, node.id, migrateErr)
		}
	}
	spec, log := w.specs[p], w.logs[p]
	id := statelog.LogID{Domain: c.Domain.Name(), Partition: p}
	stats, err := log.Stats(ctx)
	if err != nil {
		t.Fatalf("read %s's identity: %v", id, err)
	}
	checkpoint, _, err := statelog.CheckpointOf(ctx, db.Reader(), spec.Name)
	if err != nil {
		t.Fatalf("read %s's checkpoint on %s: %v", id, node.id, err)
	}
	consumer, err := w.broker.DomainConsumer(ctx, spec.Name, node.id, checkpoint.At.Seq)
	if err != nil {
		t.Fatalf("open %s's consumer on %s: %v", id, node.id, err)
	}
	recorder, err := metrics.New()
	if err != nil {
		t.Fatalf("a metrics recorder: %v", err)
	}
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain: c.Domain, Applier: c.Applier, Fetch: consumer, Spec: spec,
		Layout: w.layout, LogID: id, Log: log, Node: nodeDB, DB: db,
		Checkpoint: checkpoint.At, CheckpointStoredAt: checkpoint.StoredAt,
		StreamCreatedAt: stats.CreatedAt.UTC(), Metrics: recorder, NodeID: node.id,
	})
	if err != nil {
		t.Fatalf("build %s's applier on %s: %v", id, node.id, err)
	}
	loops.Add(1)
	go func() {
		defer loops.Done()
		defer consumer.Close()
		_ = runner.Run(run)
	}()
	rows, err := c.Rows(db.Reader(), spec)
	if err != nil {
		t.Fatalf("build %s's read seam on %s: %v", id, node.id, err)
	}
	misroute := &misroutingLog{Appender: log, from: spec.SubjectPrefix}
	race := NewRace(misroute)
	deps := statelog.Deps{
		Domain: c.Domain, Spec: spec, Layout: w.layout, LogID: id,
		Holding: node.holding, Log: race, Rows: rows,
		Fence: openFence{}, Gates: c.Gates(db.Reader()),
		Waiter: runner, Identity: runner, Metrics: recorder, NodeID: node.id,
		Generation:    func() uint32 { return runner.Committed().Generation },
		ResolveBudget: partitionedDeadline / 4,
	}
	if statelog.KeepsGateReserve(c.Domain) {
		reserve, reserveErr := statelog.NewReserve(spec.Name,
			func(ctx context.Context) (statelog.Usage, error) {
				st, statsErr := log.Stats(ctx)
				return statelog.Usage{Bytes: st.Bytes, MaxBytes: st.MaxBytes}, statsErr
			})
		if reserveErr != nil {
			t.Fatalf("build %s's gate reserve: %v", id, reserveErr)
		}
		deps.Admission = reserve
	}
	pub, err := statelog.NewPublisher(deps)
	if err != nil {
		t.Fatalf("build %s's write authority on %s: %v", id, node.id, err)
	}
	return &partitionHeld{db: db, runner: runner, pub: pub, race: race,
		misroute: misroute, metrics: recorder}
}

// servers is the nodes that serve p, in the run's node order.
func (w *partitionedWorld) servers(p statelog.PartitionID) []*partitionedNode {
	var out []*partitionedNode
	for _, node := range w.nodes {
		if serving, _ := node.holding.Serving(p); serving {
			out = append(out, node)
		}
	}
	return out
}

// joiner is the node that applies p's log and does not serve it.
func (w *partitionedWorld) joiner(p statelog.PartitionID) *partitionedNode {
	for _, node := range w.nodes {
		if serving, _ := node.holding.Serving(p); !serving {
			return node
		}
	}
	w.t.Fatalf("every node serves %s, so none is joining it", p)
	return nil
}

// end is where p's log ends now.
func (w *partitionedWorld) end(p statelog.PartitionID) uint64 {
	w.t.Helper()
	end, err := w.logs[p].End(w.t.Context())
	if err != nil {
		w.t.Fatalf("read %s's log's end: %v", p, err)
	}
	return end
}

// drain waits until every node has applied every partition's log to where it
// ends now.
func (w *partitionedWorld) drain() {
	w.t.Helper()
	for _, p := range w.partitions {
		end := w.end(p)
		for _, node := range w.nodes {
			runner := node.parts[p].runner
			deadline := time.Now().Add(partitionedDeadline)
			for runner.Committed().Seq < end {
				if err := runner.Stopped(); err != nil {
					w.t.Fatalf("%s's applier of %s stopped at %s: %v", node.id, p,
						runner.Committed(), err)
				}
				if time.Now().After(deadline) {
					w.t.Fatalf("%s's applier of %s reached %d of %d", node.id, p,
						runner.Committed().Seq, end)
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
	}
}

// gated is how many records each node's applier of p dropped under reason.
func (w *partitionedWorld) gated(p statelog.PartitionID, reason statelog.Reason) map[string]uint64 {
	out := map[string]uint64{}
	for _, node := range w.nodes {
		for _, s := range node.parts[p].metrics.Read() {
			if s.Name == metrics.StatelogRecordsGated && s.Attrs["gate"] == string(reason) {
				out[node.id] += s.Total
			}
		}
	}
	return out
}

// identical reports every partition whose replicated rows differ between two
// of the nodes applying its log — which no gate may cause, because every gate
// is the same function of the same bytes on every node.
func (w *partitionedWorld) identical(when string) []string {
	w.t.Helper()
	var out []string
	for _, p := range w.partitions {
		var first map[string][]string
		for i, node := range w.nodes {
			rows := replicatedRows(w.t, node.parts[p].db, w.c.Domain.Tables())
			if i == 0 {
				first = rows
				continue
			}
			for table, want := range first {
				if !slices.Equal(rows[table], want) {
					out = append(out, fmt.Sprintf("%s: %s and %s hold different %s "+
						"rows in %s:\n    %v\n    %v", when, w.nodes[0].id, node.id,
						table, p, want, rows[table]))
				}
			}
		}
	}
	return out
}

// onItsOwnPartition reads every record back off every log and reports one whose
// scope names another partition's object — or an object its domain places in
// no partition, which is one gate 1 cannot see — or that belongs to another
// partition than its log's: all but the record at misrouted on p's log, which
// the run put there on purpose.
func (w *partitionedWorld) onItsOwnPartition(p statelog.PartitionID, misrouted uint64) []string {
	w.t.Helper()
	var out []string
	for _, at := range w.partitions {
		end := w.end(at)
		for seq := uint64(1); seq <= end; seq++ {
			if at == p && seq == misrouted {
				continue
			}
			_, payload, _, ok, err := w.logs[at].At(w.t.Context(), seq)
			if err != nil {
				w.t.Fatalf("read %s's record %d: %v", at, seq, err)
			}
			if !ok {
				continue
			}
			env, err := w.c.Domain.Envelope(payload)
			if err != nil {
				out = append(out, fmt.Sprintf("%s's record %d does not decode through "+
					"its own domain's envelope reader: %v", at, seq, err))
				continue
			}
			if belongs, placed := w.c.Domain.PartitionOf(w.layout, env); placed && belongs != at {
				out = append(out, fmt.Sprintf("the %s record at %s's %d belongs to %s",
					env.Kind, at, seq, belongs))
			}
			for _, path := range env.Scope.Paths {
				lies, placed := w.c.Domain.ScopePartition(w.layout, path)
				switch {
				case placed && lies != at:
					out = append(out, fmt.Sprintf("the %s record at %s's %d declares "+
						"the path %q, which lies in %s", env.Kind, at, seq, path, lies))
				case !placed && !slices.Contains(w.c.LogTerms, path):
					// NOT A TERM NAMING THE LOG, so an object — and one
					// the partition function places in no partition,
					// which gate 1 therefore never refuses wherever it
					// lies. A deferral filed under it is probed only here.
					out = append(out, fmt.Sprintf("the %s record at %s's %d declares "+
						"the path %q, which its domain places in no partition and "+
						"which is not one of the terms %v it names its log by — gate 1 "+
						"cannot see where such a path lies", env.Kind, at, seq, path,
						w.c.LogTerms))
				}
			}
		}
	}
	return out
}

// replicatedRows is every row of every table the domain declares replicated,
// each rendered and the lot sorted, so two copies that hold the same rows in
// any order compare equal.
func replicatedRows(t *testing.T, db store.PartitionHandle,
	tables map[string]statelog.TableClass) map[string][]string {

	t.Helper()
	out := map[string][]string{}
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		for table, class := range tables {
			if class != statelog.Replicated {
				continue
			}
			rows, err := tx.QueryContext(t.Context(), `SELECT * FROM `+table)
			if err != nil {
				return err
			}
			columns, err := rows.Columns()
			if err != nil {
				_ = rows.Close()
				return err
			}
			var rendered []string
			for rows.Next() {
				values := make([]any, len(columns))
				ptrs := make([]any, len(columns))
				for i := range values {
					ptrs[i] = &values[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					_ = rows.Close()
					return err
				}
				rendered = append(rendered, fmt.Sprint(values...))
			}
			if err := rows.Close(); err != nil {
				return err
			}
			if err := rows.Err(); err != nil {
				return err
			}
			slices.Sort(rendered)
			out[table] = rendered
		}
		return nil
	}); err != nil {
		t.Fatalf("read the replicated rows: %v", err)
	}
	return out
}

// misroutingLog is a broker that, once armed, appends the next record it is
// handed to its own log AND, the same bytes, to another partition's — what a
// writer whose router sent a record to the wrong log leaves there. Unarmed,
// it appends to its own log alone.
type misroutingLog struct {
	statelog.Appender
	from string

	mu   sync.Mutex
	to   string
	onto *js.DomainLog
	seq  uint64
}

// arm makes the next append copied onto the log whose prefix is to.
func (m *misroutingLog) arm(to string, onto *js.DomainLog) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.to, m.onto = to, onto
}

// Append appends to the log it wraps, then — armed — the copy to the other.
func (m *misroutingLog) Append(ctx context.Context, subject, msgID string, expect *uint64,
	body []byte) (uint64, bool, error) {

	seq, duplicate, err := m.Appender.Append(ctx, subject, msgID, expect, body)
	m.mu.Lock()
	to, onto := m.to, m.onto
	if err == nil {
		m.to, m.onto = "", nil
	}
	m.mu.Unlock()
	if err != nil || onto == nil {
		return seq, duplicate, err
	}
	kind, id, _ := strings.Cut(strings.TrimPrefix(subject, m.from+"."), ".")
	copied, _, err := onto.Append(ctx, topics.LogSubject(to, kind, id), msgID, nil, body)
	if err != nil {
		return seq, duplicate, fmt.Errorf("append the misrouted copy: %w", err)
	}
	m.mu.Lock()
	m.seq = copied
	m.mu.Unlock()
	return seq, duplicate, nil
}

// landed is where the last misrouted copy landed on the other log, or 0.
func (m *misroutingLog) landed() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seq
}
