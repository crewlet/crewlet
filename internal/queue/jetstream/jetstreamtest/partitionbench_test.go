package jetstreamtest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/kv"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/pages"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// BenchmarkPartitionedEstate measures what the embedded broker pays to carry a
// PARTITIONED replicated estate: the partition layout the estate contract
// declares, on a fleet of three (or five) members, with every partition
// applied by R=3 of N data holders.
//
// # The layout it builds
//
// For a tracker count T: a tracker space of T partitions, each with a tracker
// log and a vectors log; a pages space of T/4 partitions, each with a pages log
// and a vectors log; and one company partition with a tracker log. That is
// 2T + 2·T/4 + 1 logs — 161 at T = 64 and 641 at T = 256. Every tracker, pages
// and company log carries a WAKE-FEED consumer at the stream's replicas (T +
// T/4 + 1 of them), and every holder of a partition has an APPLIER consumer on
// each of its logs, at consumer replicas 1 or 3 — the shape's choice, because
// the contract decides R = 1 and asks for both to be measured.
//
// # What it answers, and why it is a benchmark rather than a test
//
// Partition counts are a design input, and the design needs NUMBERS: how long
// the whole column takes to provision against [jsprovision]'s budgets, what a
// member costs idle in CPU, memory, descriptors, raft groups and disk, how long
// the leader elections a member restart causes take to settle, what one
// conditional append costs at the sizing estimate's rate, how fast holders
// drain a backlog, whether a holder joined as a LEAF reads its partitions and
// at what latency, and what a hundred seat nodes' presence and listing traffic
// costs beside all of it. None of that is pass/fail, and all of it is minutes
// of wall clock — which is why it is a Benchmark: `go test` without -bench
// never runs it, so the suite pays nothing, and `make test-solo` still
// compiles it.
//
// Run one shape at a time, once, on a quiet machine, with the stores on
// tmpfs. A shape takes five to fifteen minutes and up to 7 GiB of memory:
//
//	TMPDIR=/dev/shm go test -run '^$' -benchtime 1x -timeout 60m -v \
//	  -bench 'PartitionedEstate/members=3,T=64,logs=161,applier=R1,holders=3,via=member$' \
//	  ./internal/queue/jetstream/jetstreamtest/
//
// Every phase logs its own lines, each snapshot carries the host's load average
// so a phase that shared the CPUs with something else says so, and the
// headline numbers are reported as benchmark metrics.
//
// # Everything goes through the engine's own paths — with two exceptions
//
// The members are this harness's, the streams are [js.Queue.EnsureDomainStream]
// over the domains' own [statelog.StreamSpec] (split, nothing else changed),
// the wake feeds are [js.DomainLog.Group], the R = 3 appliers are
// [js.Queue.DomainConsumer], every sequence runs under the ceiling
// [jsprovision.SequenceBudget] states, the appends are [js.DomainLog.Append]
// with a per-subject expectation, and the seat nodes are leaves claiming and
// renewing through [kv.Store] at [seat.HeartbeatInterval] and listing presence
// at [seat.SweepInterval]. A number measured through a shortcut would be a
// number about the shortcut.
//
// The EXCEPTIONS are what the engine does not do yet. An R = 1 applier is the
// domain consumer's configuration at one replica, created through
// [jsprovision.Place] as every other create is ([applierConfig]). And the
// apply loop is one long-lived pull per consumer ([persistentAppliers]),
// because the runner's own pull allocates about 32 MiB per call and a
// partitioned estate's consumer count exhausts the host with it —
// [estateRun.enginePull] measures that pull on three consumers, so the number
// moves when the pull is fixed.
//
// # What it cannot see
//
// Every member, holder and seat node is in THIS process, so CPU, heap and
// goroutines are the process's: an upper bound on any one member's, and the
// hundred seat nodes' own brokers alone are about a gibibyte of it. The store
// is wherever TMPDIR points, and on tmpfs an fsync costs nothing — an append
// latency measured there is the broker's and the network's, never a disk's.
//
// # What it measured, on this container: four cores, 15 GiB, tmpfs
//
// Other test suites were running beside it for much of the matrix (host load
// 1 to 12); the ranges are across runs, and the tails of the contended ones
// are the upper ends. Every shape provisioned with nothing failing.
//
//   - THE COLUMN provisions in 14 s at T = 64 and 55-58 s at T = 256 with R = 1
//     appliers, and in 22 s and 92 s with R = 3 — against a five-minute
//     sequence ceiling. The maintainer's one sequence is most of it (every
//     stream and wake consumer at about 55 ms each, 13 s and 53-55 s); an R = 1
//     applier is 1.6-22 ms because it is not a raft group, and an R = 3 one is
//     55 ms, which at T = 256 took three holders 37 s against 2-4 s.
//   - IDLE, the whole process — three members and every holder's client —
//     used 0.09-0.15 cores at T = 64 and 0.34-0.48 at T = 256 with R = 1
//     appliers (0.32 and 0.76-0.86 with R = 3), and its RSS was 0.2-0.5 GiB
//     and 0.7-1.2 GiB (0.4-0.5 and 1.3-2.0 with R = 3). Per member at
//     T = 256: 992 raft groups with R = 1 appliers, 2,915 with R = 3, and
//     about 595 on each of five members.
//   - A LOG costs the members about 4.5 raft instances with R = 1 appliers
//     (13.5 with R = 3), 40 goroutines (76), 0.6-1.1 MiB of cold heap (1.2-1.4),
//     7 fds (13-17), 185 KiB of disk empty (440), and 0.5-0.9 ms of CPU a
//     second idle (1.2-2) — 0.1-0.2 ms per raft instance, which is the
//     leaders' heartbeats. A raft instance
//     written to in the last ten seconds also holds a 256 KiB block cache.
//   - A MEMBER RESTART rejoins in 0.2-2 s and the fleet settles — every stream
//     and replicated consumer led, every replica current, every R = 1 applier
//     answering — in 3.4-6 s, with 80-955 leaders moved: inside
//     statelog.StallGrace at both counts. That is tmpfs and an immediate
//     restart; a member recovering its groups from a disk takes longer.
//   - THE RUNNER'S PULL allocates 30-33 MiB per call. A persistent pull costs
//     4-17 KiB/s per idle consumer and stays within a tenth of a core of the
//     raft floor, and applies a record 0.6-1 ms after it is stored. (The count-bounded poll
//     this benchmark measured first cost 1.3-3 cores idle at 1,539 consumers
//     and, under contention, stalled records delivered to expired pulls for
//     the thirty-second ack window.)
//   - APPENDS at 75 records/s: 0.67-0.86 ms p50 and 2.6-4.4 ms p99 on a quiet
//     host, at either count; a leaf adds at most about 130 µs to an append and
//     60-90 µs to a one-record pull. Every holder at once drains a
//     20,000-record backlog at 27,000-64,000 records/s together.
//   - A HUNDRED SEAT NODES as leaves, each claiming presence at the heartbeat
//     and listing it at the sweep: about 30 operations a second and half a
//     core, a listing of 100 nodes 11-31 ms at the median. A gated seat claim
//     scans every lease (kv.Store.readForClaim): 40-95 ms with a hundred held
//     and 0.3-2 s with nine thousand; holding nine thousand is 650-920
//     renewals a second, 0.76 ms p50 on a quiet host.
func BenchmarkPartitionedEstate(b *testing.B) {
	for _, shape := range estateShapes {
		b.Run(shape.String(), func(b *testing.B) {
			measureEstate(b, shape)
		})
	}
}

// estateShape is one fleet to measure.
type estateShape struct {
	// members is the broker cluster; every stream is placed on 3 of them.
	members int
	// tracker is T, the tracker space's partition count. The pages space
	// has T/4 and the company space one, as the layout declares.
	tracker int
	// applier is the applier consumers' replica count: 1, as the contract
	// decides, or 3, the stream's, as the engine creates them today.
	applier int
	// holders is how many data nodes apply the partitions, each partition
	// held by [holdersPerPartition] of them.
	holders int
	// leaf joins the holders to the fleet as LEAVES — JetStream off, the
	// `crewlet` domain across their link — rather than as members' own
	// clients.
	leaf bool
	// seatNodes is how many stateless seat nodes join as leaves for the
	// coordination phase; zero skips it.
	seatNodes int
	// seats is how many seat leases each of them holds; zero measures
	// presence and listing alone.
	seats int
}

