package codetest

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/objstore/references"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// deletedRetention is code.deleted_retention_days every topology runs at:
// seven days, the committer's and the host's alike.
const deletedRetention = 7 * 24 * time.Hour

// fleetRing is the keyring every data node signs its records and opens its
// peers' under: ONE FLEET, ONE KEYRING, as every state log's records are
// signed (ADR-0031) — so a record one node appended verifies on every other,
// and a fleet's nodes share this ring whoever started them. A fixture: what
// the suite is about is what the code domain writes, and the signature's own
// rules are certified in internal/statelog.
var fleetRing = statelog.OneKey("codetest", "the codetest fleet's key material")

// DataOptions configure a data node.
type DataOptions struct {
	// ID is the node's id: its consumer's name on the code log, the writer
	// its records name, and the key its eviction is read by.
	ID string
	// Queue is the node's broker client — its own embedded broker
	// (SoloNode) or a client of its cluster member (a Cluster). The caller
	// owns it: the node's Kill stops nothing of it.
	Queue *js.Queue
	// Dir is the store's directory; Restart reopens what is here.
	Dir string
	// Limits are code.limits; a zero field takes the default.
	Limits code.Limits
	// Faults is the fleet's script, shared by every node of it; nil is a
	// fresh one.
	Faults *codefault.Script
	// LogBytes is the code log's ceiling, the one field of its stream
	// overridden: an embedded broker in a temporary directory refuses to
	// reserve the shipped one.
	LogBytes int64
	// Operator is the id an operator's gestures are published under on this
	// node's log besides its own — a second writer, as a peer would be —
	// or "" for none (Operator then answers nil).
	Operator string
	// WrapApplier wraps the domain's applier, nil for none: the window a
	// suite's meta-test has to build a node whose applier LIES, which a case
	// must then fail on (TestTheSuiteCatchesARetirementWithNoGrace).
	WrapApplier func(statelog.Applier) statelog.Applier
	// SeatRules is the chart and the clock the node's committer judges a
	// seat's push by — an engine write's among them — and its change writer
	// a seat's write of a change's conversation and every reviewer an ask
	// names; nil is the fixture chart on UTC (FixtureSeatRules).
	SeatRules func() (code.SeatRules, bool)
	// Current is whether the node serves the company's configuration, as
	// its committer reads it before it decides a seat's push
	// (code.CommitterOptions.Current) and its change writer before it
	// decides a write the chart judges (code.ChangeWriterOptions.Current) —
	// false is a node confirmed behind it, which decides neither; nil is
	// current.
	Current func() bool
}

// DataNode is one data node's code domain, composed as the engine composes it
// for one domain (internal/engine's statelog.go): its store, its applier over
// its OWN durable consumer of the code log, its write authority — handed to
// the committer and the change writer beside it wrapped by the fault publisher
// that hits codefault.CommitterAfterAppend, so a declaration or an eviction
// never fires the committer's point — and its read authority with a REAL
// barrier, so `linearizable` is the read a node serves; and over them the
// domain's reader, committer, change writer, lifecycle store and retention
// pass.
//
// ITS LIFETIME IS THE TEST THAT STARTED IT, whatever restarts it: a topology
// that outlives the case restarting one of its nodes must not lose the node
// when that case ends, so every context a node runs under is its starter's,
// and the t a Restart is handed only reports.
type DataNode struct {
	o    DataOptions
	life context.Context

	// appends counts the records this node's committer appended.
	appends atomic.Int64

	mu   sync.Mutex
	cur  *dataIncarnation
	dead []*dataIncarnation
}

// dataIncarnation is one incarnation of a data node: everything a crash ends.
type dataIncarnation struct {
	q        *js.Queue
	log      *js.DomainLog
	consumer *js.DomainConsumer
	db       *store.DB
	estate   store.ReplicatedHandle

	runner    *statelog.Runner
	applyMu   sync.Mutex
	applyStop func() error

	reader    *code.Reader
	committer *code.Committer
	// changes is the change writer beside the committer, over the same
	// fault publisher: a conversation's record appended and its writer dead
	// before it answered is the committer's point as much as a push's.
	changes *code.ChangeWriter
	// checks is the checks writer beside the committer, over the same fault
	// publisher, for the change writer's reason.
	checks *code.ChecksWriter
	// marks is the writer of persons' code inbox marks, as the engine builds
	// it beside the committer.
	marks     *code.Marks
	lifecycle *code.Store
	operator  *code.Store

	// stopping is the committer's stop a Kill began, which waits for every
	// batch in flight — a crash's among them — and so is waited for only in
	// the teardown, after the crashes are released.
	stopping chan struct{}
}

