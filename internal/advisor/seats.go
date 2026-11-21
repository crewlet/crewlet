package advisor

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/worklet"
)

// ADDING OR REMOVING A SEAT costs money either way, so these classes stand
// only on exact numbers: each seat's busy time is the sum of its turns' own
// durations, its stopped time the spans its own records say it could not take
// a turn (paused, out of its model's limit, stopped on its budget), and its
// idle time what is left of its capacity. They say nothing until those
// numbers cover the whole window, the group of seats was the same for all of
// it, and it worked enough items to judge.

// CAPACITY IS ITS AVAILABLE TIME TIMES ITS sandbox.max_concurrent_runs, the
// turns it runs at once ([Staffing.capacity]): turns that overlap add their durations,
// and never more than that many overlap, so busy never reads above 100%. A
// union of the turns' intervals cannot be read (a turn's segments are spread
// over the data nodes, and only its ending record states its duration), and
// would call a seat with a slot free all day fully busy.

// The staffing classes: the owner's to add a seat like the busiest of a group
// whose work waits for it, and a note that a group of idle seats could lose one.
const (
	ClassAddSeat    Class = "capacity.add_seat"
	ClassRemoveSeat Class = "capacity.remove_seat"
)

// SubjectGroup is the agent seats of one unit doing one job, named by the unit
// and the group's lowest handle.
const SubjectGroup SubjectKind = "group"

// The windows the classes read: a week for adding a seat, two for removing one.
const (
	AddSeatDays    = 7
	RemoveSeatDays = 14
)

// The thresholds, judgment calls to be tuned on the first weeks of data.
const (
	// minWaitADay and minWaitDays: four item-hours a day of work wanting the
	// group's seats on average, at least an hour of it on four of the days.
	minWaitADay = 4 * time.Hour
	minWaitDay  = time.Hour
	minWaitDays = 4
	// maxIdlePercent and minBusyPercent: every seat idle a tenth or less of
	// its capacity and busy six tenths or more.
	maxIdlePercent = 10
	minBusyPercent = 60
	// minItemsWorked: ten items in the window, so a week is a sample.
	minItemsWorked = 10
	// minRemoveIdlePercent and maxRemoveWait: every seat idle six tenths or
	// more, and at most an hour of work waiting in two weeks.
	minRemoveIdlePercent = 60
	maxRemoveWait        = time.Hour
)

// Staffing is what the staffing classes read over one window of whole
// company days.
type Staffing struct {
	From, To time.Time
	// Seats is each agent seat's time over the window, by handle.
	Seats map[string]SeatTime
	// Documents is every company document active at some instant of the
	// window, the one active at its start first.
	Documents []*config.Company
	// Flow is how long each seat's items stood in each flow state over the
	// window, and DueByDay each day's item-hours due, by seat.
	Flow     []worklet.FlowHours
	DueByDay []map[string]time.Duration
	// Items is how many items each seat had in progress in the window.
	Items map[string]int
}

// SeatTime is one agent seat's time over a window.
type SeatTime struct {
	// Busy is the sum of its turns' durations; Stopped the time its own
	// records say it could not take a turn.
	Busy, Stopped time.Duration
	// Measured says every day it took turns measured its busy time, and its
	// stops were read whole: without it the seat's numbers are not judged.
	Measured bool
}

// available is the window less the seat's stopped time.
func (s Staffing) available(t SeatTime) time.Duration { return max(s.To.Sub(s.From)-t.Stopped, 0) }

// capacity is the turn-time role r's seat could have spent over the window: its
// available time for each turn it runs at once. THE CAP THE ROLE HOLDS NOW, in
// the document judged: a revision that changed it mid-window makes the figure
// approximate across the change, where [Staffing.unchanged] holds only the
// seats themselves to the window.
func (s Staffing) capacity(t SeatTime, r *config.Role) time.Duration {
	return s.available(t) * time.Duration(r.Seat().Sandbox.ConcurrentRuns())
}

// idle is what is left of the seat's capacity after its busy time.
func (s Staffing) idle(t SeatTime, r *config.Role) time.Duration {
	return max(s.capacity(t, r)-t.Busy, 0)
}

