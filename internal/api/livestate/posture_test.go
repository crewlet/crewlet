package livestate_test

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/worklet"
)

// projected is a live projection as the engine installs it for the tracker's
// reads: its seats named by the handles items are assigned to.
type projected struct {
	state *livestate.LiveState
	seats map[string]string
}

func (p projected) SeatPostures() worklet.Posture { return p.state.Posture(p.seats) }

// flowEstate is one node's tracker estate, read through a reader whose live
// facts are a projection's.
type flowEstate struct {
	t       *testing.T
	db      store.ReplicatedHandle
	applier *worklet.Applier
	reader  *worklet.Reader
	seq     uint64
}

func newFlowEstate(t *testing.T, postures worklet.Postures) *flowEstate {
	t.Helper()
	node, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() {
		if err := node.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	log, err := statelogtest.LocalReader(worklet.Domain{}, db,
		statelog.Position{Stream: worklet.Domain{}.Stream().Name, Generation: 1})
	if err != nil {
		t.Fatalf("local read authority: %v", err)
	}
	reader, err := worklet.NewReader(db.Reader(), log, postures)
	if err != nil {
		t.Fatalf("tracker reader: %v", err)
	}
	return &flowEstate{t: t, db: db, applier: worklet.NewApplier(nil), reader: reader}
}

// task files one item in progress on assignee, last touched at `at`.
func (e *flowEstate) task(id, assignee string, priority worklet.Priority, at time.Time) {
	e.t.Helper()
	body, err := json.Marshal(worklet.Task{
		V: worklet.DocumentVersion, ID: id, Key: "ENG-" + id, Project: "ENG", Type: "task",
		Title: "item " + id, Status: worklet.StatusInProgress, StatusGroup: worklet.GroupActive,
		Priority: priority, Rank: worklet.RankOrigin, Assignee: assignee,
		CreatedAt: at, UpdatedAt: at,
	})
	if err != nil {
		e.t.Fatalf("encode %s: %v", id, err)
	}
	e.seq++
	rec := worklet.MutationRecord{
		Kind: worklet.ChangeCreated,
		RecordEnvelope: worklet.RecordEnvelope{
			V: worklet.RecordVersion, OpID: "op-" + id, Subject: worklet.TaskSubject(id),
			Op: worklet.OpCreate, CreatedAt: at, Writer: "node-a",
			Scope: worklet.ScopeSet{Subject: true, Container: "ENG"},
		},
		Mutation: body, Actor: "lead", ActorKind: worklet.AuthorAgent,
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		e.t.Fatalf("encode the record: %v", err)
	}
	if err := e.db.Tx(e.t.Context(), func(tx *sql.Tx) error {
		_, err := e.applier.Apply(e.t.Context(), tx, statelog.Record{
			Envelope: statelog.Envelope{V: rec.V, Kind: string(rec.Subject.Kind),
				Subject: statelog.Subject{Kind: string(rec.Subject.Kind), ID: rec.Subject.ID},
				Op:      string(rec.Op), OpID: rec.OpID, Writer: rec.Writer},
			Position: statelog.Position{Stream: worklet.Domain{}.Stream().Name, Generation: 1, Seq: e.seq},
			Payload:  payload, StoredAt: at,
		}, statelog.ApplyOptions{Now: at, StoredAt: at, MaxVariables: e.db.Caps().MaxVariables})
		return err
	}); err != nil {
		e.t.Fatalf("apply %s: %v", id, err)
	}
}

// flows reads every item's flow off the board at now, and whether the answer
// read the live facts.
func (e *flowEstate) flows(now time.Time) (map[string]string, bool) {
	e.t.Helper()
	q, err := worklet.ParseQuery(worklet.MapParams(map[string]any{}), now, time.UTC)
	if err != nil {
		e.t.Fatalf("ParseQuery: %v", err)
	}
	q.Level = statelog.ReadStale
	answer, err := e.reader.Tasks(e.t.Context(), q, now)
	if err != nil {
		e.t.Fatalf("Tasks: %v", err)
	}
	out := map[string]string{}
	for _, row := range answer.Rows {
		out[row.ID] = flowWords(row.Flow)
	}
	return out, answer.FlowLive
}

func flowWords(f *worklet.Flow) string {
	if f == nil {
		return "no flow"
	}
	words := string(f.State) + "/" + string(f.Cause)
	if f.Since != nil {
		words += " since " + f.Since.Format(time.RFC3339)
	}
	if f.Until != nil {
		words += " until " + f.Until.Format(time.RFC3339)
	}
	return words
}

// workItem is a Worklet item as a turn's or a run's event names it.
func workItem(id string) map[string]any {
	return map[string]any{"backend": string(types.WorkWorklet), "id": id, "key": "ENG-" + id,
		"project": "ENG"}
}

// AN ITEM ON A SEAT THAT CANNOT TAKE A TURN SAYS WHY AND UNTIL WHEN, from the
// projection's own facts (ADR-0162, situation 1): an urgent item on a seat at
// its model limit reads held/limit until the vendor's reset, as does the item
// its parked turn was on. The stop is the seat's own ranking: a person's pause
// outranks the limit and names no reset. Seats that are moving say on what: a
// turn, a run, a run waiting on a person, which holds that seat's other work.
// Until every record the posture reads has landed, nothing is claimed.
//
// Mutation: rank the limit before the pause in stopOf, and the paused seat's
// items read held/limit; drop StopUntil, and the reset goes missing.
func TestAFlowReadsTheSeatsStopReasonAndItsReset(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 27, 23, 20, 0, 0, time.UTC)
	s := livestate.New(livestate.WithClock(func() time.Time { return now }))
	seats := map[string]string{"id-lead": "lead", "id-dev": "dev", "id-ops": "ops"}
	e := newFlowEstate(t, projected{state: s, seats: seats})
	touched := now.Add(-time.Hour)
	for id, on := range map[string]string{"urgent": "lead", "busy": "lead", "dev-turn": "dev",
		"dev-run": "dev", "asked": "ops", "ops-other": "ops"} {
		priority := worklet.PriorityNormal
		if id == "urgent" {
			priority = worklet.PriorityUrgent
		}
		e.task(id, on, priority, touched)
	}

	// The lead's turn on one item meets its models' limit and parks;
	// the dev's turn is on another.
	turn := func(agent, turnID, item, started string) {
		s.Apply(env("agent_turn_started", map[string]any{"agent_id": agent, "turn_id": turnID,
			"work_item": workItem(item), "started_at": started}, at(started), id("ts-"+turnID)))
	}
	turn("id-lead", "tn-lead", "busy", "2026-10-27T23:15:00Z")
	turn("id-dev", "tn-dev", "dev-turn", "2026-10-27T23:10:00Z")
	s.Apply(env("seat_provider_parked", map[string]any{
		"agent_id": "id-lead", "provider_keys": []any{"default"},
		"retry_at": "2026-10-27T23:30:00Z", "resets_at": "2026-10-27T23:30:00Z",
		"harness": "claude-code", "subscription": true,
	}, at("2026-10-27T23:16:00Z"), id("park")))

	flows, live := e.flows(now)
	if live || flows["urgent"] != "moving/ since 2026-10-27T22:20:00Z" {
		t.Fatalf("before the records landed: urgent reads %s, flow_live %v; want it claimed "+
			"nothing (moving, flow_live false)", flows["urgent"], live)
	}

	// THE RECORDS LAND: every seat placed, nobody paused, a budget report
	// capping nothing, the dev's run working and the ops's run asking.
	s.SetPlacement(map[string]bool{"id-lead": true, "id-dev": true, "id-ops": true})
	s.SeedPauses(nil)
	s.Apply(env("budget_meters", map[string]any{"meter_id": "m-1", "seq": 1, "timezone": "UTC",
		"org": map[string]any{"windows": []any{}}}, streamOnly))
	run := func(turnID, agent, item string, status livestate.ExecutorStatus, started, paused string) livestate.ExecutorRecord {
		return livestate.ExecutorRecord{LaunchID: "l-" + turnID, WrittenAt: now, Entry: livestate.ExecutorEntry{
			TurnID: turnID, AgentID: agent, Status: status, StartedAt: started, PausedAt: paused,
			WorkItem: &types.WorkItem{Backend: types.WorkWorklet, ID: item, Key: "ENG-" + item, Project: "ENG"},
		}}
	}
	s.ReconcileExecutors([]livestate.ExecutorRecord{
		run("tn-run", "id-dev", "dev-run", livestate.ExecutorRunning, "2026-10-27T23:05:00Z", ""),
		run("tn-ask", "id-ops", "asked", livestate.ExecutorAwaiting, "2026-10-27T22:00:00Z",
			"2026-10-27T22:30:00Z"),
	}, now)

	flows, live = e.flows(now)
	if !live {
		t.Fatal("every record the posture reads landed, and the answer says flow_live false")
	}
	want := map[string]string{
		"urgent":    "held/limit since 2026-10-27T23:16:00Z until 2026-10-27T23:30:00Z",
		"busy":      "held/limit since 2026-10-27T23:16:00Z until 2026-10-27T23:30:00Z",
		"dev-turn":  "working/turn since 2026-10-27T23:10:00Z",
		"dev-run":   "working/run since 2026-10-27T23:05:00Z",
		"asked":     "waiting/parked since 2026-10-27T22:30:00Z",
		"ops-other": "held/parked_elsewhere since 2026-10-27T22:30:00Z",
	}
	for item, flow := range want {
		if flows[item] != flow {
			t.Errorf("%s reads %s, want %s", item, flows[item], flow)
		}
	}

	// A PERSON'S PAUSE OUTRANKS THE LIMIT, on the item as on the seat.
	s.Apply(env("seat_paused", map[string]any{"agent_id": "id-lead", "paused_by": "jane",
		"paused_by_kind": "human", "paused_at": "2026-10-27T23:18:00Z"},
		at("2026-10-27T23:18:00Z"), id("pause")))
	wantState(t, s, "id-lead", livestate.ActivityStopped, livestate.StoppedPaused)
	flows, _ = e.flows(now)
	if got := flows["urgent"]; got != "held/paused since 2026-10-27T23:18:00Z" {
		t.Errorf("urgent on a paused seat at its limit reads %s, want held/paused with no reset", got)
	}
}

