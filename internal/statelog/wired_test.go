package statelog_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// wiredLogBytes is the probe log's ceiling on a wired node: room for a
// benchmark run's tens of thousands of few-hundred-byte records and their
// barriers, which the domain's own 16 MiB is not.
const wiredLogBytes = 1 << 30

// wiredOpBudget bounds one write or read on a wired node, so a STOPPED applier
// fails the case rather than holding it to the binary's timeout. Thirty
// seconds: three [statelog.ElectionBarrierBudget]s and half [statelog.StallGrace];
// a healthy write takes milliseconds.
const wiredOpBudget = 30 * time.Second

// wiredNode is one data node's write path with every seam real: the
// publisher, its rows ([statelog.NewRows] over a migrated estate), the write
// fence ([wiredFence]), the read index, and the runner applying from the
// broker's own consumer. ONE FAKE IS LEFT: [fakeGates], asked only when the
// publisher resolves an ambiguous append.
type wiredNode struct {
	pub    *statelog.Publisher
	runner *statelog.Runner
	index  *statelog.ReadIndex

	// log is the node's probe log; a benchmark reads its end to count the
	// barriers its reads appended.
	log *js.DomainLog

	// estate is the node's replicated estate, where a case writes an
	// eviction row for the fence to read.
	estate store.ReplicatedHandle

	// spec is the probe log as this node runs it.
	spec statelog.StreamSpec

	// start runs the applier until the case ends. It is apart from building
	// because the drain benchmark builds the node outside its timer and
	// starts the applier inside it.
	start func()

	// stop ends the applier, then closes its consumer before the broker
	// stops (a pull given up on a closed connection is logged as a failure),
	// and answers how the applier stopped if not by being asked. It runs
	// once however often called: by the case's cleanup, and by the drain
	// benchmark between its bursts.
	stop func() error
}

// soloBroker is an embedded broker of one member, its streams on disk: every
// property the framework rests on is about a stream that survives.
func soloBroker(tb testing.TB, cfg js.Config) *js.Queue {
	tb.Helper()
	cfg.StoreDir = brokertest.TempDir(tb)
	if cfg.StoreMaxBytes == 0 {
		cfg.StoreMaxBytes = brokertest.StoreMaxBytes
	}
	q, err := js.Open(tb.Context(), cfg)
	if err != nil {
		tb.Fatalf("open a broker: %v", err)
	}
	tb.Cleanup(func() {
		if err := q.Stop(context.WithoutCancel(tb.Context())); err != nil {
			tb.Errorf("stop the broker: %v", err)
		}
	})
	return q
}

// ensureWiredLog provisions the probe log at [wiredLogBytes].
func ensureWiredLog(tb testing.TB, q *js.Queue) statelog.StreamSpec {
	tb.Helper()
	spec := specOf(probeDomain{})
	spec.MaxBytes = wiredLogBytes
	if err := q.EnsureDomainStream(tb.Context(), js.DomainStream{
		Name: spec.Name, Subjects: spec.Subjects, MaxBytes: spec.MaxBytes,
		Duplicates: spec.Duplicates,
	}); err != nil {
		tb.Fatalf("provision the log: %v", err)
	}
	return spec
}

// newWiredNode builds the node nodeID on q's broker, its applier not yet
// running ([running] starts it).
func newWiredNode(tb testing.TB, q *js.Queue, nodeID string) *wiredNode {
	tb.Helper()
	return newWiredNodeWith(tb, q, nodeID, store.Options{})
}

