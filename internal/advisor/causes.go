package advisor

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// ONE FINDING PER CAUSE, THE SEATS INSIDE IT (ADR-0166). A condition that
// holds on many seats (each agent learning on its main model, schedules that
// run too often, runs a pause skipped, people taking an agent's every
// recommendation) is one cause, so it is one finding about the company with
// each affected seat inside it and its number, rather than one identical row
// per seat. Its fix changes every affected seat together: one action of
// several changes, each re-checked and sent in order ([Option.Also]).

// groupedClasses are the classes stated as one finding per cause, in the
// order their findings are listed.
var groupedClasses = []Class{ClassAuxiliaryModel, ClassScheduleFrequent, ClassScheduleOverdue,
	ClassApprovalRubberStamp}

// groupCauses replaces every finding of a grouped class with one finding of
// that class about the company, its seats inside; a finding already about the
// company is kept as it is.
func groupCauses(c *config.Company, findings []Finding) []Finding {
	by := map[Class][]Finding{}
	out := make([]Finding, 0, len(findings))
	for _, f := range findings {
		if slices.Contains(groupedClasses, f.Class) && f.Subject.Kind != SubjectCompany {
			by[f.Class] = append(by[f.Class], f)
			continue
		}
		out = append(out, f)
	}
	for _, class := range groupedClasses {
		if fs := by[class]; len(fs) > 0 {
			out = append(out, groupOf(c, class, fs))
		}
	}
	return out
}

// groupOf is fs, every one of class, as one finding about the company.
func groupOf(c *config.Company, class Class, fs []Finding) Finding {
	base := fs[0]
	g := Finding{Class: class, Severity: base.Severity, Actor: base.Actor, Source: base.Source, Area: base.Area,
		Subject: Subject{Kind: SubjectCompany}, Title: base.Title, Why: base.Why, Evidence: []Fact{},
		Records: []Record{}}
	sums := map[string]int64{}
	var keys []string
	var items []string
	for _, f := range fs {
		if f.Severity.rank() < g.Severity.rank() {
			g.Severity = f.Severity
		}
		for _, fact := range f.Evidence {
			if _, seen := sums[fact.Key]; !seen {
				keys = append(keys, fact.Key)
			}
			sums[fact.Key] += fact.Value
		}
		for _, r := range f.Records {
			if !slices.Contains(g.Records, r) {
				g.Records = append(g.Records, r)
			}
		}
		if f.Item != "" {
			items = append(items, f.Item)
		}
		if seat := f.Subject.Seat; seat != "" {
			share := SeatShare{Seat: seat, Name: seatName(c, seat)}
			if v, unit, ok := seatNumber(f); ok {
				share.Value, share.Unit = v, unit
			}
			if at := slices.IndexFunc(g.Seats, func(s SeatShare) bool { return s.Seat == seat }); at >= 0 {
				g.Seats[at].Value += share.Value
			} else {
				g.Seats = append(g.Seats, share)
			}
		}
	}
	slices.SortStableFunc(g.Seats, func(a, b SeatShare) int {
		return cmp.Or(cmp.Compare(b.Value, a.Value), cmp.Compare(a.Seat, b.Seat))
	})
	for _, k := range keys {
		g.Evidence = append(g.Evidence, Fact{Key: k, Value: sums[k]})
	}
	g.Evidence = append(g.Evidence, Fact{Key: "seats", Value: int64(len(g.Seats))})
	g.Item = listed(items, 3)
	switch class {
	case ClassAuxiliaryModel:
		g.Fix = mergedOptions(fs)
		g.Headline = fmt.Sprintf("%s run their learning on the model they work on.",
			countSaid(len(g.Seats), "agent", "agents"))
	case ClassScheduleFrequent:
		fires := 0
		for _, f := range fs {
			if p, ok := placeOfSchedule(c, scheduleKey{f.Subject.Seat, f.Subject.Unit, f.Subject.Schedule}); ok {
				if s, ok := p.theSchedule(); ok {
					n, _ := config.ScheduleFiresPerDay(s)
					fires += n
				}
			}
		}
		g.Evidence = append(g.Evidence, Fact{Key: "fires_a_day", Value: int64(fires)},
			Fact{Key: "schedules", Value: int64(len(fs))})
		g.Fix = groupedScheduleFix(c, fs)
		g.Headline = fmt.Sprintf("%s run %d times a day between them, each run a turn.",
			countSaid(len(fs), "schedule", "schedules"), fires)
	case ClassScheduleOverdue:
		g.Fix = fs[0].Fix
		for _, f := range fs {
			if f.Fix.Kind == FixOpenSetting {
				g.Fix = f.Fix
				break
			}
		}
		g.Headline = fmt.Sprintf("%d scheduled runs were skipped this week across %s.",
			sums["paused_skips"]+sums["missed_skips"], countSaid(len(fs), "schedule", "schedules"))
	case ClassApprovalRubberStamp:
		g.Fix = rubberStampFix(fs, sums)
		g.Headline = fmt.Sprintf("People took the agent's recommendation in %d of %d decisions this week.",
			sums["recommended"], sums["decided"])
	}
	return g
}

