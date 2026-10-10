package livestate_test

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
)

// WHAT A MEMBER IS SERVED OF A SEAT IS ITS STATE, and every field of the member
// shapes is a decision recorded here with its reason (ADR-0031).

// memberField is one wire field of a member shape: why a member may read it.
// A field the machine processed — a prompt, a response, a tool call, a round,
// a token figure, the node running the turn — has no row here and can never
// be given one: [machineFields] refuses it by name.
var memberFields = map[string]map[string]string{
	"AgentStatus": {
		"handle":         "the seat's identity, which every screen links it by",
		"activity":       "the one seat-state word every screen draws",
		"stopped_reason": "why a stopped seat is stopped: the word a person acts on",
		"work_item":      "the item the seat is on — its key, id, project and tracker, which the tracker already shows a member",
		"step":           "where in its turn the seat is, in three words a person reads",
		"since":          "when the state began",
		"paused_by":      "the person who paused the seat, never a credential",
	},
	"SeatStatus": {
		"id":   "the address the roster links the seat by, which the chart already shows",
		"role": "the seat's name, which the chart already shows",
	},
	"BudgetStatus": {
		"timezone": "the clock every window is cut on, which the chart already publishes",
		"org":      "the company's windows that are refusing or near",
		"seats":    "the seats with a window refusing or near",
	},
	"SeatBudgetStatus": {
		"handle":  "the seat's identity",
		"windows": "its windows that are refusing or near",
	},
	"WindowStatus": {
		"period":    "which window: day, week or month",
		"window":    "the window's label on the company clock",
		"state":     "refusing or near, the judgement a person plans around",
		"resets_at": "when the window turns over and the seat can take work again",
	},
}

// machineFields is what a member is NEVER served of a seat, by its wire name:
// what the machine processed to reach its state, and how it is run.
var machineFields = []string{
	"live_call", "live_call_seq", "prompt", "prompt_messages", "response",
	"round_narration", "tool_executions", "running_call", "rounds", "versions",
	"node", "model", "provider_key", "current_phase", "current_iteration",
	"last_error", "last_turn", "turn", "paused", "budget", "runs", "agent_id",
	"input_tokens", "output_tokens", "total_tokens", "tokens", "used", "limit",
	"refused_at", "starts_at", "meter_id", "seq",
}

// memberShapes is every type a member is served of the live projection.
var memberShapes = map[string]reflect.Type{
	"AgentStatus":      reflect.TypeFor[livestate.AgentStatus](),
	"SeatStatus":       reflect.TypeFor[livestate.SeatStatus](),
	"BudgetStatus":     reflect.TypeFor[livestate.BudgetStatus](),
	"SeatBudgetStatus": reflect.TypeFor[livestate.SeatBudgetStatus](),
	"WindowStatus":     reflect.TypeFor[livestate.WindowStatus](),
}

// EVERY FIELD OF EVERY MEMBER SHAPE IS CLASSIFIED, and none is the machine's.
//
// A field added to a member shape fails here until somebody records why a
// member may read it — so the shape cannot grow a prompt the way the overlay
// grows a field, by default — and a field whose wire name is one the machine
// processed fails whatever reason it is given. A table entry for a field the
// shape no longer has fails too, so the table and the types cannot drift.
//
// Mutation: add `Node string \`json:"node"\“ to AgentStatus, and both halves
// fail.
func TestEveryAgentStatusFieldIsClassified(t *testing.T) {
	t.Parallel()
	for name, typ := range memberShapes {
		table := memberFields[name]
		if len(table) == 0 {
			t.Errorf("%s has no classification table", name)
			continue
		}
		own := ownWireFields(typ)
		for _, field := range own {
			why, ok := table[field]
			switch {
			case !ok:
				t.Errorf("livestate.%s carries %q, which nobody has classified: record why a "+
					"member may read it in memberFields, or leave it off the member shape", name, field)
			case why == "":
				t.Errorf("livestate.%s.%s is classified with no reason", name, field)
			}
			if slices.Contains(machineFields, field) {
				t.Errorf("livestate.%s carries %q, which is what the machine processed: a member "+
					"is never served it (ADR-0031)", name, field)
			}
		}
		for field := range table {
			if !slices.Contains(own, field) {
				t.Errorf("memberFields classifies %s.%s, which the type no longer carries", name, field)
			}
		}
	}
	for name := range memberFields {
		if _, ok := memberShapes[name]; !ok {
			t.Errorf("memberFields classifies %s, which is not a member shape", name)
		}
	}
}