// newWiredNodeWith is [newWiredNode] over a store opened with opts — a driver a
// case wraps to hold or fail the node's commits.
func newWiredNodeWith(tb testing.TB, q *js.Queue, nodeID string, opts store.Options) *wiredNode {
	tb.Helper()
	spec := ensureWiredLog(tb, q)
	log, err := q.DomainLog(tb.Context(), spec.Name)
	if err != nil {
		tb.Fatalf("open the log: %v", err)
	}
	stats, err := log.Stats(tb.Context())
	if err != nil {
		tb.Fatalf("read the log: %v", err)
	}
	consumer, err := q.DomainConsumer(tb.Context(), spec.Name, nodeID, 0)
	if err != nil {
		tb.Fatalf("open the consumer: %v", err)
	}
	tb.Cleanup(consumer.Close)
	db, estate := storetest.OpenEstate(tb, filepath.Join(tb.TempDir(), "node.db"), opts, 1)
	tb.Cleanup(func() {
		if err := db.Close(); err != nil {
			tb.Errorf("close the store: %v", err)
		}
	})
	if err := estate.Tx(tb.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(tb.Context(), probeDDL)
		return err
	}); err != nil {
		tb.Fatalf("create the probe domain's tables: %v", err)
	}
	// SIGNED AND VERIFIED under the package's shared keyring, as every data
	// node's records are.
	signer, err := recordseal.NewSigner(probeDomain{}.Name(), testRing())
	if err != nil {
		tb.Fatalf("NewSigner: %v", err)
	}
	verifier, err := recordseal.NewVerifier(probeDomain{}.Name(), testRing())
	if err != nil {
		tb.Fatalf("NewVerifier: %v", err)
	}
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain: probeDomain{}, Spec: spec, Verifier: verifier,
		Applier: newProbeApplier(), Fetch: consumer,
		Log: log, Node: db, DB: estate,
		Checkpoint:      statelog.Position{Stream: spec.Name, Generation: 1},
		StreamCreatedAt: stats.CreatedAt.UTC(), NodeID: nodeID,
	})
	if err != nil {
		tb.Fatalf("NewRunner: %v", err)
	}
	rows, err := statelog.NewRows(estate.Reader(), probeDomain{}, spec, nil)
	if err != nil {
		tb.Fatalf("NewRows: %v", err)
	}
	fence := &wiredFence{db: estate.Reader(), nodeID: nodeID, stream: spec.Name}
	fence.zero = statelog.ZeroFence{
		Evicted: fence.Evicted,
		Floor:   func(context.Context, uint32) (uint64, error) { return 0, nil },
		Ends: func(ctx context.Context) (statelog.LogEnds, error) {
			first, last, err := log.Bounds(ctx)
			return statelog.LogEnds{First: first, Last: last}, err
		},
		Committed: runner.Committed,
	}
	reserve := reserveOn(tb, spec, log)
	gen := func() uint32 { return runner.Committed().Generation }
	pub, err := statelog.NewPublisher(statelog.Deps{
		Domain: probeDomain{}, Spec: spec,
		Log: log, Signer: signer, Rows: rows, Fence: fence, Gates: &fakeGates{},
		Records: statelog.VerifiedLog{Log: log, Verifier: verifier},
		Waiter:  runner, Voids: runner, Identity: runner,
		Admission: reserve, NodeID: nodeID, Generation: gen,
	})
	if err != nil {
		tb.Fatalf("NewPublisher: %v", err)
	}
	index, err := statelog.NewReadIndex(probeDomain{}, spec, log, log.Members, signer, reserve, probeEncode, gen, nil)
	if err != nil {
		tb.Fatalf("NewReadIndex: %v", err)
	}
	n := &wiredNode{pub: pub, runner: runner, index: index, log: log, estate: estate, spec: spec}
	n.start = func() {
		// THE CASE'S CONTEXT'S VALUES, NOT ITS END: [wiredNode.stop] stops
		// the loop and says how it stopped; the case's context would cancel
		// it before any cleanup ran and hide a stop for any other reason.
		ctx, cancel := context.WithCancel(context.WithoutCancel(tb.Context()))
		stopped := make(chan error, 1)
		go func() { stopped <- runner.Run(ctx) }()
		n.stop = sync.OnceValue(func() error {
			cancel()
			err := <-stopped
			consumer.Close()
			if err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		})
		tb.Cleanup(func() {
			if err := n.stop(); err != nil {
				tb.Errorf("the applier stopped with %v", err)
			}
		})
	}
	return n
}

// running starts n's applier and answers n.
func running(n *wiredNode) *wiredNode {
	n.start()
	return n
}

// wiredFence is the probe domain's write fence, built as a real domain's is
// (internal/worklet's Fence): ONE INDEXED SINGLE-ROW READ of this node's own
// eviction row on every append, and the real [statelog.ZeroFence] over that
// read, the log's live ends and the applier's checkpoint.
//
// Only its trim floor is fake (production reads coordination), and only a
// subject's FIRST write, at an expectation of zero, asks for it; later writes
// arbitrate against the anchor the previous one left.
type wiredFence struct {
	db     store.ReplicatedReader
	nodeID string
	stream string
	zero   statelog.ZeroFence
}

