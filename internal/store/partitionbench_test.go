package store_test

import (
	"bufio"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// BenchmarkANodeOpensEveryPartition measures what ONE NODE pays to hold every partition
// of a partitioned layout in its store: §N2 of the partitioning contract, and
// the evidence for §A4's rule 2 and for the pool constants in partition.go.
//
// # The shape it builds
//
// engine.DefaultLayoutOne at a tracker count T — a tracker space of T
// partitions carrying the tracker's and the vectors' logs, a pages space of
// T/4 carrying the pages' and the vectors', and one company partition carrying
// the tracker's — so T + T/4 + 1 files: 81 at T = 64, 321 at T = 256, the
// default. One node opens every one through [store.DB.OpenPartition], which is
// the node holding every partition of a small fleet, the heaviest node there
// is. Each file is then put in the state a RUNNING node keeps it in: one
// pinned writer per log, held as an applier holds it for the life of its
// loop, and one read, which is a reader connection opened and returned.
//
// # What it measures
//
//   - CREATE: every file made and migrated from nothing — a node's first boot
//     under the layout, and what a join costs before its snapshot lands.
//   - BOOT: the node closed and every file opened again, when the migrations
//     only CHECK — the cost a restart pays and the one rule 2 bounds.
//   - IDLE: the process's resident set, its open descriptors against its own
//     limit, and its CPU over [idleWindow] with every writer pinned and
//     nothing asked of it.
//   - THE CACHE SPLIT: each file's page cache as its own connection reports
//     it, and the ceiling the division puts on the whole — each file's share
//     times its connections — which is what the resident set grows to at
//     most as the files fill.
//
// # The verdict, against rule 2
//
// Rule 2: a node holding every partition must boot within
// config.DefaultRejoinWindow, and keep its idle resident set and its
// descriptors within a quarter of a 32 GiB node and of its descriptor limit.
// The benchmark states the verdict per shape and FAILS when a bound is broken,
// because a measurement that finds the design over budget must not read as a
// pass. The resident set is judged with the cache ceiling ADDED, since an idle
// node's cache is empty only until it has served.
//
// It is a benchmark rather than a test because it is minutes of wall clock
// and a number rather than a property: `go test` without -bench never runs
// it. Run it once per shape, on a quiet machine, with TMPDIR on the disk a
// node would use — tmpfs makes every fsync free:
//
//	TMPDIR=/var/tmp go test -run '^$' -bench 'PartitionFiles/T=256' \
//	  -benchtime 1x -timeout 30m -v ./internal/store/
//
// # What it measured, on this container: four cores, 15 GiB, ext4 on virtio
//
// Host load 2-2.7 throughout; a descriptor limit of 20,000 (Go raises the soft
// limit to the hard one at start); the store's default pool of four readers.
//
//   - T = 64, 81 files, 161 writers pinned: CREATE 12.7 s (157 ms a file);
//     BOOT 0.42 s (5.2 ms a file); IDLE +286 MiB resident (3.5 MiB a file),
//     +409 descriptors (5.0 a file), 0.0009 cores; every file's connections
//     at the 800 KiB floor, a cache ceiling of 252 MiB.
//   - T = 256, 321 files, 641 writers pinned: CREATE 49.2 s (153 ms a file);
//     BOOT 4.2 s (13 ms a file); IDLE +979 MiB resident (3.0 MiB a file),
//     +1,609 descriptors (5.0 a file), 0.0013 cores; every file at the floor,
//     a cache ceiling of 1,002 MiB.
//
// RULE 2 HOLDS AT BOTH COUNTS. At 321 files the boot is 4.2 s of the 30
// minute window, the resident set with every connection's cache filled is at
// most 2.0 GiB of the 8 GiB bound, and the store's 1,618 descriptors are 8.1%
// of this limit. The descriptors are the one bound that depends on the host
// rather than on this design: for the store's 1,618 to stay within a quarter,
// a node holding every partition needs a limit of at least 6,500 — before the
// broker's own descriptors (seven a log idle, in BenchmarkPartitionedEstate)
// — which a 4,096 hard limit does not give it.
//
// What the numbers say about the constants: the page-cache division is FLOORS
// ONLY at both counts — past 40 files the engine's 800 KiB minimum already
// exceeds the budget — so the cache ceiling is each file's connection count,
// readers and pinned writers together, times that floor; a third reader per
// file would add a quarter to it. The boot's per-file cost grows with the
// count — an open took 7 ms among the first files and 15 ms among the last at
// T = 256 — and the division is not why: re-dividing across all 321 files is
// 1.2 ms. The rest is the driver's own open, which this does not break down,
// and at 321 files it is four seconds against thirty minutes.
func BenchmarkANodeOpensEveryPartition(b *testing.B) {
	for _, tracker := range []int{64, engine.DefaultTrackerPartitions} {
		layout := layoutAtTracker(b, tracker)
		name := fmt.Sprintf("T=%d,files=%d", tracker, len(layout.Partitions()))
		b.Run(name, func(b *testing.B) {
			for range b.N {
				measurePartitionFiles(b, layout)
			}
		})
	}
}

const (
	// idleWindow is how long the idle CPU is sampled over: long enough that
	// a periodic wake-up of a second or more is inside it, and a store with
	// no goroutine of its own reads as the zero it is.
	idleWindow = 10 * time.Second

	// ruleTwoRSS and ruleTwoFDShare are §A4 rule 2's bounds: a quarter of a
	// 32 GiB node, and a quarter of the process's descriptor limit.
	ruleTwoRSS     = 32 << 30 / 4
	ruleTwoFDShare = 0.25
)

// layoutAtTracker is engine.DefaultLayoutOne with its tracker space at
// tracker partitions and its pages space at a quarter of that, as the
// default derives the pages count.
func layoutAtTracker(tb testing.TB, tracker int) statelog.Layout {
	tb.Helper()
	layout := engine.DefaultLayoutOne()
	layout.Spaces = slices.Clone(layout.Spaces)
	for i := range layout.Spaces {
		switch layout.Spaces[i].Space {
		case statelog.SpaceTracker:
			layout.Spaces[i].Partitions = tracker
		case statelog.SpacePages:
			layout.Spaces[i].Partitions = tracker / config.DerivedPagesLogDivisor
		}
	}
	if err := layout.Validate(); err != nil {
		tb.Fatalf("layout at T = %d: %v", tracker, err)
	}
	return layout
}

// footprint is one reading of the process.
type footprint struct {
	rss     int64
	fds     int
	cpu     time.Duration
	at      time.Time
	threads int
}

func readFootprint(tb testing.TB) footprint {
	tb.Helper()
	runtime.GC()
	f := footprint{at: time.Now()}
	status, err := os.Open("/proc/self/status")
	if err != nil {
		tb.Fatalf("this benchmark reads /proc and runs on Linux, the platform "+
			"a node's footprint is judged on: %v", err)
	}
	defer func() { _ = status.Close() }()
	scan := bufio.NewScanner(status)
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "VmRSS:":
			kib, _ := strconv.ParseInt(fields[1], 10, 64)
			f.rss = kib << 10
		case "Threads:":
			f.threads, _ = strconv.Atoi(fields[1])
		}
	}
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		tb.Fatalf("count descriptors: %v", err)
	}
	f.fds = len(fds)
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		tb.Fatalf("read CPU time: %v", err)
	}
	f.cpu = time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
	return f
}

