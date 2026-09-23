package store_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
)

// buildEvent assembles an event from raw JSON, which is how one arrives off
// the queue. Going through the envelope's own decoder rather than a struct
// literal is deliberate: it is the path a real event takes, and it is the only
// way to produce the case that matters most below — a type this build has
// never heard of.
func buildEvent(t *testing.T, body string) *events.Event {
	t.Helper()
	var ev events.Event
	if err := json.Unmarshal([]byte(body), &ev); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	return &ev
}

func TestRecordForPromotesTags(t *testing.T) {
	t.Parallel()
	ev := buildEvent(t, `{
		"id": "6f1a2b3c-0000-4000-8000-000000000001",
		"type": "task_assigned",
		"timestamp": "2026-04-01T12:00:00Z",
		"source": "engine",
		"trace_id": "tr-1",
		"agent_id": "agent-9",
		"role": "engineer",
		"task_id": "task-4",
		"sender": "alice",
		"conversation_key": "slack:C1/T1",
		"failed": false
	}`)

	rec, tracked, err := store.RecordFor(ev)
	if err != nil {
		t.Fatalf("RecordFor: %v", err)
	}
	if !tracked {
		t.Fatal("agent_phase_started must be stored")
	}
	want := map[string]string{
		"agent_id":         "agent-9",
		"agent_role":       "engineer",
		"task_id":          "task-4",
		"sender":           "alice",
		"conversation_key": "slack:C1/T1",
	}
	for k, v := range want {
		if rec.Tags[k] != v {
			t.Errorf("tag %s = %q, want %q", k, rec.Tags[k], v)
		}
	}
	// Only set when true, so the tag doubles as a filter for failures.
	if _, present := rec.Tags["failed"]; present {
		t.Errorf("failed tag stamped on a successful event: %v", rec.Tags)
	}
	if rec.Category != "task" {
		t.Errorf("category %q, want task", rec.Category)
	}
	if !rec.Time.Equal(time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("time %v", rec.Time)
	}
}

func TestRecordForStampsFailure(t *testing.T) {
	t.Parallel()
	ev := buildEvent(t, `{
		"id": "6f1a2b3c-0000-4000-8000-000000000002",
		"type": "agent_phase_completed",
		"timestamp": "2026-04-01T12:00:00Z",
		"source": "engine",
		"failed": true
	}`)
	rec, _, err := store.RecordFor(ev)
	if err != nil {
		t.Fatalf("RecordFor: %v", err)
	}
	if rec.Tags["failed"] != "true" {
		t.Fatalf("failed tag = %q; a listing never selects the payload, so this "+
			"is the only thing that survives into history", rec.Tags["failed"])
	}
}

