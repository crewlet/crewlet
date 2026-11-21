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

	// Owner, Streams and Reason are the hold's own ([coord.TrimHold]); its At
	// is stamped by every put.
	Owner   string
	Streams map[string]coord.Position
	Reason  string

	// Every is how often the pin is renewed: [TrimHoldHeartbeat] in the
	// engine; a parameter so a case can renew on a cadence it can wait out.
	// Zero or below is refused: a pin that never renews is the defect this
	// type removes.
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
	// base is the taking context without its cancellation; the release
	// writes and logs under it.
	base context.Context
	stop context.CancelFunc
	done chan struct{}
	once sync.Once
}

// HoldTail writes a pin on the replay tail and renews it every spec.Every
// until [TailPin.Release].
//
// The trim ignores a hold older than [TrimHoldStale] so a crashed holder
// cannot pin the log forever, and so reads an unrenewed pin as crashed: a
// transfer running for hours would lose its tail. Every holder (a backup, a
// join) shares this one renewal. It belongs to the pin, not to ctx, since a
// join's pin outlives the call that took it ([AdoptDeps.Hold]); the release
// joins its goroutine. A refused renewal is logged and carried (the stale
// bound allows three missed beats); a refused FIRST put is returned, and
// nothing is held.
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

// Release stops renewing the pin and removes it from the register. Only the
// first call, from any goroutine, does anything. It writes under a context
// detached from the pin's cancellation, which is often the failure being
// undone (a dead one would leave the pin standing until stale), and joins the
// renewal FIRST so no beat in flight puts the pin back.
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
