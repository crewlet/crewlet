package engine

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
)

// WHAT THIS NODE'S OWN HARDWARE IS DOING, which three alarms read.
//
// `volume_low`, `wal_large` and `pool_starved` are the three conditions in the
// table that are about the MACHINE rather than about the log, and each reads a
// field of [statelog.Reading] measured from this node's own storage: the sizes
// by [retention.space], the pool wait from the histogram [retention.poolWait]
// feeds. Each fails in the same quiet way: a volume fills and the next backup,
// vacuum or peer join dies partway through; a write-ahead log a checkpoint
// cannot pass grows until it takes the volume with it; a reader pool too small
// for the node's domains makes every query slower with nothing naming the
// queue in front of it.
//
// # Why the sizes are measured per read and the pool wait per tick
//
// A size is a LEVEL: four `os.Stat` calls and one `statfs`, microseconds
// apiece, and correct whenever they are taken. So the reading measures them
// directly and the tick publishes the same numbers as gauges.
//
// A wait is a DELTA: `sql.DBStats` counts since the process started, and the
// interesting quantity is how long a caller queued RECENTLY. Consuming a delta
// is destructive — whoever reads it clears it for everyone after — so it is
// taken on the tick alone, at a cadence that is regular by construction, and
// never on a request whose frequency an operator's dashboard decides.

// capacity records what one tick can measure about this node's own storage.
func (r *retention) capacity(ctx context.Context) {
	if r.metrics == nil || r.db == nil {
		return
	}
	for _, db := range storeFiles(r.db) {
		file := filepath.Base(db.Path())
		at := metrics.Attrs{"file": file}
		if size, err := fileBytes(db.Path()); err == nil {
			r.metrics.Set(metrics.StoreBytes, float64(size), at)
		}
		if wal, err := fileBytes(db.Path() + walSuffix); err == nil {
			r.metrics.Set(metrics.StoreWalBytes, float64(wal), at)
		}
		r.poolWait(db, file)
	}
	r.backupGauges(ctx)
}

// backupGauges publishes how old this node's newest artefact is and how many
// trim holds the fleet is carrying.
//
// # Why the age is read off the disk and not off the register
//
// The register says a backup was TAKEN. The directory says one EXISTS, and the
// two differ in exactly the cases the alarm is for — a copy that was taken and
// then deleted, a volume that was never mounted, a schedule pointing at a path
// nobody ships from. In every one of those the register reports a protected
// fleet and the disk reports an unprotected one.
//
// So this walks the register for the points THIS NODE announced — the only
// artefacts whose directory is reachable from this process — and reads their
// manifests. A node that has never taken a backup publishes nothing rather
// than zero: an age of zero is the freshest backup imaginable, and it is the
// one value a fleet with no backup at all must never report.
//
// The HOLDS are fleet-wide and read here rather than on the trim's own tick
// because the trim is a singleton: a gauge published only by the lease holder
// disappears from a collector every time the lease moves, which reads as a
// node that stopped reporting.
func (r *retention) backupGauges(ctx context.Context) {
	if r.fleet == nil {
		return
	}
	if holds, err := r.fleet.Holds(ctx); err == nil {
		// A COUNT THAT DOES NOT RETURN TO ZERO IS A BACKUP THAT CRASHED
		// MID-COPY. The pin outlives its owner until the stale bound
		// expires it, and until then the trim does not advance — which
		// has no other symptom at all until the log walks into its
		// ceiling.
		r.metrics.Set(metrics.BackupHolds, float64(len(holds)), nil)
	}
	points, err := r.fleet.BackupPoints(ctx)
	if err != nil {
		return
	}
	newest, found := time.Time{}, false
	for _, point := range points {
		if point.Owner != r.nodeID || point.Dir == "" {
			continue
		}
		at, ok, err := backup.Newest(point.Dir)
		if err != nil {
			r.logs().WarnContext(ctx, "backup_age_unreadable", "dir", point.Dir,
				"err", err)
			continue
		}
		if ok && at.After(newest) {
			newest, found = at, true
		}
	}
	if found {
		r.metrics.Set(metrics.BackupAge,
			max(time.Since(newest).Seconds(), 0), nil)
	}
}

// poolWait observes how long a caller queued for a connection since the last
// tick.
//
// THE MEAN OF THE INTERVAL, one observation per tick. `sql.DBStats` gives a
// count and a total and not the individual waits, so the honest reading of a
// tick in which twelve callers queued for three seconds between them is "a
// quarter of a second each" — and the p95 the alarm takes over a day is then
// the fifth-worst tick, which is the quantity an operator acts on. A total
// divided by nothing is skipped rather than recorded as zero: a tick in which
// nobody queued is not a tick in which somebody queued for no time.
func (r *retention) poolWait(db *store.DB, file string) {
	stats := db.SQL().Stats()
	r.mu.Lock()
	previous := r.pooled[file]
	r.pooled[file] = poolCounters{count: stats.WaitCount, waited: stats.WaitDuration}
	r.mu.Unlock()

	count := stats.WaitCount - previous.count
	waited := stats.WaitDuration - previous.waited
	if count <= 0 || waited <= 0 {
		return
	}
	r.metrics.Observe(metrics.StorePoolWait, waited/time.Duration(count),
		metrics.Attrs{"file": file})
}

