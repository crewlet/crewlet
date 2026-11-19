package advisor

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
)

// A WEEK'S REPORT is written once, on the Monday after the week, from the
// same records the detectors read: what the week cost in turns that changed
// nothing, stops and capped runs, each beside the week before; how many
// findings it opened, resolved, applied and dismissed; and the open findings
// that cost the most when it was written. It is kept as written, so a report
// says what was true of its week however the list moves since.

// ReportTop is how many open findings a report names.
const ReportTop = 5

// Report is one week's report as the log keeps it.
type Report struct {
	Week string    `json:"week"`
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Metrics is each of the week's numbers its sources could be read for,
	// and Unread the sources that could not, by their words.
	Metrics []Metric `json:"metrics"`
	Unread  []string `json:"unread"`
	Tally
	// Top is the open findings that cost the most when the report was
	// written, the most costly first.
	Top []ReportFinding `json:"top"`
	// Summary is the model's summary of the report, nil where none was written.
	Summary *string `json:"summary"`
}

// Tally is how many findings a week opened, resolved, had a fix applied to,
// and dismissed.
type Tally struct {
	Opened    int `json:"opened"`
	Resolved  int `json:"resolved"`
	Applied   int `json:"applied"`
	Dismissed int `json:"dismissed"`
}

// Metric is one number of a week, with the week before's beside it where that
// week's report has it.
type Metric struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Value int64  `json:"value"`
	Unit  Unit   `json:"unit"`
	Was   *int64 `json:"was"`
}

// ReportFinding is an open finding as a report names it, kept as it read then.
type ReportFinding struct {
	ID       string   `json:"id"`
	Class    Class    `json:"class"`
	Severity Severity `json:"severity"`
	Title    string   `json:"title"`
	Subject  string   `json:"subject"`
	Item     string   `json:"item"`
}

// metricWords is each metric's label and unit, in the order a report lists them.
var metricWords = []struct {
	key, label string
	unit       Unit
	source     source
}{
	{"turns", "Turns", UnitTurns, sourceLedger},
	{"turns_nothing", "Turns that committed nothing", UnitTurns, sourceLedger},
	{"tokens", "Tokens", UnitTokens, sourceLedger},
	{"alarm_turns_nothing", "Alarm turns that changed nothing", UnitTurns, sourceLedger},
	{"alarm_tokens_nothing", "Tokens on alarm turns that changed nothing", UnitTokens, sourceLedger},
	{"budget_stops", "Turns refused on a budget", UnitCount, sourceStops},
	{"round_caps", "Runs ended on their round cap", UnitCount, sourceRuns},
}

// ReportWeek is the last whole ISO week before now on the company clock zone,
// as the log names it (`2026-W47`), the instants it starts and ends at, and
// when its report is due: the Monday it ends on, at [ReportHour].
func ReportWeek(now time.Time, zone *time.Location) (week string, from, to, due time.Time) {
	local := now.In(zone)
	back := (int(local.Weekday()) + 6) % 7
	to = time.Date(local.Year(), local.Month(), local.Day()-back, 0, 0, 0, 0, zone)
	from = to.AddDate(0, 0, -7)
	year, n := from.ISOWeek()
	return fmt.Sprintf("%04d-W%02d", year, n), from, to, to.Add(ReportHour * time.Hour)
}

// ReportHour is the hour of the Monday a week's report is written at: the
// model pass's, so the report follows it.
const ReportHour = 6

// WeekMetrics is the numbers of the week ev was read for, each source's
// numbers left out where it could not be read and its word named instead.
func WeekMetrics(ev Evidence) ([]Metric, []string) {
	values := map[string]int64{}
	for _, r := range ev.Ledger {
		if types.WakeDisposition(r.Disposition) != types.DispositionStarted {
			continue
		}
		values["turns"] += r.Turns
		values["tokens"] += r.Tokens()
		if types.TurnOutcome(r.Outcome) != types.OutcomeNothing {
			continue
		}
		values["turns_nothing"] += r.Turns
		if alarmWoken(r.Wake) {
			values["alarm_turns_nothing"] += r.Turns
			values["alarm_tokens_nothing"] += r.Tokens()
		}
	}
	values["budget_stops"] = int64(len(ev.Stops))
	for _, r := range ev.Runs {
		if r.Kind == types.ExecutorErrorMaxTurns {
			values["round_caps"]++
		}
	}
	metrics := []Metric{}
	unread := []string{}
	for _, m := range metricWords {
		if ev.unread(m.source) {
			if word := sourceWords[m.source]; !slices.Contains(unread, word) {
				unread = append(unread, word)
			}
			continue
		}
		metrics = append(metrics, Metric{Key: m.key, Label: m.label, Value: values[m.key], Unit: m.unit})
	}
	return metrics, unread
}

// Against is metrics with the week before's value of each beside it, from
// last, the week before's report (nil where none was written).
func Against(metrics []Metric, last *Report) []Metric {
	if last == nil {
		return metrics
	}
	out := slices.Clone(metrics)
	for i := range out {
		at := slices.IndexFunc(last.Metrics, func(m Metric) bool { return m.Key == out[i].Key })
		if at >= 0 {
			was := last.Metrics[at].Value
			out[i].Was = &was
		}
	}
	return out
}

// TopOf is the [ReportTop] open findings of stored that cost the most: the
// list's order, the most severe first, then the longest open.
func TopOf(stored []Stored) []ReportFinding {
	open := slices.DeleteFunc(slices.Clone(stored), func(s Stored) bool { return s.State != StateOpen })
	slices.SortStableFunc(open, func(a, b Stored) int {
		fa, fb := a.Observation.Finding, b.Observation.Finding
		return cmp.Or(cmp.Compare(fa.Severity.rank(), fb.Severity.rank()), a.OpenedAt.Compare(b.OpenedAt),
			cmp.Compare(a.ID, b.ID))
	})
	out := []ReportFinding{}
	for _, s := range open[:min(len(open), ReportTop)] {
		f := s.Observation.Finding
		out = append(out, ReportFinding{ID: s.ID, Class: f.Class, Severity: f.Severity, Title: f.Title,
			Subject: cmp.Or(f.Subject.Name, f.Subject.Seat, f.Subject.Unit, string(f.Subject.Kind)), Item: f.Item})
	}
	return out
}