// openLayout opens every partition of layout on node, pins one writer per log
// and reads each file once, answering the writers so the caller can hold them
// for as long as it measures.
func openLayout(b *testing.B, node *store.DB, layout statelog.Layout) []*store.Writer {
	b.Helper()
	var writers []*store.Writer
	for _, p := range layout.Partitions() {
		file, err := layout.File(p)
		if err != nil {
			b.Fatalf("%s: %v", p, err)
		}
		part, err := node.OpenPartition(b.Context(), file)
		if err != nil {
			b.Fatalf("open %s: %v", file.Name, err)
		}
		for range file.Logs {
			w, err := part.Writer(b.Context())
			if err != nil {
				b.Fatalf("pin a writer on %s: %v", file.Name, err)
			}
			writers = append(writers, w)
		}
		if err := part.Read(b.Context(), func(*sql.Tx) error { return nil }); err != nil {
			b.Fatalf("read %s: %v", file.Name, err)
		}
	}
	return writers
}

func closeWriters(b *testing.B, writers []*store.Writer) {
	b.Helper()
	for _, w := range writers {
		if err := w.Close(); err != nil {
			b.Fatalf("release a writer: %v", err)
		}
	}
}

// cacheSplit reads each open file's page cache back off one of its own
// connections, and the ceiling the division puts on the node: every file's
// share times the connections its pool may hold.
func cacheSplit(b *testing.B, node *store.DB, layout statelog.Layout) (lo, hi, ceiling int64) {
	b.Helper()
	lo = -1
	for _, p := range layout.Partitions() {
		file, _ := layout.File(p)
		part, err := node.PartitionDB(file.Name)
		if err != nil {
			b.Fatalf("%s: %v", file.Name, err)
		}
		// ONE CONNECTION for both, for the reason probePageCache gives:
		// the pragma answers in KiB when negative and in PAGES when
		// positive, which is what the engine answers when it raised a
		// smaller cache to its minimum.
		var raw, pageSize int64
		if err := part.Read(b.Context(), func(tx *sql.Tx) error {
			if err := tx.QueryRowContext(b.Context(), `PRAGMA cache_size`).Scan(&raw); err != nil {
				return err
			}
			return tx.QueryRowContext(b.Context(), `PRAGMA page_size`).Scan(&pageSize)
		}); err != nil {
			b.Fatalf("read %s's cache size: %v", file.Name, err)
		}
		kib := -raw
		if raw > 0 {
			kib = raw * pageSize >> 10
		}
		if lo < 0 || kib < lo {
			lo = kib
		}
		hi = max(hi, kib)
		// THE POOL'S OWN BOUND, readers and pinned writers together, as
		// the division last set it.
		ceiling += kib << 10 * int64(part.SQL().Stats().MaxOpenConnections)
	}
	return lo, hi, ceiling
}

