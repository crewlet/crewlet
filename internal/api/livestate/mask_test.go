package livestate

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
)

// class is what a live field is to a reader.
type class string

const (
	// status is everybody's: what a seat is DOING.
	status class = "status"
	// body is the seat's trail: what it is SAYING. Only its readers' —
	// an admin and the seat's lead.
	body class = "body"
	// tracker is the work item a seat is on: the tracker's.
	tracker class = "tracker"
	// config is the company's configuration: an admin's ([Reach.Config]).
	config class = "config"
	// delegated is masked by another root's row, which the walk checks
	// the field's type against rather than recursing into it again.
	delegated class = "delegated"
)

// callFamily classifies every field a live call puts on the wire, in all three
// of its shapes — the projection's own [LiveCall], the push's [leanCall] and
// the withheld [withheldCall] — keyed on the JSON name the three share.
var callFamily = map[string]class{
	"turn_id": status, "work_key": status, "phase": status, "iteration": status,
	"model": status, "input_tokens": status, "output_tokens": status,
	"total_tokens": status, "round_num": status, "rounds_used": status,
	"rounds": status, "max_rounds": status, "round_ceiling": status,
	"round_started_at": status, "run_started_at": status, "steers": status, "cache_read_tokens": status,
	"cache_write_tokens": status, "node": status, "in_progress": status,
	"failed": status, "error": status, "updated_at": status, "started_at": status,
	"versions": status, "body_withheld": status,
	// A CLI RUN IS STATUS: when it ran, on whose entry, how it ended and
	// what it spent are what the seat is doing, like a round's timing.
	"invocations": status,
	// THE RUNNING CALLS ARE STATUS WITHOUT THEIR ARGUMENTS: which tools are
	// running and since when is what the seat is doing; what each was asked
	// is the body, and [MaskCall] drops that one key from a fresh map.
	"running_calls": status,

	"trigger": body, "prompt": body, "prompt_messages": body, "response": body,
	"tool_executions": body, "round_narration": body, "partial_round": body,

	"work_item": tracker,
}

// overlayFamily classifies an [Overlay] and the [overlayView] a reader is sent.
var overlayFamily = map[string]class{
	"activity": status, "stopped_reason": status, "current_phase": status,
	"current_iteration": status, "live_call_seq": status, "last_error": status,
	"budget": status, "last_turn": status, "paused": status, "provider_limit": status,
	"live_call": delegated, "turn": delegated,
}

// turnFields classifies a [LiveTurn]. The basis is the RULE that charged the
// turn, not the item.
var turnFields = map[string]class{
	"turn_id": status, "work_item_basis": status, "started_at": status,
	"stage": status, "node": status,
	"work_item": tracker,
}

// sandboxFields classifies a [SandboxEntry]. The question is the body too, but
// shown to the people it is put to as well — [SandboxEntry.For]. The entry a
// run was launched on is the company's configuration; the model it runs under
// is what it is doing.
var sandboxFields = map[string]class{
	"turn_id": status, "role": status, "agent_handle": status, "agent_id": status,
	"coding_agent": status, "provider_key": config, "model": status,
	"sandbox_id": status, "status": status,
	"started_at": status, "audience": status, "owner": status, "paused_at": status,
	"body_withheld": status,
	"task":          body, "question": body,
	"work_item": tracker,
}

// nested classifies every struct a STATUS field reaches, field by field, so a
// field added to one of them fails the walk until somebody decides what it is.
// decisions §4's body list is exhaustive and names none of these: an error's
// message and a pause's reason are what a reader is told about the seat.
var nested = map[reflect.Type]map[string]class{
	reflect.TypeFor[ErrorInfo](): {"kind": status, "detail": config,
		"phase": status, "turn_id": status, "at": status, "event_id": status},
	reflect.TypeFor[CallVersions](): {"prompt": status, "response": status,
		"narration": status, "executions": status, "rounds": status,
		"invocations": status},
	reflect.TypeFor[BudgetMeter](): {"windows": status},
	reflect.TypeFor[WindowMeter](): {"period": status, "window": status,
		"starts_at": status, "resets_at": status, "used": status, "limit": status,
		"refused_at": status, "state": status},
	reflect.TypeFor[LastTurn](): {"turn_id": status, "ended_at": status,
		"outcome": status},
	reflect.TypeFor[Paused](): {"by": status, "by_kind": status,
		"operator_id": status, "at": status, "reason": status, "stop_running": status},
	reflect.TypeFor[ProviderLimit](): {"parked": status, "resets_at": status,
		"since": status, "cli": status, "subscription": status},
}