// StartDataNode starts a data node over o.Queue. It stops with t.
func StartDataNode(t testing.TB, o DataOptions) *DataNode {
	t.Helper()
	switch {
	case o.ID == "" || o.Queue == nil || o.Dir == "":
		t.Fatalf("codetest: a data node needs an id, a queue and a directory (%+v)", o)
	case o.LogBytes <= 0:
		t.Fatalf("codetest: data node %s needs the code log's ceiling", o.ID)
	}
	if o.Faults == nil {
		o.Faults = &codefault.Script{}
	}
	o.Limits = withDefaults(o.Limits)
	d := &DataNode{o: o, life: t.Context()}
	// THE CRASHES ARE RELEASED BEFORE ANYTHING STOPS: whatever waits for
	// held work — the committer's Stop — waits until then
	// (internal/code/codefault).
	t.Cleanup(d.teardown)
	d.cur = d.start(t, o.Queue)
	return d
}

// withDefaults is l with every zero field at code.limits' default.
func withDefaults(l code.Limits) code.Limits {
	defaults := code.DefaultLimits()
	if l.MaxPushBytes == 0 {
		l.MaxPushBytes = defaults.MaxPushBytes
	}
	if l.MaxBlobBytes == 0 {
		l.MaxBlobBytes = defaults.MaxBlobBytes
	}
	if l.RepoMaxBytes == 0 {
		l.RepoMaxBytes = defaults.RepoMaxBytes
	}
	if l.PushesPerSeatDay == 0 {
		l.PushesPerSeatDay = defaults.PushesPerSeatDay
	}
	if l.MergeByFloor == "" {
		l.MergeByFloor = defaults.MergeByFloor
	}
	return l
}