// EVERY STOP REASON THE SEAT'S ROW CAN STATE IS ONE AN ITEM'S FLOW NAMES: a
// reason added to the ranking without a cause would hold a seat's items with
// no word for why.
func TestEveryStopReasonIsAFlowCause(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	for _, reason := range livestate.StoppedReasons {
		s := livestate.New(livestate.WithClock(func() time.Time { return now }))
		s.SetPlacement(map[string]bool{"id-a": reason != livestate.StoppedUnplaced})
		s.SeedPauses(nil)
		s.ReconcileExecutors(nil, now)
		org := map[string]any{"windows": []any{}}
		if reason == livestate.StoppedBudget {
			org = map[string]any{"windows": []any{dayWindow(1000, 1000, "2026-06-14T11:00:00Z")}}
		}
		s.Apply(env("budget_meters", map[string]any{"meter_id": "m-1", "seq": 1,
			"timezone": "UTC", "org": org}, streamOnly))
		switch reason {
		case livestate.StoppedPaused:
			s.Apply(env("seat_paused", map[string]any{"agent_id": "id-a",
				"paused_at": "2026-06-14T11:00:00Z"}, at("2026-06-14T11:00:00Z"), id("p")))
		case livestate.StoppedProvider:
			s.Apply(env("seat_provider_parked", map[string]any{"agent_id": "id-a",
				"provider_keys": []any{"default"}}, at("2026-06-14T11:00:00Z"), id("k")))
		}
		wantState(t, s, "id-a", livestate.ActivityStopped, reason)
		seat, held := s.Posture(map[string]string{"id-a": "a"}).Seats["a"]
		if !held || !seat.Stop.Valid() || seat.Stop == "" {
			t.Errorf("a seat stopped for %q reads posture %+v, want a flow cause naming it", reason, seat)
		}
	}
}