// logs is 2T + 2·T/4 + 1: the tracker space's tracker and vectors logs, the
// pages space's pages and vectors logs, and the company's tracker log.
func (s estateShape) logs() int { return 2*s.tracker + 2*(s.tracker/4) + 1 }

// wakeLogs is T + T/4 + 1: every tracker, pages and company log.
func (s estateShape) wakeLogs() int { return s.tracker + s.tracker/4 + 1 }

func (s estateShape) String() string {
	via := "member"
	if s.leaf {
		via = "leaf"
	}
	return fmt.Sprintf("members=%d,T=%d,logs=%d,applier=R%d,holders=%d,via=%s",
		s.members, s.tracker, s.logs(), s.applier, s.holders, via)
}

// estateShapes is the matrix: both candidate counts, the appliers at R = 3 and
// at R = 1 on three holders (every node holds every partition, the heaviest
// node and the heaviest boot), and at R = 1 on twenty leaf holders with a
// hundred seat nodes beside them, which is the fleet the sizing estimate
// describes. The five-member fleet is the larger count's comparison point.
var estateShapes = []estateShape{
	{members: 3, tracker: 64, applier: 3, holders: 3},
	{members: 3, tracker: 64, applier: 1, holders: 3},
	{members: 3, tracker: 64, applier: 1, holders: 20, leaf: true, seatNodes: 100,
		seats: seatsPerNode},
	{members: 3, tracker: 256, applier: 3, holders: 3},
	{members: 3, tracker: 256, applier: 1, holders: 3},
	{members: 3, tracker: 256, applier: 1, holders: 20, leaf: true, seatNodes: 100,
		seats: seatsPerNode},
	{members: 5, tracker: 256, applier: 1, holders: 20, leaf: true},
}

const (
	// holdersPerPartition is the planned R: each partition applied into SQL
	// by three data holders, and replicated at three by the broker.
	holdersPerPartition = 3

	// The domains' TOTAL ceilings, divided evenly over the partitions of a
	// space as the contract's even division does. What the engine derives on
	// a volume with 16 GiB free (the tmpfs this runs on): the mutation log a
	// quarter of free space clamped to its 4 GiB floor, pages a quarter of
	// that clamped to 1 GiB, and the vectors sized like the log. The broker
	// RESERVES a ceiling at create, so the sum has to fit what every
	// member's store may hold.
	trackerTotalBytes = 4 << 30
	vectorsTotalBytes = 4 << 30
	pagesTotalBytes   = 1 << 30

	// recordBytes is one tracker record: a task change is a JSON envelope
	// of a few hundred bytes.
	recordBytes = 512

	// subjectsPerLog is how many objects each tracker log's writes land on
	// — enough that two writers rarely race one subject.
	subjectsPerLog = 40

	// idleWindow and steadyWindow are how long each steady-state phase
	// runs: long enough for a p99 over two thousand appends.
	idleWindow   = 15 * time.Second
	steadyWindow = 30 * time.Second

	// steadyRate is the sizing estimate's fleet-wide record rate at 10,000
	// seats.
	steadyRate = 75.0

	// backlogRecords is the catch-up drained in the throughput phase,
	// spread over the tracker space's tracker logs.
	backlogRecords = 20_000

	// probeLogs and probeSamples size the leaf-versus-member probe: spread
	// over logs so no one stream leader's placement decides the comparison.
	probeLogs    = 16
	probeSamples = 12

	// restartedMember is the member the election phase restarts. Not 0,
	// whose client inspects the fleet while it happens.
	restartedMember = 2

	// settleCeiling is how long the election phase waits for the fleet to
	// settle before reporting it did not: twice statelog.StallGrace, which
	// is the contract's bound, so a settle that misses it is measured
	// rather than cut off.
	settleCeiling = 2 * statelog.StallGrace

	// coordWindow is how long each coordination phase runs: three
	// heartbeats, so every node renews at least twice.
	coordWindow = 3 * seat.HeartbeatInterval

	// seatsPerNode is how many seat leases each seat node holds where a
	// shape asks for seats: 100 nodes at 100 seats is the 10,000-seat
	// fleet the sizing estimate is for.
	seatsPerNode = 100
)

// partLog is one log of one partition, and who holds the partition.
type partLog struct {
	space  string // tracker, pages or company
	domain string // tracker, vectors or pages
	spec   js.DomainStream
	prefix string
	// wake is whether this log carries a wake-feed consumer: every tracker,
	// pages and company log does, and no vectors log.
	wake    bool
	holders []int
}

// layoutFor builds the contract's layout for a shape: every registered
// domain's own stream spec, split into the space's partitions with the
// domain's ceiling divided evenly — the name and subject space per partition
// are the only other fields that change.
func layoutFor(s estateShape) []*partLog {
	var logs []*partLog
	partition := 0
	space := func(name, letter string, count int, domains []string) {
		for k := range count {
			holders := make([]int, holdersPerPartition)
			// ROUND ROBIN over the global partition index, so every
			// holder takes the same share of every space and three
			// holders degenerate to "everyone holds all".
			for r := range holders {
				holders[r] = (partition + r) % s.holders
			}
			partition++
			for _, domain := range domains {
				spec, total, shares := domainSpec(domain, s)
				suffix := fmt.Sprintf("%s%03d", letter, k)
				prefix := spec.SubjectPrefix + "." + suffix
				logs = append(logs, &partLog{
					space: name, domain: domain, prefix: prefix,
					wake: domain != "vectors", holders: holders,
					spec: js.DomainStream{
						Name:          spec.Name + "_" + strings.ToUpper(suffix),
						Subjects:      []string{prefix + ".>"},
						MaxBytes:      (total + int64(shares) - 1) / int64(shares),
						MaxPerSubject: spec.MaxPerSubject, MaxAge: spec.MaxAge,
						Duplicates: spec.Duplicates,
					},
				})
			}
		}
	}
	space("tracker", "t", s.tracker, []string{"tracker", "vectors"})
	space("pages", "p", s.tracker/4, []string{"pages", "vectors"})
	space("company", "c", 1, []string{"tracker"})
	return logs
}

// domainSpec is a domain's own stream spec, its total ceiling, and how many
// logs share that ceiling.
func domainSpec(domain string, s estateShape) (statelog.StreamSpec, int64, int) {
	switch domain {
	case "tracker":
		// The company's log is sized like a tracker partition's.
		return statelog.EstateStream(tracker.Domain{}), trackerTotalBytes, s.tracker
	case "vectors":
		return statelog.EstateStream(search.Domain{}), vectorsTotalBytes, s.tracker + s.tracker/4
	default:
		return statelog.EstateStream(pages.Domain{}), pagesTotalBytes, s.tracker / 4
	}
}

// holder is one data node: a queue client and the applier consumer it holds
// on each of its logs.
type holder struct {
	id        string
	q         *js.Queue
	logs      []*partLog
	consumers map[*partLog]string
}

