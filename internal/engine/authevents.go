package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam/authevents"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/statelog"
)

// startAuthEvents builds this node's authentication audit trail and starts the
// loop that publishes each closed minute of failed attempts.
//
// ONE TRAIL PER PROCESS, because its tally is the node's: a failed sign-in and
// a refused bearer from one source in one minute are one count only on one
// trail. Started BEFORE the native estate, whose identity writer and appliers
// announce through it, and AFTER New's failure guard is armed, so a boot that
// fails later stops the loop with everything else.
func (e *Engine) startAuthEvents(ctx context.Context) error {
	// A nil *Recorder is not nil once it is an interface, so the absence is
	// passed as an absent interface.
	var counter authevents.Counter
	if e.metrics != nil {
		counter = e.metrics
	}
	trail, err := authevents.New(authevents.Options{
		Publisher: e.backends.Queue, Counter: counter, Node: e.id,
	})
	if err != nil {
		return fmt.Errorf("engine: authentication audit trail: %w", err)
	}
	// Detached from the boot's CANCELLATION only (keeping its values):
	// [Engine.stopAuthEvents] ends it, and its last act publishes the minute
	// still open, which a loop dying with the caller's context would drop.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		trail.Run(runCtx)
	}()
	e.authEvents = trail
	e.stopAuthTrail = func() {
		cancel()
		<-done
	}
	return nil
}

// stopAuthEvents ends the trail's loop, publishing what it still holds.
//
// AFTER the native estate has stopped (its appliers witness through the trail)
// and BEFORE the backends close (the final flush publishes onto their broker).
// Nil-safe, for a boot that failed before the trail existed.
func (e *Engine) stopAuthEvents() {
	if e.stopAuthTrail != nil {
		e.stopAuthTrail()
		e.stopAuthTrail = nil
	}
}

// AuthEvents is this node's authentication audit trail: what the sign-in
// surface, the request guard and the identity directory hand what they saw to.
//
// Never nil on an engine [New] returned, so the API can refuse to be built
// without it rather than quietly audit nothing.
func (e *Engine) AuthEvents() *authevents.Trail { return e.authEvents }

// stateLogWitness is the state log's witness over this node's trail, or nil when
// the node has none — an absent interface rather than one holding a nil trail.
func (e *Engine) stateLogWitness() statelog.Witness {
	if e.authEvents == nil {
		return nil
	}
	return refusalWitness{trail: e.authEvents}
}

// refusalWitness puts a record a state log would not authenticate onto the
// audit feed.
//
// The runner decides what is said and how often (once per verdict and key id,
// capped); this only translates the verdict into the event type it names.
type refusalWitness struct{ trail *authevents.Trail }

// RecordRefused implements [statelog.Witness].
func (w refusalWitness) RecordRefused(ctx context.Context, r statelog.Refusal) {
	switch r.Verdict {
	case recordseal.KeyUnknown:
		w.trail.Emit(ctx, types.RecordUnverifiable{
			Domain: r.Domain, KeyID: r.KeyID, Held: r.Held,
			Position: r.Position.String(),
		})
	case recordseal.Tampered:
		w.trail.Emit(ctx, types.RecordTampered{
			Domain: r.Domain, KeyID: r.KeyID, Held: r.Held,
			Position: r.Position.String(),
		})
	}
}