// jsonField is one field a type puts on the wire.
type jsonField struct {
	name string
	typ  reflect.Type
}

// wireFields is every field t's JSON encoding carries, by encoding/json's own
// rules: an embedded struct's fields promoted, a shallower field shadowing a
// deeper one of the same name, a tagged field winning a tie at one depth (an
// untagged tie dropping both), and `-` and unexported fields skipped.
func wireFields(t reflect.Type) []jsonField {
	type candidate struct {
		jsonField
		depth  int
		tagged bool
	}
	type level struct {
		typ   reflect.Type
		depth int
	}
	var found []candidate
	visited := map[reflect.Type]bool{}
	next := []level{{t, 0}}
	for len(next) > 0 {
		current := next
		next = nil
		for _, l := range current {
			if visited[l.typ] {
				continue
			}
			visited[l.typ] = true
			for i := range l.typ.NumField() {
				sf := l.typ.Field(i)
				tag := sf.Tag.Get("json")
				if tag == "-" {
					continue
				}
				name, _, _ := strings.Cut(tag, ",")
				if sf.Anonymous && name == "" {
					inner := sf.Type
					if inner.Kind() == reflect.Pointer {
						inner = inner.Elem()
					}
					if inner.Kind() == reflect.Struct {
						next = append(next, level{inner, l.depth + 1})
						continue
					}
				}
				if !sf.IsExported() {
					continue
				}
				if name == "" {
					name = sf.Name
				}
				found = append(found, candidate{jsonField{name, sf.Type}, l.depth, name != sf.Name || tag != ""})
			}
		}
	}
	byName := map[string][]candidate{}
	for _, c := range found {
		byName[c.name] = append(byName[c.name], c)
	}
	var out []jsonField
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		all := byName[name]
		shallowest := slices.MinFunc(all, func(a, b candidate) int { return a.depth - b.depth }).depth
		var top, tagged []candidate
		for _, c := range all {
			if c.depth == shallowest {
				top = append(top, c)
				if c.tagged {
					tagged = append(tagged, c)
				}
			}
		}
		switch {
		case len(top) == 1:
			out = append(out, top[0].jsonField)
		case len(tagged) == 1:
			out = append(out, tagged[0].jsonField)
		}
	}
	return out
}

// workItem is the tracker's type, which is TRACKER wherever it appears.
var workItem = reflect.TypeFor[types.WorkItem]()

// structOf is the struct a field's type holds, through pointers, slices and
// arrays, or nil for a value that holds none — `any`, a map, a scalar.
func structOf(t reflect.Type) reflect.Type {
	for {
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			t = t.Elem()
		case reflect.Struct:
			return t
		default:
			return nil
		}
	}
}