// seatNumber is the number a finding of a grouped class gives its seat: the
// runs a schedule skipped, or the decisions an agent asked.
func seatNumber(f Finding) (float64, Unit, bool) {
	sum := func(keys ...string) float64 {
		var n int64
		for _, fact := range f.Evidence {
			if slices.Contains(keys, fact.Key) {
				n += fact.Value
			}
		}
		return float64(n)
	}
	switch f.Class {
	case ClassScheduleOverdue:
		return sum("paused_skips", "missed_skips"), UnitCount, true
	case ClassApprovalRubberStamp:
		return sum("decided"), UnitCount, true
	}
	return 0, "", false
}

// listed is at most n of items joined, saying how many more there are.
func listed(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, "; ")
	}
	return fmt.Sprintf("%s; and %d more", strings.Join(items[:n], "; "), len(items)-n)
}

// mergedOptions is fs's configuration fixes as one fix: each option label
// every seat offers, changing every seat that offers it at once, the first
// seat's change first and the others' after it.
func mergedOptions(fs []Finding) Fix {
	fix := Fix{Kind: FixConfigChange, Options: []Option{}, Sentence: fs[0].Fix.Sentence,
		Unchanged: fs[0].Fix.Unchanged}
	var labels []string
	changes := map[string][]Change{}
	for _, f := range fs {
		for _, o := range f.Fix.Options {
			if _, seen := changes[o.Label]; !seen {
				labels = append(labels, o.Label)
			}
			changes[o.Label] = append(changes[o.Label], o.Change)
			changes[o.Label] = append(changes[o.Label], o.Also...)
		}
	}
	for _, label := range labels {
		chs := changes[label]
		fix.Options = append(fix.Options, Option{Label: groupedLabel(label, len(chs), len(fs)), Change: chs[0],
			Also: chs[1:]})
	}
	if len(fix.Options) == 0 {
		fix = fs[0].Fix
	}
	fix.Sentence = "Change every one of them at once: each is checked again and sent in turn."
	return fix
}

// groupedLabel is an option's label with how many of the seats it changes.
func groupedLabel(label string, changes, seats int) string {
	if changes == seats {
		return fmt.Sprintf("%s (all %d)", label, seats)
	}
	return fmt.Sprintf("%s (%d of %d)", label, changes, seats)
}