// ownWireFields is a struct's JSON field names, an embedded struct's left to
// that struct's own table.
func ownWireFields(typ reflect.Type) []string {
	var out []string
	for i := range typ.NumField() {
		field := typ.Field(i)
		if field.Anonymous || !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		out = append(out, name)
	}
	return out
}

// busySeat is a projection holding one seat mid-turn on an item, with a live
// call whose prompt, response, tool call and model are all distinctive text a
// member's row must never carry.
func busySeat(t *testing.T) *livestate.LiveState {
	t.Helper()
	s := stoppedAt(t, "2026-06-14T12:30:00Z")
	item := map[string]any{"backend": "native", "id": "t-42", "key": "ENG-42", "project": "ENG"}
	s.Apply(env("agent_turn_started", map[string]any{
		"role": "Lead", "agent_handle": "lead", "turn_id": "tn-1", "work_item": item,
		"started_at": "2026-06-14T12:00:00Z", "node": "node-secret-7",
	}, at("2026-06-14T12:00:00Z"), id("ts")))
	s.Apply(env("agent_phase_started", map[string]any{
		"role": "Lead", "agent_handle": "lead", "turn_id": "tn-1", "phase": "execute",
		"work_item": item,
	}, at("2026-06-14T12:00:01Z"), id("ps")))
	s.Apply(env("agent_turn_progress", map[string]any{
		"role": "Lead", "agent_handle": "lead", "turn_id": "tn-1", "phase": "execute",
		"round_num": 0, "model": "model-secret-7", "prompt": "prompt-secret-7",
		"response":        "response-secret-7",
		"tool_executions": []any{map[string]any{"name": "tool-secret-7", "result": "result-secret-7"}},
		"total_tokens":    777,
	}, streamOnly, at("2026-06-14T12:00:02Z")))
	s.Apply(env("budget_meters", meterReport("m-1", 1, map[string]any{
		"role": "Lead", "agent_id": "a-1", "handle": "lead",
		"windows": []any{dayWindow(7770, 10000, "")},
	}), streamOnly))
	return s
}

// A MEMBER'S ROW IS THE SEAT'S STATE AND NOTHING THE MACHINE PROCESSED.
//
// The seat is mid-call with a prompt, a response, a tool call, a model, a node
// and a meter, and every one of those is distinctive text: the member's push
// row and its roster row carry exactly the classified keys — every one present,
// null where it has no value — with the state, the item and the step filled,
// and not one of those strings anywhere.
//
// Mutation: build Statuses from OverlayRows, and the key sets and the strings
// both fail.
func TestAMemberRowIsTheSeatsStateAndNothingItProcessed(t *testing.T) {
	t.Parallel()
	s := busySeat(t)

	pushed := s.Statuses([]string{"lead", "nobody"})
	if len(pushed) != 1 {
		t.Fatalf("statuses = %+v, want the one seat the projection holds", pushed)
	}
	roster := s.MergeStatuses([]map[string]any{{
		"id": "lead", "role": "Lead", "handle": "lead", "agent_id": "a-1", "kind": "agent",
	}})
	for surface, value := range map[string]any{"push": pushed, "roster": roster} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal the %s rows: %v", surface, err)
		}
		for _, secret := range []string{
			"prompt-secret-7", "response-secret-7", "tool-secret-7", "result-secret-7",
			"model-secret-7", "node-secret-7", "777", "7770", "a-1",
		} {
			if strings.Contains(string(raw), secret) {
				t.Errorf("the member's %s row carries %q: %s", surface, secret, raw)
			}
		}
		var rows []map[string]any
		if err := json.Unmarshal(raw, &rows); err != nil {
			t.Fatal(err)
		}
		want := slices.Sorted(maps.Keys(memberFields["AgentStatus"]))
		if surface == "roster" {
			want = slices.Sorted(slices.Values(append(want, "id", "role")))
		}
		if got := slices.Sorted(maps.Keys(rows[0])); !slices.Equal(got, want) {
			t.Errorf("the member's %s row carries %v, want exactly %v", surface, got, want)
		}
	}

	got := pushed[0]
	if got.Handle != "lead" || got.Activity != livestate.ActivityWorking || got.StoppedReason != nil {
		t.Errorf("status = %+v, want lead working", got)
	}
	if got.WorkItem == nil || got.WorkItem.Key != "ENG-42" || got.WorkItem.Project != "ENG" {
		t.Errorf("work item = %+v, want ENG-42 in ENG", got.WorkItem)
	}
	if got.Step == nil || *got.Step != livestate.StepExecute {
		t.Errorf("step = %v, want execute", got.Step)
	}
	if got.Since == nil || *got.Since != "2026-06-14T12:00:00Z" {
		t.Errorf("since = %v, want the turn's start", got.Since)
	}
	if roster[0].ID != "lead" || roster[0].Role != "Lead" || roster[0].Activity != livestate.ActivityWorking {
		t.Errorf("roster row = %+v, want the seat's identity and its state", roster[0])
	}
}