// EVERY FIELD A SEAT'S LIVE STATE PUTS ON THE WIRE IS CLASSIFIED — status,
// body, tracker or delegated — in both directions, all the way down.
//
// The split between what a seat is DOING (everybody's) and what it is SAYING
// (its trail's readers') is declared once, here and in [MaskCall], over the
// seven shapes the live state reaches a reader in. A field somebody adds to any
// of them — or to a struct a status field carries — is a field this walk has
// no answer for, and it fails until somebody decides: left unclassified, a new
// transcript field would ride the overview to every reader. And a table entry
// naming no field is a field that was renamed out from under its rule.
//
// A WORK ITEM IS THE TRACKER'S WHEREVER IT APPEARS, as a rule rather than an
// entry: a seat's current task reaches only a reader who uses Worklet.
//
// Mutation: add an unclassified field to [LiveCall], or to [Paused], and this
// fails; classify `work_item` as status and the rule fails.
func TestEveryLiveFieldIsClassified(t *testing.T) {
	t.Parallel()
	families := map[string]map[string]class{
		"the call family": callFamily, "the overlay family": overlayFamily,
		"the live turn": turnFields, "the coding run": sandboxFields,
	}
	roots := []struct {
		typ    reflect.Type
		family string
	}{
		{reflect.TypeFor[LiveCall](), "the call family"},
		{reflect.TypeFor[leanCall](), "the call family"},
		{reflect.TypeFor[withheldCall](), "the call family"},
		{reflect.TypeFor[Overlay](), "the overlay family"},
		{reflect.TypeFor[overlayView](), "the overlay family"},
		{reflect.TypeFor[LiveTurn](), "the live turn"},
		{reflect.TypeFor[SandboxEntry](), "the coding run"},
	}
	delegatedTo := map[string][]reflect.Type{
		"live_call": {reflect.TypeFor[*LiveCall](), reflect.TypeFor[any]()},
		"turn":      {reflect.TypeFor[*LiveTurn]()},
	}
	reached := map[string]map[string]bool{}
	reachedNested := map[reflect.Type]map[string]bool{}
	var walk func(owner string, fields []jsonField, table map[string]class,
		mark map[string]bool)
	walk = func(owner string, fields []jsonField, table map[string]class,
		mark map[string]bool) {
		for _, f := range fields {
			c, ok := table[f.name]
			if !ok {
				t.Errorf("%s puts %q on the wire and nothing classifies it: is "+
					"it what the seat is doing (status), what it is saying "+
					"(body), or its work item (tracker)?", owner, f.name)
				continue
			}
			mark[f.name] = true
			inner := structOf(f.typ)
			if inner == workItem && c != tracker {
				t.Errorf("%s.%s is a work item classified %s: a work item is "+
					"the tracker's wherever it appears", owner, f.name, c)
			}
			switch c {
			case delegated:
				if !slices.Contains(delegatedTo[f.name], f.typ) {
					t.Errorf("%s.%s is delegated and is a %s, which no root "+
						"masks", owner, f.name, f.typ)
				}
				continue
			case body, tracker, config:
				continue
			}
			if inner == nil {
				continue
			}
			sub, ok := nested[inner]
			if !ok {
				t.Errorf("%s.%s is status and carries %s, whose fields nothing "+
					"classifies", owner, f.name, inner)
				continue
			}
			if reachedNested[inner] == nil {
				reachedNested[inner] = map[string]bool{}
			}
			walk(inner.String(), wireFields(inner), sub, reachedNested[inner])
		}
	}
	for _, root := range roots {
		if reached[root.family] == nil {
			reached[root.family] = map[string]bool{}
		}
		walk(root.typ.String(), wireFields(root.typ), families[root.family],
			reached[root.family])
	}
	for _, name := range slices.Sorted(maps.Keys(families)) {
		for _, field := range slices.Sorted(maps.Keys(families[name])) {
			if !reached[name][field] {
				t.Errorf("%s classifies %q, which no root puts on the wire — "+
					"a renamed field leaves exactly this behind", name, field)
			}
		}
	}
	for typ, table := range nested {
		for _, field := range slices.Sorted(maps.Keys(table)) {
			if !reachedNested[typ][field] {
				t.Errorf("%s classifies %q, which no status field reaches", typ, field)
			}
		}
	}
	if len(reachedNested) != len(nested) {
		t.Errorf("the walk reached %d of the %d nested types, so it is not "+
			"following the status fields", len(reachedNested), len(nested))
	}
}

