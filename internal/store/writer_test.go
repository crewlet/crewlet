package store_test

import (
	"encoding/json"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// serialize is an event as it reaches the tag derivation: the bytes the
// publish listener marshalled, envelope and payload flattened together.
//
// Raw JSON rather than a typed event, which is deliberate and is the whole
// reason the derivation reads bytes: it is the only way to produce the case
// that matters most below, a type this build has never heard of.
func serialize(t *testing.T, body string) []byte {
	t.Helper()
	if !json.Valid([]byte(body)) {
		t.Fatalf("fixture is not JSON: %s", body)
	}
	return []byte(body)
}

func TestExtractTagsPromotesTheDimensions(t *testing.T) {
	t.Parallel()
	tags := store.ExtractTags(serialize(t, `{
		"id": "6f1a2b3c-0000-4000-8000-000000000001",
		"type": "task_assigned",
		"timestamp": "2026-04-01T12:00:00Z",
		"source": "engine",
		"trace_id": "tr-1",
		"agent_id": "agent-9",
		"role": "engineer",
		"task_id": "task-4",
		"turn_id": "turn-3",
		"sender": "alice",
		"conversation_key": "slack:C1/T1",
		"failed": false
	}`))

	want := map[string]string{
		"agent_id":         "agent-9",
		"agent_role":       "engineer",
		"task_id":          "task-4",
		"turn_id":          "turn-3",
		"sender":           "alice",
		"conversation_key": "slack:C1/T1",
	}
	for k, v := range want {
		if tags[k] != v {
			t.Errorf("tag %s = %q, want %q", k, tags[k], v)
		}
	}
	// Only set when true, so the tag doubles as a filter for failures.
	if _, present := tags["failed"]; present {
		t.Errorf("failed tag stamped on a successful event: %v", tags)
	}
}

func TestExtractTagsStampsFailure(t *testing.T) {
	t.Parallel()
	tags := store.ExtractTags(serialize(t, `{
		"id": "6f1a2b3c-0000-4000-8000-000000000002",
		"type": "agent_phase_completed",
		"timestamp": "2026-04-01T12:00:00Z",
		"source": "engine",
		"failed": true
	}`))
	if tags["failed"] != "true" {
		t.Fatalf("failed tag = %q; a listing never selects the payload, so this "+
			"is the only thing that survives into history", tags["failed"])
	}
}

// TestExtractTagsReadsUnknownTypes is why these dimensions are read off the
// envelope rather than off a decoded payload: a reader that needs the concrete
// type sees nothing at all on a type it does not know. A rolling upgrade
// publishes types the older half has never heard of; those events must still
// be indexed by the agent they concern.
func TestExtractTagsReadsUnknownTypes(t *testing.T) {
	t.Parallel()
	tags := store.ExtractTags(serialize(t, `{
		"id": "6f1a2b3c-0000-4000-8000-000000000003",
		"type": "an_event_type_from_the_future",
		"timestamp": "2026-04-01T12:00:00Z",
		"source": "engine",
		"role": "from-the-future",
		"a2a_context": {"channel_id": "chan-7"},
		"some_field_this_build_has_never_seen": 42
	}`))
	if tags["agent_role"] != "from-the-future" {
		t.Errorf("agent_role = %q", tags["agent_role"])
	}
	if tags["a2a_channel_id"] != "chan-7" {
		t.Errorf("a2a_channel_id = %q", tags["a2a_channel_id"])
	}
}

// TestExtractTagsIgnoresAWrongTypedField pins the per-field degradation: a
// value that is not a string zeroes that tag alone rather than failing the
// whole derivation, which is why every field is read on its own.
func TestExtractTagsIgnoresAWrongTypedField(t *testing.T) {
	t.Parallel()
	tags := store.ExtractTags(serialize(t, `{
		"type": "task_assigned",
		"task_id": 42,
		"sender": "alice"
	}`))
	if _, present := tags["task_id"]; present {
		t.Errorf("a numeric task_id became a tag: %v", tags)
	}
	if tags["sender"] != "alice" {
		t.Errorf("one wrong-typed field cost the rest: %v", tags)
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

func TestAnUntrackedTypeIsNotStored(t *testing.T) {
	t.Parallel()
	if _, ok := store.Category("agent_turn_progress"); ok {
		t.Fatal("agent_turn_progress is a live-only signal; " +
			"agent_phase_completed is its durable record")
	}
}