// start builds one incarnation over the node's files: the code log
// provisioned (a lookup where it is), the estate, the applier, the write and
// read authorities, the committer and the lifecycle store — the engine's
// order.
func (d *DataNode) start(t testing.TB, q *js.Queue) *dataIncarnation {
	t.Helper()
	ctx := d.life
	p := &dataIncarnation{q: q}
	spec := code.Domain{}.Stream()
	spec.MaxBytes = d.o.LogBytes
	// THE DOMAIN'S LARGEST RECORD AS THE BROKER COUNTS A MESSAGE, as the
	// engine provisions every state log.
	if err := q.EnsureDomainStream(ctx, js.DomainStream{Name: spec.Name, Subjects: spec.Subjects,
		MaxBytes: spec.MaxBytes, MaxMessageBytes: spec.MaxAppendBytes(),
		Duplicates: spec.Duplicates}); err != nil {
		t.Fatalf("codetest: %s: provision the code log: %v", d.o.ID, err)
	}
	var err error
	if p.log, err = q.DomainLog(ctx, spec.Name); err != nil {
		t.Fatalf("codetest: %s: open the code log: %v", d.o.ID, err)
	}
	stats, err := p.log.Stats(ctx)
	if err != nil {
		t.Fatalf("codetest: %s: read the code log: %v", d.o.ID, err)
	}
	// THE ESTATE, seeded where none exists and opened as it is where one
	// does — which is what a restarted process does.
	p.db, p.estate = storetest.OpenEstate(t, filepath.Join(d.o.Dir, "node.db"), store.Options{}, 1)
	checkpoint, _, err := statelog.CheckpointOf(ctx, p.estate.Reader(), spec.Name)
	if err != nil {
		t.Fatalf("codetest: %s: read the checkpoint: %v", d.o.ID, err)
	}
	if p.consumer, err = q.DomainConsumer(ctx, spec.Name, d.o.ID, checkpoint.At.Seq); err != nil {
		t.Fatalf("codetest: %s: open the consumer: %v", d.o.ID, err)
	}
	signer, err := statelog.NewSigner(code.Domain{}.Name(), fleetRing)
	if err != nil {
		t.Fatalf("codetest: %s: build the record signer: %v", d.o.ID, err)
	}
	verifier, err := statelog.NewVerifier(code.Domain{}.Name(), fleetRing)
	if err != nil {
		t.Fatalf("codetest: %s: build the record verifier: %v", d.o.ID, err)
	}
	var applier statelog.Applier = code.NewApplier(nil)
	if d.o.WrapApplier != nil {
		applier = d.o.WrapApplier(applier)
	}
	if p.runner, err = statelog.NewRunner(statelog.RunnerDeps{
		Domain: code.Domain{}, Spec: spec, Applier: applier, Fetch: p.consumer,
		Verifier: verifier, Log: p.log, Node: p.db, DB: p.estate,
		Checkpoint: checkpoint.At, CheckpointStoredAt: checkpoint.StoredAt,
		StreamCreatedAt: stats.CreatedAt.UTC(), NodeID: d.o.ID,
		// WHOM THE FLEET HAS EVICTED, from this node's own rows — the
		// fence's reading of any writer's row, this node's included.
		Evicted: func(ctx context.Context, node string) (bool, error) {
			return code.NewFence(p.estate.Reader(), node).Evicted(ctx)
		},
	}); err != nil {
		t.Fatalf("codetest: %s: build the applier: %v", d.o.ID, err)
	}
	p.startApplier()

	reserve, err := statelog.NewReserve(spec, func(ctx context.Context) (statelog.Usage, error) {
		s, serr := p.log.Stats(ctx)
		return statelog.Usage{Bytes: s.Bytes, MaxBytes: s.MaxBytes}, serr
	})
	if err != nil {
		t.Fatalf("codetest: %s: build the gate reserve: %v", d.o.ID, err)
	}
	rows, err := code.NewRows(p.estate.Reader(), spec)
	if err != nil {
		t.Fatalf("codetest: %s: build the read seam: %v", d.o.ID, err)
	}
	gen := func() uint32 { return p.runner.Committed().Generation }
	publisher := func(writer string) *statelog.Publisher {
		fence := code.NewFence(p.estate.Reader(), writer)
		// A FLEET THAT HAS NEVER TRIMMED: a test node runs no trim, so the
		// published floor is zero and the log's own ends are the fence's.
		fence.Floor = func(context.Context, uint32) (uint64, error) { return 0, nil }
		fence.Ends = func(ctx context.Context) (statelog.LogEnds, error) {
			first, last, berr := p.log.Bounds(ctx)
			return statelog.LogEnds{First: first, Last: last}, berr
		}
		fence.Committed = p.runner.Committed
		pub, perr := statelog.NewPublisher(statelog.Deps{
			Domain: code.Domain{}, Spec: spec, Log: p.log, Signer: signer,
			Records: statelog.VerifiedLog{Log: p.log, Verifier: verifier}, Rows: rows, Fence: fence,
			Gates: code.NewGates(p.estate.Reader()), Waiter: p.runner, Voids: p.runner, Identity: p.runner,
			Admission: reserve, NodeID: writer, Generation: gen,
		})
		if perr != nil {
			t.Fatalf("codetest: %s: build %s's write authority: %v", d.o.ID, writer, perr)
		}
		return pub
	}
	pub := publisher(d.o.ID)
	index, err := statelog.NewReadIndex(code.Domain{}, spec, p.log, p.log.Members, signer, reserve,
		code.EncodeBarrier, gen, nil)
	if err != nil {
		t.Fatalf("codetest: %s: build the read index: %v", d.o.ID, err)
	}
	authority, err := statelog.NewReader(statelog.ReaderDeps{
		Domain: code.Domain{}, Spec: spec, Mode: statelog.ModeNormal, DB: p.estate.Reader(),
		Index: index, Waiter: p.runner, Drain: p.runner.Drain,
		Health: func() (statelog.Health, error) { return p.health(ctx) },
	})
	if err != nil {
		t.Fatalf("codetest: %s: build the read authority: %v", d.o.ID, err)
	}
	limits := func() code.Limits { return d.o.Limits }
	seatRules := d.o.SeatRules
	if seatRules == nil {
		seatRules = FixtureSeatRules
	}
	// THE READER JUDGES BY THE CHART ITS WRITERS DECIDE BY, as a node reads
	// and writes by one epoch's: a seat's grant, and every change's readiness
	// whoever reads it — a reader holding none refuses a read of changes.
	roster := func() code.Roster {
		rules, held := seatRules()
		if !held {
			return nil
		}
		return rules.Roster
	}
	if p.reader, err = code.NewReader(code.ReaderOptions{Log: authority, Limits: limits,
		Roster: roster}); err != nil {
		t.Fatalf("codetest: %s: build the reader: %v", d.o.ID, err)
	}
	if p.committer, err = code.NewCommitter(code.CommitterOptions{
		Publisher: &faultPublisher{inner: pub, faults: d.o.Faults, appends: &d.appends},
		DB:        p.estate.Reader(), NodeID: d.o.ID, Limits: limits,
		Retention: func() time.Duration { return deletedRetention },
		// THE CHART A SEAT'S PUSH IS DECIDED BY — an engine write's, a seat's
		// own commit landed as a push — the one the git nodes judge it by
		// before they build it; and the node's posture, which decides no
		// seat's push while it is behind the company's configuration.
		SeatRules: seatRules, Current: d.o.Current,
	}); err != nil {
		t.Fatalf("codetest: %s: build the committer: %v", d.o.ID, err)
	}
	if p.changes, err = code.NewChangeWriter(code.ChangeWriterOptions{
		Publisher: &faultPublisher{inner: pub, faults: d.o.Faults, appends: &d.appends},
		DB:        p.estate.Reader(), NodeID: d.o.ID,
		Retention: func() time.Duration { return deletedRetention },
		// THE COMMITTER'S CHART AND POSTURE, as the engine hands the two the
		// same ones (codeSeatRules, e.admits): a seat's write of a change's
		// conversation, and every reviewer an ask names, is judged by the
		// chart a seat's push is, and by no node confirmed behind the
		// company's configuration, so a node of the suite decides what a node
		// of the fleet does.
		SeatRules: seatRules, Current: d.o.Current,
	}); err != nil {
		t.Fatalf("codetest: %s: build the change writer: %v", d.o.ID, err)
	}
	// THE CHECKS WRITER, as the engine builds it beside the committer, with no
	// event stream: the suite reads what a check's record did from its rows.
	if p.checks, err = code.NewChecksWriter(code.ChecksWriterOptions{
		Publisher: &faultPublisher{inner: pub, faults: d.o.Faults, appends: &d.appends},
		DB:        p.estate.Reader(), NodeID: d.o.ID,
		Retention: func() time.Duration { return deletedRetention },
	}); err != nil {
		t.Fatalf("codetest: %s: build the checks writer: %v", d.o.ID, err)
	}
	if p.marks, err = code.NewMarks(code.MarksOptions{
		Publisher: &faultPublisher{inner: pub, faults: d.o.Faults, appends: &d.appends},
		DB:        p.estate.Reader(), NodeID: d.o.ID,
	}); err != nil {
		t.Fatalf("codetest: %s: build the marks writer: %v", d.o.ID, err)
	}
	if p.lifecycle, err = code.NewStore(code.StoreOptions{Publisher: pub, DB: p.estate.Reader(),
		NodeID: d.o.ID}); err != nil {
		t.Fatalf("codetest: %s: build the lifecycle store: %v", d.o.ID, err)
	}
	if d.o.Operator != "" {
		if p.operator, err = code.NewStore(code.StoreOptions{Publisher: publisher(d.o.Operator),
			DB: p.estate.Reader(), NodeID: d.o.Operator}); err != nil {
			t.Fatalf("codetest: %s: build the operator's lifecycle store: %v", d.o.ID, err)
		}
	}
	return p
}

