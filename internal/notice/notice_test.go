package notice_test

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/notice"
	"github.com/crewlet/crewlet/internal/statelog"
)

var t0 = time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)

func marks(node string, kinds map[string]time.Time) notice.NodeAlarms {
	n := notice.NodeAlarms{Node: node}
	for kind, since := range kinds {
		n.Marks = append(n.Marks, coord.AlarmMark{Kind: kind, Since: since})
	}
	return n
}

func alarm(kind string, since time.Time, nodes ...string) notice.Notice {
	return notice.Notice{Class: notice.ClassAlarm, Subject: advisor.Subject{Kind: notice.SubjectAlarm, Name: kind},
		Since: since, Nodes: nodes}
}

// AN ALARM PAST ITS THRESHOLD REACHES THE ADMINISTRATORS ONCE: one notice per
// kind however many nodes report it and however often it is read, opened at the
// earliest raise; below the threshold, or with no known raise, it is no notice.
func TestAnAlarmPastItsThresholdReachesTheAdministratorsOnce(t *testing.T) {
	t.Parallel()
	past := t0.Add(notice.AlarmAfter)
	fleet := []notice.NodeAlarms{
		marks("node-b", map[string]time.Time{"apply_lag": t0.Add(time.Minute)}),
		marks("node-a", map[string]time.Time{"apply_lag": t0, "volume_low": t0.Add(time.Minute), "wal_large": {}}),
	}
	want := []notice.Notice{alarm("apply_lag", t0, "node-a", "node-b")}
	for range 100 {
		if got := notice.FromAlarms(fleet, past); !reflect.DeepEqual(got, want) {
			t.Fatalf("at the threshold the notices are\n%+v\nwant\n%+v", got, want)
		}
	}
	if got := notice.FromAlarms(fleet, past.Add(-time.Second)); len(got) != 0 {
		t.Errorf("a second before the threshold the notices are %+v, want none", got)
	}
	later := past.Add(time.Minute)
	want = []notice.Notice{alarm("apply_lag", t0, "node-a", "node-b"), alarm("volume_low", t0.Add(time.Minute), "node-a")}
	if got := notice.FromAlarms(fleet, later); !reflect.DeepEqual(got, want) {
		t.Errorf("past both thresholds the notices are\n%+v\nwant\n%+v", got, want)
	}
}

// A PLATFORM NOTICE CLEARS WITH ITS CONDITION: the next read of a fleet whose
// alarm cleared holds no notice, and a later raise is a new notice from its own
// instant once it is past the threshold.
func TestAPlatformNoticeClearsWithItsCondition(t *testing.T) {
	t.Parallel()
	now := t0.Add(notice.AlarmAfter)
	if got := notice.FromAlarms([]notice.NodeAlarms{marks("node-a", map[string]time.Time{"apply_lag": t0})}, now); len(got) != 1 {
		t.Fatalf("a standing alarm gives %d notices, want 1", len(got))
	}
	if got := notice.FromAlarms([]notice.NodeAlarms{{Node: "node-a"}}, now); len(got) != 0 {
		t.Errorf("a cleared alarm gives %+v, want no notice", got)
	}
	raised := now.Add(time.Minute)
	again := []notice.NodeAlarms{marks("node-a", map[string]time.Time{"apply_lag": raised})}
	if got := notice.FromAlarms(again, raised.Add(time.Minute)); len(got) != 0 {
		t.Errorf("a new raise below the threshold gives %+v, want none", got)
	}
	want := []notice.Notice{alarm("apply_lag", raised, "node-a")}
	if got := notice.FromAlarms(again, raised.Add(notice.AlarmAfter)); !reflect.DeepEqual(got, want) {
		t.Errorf("a new raise past the threshold gives %+v, want %+v", got, want)
	}
}

// THE BACKUP ALARM IS THE ADVISOR'S FINDING, NOT A SECOND NOTICE: its notice
// carries the class and subject the advisor's own finding has, so the dashboard
// attaches that fix, and its alarm kind, for a reader without the finding; the
// finding read off the same alarm adds no notice.
func TestTheBackupAlarmIsTheAdvisorsFinding(t *testing.T) {
	t.Parallel()
	found := advisor.Detect(advisor.Evidence{Until: t0, Backup: advisor.Backup{Standing: true, Logs: 2}}, nil)
	if len(found) != 1 {
		t.Fatalf("a standing backup alarm raises %d findings, want 1", len(found))
	}
	backup := string(statelog.KindBackupAge)
	got := notice.FromAlarms([]notice.NodeAlarms{marks("node-a", map[string]time.Time{backup: t0})}, t0.Add(notice.AlarmAfter))
	want := []notice.Notice{{Class: string(found[0].Class), Subject: found[0].Subject, Since: t0,
		Nodes: []string{"node-a"}, Alarm: backup}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the backup alarm's notice is\n%+v\nwant the finding's class and subject\n%+v", got, want)
	}
	if got := notice.FromFindings(found); len(got) != 0 {
		t.Errorf("the backup finding adds %+v beside the alarm's notice, want nothing", got)
	}
}

