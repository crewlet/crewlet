package queries

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/notice"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE ADVISOR READS THE BACKUP ALARM AS THE EVALUATION HOLDS IT, every log it
// stands on counted, and nothing from any other alarm.
func TestTheAdvisorReadsTheBackupAlarmAsItStands(t *testing.T) {
	t.Parallel()
	if got := backupEvidence(nil); got.Standing {
		t.Errorf("no evaluation reads as a standing backup alarm: %+v", got)
	}
	others := []statelog.Alarm{{Kind: statelog.KindVolumeLow}, {Kind: statelog.KindTrimBlocked}}
	if got := backupEvidence(others); got.Standing {
		t.Errorf("other alarms read as a standing backup alarm: %+v", got)
	}
	standing := append(others, statelog.Alarm{Kind: statelog.KindBackupAge, Domains: []statelog.AlarmDomain{
		{Domains: []string{"tracker", "identity"}}, {Domains: []string{"chat"}}}})
	if got := backupEvidence(standing); got != (advisor.Backup{Standing: true, Logs: 3}) {
		t.Errorf("a backup alarm on three logs reads as %+v", got)
	}
}

// patternsWork answers the patterns read and nothing else.
type patternsWork struct {
	WorkReader
	work worklet.WorkPatterns
	err  error
}

func (p patternsWork) WorkPatterns(context.Context, worklet.PatternsQuery) (worklet.WorkPatterns, error) {
	return p.work, p.err
}

// A SOURCE THE ADVISOR CANNOT READ IS NAMED, never read as clear: a failed
// tracker read and an alarm table not yet evaluated each say unread.
func TestTheAdvisorNamesASourceItCouldNotRead(t *testing.T) {
	t.Parallel()
	now := time.Date(2030, 1, 9, 12, 0, 0, 0, time.UTC)
	standing := []statelog.Alarm{{Kind: statelog.KindBackupAge}}
	for _, c := range []struct {
		name      string
		work      patternsWork
		evaluated func() bool
		want      advisor.Evidence
	}{
		{"both read", patternsWork{work: worklet.WorkPatterns{AgentComments: 3}}, func() bool { return true },
			advisor.Evidence{Until: now, Work: worklet.WorkPatterns{AgentComments: 3}, Backup: advisor.Backup{Standing: true}}},
		{"the tracker failed", patternsWork{err: errors.New("no data node answered")}, func() bool { return true },
			advisor.Evidence{Until: now, WorkUnread: true, Backup: advisor.Backup{Standing: true}}},
		{"the alarms not yet evaluated", patternsWork{}, func() bool { return false },
			advisor.Evidence{Until: now, AlarmsUnread: true}},
		{"a node with no alarm table", patternsWork{}, nil, advisor.Evidence{Until: now, AlarmsUnread: true}},
	} {
		s := Sources{Work: c.work, AlarmsEvaluated: c.evaluated,
			Alarms: func() []statelog.Alarm { return standing }}
		got := s.advisorEvidence(t.Context(), &config.Company{}, now)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: evidence %+v, want %+v", c.name, got, c.want)
		}
	}
}

// THE ADVISOR NOTICES WHAT STANDS ON THE FLEET: every node's heartbeat marks are
// read, a node that has not evaluated its table says nothing, two nodes holding
// one alarm past its threshold give one notice, and the answer carries the
// notices beside the findings.
func TestTheAdvisorNoticesTheFleetsAlarms(t *testing.T) {
	t.Parallel()
	backend := coordmemory.New()
	since := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	for node, status := range map[string]coord.NodeStatus{
		"node-a": {AlarmsEvaluated: true, Alarms: []coord.AlarmMark{{Kind: "apply_lag", Since: since}}},
		"node-b": {AlarmsEvaluated: true, Alarms: []coord.AlarmMark{{Kind: "apply_lag", Since: since.Add(time.Minute)}}},
		"node-c": {Alarms: []coord.AlarmMark{{Kind: "volume_low", Since: since}}},
	} {
		if _, _, err := backend.TryAcquire(t.Context(), coord.NodeResource(node), coord.AcquireOptions{
			Owner: node + ":1", TTL: time.Minute, Meta: map[string]any{coord.StatusKey: status.Meta()},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, known := (Sources{}).fleetAlarms(t.Context()); known {
		t.Error("a node with no coordination store says it read the fleet's alarms")
	}
	fleet, known := Sources{Coord: backend}.fleetAlarms(t.Context())
	if !known {
		t.Fatal("the fleet's alarms read as unknown")
	}
	got := notice.Read(nil, fleet, since.Add(notice.AlarmAfter))
	want := []notice.Notice{{Class: notice.ClassAlarm, Subject: advisor.Subject{Kind: notice.SubjectAlarm, Name: "apply_lag"},
		Since: since, Nodes: []string{"node-a", "node-b"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the fleet's notices are\n%+v\nwant\n%+v", got, want)
	}
	raw, err := json.Marshal(advisorAnswer{Answer: advisor.Answer{Findings: []advisor.Finding{}, Unread: []string{}},
		Notices: got})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if keys := slices.Sorted(maps.Keys(fields)); !slices.Equal(keys, []string{"findings", "notices", "revision", "unread"}) {
		t.Errorf("the advisor answers %v", keys)
	}
}

// `alarms` IS NAMED UNREAD ONCE, whether this node's evaluation has not run,
// the fleet's marks could not be read, or both.
func TestTheAdvisorNamesItsAlarmsUnreadOnce(t *testing.T) {
	t.Parallel()
	now := time.Date(2030, 1, 9, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name       string
		unread     []string
		fleetKnown bool
		want       []string
	}{
		{"both read", []string{}, true, []string{}},
		{"this node not evaluated", []string{advisor.UnreadAlarms}, true, []string{advisor.UnreadAlarms}},
		{"the fleet unread", []string{advisor.UnreadWork}, false, []string{advisor.UnreadWork, advisor.UnreadAlarms}},
		{"both unread", []string{advisor.UnreadWork, advisor.UnreadAlarms}, false,
			[]string{advisor.UnreadWork, advisor.UnreadAlarms}},
	} {
		got := noticed(advisor.Answer{Findings: []advisor.Finding{}, Unread: c.unread}, nil, c.fleetKnown, now)
		if !slices.Equal(got.Unread, c.want) {
			t.Errorf("%s: unread %v, want %v", c.name, got.Unread, c.want)
		}
	}
}
