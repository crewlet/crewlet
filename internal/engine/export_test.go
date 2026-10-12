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

// WithSweepEvery is opts with this node's seat host sweeping placement every
// every rather than every [seat.SweepInterval], for a test outside the package
// that must tell a claim the sweep's tick made from one something else asked
// for.
func WithSweepEvery(opts Options, every time.Duration) Options {
	opts.sweepEvery = every
	return opts
}

// WithSeatRetryBase is opts with a seat's per-role child that would not start
// retried first after base rather than after [seatServerRetryBase], for a test
// outside the package that must watch a retry bring the child up.
func WithSeatRetryBase(opts Options, base time.Duration) Options {
	opts.seatRetryBase = base
	return opts
}

// SettleSeatServers waits until no seat this node holds has a pass over its
// per-role children in flight, for a test outside the package that reads what
// an apply's reconcile of those children did: the apply hands each seat's
// pass to the seat's runner and returns without waiting for it, so a read
// straight after the apply sees the children as the apply found them. A
// runner is chosen before the apply returns, so every pass it asked for has
// ended — its surface filed — when this does.
func SettleSeatServers(e *Engine) {
	e.mcpMu.Lock()
	held := make([]*seatChildren, 0, len(e.seatMCP))
	for _, s := range e.seatMCP {
		held = append(held, s)
	}
	e.mcpMu.Unlock()
	for _, s := range held {
		s.mu.Lock()
		for s.running {
			s.idle.Wait()
		}
		s.mu.Unlock()
	}
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
