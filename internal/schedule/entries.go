package schedule

import (
	"cmp"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
)

// Entry is one schedule together with the scope that owns it.
//
// The scheduler and the dashboard's read-only projection both walk entries, so
// runner resolution has ONE implementation. Two — one for firing and one for
// display — is how a dashboard comes to show a runner that never receives the
// task.
type Entry struct {
	// Scope is whether a role or a unit declares this schedule.
	Scope types.ScheduleScope

	// ScopeID is the declaring role's HANDLE or the declaring unit's NAME.
	// Two different namespaces, deliberately: a handle is what addresses a
	// seat, and a unit has no handle at all.
	ScopeID string

	// Unit is the declaring unit, and nil for a role schedule. Runner
	// resolution needs the unit itself — its direct members, its lead — and
	// looking it up again by name on every fire would walk the tree twice
	// and could disagree with the walk that produced this entry.
	Unit *org.Unit

	// Schedule is the declaration as written in config.
	Schedule org.Schedule
}

// Entries returns every schedule in the company: each seat's own first, then
// each unit's.
//
// Read fresh on every tick, which is what makes hot reload free — an org is
// built new and swapped, never edited in place, so an added or removed
// schedule starts or stops on the next tick with nothing to wire.
func Entries(o *org.Organization) []Entry {
	if o == nil {
		return nil
	}
	var out []Entry
	for r := range o.AllRoles() {
		for _, s := range r.Schedules {
			out = append(out, Entry{Scope: types.ScheduleScopeRole, ScopeID: r.Handle(), Schedule: s})
		}
	}
	for u := range o.AllUnits() {
		for _, s := range u.Schedules {
			out = append(out, Entry{Scope: types.ScheduleScopeUnit, ScopeID: u.Name, Unit: u, Schedule: s})
		}
	}
	return out
}

// HasSchedules reports whether the company declares any schedule at all.
//
// The scheduler auto-enables on it: a company with no schedules never spins up
// a tick loop, a duty claim, or a ledger connection.
func HasSchedules(o *org.Organization) bool { return len(Entries(o)) > 0 }

// CountSchedules is how many schedules the company declares, for the startup
// log line and the dashboard's header.
func CountSchedules(o *org.Organization) int { return len(Entries(o)) }

// Runners resolves the handle(s) that should run this entry.
//
// A ROLE schedule always runs as that role; its target is ignored, because a
// schedule on a seat already names the seat. A UNIT schedule resolves from its
// target:
//
//   - lead — the unit's EFFECTIVE lead, inherited ones included. Kept because
//     it is dynamic: the schedule follows whoever leads the unit, so a
//     leadership change does not have to touch config.
//   - each (the default) — every DIRECT member, never a descendant. A standup
//     that fanned out to every descendant of a division would wake the whole
//     company.
//
// Human seats never appear. They are addressable but run no turns, so a fire
// addressed to one would sit in an inbox nothing consumes. Config validation
// rejects both human combinations up front; this filter covers the lenient
// window during a piecewise bootstrap, where the tick's `schedule_no_runners`
// warning is what surfaces the gap.
func (e Entry) Runners(o *org.Organization) []string {
	if o == nil {
		return nil
	}
	if e.Scope == types.ScheduleScopeRole {
		// ScopeID is the seat's own handle, and a human seat cannot declare
		// a role schedule (the org model forbids it), so this is an agent.
		return []string{e.ScopeID}
	}
	if e.Unit == nil {
		return nil
	}
	if e.Schedule.TargetsLead() {
		lead := o.EffectiveLead(e.Unit)
		if lead == nil || lead.IsHuman() {
			return nil
		}
		return []string{lead.Handle()}
	}
	var out []string
	for _, r := range e.Unit.Roles {
		if r.IsAgent() {
			out = append(out, r.Handle())
		}
	}
	return out
}

