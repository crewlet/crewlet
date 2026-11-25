package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
)

// This node's own hardware, for `volume_low`, `wal_large` and `pool_starved`.
//
// A copy (store.DB.Copies) stops every checkpoint for its whole run, so the
// reading names the copy holding the largest log and counts per volume what a
// copy holds back (store.DB.CopyHoldsBack), for `volume_low` to keep room.
//
// Sizes are LEVELS, cheap and correct whenever taken, so each reading measures
// them. A pool wait is a DELTA of `sql.DBStats`, and consuming one clears it for
// everyone after, so it is taken on the trim's tick alone
// ([statelog.TrimInterval]): never on the alarm evaluation
// ([statelog.AlarmInterval]), which would change what `pool_wait`'s p95 is a
// percentile of, nor on a request whose frequency a dashboard decides.

// capacity records what one trim tick can measure about this node's own
// storage.
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
// The age is read off the disk, not the register the trim and `backup_age`
// read: only the disk shows a copy announced then deleted or never shipped. So
// it reads the manifests of the points THIS NODE announced, the only ones it can
// reach. No backup publishes nothing, never zero, the freshest age possible.
//
// The holds are published by every node, not by the trim's singleton tick: a
// gauge only the lease holder publishes vanishes whenever the lease moves.
func (r *retention) backupGauges(ctx context.Context) {
	if r.fleet == nil {
		return
	}
	if holds, err := r.fleet.Holds(ctx); err == nil {
		// A count stuck above zero is a crashed holder, which the trim
		// reads past once it has watched its pin go unrenewed; nothing
		// removes the row but its owner.
		r.metrics.Set(metrics.BackupHolds, float64(len(holds)), nil)
	}
	points, err := r.fleet.BackupPoints(ctx)
	if err != nil {
		return
	}
	// One instant from the duty's clock, so this gauge shares the clock of
	// every report and evaluation beside it.
	now := r.clock()
	youngest, found := time.Duration(0), false
	for _, point := range points {
		if point.Owner != r.nodeID || point.Dir == "" {
			continue
		}
		// [backup.Age] clamps a manifest from a clock running ahead to zero.
		age, ok, err := backup.Age(point.Dir, now)
		if err != nil {
			log.WarnContext(ctx, "backup_age_unreadable", "dir", point.Dir,
				"err", err)
			continue
		}
		if ok && (!found || age < youngest) {
			youngest, found = age, true
		}
	}
	if found {
		r.metrics.Set(metrics.BackupAge, youngest.Seconds(), nil)
	}
}

// poolWait observes how long a caller queued for a connection since the last
// tick.
//
// One observation per tick, the interval's mean: `sql.DBStats` keeps only a
// count and a total. A tick with no waits is skipped, not recorded as zero:
// nobody queuing is not somebody queuing for no time.
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
// pages in; `internal/store`'s own list is unexported.
const walSuffix = "-wal"

// storeFiles is every database file this node holds, in a stable order: its
// own, then the replicated estate where it holds one open. An estate an
// adoption holds closed is not measured: its file is being replaced.
func storeFiles(db *store.DB) []*store.DB {
	if db == nil {
		return nil
	}
	out := []*store.DB{db}
	if replicated, err := db.ReplicatedDB(); err == nil {
		out = append(out, replicated)
	}
	return out
}

// fileBytes is a file's size, and zero with no error when it does not exist.
//
// A missing -wal is zero bytes, not a failure: nothing uncheckpointed is the
// healthiest state `wal_large` has.
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

// space fills the storage fields of a reading, at now.
//
// Measured, not read off the gauges: those are published on the trim's tick,
// far slower than [statelog.AlarmInterval], and a volume fills faster.
func (m nodeMeasures) space(out *statelog.Reading, now time.Time) {
	if m.db == nil {
		return
	}
	measure := m.volumes
	if measure == nil {
		measure = measureVolume
	}
	var files []storedFile
	var unmeasured []string
	for _, db := range storeFiles(m.db) {
		if wal, err := fileBytes(db.Path() + walSuffix); err == nil && wal > out.WALBytes {
			// The largest, not the sum: the alarm is about ONE
			// checkpoint not happening. Named with the copy holding
			// it, if any, whose end folds it back in.
			out.WALBytes, out.WALFile = wal, filepath.Base(db.Path())
			out.WALCopy, out.WALCopyFor = holdingCopy(db.Copies(), now)
		}
		// An unmeasurable file is reported, never skipped: skipped, the
		// node is judged on its other volume alone.
		size, err := fileBytes(db.Path())
		if err != nil {
			unmeasured = append(unmeasured, err.Error())
			continue
		}
		dir := filepath.Dir(db.Path())
		vol, free, err := measure(dir)
		if err != nil {
			unmeasured = append(unmeasured, err.Error())
			continue
		}
		files = append(files, storedFile{volume: vol, dir: dir, free: free, size: size,
			held: db.CopyHoldsBack(size)})
	}
	tightest := tightestVolume(files)
	out.FreeBytes, out.StoreBytes, out.CopyHeldBytes, out.StoreVolume =
		tightest.free, tightest.stored, tightest.held, tightest.dir
	out.StoreVolumeUnmeasured = strings.Join(unmeasured, "; ")
}

