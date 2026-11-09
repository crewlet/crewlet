package config

import (
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/cron"
	"github.com/crewlet/crewlet/internal/org"
)

// AdvisoryClass names what an advisory is about, for a reader that acts on
// the class rather than on the sentence: the setup advisor (internal/advisor)
// gives each class its owner-facing words and the change that fixes it.
//
// A CLOSED SET, and most advisories carry none: an advisory with no class is
// still answered by `crewlet validate`, a `/config` write and the apply log,
// and the advisor shows it in its own words. A class is given to an advisory
// whose fix the advisor can build.
type AdvisoryClass string

const (
	// ClassHealthUnheld is a company with agent seats none of which holds
	// `health_tools`, so an alarm wakes nobody.
	ClassHealthUnheld AdvisoryClass = "grant.health_unheld"
	// ClassAuxiliaryModel is an agent seat whose reflection and summaries
	// run on the very model its executor runs on.
	ClassAuxiliaryModel AdvisoryClass = "learning.auxiliary_model"
	// ClassScheduleFrequent is a schedule that fires
	// [FrequentScheduleFires] or more times in a day.
	ClassScheduleFrequent AdvisoryClass = "schedule.frequent"
)

// AdvisoryClasses is every class, in the order the advisor lists them.
var AdvisoryClasses = []AdvisoryClass{ClassHealthUnheld, ClassAuxiliaryModel, ClassScheduleFrequent}

// Valid reports whether c is one of [AdvisoryClasses].
func (c AdvisoryClass) Valid() bool { return slices.Contains(AdvisoryClasses, c) }

// FrequentScheduleFires is how many fires in one day make a schedule advised
// about: every two hours. Polling checks every one or two hours were the
// schedules a live company found changed nothing on almost every fire; a
// daily or weekday cadence, or a few fires a day, is a routine and is never
// flagged. Counted per day rather than read off the shortest gap, because
// the cost is turns, and two fires an hour apart once a day are two turns.
const FrequentScheduleFires = 12

// referenceWeek is the week a schedule's cadence is read over: fixed, so
// the same document gets the same advice on every node and every run, and
// seven days, so every day-of-week restriction is seen. It is a Monday.
var referenceWeek = time.Date(2030, time.January, 7, 0, 0, 0, 0, time.UTC)

// classed is advisory with its class.
func classed(class AdvisoryClass, path Path, message string) Warning {
	w := advisory(path, message)
	w.Class = class
	return w
}

// healthWarnings advises a company with agent seats that gives none of them
// `health_tools`: an alarm that fires on a node wakes the seats holding the
// grant (internal/alarmwake), so with none it reaches nobody until a person
// happens to open the dashboard.
//
// AT THE FIRST AGENT SEAT'S `health_tools`, the field the fix sets, though
// any agent seat may be the one; the advisor offers every one of them.
func (c *Company) healthWarnings() []Warning {
	var first Path
	for role, path := range c.EachRole() {
		if role.Kind == org.KindHuman {
			continue
		}
		if role.HealthTools {
			return nil
		}
		if first == nil {
			first = at(path, "health_tools")
		}
	}
	if first == nil {
		return nil
	}
	return []Warning{classed(ClassHealthUnheld, first,
		"no agent seat holds health_tools, so an alarm that fires on a node wakes no "+
			"agent and nobody hears it until a person opens the dashboard. Set "+
			"health_tools on the seat that looks after operations")}
}

