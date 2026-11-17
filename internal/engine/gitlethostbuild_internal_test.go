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
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	gitlethome "github.com/crewlet/crewlet/internal/gitlet/home"
	"github.com/crewlet/crewlet/internal/gitlet/host"
	"github.com/crewlet/crewlet/internal/gitlet/query"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// THE GITLET ROLE IS NEEDED ONLY WHERE THE COMPANY HOSTS CODE, and every other
// role always: the fleet's unmanned-role check asks this on every sweep, off
// the current epoch, so a fleet that declared its roles without gitlet is told
// nobody serves it only once code is hosted.
func TestTheGitletRoleIsNeededOnlyWhereCodeIsHosted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		company *Company
		git     bool
	}{
		{"no company", nil, false},
		{"a company hosting no code", &Company{Config: &config.Company{}}, false},
		{"a company hosting code", &Company{Config: &config.Company{Code: config.Code{Backend: config.CodeGitlet}}}, true},
	} {
		e := &Engine{}
		if tc.company != nil {
			e.epoch.current.Store(tc.company)
		}
		for _, role := range placement.Vocabulary() {
			want := role != placement.RoleGitlet || tc.git
			if got := e.roleNeeded(role); got != want {
				t.Errorf("%s: roleNeeded(%s) = %t, want %t", tc.name, role, got, want)
			}
		}
	}
}

// THE CACHE'S DERIVED BUDGET COUNTS WHAT THE CACHE HOLDS. Free space excludes
// the cache's own files, so a budget from it alone shrinks at every restart of
// a warm node and the janitor evicts the difference. The room is free space
// plus what the cache holds, less the request spools' allowance and the patch
// spools (query.PatchSpoolAllowance); a second boot over the same files derives
// the same budget, and a cache on the store's filesystem gets the shared half.
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

	// A GIBIBYTE THE LAST PROCESS LEFT IN trash/, which Prepare counts; sparse,
	// so the case costs no disk.
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

	b.Gitlet.CacheMaxBytes = 8 * gib
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
// its streams on the cache's volume may fill it to its store limit (three
// quarters of the free space where stream.store_max_bytes is unset), so the
// cache divides only what that limit leaves, and cache, broker and request
// spools together never claim more than the volume has. A broker on another
// volume claims nothing of this one, a small volume's share has no floor
// raising it, and a claim that cannot be read fails the build rather than be
// guessed. (A claim leaving no room is TestARoomOfNothingRefusesGitletByName's.)
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
			// THE ROWS' ROOMS are before the patch spools, which every room is less.
			room := tc.room + held - query.PatchSpoolAllowance
			if size.room != room || size.broker != tc.broker || size.dedicated {
				t.Fatalf("sized %+v, want a shared room of %d with the broker claiming %d",
					size, room, tc.broker)
			}
			if size.budget != config.DerivedCacheMaxBytes(size.room, false) {
				t.Errorf("budget %d, want the shared share of the room %d", size.budget, size.room)
			}
			// NEVER AN OVERCOMMIT: the cache, the broker's remaining claim, the
			// spools and the patches fit in the volume beside what it holds.
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

// A VOLUME WITH NO ROOM FOR A CACHE REFUSES GITLET BY NAME. Where the bounded
// tenants beside the cache (the broker's limit, the request spools, the patch
// spools) claim all the volume has, free and held, any derived budget is bytes
// the volume lacks. So the build is refused here, naming every figure and the
// setting that changes each, rather than by the cache's own refusal of a budget
// of nothing, which names none. An explicit gitlet.cache_max_bytes is the
// operator's decision, sized as written (code_cache_sized warns the room
// cannot hold it).
//
// Mutations: derive a budget of nothing without refusing it, and every refused
// row goes red; leave the patch spools out of the room, and their row is sized.
func TestARoomOfNothingRefusesGitletByName(t *testing.T) {
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
		// ROOM FOR THE REQUEST SPOOLS AND A QUARTER GIBIBYTE, less than the
		// patch spools take.
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
					"Gitlet refused", size)
			}
			msg := err.Error()
			want := []string{cold.Dir, "gitlet.cache_dir", "gitlet.cache_max_bytes", "gitlet.max_transfers",
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
			explicit.Gitlet.CacheMaxBytes = 4 * gib
			size, err = codeCacheBudget(&explicit, cold, tc.transfers, volume)
			if err != nil || size.budget != 4*gib || size.derived {
				t.Errorf("an explicit budget on a volume with no room sized %+v (%v), want "+
					"4 GiB as written", size, err)
			}
		})
	}
}

// THE REFERENCE LAYOUT CONVERGES WITHOUT EVER OVERCOMMITTING ITS VOLUME. The
// broker re-derives its limit at every start from the free space the cache's
// last budget left, and the cache from what that limit leaves: boot after
// boot, the cache full each time, the two stay within the volume and the cache
// settles rather than growing without end.
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