// Evicted reads this node's own eviction row: no row is not evicted, and a
// failed read is an error rather than "not evicted", because publishing on a
// guess lets an evicted node collect acknowledgements for records every peer
// drops.
func (f *wiredFence) Evicted(ctx context.Context) (bool, error) {
	var from, readmitted sql.NullInt64
	err := f.db.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT from_position, readmitted_position FROM probe_evictions
			WHERE node_id = ? AND log_stream = ?`,
			f.nodeID, f.stream).Scan(&from, &readmitted)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read this node's own eviction row: %w", err)
	}
	return !readmitted.Valid || readmitted.Int64 <= from.Int64, nil
}

func (f *wiredFence) ClearForZero(ctx context.Context, cursor statelog.Position) error {
	return f.zero.ClearForZero(ctx, cursor)
}

// write is one arbitrated probe write onto subject, end to end: the fence, the
// wait for this node to reach session (the caller's last write, as a seat's
// turn carries it, or zero), the publisher's snapshot and decision, the
// arbitrated append, and this node's apply. Anything but applied is an error.
func (n *wiredNode) write(ctx context.Context, subject statelog.Subject, session statelog.Position) (statelog.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, wiredOpBudget)
	defer cancel()
	opID := statelog.NewOpID(time.Now(), "wired")
	scope := statelog.ScopeSet{Paths: []string{subject.String()}}
	res, err := n.pub.Publish(ctx, statelog.Request{
		Subject: subject, Scope: scope, OpID: opID, Session: session,
		Pattern: statelog.PatternArbitrated,
		Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			return statelog.Decision{Payload: probeRecord(stamp, subject, scope, opID, "wired"), Version: 1}, nil
		},
	})
	if err == nil && res.Outcome != statelog.OutcomeApplied {
		err = fmt.Errorf("a write on %s answered %s at %s, want applied", subject, res.Outcome, res.Position)
	}
	return res, err
}

// barrier is the read index alone: the barrier a linearizable read appends,
// at the position the broker acknowledged it.
func (n *wiredNode) barrier(ctx context.Context) (statelog.Position, error) {
	ctx, cancel := context.WithTimeout(ctx, wiredOpBudget)
	defer cancel()
	return n.index.Read(ctx)
}

// read is what a linearizable read waits through before reading its rows:
// the barrier, then this node's applier reaching the record before it, as
// [statelog.Reader] derives ([statelog.WaitsThrough]) and waits for it
// ([statelog.Runner.WaitReached]), so what is measured here is a read's wait.
func (n *wiredNode) read(ctx context.Context) (statelog.Position, error) {
	ctx, cancel := context.WithTimeout(ctx, wiredOpBudget)
	defer cancel()
	at, err := n.index.Read(ctx)
	if err != nil {
		return at, err
	}
	return at, n.runner.WaitReached(ctx, statelog.WaitsThrough(at))
}

// reader is a read authority over n's log that waits through waiter (n's
// runner, or a wrapper a case watches it through), on a node reporting itself
// level and sound at its reached position, as the engine's read health does.
func (n *wiredNode) reader(tb testing.TB, waiter statelog.Waiter) *statelog.Reader {
	tb.Helper()
	r, err := statelog.NewReader(statelog.ReaderDeps{
		Domain: probeDomain{}, Spec: n.spec, Mode: statelog.ModeNormal,
		DB: n.estate, Index: n.index, Waiter: waiter,
		Health: func() (statelog.Health, error) {
			h := healthy()
			h.Position = n.runner.Reached()
			return h, nil
		},
	})
	if err != nil {
		tb.Fatalf("NewReader: %v", err)
	}
	return r
}

// objects is how many probe records' rows a read's transaction sees: the
// writes, and none of the barriers between them.
func objects(ctx context.Context, tx *sql.Tx) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM probe_rows WHERE kind = 'object'`).Scan(&n)
	return n, err
}

// enteringWaiter is a node's waiter that says when the first wait through it
// begins: the moment a read found it had to wait, and so was not served.
type enteringWaiter struct {
	statelog.Waiter
	entered chan struct{}
	once    sync.Once
}

func newEnteringWaiter(inner statelog.Waiter) *enteringWaiter {
	return &enteringWaiter{Waiter: inner, entered: make(chan struct{})}
}

func (w *enteringWaiter) WaitReached(ctx context.Context, p statelog.Position) error {
	w.once.Do(func() { close(w.entered) })
	return w.Waiter.WaitReached(ctx, p)
}

