package engine

import (
	"context"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/providers/credential"
)

// The fleet's credential cooldowns, joined to the pools that enforce them.
//
// A rate limit belongs to the KEY, at the vendor, not to the process that
// discovered it. Four nodes running one company share one bag of API keys, so
// without this each of them pays its own 429 to learn what the first already
// knew — and with a two-key pool that is four wasted calls and four turns
// slowed for one quota window. Worse, the pool's own cooldowns are measured
// on a monotonic clock, so the values are not even COMPARABLE across
// processes: there is no arithmetic that turns one node's "benched at 1m42s"
// into another's.
//
// The join is deliberately two halves that never meet on the request path:
//
//   - PUBLISH is synchronous with the bench, inside the pool, on the failure
//     that caused it. That is the only moment the fact exists, and delaying it
//     to the next tick would leave a window in which every peer rediscovers it.
//   - PULL is a ticker, here. A cooldown runs for a minute at the very least,
//     so reading one a few seconds late costs nothing — while a coordination
//     read in front of every model call would put the store's latency under
//     every turn and its availability under the whole company.

// cooldownRefresh is how often this node pulls what its peers have benched.
//
// Anchored to the SHORTEST cooldown an operator can configure: 60 seconds
// (internal/config/providers.go, minCooldownSeconds). Pulling every 15 s
// observes even that one with three quarters of its life left, which is the
// point of sharing at all — a horizon so coarse that the record lapses before
// anyone reads it would be sharing in name only. It also matches the fleet's
// existing 15 s reconcile cadence, so a node's coordination traffic stays on
// one rhythm rather than beating against a second.
const cooldownRefresh = 15 * time.Second

// cooldowns is this node's refresher: one loop for every pool in the epoch.
//
// On the ENGINE rather than on an epoch, for the same reason the retention
// sweep is: it is a loop this PROCESS runs. Rebuilding it on an apply would
// leave two loops pulling the same records, and stopping it on one would stop
// the other's too.
type cooldowns struct {
	stop chan struct{}
	done chan struct{}
	once sync.Once

	// cancel ends an in-flight pull.
	//
	// The stop channel alone is not enough: the loop's context is
	// DETACHED, so a pull parked inside the coordination store answers to
	// nothing, and stopCooldownRefresh's bare `<-done` would then wait on
	// it for ever — hanging the whole shutdown behind a broker that
	// stopped replying.
	cancel context.CancelFunc
}

// shareCooldowns attaches the fleet's ledger to every pool in an epoch.
//
// Called from equip, so it runs on the boot epoch and on every applied one,
// BEFORE the epoch is published — a turn can start the instant the pointer
// moves, and a provider whose pool was still unshared would bench a key
// nobody else would hear about.
//
// The scope is the CONFIG ENTRY'S KEY, which is what makes the ledger's
// namespacing correct: every node in the fleet builds its pools from the same
// company document, so "zulu" names the same (model, endpoint, key bag) triple
// everywhere. See credential.fleetKey for why a bare key hint would be wrong.
//
// A node with no coordination store shares nothing and says so once. That is
// the single-node case, where there is no peer to tell.
func (e *Engine) shareCooldowns(c *Company) {
	if c == nil || c.Models == nil {
		return
	}
	// A NIL LEDGER IS A VALID ONE and it means detach, which is why this
	// is passed through rather than branched around: an epoch rebuilt on a
	// node that has lost its coordination store must stop publishing
	// through the handle the previous one held, and a pool nothing ever
	// shared behaves exactly as it did before any of this existed.
	fleet := e.backends.Fleet
	shared := 0
	for key, provider := range c.Models.All() {
		// Not every backend HAS a pool. A cli-agent provider holds one
		// login in a directory rather than a bag of keys, so there is
		// nothing to rotate and nothing to share — the type assertion is
		// the honest way to ask, because "does this backend rotate
		// credentials" is exactly what it answers.
		pooled, ok := provider.(interface{ Pool() *credential.Pool })
		if !ok {
			continue
		}
		pooled.Pool().Share(key, fleet)
		shared++
	}
	if fleet == nil {
		log.Debug("credential_cooldowns_local",
			"reason", "this node has no coordination store",
			"detail", "cooldowns stay this process's own, which is correct "+
				"for a single node and invisible on a fleet")
		return
	}
	log.Info("credential_cooldowns_shared", "pools", shared)
}

