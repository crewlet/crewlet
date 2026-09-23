package queries_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
)

// longResult is the tool result a cut record's whole carries and its row does
// not.
var longResult = strings.Repeat("every word of the page ", 300)

// storeParts stores a phase record's whole the way the engine's writer does
// once the transport refused the record: in parts of the given sizes (the last
// taking the rest), writing the ones keep says. It returns the record's event,
// which nothing has stored, and its whole.
func storeParts(t *testing.T, log *store.EventLog, keep func(index int) bool, sizes ...int) (*events.Event, []byte) {
	t.Helper()
	at := time.Now().UTC().Add(-time.Minute)
	env := events.New(types.AgentPhaseCompleted{
		RoleName: "PM", Agent: "agent-pm", TurnID: "turn-cut", Phase: types.PhaseExecute,
		Model: "claude-sonnet-5", InputTokens: 700, OutputTokens: 50, TotalTokens: 750,
		ToolExecutions: []types.ToolExecution{{"name": "read_page", "result": longResult}},
	}, events.TraceContext{TraceID: "trace-cut"})
	env.Timestamp, env.Source = at, "PM"
	whole, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	offset := 0
	for index, size := range sizes {
		if index == len(sizes)-1 {
			size = len(whole) - offset
		}
		part := events.New(types.AgentPhaseRecordPart{
			RecordID: env.ID.String(), Index: index, Offset: offset,
			WholeBytes: len(whole), Data: whole[offset : offset+size],
		}, events.TraceContext{TraceID: "trace-cut"})
		part.ID = types.PhaseRecordPartID(env.ID, index)
		part.Timestamp = at
		offset += size
		if keep(index) {
			appendEvent(t, log, part)
		}
	}
	return env, whole
}

// storeCutRecord stores a phase record the way the engine's writer does once
// the transport refused it whole: its whole in parts ([storeParts]), and its
// own row cut and naming them. It returns the record's id and its whole.
func storeCutRecord(t *testing.T, log *store.EventLog, keep func(index int) bool, sizes ...int) (string, []byte) {
	t.Helper()
	env, whole := storeParts(t, log, keep, sizes...)
	cut := *env.Data.(*types.AgentPhaseCompleted)
	cut.ToolExecutions = []types.ToolExecution{{"name": "read_page", "result": "…",
		"result_bytes": len(longResult)}}
	cut.WholeBytes, cut.WholeParts = len(whole), len(sizes)
	cutEnv := *env
	cutEnv.Data = &cut
	appendEvent(t, log, &cutEnv)
	return env.ID.String(), whole
}