// holdingCopy names the copies holding a file's log — the part file the one
// holding it longest writes, and how many more hold it beside — and how long
// that one has; nothing when none does.
func holdingCopy(copies []store.Copy, now time.Time) (string, time.Duration) {
	if len(copies) == 0 {
		return "", 0
	}
	oldest := copies[0]
	name := oldest.Dest
	if more := len(copies) - 1; more > 0 {
		name = fmt.Sprintf("%s and %d more", name, more)
	}
	return name, max(now.Sub(oldest.Since), 0)
}

// measureVolume is the volume dir is on — the filesystem's own device number,
// so two paths on one volume are one volume — and what an unprivileged process
// may still write there ([volumeFree]).
func measureVolume(dir string) (string, int64, error) {
	var st unix.Stat_t
	if err := unix.Stat(dir, &st); err != nil {
		return "", 0, fmt.Errorf("engine: identify the volume holding %s: %w", dir, err)
	}
	free, err := volumeFree(dir)
	if err != nil {
		return "", 0, err
	}
	// Sprint rather than a conversion: Stat_t.Dev is a uint64 on linux and
	// an int32 on darwin, both release targets, and the key only has to
	// tell two devices apart.
	return fmt.Sprint(st.Dev), free, nil
}

// storedFile is one database file as the volume alarm weighs it: the volume it
// is on, the directory it is in — what an operator is told to grow — what that
// volume has free, how large the file is, and the log a copy of it is expected
// to hold back there ([store.DB.CopyHoldsBack]).
type storedFile struct {
	volume, dir      string
	free, size, held int64
}

// storeVolume is one volume as the alarm reads it: a directory on it, what it
// has free, what the databases on it occupy, and the log a copy of them is
// expected to hold back there.
type storeVolume struct {
	dir                string
	free, stored, held int64
}

// tightestVolume is the free space, the stored bytes and the held log of the
// volume with the least room for the next copy of what it holds and the log
// that copy holds back — the one `volume_low` has to be about.
//
// Per volume, since `store.replicated_path` may put the replicated estate on
// another one. Volumes are told apart by the filesystem's device, never by
// path, and each is named by its first file's directory in the files' order,
// so it is named the same on every reading.
func tightestVolume(files []storedFile) storeVolume {
	var order []string
	byVolume := map[string]*storeVolume{}
	for _, f := range files {
		v, ok := byVolume[f.volume]
		if !ok {
			v = &storeVolume{dir: f.dir, free: f.free}
			byVolume[f.volume] = v
			order = append(order, f.volume)
		}
		v.stored += f.size
		v.held += f.held
	}
	var out storeVolume
	for i, id := range order {
		v := byVolume[id]
		// The smallest free-to-needed ratio, compared by cross
		// multiplying so an empty volume never divides by zero, in
		// floating point since products of terabytes overflow int64.
		if i == 0 || float64(v.free)*volumeNeed(out) < float64(out.free)*volumeNeed(*v) {
			out = *v
		}
	}
	return out
}

// volumeNeed is the room the next copy of a volume's databases wants there: the
// alarm's own measure, so the volume read is the one nearest its condition.
func volumeNeed(v storeVolume) float64 {
	return statelog.VolumeHeadroomFactor*float64(v.stored) + float64(v.held)
}

// measureCoverage takes the fraction of this node's sources carrying a current
// vector, on the tick.
//
// Every tick, unconditionally, and never on a report: it scans the whole
// corpus, and a report runs on every request and dashboard poll. Gating the
// scan on the cached figure's age skips it whenever the last tick ran long.
//
// A failed measurement leaves nothing, the gauge withdrawn too: an unreadable
// corpus is neither covered nor uncovered.
func (r *retention) measureCoverage(ctx context.Context) {
	if r.coverage == nil {
		return
	}
	fraction, known, err := r.coverage(ctx)
	if err != nil {
		log.WarnContext(ctx, "vector_coverage_unreadable", "err", err)
		fraction, known = 0, false
	}
	r.mu.Lock()
	r.coverFraction, r.coverKnown = fraction, known
	r.mu.Unlock()
	switch {
	case r.metrics == nil:
	case known:
		r.metrics.Set(metrics.TrackerVectorCoverage, fraction, nil)
	default:
		r.metrics.Unset(metrics.TrackerVectorCoverage, nil)
	}
}

// semanticCoverage is the tick's coverage measurement, read back.
//
// A pointer, as [statelog.Reading.SemanticCoverage] says: zero is the alarm.
// A failed measurement or a trim that has not ticked answers nil, since an
// unmeasured corpus is not an uncovered one.
func (r *retention) semanticCoverage() *float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.coverKnown {
		return nil
	}
	fraction := r.coverFraction
	return &fraction
}