func measureEstate(b *testing.B, shape estateShape) {
	ctx := b.Context()
	r := &estateRun{b: b, shape: shape}
	r.logf("machine: %d CPUs (GOMAXPROCS %d), %s, store under %s",
		runtime.NumCPU(), runtime.GOMAXPROCS(0), runtime.Version(), os.TempDir())

	// SyncAlways because it is Tier A's default (config.Stream.SyncAlways):
	// the fleet this measures fsyncs every write.
	start := time.Now()
	r.cluster = startCluster(b, shape.members,
		js.Config{Replicas: holdersPerPartition, SyncAlways: true}, true)
	admin := r.cluster.Client(b, 0)
	openCoordination(ctx, b, admin)
	r.logf("cluster of %d with the engine's streams and coordination buckets up in %s",
		shape.members, time.Since(start).Round(time.Millisecond))
	r.snap("baseline: engine streams + coordination buckets")

	early := timeOneCreate(ctx, b, admin, "CREWLET_BENCH_EARLY")
	logs := layoutFor(shape)
	holders := r.connectHolders(ctx, logs)
	r.snap(fmt.Sprintf("%d holders connected", len(holders)))

	groups := r.provision(ctx, admin, holders, logs)
	defer stopGroups(groups)
	late := timeOneCreate(ctx, b, admin, "CREWLET_BENCH_LATE")
	r.logf("one stream + one consumer on the quiet estate: %s before the partitions, "+
		"%s after", early.Round(time.Microsecond), late.Round(time.Microsecond))
	b.ReportMetric(late.Seconds()*1e3, "late-create-ms")
	r.snap("provisioned")
	// WHAT THE RAFT GROUPS COST WITH NOTHING BUT THE WAKE FEEDS PARKED: the
	// heartbeats every group's leader sends its followers, which is the
	// floor under every applier phase below.
	none, _ := newAppliers(ctx, nil)
	floor := r.window(ctx, "provisioned, idle, no appliers", idleWindow, none, nil)
	none.stop()
	r.perLogCost(floor, len(logs))
	b.ReportMetric(floor.cores, "idle-cores")
	b.ReportMetric(mib(floor.after.rss), "idle-rss-MiB")

	writer := r.leafClient(ctx, "writer")
	pub := admin
	if shape.leaf {
		pub = writer
	}
	w := newWriter(ctx, b, pub, logs)

	r.enginePull(ctx, admin, logs)
	loops := r.persistentAppliers(ctx, holders, w)
	r.window(ctx, "appliers idle", idleWindow, loops, nil)
	r.window(ctx, fmt.Sprintf("appending at %.0f/s", steadyRate), steadyWindow, loops, w)
	loops.awaitApplied(ctx, b, holders)
	loops.stop()

	loops = r.throughput(ctx, holders, w)
	if loops == nil {
		return
	}
	r.probe(ctx, admin, writer, w)
	if shape.seatNodes > 0 {
		r.coordination(ctx, loops)
	}
	loops.stop()

	r.restart(ctx, logs, holders)
}

// estateRun carries one shape's cluster and its running record.
type estateRun struct {
	b       *testing.B
	shape   estateShape
	cluster *Cluster
	snaps   []snapshot
}

func (r *estateRun) logf(format string, args ...any) {
	r.b.Helper()
	r.b.Logf("[%s] "+format, append([]any{r.shape}, args...)...)
}

// openCoordination opens both coordination stores exactly as the engine does,
// so the baseline carries every bucket a fleet runs beside its streams.
func openCoordination(ctx context.Context, b *testing.B, q *js.Queue) {
	b.Helper()
	if _, err := kv.OpenFleet(ctx, q.JetStream(), kv.FleetConfig{
		RateWindow: coord.RateWindow, ClaimTTL: coord.ClaimTTL,
		LedgerRetention: coord.LedgerRetention, FireRetention: coord.FireRetention,
		FollowRetention: coord.FollowRetention, CooldownMax: coord.CooldownMax,
		StatusFreshness: coord.StatusFreshness, RebaseRetention: coord.RebaseRetention,
		Replicas: holdersPerPartition, Clustered: true,
	}); err != nil {
		b.Fatalf("open the fleet's coordination buckets: %v", err)
	}
	if _, err := openLeases(ctx, q); err != nil {
		b.Fatalf("open the lease buckets: %v", err)
	}
}

func openLeases(ctx context.Context, q *js.Queue) (*kv.Store, error) {
	return kv.Open(ctx, q.JetStream(), kv.Config{
		TTL: seat.SeatLeaseTTL, Replicas: holdersPerPartition, Clustered: true,
	})
}

// timeOneCreate is one stream and one consumer made in isolation: what a
// single create costs against the metadata group as it stands.
func timeOneCreate(ctx context.Context, b *testing.B, q *js.Queue, name string) time.Duration {
	b.Helper()
	start := time.Now()
	if err := q.EnsureDomainStream(ctx, js.DomainStream{
		Name: name, Subjects: []string{"crewlet.bench." + strings.ToLower(name) + ".>"},
		MaxBytes: 64 << 20, Duplicates: 2 * time.Minute,
	}); err != nil {
		b.Fatalf("create %s: %v", name, err)
	}
	if _, err := q.DomainConsumer(ctx, name, "probe", 0); err != nil {
		b.Fatalf("consume %s: %v", name, err)
	}
	return time.Since(start)
}

// leafClient starts a leaf broker joined to every member's leaf listener and
// returns a queue on it — what a node without the `data` role runs.
func (r *estateRun) leafClient(ctx context.Context, name string) *js.Queue {
	r.b.Helper()
	q, err := r.startLeaf(ctx, name)
	if err != nil {
		r.b.Fatalf("leaf %s: %v", name, err)
	}
	return q
}

func (r *estateRun) startLeaf(ctx context.Context, name string) (*js.Queue, error) {
	srv, err := js.StartServer(ctx, js.Config{
		ServerName: name, LeafURLs: r.cluster.LeafURLs(), Replicas: holdersPerPartition,
	})
	if err != nil {
		return nil, err
	}
	r.b.Cleanup(srv.Shutdown)
	q, err := srv.Client(ctx)
	if err != nil {
		return nil, err
	}
	r.b.Cleanup(func() { _ = q.Stop(context.WithoutCancel(ctx)) })
	return q, nil
}

// connectHolders gives every holder its queue: a client of member h mod M, or
// a leaf of its own.
func (r *estateRun) connectHolders(ctx context.Context, logs []*partLog) []*holder {
	r.b.Helper()
	holders := make([]*holder, r.shape.holders)
	for h := range holders {
		holders[h] = &holder{id: fmt.Sprintf("holder-%02d", h), consumers: map[*partLog]string{}}
	}
	for _, l := range logs {
		for _, h := range l.holders {
			holders[h].logs = append(holders[h].logs, l)
		}
	}
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, len(holders))
	for h, hd := range holders {
		wg.Go(func() {
			if r.shape.leaf {
				q, err := r.startLeaf(ctx, hd.id)
				hd.q = q
				errs <- err
				return
			}
			q, err := r.cluster.Servers[h%r.shape.members].Client(ctx)
			if err == nil {
				r.b.Cleanup(func() { _ = q.Stop(context.WithoutCancel(ctx)) })
			}
			hd.q = q
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			r.b.Fatalf("connect a holder: %v", err)
		}
	}
	r.logf("%d holders connected in %s, %d-%d logs each", len(holders),
		time.Since(start).Round(time.Millisecond), len(holders[len(holders)-1].logs),
		len(holders[0].logs))
	return holders
}

// createTiming is one create in one sequence.
type createTiming struct {
	kind string
	pos  int // position in its sequence, from 0
	took time.Duration
	err  error
}

// sequence is one node's provisioning run, under one
// [jsprovision.SequenceBudget] as the engine's own boot runs it.
type sequence struct {
	timings []createTiming
	took    time.Duration
}

func (s *sequence) add(kind string, took time.Duration, err error) {
	s.timings = append(s.timings, createTiming{kind: kind, pos: len(s.timings), took: took, err: err})
}