// TestRecordForReadsUnknownTypes is why these dimensions are read off the
// envelope rather than off a decoded payload: a reader that needs the concrete
// type sees nothing at all on a type it does not know. A rolling upgrade publishes types the older
// half has never heard of; those events must still be indexed by the agent
// they concern.
func TestRecordForReadsUnknownTypes(t *testing.T) {
	t.Parallel()
	ev := buildEvent(t, `{
		"id": "6f1a2b3c-0000-4000-8000-000000000003",
		"type": "task_assigned",
		"timestamp": "2026-04-01T12:00:00Z",
		"source": "engine",
		"role": "from-the-future",
		"a2a_context": {"channel_id": "chan-7"},
		"some_field_this_build_has_never_seen": 42
	}`)
	rec, tracked, err := store.RecordFor(ev)
	if err != nil {
		t.Fatalf("RecordFor: %v", err)
	}
	if !tracked {
		t.Fatal("tracked type not recognised")
	}
	if rec.Tags["agent_role"] != "from-the-future" {
		t.Errorf("agent_role = %q", rec.Tags["agent_role"])
	}
	if rec.Tags["a2a_channel_id"] != "chan-7" {
		t.Errorf("a2a_channel_id = %q", rec.Tags["a2a_channel_id"])
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload["some_field_this_build_has_never_seen"] != float64(42) {
		t.Errorf("unknown field dropped on the way to storage: %v", payload)
	}
}

func TestRecordForSkipsUntracked(t *testing.T) {
	t.Parallel()
	ev := buildEvent(t, `{
		"id": "6f1a2b3c-0000-4000-8000-000000000004",
		"type": "agent_turn_progress",
		"timestamp": "2026-04-01T12:00:00Z",
		"source": "engine"
	}`)
	_, tracked, err := store.RecordFor(ev)
	if err != nil {
		t.Fatalf("RecordFor: %v", err)
	}
	if tracked {
		t.Fatal("agent_turn_progress is a live-only signal; agent_phase_completed is its durable record")
	}

	if _, _, err := store.RecordFor(nil); err != nil {
		t.Fatalf("a nil event must be a no-op, not an error: %v", err)
	}
}

// TestCategoriesAreKnownValues guards the map against a typo that would file
// events under a category no dashboard filter offers — the row would be
// stored and unreachable.
func TestCategoriesAreKnownValues(t *testing.T) {
	t.Parallel()
	known := map[string]bool{
		"lifecycle": true, "task": true, "a2a": true,
		"decision": true, "notification": true,
		"system": true, "learning": true,
	}
	for _, typ := range []string{
		"org_started", "task_assigned", "a2a_channel_opened",
		"decision_requested", "external_notification",
		"budget_exhausted", "skill_synthesized", "sandbox_run_started",
		"config_revision_activated",
	} {
		cat, ok := store.Category(typ)
		if !ok {
			t.Errorf("%s is not stored", typ)
			continue
		}
		if !known[cat] {
			t.Errorf("%s -> unknown category %q", typ, cat)
		}
	}
}

// THE THIRD-PARTY APP A NOTIFICATION EVENT CONCERNS IS A TAG, because a
// listing deliberately never selects the payload column, so the Integrations
// room aggregating "how many of this third-party app's deliveries were dropped
// by the routing gate" has no other way to read it.
func TestRecordForTagsTheNotificationSource(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{
		"notification_skipped", "notifications_coalesced", "external_notification",
	} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			ev := buildEvent(t, `{
				"id": "6f1a2b3c-0000-4000-8000-000000000009",
				"type": "`+kind+`",
				"timestamp": "2026-04-01T12:00:00Z",
				"source": "engine",
				"notification_source": "gitlab"
			}`)
			rec, tracked, err := store.RecordFor(ev)
			if err != nil {
				t.Fatalf("RecordFor: %v", err)
			}
			if !tracked {
				t.Fatalf("%s must be stored", kind)
			}
			if got := rec.Tags["notification_source"]; got != "gitlab" {
				t.Errorf("notification_source tag = %q, want gitlab (tags: %v)",
					got, rec.Tags)
			}
		})
	}
}

// A PART OF A PHASE RECORD IS STORED, AND AS NOTHING A LISTING CAN REACH.
//
// It is kept because the record it belongs to is incomplete without it, and
// it is storage rather than an event: its identity and its bytes, and none of
// the dimensions a listing filters on — so no read keyed on a trace, a turn, a
// seat or a party can reach it, and it writes no party row.
func TestRecordForKeepsAPartAsStorageAndNothingElse(t *testing.T) {
	t.Parallel()
	record := uuid.New()
	part := events.New(types.AgentPhaseRecordPart{
		RecordID: record.String(), Index: 0, WholeBytes: 3, Data: []byte("abc"),
	}, events.TraceContext{TraceID: "tr-1", SpanID: "sp-1"})
	part.ID = types.PhaseRecordPartID(record, 0)
	part.Source = "Lead"

	rec, stored, err := store.RecordFor(part)
	if err != nil || !stored {
		t.Fatalf("RecordFor = stored %v, %v; a part must be stored, or its record's whole cannot be read back",
			stored, err)
	}
	if rec.ID != part.ID.String() || rec.Type != part.Type || !rec.Time.Equal(part.Timestamp) {
		t.Errorf("row identity = %s %s %v; want the part's own", rec.ID, rec.Type, rec.Time)
	}
	if rec.Category != "" || rec.Source != "" || rec.Actor != "" || rec.TraceID != "" ||
		rec.SpanID != "" || len(rec.Tags) != 0 || rec.Spend != nil {
		t.Errorf("a part's row names what a listing filters on: %+v", rec)
	}
	var back events.Event
	if err := json.Unmarshal(rec.Payload, &back); err != nil {
		t.Fatalf("the stored payload does not decode: %v", err)
	}
	if got, ok := back.Data.(*types.AgentPhaseRecordPart); !ok || string(got.Data) != "abc" {
		t.Errorf("the stored payload holds %#v, want the part's bytes", back.Data)
	}
}

// A ZERO TIMESTAMP IS STAMPED, NOT STORED: year one is below every read floor,
// so the row would exist and no query would return it, and Append refuses one.
func TestRecordForStampsAZeroTimestamp(t *testing.T) {
	t.Parallel()
	ev := events.New(types.TaskAssigned{Description: "ship it"}, events.TraceContext{})
	ev.Timestamp = time.Time{}
	rec, stored, err := store.RecordFor(ev)
	if err != nil || !stored {
		t.Fatalf("RecordFor = stored %v, %v", stored, err)
	}
	if rec.Time.IsZero() {
		t.Error("a zero timestamp reached the row, where no read floor lets a query return it")
	}
}