// auxiliaryWarnings advises every agent seat whose auxiliary chain starts on
// the same model as its executor chain: reflection, summaries and the memory
// writers run after the seat's turns, so on the executor's model they cost a
// share of the seat's own spend for work a smaller model does.
//
// RESOLVED BY [phase.Resolve], the call a launch and every side pass resolve
// their chain through, so the advice and the run cannot disagree about which
// entry a phase lands on. The same model is the same entry, or two entries
// driving one harness with one model id.
func (c *Company) auxiliaryWarnings() []Warning {
	order := c.Providers.ProviderOrder()
	if len(order) == 0 {
		return nil
	}
	var out []Warning
	for role, path := range c.EachRole() {
		if role.Kind == org.KindHuman {
			continue
		}
		seat := role.Seat()
		exec := c.ExecutorChain(role)
		aux := phase.Resolve(seat, phase.Auxiliary, order)
		if len(exec) == 0 || len(aux) == 0 || !c.SameModel(exec[0], aux[0]) {
			continue
		}
		handle := seat.Handle()
		w := classed(ClassAuxiliaryModel, at(path, "llm_auxiliary"), fmt.Sprintf(
			"%s's reflection and summaries run on providers.llm.%s, the model its "+
				"executor runs on, so learning costs a share of the seat's own spend "+
				"at that model's price. Point llm_auxiliary at an entry with a smaller model",
			handle, aux[0]))
		w.Seat = handle
		out = append(out, w)
	}
	return out
}

// SameModel reports whether two providers.llm entries run one model: the
// same entry, or two driving one harness with one model id.
func (c *Company) SameModel(a, b string) bool {
	if a == b {
		return true
	}
	ea, oka := c.Providers.LLM[a]
	eb, okb := c.Providers.LLM[b]
	return oka && okb && ea.Model != "" && ea.HarnessName() == eb.HarnessName() && ea.Model == eb.Model
}

// ExecutorChain is the providers.llm keys a seat's executor resolves to, in
// the order a launch tries them, by [phase.Resolve] as a launch resolves it.
func (c *Company) ExecutorChain(r *Role) []string {
	return phase.Resolve(r.Seat(), phase.Execute, c.Providers.ProviderOrder())
}

// scheduleWarnings advises every enabled schedule that fires
// [FrequentScheduleFires] or more times in a day. A fire starts a turn
// whether or not anything changed, so a check that polls for an event (a
// deploy, a merge) is better written as a work item dated for when to look.
func (c *Company) scheduleWarnings() []Warning {
	var out []Warning
	advise := func(schedules []org.Schedule, path Path, seat, unit string) {
		for i, s := range schedules {
			fires, ok := ScheduleFiresPerDay(s)
			if !ok || fires < FrequentScheduleFires {
				continue
			}
			w := classed(ClassScheduleFrequent, at(idx(path, i), "cron"), fmt.Sprintf(
				"schedule %q fires %d times a day, and each fire starts a turn whether or "+
					"not anything changed. If it checks for something that happens "+
					"(a deploy, a merge), file a work item dated for when to look "+
					"instead; otherwise run it less often", s.Name, fires))
			w.Seat, w.Unit = seat, unit
			out = append(out, w)
		}
	}
	for role, path := range c.EachRole() {
		advise(role.Schedules, at(path, "schedules"), role.Seat().Handle(), "")
	}
	var walk func(units []Unit, path Path)
	walk = func(units []Unit, path Path) {
		for i := range units {
			here := idx(path, i)
			advise(units[i].Schedules, at(here, "schedules"), "", units[i].Name)
			walk(units[i].Children, at(here, "children"))
		}
	}
	walk(c.Units, field("units"))
	return out
}

// ScheduleFiresPerDay is how many times an enabled schedule fires on the
// busiest day of [referenceWeek] (UTC days), and false for a disabled
// schedule or one whose cron does not parse.
func ScheduleFiresPerDay(s org.Schedule) (int, bool) {
	if !s.IsEnabled() {
		return 0, false
	}
	expr, err := cron.Parse(s.Cron)
	if err != nil {
		return 0, false
	}
	busiest := 0
	for day := range 7 {
		// FireTimes answers (after, until], so a day starts a minute early
		// to count its own midnight.
		from := referenceWeek.AddDate(0, 0, day).Add(-time.Minute)
		busiest = max(busiest, len(expr.FireTimes(from, from.AddDate(0, 0, 1), time.UTC)))
	}
	return busiest, true
}
