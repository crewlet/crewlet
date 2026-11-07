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

// shadowInterval is how often the fleet's standing alarms are read: the alarm
// table's own evaluation beat, so a raised alarm shadows within one beat of
// the node that raised it publishing it.
const shadowInterval = statelog.AlarmInterval

// shadowTimeout bounds one read of the fleet's alarms, well inside a beat.
const shadowTimeout = shadowInterval / 2

// shadowing reports whether a standing alarm is one a shadowed class waits out.
func shadowing(standing []string) bool {
	return slices.ContainsFunc(ShadowAlarms, func(k statelog.Kind) bool {
		return slices.Contains(standing, string(k))
	})
}

// watchAlarms keeps [Reporter.shadow] current while the reporter is on, until
// ctx ends. Off, it reads nothing.
func (r *Reporter) watchAlarms(ctx context.Context) {
	ticker := time.NewTicker(shadowInterval)
	defer ticker.Stop()
	for {
		if r.active.Load() {
			r.readAlarms(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-r.enabled:
		}
	}
}

// readAlarms reads the fleet's standing alarms once. A FLEET WHOSE MARKS COULD
// NOT BE READ SHADOWS: the read failing is itself the coordination store not
// answering, and counting through it is the mistake the shadow prevents.
func (r *Reporter) readAlarms(ctx context.Context) {
	defer quietly("alarms")
	read, cancel := context.WithTimeout(ctx, shadowTimeout)
	defer cancel()
	standing, err := r.cfg.Alarms(read)
	if ctx.Err() != nil {
		return
	}
	shadowed := err != nil || shadowing(standing)
	if r.shadow.Swap(shadowed) != shadowed {
		log.Info("issue_reports_shadow", "shadowed", shadowed)
	}
}