// windowSaid is the window as a finding names it.
func (s Staffing) windowSaid() string {
	return s.From.Format(time.DateOnly) + " to " + s.To.Format(time.DateOnly)
}

// seatGroup is the agent seats of one unit whose roles say one job.
type seatGroup struct {
	unit  *config.Unit
	roles []*config.Role
}

// key is what a group is known by across documents: its unit and its job.
func groupKey(unit string, r *config.Role) string {
	return unit + "\x00" + r.Goal + "\x00" + strings.Join(r.Responsibilities, "\x1f")
}

// groupsOf is every group of c's agent seats, each one's seats in the chart's
// order, the groups in the order their first seat comes.
func groupsOf(c *config.Company) []seatGroup {
	var out []seatGroup
	at := map[string]int{}
	add := func(unit *config.Unit, role *config.Role) {
		if role.Kind == org.KindHuman {
			return
		}
		id := ""
		if unit != nil {
			id = unit.ID
		}
		k := groupKey(id, role)
		i, ok := at[k]
		if !ok {
			i = len(out)
			at[k] = i
			out = append(out, seatGroup{unit: unit})
		}
		out[i].roles = append(out[i].roles, role)
	}
	for i := range c.Roles {
		add(nil, &c.Roles[i])
	}
	for unit := range c.EachUnit() {
		for i := range unit.Roles {
			add(unit, &unit.Roles[i])
		}
	}
	return out
}

// handles is the group's seat handles, sorted.
func (g seatGroup) handles() []string {
	out := make([]string, 0, len(g.roles))
	for _, r := range g.roles {
		out = append(out, r.Seat().Handle())
	}
	slices.Sort(out)
	return out
}

// subject is the group as a finding names it.
func (g seatGroup) subject() Subject {
	unit := ""
	if g.unit != nil {
		unit = g.unit.ID
	}
	return Subject{Kind: SubjectGroup, Unit: unit, Seat: g.handles()[0],
		Name: unit + "/" + g.handles()[0]}
}

// unchanged reports whether every document active over the window held the
// same seats in g's place and job.
func (s Staffing) unchanged(g seatGroup) bool {
	if len(s.Documents) == 0 {
		return false
	}
	unit := ""
	if g.unit != nil {
		unit = g.unit.ID
	}
	want, key := g.handles(), groupKey(unit, g.roles[0])
	for _, doc := range s.Documents {
		var got []string
		for _, other := range groupsOf(doc) {
			id := ""
			if other.unit != nil {
				id = other.unit.ID
			}
			if groupKey(id, other.roles[0]) == key {
				got = other.handles()
			}
		}
		if !slices.Equal(got, want) {
			return false
		}
	}
	return true
}

// groupFlow is a group's item-hours over the window: due (work that wanted a
// seat and did not get it), held for any cause, and the days with at least
// [minWaitDay] due.
type groupFlow struct {
	due, held time.Duration
	dueDays   int
	items     int
}

func (s Staffing) flowOf(g seatGroup) groupFlow {
	in := map[string]bool{}
	for _, h := range g.handles() {
		in[h] = true
	}
	var out groupFlow
	for _, f := range s.Flow {
		switch {
		case !in[f.Seat]:
		case f.State == worklet.FlowDue:
			out.due += f.Stood
		case f.State == worklet.FlowHeld:
			out.held += f.Stood
		}
	}
	for _, day := range s.DueByDay {
		var due time.Duration
		for h := range in {
			due += day[h]
		}
		if due >= minWaitDay {
			out.dueDays++
		}
	}
	for h := range in {
		out.items += s.Items[h]
	}
	return out
}