// ID is the node's id.
func (d *DataNode) ID() string { return d.o.ID }

// proc is the running incarnation, nil while the node is killed.
func (d *DataNode) proc() *dataIncarnation {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cur
}

// Served is the node's committer and reader as the estate serves them
// (code.Served) — its zero value while the node is killed, which runs nothing
// and refuses every operation code.ErrNotServing.
func (d *DataNode) Served() code.Served {
	p := d.proc()
	if p == nil {
		return code.Served{}
	}
	return code.Served{Committer: p.committer, ChangeWriter: p.changes, ChecksWriter: p.checks, Reader: p.reader,
		Marks: p.marks}
}

// Store is the node's lifecycle store, publishing under its own id.
func (d *DataNode) Store() *code.Store {
	if p := d.proc(); p != nil {
		return p.lifecycle
	}
	return nil
}

// Operator is the lifecycle store publishing under DataOptions.Operator — nil
// where the node was given none, or while it is killed.
func (d *DataNode) Operator() *code.Store {
	if p := d.proc(); p != nil {
		return p.operator
	}
	return nil
}

// Declare activates the code log at at — a fleet's activations are monotone,
// so EnsureRepo never meets an older one — and ensures each of repos, through
// this node's lifecycle store.
func (d *DataNode) Declare(t testing.TB, at time.Time, repos ...code.Declared) {
	t.Helper()
	s := d.Store()
	if s == nil {
		t.Fatalf("codetest: declare through data node %s, which is not running", d.o.ID)
	}
	a, err := s.Activate(t.Context(), at)
	if err != nil {
		t.Fatalf("codetest: activate through %s: %v", d.o.ID, err)
	}
	for _, r := range repos {
		if _, err := s.EnsureRepo(t.Context(), a, r); err != nil {
			t.Fatalf("codetest: declare %s through %s: %v", r.Name, d.o.ID, err)
		}
	}
}