// A SEAT THE PROJECTION HAS NEVER HEARD OF STILL HAS A STATE on the roster, and
// every other field null — never a missing key, which a client merging rows
// reads as "unchanged".
func TestAQuietSeatsMemberRowIsItsStateAndNulls(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	rows := s.MergeStatuses([]map[string]any{{"id": "qa", "role": "QA", "handle": "qa"}})
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	const want = `[{"id":"qa","role":"QA","handle":"qa","activity":"idle","stopped_reason":null,` +
		`"work_item":null,"step":null,"since":null,"paused_by":null}]`
	if string(raw) != want {
		t.Errorf("a quiet seat's member row = %s, want %s", raw, want)
	}
}

// A MEMBER READS THE STEP AN ADMIN'S STEPPER DRAWS for the same seat: the rule
// of the dashboard's `turnSteps`, so the two screens never disagree.
//
// Mutation: read the stage before the call's phase, and the overlapping
// context-stage case fails; map onboarding to execute, and its case fails.
func TestAMembersStepIsTheStepAnAdminsStepperDraws(t *testing.T) {
	t.Parallel()
	started := func(s *livestate.LiveState) {
		s.Apply(env("agent_turn_started", map[string]any{
			"role": "Lead", "agent_handle": "lead", "turn_id": "tn-1",
		}, at("2026-06-14T12:00:00Z"), id("ts")))
	}
	phase := func(name string) func(*livestate.LiveState) {
		return func(s *livestate.LiveState) {
			started(s)
			s.Apply(env("agent_phase_started", map[string]any{
				"role": "Lead", "agent_handle": "lead", "turn_id": "tn-1", "phase": name,
			}, at("2026-06-14T12:00:01Z"), id("ps-"+name)))
		}
	}
	for _, tc := range []struct {
		name  string
		drive func(*livestate.LiveState)
		want  livestate.Step
	}{
		{"gathering context before any phase", started, livestate.StepContext},
		{"onboarding", phase("onboarding"), livestate.StepContext},
		{"executing", phase("execute"), livestate.StepExecute},
		{"reviewing", phase("review"), livestate.StepReview},
		{"parked on a coding run", func(s *livestate.LiveState) {
			phase("execute")(s)
			s.Apply(env("agent_turn_completed", map[string]any{
				"role": "Lead", "agent_handle": "lead", "turn_id": "tn-1", "suspended": true,
			}, at("2026-06-14T12:00:02Z"), id("tc")))
		}, livestate.StepExecute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := livestate.New()
			tc.drive(s)
			got := s.Statuses([]string{"lead"})
			if len(got) != 1 || got[0].Step == nil || *got[0].Step != tc.want {
				t.Fatalf("status = %+v, want step %s", got, tc.want)
			}
			if !got[0].Step.Valid() {
				t.Errorf("step %q is not a word of the vocabulary", *got[0].Step)
			}
		})
	}

	// And a seat on no turn is on no step.
	s := livestate.New()
	phase("execute")(s)
	s.Apply(env("agent_turn_completed", map[string]any{
		"role": "Lead", "agent_handle": "lead", "turn_id": "tn-1",
	}, at("2026-06-14T12:00:05Z"), id("tc")))
	if got := s.Statuses([]string{"lead"}); got[0].Step != nil || got[0].WorkItem != nil {
		t.Errorf("an idle seat = %+v, want no step and no item", got[0])
	}
}

// SINCE IS WHEN THE SEAT'S STATE BEGAN: the turn's start while it works, its
// last turn's end once it is idle, the pause's instant once it is paused.
func TestAMembersSinceIsWhenTheStateBegan(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.SetPlacement(map[string]bool{"lead": true})
	s.Apply(env("agent_turn_started", map[string]any{
		"role": "Lead", "agent_handle": "lead", "turn_id": "tn-1", "started_at": "2026-06-14T12:00:00Z",
	}, at("2026-06-14T12:00:00Z"), id("ts")))
	sinceOf := func() string {
		got := s.Statuses([]string{"lead"})[0]
		if got.Since == nil {
			return "<nil>"
		}
		return string(got.Activity) + " " + *got.Since
	}
	if got := sinceOf(); got != "working 2026-06-14T12:00:00Z" {
		t.Errorf("a working seat: %s, want working since the turn's start", got)
	}
	s.Apply(env("agent_turn_completed", map[string]any{
		"role": "Lead", "agent_handle": "lead", "turn_id": "tn-1",
	}, at("2026-06-14T12:04:00Z"), id("tc")))
	if got := sinceOf(); got != "idle 2026-06-14T12:04:00Z" {
		t.Errorf("an idle seat: %s, want idle since its last turn ended", got)
	}
	s.Apply(env("seat_paused", map[string]any{
		"role": "Lead", "agent_handle": "lead", "paused_by": "ops-token", "paused_at": "2026-06-14T12:09:00Z",
	}, at("2026-06-14T12:09:00Z"), id("p1")))
	if got := sinceOf(); got != "stopped 2026-06-14T12:09:00Z" {
		t.Errorf("a paused seat: %s, want stopped since the pause", got)
	}
}