// unjudged says which seat groups of at least least seats s could not judge
// and why, "" where it judged them all: the classes stay silent rather than
// judge a partial week, and the run says so (ADR-0166).
func (s *Staffing) unjudged(c *config.Company, least int) string {
	if s == nil {
		return "The tracker keeps no flow record, so no seat's time can be judged."
	}
	groups, changed := 0, 0
	var unmeasured []string
	for _, g := range groupsOf(c) {
		if len(g.roles) < least {
			continue
		}
		groups++
		switch {
		case !s.measured(g):
			for _, h := range g.handles() {
				if t, ok := s.Seats[h]; (!ok || !t.Measured) && !slices.Contains(unmeasured, h) {
					unmeasured = append(unmeasured, h)
				}
			}
		case !s.unchanged(g):
			changed++
		}
	}
	var why []string
	if len(unmeasured) > 0 {
		slices.Sort(unmeasured)
		why = append(why, fmt.Sprintf("%s not measured every day of the window (%s)",
			countSaid(len(unmeasured), "seat's time was", "seats' time was"), strings.Join(unmeasured, ", ")))
	}
	if changed > 0 {
		why = append(why, countSaid(changed, "group changed its seats", "groups changed their seats")+
			" during the window")
	}
	if len(why) == 0 {
		return ""
	}
	return "Some seat groups could not be judged: " + strings.Join(why, "; ") + "."
}