// A TEST NODE WHOSE BROKER IS DECLARED HOSTS CODE ON ANY DISK. A broker
// [brokertest.StoreIn] sizes claims a fixed 16 GiB of the room the cache's
// budget derives from, so a budget left to the derivation hosts code only with
// about 17.5 GiB free, and on the end-to-end runner's 9.2 GB the node is
// refused at engine.New. Declared beside the broker ([brokertest.GitletCacheIn]),
// the budget and the transfer lane are the same whatever the disk.
//
// Mutation: drop the budget from GitletCacheIn, and the small disks refuse; drop
// the lane, and every disk's spools follow the machine.
func TestADeclaredBrokersNodeHostsCodeOnAnyDisk(t *testing.T) {
	t.Parallel()
	const (
		gib = int64(1) << 30
		// runner is what the end-to-end gates' runner had free.
		runner = int64(9225187328)
		// cores is the lane that runner's cores derived, sixteen transfers.
		cores = 16
	)
	b := config.DefaultBootstrap()
	base := brokertest.TempDir(t)
	b.Store.Path = filepath.Join(base, "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(base, "stream"))
	layout := prepareCache(t, filepath.Join(base, "git"))
	defer layout.Release()
	disk := func(free int64) cacheVolume {
		return cacheVolume{
			free: func(string) (int64, error) { return free, nil },
			same: func(string, string) bool { return true },
			broker: func() (jetstream.VolumeClaim, bool, error) {
				return jetstream.VolumeClaim{Dir: b.Stream.StoreDir, Limit: b.Stream.StoreMaxBytes}, true, nil
			},
		}
	}

	undeclared := b
	if size, err := codeCacheBudget(&undeclared, layout, cores, disk(runner)); err == nil {
		t.Fatalf("a node whose broker is declared, its cache left to the derivation, was sized "+
			"%+v on the gates' runner, so this case does not model the refusal it guards against", size)
	}

	brokertest.GitletCacheIn(&b.Gitlet)
	for _, free := range []int64{gib, runner, 100 * gib} {
		size, err := codeCacheBudget(&b, layout, b.Gitlet.MaxTransfers, disk(free))
		if err != nil {
			t.Errorf("a node whose broker and cache are declared was refused Gitlet on a "+
				"volume with %d free: %v", free, err)
			continue
		}
		if size.derived || size.budget != brokertest.CodeCacheMaxBytes {
			t.Errorf("on a volume with %d free the cache's budget is %d (derived %t), want the "+
				"declared %d whatever the disk", free, size.budget, size.derived,
				brokertest.CodeCacheMaxBytes)
		}
		if size.transfers != brokertest.GitTransfers ||
			size.spools != host.SpoolAllowance(brokertest.GitTransfers) {
			t.Errorf("on a volume with %d free the lane is %d transfers spooling %d, want the "+
				"declared %d whatever the machine", free, size.transfers, size.spools,
				brokertest.GitTransfers)
		}
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
// device id alone does not say: a btrfs subvolume or quota-less ZFS dataset
// has its own. So a shared device id, filesystem id or space left each says
// "one", the space only once the cache's reading held still across the
// other's, and anything uncertain reads as one. The space is compared within
// the COARSER OF THE TWO BLOCK SIZES, as each ZFS dataset rounds the pool's
// space down to its recordsize. A FILESYSTEM OF NO BLOCKS (procfs) IS APART AT
// ONCE, before any wait, lest a busy cache volume be judged one with /proc.
//
// Mutation: device ids alone reddens the pooled rows; space compared to the
// byte, the row of two block sizes; asking whether the cache's volume moved
// before whether either has blocks, the row of procfs beside a busy disk.
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

// gitlet.max_transfers' DESCRIPTION SAYS WHAT THE POOL RESOLVES ITS ZERO TO —
// four transfers a process slot, and no more than one for every
// gitproc.TransferShare of the node's memory. The schema and the docs read the
// description, and config cannot import the pool that owns the figures, so
// the two are held together here, where both are importable.
func TestTheMaxTransfersDescriptionIsThePoolsDefault(t *testing.T) {
	t.Parallel()
	field, ok := reflect.TypeFor[config.Gitlet]().FieldByName("MaxTransfers")
	if !ok {
		t.Fatal("config.Gitlet has no MaxTransfers")
	}
	desc := field.Tag.Get("desc")
	share := fmt.Sprintf("%d MiB", gitproc.TransferShare>>20)
	if gitproc.TransfersPerProcess != 4 || !strings.Contains(desc, "four for each of max_processes") ||
		!strings.Contains(desc, share) {
		t.Fatalf("gitlet.max_transfers says %q; the pool resolves 0 to %d a process slot and one "+
			"for every %s of memory", desc, gitproc.TransfersPerProcess, share)
	}
}

// THE HOST IS STOPPED ONCE, AND WAITED FOR ONE BUDGET, however many ask. The
// teardown and the runtime's shutdown both stop it, and a host whose git work
// outlives the bound would otherwise wait a second budget of the termination
// grace for work the first wait gave up on.
//
// It runs in a synctest bubble: only the bound's timer moves the clock, so the
// pair takes EXACTLY one budget when the stop runs once and two when it runs
// twice, which a loaded wall clock under -race cannot tell apart.
//
// Mutation: drop the once, and the second call runs the stop again and the
// pair takes two budgets. Bound the stop by the caller's context rather than
// detaching it, and the ended caller's stop waits no time at all.
func TestGitletIsStoppedOnceWithinOneBudget(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
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
		if took != budget {
			t.Fatalf("two stops took %v, want one budget of %v", took, budget)
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
		if waited != budget {
			t.Fatalf("a stop asked for on an ended context waited %v, want its whole %v bound", waited, budget)
		}
	})
}

// EVERY FACTS READ OF A HOME HAS A SPOOL. A node's facts reads are its own
// home's in the steady state (the router asks a repository's home first), and
// a home asks at most as many at once as its loop works repositories, so the
// query server spools exactly that many: fewer, and a home's reads wait on
// each other and turn unready; more, and the cache's room is sized beside
// spools nothing asks for. The two packages may not import each other, so the
// engine, which builds both, holds them equal.
//
// Mutation: set query.MaxPatchSpools to one — red.
func TestEveryFactsReadOfAHomeHasASpool(t *testing.T) {
	t.Parallel()
	if query.MaxPatchSpools != gitlethome.Concurrency {
		t.Errorf("a Gitlet node spools %d patches at once and its home asks %d facts at once, want them equal",
			query.MaxPatchSpools, gitlethome.Concurrency)
	}
}
