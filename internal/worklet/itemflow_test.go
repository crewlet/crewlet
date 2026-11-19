package worklet_test

import (
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/worklet"
)

// fixedPostures is a live projection that always answers one posture.
type fixedPostures worklet.Posture

func (p fixedPostures) SeatPostures() worklet.Posture { return worklet.Posture(p) }

// flowHarness is one node's estate, its applier and a reader whose seats'
// live facts are the posture a case states.
type flowHarness struct {
	*applyHarness
	reader *worklet.Reader
}

func newFlowHarness(t *testing.T, posture worklet.Posture) *flowHarness {
	t.Helper()
	h := newApplyHarness(t)
	log, err := statelogtest.LocalReader(worklet.Domain{}, h.db,
		statelog.Position{Stream: worklet.Domain{}.Stream().Name, Generation: 1})
	if err != nil {
		t.Fatalf("local read authority: %v", err)
	}
	reader, err := worklet.NewReader(h.db.Reader(), log, fixedPostures(posture))
	if err != nil {
		t.Fatalf("tracker reader: %v", err)
	}
	return &flowHarness{applyHarness: h, reader: reader}
}

// record applies one record written by actor at `at`, the instant the broker
// stored it, which is the instant every derived time reads.
func (h *flowHarness) record(subject worklet.Subject, op worklet.OpKind,
	kind worklet.ChangeKind, body any, actor string, at time.Time) {

	h.t.Helper()
	rec := worklet.MutationRecord{
		Kind: kind,
		RecordEnvelope: worklet.RecordEnvelope{
			V: worklet.RecordVersion, OpID: "op-" + strconv.FormatUint(h.seq+1, 10),
			Subject: subject, Op: op, CreatedAt: at, Writer: "node-a",
			Scope: worklet.ScopeSet{Subject: true, Container: "ENG"},
		},
		Mutation: mustJSON(body), Actor: actor, ActorKind: worklet.AuthorAgent,
	}
	if _, err := h.apply(rec, at); err != nil {
		h.t.Fatalf("apply %s %s: %v", op, subject.ID, err)
	}
}

// task files one item in ENG at `at`, in progress and on ada unless edit says
// otherwise.
func (h *flowHarness) task(id string, at time.Time, edit func(*worklet.Task)) {
	h.t.Helper()
	task := newTask(id)
	task.Key = "ENG-" + id
	task.Status, task.StatusGroup = worklet.StatusInProgress, worklet.GroupActive
	task.Assignee = "ada"
	task.CreatedAt, task.UpdatedAt = at, at
	if edit != nil {
		edit(&task)
	}
	h.record(worklet.TaskSubject(id), worklet.OpCreate, worklet.ChangeCreated, task, "lead", at)
}

// roster applies the chart's seats.
func (h *flowHarness) roster(at time.Time, seats ...string) {
	h.t.Helper()
	h.record(worklet.RosterSubject(), worklet.OpPatch, "", worklet.Roster{
		V: worklet.DocumentVersion, Seats: seats, ChartEpoch: 1, UpdatedAt: at,
	}, "", at)
}

// turn applies one ended turn segment of seat on an item.
func (h *flowHarness) turn(item, seat string, at time.Time) {
	h.t.Helper()
	h.record(worklet.TurnSubject(item), worklet.OpTurn, "", map[string]any{
		"task": item, "seat": seat, "spend": map[string]int{"turns": 1},
	}, seat, at)
}

