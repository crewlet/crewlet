package worklet_test

import (
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/worklet"
)

// flowChart is the chart the flow duty reads: each project's lead, and which
// seats are agents.
type flowChart struct {
	leads  map[string]string
	agents map[string]bool
}

func (c flowChart) ProjectLead(project string) string { return c.leads[project] }
func (c flowChart) UnitLead(string) string            { return "" }
func (c flowChart) Agent(handle string) bool          { return c.agents[handle] }

// engChart is ENG led by the person lee, with bot an agent seat.
var engChart = flowChart{leads: map[string]string{"ENG": "lee"}, agents: map[string]bool{"bot": true}}

// livePosture is a seeded projection in which no seat is busy or stopped.
var livePosture = worklet.Posture{Live: true}

// flowDutyOn is the round trip's flow duty: a reader seeing posture, and the
// node's own writer acting as the system, as the engine runs it.
func flowDutyOn(t *testing.T, r *roundTrip, posture worklet.Posture, chart flowChart) *worklet.FlowDuty {
	t.Helper()
	log, err := statelogtest.LocalReader(worklet.Domain{}, r.db,
		statelog.Position{Stream: worklet.Domain{}.Stream().Name, Generation: 1})
	if err != nil {
		t.Fatalf("local read authority: %v", err)
	}
	reader, err := worklet.NewReader(r.db.Reader(), log, fixedPostures(posture))
	if err != nil {
		t.Fatalf("tracker reader: %v", err)
	}
	writer, err := worklet.NewWriter(worklet.WriterDeps{
		Publisher: r.publisher, DB: r.db.Reader(), NodeID: r.nodeID, Claims: r.claims,
		Actor: r.nodeID, ActorKind: worklet.AuthorSystem,
		Now: func() time.Time { return r.at },
	})
	if err != nil {
		t.Fatalf("system writer: %v", err)
	}
	duty, err := worklet.NewFlowDuty(worklet.FlowDutyDeps{Reader: reader, Writer: writer,
		Chart: chart, NodeID: r.nodeID})
	if err != nil {
		t.Fatalf("flow duty: %v", err)
	}
	return duty
}

// flowTick runs one tick at now and applies what it published.
func flowTick(t *testing.T, r *roundTrip, duty *worklet.FlowDuty, now time.Time) worklet.FlowTick {
	t.Helper()
	got, err := duty.Tick(t.Context(), now)
	if err != nil {
		t.Fatalf("tick at %s: %v", now, err)
	}
	r.drain()
	return got
}

// fileInProgress files an item in ENG, in progress on assignee, with edit applied.
func fileInProgress(t *testing.T, r *roundTrip, id, assignee string, edit func(*worklet.Task)) {
	t.Helper()
	task := newTask(id)
	task.Key = "ENG-" + id
	task.Status, task.StatusGroup = worklet.StatusInProgress, worklet.GroupActive
	task.Assignee = assignee
	if edit != nil {
		edit(&task)
	}
	if _, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	r.drain()
}

// toldOf is every notice an item's flow wrote, as `recipient:reason`, in the
// order they were written.
func toldOf(t *testing.T, r *roundTrip, item string) []string {
	t.Helper()
	var out []string
	err := r.db.Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(), `SELECT recipient, reason, kind
			FROM tracker_notifications WHERE task_id = ? ORDER BY log_seq`, item)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var recipient, reason, kind string
			if err := rows.Scan(&recipient, &reason, &kind); err != nil {
				return err
			}
			if kind == string(worklet.ChangeFlow) {
				out = append(out, recipient+":"+reason)
			}
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("read %s's notices: %v", item, err)
	}
	return out
}