// provision brings the whole column up in the contract's order. The MAP'S
// MAINTAINER provisions every stream of the layout once, with the wake feeds
// beside them; then every holder boots at once, ensuring the streams of the
// partitions it holds and opening its applier on each of their logs — each of
// the two a sequence under the budget a boot runs under.
func (r *estateRun) provision(ctx context.Context, admin *js.Queue, holders []*holder,
	logs []*partLog) []*js.DomainGroup {

	r.b.Helper()
	budget := jsprovision.Clustered(true).SequenceBudget()
	before := r.snap("before provisioning")

	var maintainer sequence
	var groups []*js.DomainGroup
	began := time.Now()
	func() {
		seqCtx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		for _, l := range logs {
			t0 := time.Now()
			err := admin.EnsureDomainStream(seqCtx, l.spec)
			maintainer.add("stream", time.Since(t0), err)
		}
		for _, l := range logs {
			if !l.wake {
				continue
			}
			t0 := time.Now()
			appendTo, err := admin.DomainLog(seqCtx, l.spec.Name)
			var g *js.DomainGroup
			if err == nil {
				// ON THE RUN'S CONTEXT, not the sequence's: a group's
				// consumption lives as long as the run does.
				g, err = appendTo.Group(ctx, "wake")
			}
			maintainer.add("wake consumer", time.Since(t0), err)
			if err == nil {
				groups = append(groups, g)
			}
		}
		maintainer.took = time.Since(began)
	}()
	drainGroups(ctx, groups)

	booted := time.Now()
	perHolder := make([]sequence, len(holders))
	var wg sync.WaitGroup
	for h, hd := range holders {
		wg.Go(func() {
			seqCtx, cancel := context.WithTimeout(ctx, budget)
			defer cancel()
			seq := &perHolder[h]
			start := time.Now()
			for _, l := range hd.logs {
				t0 := time.Now()
				err := hd.q.EnsureDomainStream(seqCtx, l.spec)
				seq.add("stream observe", time.Since(t0), err)
			}
			for _, l := range hd.logs {
				t0 := time.Now()
				name, err := r.openApplier(seqCtx, hd, l)
				seq.add(fmt.Sprintf("applier R=%d", r.shape.applier), time.Since(t0), err)
				if err == nil {
					hd.consumers[l] = name
				}
			}
			seq.took = time.Since(start)
		})
	}
	wg.Wait()
	holdersTook := time.Since(booted)
	after := r.snapQuiet()

	wakes, appliers := r.shape.wakeLogs(), len(logs)*holdersPerPartition
	r.logf("PROVISIONING the column — %d streams at R=3, %d wake consumers at R=3, %d "+
		"appliers at R=%d: %s in all, CPU %.1f cores", len(logs), wakes, appliers,
		r.shape.applier, (maintainer.took + holdersTook).Round(time.Millisecond),
		cores(before, after))
	failures := r.summarise("maintainer", []sequence{maintainer}, budget)
	failures = append(failures, r.summarise(fmt.Sprintf("%d holders, at once", len(holders)),
		perHolder, budget)...)
	for i, err := range failures {
		if i == 5 {
			r.logf("  … and %d more failures", len(failures)-5)
			break
		}
		r.logf("  FAILED: %v", err)
	}
	r.b.ReportMetric(maintainer.took.Seconds(), "maintainer-s")
	r.b.ReportMetric(holdersTook.Seconds(), "holders-s")
	r.b.ReportMetric(float64(len(failures)), "provision-failures")
	if len(failures) > 0 {
		r.b.Errorf("%d creates failed during provisioning; first: %v", len(failures), failures[0])
	}
	return groups
}

// summarise logs what a set of sequences cost and returns their failures.
func (r *estateRun) summarise(who string, seqs []sequence, budget time.Duration) []error {
	byKind := map[string]*samples{}
	var kinds []string
	var late samples
	var failures []error
	slow := 0
	var slowest time.Duration
	for _, seq := range seqs {
		slowest = max(slowest, seq.took)
		for _, c := range seq.timings {
			if c.err != nil {
				failures = append(failures, fmt.Errorf("%s %s: %w", who, c.kind, c.err))
				continue
			}
			if byKind[c.kind] == nil {
				byKind[c.kind] = &samples{}
				kinds = append(kinds, c.kind)
			}
			byKind[c.kind].add(c.took)
			if c.took >= jsprovision.SlowAfter {
				slow++
			}
			// THE LAST TENTH of the sequence is "late": by then the
			// metadata group holds nearly the whole column.
			if c.pos >= len(seq.timings)*9/10 {
				late.add(c.took)
			}
		}
	}
	r.logf("  %s: slowest sequence %s of the %s budget, %d failed, %d creates past "+
		"jsprovision.SlowAfter (a request the metadata group dropped, re-asked a term later)",
		who, slowest.Round(time.Millisecond), budget, len(failures), slow)
	for _, kind := range kinds {
		r.logf("    %-16s %s", kind, byKind[kind].summary())
	}
	r.logf("    %-16s %s", "last tenth", late.summary())
	return failures
}

// openApplier opens a holder's applier on one log, at the shape's replicas.
func (r *estateRun) openApplier(ctx context.Context, hd *holder, l *partLog) (string, error) {
	if r.shape.applier == holdersPerPartition {
		c, err := hd.q.DomainConsumer(ctx, l.spec.Name, hd.id, 0)
		if err != nil {
			return "", err
		}
		return c.Name(), nil
	}
	name := "statelog__" + l.spec.Name + "__" + hd.id
	createCtx, cancel := context.WithTimeout(ctx, hd.q.Clustered().Budget())
	defer cancel()
	return name, jsprovision.Place(createCtx, hd.q.Clustered().AskTerm(),
		func(ctx context.Context) error {
			_, err := hd.q.JetStream().CreateConsumer(ctx, l.spec.Name,
				applierConfig(name, r.shape.applier))
			return err
		}, nil)
}

// applierConfig is the domain consumer's configuration at a chosen replica
// count — what the contract's R = 1 decision will create, which the engine's
// [js.Queue.DomainConsumer] does not yet take. Every field but Replicas is the
// engine's own: explicit acknowledgement, the thirty-second ack window, the
// in-flight ceiling at [statelog.FetchMessages], no delivery limit, and every
// record from the start.
func applierConfig(name string, replicas int) jetstream.ConsumerConfig {
	return jetstream.ConsumerConfig{
		Durable:       name,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       30 * time.Second,
		MaxAckPending: statelog.FetchMessages,
		MaxDeliver:    -1,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		Replicas:      replicas,
	}
}

// drainGroups acknowledges every wake the feeds deliver, as the change feed
// does, until the run ends.
func drainGroups(ctx context.Context, groups []*js.DomainGroup) {
	for _, g := range groups {
		go func() {
			for ctx.Err() == nil {
				d, err := g.Next(ctx)
				if err != nil || d == nil {
					if d == nil && err == nil {
						return
					}
					continue
				}
				_ = d.Ack()
			}
		}()
	}
}

func stopGroups(groups []*js.DomainGroup) {
	for _, g := range groups {
		_ = g.Stop()
	}
}

// perLogCost divides what the layout added by how many logs it has, and says
// what one PARTITION of the tracker space — two logs, two wake-or-not
// consumers and six appliers — costs.
func (r *estateRun) perLogCost(floor windowResult, logs int) {
	var base snapshot
	for _, s := range r.snaps {
		if s.label == "before provisioning" {
			base = s
		}
	}
	p := floor.after
	groups := sum(p.raft) - sum(base.raft)
	r.logf("PER LOG, over all members: %.2f raft group instances, %.0f goroutines, "+
		"%.0f KiB heap, %.2f fds, %.0f KiB on disk, %.2f ms/s CPU idle",
		float64(groups)/float64(logs),
		float64(p.goroutines-base.goroutines)/float64(logs),
		float64(int64(p.heapInuse)-int64(base.heapInuse))/float64(logs)/1024,
		float64(p.fds-base.fds)/float64(logs),
		float64(sum(p.disk)-sum(base.disk))/float64(logs)/1024,
		floor.cores*1e3/float64(logs))
}

// writer appends tracker records with a per-subject expectation, remembering
// each subject's last sequence as the statelog writer remembers it in SQL.
type writer struct {
	logs     map[*partLog]*js.DomainLog
	tracker  []*partLog
	mu       sync.Mutex
	last     map[string]uint64
	locks    map[string]*sync.Mutex
	counts   map[*partLog]*atomic.Int64
	appended samples
	failed   atomic.Int64
	missed   atomic.Int64
	firstErr atomic.Pointer[error]
}

// newWriter opens every tracker-space tracker log for appends.
func newWriter(ctx context.Context, b *testing.B, q *js.Queue, logs []*partLog) *writer {
	b.Helper()
	w := &writer{logs: map[*partLog]*js.DomainLog{}, last: map[string]uint64{},
		locks: map[string]*sync.Mutex{}, counts: map[*partLog]*atomic.Int64{}}
	for _, l := range logs {
		w.counts[l] = &atomic.Int64{}
		if l.space != "tracker" || l.domain != "tracker" {
			continue
		}
		appendTo, err := q.DomainLog(ctx, l.spec.Name)
		if err != nil {
			b.Fatalf("open %s for appends: %v", l.spec.Name, err)
		}
		w.logs[l] = appendTo
		w.tracker = append(w.tracker, l)
	}
	return w
}

var payload = make([]byte, recordBytes)