// walSuffix is the sidecar a database in WAL mode keeps its uncheckpointed
// pages in. Named here rather than reached for from `internal/store`, whose
// own list is unexported and is about REMOVING sidecars from a copy.
const walSuffix = "-wal"

// storeFiles is every database file this node holds, in a stable order.
func storeFiles(db *store.DB) []*store.DB {
	if db == nil {
		return nil
	}
	out := []*store.DB{db}
	if peer := db.Replicated(); peer != nil && peer != db {
		out = append(out, peer)
	}
	return out
}

// fileBytes is a file's size, and zero with no error when it does not exist.
//
// A MISSING -wal IS ZERO BYTES, not a failure: a database with nothing
// uncheckpointed has no sidecar at all, which is the healthiest state the
// `wal_large` alarm has and must not read as unmeasurable.
func fileBytes(path string) (int64, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// space fills the three storage fields of a reading.
//
// MEASURED RATHER THAN READ BACK OFF THE GAUGES. The gauges are published on
// the trim's own tick, which is a quarter of an hour; a volume fills in less
// time than that, and an alarm evaluated against a fifteen-minute-old free
// count is one that reports the space that was there before the thing that
// used it.
func (r *retention) space(out *statelog.Reading) {
	if r.db == nil {
		return
	}
	for _, db := range storeFiles(r.db) {
		if size, err := fileBytes(db.Path()); err == nil {
			out.StoreBytes += size
		}
		if wal, err := fileBytes(db.Path() + walSuffix); err == nil {
			// THE LARGEST, NOT THE SUM. The alarm is about ONE
			// checkpoint that is not happening, and a gibibyte
			// spread evenly over two estates is two healthy logs
			// where a gibibyte in one is the fault.
			out.WALBytes = max(out.WALBytes, wal)
		}
	}
	// ONE VOLUME, from the node estate's path. Both files are opened under
	// `store.dir` and a deployment that split them across two mounts would
	// need two free counts — but it cannot: [store.Open] derives both from
	// one directory.
	if free, err := freeSpace(r.db.Path()); err == nil {
		out.FreeBytes = free
	}
}

// semanticCoverage is the fraction of this node's sources carrying a current
// vector, measured at most once per [RetentionInterval] — see
// [retention.coverAt].
//
// A POINTER, because zero coverage is the alarm and "no embeddings configured"
// is a company that asked for none — see [statelog.Reading.SemanticCoverage].
// A measurement that fails answers nil for the same reason, until the
// interval allows another: an unreadable corpus is not an uncovered one.
//
// THE ATTEMPT IS CLAIMED BEFORE IT IS MADE. The tick and every API request
// assemble a report on goroutines of their own, and two that found the cache
// due at once would otherwise both scan the corpus; the one that did not claim
// it answers from the measurement before, which is what it would have read a
// moment earlier.
//
// A SCAN ITS CALLER GAVE UP ON IS NO ATTEMPT. A report runs on the API
// request's context, so a client that disconnects mid-scan ends the scan with
// its own cancellation — which says nothing about the corpus. Cached as a
// failure it would blank this node's coverage for a whole interval and write
// `vector_coverage_unreadable` about a corpus that reads perfectly well; so
// the claim is handed back, nothing is cached or written, and the next report
// scans. A failure while the caller's context is still live is the corpus's,
// and is cached.
func (r *retention) semanticCoverage(ctx context.Context, now time.Time) *float64 {
	if r.coverage == nil {
		return nil
	}
	r.mu.Lock()
	claimedBefore := r.coverAt
	due := claimedBefore.IsZero() || now.Sub(claimedBefore) >= RetentionInterval
	if due {
		r.coverAt = now
	}
	fraction, known := r.coverFraction, r.coverKnown
	r.mu.Unlock()
	if !due {
		return coverageAnswer(fraction, known)
	}
	measured, ok, err := r.coverage(ctx)
	switch {
	case err != nil && ctx.Err() != nil:
		// HANDED BACK only while it is still this call's: a report that
		// claimed the scan after this one did owns the stamp now.
		r.mu.Lock()
		if r.coverAt.Equal(now) {
			r.coverAt = claimedBefore
		}
		r.mu.Unlock()
		return coverageAnswer(fraction, known)
	case err != nil:
		// A FAILURE IS CACHED AS NOTHING KNOWN, so the scan and this line
		// come once per interval while it lasts.
		measured, ok = 0, false
		r.logs().WarnContext(ctx, "vector_coverage_unreadable", "err", err,
			"detail", "the recall_below_floor alarm has nothing to judge "+
				"until a scan succeeds; the next is tried one trim "+
				"interval from now")
	}
	r.mu.Lock()
	r.coverFraction, r.coverKnown = measured, ok
	r.mu.Unlock()
	if r.metrics != nil && ok {
		r.metrics.Set(metrics.TrackerVectorCoverage, measured, nil)
	}
	return coverageAnswer(measured, ok)
}

// coverageAnswer is a coverage measurement as a report carries it: the
// fraction, or nil when nothing is known.
func coverageAnswer(fraction float64, known bool) *float64 {
	if !known {
		return nil
	}
	return &fraction
}