// flows reads one item's flow the three ways a reader can: its board row,
// its detail and the assignee's own day, and holds them to one answer.
func (h *flowHarness) flows(id, assignee string, now time.Time) (*worklet.Flow, worklet.TaskRow) {
	h.t.Helper()
	q, err := worklet.ParseQuery(worklet.MapParams(map[string]any{}), now, time.UTC)
	if err != nil {
		h.t.Fatalf("ParseQuery: %v", err)
	}
	q.Level = statelog.ReadStale
	answer, err := h.reader.Tasks(h.t.Context(), q, now)
	if err != nil {
		h.t.Fatalf("Tasks: %v", err)
	}
	var row *worklet.TaskRow
	for i := range answer.Rows {
		if answer.Rows[i].ID == id {
			row = &answer.Rows[i]
		}
	}
	if row == nil {
		h.t.Fatalf("the board does not hold %s: %v", id, ids(answer))
	}
	detail, err := h.reader.Task(h.t.Context(), id, worklet.DetailWants{
		Clock: &worklet.DayClock{Now: now, Zone: time.UTC},
	}, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		h.t.Fatalf("Task: %v", err)
	}
	if !reflect.DeepEqual(detail.Flow, row.Flow) {
		h.t.Errorf("the detail reads %s as %s and its board row as %s",
			id, flowText(detail.Flow), flowText(row.Flow))
	}
	if answer.FlowLive != detail.FlowLive {
		h.t.Errorf("the board says flow_live %v and the detail %v",
			answer.FlowLive, detail.FlowLive)
	}
	if assignee != "" {
		day, err := h.reader.MyWork(h.t.Context(), worklet.MyWorkQuery{
			Handle: assignee, Level: statelog.ReadStale}, now, time.UTC)
		if err != nil {
			h.t.Fatalf("MyWork: %v", err)
		}
		for _, mine := range day.Assigned {
			if mine.ID == id && !reflect.DeepEqual(mine.Flow, row.Flow) {
				h.t.Errorf("my_work reads %s as %s and its board row as %s",
					id, flowText(mine.Flow), flowText(row.Flow))
			}
		}
	}
	return row.Flow, *row
}

func flowText(f *worklet.Flow) string {
	if f == nil {
		return "no flow"
	}
	text := string(f.State) + "/" + string(f.Cause)
	if f.Since != nil {
		text += " since " + f.Since.Format(time.RFC3339)
	}
	if f.Until != nil {
		text += " until " + f.Until.Format(time.RFC3339)
	}
	if f.Reason != "" {
		text += " (" + f.Reason + ")"
	}
	return text
}

