package artifactspike

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/jetstreamtest"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE FLEET A SPIKE RUNS ON: the real broker, the real state-log runner, the
// real publisher and the shipped datastore applier, writer and reader, wired
// as internal/engine wires them (artifactdataregister.go). Nothing between a
// write and its verdict is modelled, so a number this harness produces is the
// engine's own number on the machine that ran it.

// topology is the broker shape a spike runs on.
type topology string

const (
	// topoSolo is one embedded server and one data node: the smallest
	// deployment, and the one the review's single-node figures describe.
	topoSolo topology = "solo"

	// topoFleet is a three-member embedded cluster with a data node on
	// each member, every stream at three replicas: the smallest fleet.
	topoFleet topology = "fleet3"
)

// memberStoreBytes is what each embedded member may hold. Declared rather than
// derived from the volume's free space, so two runs on machines with different
// free space provision the same streams, and small enough that a spike can
// never fill a developer's disk: the largest spike holds a 64 MiB publish and
// a few hundred MiB of datastore records at three copies.
const memberStoreBytes = 1 << 30

// dataLogCeiling is the datastore log's ceiling in a spike: a quarter of the
// Tier A floor, as internal/artifactdata's own harness provisions it, which
// holds every record the largest DS-1 run appends with room to spare.
const dataLogCeiling = 256 << 20

// spikeRing is the fleet keyring every node of a spike holds.
var spikeRing = statelog.OneKey("k1", "the artifacts spikes' fleet key material")

// brokers is a running broker topology and one client per data node, each on
// its own member, as each engine in a fleet talks to the member it embeds.
type brokers struct {
	queues  []*js.Queue
	cluster *jetstreamtest.Cluster
}

// startBrokers starts the topology with every write fsynced (`sync: always`),
// the durability the review's figures assume.
func startBrokers(tb testing.TB, topo topology) *brokers {
	tb.Helper()
	return startBrokersOn(tb, topo, js.Config{SyncAlways: true, StoreMaxBytes: memberStoreBytes})
}

// startBrokersOn starts the topology from base, for the one spike whose
// figure does not depend on the fsync and would take an hour behind it.
func startBrokersOn(tb testing.TB, topo topology, base js.Config) *brokers {
	tb.Helper()
	switch topo {
	case topoSolo:
		base.StoreDir = tb.TempDir()
		q, err := js.Open(tb.Context(), base)
		if err != nil {
			tb.Fatalf("start the embedded broker: %v", err)
		}
		tb.Cleanup(func() {
			if err := q.Stop(context.WithoutCancel(tb.Context())); err != nil {
				tb.Errorf("stop the embedded broker: %v", err)
			}
		})
		return &brokers{queues: []*js.Queue{q}}
	case topoFleet:
		c := jetstreamtest.StartCluster(tb, 3, base)
		out := &brokers{cluster: c}
		for i := range c.Servers {
			out.queues = append(out.queues, c.Client(tb, i))
		}
		return out
	}
	tb.Fatalf("topology %q is not one the spikes run", topo)
	return nil
}

// provisionDataLog creates the datastore log on the brokers, at the domain's
// own declaration and the spike's ceiling.
func (b *brokers) provisionDataLog(tb testing.TB) {
	tb.Helper()
	spec := artifactdata.Domain{}.Stream()
	if err := b.queues[0].EnsureDomainStream(tb.Context(), js.DomainStream{
		Name: spec.Name, Subjects: spec.Subjects, MaxBytes: dataLogCeiling,
		MaxMessageBytes: spec.MaxAppendBytes(), Duplicates: spec.Duplicates,
	}); err != nil {
		tb.Fatalf("provision the datastore log: %v", err)
	}
}

// dataNode is one data node's datastore: its replicated estate, the runner
// applying the log into it, and the write and read paths over it.
type dataNode struct {
	id      string
	db      *store.DB
	log     *js.DomainLog
	runner  *statelog.Runner
	writer  *artifactdata.Writer
	reader  *artifactdata.Reader
	metrics *metrics.Recorder

	// moved counts the documents each committed batch moved on this node,
	// which is what the socket hub fans out (S6 reads it for push lag).
	moved atomic.Int64
}