// appendOne writes one record to a random object of a tracker log,
// conditional on that object's last sequence.
func (w *writer) appendOne(ctx context.Context, l *partLog, lat *samples) error {
	subject := fmt.Sprintf("%s.task.%04d", l.prefix, rand.IntN(subjectsPerLog))
	w.mu.Lock()
	lock, ok := w.locks[subject]
	if !ok {
		lock = &sync.Mutex{}
		w.locks[subject] = lock
	}
	w.mu.Unlock()
	lock.Lock()
	defer lock.Unlock()
	w.mu.Lock()
	expect := w.last[subject]
	w.mu.Unlock()

	start := time.Now()
	seq, _, err := w.logs[l].Append(ctx, subject, uuid.NewString(), &expect, payload)
	if err != nil {
		w.failed.Add(1)
		w.firstErr.CompareAndSwap(nil, &err)
		// RE-READ, as the statelog writer does on a refusal: an append
		// whose answer was lost may still have landed, and an expectation
		// left behind it refuses every later write to this subject.
		if last, ok, readErr := w.logs[l].LastSeq(ctx, subject); readErr == nil {
			w.mu.Lock()
			if ok {
				w.last[subject] = last
			} else {
				w.last[subject] = 0
			}
			w.mu.Unlock()
		}
		return err
	}
	if lat != nil {
		lat.add(time.Since(start))
	}
	w.mu.Lock()
	w.last[subject] = seq
	w.mu.Unlock()
	w.counts[l].Add(1)
	return nil
}

// atRate appends at rate records per second, spread uniformly over the
// tracker logs, until ctx ends. A tick that finds every worker busy is counted
// as missed rather than queued, so a slow broker shows up as a rate shortfall
// and not as a latency the queue manufactured.
//
// The appends run on a context the window's end does NOT cancel: an append cut
// off mid-flight may still land, and its expectation would then be stale.
func (w *writer) atRate(ctx context.Context, rate float64) {
	work := make(chan *partLog, 64)
	appendCtx := context.WithoutCancel(ctx)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for l := range work {
				_ = w.appendOne(appendCtx, l, &w.appended)
			}
		})
	}
	tick := time.NewTicker(time.Duration(float64(time.Second) / rate))
	defer func() {
		tick.Stop()
		close(work)
		wg.Wait()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			select {
			case work <- w.tracker[rand.IntN(len(w.tracker))]:
			default:
				w.missed.Add(1)
			}
		}
	}
}

func firstError(w *writer) string {
	if err := w.firstErr.Load(); err != nil {
		return fmt.Sprintf(" (first: %v)", *err)
	}
	return ""
}

// appliers is every holder's apply loop, one per consumer — without the SQL.
type appliers struct {
	cancel   context.CancelFunc
	stoppers []func()
	wg       sync.WaitGroup
	pulls    atomic.Int64 // pull requests (poll) or deliveries (persistent)
	errs     atomic.Int64
	loops    int
	applied  map[*holder]*atomic.Int64
	lat      samples

	// w and base are what awaitApplied measures against: every record w
	// wrote to a holder's logs since these loops started.
	w    *writer
	base map[*partLog]int64
}

func newAppliers(ctx context.Context, w *writer) (*appliers, context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	a := &appliers{cancel: cancel, applied: map[*holder]*atomic.Int64{}, w: w,
		base: map[*partLog]int64{}}
	if w != nil {
		for l, c := range w.counts {
			a.base[l] = c.Load()
		}
	}
	return a, ctx
}

// persistentAppliers applies through ONE long-lived pull per consumer — the
// client's Messages iterator, bounded by [statelog.FetchMessages] in flight
// and [statelog.FetchBytes] per request — which is what a fixed
// [js.DomainConsumer.Fetch] amounts to: a pull request re-issued at the
// client's expiry and threshold rather than twice a second, and a delivery the
// moment a record is stored rather than when a parked pull expires.
func (r *estateRun) persistentAppliers(ctx context.Context, holders []*holder,
	w *writer) *appliers {

	r.b.Helper()
	a, ctx := newAppliers(ctx, w)
	for _, hd := range holders {
		count := &atomic.Int64{}
		a.applied[hd] = count
		for l, name := range hd.consumers {
			cons, err := hd.q.JetStream().Consumer(ctx, l.spec.Name, name)
			if err != nil {
				r.b.Fatalf("look up %s: %v", name, err)
			}
			it, err := cons.Messages(jetstream.PullMaxMessagesWithBytesLimit(
				statelog.FetchMessages, statelog.FetchBytes))
			if err != nil {
				r.b.Fatalf("persistent pull on %s: %v", name, err)
			}
			a.stoppers = append(a.stoppers, it.Stop)
			a.loops++
			a.wg.Go(func() {
				for {
					msg, err := it.Next()
					if err != nil {
						if ctx.Err() != nil || errors.Is(err, jetstream.ErrMsgIteratorClosed) {
							return
						}
						a.errs.Add(1)
						continue
					}
					a.pulls.Add(1)
					if meta, err := msg.Metadata(); err == nil {
						a.lat.add(time.Since(meta.Timestamp))
					}
					_ = msg.Ack()
					count.Add(1)
				}
			})
		}
	}
	return a
}

func (a *appliers) stop() {
	a.cancel()
	for _, stop := range a.stoppers {
		stop()
	}
	a.wg.Wait()
}

// owed is every record written to a holder's logs since these loops started.
func (a *appliers) owed(hd *holder) int64 {
	var n int64
	for _, l := range hd.logs {
		n += a.w.counts[l].Load() - a.base[l]
	}
	return n
}

