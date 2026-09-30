package engine

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The layouts this build knows how to describe, and the partition counts a
// NEW deployment's first partitioned layout is given.
//
// They live here because the ENGINE composes the domains (registeredDomains,
// beside them in statelog.go), so the layouts' shapes live where the
// composition does. statelog stays a framework that knows the Layout TYPE and
// no product's space sizes, and config stays unimported by the framework.
//
// # Counts are recorded, not assumed
//
// The constants are defaults for a deployment's FIRST partitioned layout.
// The layout actually in force is the one recorded in the estate map and in
// every partition file's cursor rows: a node whose build defaults differ
// still runs the recorded layout, and a build whose default changes never
// repartitions an existing deployment. Changing a count is a repartition — a
// new layout, drained and split — never an edit, because a partition is a
// hash of an object's key modulo the count.
//
// # What each tracker count costs the broker
//
// The figure the NATS benchmark must match. With T the tracker count:
//
//	                                            T = 64            T = 256
//	partition logs: 2T (tracker + vectors)
//	  + 2·T/4 (pages + vectors) + 1 (company)   161               641
//	durable consumers, each at its stream's
//	  replicas: a wake feed per tracker,
//	  pages and company log (T + T/4 + 1),
//	  plus an applier per holder per log        81 + 483 = 564    321 + 1,923 = 2,244
//	fixed: engine streams (6) + coordination
//	  buckets (19 + the estate map's)           26                26
//	files on a node holding every partition
//	  (a fleet of at most R data nodes)         1 node + 81       1 node + 321
//
// What was measured (jetstreamtest.BenchmarkPartitionedEstate, 4 vCPU, one
// process holding every member, holder and leaf) were an earlier design's
// shapes, 129 and 513 streams: 64 or 256 search partitions plus one pages log,
// with no pages split and no company log. Both provisioned inside the
// clustered sequence budget with no failed create (17–25 s at 129, 21–64 s at
// 513), and every axis cost scaled linearly with the stream count — per
// partition across three members, 12 raft instances, 66 goroutines, about
// 1.1 MiB of cold heap and 0.1 ms/s of heartbeat CPU per raft instance. The
// 513-stream point is therefore the one 256 was judged by, and its idle cost
// is only affordable with the state-log pull answered on one standing inbox
// (the per-pull poll measured 1.3–3 cores at 1,539 consumers). At T = 256 the
// 641 here is a quarter beyond the measured 513, so the activation step
// measures the 641-stream point with these consumer counts before it merges,
// rather than extrapolating it.
//
// # Every applier at its stream's replicas
//
// An applier's consumer names no replica count of its own
// (internal/queue/jetstream's domain consumer), so it takes its stream's, and
// the table counts it there. An applier at consumer replicas 1 would take two
// of its three raft replicas off the broker for every holder of every log —
// 3,846 of the 8,733 replicas the table counts at T = 256 — and it is NOT
// applied: the broker places an R1 consumer on one of its stream's peers at
// random, so restarting that one member stalls every applier whose consumer
// sits there until it is back, where at the stream's replicas the consumer
// fails over with it. Whether that stall is worth the replicas it saves is
// measured at the 641-stream point before the partitioned estate activates;
// until then every applier keeps its stream's replica count. Counted at R = 1
// here, the table promised a broker the engine does not build.
//
// # The rule for choosing
//
// The owner applies it to the measurements:
//
//  1. Broker (hard). At 3 members and stream.replicas 3, every stream and
//     consumer in the column is provisioned within jsprovision's clustered
//     sequence budget; idle member CPU is at most 25% of an 8-vCPU node and
//     RSS at most 25% of 32 GiB; and a leader-election storm after a member
//     restart settles within statelog.StallGrace (60 s).
//  2. Node footprint (hard). A node holding every partition, at the count in
//     the table's last row, boots within config.DefaultRejoinWindow (30 min),
//     with its idle RSS and open file descriptors within the same 25%.
//  3. Balance (soft). Each holder carries at least 8 partitions of a space
//     (R·T/D ≥ 8), so count-balance is within about 12%. That needs
//     T ≥ 8D/3: T = 64 serves up to 24 data nodes and T = 256 up to 96.
//  4. Size (soft). At the owner's target size and horizon, one tracker
//     partition's bytes × 4.2 is at most an eighth of a holder's disk. At
//     10,000 seats in year 5 that is 3.9 TB ÷ T × 4.2: 257 GB at T = 64 and
//     64 GB at T = 256.
//
// Take the largest candidate that passes 1 and 2. If 256 fails either, take
// 64. If 64 fails either, the design needs a narrower stream shape, and that
// finding goes to the owner rather than into the code.
const (
	// DefaultTrackerPartitions is how many partitions the tracker space of
	// a NEW deployment's first partitioned layout gets: 256, the larger
	// candidate, which the owner chose because the engine is built to scale
	// far and the count cannot be changed without a drained repartition.
	// It passed rule 1 at the measured 513-stream point; by rule 3 it keeps
	// holders balanced up to 96 data nodes where 64 would stop at 24, and by
	// rule 4 it keeps a tracker partition near 64 GB at 10,000 seats in year
	// five where 64 would put it at 257 GB. The price is four times the
	// broker cost of 64 on every axis, which is why a fleet at this count
	// runs five broker members rather than three (1,747 raft replicas per
	// member — the table's 2,911 streams and consumers at three replicas
	// each, over five — against 2,082 at the measured point), and why rule
	// 2 — a single
	// node holding all 321 partition files — is measured before activation.
	DefaultTrackerPartitions = 256

	// DefaultPagesPartitions is a QUARTER of the tracker's, borrowed from
	// config.DerivedPagesLogDivisor: the knowledge base's log grows at about
	// a quarter of the tracker's rate, so at a quarter of the count a pages
	// partition's log grows at the same rate as a tracker partition's, and
	// every partition log is sized alike.
	DefaultPagesPartitions = DefaultTrackerPartitions / config.DerivedPagesLogDivisor

	// CompanyPartitions is one by construction: the company space holds the
	// objects EVERY tracker partition must see in order, and those have one
	// authoritative source.
	CompanyPartitions = 1
)