// joinData builds a data node on q: the runner over the node's own consumer,
// the write authority on the same log, and the datastore's writer and reader,
// and starts the runner for the life of tb.
func joinData(tb testing.TB, q *js.Queue, id string) *dataNode {
	tb.Helper()
	d := artifactdata.Domain{}
	spec := d.Stream()
	db, _ := storetest.OpenEstate(tb, filepath.Join(tb.TempDir(), "node.db"), store.Options{}, 1)
	tb.Cleanup(func() {
		if err := db.Close(); err != nil {
			tb.Errorf("%s: close the store: %v", id, err)
		}
	})

	// RETRIED UNTIL THE STREAM HAS A LEADER, as an engine's boot rides the
	// same election under its provisioning budget.
	var (
		log      *js.DomainLog
		stats    js.LogStats
		consumer *js.DomainConsumer
		err      error
	)
	deadline := time.Now().Add(45 * time.Second)
	for {
		if log, err = q.DomainLog(tb.Context(), spec.Name); err == nil {
			if stats, err = log.Stats(tb.Context()); err == nil {
				consumer, err = q.DomainConsumer(tb.Context(), spec.Name, id, 0)
			}
		}
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			tb.Fatalf("%s: open the log, its identity and a consumer: %v", id, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	checkpoint, found, err := statelog.CheckpointOf(tb.Context(), db.Replicated(), spec.Name)
	if err != nil {
		tb.Fatalf("%s: read the checkpoint: %v", id, err)
	}
	if !found {
		checkpoint.At.Generation = 1
	}
	verifier, err := statelog.NewVerifier(d.Name(), spikeRing)
	if err != nil {
		tb.Fatalf("%s: build the verifier: %v", id, err)
	}
	signer, err := statelog.NewSigner(d.Name(), spikeRing)
	if err != nil {
		tb.Fatalf("%s: build the signer: %v", id, err)
	}
	recorder, err := metrics.New()
	if err != nil {
		tb.Fatalf("%s: build the recorder: %v", id, err)
	}
	n := &dataNode{id: id, db: db, log: log, metrics: recorder}
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain: d, Spec: spec, Fetch: consumer, Verifier: verifier, Log: log,
		Applier: artifactdata.NewApplier(id, func(moved []artifactdata.Moved) {
			for _, m := range moved {
				n.moved.Add(int64(len(m.Docs)))
			}
		}),
		Node: db, DB: db.Replicated(),
		Checkpoint: checkpoint.At, CheckpointStoredAt: checkpoint.StoredAt,
		StreamCreatedAt: stats.CreatedAt.UTC(), NodeID: id, Metrics: recorder,
	})
	if err != nil {
		tb.Fatalf("%s: build the runner: %v", id, err)
	}
	n.runner = runner

	rows, err := artifactdata.NewRows(db.Replicated().Reader(), spec)
	if err != nil {
		tb.Fatalf("%s: build the read seam: %v", id, err)
	}
	fence := artifactdata.NewFence(db.Replicated().Reader(), id)
	// NO TRIM RUNS IN A SPIKE, so the trim floor is the log's start, which
	// is what the engine's floor answers before its first trim.
	fence.Floor = func(context.Context, uint32) (uint64, error) { return 0, nil }
	fence.Ends = func(ctx context.Context) (statelog.LogEnds, error) {
		first, last, err := log.Bounds(ctx)
		return statelog.LogEnds{First: first, Last: last}, err
	}
	fence.Committed = runner.Committed
	reserve, err := statelog.NewReserve(spec, func(ctx context.Context) (statelog.Usage, error) {
		s, err := log.Stats(ctx)
		return statelog.Usage{Bytes: s.Bytes, MaxBytes: s.MaxBytes}, err
	})
	if err != nil {
		tb.Fatalf("%s: build the gate reserve: %v", id, err)
	}
	publisher, err := statelog.NewPublisher(statelog.Deps{
		Domain: d, Spec: spec, Log: log,
		Records: statelog.VerifiedLog{Log: log, Verifier: verifier},
		Signer:  signer, Rows: rows, Fence: fence,
		Gates:  artifactdata.NewGates(db.Replicated().Reader()),
		Waiter: runner, Voids: runner, Identity: runner, NodeID: id, Admission: reserve,
		Generation:    func() uint32 { return runner.Committed().Generation },
		ResolveBudget: 5 * time.Second, Metrics: recorder,
	})
	if err != nil {
		tb.Fatalf("%s: build the write authority: %v", id, err)
	}
	n.writer, err = artifactdata.NewWriter(artifactdata.WriterDeps{
		Publisher: publisher, DB: db.Replicated().Reader(), SeatExists: seatExists,
		Now: ceilingClock(),
	})
	if err != nil {
		tb.Fatalf("%s: build the writer: %v", id, err)
	}
	authority, err := statelogtest.LocalReaderOver(d, db.Replicated(), runner)
	if err != nil {
		tb.Fatalf("%s: build the read authority: %v", id, err)
	}
	n.reader, err = artifactdata.NewReader(artifactdata.ReaderOptions{
		DB: db.Replicated().Reader(), Log: authority})
	if err != nil {
		tb.Fatalf("%s: build the reader: %v", id, err)
	}

	ctx, cancel := context.WithCancel(context.WithoutCancel(tb.Context()))
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	// REGISTERED AFTER THE STORE, so it runs BEFORE the store closes: a
	// runner still holding its pinned connection would fail the close.
	tb.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			tb.Errorf("%s: the runner stopped with %v", id, err)
		}
	})
	return n
}

