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
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// wiredLogBytes is the probe log's ceiling on a wired node: room for every
// record a benchmark run publishes — some tens of thousands of records of a
// few hundred bytes each, and the barriers between them — which the domain's
// own sixteen mebibytes is not, and far under any machine's broker storage.
const wiredLogBytes = 1 << 30

// wiredOpBudget bounds one write or one read on a wired node, so an applier
// that STOPPED fails the case rather than holding it until the binary's
// timeout — a write waits for this node's apply, and a stopped runner never
// gets there. Thirty seconds: six of [statelog.DefaultBarrierBudget], and half
// the [statelog.StallGrace] at which a node that has stopped applying is named
// as stalled, where a healthy write here takes milliseconds.
const wiredOpBudget = 30 * time.Second

// wiredNode is one data node's write path with every seam of it real: the
// publisher, the rows it snapshots ([statelog.NewRows] over a migrated
// estate), the write fence ([wiredFence], the read every append makes of this
// node's own eviction row, as a real domain's fence makes it), the read index,
// and the runner applying the log from the broker's own consumer into that
// estate.
//
// ONE FAKE IS LEFT, on a path a healthy write never takes: [fakeGates] is
// asked only when the publisher resolves an ambiguous append.
type wiredNode struct {
	pub    *statelog.Publisher
	runner *statelog.Runner
	index  *statelog.ReadIndex

	// log is the node's handle on the probe log, which a benchmark reads the
	// end of to count the barriers its reads appended.
	log *js.DomainLog

	// estate is the node's replicated estate, which a case writes an
	// eviction row into to see the fence read it.
	estate store.ReplicatedHandle

	// start runs the applier until the case ends. Apart from building the
	// node, because the drain benchmark builds the node — its estate, its
	// migrations — outside its timer and starts the applier inside it.
	start func()

	// stop ends the applier started, and then closes its consumer — before
	// whoever stops the broker does, since a pull given up on a closed
	// connection is logged as one that could not be — answering how the
	// applier stopped if that was not by being asked to. Once, however
	// often it is called: the case's own cleanup calls it, and so does a
	// caller done with the node before the case is — the drain benchmark,
	// between its bursts.
	stop func() error
}

// soloBroker is an embedded broker of one member, its streams on disk: every
// property the framework rests on is about a stream that survives.
func soloBroker(tb testing.TB, cfg js.Config) *js.Queue {
	tb.Helper()
	cfg.StoreDir = tb.TempDir()
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
	db, estate := storetest.OpenEstate(tb, filepath.Join(tb.TempDir(), "node.db"), store.Options{}, 1)
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
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain: probeDomain{}, Spec: spec,
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
	reserve := reserveOn(tb, log)
	gen := func() uint32 { return runner.Committed().Generation }
	pub, err := statelog.NewPublisher(statelog.Deps{
		Domain: probeDomain{}, Spec: spec,
		Log: log, Records: log, Rows: rows, Fence: fence, Gates: &fakeGates{},
		Waiter: runner, Voids: runner, Identity: runner,
		Admission: reserve, NodeID: nodeID, Generation: gen,
	})
	if err != nil {
		tb.Fatalf("NewPublisher: %v", err)
	}
	index, err := statelog.NewReadIndex(probeDomain{}, spec, log, reserve, probeEncode, gen, nil)
	if err != nil {
		tb.Fatalf("NewReadIndex: %v", err)
	}
	n := &wiredNode{pub: pub, runner: runner, index: index, log: log, estate: estate}
	n.start = func() {
		// THE CASE'S CONTEXT'S VALUES, NOT ITS END: the loop is stopped by
		// [wiredNode.stop], which then says how it stopped — under the
		// case's own context it would be cancelled before any cleanup ran,
		// and a stop for any other reason would go unseen.
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

// wiredFence is the probe domain's write fence on a wired node, built as a
// real domain's is (internal/tracker's Fence): its eviction half is ONE
// INDEXED SINGLE-ROW READ of this node's own applied rows, made on every
// append, and its zero half is the real [statelog.ZeroFence] over that read,
// the log's live ends and the applier's checkpoint.
//
// Its trim floor is the one input left fake — production reads it from
// coordination — and only a subject's FIRST write, published at an
// expectation of zero, asks for it: every later write on the subject is
// arbitrated against the anchor the one before it left.
type wiredFence struct {
	db     store.ReplicatedReader
	nodeID string
	stream string
	zero   statelog.ZeroFence
}

// Evicted reads this node's own eviction row, as a real domain's fence does:
// no row is not evicted, and a read that fails is unknown rather than "not
// evicted", because publishing on a guess is how an evicted node collects
// acknowledgements for records every peer drops.
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

// write is one arbitrated write of a probe record onto subject, end to end:
// the fence, the wait for this node to reach session — the caller's own last
// write, as a seat's turn carries it, or the zero position for none — the
// publisher's snapshot and decision, the append the broker arbitrates, and
// this node's applier reaching it. Anything but applied is an error.
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

// read is what a linearizable read waits through before it reads its rows:
// the barrier, and this node's applier reaching it.
func (n *wiredNode) read(ctx context.Context) (statelog.Position, error) {
	ctx, cancel := context.WithTimeout(ctx, wiredOpBudget)
	defer cancel()
	at, err := n.index.Read(ctx)
	if err != nil {
		return at, err
	}
	return at, n.runner.WaitCommitted(ctx, at)
}

// A WRITE RUNS THE WHOLE WRITE PATH ON A REAL APPLIER — the one case in this
// package whose every seam is real, and the wiring every benchmark here
// measures through.
//
// Two writes on ONE subject both apply, the second at a higher sequence than
// the first, because it was arbitrated against the anchor the first one's
// apply advanced, and carrying the first one's position as its session, as a
// seat's turn carries its own last write; then a linearizable read waits past
// both. The second write holds only if the record NAMES ITS SUBJECT, since the
// applier anchors an arbitrated record by its envelope's subject, and any
// write holds only if the record DECLARES ITS SCOPE, since the applier stops
// on one that does not. Every other case here fakes the publisher's side or
// the applier's, so a record shape no applier accepts went unnoticed until
// this one ran.
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

// A WIRED NODE'S EVERY WRITE READS ITS OWN EVICTION ROW: the fence is the
// read a real domain's fence makes on every append, not an answer held in
// memory. A row naming this node evicted refuses the next write, as evicted;
// a readmission past it lets the one after that through.
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