// appendEvent writes ev as the engine's writer would.
func appendEvent(t *testing.T, log *store.EventLog, ev *events.Event) {
	t.Helper()
	rec, stored, err := store.RecordFor(ev)
	if err != nil || !stored {
		t.Fatalf("RecordFor(%s) = %v, %v", ev.Type, stored, err)
	}
	if err := log.Append(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
}

func everyPart(int) bool { return true }

// A PHASE RECORD IS ANSWERED WHOLE, from the parts its whole was kept in.
//
// The row a cut record left carries its longest texts shortened and says so;
// this is the one question that answers what those texts were, and it answers
// the record exactly as it would have been stored whole.
func TestAPhaseRecordIsAnsweredWholeFromItsParts(t *testing.T) {
	t.Parallel()
	log := openStore(t).Events()
	id, whole := storeCutRecord(t, log, everyPart, 1000, 1000, 0)

	got := asMap(t, answer(t, queries.Sources{Events: log}, "phase_record", map[string]any{"id": id}))
	if got["whole"] != true || got["parts"] != float64(3) || got["whole_bytes"] != float64(len(whole)) {
		t.Fatalf("answer = whole %v from %v parts of %v bytes; want the whole, from its 3 parts",
			got["whole"], got["parts"], got["whole_bytes"])
	}
	payload, _ := got["payload"].(map[string]any)
	calls, _ := payload["tool_executions"].([]any)
	if len(calls) != 1 || calls[0].(map[string]any)["result"] != longResult {
		t.Errorf("the whole's tool call is %v; want its result whole", calls)
	}
	if payload["id"] != id {
		t.Errorf("the whole's id is %v, want the record's own %s", payload["id"], id)
	}
}

// A RECORD PUBLISHED WHOLE IS ITS OWN WHOLE, so a reader asks for the whole of
// any phase record without first working out whether it was cut.
func TestAPhaseRecordPublishedWholeIsAnsweredWithItsOwnRow(t *testing.T) {
	t.Parallel()
	log := openStore(t).Events()
	ev := events.New(types.AgentPhaseCompleted{RoleName: "PM", Phase: types.PhaseReview,
		Response: "done and dusted"}, events.TraceContext{})
	appendEvent(t, log, ev)

	got := asMap(t, answer(t, queries.Sources{Events: log}, "phase_record",
		map[string]any{"id": ev.ID.String()}))
	payload, _ := got["payload"].(map[string]any)
	if got["whole"] != true || got["parts"] != float64(0) || payload["response"] != "done and dusted" {
		t.Errorf("answer = %v; want the record's own row, whole, from no parts", got)
	}
}

// A WHOLE NOT ALL HERE IS ANSWERED, WITH HOW MUCH IS — never with the bytes
// that are, which would read as the whole, and never as a failure, which would
// hide how much is missing.
func TestAWholeNotAllHereSaysHowMuchIs(t *testing.T) {
	t.Parallel()
	log := openStore(t).Events()
	id, whole := storeCutRecord(t, log, func(index int) bool { return index != 1 }, 1000, 1000, 0)

	got := asMap(t, answer(t, queries.Sources{Events: log}, "phase_record", map[string]any{"id": id}))
	if got["whole"] != false || got["found_bytes"] != float64(1000) ||
		got["whole_bytes"] != float64(len(whole)) || got["parts"] != float64(1) {
		t.Fatalf("answer = %v; want 1000 of %d bytes, in 1 part, and not whole", got, len(whole))
	}
	if _, carried := got["payload"]; carried {
		t.Error("an answer that is not whole carries a payload a reader would take for the whole")
	}
	if note, _ := got["note"].(string); !strings.Contains(note, "every part was published") {
		t.Errorf("note = %q, want it to say the parts were all published and are gone", note)
	}
}

// A WHOLE WITH NO RECORD BESIDE IT DOES NOT GUESS WHY IT IS SHORT. With no
// row, nothing on this node says whether every part was published, so the
// note names both places the rest can have gone rather than picking one.
func TestAWholeWithNoRecordNamesBothReasonsItIsShort(t *testing.T) {
	t.Parallel()
	log := openStore(t).Events()
	env, whole := storeParts(t, log, func(index int) bool { return index == 0 }, 1000, 0)

	got := asMap(t, answer(t, queries.Sources{Events: log}, "phase_record",
		map[string]any{"id": env.ID.String()}))
	if got["whole"] != false || got["found_bytes"] != float64(1000) ||
		got["whole_bytes"] != float64(len(whole)) || got["parts"] != float64(1) {
		t.Fatalf("answer = %v; want 1000 of %d bytes, in 1 part, and not whole", got, len(whole))
	}
	note, _ := got["note"].(string)
	for _, reason := range []string{"phase_record_whole_not_kept", "event_write_failed"} {
		if !strings.Contains(note, reason) {
			t.Errorf("note = %q, want it to name %s", note, reason)
		}
	}
}

// NOTHING BY THE ID IS NOT FOUND — a dead link, not a broken node.
func TestAPhaseRecordNobodyHoldsIsNotFound(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Events: openStore(t).Events()})
	_, err := r.Answer(t.Context(), "phase_record",
		map[string]any{"id": "6f1c3d2e-0000-4000-8000-000000000001"}, "")
	if !errors.Is(err, queries.ErrNotFound) {
		t.Errorf("an id nothing holds answered %v, want ErrNotFound", err)
	}
	if _, err := r.Answer(t.Context(), "phase_record", map[string]any{}, ""); !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("a question with no id answered %v, want ErrBadParams", err)
	}
}

// A PART NEVER REACHES THE TURN, THE PHASE LIST OR THE EVENT LOG.
//
// Each part is storage for the record beside it, and on a real record nearly
// as large as one event may be: every answer that lists events would carry
// that for a row nobody reads, and the turn view would show a phase twice.
func TestAPartNeverReachesTheTurnThePhasesOrTheEventLog(t *testing.T) {
	t.Parallel()
	log := openStore(t).Events()
	id, _ := storeCutRecord(t, log, everyPart, 1000, 0)
	sources := queries.Sources{Events: log}
	partType := types.AgentPhaseRecordPart{}.EventType()

	for _, ask := range []struct {
		what, key string
		params    map[string]any
	}{
		{"turn", "events", map[string]any{"turn_id": "turn-cut"}},
		{"phases", "phases", map[string]any{}},
		{"events", "events", map[string]any{}},
		{"events", "events", map[string]any{"type": partType}},
		{"trace", "events", map[string]any{"trace_id": "trace-cut"}},
	} {
		got := asMap(t, answer(t, sources, ask.what, ask.params))
		listed := rows(t, got[ask.key])
		for _, row := range listed {
			if row["type"] == partType {
				t.Errorf("%s %v returned a part (%v)", ask.what, ask.params, row["id"])
			}
		}
		if ask.params["type"] == nil && (len(listed) != 1 || listed[0]["id"] != id) {
			t.Errorf("%s %v returned %d rows; want the phase record alone", ask.what, ask.params, len(listed))
		}
	}
	series := asMap(t, answer(t, sources, "event_series", map[string]any{"bucket": "day"}))
	if series["total"] != float64(1) {
		t.Errorf("the event series counts %v rows; want the phase record alone", series["total"])
	}
}