// ceilingClock is the writer's clock in a spike: the wall clock, plus one
// minute for every reading.
//
// WHY IT RUNS AHEAD: the writer's cost-weighted limits
// ([artifacts.DataWritesPerStorePerSecond] and its siblings) are a per-node
// guard a node sets BELOW what its applier sustains, and DS-1 exists to
// measure that ceiling. Measured through the guard, a run would report the
// guard. Each reading moving the clock a minute refills every bucket (the
// slowest, a principal's, refills its burst in five seconds), so the
// spike drives the applier and the broker, and the guard is judged against
// what they measured. The instant a record carries is authored and never
// decides anything (rows are stamped with the broker's instant).
func ceilingClock() func() time.Time {
	var readings atomic.Int64
	return func() time.Time {
		return time.Now().UTC().Add(time.Duration(readings.Add(1)) * time.Minute)
	}
}

// spikeSeats are the people a spike writes as; a person column names one.
var spikeSeats = func() map[string]bool {
	out := map[string]bool{}
	for i := range 64 {
		out[fmt.Sprintf("member-%02d", i)] = true
	}
	return out
}()

func seatExists(_ context.Context, handle string) (bool, error) { return spikeSeats[handle], nil }

// spikeLimits are the default plan quotas, stamped into every write as the
// API stamps the limits in force.
func spikeLimits() artifacts.Limits {
	return artifacts.Limits{
		CompanyBytes: artifacts.DefaultCompanyBytes, Artifacts: artifacts.DefaultArtifacts,
		DataBytes: artifacts.DefaultDataBytes, DataStoreBytes: artifacts.DefaultDataStoreBytes,
		PendingBytes: artifacts.DefaultPendingBytes,
	}
}

// editor is a person writing through the dashboard with the editor role.
func editor(handle string) artifactdata.Call {
	return artifactdata.Call{Authority: artifacts.Authority{
		Actor: iam.Actor{Name: handle, Kind: iam.ActorHuman, OperatorID: "session:" + handle},
		Role:  artifacts.RoleEditor, Limits: spikeLimits(), Origin: artifacts.OriginDashboard,
	}}
}

// owner is the artifact's owner, who configures its collections.
func owner() artifactdata.Call {
	c := editor("member-00")
	c.Authority.Role = artifacts.RoleOwner
	return c
}

// applied waits until every node has applied the datastore log through its
// end, and answers when each did, in node order.
func applied(tb testing.TB, nodes []*dataNode) []time.Time {
	tb.Helper()
	end, err := nodes[0].log.End(tb.Context())
	if err != nil {
		tb.Fatalf("read the datastore log's end: %v", err)
	}
	out := make([]time.Time, len(nodes))
	deadline := time.Now().Add(5 * time.Minute)
	for i, n := range nodes {
		for n.runner.Committed().Seq < end {
			if time.Now().After(deadline) {
				tb.Fatalf("%s applied through %s after five minutes, short of %d",
					n.id, n.runner.Committed(), end)
			}
			time.Sleep(5 * time.Millisecond)
		}
		out[i] = time.Now()
	}
	return out
}

// appliedRows is the rows a node's applier has written so far, as the runner
// itself reports them ([metrics.StatelogApplyBatchRows]): the figure an
// operator's dashboard shows, read from the same instrument.
func (n *dataNode) appliedRows() float64 {
	for _, s := range n.metrics.Read() {
		if s.Name == metrics.StatelogApplyBatchRows {
			return s.Sum
		}
	}
	return 0
}

// appliedRecords is the records a node's applier has applied so far.
func (n *dataNode) appliedRecords() uint64 {
	var total uint64
	for _, s := range n.metrics.Read() {
		if s.Name == metrics.StatelogApplyRecords && s.Attrs["result"] == "applied" {
			total += s.Total
		}
	}
	return total
}
