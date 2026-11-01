package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	codehome "github.com/crewlet/crewlet/internal/code/home"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/code/query"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// THE GIT ROLE IS NEEDED ONLY WHERE THE COMPANY HOSTS CODE, and every other
// role always: the fleet's unmanned-role check asks this on every sweep, off
// the current epoch, so a fleet that declared its roles without git is told
// nobody serves it only once code is hosted.
func TestTheGitRoleIsNeededOnlyWhereCodeIsHosted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		company *Company
		git     bool
	}{
		{"no company", nil, false},
		{"a company hosting no code", &Company{Config: &config.Company{}}, false},
		{"a company hosting code", &Company{Config: &config.Company{Code: config.Code{Native: true}}}, true},
	} {
		e := &Engine{}
		if tc.company != nil {
			e.epoch.current.Store(tc.company)
		}
		for _, role := range placement.Vocabulary() {
			want := role != placement.RoleGit || tc.git
			if got := e.roleNeeded(role); got != want {
				t.Errorf("%s: roleNeeded(%s) = %t, want %t", tc.name, role, got, want)
			}
		}
	}
}

// THE CACHE'S DERIVED BUDGET COUNTS WHAT THE CACHE HOLDS. Free space excludes
// the cache's own files, so a budget derived from it alone shrank at every
// restart of a warm node, and its first janitor pass evicted the difference:
// warm repositories downloaded again for no change in the disk. A cache
// holding a gibibyte on a volume with F free derives from F plus that
// gibibyte, less the request spools' allowance and the revision facts' patch
// spools (query.PatchSpoolAllowance), a second boot over the same files
// derives the same budget, and a cache on the store's own filesystem is held
// to the shared half.
//
// Mutations: derive from the free space alone, and the budgets go red; leave
// the patch spools out of the room, and they go red too.
func TestTheCacheBudgetCountsWhatTheCacheHolds(t *testing.T) {
	t.Parallel()
	const (
		gib       = int64(1) << 30
		free      = 40 * gib
		transfers = 4
	)
	spools := host.SpoolAllowance(transfers)
	b := config.DefaultBootstrap()
	base := t.TempDir()
	b.Store.Path = filepath.Join(base, "crewlet.db")
	dir := filepath.Join(base, "git")

	// A GIBIBYTE THE LAST PROCESS LEFT IN trash/, which Prepare counts (the
	// janitor gives it back after the boot) — sparse, so the case costs no
	// disk.
	first := prepareCache(t, dir)
	first.Release()
	leftover := filepath.Join(dir, "trash", "leftover")
	if err := os.WriteFile(leftover, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(leftover, gib); err != nil {
		t.Fatal(err)
	}

	volume := cacheVolume{
		free:   func(string) (int64, error) { return free, nil },
		same:   sameVolume,
		broker: noBroker,
	}
	var sizes []codeCacheSize
	for range 2 {
		layout := prepareCache(t, dir)
		size, err := codeCacheBudget(&b, layout, transfers, volume)
		layout.Release()
		if err != nil {
			t.Fatalf("codeCacheBudget: %v", err)
		}
		sizes = append(sizes, size)
	}
	room := free + gib - spools - query.PatchSpoolAllowance
	want := codeCacheSize{budget: config.DerivedCacheMaxBytes(room, false), derived: true,
		room: room, free: free, held: gib, spools: spools, transfers: transfers,
		patches: query.PatchSpoolAllowance, dedicated: false}
	for i, got := range sizes {
		if !reflect.DeepEqual(got, want) {
			t.Errorf("boot %d sized the cache %+v, want %+v — the room it can use is the free "+
				"space and what it holds, less the spools' allowance and the patch spools'", i+1, got, want)
		}
	}

	b.Git.CacheMaxBytes = 8 * gib
	layout := prepareCache(t, dir)
	defer layout.Release()
	if size, err := codeCacheBudget(&b, layout, transfers, volume); err != nil ||
		size.budget != 8*gib || size.derived {
		t.Errorf("an explicit budget sized the cache %+v (%v), want 8 GiB as written", size, err)
	}
}

// noBroker is a node whose broker keeps no streams on this node's disk.
func noBroker() (jetstream.VolumeClaim, bool, error) { return jetstream.VolumeClaim{}, false, nil }

// THE CACHE LEAVES THE BROKER BESIDE IT ITS CLAIM. An embedded broker storing
// its streams on the cache's volume may fill it up to the store limit its
// server enforces — three quarters of that volume's free space where
// stream.store_max_bytes is unset — so the room the cache divides is what that
// limit leaves, and the cache, the broker and the request spools together
// never claim more than the volume has: the reference layout, with everything
// under one directory, gave the broker three quarters and the cache a quarter
// of one free space, and the store nothing to grow into. A broker on another
// volume claims nothing of this one, a small volume's share is the share with
// no floor raising it, and a claim that cannot be read fails the build rather
// than be guessed. (A claim that leaves no room at all is
// TestARoomOfNothingRefusesTheCodeHostByName's.)
//
// Mutation: leave the broker's claim out of the room, and the overcommit rows
// go red.
func TestTheCacheBudgetLeavesTheBrokerItsClaim(t *testing.T) {
	t.Parallel()
	const gib = int64(1) << 30
	b := config.DefaultBootstrap()
	base := t.TempDir()
	b.Store.Path = filepath.Join(base, "crewlet.db")
	layout := prepareCache(t, filepath.Join(base, "git"))
	defer layout.Release()
	held := layout.Held

	for _, tc := range []struct {
		name         string
		free         int64
		claim        jetstream.VolumeClaim
		apart        bool // the broker's directory is on another volume
		transfers    int  // 64 MiB of spool each
		room, broker int64
	}{
		{name: "the reference layout's first boot", free: 100 * gib,
			claim: jetstream.VolumeClaim{Dir: "stream", Limit: 75 * gib}, transfers: 16,
			room: 24 * gib, broker: 75 * gib},
		{name: "a broker that has stored part of its limit", free: 80 * gib,
			claim:     jetstream.VolumeClaim{Dir: "stream", Limit: 75 * gib, Stored: 20 * gib},
			transfers: 16, room: 24 * gib, broker: 55 * gib},
		{name: "a small volume, its share under any floor", free: 8 * gib,
			claim: jetstream.VolumeClaim{Dir: "stream", Limit: 6 * gib}, transfers: 4,
			room: 2*gib - gib/4, broker: 6 * gib},
		{name: "a broker on a volume of its own", free: 100 * gib, apart: true,
			claim: jetstream.VolumeClaim{Dir: "stream", Limit: 75 * gib}, transfers: 16,
			room: 99 * gib, broker: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			size, err := codeCacheBudget(&b, layout, tc.transfers, cacheVolume{
				free: func(string) (int64, error) { return tc.free, nil },
				same: func(_, other string) bool { return !tc.apart || other != tc.claim.Dir },
				broker: func() (jetstream.VolumeClaim, bool, error) {
					return tc.claim, true, nil
				},
			})
			if err != nil {
				t.Fatalf("codeCacheBudget: %v", err)
			}
			// THE ROWS' ROOMS are before the revision facts' patch spools,
			// which every node's room is less.
			room := tc.room + held - query.PatchSpoolAllowance
			if size.room != room || size.broker != tc.broker || size.dedicated {
				t.Fatalf("sized %+v, want a shared room of %d with the broker claiming %d",
					size, room, tc.broker)
			}
			if size.budget != config.DerivedCacheMaxBytes(size.room, false) {
				t.Errorf("budget %d, want the shared share of the room %d", size.budget, size.room)
			}
			// NEVER AN OVERCOMMIT: what the cache may hold, what the broker
			// may still write and what the spools and the patches may hold
			// fit in what the volume has room for beside everything already
			// there.
			if size.budget+size.broker+size.spools+size.patches > tc.free+held {
				t.Errorf("the cache's %d, the broker's %d, the spools' %d and the patches' %d claim "+
					"more than the %d free and %d held", size.budget, size.broker, size.spools,
					size.patches, tc.free, held)
			}
		})
	}

	_, err := codeCacheBudget(&b, layout, 16, cacheVolume{
		free: func(string) (int64, error) { return 100 * gib, nil },
		same: func(string, string) bool { return true },
		broker: func() (jetstream.VolumeClaim, bool, error) {
			return jetstream.VolumeClaim{}, false, errors.New("jetstream disabled")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "jetstream disabled") {
		t.Errorf("a claim that could not be read sized the cache anyway (%v)", err)
	}
}

// A VOLUME WITH NO ROOM FOR A CACHE REFUSES THE CODE HOST BY NAME. Where what
// the cache's volume has — its free space and what the cache holds — is all
// claimed by the bounded tenants beside it, the broker's limit, the request
// spools and the revision facts' patch spools, there is no share to derive:
// any budget would be bytes the volume does not have. So the build is refused
// here, naming every figure it was decided from and each setting that changes
// one, rather than by the cache's own refusal of a budget of nothing, which
// names none of them. A sixteen-core node's spools reach that with eighteen
// gibibytes free on the single-volume layout; a declared broker limit past
// the volume, a broker holding to no limit, a small volume of the cache's own
// and one the patch spools alone overfill reach it too. An explicit
// git.cache_max_bytes is the operator's decision on the same volume, and is
// sized as written (code_cache_sized warns that the room cannot hold it).
//
// Mutations: derive a budget of nothing without refusing it, and every refused
// row goes red; leave the patch spools out of the room, and their row is sized.
func TestARoomOfNothingRefusesTheCodeHostByName(t *testing.T) {
	t.Parallel()
	const gib = int64(1) << 30
	b := config.DefaultBootstrap()
	base := t.TempDir()
	b.Store.Path = filepath.Join(base, "crewlet.db")
	layout := prepareCache(t, filepath.Join(base, "git"))
	defer layout.Release()
	// A COLD CACHE, holding nothing a room could be made of.
	cold := layout
	cold.Held = 0

	for _, tc := range []struct {
		name      string
		free      int64
		claim     jetstream.VolumeClaim
		stores    bool   // an embedded broker keeps its streams on this node's disk
		declared  int64  // stream.store_max_bytes as written
		broker    int64  // what it may still write there
		limit     string // how the refusal names the limit it writes under
		dedicated bool
		transfers int
	}{
		{name: "the reference layout on a sixteen-core node", free: 16 * gib,
			claim: jetstream.VolumeClaim{Dir: "stream", Limit: 12 * gib}, stores: true,
			broker: 12 * gib, limit: fmt.Sprintf("its own limit of %d", 12*gib), transfers: 64},
		{name: "a declared broker limit past what the volume has", free: 10 * gib,
			claim: jetstream.VolumeClaim{Dir: "stream", Limit: 40 * gib}, stores: true,
			declared: 40 * gib, broker: 40 * gib,
			limit: fmt.Sprintf("stream.store_max_bytes, %d", 40*gib), transfers: 16},
		// A SERVER HOLDING TO NO LIMIT may take every free byte.
		{name: "a broker holding to no limit", free: 10 * gib,
			claim: jetstream.VolumeClaim{Dir: "stream", Limit: -1}, stores: true,
			broker: 10 * gib, limit: "no limit at all", transfers: 4},
		{name: "spools past a small volume of the cache's own", free: gib / 2, dedicated: true,
			transfers: 16},
		// ROOM FOR THE REQUEST SPOOLS AND A QUARTER GIBIBYTE MORE, which the
		// revision facts' patch spools take, and more.
		{name: "the patch spools past a small volume of the cache's own",
			free: host.SpoolAllowance(4) + gib/4, dedicated: true, transfers: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			volume := cacheVolume{
				free: func(string) (int64, error) { return tc.free, nil },
				same: func(string, string) bool { return !tc.dedicated },
				broker: func() (jetstream.VolumeClaim, bool, error) {
					return tc.claim, tc.stores, nil
				},
			}
			b := b
			b.Stream.StoreMaxBytes = tc.declared
			size, err := codeCacheBudget(&b, cold, tc.transfers, volume)
			if err == nil {
				t.Fatalf("a volume whose tenants claim all of it sized the cache %+v, want "+
					"the code host refused", size)
			}
			msg := err.Error()
			want := []string{cold.Dir, "git.cache_dir", "git.cache_max_bytes", "git.max_transfers",
				fmt.Sprintf("%d bytes free", tc.free), fmt.Sprintf("%d transfers", tc.transfers),
				fmt.Sprintf("%d bytes", host.SpoolAllowance(tc.transfers)),
				fmt.Sprintf("patch spools %d", query.PatchSpoolAllowance)}
			if tc.stores {
				want = append(want, "stream.store_max_bytes",
					fmt.Sprintf("may still write %d bytes under %s", tc.broker, tc.limit))
			}
			for _, w := range want {
				if !strings.Contains(msg, w) {
					t.Errorf("the refusal does not say %q: %s", w, msg)
				}
			}
			if !tc.stores && strings.Contains(msg, "stream.store_max_bytes") {
				t.Errorf("a volume no broker writes to was told to lower the broker's "+
					"limit: %s", msg)
			}

			// THE OPERATOR'S OWN BUDGET on the same volume is theirs to set.
			explicit := b
			explicit.Git.CacheMaxBytes = 4 * gib
			size, err = codeCacheBudget(&explicit, cold, tc.transfers, volume)
			if err != nil || size.budget != 4*gib || size.derived {
				t.Errorf("an explicit budget on a volume with no room sized %+v (%v), want "+
					"4 GiB as written", size, err)
			}
		})
	}
}

// THE REFERENCE LAYOUT CONVERGES WITHOUT EVER OVERCOMMITTING ITS VOLUME. The
// broker beside the cache re-derives its limit from the free space at every
// start, which the cache's last budget has spent, and the cache re-derives its
// budget from what that limit leaves: boot after boot, with the cache full to
// its budget each time, the broker's limit and the cache's budget together
// stay within the volume, and the cache settles rather than growing without
// end.
func TestTheReferenceLayoutConverges(t *testing.T) {
	t.Parallel()
	const gib = int64(1) << 30
	b := config.DefaultBootstrap()
	base := t.TempDir()
	b.Store.Path = filepath.Join(base, "crewlet.db")
	layout := prepareCache(t, filepath.Join(base, "git"))
	defer layout.Release()

	const volume = 100 * gib
	var budget int64
	for boot := range 20 {
		// THE CACHE HOLDS ITS LAST BUDGET, which is not free; the broker,
		// storing nothing, derives three quarters of what is.
		free := volume - budget
		at := layout
		at.Held = budget
		size, err := codeCacheBudget(&b, at, 1, cacheVolume{
			free: func(string) (int64, error) { return free, nil },
			same: func(string, string) bool { return true },
			broker: func() (jetstream.VolumeClaim, bool, error) {
				return jetstream.VolumeClaim{Dir: "stream", Limit: free / 4 * 3}, true, nil
			},
		})
		if err != nil {
			t.Fatalf("boot %d: %v", boot, err)
		}
		if size.budget+size.broker > volume {
			t.Fatalf("boot %d: the cache's %d and the broker's %d overcommit a volume of %d",
				boot, size.budget, size.broker, volume)
		}
		budget = size.budget
	}
	if budget < volume/6 || budget > volume/4 {
		t.Errorf("the cache settled at %d of a %d volume, want the fifth the arithmetic gives",
			budget, volume)
	}
}

// prepareCache is cache.Prepare of dir, failing the test on an error.
func prepareCache(t *testing.T, dir string) cache.Layout {
	t.Helper()
	layout, err := cache.Prepare(t.Context(), dir)
	if err != nil {
		t.Fatalf("cache.Prepare: %v", err)
	}
	return layout
}

// A DIRECTORY NOT MADE YET IS ON THE FILESYSTEM IT WILL BE MADE ON: a tenant
// the cache's volume may share — the snapshots before the first one — is read
// at its nearest existing ancestor, never as unreadable.
//
// Mutation: drop the ancestor walk, and the directory not made yet is an error.
func TestAVolumeNotMadeYetIsItsAncestors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	parent, err := statVolume(dir)
	if err != nil {
		t.Fatalf("statVolume(%s): %v", dir, err)
	}
	child, err := statVolume(filepath.Join(dir, "not", "made", "yet"))
	if err != nil {
		t.Fatalf("a directory not made yet could not be read: %v", err)
	}
	if child.dev != parent.dev || child.fsid != parent.fsid {
		t.Fatalf("a directory not made yet reads as %+v, its parent as %+v", child, parent)
	}
}