// Remove deletes the repository called name at the activation at, through this
// node.
func (d *DataNode) Remove(t testing.TB, at time.Time, name string) {
	t.Helper()
	served, s := d.Served(), d.Store()
	if s == nil {
		t.Fatalf("codetest: remove through data node %s, which is not running", d.o.ID)
	}
	ref, err := served.Resolve(t.Context(), code.ResolveQuery{Name: name, Fresh: Linearizable})
	if err != nil {
		t.Fatalf("codetest: resolve %s through %s: %v", name, d.o.ID, err)
	}
	a, err := s.Activate(t.Context(), at)
	if err != nil {
		t.Fatalf("codetest: activate through %s: %v", d.o.ID, err)
	}
	if _, err := s.DeleteRepo(t.Context(), a, ref.ID); err != nil {
		t.Fatalf("codetest: remove %s through %s: %v", name, d.o.ID, err)
	}
}

// Purge runs code-retention passes through this node with the shortest
// retention there is — the domain refuses a zero one — until every purge they
// began is done.
func (d *DataNode) Purge(t testing.TB) {
	t.Helper()
	p := d.proc()
	if p == nil {
		t.Fatalf("codetest: purge through data node %s, which is not running", d.o.ID)
	}
	r := &code.Retention{Store: p.lifecycle, Reader: p.reader,
		Retention: func() time.Duration { return time.Nanosecond },
		Policy:    scratchPolicy, Holds: holdsNone, Propose: p.committer.Propose, Node: d.o.ID}
	for range 64 {
		report, err := r.Pass(t.Context())
		if err != nil {
			t.Fatalf("codetest: a retention pass through %s: %v", d.o.ID, err)
		}
		if !report.Remain && len(report.Unhanded) == 0 && len(report.Failed) == 0 {
			return
		}
	}
	t.Fatalf("codetest: sixty-four retention passes through %s left a purge unfinished", d.o.ID)
}

// scratchPolicy is every topology's code.scratch: the defaults a company that
// sets none runs under.
func scratchPolicy() code.LifecyclePolicy { return code.ScratchOf(config.Code{}) }

// holdsNone is the live turns' holds in a topology that runs no turn: none, so
// a pass expires every scratch repository past its idle time.
func holdsNone(context.Context, []string) (map[string]bool, error) { return map[string]bool{}, nil }

// Retain runs ONE code-retention pass through this node with its clock at now
// — the instant it judges a retired pack's grace and a deletion's age by — at
// the retention every topology runs at: a case moves a retirement's grace by
// the pass's clock, since the broker's instant on the retirement cannot be
// moved.
func (d *DataNode) Retain(t testing.TB, now time.Time) code.RetentionReport {
	t.Helper()
	p := d.proc()
	if p == nil {
		t.Fatalf("codetest: a retention pass through data node %s, which is not running", d.o.ID)
	}
	r := &code.Retention{Store: p.lifecycle, Reader: p.reader,
		Retention: func() time.Duration { return deletedRetention }, Now: func() time.Time { return now },
		Policy: scratchPolicy, Holds: holdsNone, Propose: p.committer.Propose, Node: d.o.ID}
	report, err := r.Pass(t.Context())
	if err != nil {
		t.Fatalf("codetest: a retention pass through %s: %v", d.o.ID, err)
	}
	return report
}

// Collect runs one collection of objects over the code estate as this node
// holds it, its clock a day and an hour on, so every object past its grace is
// judged.
func (d *DataNode) Collect(t testing.TB, objects objstore.Backend) collect.CollectionReport {
	t.Helper()
	return d.CollectAt(t, objects, time.Now().Add(25*time.Hour))
}

