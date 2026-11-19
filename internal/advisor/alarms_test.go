package advisor_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/statelog"
)

// episode is one alarm episode on node, raised at raised for stood (zero: it stands).
func episode(node, kind string, raised time.Time, stood time.Duration) advisor.AlarmEpisode {
	e := advisor.AlarmEpisode{Node: node, Kind: kind, Instance: kind, RaisedAt: raised}
	if stood > 0 {
		e.ClearedAt = raised.Add(stood)
	}
	return e
}

// THE ALARM CLASSES READ THE ALARM TABLE'S OWN KINDS. Mutation: misspell one,
// and its class never raises.
func TestTheAlarmClassesReadTheTablesKinds(t *testing.T) {
	t.Parallel()
	if advisor.AlarmPoolStarved != string(statelog.KindPoolStarved) ||
		advisor.AlarmObjectiveBurn != string(statelog.KindObjectiveBurn) {
		t.Error("an alarm class reads a kind the alarm table does not name")
	}
}

// A NODE WHOSE CALLERS QUEUED FOR A CONNECTION AGAIN AND AGAIN, OR LONG, IS
// THE OWNER'S: three waits in a day, or an hour of them in the week, named
// per node, with the bootstrap setting to raise. Two waits a day, minutes
// long, are not. Mutation: count the week's waits as the day's, and the
// node with old waits is raised.
func TestANodeWhoseCallersQueueForAConnectionIsNamed(t *testing.T) {
	t.Parallel()
	ev := advisor.Evidence{Until: monday, Episodes: []advisor.AlarmEpisode{
		episode("data-1", advisor.AlarmPoolStarved, monday.Add(-3*time.Hour), time.Minute),
		episode("data-1", advisor.AlarmPoolStarved, monday.Add(-2*time.Hour), time.Minute),
		episode("data-1", advisor.AlarmPoolStarved, monday.Add(-time.Hour), 0),
		episode("data-2", advisor.AlarmPoolStarved, monday.Add(-72*time.Hour), 70*time.Minute),
		episode("data-3", advisor.AlarmPoolStarved, monday.Add(-3*time.Hour), time.Minute),
		episode("data-3", advisor.AlarmPoolStarved, monday.Add(-50*time.Hour), time.Minute),
		episode("data-3", advisor.AlarmPoolStarved, monday.Add(-60*time.Hour), time.Minute),
	}}
	found := stalls(t, stallShape, ev, advisor.ClassPoolWaits)
	if len(found) != 2 || found[0].Subject.Name != "data-1" || found[1].Subject.Name != "data-2" {
		t.Fatalf("raised %+v, want data-1 (three in a day) and data-2 (over an hour)", found)
	}
	if f := found[0]; f.Item != "3 waits in the last day, 62 minutes this week" || f.Severity != advisor.SeverityCosts ||
		f.Fix.Kind != advisor.FixExternalStep || f.Fix.Setting != "store.max_open_conns" {
		t.Errorf("data-1 = %+v", f)
	}
	if got := stalls(t, stallShape, advisor.Evidence{Until: monday, Episodes: ev.Episodes, EpisodesUnread: true},
		advisor.ClassPoolWaits); len(got) != 0 {
		t.Errorf("an unread history raised %+v", got)
	}
}

// AN OBJECTIVE ALARM THAT CLEARS WITHIN MINUTES, TIME AFTER TIME, IS SAID AS
// THE ENGINE'S: five in a week each gone within ten minutes, so nobody reads
// them as outages. Four, or ones that stood longer, are not. Mutation: count
// one still standing as quick, and data-2 is raised.
func TestAnObjectiveAlarmThatClearsWithinMinutesIsSaidAsTheEngines(t *testing.T) {
	t.Parallel()
	var eps []advisor.AlarmEpisode
	for i := range 5 {
		eps = append(eps, episode("data-1", advisor.AlarmObjectiveBurn, monday.Add(-time.Duration(i+1)*time.Hour), 4*time.Minute))
	}
	for i := range 4 {
		eps = append(eps, episode("data-2", advisor.AlarmObjectiveBurn, monday.Add(-time.Duration(i+1)*time.Hour), 4*time.Minute))
	}
	eps = append(eps, episode("data-2", advisor.AlarmObjectiveBurn, monday.Add(-10*time.Hour), 0),
		episode("data-2", advisor.AlarmObjectiveBurn, monday.Add(-11*time.Hour), time.Hour))
	found := stalls(t, stallShape, advisor.Evidence{Until: monday, Episodes: eps}, advisor.ClassFalseAlarm)
	if len(found) != 1 || found[0].Subject.Name != "data-1" || found[0].Actor != advisor.ActorEngine ||
		found[0].Severity != advisor.SeverityNote || found[0].Fix.Kind != advisor.FixNone {
		t.Fatalf("raised %+v, want data-1 alone, the engine's note", found)
	}
}
