package store

import (
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"runtime"
	"testing"
)

// A NODE'S PAGE CACHE IS A SIXTEENTH OF ITS MEMORY, SHARED BY EVERY
// CONNECTION, within the floor and the cap: a connection never keeps more
// because the node is large, nor less than the floor because it is small.
//
// Mutations, each red: size each connection by the whole sixteenth; drop the
// floor; drop the cap; divide by no connections.
func TestThePageCacheIsASixteenthOfTheNodeSharedByItsConnections(t *testing.T) {
	t.Parallel()
	const gib = int64(1) << 30
	for _, tc := range []struct {
		memory int64
		conns  int
		want   int
	}{
		{2 * gib, 34, int(2 * gib / 16 / 34 >> 10)},
		{8 * gib, 34, int(8 * gib / 16 / 34 >> 10)},
		{64 * gib, 34, pageCacheMaxKiB},
		{512 << 20, 34, pageCacheFloorKiB},
		{0, 34, pageCacheFloorKiB},
	} {
		if got := PageCacheKiB(tc.memory, tc.conns); got != tc.want {
			t.Errorf("%d bytes over %d connections: %d KiB each, want %d", tc.memory, tc.conns, got, tc.want)
		}
	}
}

// WHAT A NODE COUNTS OF ITS PAGE CACHE IS NEVER LESS THAN ITS CONNECTIONS
// CACHE, AND GROWS MORE SLOWLY THAN ITS MEMORY: the share is each
// connection's part rounded up, never down, and gains at most a sixteenth of
// what the memory gains (gitlet/cache's memory_needed halves on it).
//
// Mutations, each red: count a connection's part rounded up; grow the share as
// fast as the memory.
func TestThePageCacheShareCountsWhatItsConnectionsCache(t *testing.T) {
	t.Parallel()
	const conns = 32
	last := PageCacheShare(0, conns)
	for memory := int64(0); memory <= 64<<30; memory += 64<<20 + 4093 {
		share, part := PageCacheShare(memory, conns), int64(conns*PageCacheKiB(memory, conns))<<10
		if part > share || share-part >= conns<<10 {
			t.Errorf("%d bytes: the share is %d, its %d connections cache %d", memory, share, conns, part)
		}
		if share < last || share-last > (64<<20+4093)/pageCacheShare+1 {
			t.Errorf("%d bytes: the share moved from %d to %d over %d bytes of memory", memory, last, share,
				64<<20+4093)
		}
		last = share
	}
}

// WHAT A NODE COUNTS OF ITS PAGE CACHE IS WHAT ITS FILES OPEN: every
// connection of its own file and of the replicated estate, pinned writers
// included, at the cache each was opened with, so its line in the node's
// memory budget is never short of what it holds.
//
// Mutations, each red: count the node's file alone; leave the pins out; leave
// the replicated estate's read floor out; keep the requested figure past the
// cap.
func TestTheNodeCountsEveryConnectionItCaches(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	const logs = 3
	for _, opts := range []Options{{}, {MaxOpenConns: 1, PageCacheKiB: 64 << 10}, {PageCacheKiB: 8 << 10}} {
		node, err := OpenNode(ctx, filepath.Join(t.TempDir(), "node.db"), opts)
		if err != nil {
			t.Fatalf("open the node: %v", err)
		}
		replicated, err := node.OpenReplicated(ctx, logs)
		if err != nil {
			t.Fatalf("open the replicated estate: %v", err)
		}
		held := int64(node.pool*node.Caps().PageCacheKiB + replicated.pool*replicated.Caps().PageCacheKiB)
		if got := int64(opts.NodeConnections(logs) * opts.pageCacheKiB()); got != held {
			t.Errorf("a node opened with %+v counts %d KiB of page cache, its files hold %d (pools %d and %d)",
				opts, got, held, node.pool, replicated.pool)
		}
		_ = node.Close()
	}
}

// BenchmarkAKeyedReadAtThePageCacheFloor prices the floor against the cap:
// keyed reads, by rowid and through a secondary index, from eight readers at
// once over a file whose interior pages pass the floor (a million 1 KiB rows,
// about 1 GiB), the file itself in the OS page cache as a node's would be.
// Figures are at pageCacheFloorKiB.
func BenchmarkAKeyedReadAtThePageCacheFloor(b *testing.B) {
	const rows = 1_000_000
	path := filepath.Join(b.TempDir(), "keyed.db")
	build, err := OpenNode(b.Context(), path, Options{})
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	for _, q := range []string{`CREATE TABLE zz (id INTEGER PRIMARY KEY, k TEXT NOT NULL, v BLOB NOT NULL)`,
		`CREATE INDEX zz_k ON zz (k)`} {
		if _, err := build.sql.ExecContext(b.Context(), q); err != nil {
			b.Fatalf("%s: %v", q, err)
		}
	}
	blob := make([]byte, 1024)
	for i := 0; i < rows; i += 5000 {
		tx, err := build.sql.BeginTx(b.Context(), nil)
		if err != nil {
			b.Fatalf("begin: %v", err)
		}
		for j := i; j < i+5000; j++ {
			if _, err := tx.ExecContext(b.Context(), `INSERT INTO zz (id, k, v) VALUES (?, ?, ?)`, j,
				fmt.Sprintf("key-%09d", j*7919%rows), blob); err != nil {
				b.Fatalf("insert: %v", err)
			}
		}
		if err := tx.Commit(); err != nil {
			b.Fatalf("commit: %v", err)
		}
	}
	_ = build.Close()
	for _, kib := range []int{pageCacheFloorKiB, 8 << 10, pageCacheMaxKiB} {
		for _, read := range []struct {
			name, query string
			arg         func(n int) any
		}{
			{"rowid", `SELECT length(v) FROM zz WHERE id = ?`, func(n int) any { return n }},
			{"index", `SELECT id FROM zz WHERE k = ?`, func(n int) any { return fmt.Sprintf("key-%09d", n) }},
		} {
			b.Run(fmt.Sprintf("%dKiB/%s", kib, read.name),
				func(b *testing.B) {
					db, err := OpenNode(b.Context(), path, Options{PageCacheKiB: kib})
					if err != nil {
						b.Fatalf("open: %v", err)
					}
					defer func() { _ = db.Close() }()
					b.SetParallelism(max(1, (MinReaderConns+runtime.GOMAXPROCS(0)-1)/runtime.GOMAXPROCS(0)))
					b.ResetTimer()
					b.RunParallel(func(pb *testing.PB) {
						for pb.Next() {
							var out int64
							if err := db.sql.QueryRowContext(b.Context(), read.query, read.arg(rand.IntN(rows))).Scan(&out); err != nil {
								b.Errorf("%s: %v", read.query, err)
								return
							}
						}
					})
				})
		}
	}
}
