package issuereport

import (
	"context"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// ShadowAlarms are the environment alarms a shadowed class waits out: each is
// evaluated from a level the host or the fleet is at (a volume, a pool, a
// clock, a feed, an applier), so while one stands the failures it causes look
// like the engine's own and are not counted. A panic class is never shadowed.
var ShadowAlarms = []statelog.Kind{
	statelog.KindVolumeLow, statelog.KindWALLarge, statelog.KindPoolStarved,
	statelog.KindClockSkew, statelog.KindFeedStopped, statelog.KindFloorUnknown,
	statelog.KindMaintenanceOpen, statelog.KindApplyLag,
}

// shadowInterval is how often the fleet is read: the alarm
// table's own evaluation beat, so a raised alarm shadows within one beat of
// the node that raised it publishing it.
const shadowInterval = statelog.AlarmInterval

// shadowTimeout bounds one read of the fleet, well inside a beat.
const shadowTimeout = shadowInterval / 2

// shadowing reports whether a standing alarm is one a shadowed class waits out.
func shadowing(standing []string) bool {
	return slices.ContainsFunc(ShadowAlarms, func(k statelog.Kind) bool {
		return slices.Contains(standing, string(k))
	})
}

// Fleet is what the reporter reads of the whole fleet every alarm beat.
type Fleet struct {
	// Alarms are the alarm kinds standing on any node.
	Alarms []string
	// Moving is a fleet handing seats between nodes by design: a node is
	// draining, or the live nodes run more than one build (a rolling
	// upgrade), a node that does not say its build counting as one more.
	Moving bool
}

// unread is the shadow before any reading answered: a reporter that can read
// the fleet counts nothing a reading could shadow until one has.
func (r *Reporter) unread() {
	r.shadow.Store(r.cfg.Fleet != nil)
	r.moving.Store(r.cfg.Fleet != nil)
}

// watchFleet keeps the shadow current while the reporter is on, until ctx
// ends. Off, it reads nothing.
func (r *Reporter) watchFleet(ctx context.Context) {
	ticker := time.NewTicker(shadowInterval)
	defer ticker.Stop()
	for {
		if r.active.Load() {
			r.readFleet(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-r.enabled:
		}
	}
}

// readFleet reads the fleet once. A FLEET THAT COULD NOT BE READ SHADOWS
// EVERYTHING: the read failing is itself the coordination store not answering,
// and counting through it is the mistake the shadow prevents.
func (r *Reporter) readFleet(ctx context.Context) {
	defer quietly("fleet")
	read, cancel := context.WithTimeout(ctx, shadowTimeout)
	defer cancel()
	fleet, err := r.cfg.Fleet(read)
	if ctx.Err() != nil {
		return
	}
	shadowed, moving := err != nil || shadowing(fleet.Alarms), err != nil || fleet.Moving
	wasShadowed, wasMoving := r.shadow.Swap(shadowed), r.moving.Swap(moving)
	if wasShadowed != shadowed || wasMoving != moving {
		log.Info("issue_reports_shadow", "shadowed", shadowed, "moving", moving)
	}
}