// awaitApplied waits until every holder has applied every record written to
// its logs.
func (a *appliers) awaitApplied(ctx context.Context, b *testing.B, holders []*holder) {
	b.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		behind := 0
		for _, hd := range holders {
			if a.applied[hd].Load() < a.owed(hd) {
				behind++
			}
		}
		if behind == 0 {
			return
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			b.Errorf("%d holders never applied what was written", behind)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// windowResult is what one phase cost.
type windowResult struct {
	before, after snapshot
	cores         float64
}

// window runs one steady-state phase — w appending at [steadyRate], or idle
// when w is nil — and logs what it cost.
func (r *estateRun) window(ctx context.Context, label string, d time.Duration, a *appliers,
	w *writer) windowResult {

	r.b.Helper()
	before := r.snapQuiet()
	pulls0, errs0 := a.pulls.Load(), a.errs.Load()
	a.lat.reset()
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	if w != nil {
		w.appended.reset()
		failed0, missed0 := w.failed.Load(), w.missed.Load()
		w.atRate(ctx, steadyRate)
		r.logf("%s: %d appended (%.1f/s), %d failed, %d ticks missed; conditional append %s",
			strings.ToUpper(label), w.appended.count(),
			float64(w.appended.count())/d.Seconds(), w.failed.Load()-failed0,
			w.missed.Load()-missed0, w.appended.summary())
		r.b.ReportMetric(w.appended.pct(0.5).Seconds()*1e3, "append-p50-ms")
		r.b.ReportMetric(w.appended.pct(0.99).Seconds()*1e3, "append-p99-ms")
	} else {
		<-ctx.Done()
	}
	after := r.snap(label)
	secs := after.at.Sub(before.at).Seconds()
	pulls := a.pulls.Load() - pulls0
	allocated := float64(after.alloc - before.alloc)
	res := windowResult{before: before, after: after, cores: cores(before, after)}
	r.logf("%s: CPU %.2f cores, %d loops, %.0f pulls-or-deliveries/s, allocating %.0f MiB/s "+
		"(%.0f KiB/s per loop), %d apply errors, apply latency %s",
		strings.ToUpper(label), res.cores, a.loops, float64(pulls)/secs,
		allocated/secs/(1<<20), allocated/secs/float64(max(a.loops, 1))/1024,
		a.errs.Load()-errs0, a.lat.summary())
	return res
}

// enginePull runs the runner's pull EXACTLY — [js.DomainConsumer.Fetch] with
// [statelog.FetchBytes], parked for [statelog.FetchWait] — on three consumers,
// the number a node applies today, idle, and reports what each pull allocates.
// It is the one number here that moves when the pull is fixed.
func (r *estateRun) enginePull(ctx context.Context, admin *js.Queue, logs []*partLog) {
	r.b.Helper()
	a, ctx := newAppliers(ctx, nil)
	for _, l := range logs[:3] {
		c, err := admin.DomainConsumer(ctx, l.spec.Name, "engine-pull", 0)
		if err != nil {
			r.b.Fatalf("engine pull on %s: %v", l.spec.Name, err)
		}
		a.loops++
		a.wg.Go(func() {
			for ctx.Err() == nil {
				msgs, err := c.Fetch(ctx, statelog.FetchMessages, statelog.FetchBytes,
					statelog.FetchWait)
				a.pulls.Add(1)
				if err != nil && ctx.Err() == nil {
					a.errs.Add(1)
					time.Sleep(statelog.ApplyRetryBeat)
				}
				for _, m := range msgs {
					_ = m.Ack()
				}
			}
		})
	}
	res := r.window(ctx, "engine pull (FetchBytes), 3 idle consumers", idleWindow, a, nil)
	a.stop()
	pulls := max(float64(a.pulls.Load()), 1)
	r.logf("  the runner's pull: %.0f KiB per pull",
		float64(res.after.alloc-res.before.alloc)/pulls/1024)
}

// throughput drains a backlog through fresh persistent appliers on every
// holder at once — a fleet catching up after an outage — and returns them
// running.
//
// IT WAITS OUT THE STOPPED LOOPS' PULLS FIRST. A stopped iterator's pull
// request stays live at the server until it expires, and a record published
// into it is delivered to nobody and held for the consumer's thirty-second
// ack window — measured here as a drain of 60,000 records taking 25 s instead
// of about one. That is a hazard for the fixed pull as much as for this
// benchmark: an applier stopped and restarted inside the expiry loses what
// arrives in between for the ack window.
func (r *estateRun) throughput(ctx context.Context, holders []*holder, w *writer) *appliers {
	r.b.Helper()
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(jetstream.DefaultExpires + time.Second):
	}
	before := map[*partLog]int64{}
	for l, c := range w.counts {
		before[l] = c.Load()
	}
	start := time.Now()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 64)
	for i := range backlogRecords {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			_ = w.appendOne(ctx, w.tracker[i%len(w.tracker)], nil)
		})
	}
	wg.Wait()
	loaded := time.Since(start)
	r.logf("BACKLOG %d records appended in %s (%.0f/s, 64 in flight), %d appends failed "+
		"in the whole run%s", backlogRecords, loaded.Round(time.Millisecond),
		float64(backlogRecords)/loaded.Seconds(), w.failed.Load(), firstError(w))

	// THE BASE IS THE BACKLOG'S START, so what each holder owes is exactly
	// the backlog on its logs — and the clock starts before the appliers
	// do, because a holder's first logs are draining while its last are
	// still being looked up.
	start = time.Now()
	a := r.persistentAppliers(ctx, holders, w)
	a.base = before
	took := map[*holder]time.Duration{}
	var total int64
	for len(took) < len(holders) && time.Since(start) < 5*time.Minute {
		for _, hd := range holders {
			if _, done := took[hd]; !done && a.applied[hd].Load() >= a.owed(hd) {
				took[hd] = time.Since(start)
				total += a.owed(hd)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	wall := time.Since(start)
	var rates []float64
	for hd, d := range took {
		rates = append(rates, float64(a.owed(hd))/d.Seconds())
	}
	if len(took) < len(holders) || len(rates) == 0 {
		r.b.Errorf("%d of %d holders did not drain in %s", len(holders)-len(took),
			len(holders), wall)
		return a
	}
	slices.Sort(rates)
	r.logf("  every holder at once: %d records in %s, %.0f records/s aggregate, per holder "+
		"min %.0f / median %.0f / max %.0f records/s", total, wall.Round(time.Millisecond),
		float64(total)/wall.Seconds(), rates[0], rates[len(rates)/2], rates[len(rates)-1])
	return a
}

// raftObject is one replicated object whose leadership the election phase
// follows: a stream, or a consumer on one.
type raftObject struct {
	stream, consumer string
	// before is the leader before the restart and after the one the last
	// inspection saw, which is how a move is counted.
	before, after string
}

// restart takes one member down and brings it straight back, as a rolling
// restart does, and measures what the fleet then has to do: how long the
// member takes to rejoin, how many leaders moved, and how long until every
// stream and every replicated consumer has a leader with every replica current
// — against statelog.StallGrace, the contract's bound. An R = 1 applier on the
// restarted member counts as settled when it answers again.
func (r *estateRun) restart(ctx context.Context, logs []*partLog, holders []*holder) {
	r.b.Helper()
	inspect, err := r.cluster.Servers[0].Client(ctx)
	if err != nil {
		r.b.Fatalf("a client of member 0 to inspect the fleet: %v", err)
	}
	r.b.Cleanup(func() { _ = inspect.Stop(context.WithoutCancel(ctx)) })
	var objects []*raftObject
	for _, l := range logs {
		objects = append(objects, &raftObject{stream: l.spec.Name})
		if l.wake {
			objects = append(objects, &raftObject{stream: l.spec.Name,
				consumer: wakeConsumerName(ctx, inspect, l)})
		}
	}
	for _, hd := range holders {
		for l, name := range hd.consumers {
			objects = append(objects, &raftObject{stream: l.spec.Name, consumer: name})
		}
	}
	unsettled := r.unsettled(ctx, inspect, objects, true)
	r.logf("RESTART: %d objects before the restart, %d not settled", len(objects), len(unsettled))

	before := r.snapQuiet()
	start := time.Now()
	cfg := r.cluster.Configs[restartedMember]
	r.cluster.Servers[restartedMember].Shutdown()
	srv, err := js.StartServer(ctx, cfg)
	if err != nil {
		r.b.Fatalf("restart member %d: %v", restartedMember, err)
	}
	r.b.Cleanup(srv.Shutdown)
	r.cluster.Servers[restartedMember] = srv
	rejoined := time.Since(start)

	pending := objects
	for len(pending) > 0 && time.Since(start) < settleCeiling {
		pending = r.unsettled(ctx, inspect, pending, false)
		if len(pending) > 0 {
			time.Sleep(time.Second)
		}
	}
	settled := time.Since(start)
	after := r.snap("after a member restart")
	moved := 0
	for _, o := range objects {
		if o.before != "" && o.after != "" && o.before != o.after {
			moved++
		}
	}
	verdict := "within"
	if len(pending) > 0 || settled > statelog.StallGrace {
		verdict = "NOT within"
	}
	r.logf("RESTART of member %d: rejoined in %s, every object settled in %s (%s "+
		"statelog.StallGrace, %s), %d of %d leaders moved, %d still unsettled, CPU %.2f cores",
		restartedMember, rejoined.Round(time.Millisecond), settled.Round(time.Millisecond),
		verdict, statelog.StallGrace, moved, len(objects), len(pending), cores(before, after))
	for i, o := range pending {
		if i == 3 {
			break
		}
		r.logf("  unsettled: %s %s", o.stream, o.consumer)
	}
	r.b.ReportMetric(settled.Seconds(), "restart-settle-s")
}

// unsettled inspects objects and returns the ones without a leader, with a
// replica that is not current, or — for an R = 1 consumer — that do not
// answer. first records each object's leader as the one before the restart;
// every pass records the latest, so the two say which moved.
func (r *estateRun) unsettled(ctx context.Context, q *js.Queue, objects []*raftObject,
	first bool) []*raftObject {

	var mu sync.Mutex
	var out []*raftObject
	sem := make(chan struct{}, 32)
	var wg sync.WaitGroup
	for _, o := range objects {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			askCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			leader, settled := inspectObject(askCtx, q, o)
			mu.Lock()
			defer mu.Unlock()
			if first {
				o.before = leader
			}
			if leader != "" {
				o.after = leader
			}
			if !settled {
				out = append(out, o)
			}
		})
	}
	wg.Wait()
	return out
}

// inspectObject reads one object's cluster state and judges it settled.
func inspectObject(ctx context.Context, q *js.Queue, o *raftObject) (string, bool) {
	stream, err := q.JetStream().Stream(ctx, o.stream)
	if err != nil {
		return "", false
	}
	var cluster *jetstream.ClusterInfo
	if o.consumer == "" {
		info, err := stream.Info(ctx)
		if err != nil {
			return "", false
		}
		cluster = info.Cluster
	} else {
		cons, err := stream.Consumer(ctx, o.consumer)
		if err != nil {
			return "", false
		}
		info, err := cons.Info(ctx)
		if err != nil {
			return "", false
		}
		cluster = info.Cluster
	}
	if cluster == nil || cluster.Leader == "" {
		return "", false
	}
	for _, peer := range cluster.Replicas {
		if !peer.Current || peer.Offline {
			return cluster.Leader, false
		}
	}
	return cluster.Leader, true
}