// LayoutZero is today's estate as a layout: one space, estate, with one
// partition carrying every registered domain, in the register's order, under
// the stream names and register keys they have always had.
//
// Built FROM the register rather than from a second list of names, so a
// domain registered is a domain layout 0 carries and the two orders cannot
// disagree.
func LayoutZero() statelog.Layout {
	return statelog.EstateLayout(registeredNames()...)
}

// layout is the layout this node runs: its state log's, or [LayoutZero] on a
// node running none.
func (e *Engine) layout() statelog.Layout {
	if n := e.native.Load(); n != nil && n.log != nil {
		return n.log.layout
	}
	return LayoutZero()
}

// domainEstate is layout 0's one partition as this node's DOMAIN-LEVEL
// consumers read it — a seat's tracker search, the search index and its
// coverage — which is where [stateLog.Domain]'s logs are, while those
// consumers key nothing to a partition. A node holding no such partition
// answers [store.ErrNoEstate] through it.
//
// A READ handle, because every one of those consumers only reads: what may
// write a partition is the short list internal/store's applier gate reads,
// and a domain-level accessor answering the write handle would have put every
// caller of it on that list.
func (e *Engine) domainEstate() store.PartitionReader {
	if e.backends == nil || e.backends.Store == nil {
		return store.PartitionReader{}
	}
	return e.backends.Store.PartitionHandle(statelog.EstatePartition.String()).Reader()
}

// estateLog is a registered domain's one log under [LayoutZero], and
// estateSpec its stream — named as it has always been, at the domain's whole
// declared budget.
func estateLog(domain statelog.Domain) statelog.LogID {
	return statelog.LogID{Domain: domain.Name(), Partition: statelog.EstatePartition}
}

func estateSpec(domain statelog.Domain) statelog.StreamSpec {
	return LayoutZero().StreamSpec(domain, estateLog(domain))
}

// DefaultLayoutOne is the first partitioned layout at the default counts:
// the tracker space carrying the tracker's log and its vectors, the pages
// space carrying the pages' log and theirs — vectors co-located with the
// documents they embed, so a partition's embedding selection stays one
// statement over one file — and the company space carrying the tracker's
// company-wide objects in one partition.
func DefaultLayoutOne() statelog.Layout {
	trackerLog, vectors, pagesLog := tracker.Domain{}.Name(), search.Domain{}.Name(), pages.Domain{}.Name()
	return statelog.Layout{Number: 1, Spaces: []statelog.SpaceLayout{
		{Space: statelog.SpaceTracker, Partitions: DefaultTrackerPartitions, Domains: []string{trackerLog, vectors}},
		{Space: statelog.SpacePages, Partitions: DefaultPagesPartitions, Domains: []string{pagesLog, vectors}},
		{Space: statelog.SpaceCompany, Partitions: CompanyPartitions, Domains: []string{trackerLog}},
	}}
}
