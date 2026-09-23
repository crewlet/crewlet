package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam/authevents"
	"github.com/crewlet/crewlet/internal/statelog"
)

// startAuthEvents builds this node's authentication audit trail and starts the
// loop that publishes each closed minute of failed attempts.
//
// ONE TRAIL PER PROCESS, built here rather than by each surface, because the
// dedupe it keeps is the node's: a Tier A token's first use in an hour is one
// row whether the request reached the guard through REST or the socket, and a
// replayed cookie ends its sessions once however many surfaces saw it. Two
// trails would be two opinions about "once".
//
// BEFORE THE NATIVE ESTATE, because both the identity writer and the state log's
// appliers announce through it, and AFTER New's failure guard is armed, so a
// boot that fails later stops the loop with everything else.
func (e *Engine) startAuthEvents() error {
	// NIL COUNTS NOTHING, and a nil *Recorder is not nil once it is an
	// interface — so the absence is passed as an absent interface.
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
	// DETACHED, like every loop the node owns: SIGTERM does not end it,
	// [Engine.stopAuthEvents] does — and its last act is to publish the
	// minute still open, which a loop that died with the caller's context
	// would drop.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		trail.Run(ctx)
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
// AFTER the native estate has stopped — its appliers witness refused records
// through the trail — and BEFORE the backends close, because the final flush
// publishes onto the broker they own. Nil-safe, for a boot that failed before
// the trail existed.
func (e *Engine) stopAuthEvents() {
	if e.stopAuthTrail != nil {
		e.stopAuthTrail()
		e.stopAuthTrail = nil
	}
}

// AuthEvents is this node's authentication audit trail: what the sign-in
// surface, the request guard and the identity directory hand what they saw to.
//
// Never nil on an engine [New] returned, which is what lets the API refuse to
// be built without it rather than quietly auditing nothing.
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
// THE RUNNER DECIDES WHAT IS SAID and how often — once per verdict and key id,
// capped — so this only translates: the framework's verdict becomes the event
// type it names, on the ordinary path.
type refusalWitness struct{ trail *authevents.Trail }

// RecordRefused implements [statelog.Witness].
func (w refusalWitness) RecordRefused(ctx context.Context, r statelog.Refusal) {
	switch r.Verdict {
	case statelog.KeyUnknown:
		w.trail.Emit(ctx, types.RecordUnverifiable{
			Domain: r.Domain, KeyID: r.KeyID, Held: r.Held,
			Position: r.Position.String(),
		})
	case statelog.Tampered:
		w.trail.Emit(ctx, types.RecordTampered{
			Domain: r.Domain, KeyID: r.KeyID, Held: r.Held,
			Position: r.Position.String(),
		})
	}
}