// wakeConsumerName is the durable name of a log's wake feed, read back from
// the broker: the group's name is the queue's to derive.
func wakeConsumerName(ctx context.Context, q *js.Queue, l *partLog) string {
	stream, err := q.JetStream().Stream(ctx, l.spec.Name)
	if err != nil {
		return ""
	}
	names := stream.ConsumerNames(ctx)
	for name := range names.Name() {
		if strings.HasPrefix(name, "wake__") {
			return name
		}
	}
	return ""
}

// probe compares a member's own client with a leaf's, on the same logs:
// one conditional append and one single-record pull each, alternating.
func (r *estateRun) probe(ctx context.Context, member, leaf *js.Queue, w *writer) {

	r.b.Helper()
	var memberAppend, leafAppend, memberFetch, leafFetch samples
	var errs []error
	tracked := w.tracker[:min(probeLogs, len(w.tracker))]
	for i, p := range tracked {
		logs := map[string]*js.DomainLog{}
		cons := map[string]*js.DomainConsumer{}
		for name, q := range map[string]*js.Queue{"member": member, "leaf": leaf} {
			l, err := q.DomainLog(ctx, p.spec.Name)
			if err != nil {
				r.b.Fatalf("probe %s log on %s: %v", name, p.spec.Name, err)
			}
			end, err := l.End(ctx)
			if err != nil {
				r.b.Fatalf("probe %s end of %s: %v", name, p.spec.Name, err)
			}
			c, err := q.DomainConsumer(ctx, p.spec.Name, "probe-"+name, end)
			if err != nil {
				r.b.Fatalf("probe %s consumer on %s: %v", name, p.spec.Name, err)
			}
			logs[name], cons[name] = l, c
		}
		for s := range probeSamples {
			order := []string{"member", "leaf"}
			if (i+s)%2 == 1 {
				order = []string{"leaf", "member"}
			}
			for _, name := range order {
				subject := fmt.Sprintf("%s.probe.%s", p.prefix, name)
				t0 := time.Now()
				_, _, err := logs[name].Append(ctx, subject, "", nil, payload)
				d := time.Since(t0)
				if err != nil {
					errs = append(errs, err)
					continue
				}
				if name == "member" {
					memberAppend.add(d)
				} else {
					leafAppend.add(d)
				}
			}
			for _, name := range order {
				for range 2 { // both appends above land on both probes
					t0 := time.Now()
					msgs, err := cons[name].Fetch(ctx, 1, 0, 2*time.Second)
					d := time.Since(t0)
					if err != nil || len(msgs) != 1 {
						errs = append(errs, fmt.Errorf("probe fetch %s: %d records, %w",
							name, len(msgs), err))
						continue
					}
					_ = msgs[0].Ack()
					if name == "member" {
						memberFetch.add(d)
					} else {
						leafFetch.add(d)
					}
				}
			}
		}
	}
	r.logf("LEAF VS MEMBER over %d logs: %d errors", len(tracked), len(errs))
	r.logf("  append, member client %s", memberAppend.summary())
	r.logf("  append, leaf client   %s", leafAppend.summary())
	r.logf("  1-record pull, member %s", memberFetch.summary())
	r.logf("  1-record pull, leaf   %s", leafFetch.summary())
	for i, err := range errs {
		if i == 3 {
			break
		}
		r.logf("  probe error: %v", err)
	}
	r.b.ReportMetric(leafFetch.pct(0.5).Seconds()*1e3, "leaf-pull-p50-ms")
	r.b.ReportMetric(memberFetch.pct(0.5).Seconds()*1e3, "member-pull-p50-ms")
}

// seatNode is one stateless node: a leaf broker and the lease store over it.
type seatNode struct {
	id    string
	store *kv.Store
	owner string

	mu    sync.Mutex
	seats []coord.Lease
}

// coordination joins the shape's seat nodes as leaves and runs the two loops
// every node runs against the coordination store — its presence claim and the
// renewal of every seat it holds at [seat.HeartbeatInterval], the membership
// listing at [seat.SweepInterval] — first with no seats, then while the fleet
// claims its seats from cold, then holding them.
func (r *estateRun) coordination(ctx context.Context, a *appliers) {
	r.b.Helper()
	start := time.Now()
	nodes := make([]*seatNode, r.shape.seatNodes)
	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup
	var failed atomic.Int64
	for i := range nodes {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			id := fmt.Sprintf("seatnode-%03d", i)
			q, err := r.startLeaf(ctx, id)
			if err != nil {
				failed.Add(1)
				r.b.Logf("seat node %s: %v", id, err)
				return
			}
			store, err := openLeases(ctx, q)
			if err != nil {
				failed.Add(1)
				r.b.Logf("seat node %s leases: %v", id, err)
				return
			}
			nodes[i] = &seatNode{id: id, store: store, owner: id + "/" + uuid.NewString()}
		})
	}
	wg.Wait()
	if failed.Load() > 0 {
		r.b.Fatalf("%d seat nodes failed to join", failed.Load())
	}
	r.logf("%d seat nodes joined as leaves in %s", len(nodes),
		time.Since(start).Round(time.Millisecond))
	r.window(ctx, "seat nodes connected, idle", idleWindow, a, nil)

	f := startFleet(ctx, nodes)
	defer f.stop()
	r.fleetWindow(ctx, "presence + listing", coordWindow, f, nil)
	if r.shape.seats == 0 {
		return
	}

	// A GATED CLAIM SCANS EVERY LEASE IN BOTH BUCKETS (kv.Store.readForClaim),
	// so its cost is a function of the seats already held and it is measured
	// at both ends rather than by claiming ten thousand of them that way: a
	// hundred nodes doing that at once drove this process past 10 GB and the
	// members' raft groups out of quorum on four cores before it finished.
	// The seats are SEEDED ungated in between — one key read per claim — and
	// what the steady phase measures is holding them, which does not depend
	// on how they were claimed.
	r.logf("  gated seat claim with %d leases held: %s", len(nodes),
		gatedClaims(ctx, nodes[0], 50))
	var seeded, refused atomic.Int64
	var firstErr atomic.Pointer[error]
	r.fleetWindow(ctx, fmt.Sprintf("seeding %d seats, ungated", r.shape.seats*len(nodes)), 0, f,
		func() {
			for _, n := range nodes {
				wg.Go(func() {
					for range r.shape.seats {
						lease, _, err := n.store.TryAcquire(ctx, coord.ClassSeat.Resource(uuid.NewString()),
							coord.AcquireOptions{Owner: n.owner, TTL: seat.SeatLeaseTTL,
								Preferred: n.id, Ungated: true})
						switch {
						case err != nil:
							failed.Add(1)
							firstErr.CompareAndSwap(nil, &err)
							continue
						case lease == nil:
							refused.Add(1)
							continue
						}
						seeded.Add(1)
						n.mu.Lock()
						n.seats = append(n.seats, *lease)
						n.mu.Unlock()
					}
				})
			}
			wg.Wait()
		})
	detail := ""
	if err := firstErr.Load(); err != nil {
		detail = fmt.Sprintf(" (first: %v)", *err)
	}
	r.logf("  %d seeded, %d failed%s, %d refused as held", seeded.Load(), failed.Load(),
		detail, refused.Load())
	r.logf("  gated seat claim with %d leases held: %s", int(seeded.Load())+len(nodes),
		gatedClaims(ctx, nodes[0], 50))
	r.fleetWindow(ctx, fmt.Sprintf("presence + listing + %d seats held", seeded.Load()),
		coordWindow, f, nil)
}

// gatedClaims makes n seat claims in a row the way a seat host does — gated,
// so each one scans the lease buckets — and summarises what each cost. The
// seats are released again so the fleet's lease count is unchanged.
func gatedClaims(ctx context.Context, n *seatNode, count int) string {
	var took samples
	failed := 0
	for range count {
		resource := coord.ClassSeat.Resource(uuid.NewString())
		t0 := time.Now()
		lease, _, err := n.store.TryAcquire(ctx, resource,
			coord.AcquireOptions{Owner: n.owner, TTL: seat.SeatLeaseTTL, Preferred: n.id})
		if err != nil || lease == nil {
			failed++
			continue
		}
		took.add(time.Since(t0))
		_, _ = n.store.Release(ctx, resource, n.owner, lease.Epoch)
	}
	return fmt.Sprintf("%s, %d failed", took.summary(), failed)
}

