package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// Reflects reports whether e has built its reflect dispatcher, for a test
// outside the package that drives an engine through its public surface.
func Reflects(e *Engine) bool { return e.reflector.Load() != nil }

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
