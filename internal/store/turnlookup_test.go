package store_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// READING ONE TURN MEANS EVERY ROW IT TOUCHED.
//
// `turn_id` is promoted out of the payload by the spend extractor, whose
// subject is SPEND — so it reads one event type and returns nil for the rest.
// That is right for the rollup and wrong for identity: a delivery, a tool call
// or an A2A ask carries a turn id without carrying a token count, and a trace
// built only from phase completions is missing most of what happened.
func TestATurnReadsEveryEventItTouchedNotOnlyItsPhases(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	at := time.Now().UTC().Add(-time.Minute)

	for i, rec := range []store.EventRecord{
		{
			ID: "e-phase", Type: "agent_phase_completed", Source: "engine",
			Category: "agent", Tags: map[string]string{"turn_id": "turn-1"},
			Payload: []byte(`{"turn_id":"turn-1","phase":"plan","total_tokens":12}`),
		},
		{
			ID: "e-delivery", Type: "message_delivered", Source: "engine",
			Category: "comms", Tags: map[string]string{"turn_id": "turn-1"},
			Payload: []byte(`{"turn_id":"turn-1"}`),
		},
		{
			ID: "e-other", Type: "message_delivered", Source: "engine",
			Category: "comms", Tags: map[string]string{"turn_id": "turn-2"},
			Payload: []byte(`{"turn_id":"turn-2"}`),
		},
	} {
		rec.Time = at.Add(time.Duration(i) * time.Second)
		if err := log.Append(t.Context(), rec); err != nil {
			t.Fatalf("append %s: %v", rec.ID, err)
		}
	}

	got, err := log.Turn(t.Context(), "turn-1")
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	ids := make([]string, 0, len(got))
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	if len(ids) != 2 {
		t.Fatalf("turn events = %v, want the phase AND the delivery", ids)
	}
	// Oldest first: a turn is read forwards.
	if ids[0] != "e-phase" || ids[1] != "e-delivery" {
		t.Errorf("turn events = %v, want [e-phase e-delivery]", ids)
	}
}

// A non-phase row must not acquire a phase's numbers on the way in: the
// identity fallback fills the identifier columns only.
//
// ASSERTED THROUGH A READ THAT SELECTS THE COLUMNS. It used to read
// `Turn(...)[0].Spend`, which a read NEVER populates — `finishRecord` does not
// set that field — so the guard short-circuited on nil and the test could not
// fail whatever the writer did. `Turns` sums `total_tokens` over every row of
// a turn regardless of type, so a delivery that wrongly acquired a phase's
// numbers shows up there and nowhere else.
func TestTheTurnFallbackDoesNotInventSpend(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	at := time.Now().UTC().Add(-time.Minute)
	// A real phase, so the assertion below is "only this one counted"
	// rather than "nothing counted", which a broken writer also satisfies.
	if err := log.Append(t.Context(), store.EventRecord{
		ID: "e-phase", Type: "agent_phase_completed", Source: "engine",
		Category: "lifecycle", Time: at,
		Tags:    map[string]string{"turn_id": "turn-1", "agent_role": "PM"},
		Payload: []byte(`{"turn_id":"turn-1","total_tokens":10,"model":"real"}`),
	}); err != nil {
		t.Fatalf("append the phase: %v", err)
	}
	if err := log.Append(t.Context(), store.EventRecord{
		ID: "e-1", Type: "message_delivered", Source: "engine", Category: "comms",
		Time: at.Add(time.Second),
		Tags: map[string]string{"turn_id": "turn-1"},
		// A payload that WOULD look like spend if anything read it here.
		Payload: []byte(`{"turn_id":"turn-1","total_tokens":999,"model":"ghost"}`),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	rows, err := log.Turns(t.Context(), store.TurnQuery{})
	if err != nil {
		t.Fatalf("Turns: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d turn rows, want 1: %+v", len(rows), rows)
	}
	if rows[0].TotalTokens != 10 {
		t.Errorf("the turn totals %d tokens, want the phase's 10 — a delivery "+
			"was credited with a phase's numbers", rows[0].TotalTokens)
	}
	if rows[0].Models != "real" {
		t.Errorf("models = %q, want only the phase's", rows[0].Models)
	}
}

// A ROW'S WORK KEY IS READ OFF THE COLUMN, never out of its tags blob.
//
// schema/0029 backfilled `work_key` from `turn_id` — which is where the work
// key lived before ADR-0017 split the two — and deliberately did not rewrite
// the stored tags, because those blobs record what the writer actually
// extracted from an event whose JSON carried no such field. So for every row
// written before that migration the column holds the key and the tags do not,
// and a reader going through the tags answers "no unit of work" for exactly
// the history the backfill exists to preserve, while `/events?work_key=` —
// which filters on the column — returns those same rows. Two surfaces
// disagreeing about one question.
//
// The shape is reproduced here the way Append itself produces it: Spend is the
// carrier for every promoted column, so a record that sets the key there and
// not in its tags writes precisely a post-backfill row.
func TestAWorkKeyIsReadFromItsColumnAndNotFromTheTags(t *testing.T) {
	t.Parallel()
	log := open(t).Events()

	if err := log.Append(t.Context(), store.EventRecord{
		ID: "e-legacy", Type: "agent_phase_completed", Source: "engine",
		Category: "agent", Time: time.Now().UTC().Add(-time.Minute),
		// AS THE BACKFILL LEAVES IT: the column carries the key, the
		// blob carries only what that build knew to extract.
		Tags:    map[string]string{"turn_id": "run-legacy"},
		Spend:   &store.Spend{TurnID: "run-legacy", WorkKey: "wk-legacy"},
		Payload: []byte(`{"turn_id":"run-legacy","phase":"execute"}`),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	rows, err := log.Turn(t.Context(), "run-legacy")
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d rows, want the one appended", len(rows))
	}
	if rows[0].WorkKey != "wk-legacy" {
		t.Errorf("WorkKey = %q, want the backfilled column; a read that went "+
			"through Tags answers %q for every row written before schema/0029",
			rows[0].WorkKey, rows[0].Tags["work_key"])
	}
	// AND THE FILTER AGREES WITH IT, which is the whole point of one
	// authority: a listing found by the column must say which key it was
	// found by.
	found, err := log.List(t.Context(), store.ListQuery{WorkKey: "wk-legacy"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(found) != 1 || found[0].WorkKey != "wk-legacy" {
		t.Errorf("filtering on the column returned %d rows carrying %q",
			len(found), keysOf(found))
	}
	// AND THE SINGLE-EVENT READ AGREES, which is the read a person reaches
	// by pasting an id and the one a second hand-written Scan silently
	// left behind when the column was added.
	one, err := log.ByID(t.Context(), "e-legacy")
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if one.WorkKey != "wk-legacy" {
		t.Errorf("ByID WorkKey = %q, want wk-legacy", one.WorkKey)
	}
}

func keysOf(recs []store.EventRecord) []string {
	out := make([]string, 0, len(recs))
	for _, rec := range recs {
		out = append(out, rec.WorkKey)
	}
	return out
}