// groupedScheduleFix runs every frequent schedule once a day, or removes them
// all: ONE CHANGE PER SEAT OR UNIT, since a change sets the whole list of its
// schedules and two changes to one list would undo each other.
func groupedScheduleFix(c *config.Company, fs []Finding) Fix {
	type owner struct {
		entity, id string
		schedules  []org.Schedule
		names      []string
	}
	var owners []*owner
	for _, f := range fs {
		p, ok := placeOfSchedule(c, scheduleKey{f.Subject.Seat, f.Subject.Unit, f.Subject.Schedule})
		if !ok {
			continue
		}
		entity, id := EntityUnits, ""
		if p.role != nil {
			entity, id = EntityRoles, p.role.Seat().Handle()
		} else if p.unit != nil {
			id = p.unit.ID
		}
		at := slices.IndexFunc(owners, func(o *owner) bool { return o.entity == entity && o.id == id })
		if at < 0 {
			owners = append(owners, &owner{entity: entity, id: id, schedules: p.schedules()})
			at = len(owners) - 1
		}
		owners[at].names = append(owners[at].names, f.Subject.Schedule)
	}
	if len(owners) == 0 {
		return fs[0].Fix
	}
	var daily, removed []Change
	for _, o := range owners {
		d := slices.Clone(o.schedules)
		var kept []org.Schedule
		for i := range d {
			if slices.Contains(o.names, d[i].Name) {
				d[i].Cron = dailyCron(d[i].Cron)
				continue
			}
			kept = append(kept, d[i])
		}
		if kept == nil {
			kept = []org.Schedule{}
		}
		names := strings.Join(o.names, ", ")
		daily = append(daily, Change{Entity: o.entity, ID: o.id, Set: map[string]any{"schedules": d},
			Summary: fmt.Sprintf("Run schedules %s once a day", names)})
		removed = append(removed, Change{Entity: o.entity, ID: o.id, Set: map[string]any{"schedules": kept},
			Summary: fmt.Sprintf("Remove schedules %s", names)})
	}
	return Fix{Kind: FixConfigChange, Unchanged: "Their tasks stay as they are written.",
		Sentence: "Run them less often, or remove them: every one is changed at once.",
		Options: []Option{
			{Label: fmt.Sprintf("Run each once a day at %02d:00", dailyHour), Change: daily[0], Also: daily[1:]},
			{Label: "Remove them", Change: removed[0], Also: removed[1:]},
		}}
}

// rubberStampFix asks Crewlet to draft the policies for every agent whose
// recommendations people take.
func rubberStampFix(fs []Finding, sums map[string]int64) Fix {
	var said []string
	for _, f := range fs {
		said = append(said, f.Item)
	}
	return Fix{Kind: FixAskCrewlet, Options: []Option{},
		Sentence:  "Draft the policies with Ask Crewlet, and review them before they are saved.",
		Unchanged: "Nothing changes until you approve what Ask Crewlet drafts.",
		Prompt: fmt.Sprintf("In the last 7 days people chose an agent's recommended option in %d of %d "+
			"decisions: %s. Read those decisions, tell me which kinds of them were always taken as recommended, "+
			"and draft a change to the company's policies that lets each agent act on those kinds without "+
			"asking, keeping every other decision with the person. Show me the change before making it.",
			sums["recommended"], sums["decided"], strings.Join(said, "; "))}
}

// Restates reports whether model finding m says what rule finding r already
// says with its numbers: they are about a seat in common and m's words name
// what r is about (the kind of wake, the model entry, the schedule). Such a
// model finding is not listed, and one already open is closed as a duplicate.
func Restates(m, r Finding) bool {
	if m.Source != SourceModel || r.Source == SourceModel {
		return false
	}
	name := r.Subject.Name
	if r.Subject.Kind == SubjectSchedule {
		name = r.Subject.Schedule
	}
	if name == "" || r.Subject.Kind == SubjectSeat || r.Subject.Kind == SubjectCompany {
		return false
	}
	if !slices.ContainsFunc(seatsAbout(m), func(s string) bool { return slices.Contains(seatsAbout(r), s) }) {
		return false
	}
	return strings.Contains(plainOf(m.Title+" "+m.Headline+" "+m.Item+" "+m.Why), plainOf(name))
}

// seatsAbout is every seat a finding is about.
func seatsAbout(f Finding) []string {
	var out []string
	if f.Subject.Seat != "" {
		out = append(out, f.Subject.Seat)
	}
	for _, s := range f.Seats {
		out = append(out, s.Seat)
	}
	return out
}

// plainOf is s lowercased with its separators as spaces, so "lead-fallback",
// "lead_fallback" and "Lead fallback" read alike.
func plainOf(s string) string {
	s = strings.NewReplacer("-", " ", "_", " ", ":", " ").Replace(strings.ToLower(s))
	return " " + strings.Join(strings.Fields(s), " ") + " "
}
