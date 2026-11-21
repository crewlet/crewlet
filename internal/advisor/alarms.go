package advisor

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/words"
)

// THE ALARM CLASSES read the alarm history the advisor log keeps: every fire
// and clear of every instance on every node (internal/engine's alarm
// episodes), which says how often and how long an alarm stood where the
// fleet's heartbeat says only what stands now.

// ClassPoolWaits is the owner's: a node whose callers queued for a database
// connection again and again, or for long. ClassFalseAlarm is the engine's: an
// objective alarm that fired and cleared within minutes, time after time.
const (
	ClassPoolWaits  Class = "store.pool_waits"
	ClassFalseAlarm Class = "objective.false_alarm"
)

// The alarm kinds these classes read, as the published alarm table names them
// (internal/statelog's kinds, which this package does not import: a test holds
// them equal).
const (
	AlarmPoolStarved   = "pool_starved"
	AlarmObjectiveBurn = "objective_burn"
)

// SubjectNode is a node of the fleet, by its name.
const SubjectNode SubjectKind = "node"

// The thresholds: three waits in a day, or an hour of them in a week, is a
// pool below its node's load; five objective alarms in a week each gone in ten
// minutes is an objective judging bursts the node recovers from.
const (
	minPoolWaitsADay = 3
	minPoolWaitTotal = time.Hour
	minQuickClears   = 5
	quickClear       = 10 * time.Minute
)

// AlarmEpisode is one stretch an alarm instance stood on a node: from its
// raise to its clear, the clear zero while it stands.
type AlarmEpisode struct {
	Node, Kind, Instance string
	RaisedAt, ClearedAt  time.Time
}

// stood is how long the episode stood within [from, until].
func (e AlarmEpisode) stood(from, until time.Time) time.Duration {
	start, end := e.RaisedAt, e.ClearedAt
	if end.IsZero() || end.After(until) {
		end = until
	}
	if start.Before(from) {
		start = from
	}
	return max(end.Sub(start), 0)
}

// episodesOf is ev's episodes of kind, by node, the nodes sorted.
func episodesOf(ev Evidence, kind string) ([]string, map[string][]AlarmEpisode) {
	by := map[string][]AlarmEpisode{}
	for _, e := range ev.Episodes {
		if e.Kind == kind {
			by[e.Node] = append(by[e.Node], e)
		}
	}
	return slices.Sorted(maps.Keys(by)), by
}

// poolWaits is one finding per node whose `pool_starved` alarm was raised
// [minPoolWaitsADay] times in the last day, or stood [minPoolWaitTotal] in all
// over the week.
func poolWaits(ev Evidence, _ func(string) SeatKind) []Finding {
	nodes, by := episodesOf(ev, AlarmPoolStarved)
	var out []Finding
	for _, node := range nodes {
		var day int64
		var total time.Duration
		for _, e := range by[node] {
			if !e.RaisedAt.Before(ev.Until.Add(-24 * time.Hour)) {
				day++
			}
			total += e.stood(ev.Until.Add(-Window), ev.Until)
		}
		if day < minPoolWaitsADay && total < minPoolWaitTotal {
			continue
		}
		minutes := int64(total / time.Minute)
		f := stallFinding(ClassPoolWaits, AreaSetup, Subject{Kind: SubjectNode, Name: node},
			fmt.Sprintf("%s in the last day, %s this week", words.Count(int(day), "wait", "waits"),
				words.Count(int(minutes), "minute", "minutes")),
			[]Fact{{Key: "waits_day", Value: day}, {Key: "minutes", Value: minutes}}, nil,
			Fix{Kind: FixExternalStep, Options: []Option{}, Setting: "store.max_open_conns",
				Sentence: fmt.Sprintf("On %s, raise store.max_open_conns in its bootstrap file and restart it, "+
					"or move work off it.", node),
				Unchanged: "Callers keep queuing for a connection whenever the node is this busy."})
		out = append(out, f)
	}
	return out
}

// falseAlarm is one finding per node whose `objective_burn` alarm cleared within
// [quickClear] of its raise [minQuickClears] times in the week: the engine's
// objective judging short bursts the node recovered from, said so that nobody
// reads them as outages.
func falseAlarm(ev Evidence, _ func(string) SeatKind) []Finding {
	nodes, by := episodesOf(ev, AlarmObjectiveBurn)
	var out []Finding
	for _, node := range nodes {
		var quick int64
		for _, e := range by[node] {
			if !e.ClearedAt.IsZero() && e.ClearedAt.Sub(e.RaisedAt) <= quickClear &&
				!e.RaisedAt.Before(ev.Until.Add(-Window)) {
				quick++
			}
		}
		if quick < minQuickClears {
			continue
		}
		out = append(out, engineFinding(ClassFalseAlarm, Subject{Kind: SubjectNode, Name: node},
			words.Count(int(quick), "alarm gone within ten minutes", "alarms gone within ten minutes"),
			[]Fact{{Key: "quick_clears", Value: quick}}, nil))
	}
	return out
}