// fullCall is a live call with every field set, its body included.
func fullCall() *LiveCall {
	return &LiveCall{
		TurnID: "turn-1", WorkKey: "wk-1", Phase: "execute", Model: "claude",
		Trigger: map[string]any{"summary": "please fix it"},
		Prompt:  "fix the flaky test", PromptMessages: []any{"system", "user"},
		Response: "reading", ToolExecutions: []any{map[string]any{"name": "read"}},
		RoundNarration: []any{map[string]any{"round": 1}},
		PartialRound:   map[string]any{"round": 2},
		Rounds:         []any{map[string]any{"round": 1}},
		Invocations: []any{map[string]any{"first_round": 1, "last_round": 1,
			"outcome": "stopped"}},
		// Two calls of one response running side by side, as a CLI runs
		// them.
		RunningCalls: []any{
			map[string]any{"id": "call-1", "round": 2, "name": "read_file",
				"arguments": `{"path":"x"}`, "started_at": "now"},
			map[string]any{"id": "call-2", "round": 2, "name": "read_file",
				"arguments": `{"path":"y"}`, "started_at": "now"},
		},
		WorkItem:   &types.WorkItem{Backend: "native", ID: "t-1", Key: "ENG-1", Project: "ENG"},
		InProgress: true,
		Versions: CallVersions{Prompt: 1, Response: 2, Narration: 3, Executions: 4, Rounds: 5,
			Invocations: 6},
	}
}

