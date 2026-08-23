package store_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
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
		"type": "task_started",
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
		t.Fatal("task_started must be stored")
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

// TestRecordForReadsUnknownTypes is the improvement over the Python writer,
// which read these dimensions by attribute lookup and therefore saw nothing at
// all on a type it did not know. A rolling upgrade publishes types the older
// half has never heard of; those events must still be indexed by the agent
// they concern.
func TestRecordForReadsUnknownTypes(t *testing.T) {
	t.Parallel()
	ev := buildEvent(t, `{
		"id": "6f1a2b3c-0000-4000-8000-000000000003",
		"type": "task_started",
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
		"lifecycle": true, "task": true, "communication": true, "a2a": true,
		"decision": true, "knowledge": true, "notification": true,
		"system": true, "learning": true,
	}
	for _, typ := range []string{
		"org_started", "task_created", "message_sent", "a2a_channel_opened",
		"decision_requested", "document_created", "external_notification",
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