// AN ITEM IN PROGRESS SAYS WHY IT IS OR IS NOT MOVING, the first state that
// holds winning (ADR-0162): a row per state and per precedence pair, read on
// the board, the detail and the assignee's day alike.
//
// Each situation the record was measured on has its row: an item parked until
// a release's health check reads waiting (3) and, once its start passed with
// nothing done, due (4); an item nobody touched for the project's window reads
// stuck (6); a removed seat's item reads held/departed (7); and an urgent item
// on a seat out of its model limit reads held/limit until the reset (1).
func TestAnItemInProgressSaysWhyItIsNotMoving(t *testing.T) {
	t.Parallel()
	created := time.Date(2031, 4, 16, 9, 0, 0, 0, time.UTC)
	now := created.Add(2 * time.Hour)
	reset := now.Add(90 * time.Minute)
	tomorrow := now.Add(24 * time.Hour)
	passed := now.Add(-time.Hour)
	turnStart := now.Add(-5 * time.Minute)
	limited := worklet.SeatPosture{Stop: worklet.FlowCauseLimit, StopSince: created, StopUntil: reset}
	working := worklet.SeatPosture{On: []worklet.PostureItem{
		{Item: "it", Cause: worklet.FlowCauseTurn, Since: turnStart}}}
	live := func(seats map[string]worklet.SeatPosture) worklet.Posture {
		return worklet.Posture{Live: true, Seats: seats}
	}
	blocked := func(task *worklet.Task) {
		task.Relations = []worklet.Relation{{Kind: worklet.RelationWaitingOn,
			Other: "https://github.com/acme/web/pull/12"}}
	}
	parkedUntil := func(start time.Time) func(*worklet.Task) {
		return func(task *worklet.Task) {
			task.StartAt, task.WaitReason = &start, "the release's health check"
		}
	}

	for name, c := range map[string]struct {
		posture worklet.Posture
		edit    func(*worklet.Task)
		seed    func(h *flowHarness)
		now     time.Time
		want    *worklet.Flow // Since nil means "since the last change"
		live    bool
	}{
		"an item to do has no flow": {
			edit: func(task *worklet.Task) {
				task.Status, task.StatusGroup = worklet.StatusTodo, worklet.GroupNotStarted
			},
			want: nil,
		},
		"an ordinary item is moving": {
			want: &worklet.Flow{State: worklet.FlowMoving},
		},
		"a turn on it is working": {
			posture: live(map[string]worklet.SeatPosture{"ada": working}),
			want:    &worklet.Flow{State: worklet.FlowWorking, Cause: worklet.FlowCauseTurn, Since: &turnStart},
			live:    true,
		},
		"another seat's turn on it is not the assignee working": {
			posture: live(map[string]worklet.SeatPosture{"bo": working}),
			want:    &worklet.Flow{State: worklet.FlowMoving},
			live:    true,
		},
		"working comes before blocked": {
			posture: live(map[string]worklet.SeatPosture{"ada": working}),
			edit:    blocked,
			want:    &worklet.Flow{State: worklet.FlowWorking, Cause: worklet.FlowCauseTurn, Since: &turnStart},
			live:    true,
		},
		"an open blocker is blocked": {
			edit: blocked,
			want: &worklet.Flow{State: worklet.FlowBlocked},
		},
		"blocked comes before waiting": {
			edit: func(task *worklet.Task) { blocked(task); parkedUntil(tomorrow)(task) },
			want: &worklet.Flow{State: worklet.FlowBlocked},
		},
		"situation 3: parked until a later start is waiting": {
			edit: parkedUntil(tomorrow),
			want: &worklet.Flow{State: worklet.FlowWaiting, Cause: worklet.FlowCauseStart,
				Until: &tomorrow, Reason: "the release's health check"},
		},
		"the assignee's own open question is waiting": {
			seed: func(h *flowHarness) {
				h.record(worklet.TaskSubject("it"), worklet.OpPatch, worklet.ChangeComment,
					worklet.TaskPatch{Comment: &worklet.Comment{ID: "c-1", Task: "it",
						Author: "ada", AuthorKind: worklet.AuthorAgent, Ask: "bo",
						Body: "which region?", CreatedAt: created}}, "ada", created)
			},
			want: &worklet.Flow{State: worklet.FlowWaiting, Cause: worklet.FlowCauseQuestion},
		},
		"the assignee's run parked on it is waiting": {
			posture: live(map[string]worklet.SeatPosture{"ada": {Parked: "it", ParkedSince: passed}}),
			want:    &worklet.Flow{State: worklet.FlowWaiting, Cause: worklet.FlowCauseParked, Since: &passed},
			live:    true,
		},
		"waiting comes before held": {
			posture: live(map[string]worklet.SeatPosture{"ada": limited}),
			edit:    parkedUntil(tomorrow),
			want: &worklet.Flow{State: worklet.FlowWaiting, Cause: worklet.FlowCauseStart,
				Until: &tomorrow, Reason: "the release's health check"},
			live: true,
		},
		"no assignee is held": {
			edit: func(task *worklet.Task) { task.Assignee = "" },
			want: &worklet.Flow{State: worklet.FlowHeld, Cause: worklet.FlowCauseNobody},
		},
		"situation 7: an assignee gone from the chart is held": {
			seed: func(h *flowHarness) { h.roster(created, "bo", "cy") },
			want: &worklet.Flow{State: worklet.FlowHeld, Cause: worklet.FlowCauseDeparted},
		},
		"an estate holding no roster departs nobody": {
			want: &worklet.Flow{State: worklet.FlowMoving},
		},
		"situation 1: a seat out of its model limit is held until the reset": {
			posture: live(map[string]worklet.SeatPosture{"ada": limited}),
			want: &worklet.Flow{State: worklet.FlowHeld, Cause: worklet.FlowCauseLimit,
				Since: &created, Until: &reset},
			live: true,
		},
		"the assignee's run parked on another item holds this one": {
			posture: live(map[string]worklet.SeatPosture{"ada": {Parked: "other", ParkedSince: passed}}),
			want:    &worklet.Flow{State: worklet.FlowHeld, Cause: worklet.FlowCauseParkedElsewhere, Since: &passed},
			live:    true,
		},
		"held comes before due": {
			posture: live(map[string]worklet.SeatPosture{"ada": limited}),
			edit:    parkedUntil(passed),
			want: &worklet.Flow{State: worklet.FlowHeld, Cause: worklet.FlowCauseLimit,
				Since: &created, Until: &reset},
			live: true,
		},
		"situation 4: a start passed with nothing done is due": {
			edit: parkedUntil(passed),
			want: &worklet.Flow{State: worklet.FlowDue, Since: &passed},
		},
		"a turn of the assignee since the start ends due": {
			edit: parkedUntil(passed),
			seed: func(h *flowHarness) { h.turn("it", "ada", passed.Add(10*time.Minute)) },
			want: &worklet.Flow{State: worklet.FlowMoving},
		},
		"a turn before the start or of another seat does not": {
			edit: parkedUntil(passed),
			seed: func(h *flowHarness) {
				h.turn("it", "ada", passed.Add(-10*time.Minute))
				h.turn("it", "bo", passed.Add(10*time.Minute))
			},
			want: &worklet.Flow{State: worklet.FlowDue, Since: &passed},
		},
		"the assignee's own write since the start ends due": {
			edit: parkedUntil(passed),
			seed: func(h *flowHarness) {
				h.record(worklet.TaskSubject("it"), worklet.OpPatch, worklet.ChangeFields,
					worklet.TaskPatch{Title: ptr("checked")}, "ada", passed.Add(time.Minute))
			},
			want: &worklet.Flow{State: worklet.FlowMoving},
		},
		"situation 6: nothing changed for the window is stuck": {
			now:  created.Add(9 * time.Hour),
			want: &worklet.Flow{State: worklet.FlowStuck},
		},
		"the project's own window decides stuck": {
			seed: func(h *flowHarness) {
				h.record(worklet.ProjectSubject("ENG"), worklet.OpCreate, worklet.ChangeProjectCreated,
					worklet.Project{V: worklet.DocumentVersion, Key: "ENG", Name: "Engineering",
						StuckAfterHours: 24, CreatedAt: created, UpdatedAt: created}, "lead", created)
			},
			now:  created.Add(9 * time.Hour),
			want: &worklet.Flow{State: worklet.FlowMoving},
		},
		"due comes before stuck": {
			edit: parkedUntil(created.Add(time.Hour)),
			now:  created.Add(9 * time.Hour),
			want: &worklet.Flow{State: worklet.FlowDue, Since: ptr(created.Add(time.Hour))},
		},
		"a projection not yet seeded reads no live fact and says so": {
			posture: worklet.Posture{Seats: map[string]worklet.SeatPosture{"ada": limited}},
			want:    &worklet.Flow{State: worklet.FlowMoving},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newFlowHarness(t, c.posture)
			h.task("it", created, c.edit)
			if c.seed != nil {
				c.seed(h)
			}
			read := now
			if !c.now.IsZero() {
				read = c.now
			}
			got, row := h.flows("it", row0Assignee(c.edit), read)
			want := c.want
			if want != nil && want.Since == nil &&
				(want.State == worklet.FlowMoving || want.State == worklet.FlowStuck) {
				copied := *want
				copied.Since = &row.Updated
				want = &copied
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("the item reads %s, want %s", flowText(got), flowText(want))
			}
		})
	}
}