// A MEMBER IS TOLD WHICH PERSON PAUSED A SEAT, AND NEVER A CREDENTIAL'S NAME: a
// person is part of the company, and a key's label is how the engine is run.
// The seed from the coordination record says the same as the event.
//
// Mutation: serve PausedBy whatever BySeat says, and the credential leaks.
func TestAMemberIsToldWhoPausedOnlyWhenAPersonDid(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("seat_paused", map[string]any{
		"role": "Lead", "agent_handle": "lead", "paused_by": "jane-token", "paused_by_seat": "jane",
		"paused_at": "2026-06-14T12:00:00Z",
	}, at("2026-06-14T12:00:00Z"), id("p1")))
	s.Apply(env("seat_paused", map[string]any{
		"role": "QA", "agent_handle": "qa", "paused_by": "ci-token", "paused_at": "2026-06-14T12:00:00Z",
	}, at("2026-06-14T12:00:00Z"), id("p2")))
	s.SeedPauses([]livestate.SeedPause{
		{Handle: "ops", Paused: livestate.Paused{By: "ops-token", At: "2026-06-01T09:00:00Z"}},
		{Handle: "eng", Paused: livestate.Paused{By: "omar", At: "2026-06-01T09:00:00Z", BySeat: true}},
	})
	got := map[string]string{}
	for _, row := range s.Statuses([]string{"eng", "lead", "ops", "qa"}) {
		got[row.Handle] = "<nil>"
		if row.PausedBy != nil {
			got[row.Handle] = *row.PausedBy
		}
	}
	want := map[string]string{"lead": "jane", "eng": "omar", "qa": "<nil>", "ops": "<nil>"}
	if !maps.Equal(got, want) {
		t.Errorf("paused by = %v, want %v", got, want)
	}
}

// A MEMBER'S METER NAMES WHAT IS PRESSING AND WHEN IT TURNS OVER, AND NO FIGURE.
//
// The company's day is refusing and one seat's is near; a seat comfortably
// inside its ceiling is not listed, nor is a window that already turned over
// whatever its last report said. Nothing the meter spent or is capped at
// reaches the member, and before any report there is no status at all —
// distinct from one listing nothing.
//
// Mutation: drop the turned-over check, and the stale seat is listed.
func TestAMembersBudgetNamesWhatIsPressingAndNoFigure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 14, 12, 30, 0, 0, time.UTC)
	s := livestate.New(livestate.WithClock(func() time.Time { return now }))
	if s.BudgetStatus() != nil {
		t.Fatal("a projection that has had no report states a budget status")
	}

	near := dayWindow(8100, 9000, "")
	near["state"] = "near"
	stale := dayWindow(6000, 6000, "2026-06-13T20:00:00Z")
	stale["window"], stale["resets_at"] = "2026-06-13", "2026-06-14T00:00:00Z"
	report := meterReport("m-1", 1,
		map[string]any{"role": "Lead", "agent_id": "a-1", "handle": "lead", "windows": []any{near}},
		map[string]any{"role": "QA", "agent_id": "a-2", "handle": "qa", "windows": []any{dayWindow(10, 9000, "")}},
		map[string]any{"role": "Ops", "agent_id": "a-3", "handle": "ops", "windows": []any{stale}},
	)
	report["org"] = map[string]any{"windows": []any{dayWindow(10000, 10000, "2026-06-14T12:10:00Z")}}
	s.Apply(env("budget_meters", report, streamOnly))

	got := s.BudgetStatus()
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"timezone":"UTC",` +
		`"org":[{"period":"day","window":"2026-06-14","state":"refusing","resets_at":"2026-06-15T00:00:00Z"}],` +
		`"seats":[{"handle":"lead","windows":[{"period":"day","window":"2026-06-14","state":"near",` +
		`"resets_at":"2026-06-15T00:00:00Z"}]}]}`
	if string(raw) != want {
		t.Errorf("a member's budget = %s\nwant %s", raw, want)
	}
}
