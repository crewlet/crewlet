package statelog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// HoldWriter is the slice of the fleet's trim-hold register a pin writes
// ([coord.HoldRegister]).
type HoldWriter interface {
	PutHold(ctx context.Context, h coord.TrimHold) error
	ReleaseHold(ctx context.Context, owner string) error
}

// TailHold is a pin on the replay tail, as [HoldTail] is asked to take and keep
// it.
type TailHold struct {
	// Register is where the pin is written and renewed.
	Register HoldWriter

	// Owner, Streams and Reason are the hold's own — see [coord.TrimHold].
	// Its At is stamped by every put, so it is not asked for.
	Owner   string
	Streams map[string]coord.Position
	Reason  string

	// Every is how often the pin is renewed: [TrimHoldHeartbeat], which is
	// what every holder in the engine passes. A parameter so a case can
	// renew on a cadence it can wait out. Zero or below is refused — a pin
	// that never renewed is the defect this type exists to remove.
	Every time.Duration

	// Now stamps each put. Nil is the wall clock, in UTC.
	Now func() time.Time

	// Logger is where a missed renewal and a failed release are said. Nil is
	// the package's own component logger.
	Logger *slog.Logger
}

// TailPin is a pin [HoldTail] took and is renewing. [TailPin.Release] ends it.
type TailPin struct {
	owner    string
	register HoldWriter
	log      *slog.Logger
	// base is the context the pin was taken under, detached from its
	// cancellation: what the release is logged and written under.
	base context.Context
	stop context.CancelFunc
	done chan struct{}
	once sync.Once
}

// HoldTail writes a pin on the replay tail and renews it every spec.Every
// until it is released.
//
// # Why a pin renews itself
//
// The trim ignores a hold older than [TrimHoldStale], which is what stops a
// holder that crashed pinning the log for the life of the deployment. The same
// rule reads a pin nobody renews as a crashed holder — exactly right for a
// crashed holder, and exactly wrong for a forty-minute copy of a large store
// or a snapshot transfer that runs for hours. The join's pin was put once and
// never renewed: a minute into any transfer it pinned nothing, and the trim
// could pass the tail the transfer was fetching.
//
// # One implementation, for every holder
//
// A backup and a join hold the tail for the same reason, against the same
// arithmetic, so the renewal, its cadence ([TrimHoldHeartbeat]) and the
// release are written once, here. Two copies is how one of them came to have
// no renewal at all.
//
// THE RENEWAL BELONGS TO THE PIN, not to the context it was taken under: it
// runs until [TailPin.Release], on a goroutine the release joins, because a
// pin is held for as long as its holder needs the tail — which for a join
// outlives the call that took it ([AdoptDeps.Hold]). A renewal the register
// refuses is logged and carried: a missed beat is survivable, the stale bound
// allows three. The FIRST put is not: a holder that cannot pin the log at all
// is told, and holds nothing.
func HoldTail(ctx context.Context, spec TailHold) (*TailPin, error) {
	switch {
	case spec.Register == nil:
		return nil, errors.New("statelog: a pin on the replay tail has no " +
			"register to be written to")
	case spec.Every <= 0:
		return nil, fmt.Errorf("statelog: a pin on the replay tail renewed every "+
			"%s is never renewed, and the trim ignores it once it is older than "+
			"%s — pass statelog.TrimHoldHeartbeat", spec.Every, TrimHoldStale)
	}
	now := spec.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	put := func(ctx context.Context) error {
		return spec.Register.PutHold(ctx, coord.TrimHold{
			Owner: spec.Owner, At: now(), Streams: spec.Streams, Reason: spec.Reason,
		})
	}
	if err := put(ctx); err != nil {
		return nil, fmt.Errorf("statelog: put the trim hold %s: %w", spec.Owner, err)
	}
	base := context.WithoutCancel(ctx)
	beat, stop := context.WithCancel(base)
	pin := &TailPin{
		owner: spec.Owner, register: spec.Register, log: loggerOr(spec.Logger),
		base: base, stop: stop, done: make(chan struct{}),
	}
	go func() {
		defer close(pin.done)
		ticker := time.NewTicker(spec.Every)
		defer ticker.Stop()
		for {
			select {
			case <-beat.Done():
				return
			case <-ticker.C:
				if err := put(beat); err != nil && beat.Err() == nil {
					pin.log.WarnContext(beat, "statelog_hold_not_renewed",
						"owner", spec.Owner, "error", err.Error(),
						"detail", "the trim ignores a pin older than "+
							TrimHoldStale.String()+", so a pin that misses three "+
							"renewals in a row stops protecting the tail its "+
							"holder needs; the next beat tries again")
				}
			}
		}
	}()
	return pin, nil
}

// Release stops renewing the pin and removes it from the register. It is safe
// to call more than once, and from any goroutine: only the first call does
// anything.
//
// WRITTEN UNDER A CONTEXT THAT OUTLIVES THE FAILURE. The failure a release is
// undoing is often the cancellation itself, and a release that inherited a
// dead context would leave the pin standing until the stale bound expired it.
// The renewal is joined FIRST, so no beat in flight can put the pin back after
// it has been removed.
func (p *TailPin) Release() {
	if p == nil {
		return
	}
	p.once.Do(func() {
		p.stop()
		<-p.done
		if err := p.register.ReleaseHold(p.base, p.owner); err != nil {
			p.log.WarnContext(p.base, "statelog_hold_not_released",
				"owner", p.owner, "error", err.Error(),
				"detail", "the trim ignores it once it is older than "+
					TrimHoldStale.String()+", so this delays trimming rather "+
					"than stopping it")
		}
	})
}
