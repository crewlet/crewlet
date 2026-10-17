package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// Reflects reports whether e has built its reflect dispatcher, for a test
// outside the package that drives an engine through its public surface.
func Reflects(e *Engine) bool { return e.reflector.Load() != nil }

// ItemRanker is the ranker the tracker's item search is built with, so the
// serving-seams audit outside the package reflects over the fields it reads
// rather than over a description of them a new field would leave behind.
type ItemRanker = itemRanker

// LearningDutyName is the duty every background learning pass claims, so a
// test outside the package reads the key the engine claims rather than a copy
// of it a rename would leave behind.
const LearningDutyName = learningDutyName

// WithUsageFlushEvery is opts with this node's usage publisher ticking every
// every rather than every [usage.FlushInterval], for a test outside the
// package that must see the engine keep a LIVE loop running.
func WithUsageFlushEvery(opts Options, every time.Duration) Options {
	opts.usageEvery = every
	return opts
}

// WithSweepEvery is opts with this node's seat host sweeping placement every
// every rather than every [seat.SweepInterval], for a test outside the package
// that must tell a claim the sweep's tick made from one something else asked
// for.
func WithSweepEvery(opts Options, every time.Duration) Options {
	opts.sweepEvery = every
	return opts
}

// WithCacheDisk is opts with the code cache sized as though its volume had free
// bytes free and an embedded broker storing its streams there could still
// write broker bytes — one volume holding every tenant, as the single-volume
// layout does — for a case outside the package whose subject is the budget a
// disk derives, which the machine running it would otherwise decide.
func WithCacheDisk(opts Options, free, broker int64) Options {
	opts.cacheDisk = &cacheVolume{
		free: func(string) (int64, error) { return free, nil },
		same: func(string, string) bool { return true },
		broker: func() (jetstream.VolumeClaim, bool, error) {
			return jetstream.VolumeClaim{Dir: "stream", Limit: broker}, true, nil
		},
	}
	return opts
}

// WithGitMemory is opts with the git pool sizing its lanes, and the code cache
// budgeting its background work, from a node of memory bytes rather than from
// the memory of the machine running the case ([gitproc.Options.Memory]), for a
// case outside the package whose subject is what a node's memory leaves its
// git cache.
func WithGitMemory(opts Options, memory int64) Options {
	opts.gitMemory = memory
	return opts
}

// TestGitTransfers is the transfer lane [PinGitLanes] gives a case's node:
// four, 256 MiB of request spool.
const TestGitTransfers = 4

// PinGitLanes sets b's git.max_transfers to [TestGitTransfers], for every
// case booting a node that may build a code host. The transfer lane sizes the
// request spools the cache's room is taken out of (host.SpoolAllowance), and
// unset it follows the machine's cores — four a core, as memory allows. Beside
// a broker on the same temporary volume, which claims three quarters of its
// free space, a sixteen-core machine's 4 GiB of spools left a node no room
// under 16 GiB free, and its node refused to host code: a property of the
// machine running the suite, never of the case. Pinned, the room is a quarter
// of the free space less 256 MiB, which every volume a node boots on has.
func PinGitLanes(b *config.Bootstrap) { b.Git.MaxTransfers = TestGitTransfers }

// SeedStore writes the migrated store image ([storetest.Seed]) where b's store
// opens, for a case booting a node on a path of its own: the node's file
// unless the store is scratch — discarded at open, so a seed would only be
// deleted — and the replicated file beside it only when the node holds the
// estate ([HoldsEstate]), since a fresh node without `data` has no such file
// and a backup or an estate check would find one. A file already there is
// left as it is, so a case booting twice on one path reopens its own.
//
// Call it once b is final — its roles and paths set — and before the first
// open. NOT for a case whose subject is the fresh file itself: the boot that
// creates and migrates the estates, or a store that cannot open.
func SeedStore(t testing.TB, b *config.Bootstrap) {
	t.Helper()
	if b.Store.Scratch {
		return
	}
	storetest.Seed(t, store.EstateNode, b.Store.Path)
	if HoldsEstate(b) {
		storetest.Seed(t, store.EstateReplicated,
			store.ReplicatedPath(b.Store.Path, b.Store.ReplicatedPath))
	}
}