// ONLY A FINDING THAT BLOCKS THE COMPANY AND IS THE OWNER'S TO FIX IS A NOTICE:
// what costs or is worth knowing stays on the advisor's page, and the engine's
// own work never reaches the owner as a to-do.
func TestOnlyAnOwnersBlockingFindingIsANotice(t *testing.T) {
	t.Parallel()
	github := advisor.Subject{Kind: advisor.SubjectIntegration, Name: "GitHub"}
	findings := []advisor.Finding{
		{Class: "integration.awaiting_admin", Severity: advisor.SeverityBlocks, Actor: advisor.ActorOwner, Subject: github},
		{Class: "integration.awaiting_admin", Severity: advisor.SeverityBlocks, Actor: advisor.ActorOwner, Subject: github},
		{Class: "integration.unconfigured", Severity: advisor.SeverityBlocks, Actor: advisor.ActorEngine, Subject: github},
		{Class: "grant.health_unheld", Severity: advisor.SeverityCosts, Actor: advisor.ActorOwner},
		{Class: "schedule.frequent", Severity: advisor.SeverityNote, Actor: advisor.ActorOwner},
	}
	want := []notice.Notice{{Class: "integration.awaiting_admin", Subject: github}}
	if got := notice.FromFindings(findings); !reflect.DeepEqual(got, want) {
		t.Errorf("the notices are %+v, want %+v", got, want)
	}
}

// A PLATFORM NOTICE OFFERS NO ANSWER: it says what holds and since when, and
// carries nothing a reader could answer or decide; it ends when its condition does.
func TestAPlatformNoticeOffersNoAnswer(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(alarm("apply_lag", t0, "node-a"))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	got := slices.Sorted(maps.Keys(fields))
	if want := []string{"class", "nodes", "since", "subject"}; !slices.Equal(got, want) {
		t.Errorf("a notice carries %v, want only %v", got, want)
	}
}

// A CONVERSATION THE ENGINE HOLDS IS ONE NOTICE, under its seat and the
// conversation, from when it was held, saying how it failed; one whose seat the
// company no longer has is left to its removal.
func TestAHeldConversationIsANotice(t *testing.T) {
	t.Parallel()
	dev, gone := uuid.New(), uuid.New()
	holds := []coord.ConversationHold{
		{Seat: dev, Conversation: "task:t-1", Kind: notice.CauseContextExhausted, Basis: "b", At: t0},
		{Seat: dev, Conversation: "task:t-3", Kind: notice.CauseOutOfMemory, Basis: "b", At: t0},
		{Seat: gone, Conversation: "task:t-2", Kind: notice.CauseContextExhausted, Basis: "b", At: t0},
	}
	got := notice.FromConversationHolds(holds, func(seat uuid.UUID) (string, bool) { return "dev", seat == dev })
	want := []notice.Notice{
		{Class: notice.ClassConversationHeld, Since: t0, Cause: notice.CauseContextExhausted,
			Subject: advisor.Subject{Kind: advisor.SubjectSeat, Seat: "dev", Name: "task:t-1"}},
		{Class: notice.ClassConversationHeld, Since: t0, Cause: notice.CauseOutOfMemory,
			Subject: advisor.Subject{Kind: advisor.SubjectSeat, Seat: "dev", Name: "task:t-3"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the held conversations' notices are %+v, want %+v", got, want)
	}
}

// A DISK HOVERING AT THE LINE STILL REACHES A PERSON. A volume whose free space
// moves around the raise threshold every beat stays one alarm, and one that
// clears past the wider margin and comes back keeps its first raise, so the
// administrators get one notice once the grace has passed since that raise.
//
// Mutation: drop volume_low's wider clear, or the tracker's resumption of a
// run, and a case is red.
func TestAFlappingVolumeAlarmStillReachesTheAdministrators(t *testing.T) {
	t.Parallel()
	const store = 1900 << 20
	free := func(factor float64) int64 { return int64(factor * store) }
	clock := t0
	tracker := statelog.NewTracker(nil, func() time.Time { return clock })
	var said []statelog.AlarmTransition
	tracker.OnTransition("node-a", func(tr statelog.AlarmTransition) { said = append(said, tr) })
	volume := string(statelog.KindVolumeLow)

	// The first ten minutes alternate every beat between short of the raise
	// and short of the clear; then each six minutes spend two past the clear.
	reading := func(at time.Duration, beat int) int64 {
		if at < 10*time.Minute {
			return free([]float64{1.05, 1.18}[beat%2])
		}
		if (at-10*time.Minute)%(6*time.Minute) < 2*time.Minute {
			return free(1.4)
		}
		return free(1.05)
	}
	cleared := 0
	notified := false
	for beat := 0; ; beat++ {
		at := time.Duration(beat) * statelog.AlarmInterval
		if at > 45*time.Minute {
			break
		}
		clock = t0.Add(at)
		up, _ := tracker.Standing()
		tracker.Observe(t.Context(), statelog.Evaluate(statelog.Reading{Up: up,
			FreeBytes: reading(at, beat), StoreBytes: store, StoreVolume: "/var/lib/crewlet"}))
		for _, tr := range said {
			if tr.Change == statelog.TransitionCleared {
				cleared++
			}
		}
		said = said[:0]
		if at < 10*time.Minute && cleared > 0 {
			t.Fatalf("at %v the alarm cleared on a reading short of its clear margin", at)
		}
		marks, _ := tracker.Marks()
		got := notice.FromAlarms([]notice.NodeAlarms{{Node: "node-a", Marks: marks}}, clock)
		standing := len(marks) > 0
		switch {
		case at < notice.AlarmAfter && len(got) != 0:
			t.Fatalf("at %v, inside the grace, the notices are %+v, want none", at, got)
		case at >= notice.AlarmAfter && standing:
			want := []notice.Notice{alarm(volume, t0, "node-a")}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("at %v the notices are %+v, want one dated from the first raise %+v", at, got, want)
			}
			notified = true
		}
	}
	if cleared < 3 {
		t.Fatalf("the alarm cleared %d times, want the reading past the margin to clear it each cycle", cleared)
	}
	if !notified {
		t.Fatal("no beat past the grace found the alarm standing: the case proves nothing")
	}
}
