package engine

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/membership"
)

// mapInterval is how often the estate map's holder brings it up to date: the
// maintainer's own tick, which every absence is counted in — so the duty runs
// it at exactly the cadence its grace was derived from
// ([membership.OutTicks]).
const mapInterval = membership.TickInterval

// mapDutyTTL is three ticks, the ratio every singleton here uses: one missed
// tick must not hand a map to a peer mid-change.
const mapDutyTTL = 3 * mapInterval

// mapAwaitedPoll is how soon a map's duty ticks again after a tick that found
// no stored map where one is wanted.
//
// ONE SECOND, and only until a map exists. Without the object map no node can
// store a file — every upload is refused as unavailable — and without the
// estate map at a partitioned layout no partition has a holder, so nothing in
// the estate can be served. And the first tick of a booting fleet routinely
// finds nothing to place on: it runs as the engine is built, racing this
// node's own membership lease, whose first claim is on a goroutine of its own.
// At the reconcile interval that cost every fresh fleet fifteen seconds with no
// object store (measured in internal/e2e, a seat's first upload failing on
// exactly that). A tick with no map is one lease listing and one key read, so a
// second is cheap, and the first map written ends it.
//
// It counts no absence, and that is guaranteed by WHAT IT IS KEYED ON: the
// tick's own report that it found no map in the store where one is wanted
// ([mapTick]), never this node's cache of the map. The cache is a second
// reading on its own refresh — it lags a map the maintainer just read, and
// refuses one it cannot decode — and a poll keyed on it ticked every second
// against a map that existed, counting each absence fifteen times too fast.
// And never where no map is wanted: under the single-file layout the estate
// map is never written, and a duty polling for it would read the store every
// second for ever.
const mapAwaitedPoll = time.Second

// mapTick is what one tick of a map's maintainer found, as its duty paces on
// it.
type mapTick struct {
	// mapped is whether the store held the map when the tick read it, and
	// so whether the tick counted one tick of every open absence.
	mapped bool

	// awaited is whether the store held no map where one is wanted.
	awaited bool
}

// mapDuty is this node's part in a map's duty: ask for it, and tick while it
// holds it.
type mapDuty struct {
	// event names the map in the duty's log lines: object_map, estate_map.
	event string

	// claim answers whether this node holds the duty, nil where it has no
	// node to claim it through and ticks alone.
	claim func(context.Context) (bool, error)

	tick func(context.Context) (mapTick, error)

	// released is told of every turn that does not hold the duty, nil where
	// the map keeps nothing per tenure: whatever it did while it held the
	// duty may have been undone by the holder since.
	released func()
}

// turn is one turn of the duty, answering how long to wait before the next.
//
// [mapInterval] after a tick that found a map, WHATEVER ELSE HAPPENED: that
// tick counted every open absence, and the grace is [membership.OutTicks] of
// them only if they are that far apart. The poll only after a tick that read
// the store cleanly and found no map where one is wanted. And the interval
// after everything else:
//
//   - A turn that does not hold the duty counts nothing, and asks again at the
//     tick's cadence — the ratio every singleton's lease is sized to — since
//     the node that holds it is the one bringing a first map in. Asking every
//     second would be a lease write per second from every other node.
//   - A tick that failed before it could say is retried at the tick's cadence
//     too: the poll is sized for a read that answers, and a store that is
//     failing is not asked every second by a loop that cannot help it.
//   - A tick that found no map and wants none — the estate map under the
//     single-file layout — has nothing to wait for sooner.
func (d mapDuty) turn(ctx context.Context) time.Duration {
	if d.claim != nil {
		mine, err := d.claim(ctx)
		if err != nil && ctx.Err() == nil {
			// SAID, as every other duty says it: "a peer holds it" and
			// "the store could not be asked" are the same silence here and
			// very different situations to an operator reading why the
			// map stopped moving.
			log.WarnContext(ctx, d.event+"_duty_unclaimed", "error", err)
		}
		if err != nil || !mine {
			if d.released != nil {
				d.released()
			}
			return mapInterval
		}
	}
	res, err := d.tick(ctx)
	if err != nil && ctx.Err() == nil {
		log.WarnContext(ctx, d.event+"_not_maintained", "error", err)
	}
	if res.mapped || err != nil || !res.awaited {
		return mapInterval
	}
	return mapAwaitedPoll
}
