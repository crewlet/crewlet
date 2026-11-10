package advisor

import (
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/config"
)

// ClassScheduleUnproductive is a schedule whose fires over the window rarely
// moved any work: the owner's to run less often or remove.
const ClassScheduleUnproductive Class = "schedule.unproductive"

// PlanWindow is a subscription plan's short window (five hours on Claude
// Code), the span an owner who reserves capacity counts turns in.
const PlanWindow = 5 * time.Hour

// minUnproductiveFires and movedPercent: 7 fires in the window, and 10% or
// fewer of them moving anything.
const (
	minUnproductiveFires = 7
	movedPercent         = 10
)

// ScheduleFire is one fire in the window whose turn could be read: the
// schedule by its seat's handle or its unit's id and its name, and whether the
// turn moved any work (wrote more than comments).
type ScheduleFire struct {
	Seat, Unit, Name string
	Moved            bool
}

// fireTally is one schedule's fires in the window and how many moved work.
type fireTally struct{ fires, moved int }

func (t fireTally) perWindow(over time.Duration) float64 {
	return float64(t.fires) * float64(PlanWindow) / float64(over)
}

// scheduleKey names a schedule as a fire and a place do.
type scheduleKey struct{ seat, unit, name string }

func talliesOf(fires []ScheduleFire) map[scheduleKey]fireTally {
	out := map[scheduleKey]fireTally{}
	for _, f := range fires {
		k := scheduleKey{f.Seat, f.Unit, f.Name}
		t := out[k]
		t.fires++
		if f.Moved {
			t.moved++
		}
		out[k] = t
	}
	return out
}

// placeOfSchedule is the document's schedule a key names, and false when the
// document no longer has it.
func placeOfSchedule(c *config.Company, k scheduleKey) (place, bool) {
	find := func(p place) (place, bool) {
		for i, s := range p.schedules() {
			if s.Name == k.name {
				p.schedule = i
				return p, true
			}
		}
		return place{}, false
	}
	if k.seat != "" {
		for role := range c.EachRole() {
			if role.Seat().Handle() == k.seat {
				return find(place{role: role, schedule: -1})
			}
		}
		return place{}, false
	}
	for unit := range c.EachUnit() {
		if unit.ID == k.unit {
			return find(place{unit: unit, schedule: -1})
		}
	}
	return place{}, false
}

// keyOf is the schedule a finding's subject names.
func keyOf(s Subject) scheduleKey {
	return scheduleKey{seat: s.Seat, unit: s.Unit, name: s.Schedule}
}

// scheduleEvidence raises schedule.unproductive for each schedule of c whose
// fires rarely moved work, and says what each schedule.frequent finding cost.
// A frequent schedule that is also unproductive is said once, as the cost.
func scheduleEvidence(c *config.Company, ev Evidence, found []Finding) []Finding {
	if ev.unread(sourceFires) {
		return found
	}
	tallies := talliesOf(ev.Fires)
	unproductive := map[scheduleKey]bool{}
	for k, t := range tallies {
		if t.fires < minUnproductiveFires || t.moved*100 > movedPercent*t.fires {
			continue
		}
		p, ok := placeOfSchedule(c, k)
		if !ok {
			continue
		}
		unproductive[k] = true
		found = append(found, unproductiveFinding(c, p, t))
	}
	out := found[:0]
	for _, f := range found {
		if f.Class == ClassScheduleFrequent {
			if unproductive[keyOf(f.Subject)] {
				continue
			}
			if t, ok := tallies[keyOf(f.Subject)]; ok {
				f.Why += " " + firesSaid(t)
				f.Evidence = append(f.Evidence, Fact{Key: "fires", Value: int64(t.fires)},
					Fact{Key: "moved", Value: int64(t.moved)})
			}
		}
		out = append(out, f)
	}
	return out
}

// unproductiveFinding is the owner's cost for one schedule, with
// schedule.frequent's fix: run it once a day, or remove it.
func unproductiveFinding(c *config.Company, p place, t fireTally) Finding {
	s, _ := p.theSchedule()
	return Finding{
		Class: ClassScheduleUnproductive, Severity: SeverityCosts, Actor: ActorOwner,
		Source: SourceEvidence, Area: AreaSchedules, Subject: p.subject(),
		Title: fmt.Sprintf("%s ran %d times and changed something %s.", s.Name, t.fires, times(t.moved)),
		Why: fmt.Sprintf("In the last 7 days each of its %d fires started a turn on the agent's "+
			"models, and %d of them moved no work: no item changed, only comments or nothing. "+
			"That is about %s turns in every five-hour plan window. A check that waits for "+
			"something to happen is better as a work item dated for when to look.",
			t.fires, t.fires-t.moved, oneDecimal(t.perWindow(Window))),
		Evidence: []Fact{{Key: "fires", Value: int64(t.fires)}, {Key: "moved", Value: int64(t.moved)}},
		Fix:      scheduleFix(c, p, config.Warning{}),
	}
}

// firesSaid is what a schedule's fires in the window did, for schedule.frequent.
func firesSaid(t fireTally) string {
	return fmt.Sprintf("In the last 7 days it fired %d times and %d of them (%d%%) moved any work.",
		t.fires, t.moved, t.moved*100/t.fires)
}

// times is how often, as a sentence says it.
func times(n int) string {
	switch n {
	case 0:
		return "no times"
	case 1:
		return "once"
	case 2:
		return "twice"
	}
	return fmt.Sprintf("%d times", n)
}

// oneDecimal is n to one decimal place, with no trailing ".0".
func oneDecimal(n float64) string {
	s := fmt.Sprintf("%.1f", n)
	if len(s) > 2 && s[len(s)-2:] == ".0" {
		return s[:len(s)-2]
	}
	return s
}