// startCooldownRefresh arms the pull.
//
// Detached from the caller's context for the same reason the retention sweep
// is: a loop bound to a signal context would stop at SIGTERM while turns are
// still finishing, and a turn that starts during the drain deserves the same
// view of the fleet's cooldowns as one that started a minute earlier.
func (e *Engine) startCooldownRefresh(ctx context.Context) {
	if e.backends.Fleet == nil {
		return
	}
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	e.cooldowns = &cooldowns{
		stop: make(chan struct{}), done: make(chan struct{}), cancel: cancel,
	}
	go e.cooldowns.run(loopCtx, e.refreshCooldowns)
}

// run pulls immediately and then on the tick.
//
// IMMEDIATELY, because the interesting moment is a node that has just started:
// it has an empty bench and the fleet may have a key benched for the next
// hour. Waiting a full interval would spend that node's first turns
// rediscovering exactly what this exists to tell it.
func (c *cooldowns) run(ctx context.Context, pull func(context.Context)) {
	defer close(c.done)
	ticker := time.NewTicker(cooldownRefresh)
	defer ticker.Stop()
	for {
		pull(ctx)
		select {
		case <-c.stop:
			return
		case <-ticker.C:
		}
	}
}

// refreshCooldowns pulls the fleet's bench into every pool in the current
// epoch, and reports what it changed.
//
// Read off the epoch on every tick rather than captured once: an apply
// replaces the whole provider registry, and a loop holding the boot epoch's
// pools would be refreshing backends no turn is using.
func (e *Engine) refreshCooldowns(ctx context.Context) {
	// BOUNDED BY THE INTERVAL ITSELF, the same rule syncSeatMemory states:
	// this loop's context is detached, and context.WithoutCancel strips the
	// DEADLINE as well as the cancellation — so without a bound of its own
	// a pull against a wedged coordination store blocks for ever. That
	// matters more here than the lost tick: FleetStore.Since iterates keys,
	// which the client's default per-API timeout does not cover, and
	// stopCooldownRefresh waits on this loop. A pull that cannot finish
	// before the next one is due has fallen behind anyway, and the next
	// tick re-reads the whole bench, so nothing is lost by abandoning it.
	ctx, cancel := context.WithTimeout(ctx, cooldownRefresh)
	defer cancel()

	c := e.Company()
	if c == nil || c.Models == nil {
		return
	}
	for key, provider := range c.Models.All() {
		pooled, ok := provider.(interface{ Pool() *credential.Pool })
		if !ok {
			continue
		}
		pool := pooled.Pool()
		applied, err := pool.Refresh(ctx)
		if err != nil {
			// One line per failing pull, not per pool: the ledger is the
			// same store for all of them, so a warning each would say the
			// same thing N times a tick for as long as it is down.
			log.WarnContext(ctx, "credential_cooldowns_unreadable",
				"provider", key, "error", err,
				"detail", "this node keeps its own cooldowns and will "+
					"rediscover a peer's the expensive way")
			return
		}
		if applied == 0 {
			continue
		}
		// WHICH KEYS, and for how long. This is the only answer to the
		// question the feature itself creates — "why is a key benched on
		// a node that never saw a failure" — and without it the state is
		// invisible from the outside.
		for _, stat := range pool.Stats() {
			if stat.Cooling <= 0 {
				continue
			}
			log.InfoContext(ctx, "credential_cooled_by_peer",
				"provider", key, "hint", stat.Hint,
				"cooldown_seconds", stat.Cooling.Seconds())
		}
	}
}

// stopCooldownRefresh ends the pull, waiting for an in-flight one.
//
// Waits, rather than signalling and moving on, because the pull writes into
// pools a draining turn may still be leasing from — and a refresher racing a
// shutdown is a data race that only ever appears under -race on a busy node.
func (e *Engine) stopCooldownRefresh() {
	if e.cooldowns == nil {
		return
	}
	// CANCEL INSIDE the once, before the wait: the pull is what the wait is
	// waiting for, and on a store that has stopped answering the stop
	// channel alone leaves it parked past its own bound's worst case.
	e.cooldowns.once.Do(func() {
		e.cooldowns.cancel()
		close(e.cooldowns.stop)
	})
	<-e.cooldowns.done
}
