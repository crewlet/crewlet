package observe_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/observe"
)

func TestEveryRegisteredTypeIsPlaced(t *testing.T) {
	t.Parallel()
	// THE MAP IS THE ADMISSION LIST, and a type missing from it is a
	// SILENT drop: no store row, and no activity-feed entry either,
	// because the projection keys "is this persisted" on the category
	// being non-empty. Nothing raises — the event is published, delivered
	// and discarded — which is how the sandbox panel came to show rows
	// that vanished on the next reload and 404'd when clicked.
	//
	// So a new event type has to be PLACED: given a category, or named in
	// the live-only list with the reason it is excluded. This is the test
	// that makes forgetting a build failure rather than a mystery.
	var unplaced []string
	for _, name := range events.RegisteredTypes() {
		if observe.Category(name) == "" && observe.Excluded(name) == "" {
			unplaced = append(unplaced, name)
		}
	}
	slices.Sort(unplaced)
	if len(unplaced) > 0 {
		t.Errorf("event types that are neither categorised nor deliberately "+
			"excluded:\n  %s\n\nEach is published and then silently discarded "+
			"— it reaches neither the event store nor the activity feed. Give it "+
			"a category in observe/categories.go, or add it to `excluded` with "+
			"the reason it must stay out.", strings.Join(unplaced, "\n  "))
	}
}

func TestEveryExclusionStatesItsReason(t *testing.T) {
	t.Parallel()
	// The reason is the whole value of the exclusion list: an exclusion and
	// an oversight look identical from outside — both are a type that is
	// published and then vanishes — and the reason is what tells them
	// apart. An empty one turns the list back into a set of shrugs.
	for _, name := range events.RegisteredTypes() {
		if observe.Category(name) != "" {
			continue
		}
		if reason := observe.Excluded(name); len(reason) < 40 {
			t.Errorf("%s is excluded with reason %q; say why it must stay out",
				name, reason)
		}
	}
}

func TestALiveOnlyTypeIsAlsoExcluded(t *testing.T) {
	t.Parallel()
	// liveOnly is a SUBSET of excluded — it names the exclusions that still
	// drive the projection. A type in one and not the other would be a
	// contradiction the two readers resolve differently.
	for _, name := range events.RegisteredTypes() {
		if observe.LiveOnly(name) && observe.Excluded(name) == "" {
			t.Errorf("%s is live-only but not excluded", name)
		}
		if observe.LiveOnly(name) && observe.Category(name) != "" {
			t.Errorf("%s is live-only and categorised; it would be persisted "+
				"after all", name)
		}
	}
}

func TestALiveOnlyTypeIsNeverPersisted(t *testing.T) {
	t.Parallel()
	// The two exclusions are not the same as an unknown type, and the
	// difference has to hold in both directions: a live-only type must
	// produce an envelope (it drives a seat's live row) and must NOT
	// produce a record (it would fill the log with intermediate states of
	// rows it also holds finished).
	ev := events.New(types.AgentTurnProgress{
		RoleName: "CEO", TurnID: "t1", Phase: types.PhaseExecute, RoundNum: 0,
	}, events.TraceContext{})

	if _, ok := observe.Record(ev); ok {
		t.Error("agent_turn_progress produced a store record; it is live-only")
	}
	env, ok := observe.Envelope(ev)
	if !ok {
		t.Fatal("agent_turn_progress produced no envelope, so no seat would ever " +
			"show as working mid-phase")
	}
	if env.Category != "" {
		t.Errorf("category = %q, want empty: a non-empty one makes the projection "+
			"mirror it into the activity buffer", env.Category)
	}
}

func TestAnUnknownTypeReachesNeitherConsumer(t *testing.T) {
	t.Parallel()
	ev := &events.Event{Type: "not_a_real_event", Timestamp: time.Now().UTC()}
	if _, ok := observe.Record(ev); ok {
		t.Error("an unplaced type was persisted")
	}
	if _, ok := observe.Envelope(ev); ok {
		t.Error("an unplaced type reached the projection")
	}
}