// codeTables is every table the code domain declares in the one list of
// tables that name objects (internal/objstore/references), whatever its
// standing: what a collection over the code estate is held to — a live pack's
// row, a retired one's, a published layer's part's — as the engine's collector
// is, every declared table shielding what it names.
var codeTables = func() []objstore.ReferenceTable {
	var out []objstore.ReferenceTable
	for _, table := range references.All {
		if table.Domain == (code.Domain{}).Name() {
			out = append(out, table)
		}
	}
	return out
}()

// CollectAt runs one collection of objects over the code estate as this node
// holds it, with the collector's clock at at — every table the code domain
// declares shielding what its rows name, as the engine's collector is built
// over every declared table.
func (d *DataNode) CollectAt(t testing.TB, objects objstore.Backend, at time.Time) collect.CollectionReport {
	t.Helper()
	report, err := d.collector(t, objects, at).Collect(t.Context())
	if err != nil {
		t.Fatalf("codetest: a collection: %v", err)
	}
	return report
}

// Audit runs the collector's audit of objects over the code estate as this
// node holds it — what raises objects_missing — every table the code domain
// declares handed to it, as the engine's collector is built, so a table the
// audit must leave alone is one it is handed and leaves.
func (d *DataNode) Audit(t testing.TB, objects objstore.Backend) collect.AuditReport {
	t.Helper()
	report, err := d.collector(t, objects, time.Now()).Audit(t.Context())
	if err != nil {
		t.Fatalf("codetest: an audit: %v", err)
	}
	return report
}

// collector is a collector of objects over the code estate as this node holds
// it, its clock at at.
func (d *DataNode) collector(t testing.TB, objects objstore.Backend, at time.Time) *collect.Collector {
	t.Helper()
	store, err := objstore.NewStore(objects)
	if err != nil {
		t.Fatalf("codetest: the object store: %v", err)
	}
	sources, err := collect.Sources(codeTables, code.ObjectEstate{Reader: d.Served().Reader})
	if err != nil {
		t.Fatalf("codetest: the collector's sources: %v", err)
	}
	c, err := collect.New(collect.Options{Store: store, References: sources,
		Now: func() time.Time { return at }})
	if err != nil {
		t.Fatalf("codetest: a collector: %v", err)
	}
	return c
}

// Evict publishes an operator's eviction of node through this node's log —
// under DataOptions.Operator where it has one, its own id otherwise — and
// answers where it landed, applied here.
//
// AN OPERATOR'S GESTURE COMES THROUGH ANOTHER WRITER: every record an evicted
// node publishes is dropped, its own readmission included, so a fleet evicts
// and readmits a node through a peer — and a node with no peer, through a
// writer of its own (DataOptions.Operator).
func (d *DataNode) Evict(t testing.TB, node string) statelog.Position {
	t.Helper()
	return d.gesture(t, node, (*code.Store).EvictNode, "evict")
}

// Readmit publishes an operator's readmission of node, as Evict does.
func (d *DataNode) Readmit(t testing.TB, node string) statelog.Position {
	t.Helper()
	return d.gesture(t, node, (*code.Store).ReadmitNode, "readmit")
}

// gesture publishes an operator's eviction or readmission of node as operator
// `ops`.
func (d *DataNode) gesture(t testing.TB, node string,
	op func(*code.Store, context.Context, code.Actor, string, string) (statelog.Result, error),
	name string) statelog.Position {

	t.Helper()
	s := d.Operator()
	if s == nil {
		s = d.Store()
	}
	if s == nil {
		t.Fatalf("codetest: %s %s through data node %s, which is not running", name, node, d.o.ID)
	}
	actor := code.Actor{ID: "ops", Kind: code.ActorOperator, OperatorID: "ops"}
	res, err := op(s, t.Context(), actor, statelog.NewOpID(time.Now(), name), node)
	if err != nil || res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("codetest: %s %s through %s = (%+v, %v), want applied", name, node, d.o.ID, res, err)
	}
	return res.Position
}

// WaitApplied waits until this node's applier has reached at
// ([statelog.Runner.WaitReached]).
func (d *DataNode) WaitApplied(ctx context.Context, at statelog.Position) error {
	p := d.proc()
	if p == nil {
		return fmt.Errorf("codetest: data node %s is not running", d.o.ID)
	}
	return p.runner.WaitReached(ctx, at)
}