// row0Assignee is whom a case's item is assigned to, for its my_work read.
func row0Assignee(edit func(*worklet.Task)) string {
	task := worklet.Task{Assignee: "ada"}
	if edit != nil {
		edit(&task)
	}
	return task.Assignee
}

// A ROW'S FLOW SAYS WHETHER IT READ THE SEATS' LIVE FACTS: a node whose
// projection is not seeded answers `flow_live: false` on every surface rather
// than letting a stopped seat's work read as moving with nobody the wiser.
func TestAnAnswerSaysWhetherItsFlowReadTheLiveFacts(t *testing.T) {
	t.Parallel()
	created := time.Date(2031, 4, 16, 9, 0, 0, 0, time.UTC)
	now := created.Add(time.Hour)
	for _, live := range []bool{false, true} {
		h := newFlowHarness(t, worklet.Posture{Live: live})
		h.task("it", created, nil)
		q, err := worklet.ParseQuery(worklet.MapParams(map[string]any{}), now, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		q.Level = statelog.ReadStale
		board, err := h.reader.Tasks(t.Context(), q, now)
		if err != nil {
			t.Fatal(err)
		}
		day, err := h.reader.MyWork(t.Context(), worklet.MyWorkQuery{Handle: "ada",
			Level: statelog.ReadStale}, now, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		ready, err := h.reader.ReadyWork(t.Context(), worklet.ReadyWorkQuery{Handle: "ada",
			Level: statelog.ReadStale}, now)
		if err != nil {
			t.Fatal(err)
		}
		load, err := h.reader.Workload(t.Context(), worklet.WorkloadQuery{
			Level: statelog.ReadStale}, now, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		for surface, said := range map[string]bool{"the board": board.FlowLive,
			"my_work": day.FlowLive, "ready work": ready.FlowLive, "Workload": load.FlowLive} {
			if said != live {
				t.Errorf("with a projection seeded %v, %s says flow_live %v", live, surface, said)
			}
		}
		if len(ready.Items) != 1 || ready.Items[0].Flow == nil ||
			ready.Items[0].Flow.State != worklet.FlowMoving {
			t.Errorf("ready work does not carry the item's flow: %+v", ready.Items)
		}
	}
}

// SITUATION 3: A SEAT'S LOAD IS THE WORK THAT WANTS IT NOW. An item its agent
// parked until tomorrow is open work and no load; so is blocked work and work
// a stopped seat holds. Load is working, moving, due and stuck, read by the
// same expression as every row, inside Workload's one statement.
func TestWorkloadCountsOnlyWorkThatWantsTheSeat(t *testing.T) {
	t.Parallel()
	created := time.Date(2031, 4, 16, 9, 0, 0, 0, time.UTC)
	now := created.Add(2 * time.Hour)
	tomorrow := now.Add(24 * time.Hour)
	passed := now.Add(-time.Hour)
	h := newFlowHarness(t, worklet.Posture{Live: true, Seats: map[string]worklet.SeatPosture{
		"ada": {On: []worklet.PostureItem{{Item: "on", Cause: worklet.FlowCauseRun, Since: passed}}},
		"bo":  {Stop: worklet.FlowCauseBudget},
	}})
	h.task("on", created, nil)
	h.task("plain", created, nil)
	h.task("parked", created, func(task *worklet.Task) {
		task.StartAt, task.WaitReason = &tomorrow, "the release's health check"
	})
	h.task("late", created, func(task *worklet.Task) { task.StartAt = &passed })
	h.task("blocked", created, func(task *worklet.Task) {
		task.Relations = []worklet.Relation{{Kind: worklet.RelationWaitingOn,
			Other: "https://github.com/acme/web/pull/12"}}
	})
	h.task("queued", created, func(task *worklet.Task) {
		task.Status, task.StatusGroup = worklet.StatusTodo, worklet.GroupNotStarted
	})
	h.task("stopped", created, func(task *worklet.Task) { task.Assignee = "bo" })

	load, err := h.reader.Workload(t.Context(), worklet.WorkloadQuery{
		Level: statelog.ReadStale}, now, time.UTC)
	if err != nil {
		t.Fatalf("Workload: %v", err)
	}
	rows := map[string]worklet.WorkloadRow{}
	for _, row := range load.Rows {
		rows[row.Handle] = row
	}
	ada, bo := rows["ada"], rows["bo"]
	if want := (worklet.FlowCounts{Working: 1, Moving: 1, Waiting: 1, Due: 1, Blocked: 1}); ada.Flow != want {
		t.Errorf("ada's items in progress count %+v, want %+v", ada.Flow, want)
	}
	if ada.Open != 6 {
		t.Errorf("ada holds %d open items, want 6: the flow splits load, never the open count", ada.Open)
	}
	if ada.Load != 3 {
		t.Errorf("ada's load is %d, want 3 (working, moving, due): an item parked until "+
			"tomorrow and a blocked one are open work, not load", ada.Load)
	}
	if want := (worklet.FlowCounts{Held: 1}); bo.Flow != want || bo.Load != 0 {
		t.Errorf("bo, out of budget, counts %+v with load %d, want %+v and no load",
			bo.Flow, bo.Load, want)
	}
	if !load.FlowLive {
		t.Error("a Workload read on a seeded projection says flow_live false")
	}
}
