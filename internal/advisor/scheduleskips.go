package advisor

import (
	"cmp"
	"fmt"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/words"
)

// ClassScheduleOverdue is the owner's: a schedule whose runs were skipped
// again and again over the week, because its agent was paused, or because no
// server ran the schedules when they came due.
const ClassScheduleOverdue Class = "schedule.overdue"

// minSkips is two skipped runs a week: one is a restart, two is a pattern.
const minSkips = 2

// ScheduleSkip is one run of a schedule the dispatch ledger recorded as
// skipped: the schedule by its seat's handle or its unit's id and its name,
// the seat it was addressed to, and whether that seat was paused (else it
// fell outside the catch-up window, the engine not running when it came due).
type ScheduleSkip struct {
	Seat, Unit, Name, Target string
	Paused                   bool
}

// skipTally is one schedule's skipped runs: while paused, and missed.
type skipTally struct {
	paused, missed int
	target         string
}

// scheduleOverdue is one finding per schedule of c whose runs were skipped
// [minSkips] times or more in the week.
func scheduleOverdue(c *config.Company, ev Evidence) []Finding {
	tallies := map[scheduleKey]skipTally{}
	for _, s := range ev.Skips {
		k := scheduleKey{s.Seat, s.Unit, s.Name}
		t := tallies[k]
		if s.Paused {
			t.paused++
		} else {
			t.missed++
		}
		if t.target == "" {
			t.target = s.Target
		}
		tallies[k] = t
	}
	var out []Finding
	for _, k := range slices.SortedFunc(maps.Keys(tallies), func(a, b scheduleKey) int {
		return cmp.Or(cmp.Compare(a.seat, b.seat), cmp.Compare(a.unit, b.unit), cmp.Compare(a.name, b.name))
	}) {
		t := tallies[k]
		if t.paused+t.missed < minSkips {
			continue
		}
		p, ok := placeOfSchedule(c, k)
		if !ok {
			continue
		}
		s, _ := p.theSchedule()
		out = append(out, Finding{
			Class: ClassScheduleOverdue, Severity: SeverityCosts, Actor: ActorOwner, Source: SourceEvidence,
			Area: AreaSchedules, Subject: p.subject(),
			Title: "Scheduled runs were skipped.",
			Why: "A run a schedule skips is a check nobody made: while its agent is paused a schedule " +
				"does not run, and a run that came due while no server ran the schedules is made up " +
				"only within the catch-up window.",
			Item: fmt.Sprintf("%s (%s skipped: %d while paused, %d missed)", s.Name,
				words.Count(t.paused+t.missed, "run", "runs"), t.paused, t.missed),
			Evidence: []Fact{{Key: "paused_skips", Value: int64(t.paused)}, {Key: "missed_skips", Value: int64(t.missed)}},
			Fix:      overdueFix(c, t),
		})
	}
	return out
}

// overdueFix opens the paused agent's page where pauses skipped most runs,
// and the catch-up window where missed runs did.
func overdueFix(c *config.Company, t skipTally) Fix {
	if t.paused >= t.missed && t.target != "" {
		return Fix{Kind: FixOpenPage, Options: []Option{}, Page: PageSeat, Target: t.target,
			Sentence:  fmt.Sprintf("Open %s's page: its schedules do not run while it is paused.", seatName(c, t.target)),
			Unchanged: "The schedule stays as it is."}
	}
	return Fix{Kind: FixOpenSetting, Options: []Option{}, Setting: "scheduling.catchup_max_seconds",
		Sentence: "Keep a server running when the schedule comes due, or widen the catch-up window so the " +
			"latest missed run is made up after a restart.",
		Unchanged: "Older missed runs are never made up, so a restart never replays a night of them."}
}
