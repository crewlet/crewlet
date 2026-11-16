package advisor

import (
	"fmt"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/config"
)

// ClassScheduleUnproductive is a schedule whose fires over the window rarely
// moved any work: the owner's to run less often or remove.
const ClassScheduleUnproductive Class = "schedule.unproductive"

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

// writesUnseen reports a seat granted a vendor home for work, where the engine
// keeps no record of what each turn wrote: GitHub with a write permission,
// GitLab, Jira or Confluence.
func writesUnseen(in config.RoleIntegrations) bool {
	on := func(enabled *bool) bool { return enabled == nil || *enabled }
	if g := in.GitHub; g != nil && on(g.Enabled) && slices.Contains(slices.Collect(maps.Values(g.Permissions)), config.GitHubWrite) {
		return true
	}
	return in.GitLab != nil && on(in.GitLab.Enabled) || in.Jira != nil && on(in.Jira.Enabled) ||
		in.Confluence != nil && on(in.Confluence.Enabled)
}

// unseenWriters is the handle of every seat of c that [writesUnseen].
func unseenWriters(c *config.Company) map[string]bool {
	out := map[string]bool{}
	for role := range c.EachRole() {
		if writesUnseen(role.Integrations) {
			out[role.Seat().Handle()] = true
		}
	}
	return out
}

// seen reports whether every agent p's schedule runs on is outside unseen, so
// its fires can be judged.
func (p place) seen(unseen map[string]bool) bool {
	if p.role != nil {
		return !unseen[p.role.Seat().Handle()]
	}
	for _, r := range p.unit.Unit().Roles {
		if unseen[r.Handle()] {
			return false
		}
	}
	return true
}

// seenSchedule reports whether the schedule k names is in c and its fires can
// be judged.
func seenSchedule(c *config.Company, k scheduleKey, unseen map[string]bool) bool {
	p, ok := placeOfSchedule(c, k)
	return ok && p.seen(unseen)
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
	tallies, unseen := talliesOf(ev.Fires), unseenWriters(c)
	unproductive := map[scheduleKey]bool{}
	for k, t := range tallies {
		if t.fires < minUnproductiveFires || t.moved*100 > movedPercent*t.fires {
			continue
		}
		p, ok := placeOfSchedule(c, k)
		if !ok || !p.seen(unseen) {
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
			// THE WEEK'S TALLY IS EVIDENCE, never words, so every frequent
			// schedule's finding says the same.
			if t, ok := tallies[keyOf(f.Subject)]; ok && seenSchedule(c, keyOf(f.Subject), unseen) {
				f.Evidence = append(f.Evidence, Fact{Key: "fires", Value: int64(t.fires)},
					Fact{Key: "moved", Value: int64(t.moved)})
			}
		}
		out = append(out, f)
	}
	return out
}

// unproductiveFinding is the owner's cost for one schedule, with
// schedule.frequent's fix: run it once a day, or remove it. Its words are every
// such schedule's; its name and the week's tally are its Item.
func unproductiveFinding(c *config.Company, p place, t fireTally) Finding {
	s, _ := p.theSchedule()
	return Finding{
		Class: ClassScheduleUnproductive, Severity: SeverityCosts, Actor: ActorOwner,
		Source: SourceEvidence, Area: AreaSchedules, Subject: p.subject(),
		Title: "Schedules that rarely change anything.",
		Why: "In the last 7 days each of its fires started a turn on the agent's models, and " +
			"almost none moved any work: no work item, page or code changed, only comments or " +
			"nothing. A check that waits for something to happen is better as a work item " +
			"dated for when to look.",
		Item:     fmt.Sprintf("%s (%s in %d runs)", s.Name, changedSaid(t.moved), t.fires),
		Evidence: []Fact{{Key: "fires", Value: int64(t.fires)}, {Key: "moved", Value: int64(t.moved)}},
		Fix:      scheduleFix(c, p, config.Warning{}),
	}
}

// changedSaid is how often a schedule's runs changed something, as its item
// says it.
func changedSaid(n int) string {
	switch n {
	case 0:
		return "changed nothing"
	case 1:
		return "changed something once"
	case 2:
		return "changed something twice"
	}
	return fmt.Sprintf("changed something %d times", n)
}