// countSaid is n with the singular or plural words that follow it.
func countSaid(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// measured reports whether every seat of g has numbers to judge.
func (s Staffing) measured(g seatGroup) bool {
	for _, h := range g.handles() {
		if t, ok := s.Seats[h]; !ok || !t.Measured {
			return false
		}
	}
	return true
}

// addSeat is one finding per group whose work waited for its seats day after
// day while every seat of it was busy against its capacity: the owner's to let
// its seats run one more turn at once, or to add a seat like its busiest.
// Silent without exact numbers over the whole week, an unchanged group, and
// ten items worked.
func addSeat(c *config.Company, ev Evidence) []Finding {
	s := ev.StaffingWeek
	if s == nil {
		return nil
	}
	days := s.To.Sub(s.From).Hours() / 24
	var out []Finding
	for _, g := range groupsOf(c) {
		if !s.measured(g) || !s.unchanged(g) {
			continue
		}
		flow := s.flowOf(g)
		if flow.items < minItemsWorked || flow.due < time.Duration(days*float64(minWaitADay)) ||
			flow.dueDays < minWaitDays || flow.held >= flow.due {
			continue
		}
		var busiest *config.Role
		busy, avail := time.Duration(0), time.Duration(0)
		ok := true
		for _, r := range g.roles {
			t := s.Seats[r.Seat().Handle()]
			a := s.capacity(t, r)
			if a == 0 || s.idle(t, r)*100 > maxIdlePercent*a || t.Busy*100 < minBusyPercent*a {
				ok = false
				break
			}
			busy, avail = busy+t.Busy, avail+a
			if busiest == nil || t.Busy > s.Seats[busiest.Seat().Handle()].Busy {
				busiest = r
			}
		}
		if !ok {
			continue
		}
		f := stallFinding(ClassAddSeat, AreaSetup, g.subject(),
			fmt.Sprintf("%s item-hours waited for %s, its seats busy %d%% of their capacity, %s",
				hoursSaid(flow.due), seatsSaid(g), percentOf(busy, avail), s.windowSaid()),
			[]Fact{{Key: "wait_minutes", Value: int64(flow.due / time.Minute)},
				{Key: "busy_percent", Value: int64(percentOf(busy, avail))},
				{Key: "items", Value: int64(flow.items)}, {Key: "seats", Value: int64(len(g.roles))}},
			nil, addSeatFix(c, g, busiest))
		out = append(out, f)
	}
	return out
}

// removeSeat is one finding per group of two or more seats every one of which
// sat idle most of two weeks while nothing waited for them: a note that one
// seat could go. Silent on the same terms as [addSeat].
func removeSeat(c *config.Company, ev Evidence) []Finding {
	s := ev.StaffingFortnight
	if s == nil {
		return nil
	}
	var out []Finding
	for _, g := range groupsOf(c) {
		if len(g.roles) < 2 || !s.measured(g) || !s.unchanged(g) {
			continue
		}
		flow := s.flowOf(g)
		if flow.due > maxRemoveWait || flow.held > 0 {
			continue
		}
		var least *config.Role
		idle, avail := time.Duration(0), time.Duration(0)
		ok := true
		for _, r := range g.roles {
			t := s.Seats[r.Seat().Handle()]
			a := s.capacity(t, r)
			if a == 0 || s.idle(t, r)*100 < minRemoveIdlePercent*a {
				ok = false
				break
			}
			idle, avail = idle+s.idle(t, r), avail+a
			if least == nil || t.Busy < s.Seats[least.Seat().Handle()].Busy {
				least = r
			}
		}
		if !ok {
			continue
		}
		f := engineFinding(ClassRemoveSeat, g.subject(),
			fmt.Sprintf("%s idle %d%% of their capacity, %s", seatsSaid(g), percentOf(idle, avail),
				s.windowSaid()),
			[]Fact{{Key: "idle_percent", Value: int64(percentOf(idle, avail))},
				{Key: "wait_minutes", Value: int64(flow.due / time.Minute)}, {Key: "seats", Value: int64(len(g.roles))}},
			nil)
		f.Actor, f.Area, f.Fix = ActorOwner, AreaSetup, removeSeatFix(c, g, least)
		out = append(out, f)
	}
	return out
}

// percentOf is part as a whole percentage of whole.
func percentOf(part, whole time.Duration) int {
	if whole <= 0 {
		return 0
	}
	return int(part * 100 / whole)
}

// seatsSaid names a group's seats as a person reads them.
func seatsSaid(g seatGroup) string {
	names := make([]string, 0, len(g.roles))
	for _, r := range g.roles {
		names = append(names, r.Name)
	}
	return strings.Join(names, ", ")
}

// rolesOf is the unit's roles as JSON values a unit write sends, from the
// redacted document where redact (what a finding keeps and a person sends: the
// write restores a masked credential), else from c (what the check applies).
func rolesOf(c *config.Company, unitID string, redact bool) ([]any, bool) {
	doc := c
	if redact {
		doc = c.Redact()
	}
	for unit := range doc.EachUnit() {
		if unit.ID != unitID {
			continue
		}
		raw, err := json.Marshal(unit.Roles)
		if err != nil {
			return nil, false
		}
		var out []any
		return out, json.Unmarshal(raw, &out) == nil
	}
	return nil, false
}

// cloneLeftOut are the template's fields a clone does not copy: what names a
// seat, how it is reached, its secrets and its schedules (a schedule is the
// template's own duty).
var cloneLeftOut = []string{"handle", "name", "email", "avatar", "contact", "mcp_env", "schedules"}

// addSeatFix offers, FIRST, every seat of g running one more turn at once —
// the same seats and their context, at the cost of their boxes running side
// by side — and then one seat cloned from template into its unit, with the
// first free name and handle; a group in no unit opens the roles.
func addSeatFix(c *config.Company, g seatGroup, template *config.Role) Fix {
	handle := template.Seat().Handle()
	sentence := fmt.Sprintf("Let these seats run one more turn at once (sandbox.max_concurrent_runs), "+
		"or add a seat like %s: the same job, model and limits under a new name.", template.Name)
	open := Fix{Kind: FixOpenSetting, Options: []Option{}, Setting: "roles", Sentence: sentence,
		Unchanged: "Every other seat stays as it is."}
	if g.unit == nil || g.unit.ID == "" {
		return open
	}
	name, newHandle := freeSeat(c, template)
	withClone := func(redact bool) (Change, bool) {
		roles, ok := rolesOf(c, g.unit.ID, redact)
		if !ok {
			return Change{}, false
		}
		var clone map[string]any
		for _, r := range roles {
			if m, isMap := r.(map[string]any); isMap && m["handle"] == handle {
				clone = maps.Clone(m)
			}
		}
		if clone == nil {
			return Change{}, false
		}
		for _, k := range cloneLeftOut {
			delete(clone, k)
		}
		clone["name"], clone["handle"] = name, newHandle
		return Change{Entity: EntityUnits, ID: g.unit.ID, Set: map[string]any{"roles": append(roles, clone)},
			Summary: fmt.Sprintf("Add %s to %s, like %s", newHandle, g.unit.ID, handle)}, true
	}
	var options []Option
	if ch, ok := raiseConcurrentRuns(c, g, true); ok {
		if checked, checkable := raiseConcurrentRuns(c, g, false); checkable && CheckChange(c, checked) == nil {
			options = append(options, Option{Label: "Run one more turn at once on each seat", Change: ch})
		}
	}
	ch, ok := withClone(true)
	checked, checkable := withClone(false)
	if ok && checkable && CheckChange(c, checked) == nil {
		options = append(options, Option{Label: fmt.Sprintf("Add %s, like %s", name, template.Name), Change: ch})
	}
	if len(options) == 0 {
		return open
	}
	return Fix{Kind: FixConfigChange, Options: options, Sentence: sentence,
		Unchanged: fmt.Sprintf("A new seat copies no contact, credentials or schedules from %s: add any "+
			"it needs by hand.", template.Name)}
}

// raiseConcurrentRuns is g's unit with each of g's seats taking one more turn at
// once than the sandbox.max_concurrent_runs it holds now, from the redacted
// document where redact ([rolesOf]); false where a seat is not found.
func raiseConcurrentRuns(c *config.Company, g seatGroup, redact bool) (Change, bool) {
	roles, ok := rolesOf(c, g.unit.ID, redact)
	if !ok {
		return Change{}, false
	}
	raised := map[string]int{}
	for _, r := range g.roles {
		raised[r.Seat().Handle()] = r.Seat().Sandbox.ConcurrentRuns() + 1
	}
	found := 0
	for i, r := range roles {
		m, isMap := r.(map[string]any)
		handle, _ := m["handle"].(string)
		runs, in := raised[handle]
		if !isMap || !in {
			continue
		}
		sandbox, _ := m["sandbox"].(map[string]any)
		sandbox = maps.Clone(sandbox)
		if sandbox == nil {
			sandbox = map[string]any{}
		}
		sandbox["max_concurrent_runs"] = runs
		m = maps.Clone(m)
		m["sandbox"] = sandbox
		roles[i] = m
		found++
	}
	return Change{Entity: EntityUnits, ID: g.unit.ID, Set: map[string]any{"roles": roles},
			Summary: fmt.Sprintf("Let %s run one more turn at once", strings.Join(g.handles(), ", "))},
		found == len(g.roles)
}

// freeSeat is the first "<Name> N" and "<handle>-N" no seat of c holds.
func freeSeat(c *config.Company, template *config.Role) (string, string) {
	taken := map[string]bool{}
	for role := range c.EachRole() {
		taken[role.Seat().Handle()], taken["\x00"+role.Name] = true, true
	}
	for n := 2; ; n++ {
		name, handle := fmt.Sprintf("%s %d", template.Name, n), fmt.Sprintf("%s-%d", template.Seat().Handle(), n)
		if !taken[handle] && !taken["\x00"+name] {
			return name, handle
		}
	}
}

// removeSeatFix offers the group's unit without its least busy seat; a seat in
// no unit opens the roles.
func removeSeatFix(c *config.Company, g seatGroup, least *config.Role) Fix {
	handle := least.Seat().Handle()
	open := Fix{Kind: FixOpenSetting, Options: []Option{}, Setting: "roles",
		Sentence:  fmt.Sprintf("Remove %s, the least busy of these seats.", least.Name),
		Unchanged: "The other seats stay; the removed seat's open items are handed back once it is gone."}
	if g.unit == nil || g.unit.ID == "" {
		return open
	}
	without := func(redact bool) (Change, bool) {
		roles, ok := rolesOf(c, g.unit.ID, redact)
		if !ok {
			return Change{}, false
		}
		kept := slices.DeleteFunc(slices.Clone(roles), func(r any) bool {
			m, isMap := r.(map[string]any)
			return isMap && m["handle"] == handle
		})
		return Change{Entity: EntityUnits, ID: g.unit.ID, Set: map[string]any{"roles": kept},
			Summary: fmt.Sprintf("Remove %s from %s", handle, g.unit.ID)}, len(kept) < len(roles)
	}
	ch, ok := without(true)
	checked, checkable := without(false)
	if !ok || !checkable || CheckChange(c, checked) != nil {
		return open
	}
	return Fix{Kind: FixConfigChange, Options: []Option{{Label: fmt.Sprintf("Remove %s", least.Name), Change: ch}},
		Sentence: fmt.Sprintf("Remove %s, the least busy of these seats.", least.Name),
		Unchanged: "The other seats stay; the removed seat's open items are handed back after the " +
			"tracker's grace for a seat that left."}
}