// detailOf reads one item with its history.
func detailOf(t *testing.T, r *roundTrip, id string, now time.Time) worklet.TaskDetail {
	t.Helper()
	detail, err := r.reader.Task(t.Context(), id, worklet.DetailWants{History: true,
		Clock: &worklet.DayClock{Now: now, Zone: time.UTC}},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return detail
}

// A FLOW RECORD MOVES NOTHING THE ITEM OWNS (ADR-0162).
//
// The flow is read from the item's version, history and `updated_at`: a record
// of the flow that moved any of them would change the state it records, reset
// the stuck clock it measures and wake everybody an item's change wakes. So a
// change of state and a notice land in `tracker_flow` alone, the notice writes
// the one inbox row its person reads, and the same notice published again
// under another operation lands once.
func TestAFlowRecordMovesNothingTheItemOwns(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	now := time.Now().UTC()
	fileInProgress(t, r, "t-1", "ada", nil)
	before := detailOf(t, r, "t-1", now)

	writer, err := worklet.NewWriter(worklet.WriterDeps{
		Publisher: r.publisher, DB: r.db.Reader(), NodeID: r.nodeID, Claims: r.claims,
		Actor: r.nodeID, ActorKind: worklet.AuthorSystem, Now: func() time.Time { return r.at },
	})
	if err != nil {
		t.Fatalf("system writer: %v", err)
	}
	since := before.Task.UpdatedAt
	change := worklet.FlowRecord{Task: "t-1", State: worklet.FlowStuck, Since: &since, Seat: "ada",
		ID: worklet.FlowChangeID("t-1", worklet.FlowStuck, "", &since, "")}
	notice := change
	notice.Notice, notice.Told = worklet.FlowToldLead, "lee"
	notice.ID = worklet.FlowNoticeID("t-1", worklet.FlowStuck, since, "lee")
	for i, rec := range []worklet.FlowRecord{change, notice, notice} {
		if _, err := writer.RecordFlow(t.Context(), "op-flow-"+string(rune('a'+i)), rec); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		r.drain()
	}

	after := detailOf(t, r, "t-1", now)
	if after.Task.Version != before.Task.Version || !after.Task.UpdatedAt.Equal(before.Task.UpdatedAt) {
		t.Errorf("recording the item's flow moved its version from %d to %d and its "+
			"updated_at from %s to %s", before.Task.Version, after.Task.Version,
			before.Task.UpdatedAt, after.Task.UpdatedAt)
	}
	if len(after.History) != len(before.History) {
		t.Errorf("recording the item's flow wrote %d history rows", len(after.History)-len(before.History))
	}
	if got := toldOf(t, r, "t-1"); !slices.Equal(got, []string{"lee:flow"}) {
		t.Errorf("the notice told %v, want the lead once under the reason flow, "+
			"however often it was published", got)
	}
	if len(after.FlowHistory) != 2 || after.FlowHistory[0].Notice != worklet.FlowToldLead ||
		after.FlowHistory[1].Notice != "" || after.FlowHistory[1].State != worklet.FlowStuck {
		t.Errorf("the item's flow history is %+v, want its notice to the lead over "+
			"its change to stuck, newest first", after.FlowHistory)
	}
}

// A LATE CHECK IS TOLD ONCE PER START (ADR-0162, situation 4).
//
// An item parked until a start is due once the start passes with nothing of
// its assignee's on it since. A person assignee is told once the grace has
// passed and the lead one grace later, each once however many ticks read it;
// an item parked again and due again at a later start is a new transition,
// told again. An agent assignee is never told: its notice is its pickup, so
// its lead is the first person told.
func TestALateCheckIsToldOncePerStart(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	base := time.Now().UTC().Truncate(time.Second)
	start := base.Add(-time.Minute)
	reason := "the release's health check"
	park := func(task *worklet.Task) { task.StartAt, task.WaitReason = &start, reason }
	fileInProgress(t, r, "t-1", "ada", park)
	fileInProgress(t, r, "t-2", "bot", park)
	duty := flowDutyOn(t, r, livePosture, engChart)

	if got := flowTick(t, r, duty, base); got.Changes != 2 || got.Notices != 0 {
		t.Fatalf("the first tick recorded %d changes and told %d, want both items "+
			"recorded due and nobody told", got.Changes, got.Notices)
	}
	flowTick(t, r, duty, start.Add(worklet.FlowDueGrace-time.Minute))
	if got := toldOf(t, r, "t-1"); len(got) != 0 {
		t.Fatalf("an item due for less than the grace told %v", got)
	}
	for _, at := range []time.Duration{1, 2, 3} {
		flowTick(t, r, duty, start.Add(worklet.FlowDueGrace+at*time.Minute))
	}
	if got := toldOf(t, r, "t-1"); !slices.Equal(got, []string{"ada:flow"}) {
		t.Fatalf("past the grace the person assignee's item told %v, want ada once", got)
	}
	if got := toldOf(t, r, "t-2"); !slices.Equal(got, []string{"lee:flow"}) {
		t.Fatalf("past the grace the agent's item told %v, want its lead once and "+
			"never the agent, whose notice is its pickup", got)
	}
	// AND A TICK AFTER PUBLISHES NOTHING: a notice already told is no record
	// on the log every minute, which its key alone would land once anyway.
	if got := flowTick(t, r, duty, start.Add(worklet.FlowDueGrace+4*time.Minute)); got.Changes != 0 ||
		got.Notices != 0 {
		t.Fatalf("a tick after both were told published %d changes and %d notices", got.Changes,
			got.Notices)
	}
	for _, at := range []time.Duration{1, 2} {
		flowTick(t, r, duty, start.Add(2*worklet.FlowDueGrace+at*time.Minute))
	}
	if got := toldOf(t, r, "t-1"); !slices.Equal(got, []string{"ada:flow", "lee:flow"}) {
		t.Fatalf("a grace later the person's item told %v, want ada then the lead", got)
	}
	if got := toldOf(t, r, "t-2"); !slices.Equal(got, []string{"lee:flow"}) {
		t.Fatalf("the agent's item told %v a grace later, want its lead once", got)
	}

	// PARKED AGAIN, AND DUE AGAIN AT THE LATER START: a new transition.
	later := base.Add(2 * time.Hour)
	if _, err := r.writer.UpdateTask(t.Context(), "op-repark", "t-1", "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{StartAt: &later, WaitReason: &reason}, worklet.ChangeFields,
		nil); err != nil {
		t.Fatalf("park again: %v", err)
	}
	r.drain()
	flowTick(t, r, duty, base.Add(time.Hour))
	for _, at := range []time.Duration{1, 2} {
		flowTick(t, r, duty, later.Add(worklet.FlowDueGrace+at*time.Minute))
	}
	if got := toldOf(t, r, "t-1"); !slices.Equal(got, []string{"ada:flow", "lee:flow", "ada:flow"}) {
		t.Fatalf("due again at a later start the item told %v, want ada told again", got)
	}
}

// A FORGOTTEN ITEM IS TOLD, THEN ITS LEAD (ADR-0162, situation 6).
//
// An item in progress nothing changed on for its project's window is stuck:
// its person assignee is told when it becomes stuck, and the project's lead a
// window later if it is still stuck since the same change. An agent's stuck
// item tells its lead at once.
func TestAForgottenItemIsToldThenItsLead(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	fileInProgress(t, r, "t-1", "ada", nil)
	fileInProgress(t, r, "t-2", "bot", nil)
	duty := flowDutyOn(t, r, livePosture, engChart)
	changed := detailOf(t, r, "t-1", time.Now()).Task.UpdatedAt
	window := worklet.DefaultStuckAfterHours * time.Hour

	flowTick(t, r, duty, changed.Add(time.Hour))
	flowTick(t, r, duty, changed.Add(window+time.Minute))
	flowTick(t, r, duty, changed.Add(window+2*time.Minute))
	if got := toldOf(t, r, "t-1"); !slices.Equal(got, []string{"ada:flow"}) {
		t.Fatalf("a stuck item told %v, want its person assignee", got)
	}
	if got := toldOf(t, r, "t-2"); !slices.Equal(got, []string{"lee:flow"}) {
		t.Fatalf("an agent's stuck item told %v, want its lead and never the agent", got)
	}
	flowTick(t, r, duty, changed.Add(2*window-time.Minute))
	if got := toldOf(t, r, "t-1"); len(got) != 1 {
		t.Fatalf("within a window of becoming stuck the item told %v", got)
	}
	flowTick(t, r, duty, changed.Add(2*window+time.Minute))
	flowTick(t, r, duty, changed.Add(2*window+2*time.Minute))
	if got := toldOf(t, r, "t-1"); !slices.Equal(got, []string{"ada:flow", "lee:flow"}) {
		t.Fatalf("a window later the stuck item told %v, want ada then the lead", got)
	}
	if got := toldOf(t, r, "t-2"); !slices.Equal(got, []string{"lee:flow"}) {
		t.Fatalf("the agent's stuck item told its lead again: %v", got)
	}
}

// A HELD ITEM TELLS THE LEAD ONLY WHEN IT COSTS (ADR-0162, situation 1).
//
// An item whose seat is out of its model limit reads held, and a hold that
// ends on its own on an item nothing waits for asks nobody to act. So only the
// lead is told, and only when the hold costs something: the item is urgent
// (told at once), or it has gone quiet past its window while held.
func TestAHeldItemTellsTheLeadOnlyWhenItCosts(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	fileInProgress(t, r, "t-1", "bot", func(task *worklet.Task) { task.Priority = worklet.PriorityUrgent })
	fileInProgress(t, r, "t-2", "bot", nil)
	changed := detailOf(t, r, "t-2", time.Now()).Task.UpdatedAt
	stopped := changed.Add(-5 * time.Minute)
	resets := changed.Add(3 * time.Hour)
	posture := worklet.Posture{Live: true, Seats: map[string]worklet.SeatPosture{
		"bot": {Stop: worklet.FlowCauseLimit, StopSince: stopped, StopUntil: resets},
	}}
	duty := flowDutyOn(t, r, posture, engChart)

	flowTick(t, r, duty, changed.Add(time.Minute))
	flowTick(t, r, duty, changed.Add(2*time.Minute))
	if got := toldOf(t, r, "t-1"); !slices.Equal(got, []string{"lee:flow"}) {
		t.Fatalf("an urgent item held by its seat's model limit told %v, want its lead at once", got)
	}
	if got := toldOf(t, r, "t-2"); len(got) != 0 {
		t.Fatalf("a held item nothing waits for told %v, want nobody", got)
	}
	window := worklet.DefaultStuckAfterHours * time.Hour
	flowTick(t, r, duty, changed.Add(window+time.Minute))
	if got := toldOf(t, r, "t-2"); !slices.Equal(got, []string{"lee:flow"}) {
		t.Fatalf("a held item quiet past its window told %v, want its lead", got)
	}
	if got := toldOf(t, r, "t-1"); len(got) != 1 {
		t.Fatalf("the urgent held item was told again: %v", got)
	}
	detail := detailOf(t, r, "t-1", changed.Add(window+time.Minute))
	if len(detail.FlowHistory) == 0 || detail.FlowHistory[len(detail.FlowHistory)-1].Cause !=
		worklet.FlowCauseLimit {
		t.Errorf("the held item's flow history is %+v, want its hold by the limit "+
			"recorded", detail.FlowHistory)
	}
}

// A BLOCKED ITEM IS BLOCKED SINCE ITS CHANGE WAS RECORDED (ADR-0162).
//
// A blocker gives no instant of its own, so the flow reads a blocked item's
// `since` from the change the duty recorded, and none before it is recorded.
func TestABlockedItemIsBlockedSinceItsChangeWasRecorded(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	blocker := newTask("t-0")
	if _, err := r.writer.CreateTask(t.Context(), "op-t-0", blocker, nil); err != nil {
		t.Fatalf("create the blocker: %v", err)
	}
	r.drain()
	fileInProgress(t, r, "t-1", "ada", func(task *worklet.Task) {
		task.Relations = []worklet.Relation{{Kind: worklet.RelationWaitingOn, Other: "t-0"}}
	})
	now := time.Now().UTC()
	if flow := detailOf(t, r, "t-1", now).Flow; flow == nil || flow.State != worklet.FlowBlocked ||
		flow.Since != nil {
		t.Fatalf("before the duty recorded it the item reads %+v, want blocked since nothing", flow)
	}
	flowTick(t, r, flowDutyOn(t, r, livePosture, engChart), now)
	detail := detailOf(t, r, "t-1", now)
	if len(detail.FlowHistory) != 1 || detail.Flow == nil || detail.Flow.Since == nil ||
		!detail.Flow.Since.Equal(detail.FlowHistory[0].At) {
		t.Fatalf("after the duty recorded it the item reads %+v beside %+v, want blocked "+
			"since the recorded change", detail.Flow, detail.FlowHistory)
	}
}

// A FLOW NOTICE IS MOOT ONLY WHERE THE READ CAN TELL (ADR-0162).
//
// A read with no live facts cannot see a turn, a parked run or a stopped seat,
// so it settles a notice of a hold only where an estate state that comes before
// held reads instead; any read settles an item no longer in progress.
func TestAFlowNoticeIsMootOnlyWhereTheReadCanTell(t *testing.T) {
	t.Parallel()
	read := func(state worklet.FlowState) *worklet.Flow { return &worklet.Flow{State: state} }
	cases := []struct {
		told worklet.FlowState
		now  *worklet.Flow
		live bool
		moot bool
	}{
		{worklet.FlowStuck, nil, false, true},
		{worklet.FlowStuck, read(worklet.FlowStuck), true, false},
		{worklet.FlowStuck, read(worklet.FlowMoving), true, true},
		{worklet.FlowStuck, read(worklet.FlowMoving), false, true},
		{worklet.FlowDue, read(worklet.FlowWaiting), false, true},
		{worklet.FlowHeld, read(worklet.FlowDue), false, false},
		{worklet.FlowHeld, read(worklet.FlowMoving), true, true},
		{worklet.FlowHeld, read(worklet.FlowBlocked), false, true},
		{worklet.FlowHeld, read(worklet.FlowHeld), false, false},
	}
	for _, c := range cases {
		state := "not in progress"
		if c.now != nil {
			state = string(c.now.State)
		}
		if got := worklet.FlowLeft(c.told, c.now, c.live); got != c.moot {
			t.Errorf("a notice of %s on an item read %s (live %v): moot %v, want %v",
				c.told, state, c.live, got, c.moot)
		}
	}
}