// Level waits until this node's rows hold every record the code log held when
// it was asked: its applier has reached the log's end
// ([statelog.Runner.Reached]), as a reader of them waits for it to.
func (d *DataNode) Level(ctx context.Context) error {
	p := d.proc()
	if p == nil {
		return fmt.Errorf("codetest: data node %s is not running", d.o.ID)
	}
	_, last, err := p.log.Bounds(ctx)
	if err != nil {
		return fmt.Errorf("codetest: %s: the code log's end: %w", d.o.ID, err)
	}
	for p.runner.Reached().Seq < last {
		select {
		case <-ctx.Done():
			return fmt.Errorf("codetest: %s reached %d of the log's %d: %w", d.o.ID,
				p.runner.Reached().Seq, last, ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	return nil
}

// Drained is the applier's own: whether it has reached the log's end once
// since it started.
func (d *DataNode) Drained() bool {
	p := d.proc()
	return p != nil && p.runner.Drained()
}

// Health is the node's readiness as the engine assembles it.
func (d *DataNode) Health(ctx context.Context) (statelog.Health, error) {
	p := d.proc()
	if p == nil {
		return statelog.Health{}, fmt.Errorf("codetest: data node %s is not running", d.o.ID)
	}
	return p.health(ctx)
}

// Appends is how many records this node's COMMITTER appended — never a
// declaration's or an eviction's, which go through the lifecycle store.
func (d *DataNode) Appends() int { return int(d.appends.Load()) }

// LogStats is the code log's stream as the broker holds it — its ends, its
// records and its BYTES, every record's signed frame included: what a
// benchmark reads the bytes a kind of record costs the log off, before and
// after it appends one.
func (d *DataNode) LogStats(ctx context.Context) (js.LogStats, error) {
	p := d.proc()
	if p == nil {
		return js.LogStats{}, fmt.Errorf("codetest: the code log's stats through data node %s, which is not "+
			"running", d.o.ID)
	}
	return p.log.Stats(ctx)
}

// PauseApplier halts the node's applier until resume, which moves its
// consumer back to the checkpoint and runs it again — the engine's halt and
// resume. t reports a resume that could not move the consumer.
func (d *DataNode) PauseApplier(t testing.TB) (resume func()) {
	t.Helper()
	p := d.proc()
	if p == nil {
		t.Fatalf("codetest: PauseApplier of data node %s, which is not running", d.o.ID)
		return func() {}
	}
	p.stopApplier()
	return func() {
		ctx := d.life
		at, _, _, err := statelog.CursorFor(ctx, p.estate.Reader(), code.Domain{}.Stream().Name)
		if err == nil {
			err = p.consumer.Reset(ctx, at.Seq)
		}
		if err != nil {
			t.Errorf("codetest: %s: move the consumer back to the checkpoint: %v", d.o.ID, err)
		}
		p.startApplier()
	}
}

// Kill ends the node as a process death does, as far as the store sees it:
// the committer's stop begun under an ended context — its batches cancelled,
// and WAITED FOR ONLY IN THE TEARDOWN, because a crash holds one past the
// node's death (internal/code/codefault) — the applier halted, the consumer
// and the store closed. The queue is the caller's, and so is the order: a
// caller that must keep every answer the node was serving from reaching its
// asker stops the node's broker client first.
func (d *DataNode) Kill() {
	d.mu.Lock()
	p := d.cur
	d.cur = nil
	if p != nil {
		d.dead = append(d.dead, p)
		p.stopping = make(chan struct{})
	}
	d.mu.Unlock()
	if p == nil {
		return
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	go func() {
		defer close(p.stopping)
		p.committer.Stop(ended)
		p.changes.Stop(ended)
		p.checks.Stop(ended)
	}()
	p.stopApplier()
	p.consumer.Close()
	_ = p.db.Close()
}

// Restart rebuilds the node over the same files and q — a new client where
// its broker restarted. t reports; the node's lifetime stays its starter's.
func (d *DataNode) Restart(t testing.TB, q *js.Queue) {
	t.Helper()
	if d.proc() != nil {
		t.Fatalf("codetest: Restart of data node %s, which was not killed", d.o.ID)
	}
	p := d.start(t, q)
	d.mu.Lock()
	d.cur = p
	d.mu.Unlock()
}

// StopCommitter stops the committer the way a graceful stop does — the
// engine's teardown's first step, before the node stops answering — bounded
// by statelog.DefaultResolveBudget: every new proposal refused
// code.ErrNotServing, every queued one answered so, and the batches in flight
// finished.
func (d *DataNode) StopCommitter() {
	p := d.proc()
	if p == nil {
		return
	}
	bounded, cancel := context.WithTimeout(context.Background(), statelog.DefaultResolveBudget)
	defer cancel()
	p.committer.Stop(bounded)
	p.changes.Stop(bounded)
	p.checks.Stop(bounded)
}

// teardown ends every incarnation: the crashes released FIRST, before anything
// stops or waits.
func (d *DataNode) teardown() {
	d.o.Faults.ReleaseCrashes()
	d.mu.Lock()
	procs := slices.Clone(d.dead)
	if d.cur != nil {
		procs = append(procs, d.cur)
	}
	d.cur, d.dead = nil, nil
	d.mu.Unlock()
	for _, p := range procs {
		if p.stopping != nil {
			<-p.stopping
		} else {
			p.committer.Stop(context.Background())
			p.changes.Stop(context.Background())
			p.checks.Stop(context.Background())
		}
		p.stopApplier()
		p.consumer.Close()
		_ = p.db.Close()
	}
}

// startApplier runs the applier until stopApplier.
func (p *dataIncarnation) startApplier() {
	p.applyMu.Lock()
	defer p.applyMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- p.runner.Run(ctx) }()
	p.applyStop = sync.OnceValue(func() error {
		cancel()
		err := <-stopped
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	})
}

// stopApplier halts the applier and waits for it.
func (p *dataIncarnation) stopApplier() {
	p.applyMu.Lock()
	stop := p.applyStop
	p.applyMu.Unlock()
	if stop != nil {
		_ = stop()
	}
}

// health is the data node's readiness as the engine assembles it: the
// applier's position — the reached one, as the engine's read health takes it —
// whether it has drained, the log's two ends and its lag, the stream's
// identity, and a floor a fleet that never trimmed publishes.
func (p *dataIncarnation) health(ctx context.Context) (statelog.Health, error) {
	at := p.runner.Reached()
	h := statelog.Health{Position: at, AppliedThrough: at.Seq, Drained: p.runner.Drained(),
		Floor: statelog.Floor{State: statelog.FloorOK, ReadAt: time.Now()}}
	if err := p.runner.Stopped(); err != nil && !errors.Is(err, context.Canceled) {
		h.Err = err.Error()
	}
	if deferral, ok := p.runner.Deferred(); ok {
		h.Deferred, h.DeferredFrom = 1, deferral.Position.Seq
		if deferral.Position.Seq > 0 {
			h.AppliedThrough = deferral.Position.Seq - 1
		}
	}
	first, last, err := p.log.Bounds(ctx)
	if err != nil {
		return h, err
	}
	end := max(last, at.Seq)
	lag, trim := uint64(0), uint64(0)
	if end > at.Seq {
		lag = end - at.Seq
	}
	h.FirstSeq, h.LastSeq, h.Lag, h.TrimFloor = &first, &end, &lag, &trim
	identity := p.runner.StreamIdentity()
	h.StreamRecreated = errors.Is(identity, statelog.ErrStreamRecreated)
	h.GenerationPassed = errors.Is(identity, statelog.ErrGenerationPassed)
	h.LogDiverged = p.runner.Diverged()
	return h, nil
}

// faultPublisher is the committer's write authority with the push path's
// CommitterAfterAppend point between the append and its result: the durable
// record the receiver never heard about. Hit here rather than in
// internal/code, so the domain imports nothing from codefault. It counts the
// records it appended — [DataNode.Appends].
type faultPublisher struct {
	inner   code.Publisher
	faults  *codefault.Script
	appends *atomic.Int64
}

func (p *faultPublisher) Publish(ctx context.Context, req statelog.Request) (statelog.Result, error) {
	res, err := p.inner.Publish(ctx, req)
	if err == nil {
		if res.Wrote() && !res.Collapsed {
			p.appends.Add(1)
		}
		if ferr := p.faults.Hit(ctx, codefault.CommitterAfterAppend, 0); ferr != nil {
			return statelog.Result{}, ferr
		}
	}
	return res, err
}