// wire is v as a reader receives it.
func wire(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// noBody asserts a masked call carries the seat's status and none of its body.
func noBody(t *testing.T, who string, got map[string]any) {
	t.Helper()
	if got == nil || got["turn_id"] != "turn-1" {
		t.Fatalf("%s: the call's status did not survive the mask: %v", who, got)
	}
	for field, c := range callFamily {
		if _, ok := got[field]; ok && c == body {
			t.Errorf("%s carries the body field %q", who, field)
		}
	}
	running, _ := got["running_calls"].([]any)
	if len(running) != 2 {
		t.Errorf("%s lists %d calls in flight, want both: %v", who, len(running), got["running_calls"])
	}
	for i, entry := range running {
		call, _ := entry.(map[string]any)
		if _, ok := call[runningArguments]; ok {
			t.Errorf("%s carries running call %d's arguments", who, i)
		}
		if call["name"] != "read_file" || call["id"] == nil {
			t.Errorf("%s lost which call is running: %v", who, call)
		}
	}
	versions, _ := got["versions"].(map[string]any)
	for _, v := range []string{"prompt", "response", "narration", "executions"} {
		if n, _ := versions[v].(float64); n != 0 {
			t.Errorf("%s names the %s version %v", who, v, n)
		}
	}
	if masked, _ := got["body_withheld"].(bool); !masked {
		t.Errorf("%s does not say its body was withheld", who)
	}
}

// A MASKED CALL CARRIES NO BODY AND NO BODY VERSION, and keeps its status.
//
// What a seat is doing reaches a reader the seat's trail is not open to: its
// phase, its round, which tool is running. What it is saying — prompt, answer,
// narration, the tool calls and the running one's arguments, what woke it —
// does not, and neither does any version of it: the dashboard asks for a call
// whole when a push names a newer version than it holds, so a version that
// moved would send the reader round for a body it will never be sent. The
// rounds' and the runs' versions stay: both are status. And the work item goes
// with the tracker.
//
// Mutation: keep a body field, a running call's arguments or a body version,
// or drop the runs, and this fails.
func TestAMaskedCallCarriesNoBodyAndZeroBodyVersions(t *testing.T) {
	t.Parallel()
	got := wire(t, MaskCall(fullCall(), Reach{Tracker: true}))
	noBody(t, "a status reader's call", got)
	if _, ok := got["work_item"]; !ok {
		t.Error("a tracker holder's call lost its work item")
	}
	for detail, want := range map[string]float64{"rounds": 5, "invocations": 6} {
		if _, ok := got[detail]; !ok {
			t.Errorf("the masked call lost its %s, which are status", detail)
		}
		if n, _ := got["versions"].(map[string]any)[detail].(float64); n != want {
			t.Errorf("the %s' version is %v, want %v: they are status", detail, n, want)
		}
	}
	if _, ok := wire(t, MaskCall(fullCall(), Reach{}))["work_item"]; ok {
		t.Error("a reader without Worklet was sent the work item")
	}
	whole := wire(t, MaskCall(fullCall(), Reach{Body: true, Tracker: true}))
	if whole["prompt"] != "fix the flaky test" || whole["body_withheld"] != nil {
		t.Errorf("a reader of the seat's trail was not sent the call whole: %v", whole)
	}
}

// MaskRow AND MaskCall HANDLE EVERY SHAPE A CALL TRAVELS IN.
//
// A row's call is the projection's own [*LiveCall] on a snapshot and a
// lean call once a push has left fields out ([LiveCall.Without]) — with its
// rounds carried or held — and null between calls, a typed nil included. A
// status reader gets no body from any of them and a reader without Worklet
// no work item; a reader of the trail gets each exactly as the push carried it.
//
// Mutation: make MaskCall handle only the projection's own shape, and the lean
// rows lose their call or keep their body.
func TestMaskRowHandlesEveryShape(t *testing.T) {
	t.Parallel()
	call := fullCall()
	held := call.Versions
	held.Rounds--
	held.Invocations--
	for _, tc := range []struct {
		name string
		call any
	}{
		{"the projection's own call", call},
		{"a lean call carrying its rounds and runs", call.Without(held)},
		{"a lean call holding everything", call.Without(call.Versions)},
	} {
		row := map[string]any{"agent_id": "agent-1", "live_call": tc.call,
			"turn": &LiveTurn{TurnID: "turn-1", WorkItem: call.WorkItem}}
		status := wire(t, MaskRow(row, Reach{}))
		masked, _ := status["live_call"].(map[string]any)
		noBody(t, tc.name, masked)
		if _, ok := masked["work_item"]; ok {
			t.Errorf("%s: a reader without Worklet was sent the call's work item", tc.name)
		}
		turn, _ := status["turn"].(map[string]any)
		if _, ok := turn["work_item"]; ok || turn == nil {
			t.Errorf("%s: a reader without Worklet was sent the turn %v, want "+
				"it without its work item", tc.name, turn)
		}
		if got := wire(t, MaskCall(tc.call, Reach{}))["turn_id"]; got != "turn-1" {
			t.Errorf("%s: MaskCall alone lost the call: %v", tc.name, got)
		}
		whole := wire(t, MaskRow(row, Reach{Body: true, Tracker: true}))
		if !reflect.DeepEqual(whole["live_call"], wire(t, tc.call)) {
			t.Errorf("%s: a reader of the trail was not sent the call as the push "+
				"carried it:\n got %v\nwant %v", tc.name, whole["live_call"], wire(t, tc.call))
		}
	}
	for _, detail := range []string{"rounds", "invocations"} {
		if _, ok := wire(t, MaskCall(call.Without(call.Versions), Reach{}))[detail]; ok {
			t.Errorf("a lean call that held its %s was sent them masked", detail)
		}
		if _, ok := wire(t, MaskCall(call.Without(held), Reach{}))[detail]; !ok {
			t.Errorf("a lean call that carried its %s lost them masked", detail)
		}
	}
	for name, none := range map[string]any{"nil": nil, "a typed nil": (*LiveCall)(nil)} {
		row := MaskRow(map[string]any{"agent_id": "agent-1", "live_call": none}, Reach{})
		if raw, _ := json.Marshal(row["live_call"]); string(raw) != "null" {
			t.Errorf("%s call was masked to %s, want null", name, raw)
		}
	}
}

// A MASK NEVER WRITES THROUGH TO THE PROJECTION.
//
// One clone of a seat's live state is shared by every audience variant of a
// broadcast, so a mask that edited what it was handed would hand the next
// variant — the seat's lead — a call already stripped, or a map already
// missing its arguments. Masking a row, an overlay and a call for a status
// reach leaves each byte-identical.
//
// Mutation: delete the arguments from the running call in place, or clear
// the work item on the input, and this fails.
func TestAMaskNeverWritesThroughToTheProjection(t *testing.T) {
	t.Parallel()
	encode := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	call := fullCall()
	held := call.Versions
	held.Response--
	row := map[string]any{"agent_id": "agent-1", "live_call": call.Without(held),
		"turn": &LiveTurn{TurnID: "turn-1", WorkItem: call.WorkItem}}
	overlay := &Overlay{LiveCall: fullCall(), Turn: &LiveTurn{TurnID: "turn-1",
		WorkItem: call.WorkItem}}
	before := []string{encode(call), encode(row), encode(overlay)}
	_ = MaskCall(call, Reach{})
	_ = MaskRow(row, Reach{})
	_ = overlay.For(Reach{})
	_ = MaskRow(row, Reach{Body: true})
	after := []string{encode(call), encode(row), encode(overlay)}
	for i, name := range []string{"the call", "the row", "the overlay"} {
		if before[i] != after[i] {
			t.Errorf("masking %s wrote through to it:\nbefore %s\n after %s",
				name, before[i], after[i])
		}
	}
}

// AN OVERLAY VIEW STATES ITS LIVE CALL AND ITS TURN ALWAYS.
//
// A client merges an overlay onto the row it holds, so an omitted key reads as
// "unchanged": a seat between calls masked to a view without `live_call` would
// keep the call it had on screen. Masked, a nil call and a nil turn are still
// sent as null.
//
// Mutation: give the view's shadows omitempty, and this fails.
func TestAnOverlayViewStatesLiveCallAndTurnAlways(t *testing.T) {
	t.Parallel()
	got := wire(t, (&Overlay{}).For(Reach{}))
	for _, key := range []string{"live_call", "turn"} {
		v, ok := got[key]
		if !ok || v != nil {
			t.Errorf("a masked overlay with no %s sent %v (present %v), want null",
				key, v, ok)
		}
	}
	masked := wire(t, (&Overlay{LiveCall: fullCall()}).For(Reach{}))
	call, _ := masked["live_call"].(map[string]any)
	noBody(t, "a masked overlay's call", call)
}

// A CODING RUN SHOWS ITS QUESTION TO THE PEOPLE IT IS PUT TO.
//
// What the run was asked and what it asked back are its seat's trail, and a
// reader the trail is not open to is told the run's status and that the rest
// was withheld. But a parked question is put to people — its audience — who
// are asked it whatever their reach to the seat that asked, and could not
// answer what they were not shown. The work item is the tracker's.
//
// Mutation: withhold the question from its audience, or show the task to
// them, and this fails.
func TestACodingRunShowsItsQuestionToThePeopleItIsPutTo(t *testing.T) {
	t.Parallel()
	entry := SandboxEntry{TurnID: "turn-1", Task: "fix the build", Status: SandboxAwaiting,
		Question: "which branch?", AudienceHandles: []string{"dana"},
		WorkItem: &types.WorkItem{Key: "ENG-1"}}
	for _, tc := range []struct {
		name          string
		reach         Reach
		task, ask     bool
		withheld, wid bool
	}{
		{"a reader of the trail", Reach{Body: true, Tracker: true}, true, true, false, true},
		{"the person it is put to", Reach{Record: "dana"}, false, true, true, false},
		{"anybody else", Reach{Record: "sam", Tracker: true}, false, false, true, true},
	} {
		got := entry.For(tc.reach)
		if (got.Task != "") != tc.task || (got.Question != "") != tc.ask ||
			got.BodyWithheld != tc.withheld || (got.WorkItem != nil) != tc.wid {
			t.Errorf("%s: task %q, question %q, withheld %v, work item %v", tc.name,
				got.Task, got.Question, got.BodyWithheld, got.WorkItem)
		}
	}
	if entry.Task != "fix the build" || entry.Question != "which branch?" ||
		entry.WorkItem == nil {
		t.Error("masking a coding run wrote through to the entry it was handed")
	}
	if strings.Contains(string(mustJSON(t, entry)), "dana") {
		t.Error("a coding run puts the people its question is put to on the wire")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