// TWO DIRECTORIES ARE ONE VOLUME WHEN THEY DRAW ON ONE POOL OF SPACE, which a
// device id alone does not say: a btrfs subvolume and a quota-less ZFS dataset
// each have a device id of their own and write into the space every other one
// of their pool writes into. So the device id, the filesystem id and the space
// left read at one instant each say "one volume", the last only once the
// cache's own reading held still across the other's; and everything uncertain
// — an unreadable directory, a volume that never holds still — reads as one.
//
// The space left is compared within the COARSER OF THE TWO BLOCK SIZES, never
// to the byte: each dataset of a ZFS pool reports the pool's space rounded down
// to its own recordsize, so a store's dataset at 16 KiB beside a cache's at the
// default 128 KiB reported the same space only when the pool's free bytes
// happened to fall within 16 KiB of a 128 KiB boundary — one boot in eight —
// and at every other boot the cache took seven eighths of the pool.
//
// A FILESYSTEM OF NO BLOCKS IS APART AT ONCE, however the cache's volume
// moves: the space left is the only reading the instant is for, and it can
// make nothing one with procfs. Read for an instant first, a cache volume a
// suite's other cases wrote to between every pair of reads was judged to share
// with /proc once its reads ran out (TestAnotherFilesystemIsAnotherVolume, in
// a loaded run).
//
// Mutation: compare the device ids alone, and the pooled rows go red; compare
// the space left to the byte, and the row of two block sizes goes red; ask
// whether the cache's volume moved before whether either filesystem has
// blocks, and the row of procfs beside a busy disk goes red.
func TestTwoDirectoriesOnOnePoolAreOneVolume(t *testing.T) {
	t.Parallel()
	const kib = int64(1) << 10
	disk := volumeStat{dev: 1, fsid: [2]int32{7, 1}, blocks: 1000, bsize: 4 * kib, avail: 500 << 20}
	// ONE ZFS POOL WITH 500 MiB AND 100 KiB FREE, read through two datasets:
	// each rounds the pool's space down to its own recordsize.
	pool := 500<<20 + 100*kib
	cacheDataset := volumeStat{dev: 1, fsid: [2]int32{7, 1}, blocks: 1000, bsize: 128 * kib,
		avail: pool / (128 * kib) * (128 * kib)}
	storeDataset := volumeStat{dev: 2, fsid: [2]int32{8, 2}, blocks: 64000, bsize: 16 * kib,
		avail: pool / (16 * kib) * (16 * kib)}
	for _, tc := range []struct {
		name   string
		cache  []volumeStat // its successive readings; the last repeats
		other  volumeStat
		failed bool
		same   bool
	}{
		{"one filesystem", []volumeStat{disk}, disk, false, true},
		{"one filesystem id on two devices",
			[]volumeStat{disk}, volumeStat{dev: 2, fsid: disk.fsid, blocks: 9, avail: 3}, false, true},
		// A btrfs subvolume masks its root into its filesystem id, and a ZFS
		// dataset counts only what it refers to in its blocks; the space left
		// is the reading both share with their pool.
		{"two subvolumes of one pool",
			[]volumeStat{disk}, volumeStat{dev: 2, fsid: [2]int32{8, 2}, blocks: 1200, bsize: 4 * kib,
				avail: disk.avail},
			false, true},
		{"two datasets of one pool with different block sizes",
			[]volumeStat{cacheDataset}, storeDataset, false, true},
		{"two filesystems", []volumeStat{disk},
			volumeStat{dev: 2, fsid: [2]int32{8, 2}, blocks: 1000, bsize: 4 * kib, avail: 300 << 20},
			false, false},
		{"two filesystems a block apart",
			[]volumeStat{disk}, volumeStat{dev: 2, fsid: [2]int32{8, 2}, blocks: 1000, bsize: 4 * kib,
				avail: disk.avail - 4*kib},
			false, false},
		{"two filesystems giving no filesystem id", []volumeStat{{dev: 1, blocks: 1000, avail: 5}},
			volumeStat{dev: 2, blocks: 1000, avail: 6}, false, false},
		{"a filesystem of no blocks beside another of none", []volumeStat{{dev: 1, fsid: [2]int32{1, 1}}},
			volumeStat{dev: 2, fsid: [2]int32{2, 2}}, false, false},
		{"a cache volume that moved once, then held still",
			[]volumeStat{disk, {dev: 1, fsid: disk.fsid, blocks: 1000, avail: 400 << 20}},
			volumeStat{dev: 2, fsid: [2]int32{8, 2}, blocks: 1000, avail: 300 << 20}, false, false},
		{"a cache volume that never holds still", nil,
			volumeStat{dev: 2, fsid: [2]int32{8, 2}, blocks: 1000, avail: 300 << 20}, false, true},
		// PROCFS BESIDE A BUSY DISK: no instant is needed, since no reading of
		// space makes a filesystem of no blocks one with anything.
		{"a cache volume that never holds still beside a filesystem of no blocks", nil,
			volumeStat{dev: 2, fsid: [2]int32{2, 2}}, false, false},
		{"a directory that cannot be read", []volumeStat{disk}, volumeStat{}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reads := 0
			read := func(dir string) (volumeStat, error) {
				if dir == "other" {
					if tc.failed {
						return volumeStat{}, errors.New("permission denied")
					}
					return tc.other, nil
				}
				reads++
				if tc.cache == nil {
					// EVERY READING DIFFERENT: a volume written to between
					// every pair of calls.
					return volumeStat{dev: 1, fsid: disk.fsid, blocks: 1000, avail: int64(reads)}, nil
				}
				return tc.cache[min(reads-1, len(tc.cache)-1)], nil
			}
			if got := sameVolumeBy(read, "cache", "other"); got != tc.same {
				t.Fatalf("sameVolumeBy = %t, want %t", got, tc.same)
			}
		})
	}
}