// Row is one schedule as the dashboard's /schedules view reads it.
//
// A flat projection rather than the org objects themselves: the view needs the
// RESOLVED answers — which timezone actually applies, who actually runs it,
// when it next fires — and none of those is a field anyone wrote down.
type Row struct {
	ScopeType types.ScheduleScope `json:"scope_type"`
	ScopeID   string              `json:"scope_id"`
	Name      string              `json:"name"`
	Cron      string              `json:"cron"`
	Timezone  string              `json:"timezone"`
	Task      string              `json:"task"`

	// Target is empty for a role schedule. A role schedule's target is not
	// "each" — it is meaningless, and reporting a default would invite a
	// reader to change it and expect something to happen.
	Target org.ScheduleTarget `json:"target"`

	Enabled        bool     `json:"enabled"`
	TimeoutSeconds int      `json:"timeout_seconds"`
	Catchup        bool     `json:"catchup"`
	Runners        []string `json:"runners"`

	// NextRun is the next UTC fire, zero when there is none: a disabled
	// schedule, an expression that cannot be parsed, or a date the calendar
	// never reaches. Zero rather than an error because this is a display
	// projection — one unparseable cron must not blank the other nineteen
	// rows — and the reason is in [Row.Problem].
	NextRun time.Time `json:"next_run"`

	// Problem says why NextRun is empty when the reason is a defect rather
	// than a choice: an unparseable cron, an unknown timezone. Empty for a
	// healthy row and for a merely disabled one.
	//
	// It exists because the alternative is a blank cell. A schedule whose
	// timezone was renamed shows "next run: —" and looks idle, which is
	// indistinguishable from one that is simply not due for a while.
	Problem string `json:"problem,omitempty"`
}

// DescribeOptions carries what a projection needs beyond the org itself.
type DescribeOptions struct {
	// DefaultTimezone is applied to any schedule that names none. Empty
	// means UTC.
	DefaultTimezone string

	// Now is the instant NextRun is computed from. Zero reads the clock.
	Now time.Time
}

// Describe projects every schedule in the company into a display row.
//
// It resolves the effective timezone and the runners through the same
// [Entry.Runners] the tick uses, so what the dashboard shows and what the
// scheduler dispatches to cannot drift apart.
func Describe(o *org.Organization, opts DescribeOptions) []Row {
	ref := opts.Now
	if ref.IsZero() {
		ref = now()
	}
	defaultTZ := opts.DefaultTimezone
	if defaultTZ == "" {
		defaultTZ = DefaultTimezone
	}

	entries := Entries(o)
	out := make([]Row, 0, len(entries))
	for _, e := range entries {
		s := e.Schedule
		zone := s.Timezone
		if zone == "" {
			zone = defaultTZ
		}
		row := Row{
			ScopeType:      e.Scope,
			ScopeID:        e.ScopeID,
			Name:           s.Name,
			Cron:           s.Cron,
			Timezone:       zone,
			Task:           s.Task,
			Enabled:        s.IsEnabled(),
			TimeoutSeconds: int(s.Timeout() / time.Second),
			Catchup:        s.CatchesUp(),
			Runners:        e.Runners(o),
		}
		if e.Scope == types.ScheduleScopeUnit {
			target := s.Target
			if target == "" {
				target = org.TargetEach
			}
			row.Target = target
		}
		if row.Enabled {
			row.NextRun, row.Problem = nextRun(s.Cron, zone, ref)
		}
		out = append(out, row)
	}
	return out
}

// nextRun resolves a row's next fire, or the reason it has none.
func nextRun(expr, zone string, ref time.Time) (time.Time, string) {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return time.Time{}, "unknown timezone " + zone
	}
	cron, err := Parse(expr)
	if err != nil {
		return time.Time{}, err.Error()
	}
	fire, ok := cron.Next(ref, loc)
	if !ok {
		// Parsed, and the calendar never reaches it — February 30th. Not a
		// config typo the parser can catch, and worth saying out loud
		// because the row otherwise looks like any other quiet schedule.
		return time.Time{}, "no fire within " + Horizon.String()
	}
	return fire, ""
}

// SortRows orders a projection for display: by scope, then by scope id, then
// by name.
//
// Stable and total, so two dashboard reads of one company draw the same list.
// Entries come out of the org in tree order, which is meaningful to nobody
// reading a flat table.
func SortRows(rows []Row) {
	slices.SortStableFunc(rows, func(a, b Row) int {
		return cmp.Or(
			cmp.Compare(a.ScopeType, b.ScopeType),
			cmp.Compare(a.ScopeID, b.ScopeID),
			cmp.Compare(a.Name, b.Name),
		)
	})
}