// fleet is every seat node's heartbeat and sweep, running until stopped.
type fleet struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup

	presence, renew, list samples
	errs, lost, listed    atomic.Int64
}

// startFleet starts every node's loops, each at a random phase so the fleet
// does not beat in step.
func startFleet(ctx context.Context, nodes []*seatNode) *fleet {
	ctx, cancel := context.WithCancel(ctx)
	f := &fleet{cancel: cancel}
	every := func(period time.Duration, beat func()) {
		f.wg.Go(func() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(rand.N(period)):
			}
			tick := time.NewTicker(period)
			defer tick.Stop()
			for {
				beat()
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
			}
		})
	}
	for _, n := range nodes {
		every(seat.HeartbeatInterval, func() {
			t0 := time.Now()
			if _, _, err := n.store.TryAcquire(ctx, coord.NodeResource(n.id), coord.AcquireOptions{
				Owner: n.owner, TTL: seat.SeatLeaseTTL, Preferred: n.id, Ungated: true,
				Meta: map[string]any{"roles": []string{"agent"}},
			}); err != nil {
				if ctx.Err() == nil {
					f.errs.Add(1)
				}
			} else {
				f.presence.add(time.Since(t0))
			}
			n.mu.Lock()
			held := slices.Clone(n.seats)
			n.mu.Unlock()
			for _, l := range held {
				t0 := time.Now()
				alive, err := n.store.Renew(ctx, l.Resource, n.owner, l.Epoch, seat.SeatLeaseTTL)
				switch {
				case ctx.Err() != nil:
					return
				case err != nil:
					f.errs.Add(1)
				case !alive:
					f.lost.Add(1)
				default:
					f.renew.add(time.Since(t0))
				}
			}
		})
		every(seat.SweepInterval, func() {
			t0 := time.Now()
			leases, err := n.store.ListLive(ctx, coord.ClassNode)
			if err != nil {
				if ctx.Err() == nil {
					f.errs.Add(1)
				}
				return
			}
			f.list.add(time.Since(t0))
			f.listed.Store(int64(len(leases)))
		})
	}
	return f
}

func (f *fleet) stop() {
	f.cancel()
	f.wg.Wait()
}

// fleetWindow measures the fleet's loops for d — or for as long as work runs,
// when work is given — and logs what they cost.
func (r *estateRun) fleetWindow(ctx context.Context, label string, d time.Duration, f *fleet, work func()) {
	r.b.Helper()
	f.presence.reset()
	f.renew.reset()
	f.list.reset()
	errs0, lost0 := f.errs.Load(), f.lost.Load()
	before := r.snapQuiet()
	if work != nil {
		work()
	} else {
		select {
		case <-ctx.Done():
		case <-time.After(d):
		}
	}
	after := r.snap(label)
	secs := after.at.Sub(before.at).Seconds()
	ops := f.presence.count() + f.renew.count() + f.list.count()
	r.logf("%s: %s, CPU %.2f cores, %.0f heartbeat/sweep ops/s, %d errors, %d leases lost, "+
		"the last listing saw %d nodes", strings.ToUpper(label),
		after.at.Sub(before.at).Round(time.Millisecond), cores(before, after),
		float64(ops)/secs, f.errs.Load()-errs0, f.lost.Load()-lost0, f.listed.Load())
	r.logf("  presence claim %s", f.presence.summary())
	r.logf("  seat renew     %s", f.renew.summary())
	r.logf("  node listing   %s", f.list.summary())
}

// snapshot is the process and the members' stores at one instant.
type snapshot struct {
	label      string
	at         time.Time
	goroutines int
	heapInuse  uint64
	alloc      uint64
	sys        uint64
	rss        int64
	fds        int
	cpu        time.Duration
	raft       []int
	disk       []int64
}

func (r *estateRun) snap(label string) snapshot {
	s := r.snapQuiet()
	s.label = label
	r.snaps = append(r.snaps, s)
	r.logf("SNAPSHOT %-44s goroutines %6d  heap %6.0f MiB  sys %6.0f MiB  rss %6.0f MiB  "+
		"fds %5d  raft groups/member %v  disk/member %v MiB  host load %s", label, s.goroutines,
		mib(int64(s.heapInuse)), mib(int64(s.sys)), mib(s.rss), s.fds, s.raft, mibs(s.disk),
		loadAverage())
	return s
}

func (r *estateRun) snapQuiet() snapshot {
	// TWICE, so a buffer the broker returned to a sync.Pool — an expired
	// block cache, most of all — has left the victim cache too, and the heap
	// figure is what is live rather than what is pooled.
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	s := snapshot{at: time.Now(), goroutines: runtime.NumGoroutine(),
		heapInuse: ms.HeapInuse, alloc: ms.TotalAlloc, sys: ms.Sys, rss: rss(), fds: openFDs(), cpu: cpuTime()}
	for _, cfg := range r.cluster.Configs {
		s.raft = append(s.raft, raftGroups(cfg.StoreDir))
		s.disk = append(s.disk, diskUsage(cfg.StoreDir))
	}
	return s
}

// raftGroups counts the raft groups a member's store holds: one directory per
// group under the system account's `_js_`, the metadata group included.
func raftGroups(storeDir string) int {
	entries, err := os.ReadDir(filepath.Join(storeDir, "jetstream", "$SYS", "_js_"))
	if err != nil {
		return -1
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			n++
		}
	}
	return n
}

// diskUsage is what a directory occupies on its volume — allocated blocks,
// not apparent sizes, since a store's files are sparse where they are sized
// ahead of their contents.
func diskUsage(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // a file removed mid-walk is not a failure of the walk
		}
		info, err := d.Info()
		if err != nil {
			return nil //nolint:nilerr // likewise
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			total += st.Blocks * 512
		}
		return nil
	})
	return total
}

func rss() int64 {
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return -1
	}
	for line := range strings.Lines(string(raw)) {
		if rest, ok := strings.CutPrefix(line, "VmRSS:"); ok {
			var kib int64
			_, _ = fmt.Sscanf(strings.TrimSpace(rest), "%d", &kib)
			return kib << 10
		}
	}
	return -1
}

// loadAverage is the host's one-minute load, which says whether anything
// OUTSIDE this process was competing for the CPUs a phase was measured on.
func loadAverage() string {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return "?"
	}
	first, _, _ := strings.Cut(string(raw), " ")
	return first
}

func openFDs() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	return len(entries)
}

func cpuTime() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// cores is the CPU the process burned between two snapshots, in cores.
func cores(before, after snapshot) float64 {
	wall := after.at.Sub(before.at)
	if wall <= 0 {
		return 0
	}
	return float64(after.cpu-before.cpu) / float64(wall)
}

// samples is a set of latencies.
type samples struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *samples) add(d time.Duration) {
	s.mu.Lock()
	s.d = append(s.d, d)
	s.mu.Unlock()
}

func (s *samples) reset() {
	s.mu.Lock()
	s.d = nil
	s.mu.Unlock()
}

func (s *samples) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.d)
}

// pct is the q-quantile, nearest rank; zero with no samples.
func (s *samples) pct(q float64) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.d) == 0 {
		return 0
	}
	sorted := slices.Clone(s.d)
	slices.Sort(sorted)
	i := min(int(q*float64(len(sorted))), len(sorted)-1)
	return sorted[i]
}

func (s *samples) summary() string {
	n := s.count()
	if n == 0 {
		return "(no samples)"
	}
	s.mu.Lock()
	top := slices.Max(s.d)
	s.mu.Unlock()
	return fmt.Sprintf("n=%d p50 %s p90 %s p99 %s max %s", n,
		round(s.pct(0.5)), round(s.pct(0.9)), round(s.pct(0.99)), round(top))
}

func round(d time.Duration) time.Duration {
	switch {
	case d >= time.Second:
		return d.Round(time.Millisecond)
	case d >= time.Millisecond:
		return d.Round(10 * time.Microsecond)
	default:
		return d.Round(time.Microsecond)
	}
}

func mib(n int64) float64 { return float64(n) / (1 << 20) }

func mibs(ns []int64) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = fmt.Sprintf("%.1f", mib(n))
	}
	return out
}

func sum[T int | int64](xs []T) T {
	var t T
	for _, x := range xs {
		t += x
	}
	return t
}