// git.max_transfers' DESCRIPTION SAYS WHAT THE POOL RESOLVES ITS ZERO TO —
// four transfers a process slot, and no more than one for every
// gitproc.TransferShare of the node's memory. The schema and the docs read the
// description, and config cannot import the pool that owns the figures, so
// the two are held together here, where both are importable.
func TestTheMaxTransfersDescriptionIsThePoolsDefault(t *testing.T) {
	t.Parallel()
	field, ok := reflect.TypeFor[config.Git]().FieldByName("MaxTransfers")
	if !ok {
		t.Fatal("config.Git has no MaxTransfers")
	}
	desc := field.Tag.Get("desc")
	share := fmt.Sprintf("%d MiB", gitproc.TransferShare>>20)
	if gitproc.TransfersPerProcess != 4 || !strings.Contains(desc, "four for each of max_processes") ||
		!strings.Contains(desc, share) {
		t.Fatalf("git.max_transfers says %q; the pool resolves 0 to %d a process slot and one "+
			"for every %s of memory", desc, gitproc.TransfersPerProcess, share)
	}
}

// THE HOST IS STOPPED ONCE, AND WAITED FOR ONE BUDGET, however many ask. The
// teardown and the runtime's shutdown both stop it, and a host whose git work
// outlives the bound is exactly the stop that runs the whole budget: asked
// again, it would wait a second budget of the process's termination grace for
// work a first wait already gave up on. Here a stop that holds until its bound
// is asked for twice, the way the teardown and the shutdown ask: it runs once,
// and the two together take one budget.
//
// Mutation: drop the once, and the second call runs the stop again and the
// pair takes two budgets.
func TestTheCodeHostIsStoppedOnceWithinOneBudget(t *testing.T) {
	t.Parallel()
	const budget = 200 * time.Millisecond
	var runs atomic.Int32
	held := func(ctx context.Context) {
		runs.Add(1)
		<-ctx.Done() // git work that outlives the bound
	}
	stop := boundedStop{budget: budget}
	start := time.Now()
	stop.stop(t.Context(), held) // the teardown's
	stop.stop(t.Context(), held) // the shutdown's
	took := time.Since(start)
	if runs.Load() != 1 {
		t.Fatalf("the stop ran %d times, want once", runs.Load())
	}
	if took >= 2*budget {
		t.Fatalf("two stops took %v, two budgets of %v", took, budget)
	}

	// A CALLER WHOSE CONTEXT HAS ENDED STILL GETS THE WHOLE BOUND: the stop
	// is often asked for because it ended.
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	var waited time.Duration
	fresh := boundedStop{budget: budget}
	fresh.stop(ended, func(ctx context.Context) {
		begun := time.Now()
		<-ctx.Done()
		waited = time.Since(begun)
	})
	if waited < budget/2 {
		t.Fatalf("a stop asked for on an ended context waited %v of its %v bound", waited, budget)
	}
}

// EVERY FACTS READ OF A HOME HAS A SPOOL. A node's facts reads are its own
// home's in the steady state — the router's walk asks a repository's home
// first — and a home asks at most as many at once as its loop works
// repositories, so the query server spools exactly that many patches at once:
// fewer, and a home's own reads would wait on each other and turn unready; more,
// and the cache's room would be sized beside spools nothing asks for. The two
// packages may not import each other, so the engine, which builds both, holds
// them equal here.
//
// Mutation: set query.MaxPatchSpools to one — red.
func TestEveryFactsReadOfAHomeHasASpool(t *testing.T) {
	t.Parallel()
	if query.MaxPatchSpools != codehome.Concurrency {
		t.Errorf("a git node spools %d patches at once and its home asks %d facts at once, want them equal",
			query.MaxPatchSpools, codehome.Concurrency)
	}
}