func measurePartitionFiles(b *testing.B, layout statelog.Layout) {
	files := len(layout.Partitions())
	dir := b.TempDir()
	path := filepath.Join(dir, "node.db")
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		b.Fatalf("read the descriptor limit: %v", err)
	}
	base := readFootprint(b)
	b.Logf("baseline: rss %.1f MiB, %d fds (limit %d), %d threads",
		mib(base.rss), base.fds, limit.Cur, base.threads)

	// CREATE: the node's first boot under the layout.
	start := time.Now()
	node, err := store.OpenNode(b.Context(), path, store.Options{})
	if err != nil {
		b.Fatalf("open the node: %v", err)
	}
	writers := openLayout(b, node, layout)
	created := time.Since(start)
	b.Logf("create: %d files made and migrated in %s (%s a file), %d writers pinned",
		files, created.Round(time.Millisecond), (created / time.Duration(files)).Round(time.Microsecond),
		len(writers))
	closeWriters(b, writers)
	if err := node.Close(); err != nil {
		b.Fatalf("close the node: %v", err)
	}

	// BOOT: every file opened again, its migrations only checked.
	start = time.Now()
	node, err = store.OpenNode(b.Context(), path, store.Options{})
	if err != nil {
		b.Fatalf("reopen the node: %v", err)
	}
	defer func() { _ = node.Close() }()
	writers = openLayout(b, node, layout)
	booted := time.Since(start)
	defer closeWriters(b, writers)
	b.Logf("boot: %d files opened, checked and pinned in %s (%s a file)",
		files, booted.Round(time.Millisecond), (booted / time.Duration(files)).Round(time.Microsecond))

	// IDLE: every writer pinned, nothing asked.
	open := readFootprint(b)
	time.Sleep(idleWindow)
	idle := readFootprint(b)
	cpu := float64(idle.cpu-open.cpu) / float64(idle.at.Sub(open.at))
	rss, fds := idle.rss-base.rss, idle.fds-base.fds
	b.Logf("idle: rss +%.1f MiB (%.2f MiB a file), +%d fds (%.1f a file), "+
		"+%d threads, %.4f cores over %s",
		mib(rss), mib(rss)/float64(files), fds, float64(fds)/float64(files),
		idle.threads-base.threads, cpu, idleWindow)

	lo, hi, ceiling := cacheSplit(b, node, layout)
	b.Logf("cache: each file's connections cache %d-%d KiB; the division's "+
		"ceiling over every connection is %.1f MiB", lo, hi, mib(ceiling))

	// THE VERDICT.
	window := config.DefaultRejoinWindow
	worst := idle.rss + ceiling
	share := float64(idle.fds) / float64(limit.Cur)
	b.Logf("rule 2: boot %s of %s; resident %.1f MiB idle + %.1f MiB cache "+
		"ceiling = %.1f MiB of %.0f MiB; %d fds = %.2f%% of %d",
		booted.Round(time.Millisecond), window, mib(idle.rss), mib(ceiling),
		mib(worst), mib(ruleTwoRSS), idle.fds, 100*share, limit.Cur)
	if booted > window {
		b.Errorf("rule 2 FAILS at %d files: booting took %s, over the %s rejoin window",
			files, booted, window)
	}
	if worst > ruleTwoRSS {
		b.Errorf("rule 2 FAILS at %d files: %.1f MiB resident with the cache "+
			"filled, over a quarter of a 32 GiB node", files, mib(worst))
	}
	if share > ruleTwoFDShare {
		b.Errorf("rule 2 FAILS at %d files: %d descriptors, over a quarter of "+
			"the limit of %d", files, idle.fds, limit.Cur)
	}

	b.ReportMetric(created.Seconds(), "create-s")
	b.ReportMetric(booted.Seconds(), "boot-s")
	b.ReportMetric(mib(rss), "idle-rss-MiB")
	b.ReportMetric(float64(fds), "fds")
	b.ReportMetric(100*share, "fd-limit-%")
	b.ReportMetric(cpu, "idle-cores")
	b.ReportMetric(mib(ceiling), "cache-ceiling-MiB")
}

func mib(bytes int64) float64 { return float64(bytes) / (1 << 20) }