func TestARecordCarriesWhatAListingCanFilterOn(t *testing.T) {
	t.Parallel()
	// A listing NEVER selects the payload column, so a dimension that is
	// not a tag cannot be filtered on once the event is history. These are
	// the ones a dashboard actually asks for.
	ev := events.New(types.AgentPhaseCompleted{
		Agent: "a-1", RoleName: "CEO", TurnID: "t1",
		Phase: types.PhaseExecute, ConversationKey: "slack:C1",
		Failed: true, ErrorKind: "auth",
	}, events.TraceContext{TraceID: "tr", SpanID: "sp"})
	ev.Source = "CEO"

	rec, ok := observe.Record(ev)
	if !ok {
		t.Fatal("a phase completion was not persisted")
	}
	for key, want := range map[string]string{
		"agent_id":         "a-1",
		"agent_role":       "CEO",
		"conversation_key": "slack:C1",
		"failed":           "true",
	} {
		if got := rec.Tags[key]; got != want {
			t.Errorf("tag %q = %q, want %q", key, got, want)
		}
	}
	if rec.Category != "system" || rec.Actor != "CEO" || rec.TraceID != "tr" {
		t.Errorf("record = %+v", rec)
	}
	if rec.Summary == "" {
		t.Error("no summary: the feed would render a blank row")
	}
	// The whole event, not the body: a reader opening one row expects the
	// envelope's trace context beside the payload.
	var stored map[string]any
	if err := json.Unmarshal(rec.Payload, &stored); err != nil {
		t.Fatalf("stored payload does not decode: %v", err)
	}
	for _, key := range []string{"id", "type", "timestamp", "trace_id", "turn_id"} {
		if _, ok := stored[key]; !ok {
			t.Errorf("stored payload has no %q", key)
		}
	}
}

func TestAFailedTagIsOnlySetWhenItFailed(t *testing.T) {
	t.Parallel()
	// Set only when true, so the tag doubles as the filter. A "false"
	// written on every row would make the filter match everything.
	ev := events.New(types.AgentPhaseCompleted{
		RoleName: "CEO", TurnID: "t1", Phase: types.PhaseExecute,
	}, events.TraceContext{})
	rec, ok := observe.Record(ev)
	if !ok {
		t.Fatal("not persisted")
	}
	if _, present := rec.Tags["failed"]; present {
		t.Errorf("a successful phase carries a failed tag: %v", rec.Tags)
	}
}

func TestAnEnvelopeTimestampParsesAsTheProjectionReadsIt(t *testing.T) {
	t.Parallel()
	// The projection orders every state transition on this string, and
	// degrades to LEXICOGRAPHIC comparison for one it cannot parse. A
	// different spelling therefore still orders — wrongly, against the
	// ones that did parse — rather than failing, which is precisely the
	// kind of bug that never surfaces as an error.
	at := time.Date(2026, 8, 23, 10, 30, 0, 123456789, time.UTC)
	ev := events.New(types.AgentPhaseStarted{RoleName: "CEO"}, events.TraceContext{})
	ev.Timestamp = at

	env, ok := observe.Envelope(ev)
	if !ok {
		t.Fatal("no envelope")
	}
	got, err := time.Parse(time.RFC3339Nano, env.Timestamp)
	if err != nil {
		t.Fatalf("the projection's own parser cannot read %q: %v", env.Timestamp, err)
	}
	if !got.Equal(at) {
		t.Errorf("timestamp round-tripped to %v, want %v", got, at)
	}
}

func TestAnEventWithNoTimestampIsStillReadable(t *testing.T) {
	t.Parallel()
	// A zero time lands in year 1, permanently below every read floor: the
	// row exists and no query returns it. The store refuses it outright,
	// which would drop the event entirely — so it is stamped instead, one
	// write late rather than lost.
	ev := events.New(types.AgentPhaseStarted{RoleName: "CEO"}, events.TraceContext{})
	ev.Timestamp = time.Time{}

	rec, ok := observe.Record(ev)
	if !ok {
		t.Fatal("not persisted")
	}
	if rec.Time.IsZero() {
		t.Error("a zero timestamp reached the store, where the row is unreadable")
	}
	env, _ := observe.Envelope(ev)
	if env.Timestamp == "" {
		t.Error("a zero timestamp reached the projection as an empty string, " +
			"which skips its ordering guard entirely")
	}
}

func TestTheEnvelopePayloadIsFlatForTheClient(t *testing.T) {
	t.Parallel()
	// The wire contract says a payload is one flat object of the
	// envelope and the body together, and the projection's own accessors
	// read `role`, `agent_id` and `turn_id` straight off it. A nested
	// {"data": {...}} would make every one of them miss.
	ev := events.New(types.AgentPhaseStarted{
		Agent: "a-1", RoleName: "CEO", TurnID: "t1", Iteration: 2,
		Phase: types.PhaseExecute,
	}, events.TraceContext{})

	env, ok := observe.Envelope(ev)
	if !ok {
		t.Fatal("no envelope")
	}
	for key, want := range map[string]any{
		"role": "CEO", "agent_id": "a-1", "turn_id": "t1", "phase": "execute",
	} {
		if got := env.Payload[key]; got != want {
			t.Errorf("payload[%q] = %v, want %v", key, got, want)
		}
	}
}