// A LINEARIZABLE READ STILL WAITS FOR THE RECORD BEFORE ITS BARRIER, here a
// write whose rows the read must see. Held at its commit, it leaves the read
// waiting; released, the read is served WITH its row. A read that waited for
// nothing, or for an earlier record, would be served without it.
func TestALinearizableReadStillWaitsForTheRecordBeforeItsBarrier(t *testing.T) {
	t.Parallel()
	hold := storetest.HoldCommits(t)
	n := running(newWiredNodeWith(t, soloBroker(t, js.Config{SyncAlways: true}), "node-w",
		store.Options{WrapDriver: hold.Wrap}))
	ctx, cancel := context.WithTimeout(t.Context(), wiredOpBudget)
	defer cancel()
	subject := probeSubject("held")
	first, err := n.write(ctx, subject, statelog.Position{})
	if err != nil {
		t.Fatalf("a write before the hold: %v", err)
	}

	hold.Arm()
	// RELEASED ON EVERY WAY OUT, before the node's own cleanup stops an applier
	// that would otherwise wait on the held commit for ever.
	defer hold.Release()
	written := make(chan error, 1)
	go func() {
		_, err := n.write(ctx, subject, first.Position)
		written <- err
	}()
	if err := hold.AwaitHeld(ctx, 1); err != nil {
		t.Fatalf("the second write never reached its commit: %v", err)
	}
	type answer struct {
		rows int
		err  error
	}
	waiter := newEnteringWaiter(n.runner)
	served := make(chan answer, 1)
	go func() {
		var rows int
		_, err := n.reader(t, waiter).Read(ctx, statelog.Query{
			Level: statelog.ReadLinearizable,
			Scope: statelog.ScopeSet{Paths: []string{subject.String()}},
		}, func(tx *sql.Tx) error {
			var err error
			rows, err = objects(ctx, tx)
			return err
		})
		served <- answer{rows: rows, err: err}
	}()
	select {
	case <-waiter.entered:
	case got := <-served:
		t.Fatalf("the read was answered (%d rows, %v) before it waited", got.rows, got.err)
	}
	hold.Release()
	got := <-served
	if got.err != nil {
		t.Fatalf("the read, once the write committed: %v", got.err)
	}
	if got.rows != 2 {
		t.Errorf("the read saw %d writes, want both — it was served from rows without "+
			"the record before its barrier", got.rows)
	}
	if err := <-written; err != nil {
		t.Fatalf("the held write, released: %v", err)
	}
}

// A WRITE RUNS THE WHOLE WRITE PATH ON A REAL APPLIER: the one case here with
// every seam real, and the wiring every benchmark measures through.
//
// Two writes on ONE subject both apply, the second (its session the first's
// position) at a higher sequence, arbitrated against the anchor the first's
// apply advanced; then a linearizable read waits past both. The second holds
// only if the record NAMES ITS SUBJECT (the applier anchors by the envelope's
// subject), and any write only if it DECLARES ITS SCOPE (the applier stops
// otherwise). Every other case fakes one side, so only this one catches a
// record shape no applier accepts.
func TestAWriteRunsTheWholeWritePathOnARealApplier(t *testing.T) {
	t.Parallel()
	n := running(newWiredNode(t, soloBroker(t, js.Config{SyncAlways: true}), "node-a"))
	subject := probeSubject("wired")
	first, err := n.write(t.Context(), subject, statelog.Position{})
	if err != nil {
		t.Fatalf("the first write: %v", err)
	}
	second, err := n.write(t.Context(), subject, first.Position)
	if err != nil {
		t.Fatalf("a second write on the same subject: %v", err)
	}
	if second.Position.Seq <= first.Position.Seq {
		t.Fatalf("the second write landed at %s, not past the first at %s", second.Position, first.Position)
	}
	at, err := n.read(t.Context())
	if err != nil {
		t.Fatalf("a linearizable read: %v", err)
	}
	if at.Seq <= second.Position.Seq {
		t.Fatalf("the read's barrier is at %s, not past the second write at %s", at, second.Position)
	}
}

// A WIRED NODE'S EVERY WRITE READS ITS OWN EVICTION ROW, not an answer held
// in memory: a row naming this node evicted refuses the next write as
// evicted, and a readmission past it lets the one after that through.
func TestAWiredNodeReadsItsOwnEvictionOnEveryWrite(t *testing.T) {
	t.Parallel()
	n := running(newWiredNode(t, soloBroker(t, js.Config{SyncAlways: true}), "node-e"))
	subject := probeSubject("evicted")
	first, err := n.write(t.Context(), subject, statelog.Position{})
	if err != nil {
		t.Fatalf("a write before any eviction: %v", err)
	}
	evict := func(readmitted any) {
		t.Helper()
		if err := n.estate.Tx(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), `
				INSERT INTO probe_evictions (node_id, log_stream, from_position, readmitted_position)
				VALUES (?, ?, ?, ?)
				ON CONFLICT (node_id, log_stream) DO UPDATE SET
					from_position = excluded.from_position,
					readmitted_position = excluded.readmitted_position`,
				"node-e", probeStream, first.Position.Packed(), readmitted)
			return err
		}); err != nil {
			t.Fatalf("write the eviction row: %v", err)
		}
	}

	evict(nil)
	_, err = n.write(t.Context(), subject, first.Position)
	var refused *statelog.Unavailable
	if !errors.As(err, &refused) || refused.Reason != statelog.ReasonEvicted {
		t.Fatalf("a write by a node its own rows name evicted = %v, want refused as %s",
			err, statelog.ReasonEvicted)
	}

	evict(first.Position.Packed() + 1)
	if _, err := n.write(t.Context(), subject, first.Position); err != nil {
		t.Fatalf("a write by a node readmitted past its eviction: %v", err)
	}
}
