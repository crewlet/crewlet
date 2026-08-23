package queries_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/store"
)

func openStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "q.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// seedEvents writes n rows, oldest first.
func seedEvents(t *testing.T, log *store.EventLog, n int, mutate func(int, *store.EventRecord)) {
	t.Helper()
	// Relative to NOW, not a literal date: List filters on the store's
	// retention window, so a fixture dated in the past — which every
	// fixture with a literal date eventually is — is outside it and the
	// listing comes back empty for a reason that has nothing to do with
	// what is under test. Trace has no such filter, which is exactly how
	// this hid: the trace cases passed while the listings did not.
	base := time.Now().UTC().Add(-time.Hour)
	for i := range n {
		rec := store.EventRecord{
			ID:       "e" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			Type:     "task_started",
			Source:   "engine",
			Time:     base.Add(time.Duration(i) * time.Second),
			Category: "task",
			Actor:    "Lead",
			Summary:  "did a thing",
			TraceID:  "tr-1",
			Payload:  json.RawMessage(`{"role":"Lead"}`),
		}
		if mutate != nil {
			mutate(i, &rec)
		}
		if err := log.Append(t.Context(), rec); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
}

func registryOver(t *testing.T, s queries.Sources) *queries.Registry {
	t.Helper()
	r := queries.NewRegistry()
	queries.Register(r, s)
	return r
}

func ask(t *testing.T, r *queries.Registry, what string, params map[string]any) map[string]any {
	t.Helper()
	got, err := r.Answer(t.Context(), what, params, "")
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	out, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("%s answered %T, want a map", what, got)
	}
	return out
}

// --- what a node without a surface answers ------------------------------- //

func TestAQuestionWithNoSourceIsNotRegistered(t *testing.T) {
	t.Parallel()
	// Unknown rather than a failure, which is the honest answer for a node
	// that does not have that surface at all — and distinct from an empty
	// one, because a dashboard drawing "no events" for "this node has no
	// event log" would report a quiet company during a misconfiguration.
	r := registryOver(t, queries.Sources{})
	if got := r.Names(); len(got) != 0 {
		t.Errorf("names = %v, want none with no sources", got)
	}
	if _, err := r.Answer(t.Context(), "events", nil, ""); !errors.Is(err, queries.ErrUnknown) {
		t.Errorf("err = %v, want ErrUnknown", err)
	}
}

func TestEachSourceRegistersItsOwnQuestions(t *testing.T) {
	t.Parallel()
	state := livestate.New()
	db := openStore(t)

	if got := registryOver(t, queries.Sources{State: state}).Names(); len(got) != 2 {
		t.Errorf("projection questions = %v", got)
	}
	if got := registryOver(t, queries.Sources{Events: db.Events()}).Names(); len(got) != 3 {
		t.Errorf("event-log questions = %v", got)
	}
	full := registryOver(t, queries.Sources{
		State: state, Events: db.Events(),
		Health: func() any { return map[string]any{"status": "ok"} },
	})
	for _, want := range []string{"agent", "event", "events", "stream", "tokens", "trace"} {
		found := false
		for _, got := range full.Names() {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%q is not registered: %v", want, full.Names())
		}
	}
}

// --- the projection questions -------------------------------------------- //

func TestAgentAnswersOneSeatsLiveState(t *testing.T) {
	t.Parallel()
	state := livestate.New()
	state.Apply(livestate.Envelope{
		ID: "e1", Type: "task_started", Timestamp: "2026-06-14T12:00:00Z",
		Category: "task", Payload: map[string]any{"role": "Lead", "task_id": "t-1"},
	})
	r := registryOver(t, queries.Sources{State: state})

	got := ask(t, r, "agent", map[string]any{"role": "Lead"})
	live, _ := got["live"].(*livestate.Overlay)
	if live == nil || live.State != "working" {
		t.Fatalf("answer = %+v", got)
	}
}

func TestAgentAnswersASeatItHasNeverSeen(t *testing.T) {
	t.Parallel()
	// A role configured and never spawned is exactly this, and a 404 there
	// would make a healthy new company look broken.
	r := registryOver(t, queries.Sources{State: livestate.New()})
	got := ask(t, r, "agent", map[string]any{"role": "Nobody"})
	if got["role"] != "Nobody" || got["live"] != nil {
		t.Errorf("answer = %+v", got)
	}
}

func TestAgentNeedsARole(t *testing.T) {
	t.Parallel()
	r := registryOver(t, queries.Sources{State: livestate.New()})
	if _, err := r.Answer(t.Context(), "agent", nil, ""); !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("err = %v, want ErrBadParams", err)
	}
}

func TestTokensAnswersTheLiveWindow(t *testing.T) {
	t.Parallel()
	state := livestate.New()
	state.Apply(livestate.Envelope{
		ID: "p1", Type: "agent_phase_completed", Timestamp: "2026-06-14T12:00:00Z",
		Category: "agent", Payload: map[string]any{
			"role": "Lead", "phase": "plan", "total_tokens": 12,
		},
	})
	r := registryOver(t, queries.Sources{State: state})

	got := ask(t, r, "tokens", nil)
	records, _ := got["records"].([]livestate.SpendRecord)
	if len(records) != 1 || records[0].TotalTokens != 12 {
		t.Errorf("records = %+v", records)
	}
	// The window is reported, not assumed: the rollup beside it on screen
	// covers a different one, and a reader has to be able to tell.
	if got["window_hours"] != 24 {
		t.Errorf("window = %v", got["window_hours"])
	}
}

func TestStreamAnswersHealthUnderItsOwnName(t *testing.T) {
	t.Parallel()
	// Deliberately not called health: a query must never share a name with
	// a push kind, or a reader of the protocol has to know which direction
	// a frame was travelling to know what it means.
	r := registryOver(t, queries.Sources{
		Health: func() any { return map[string]any{"status": "ok"} },
	})
	if got := ask(t, r, "stream", nil); got["status"] != "ok" {
		t.Errorf("answer = %+v", got)
	}
	if _, err := r.Answer(t.Context(), "health", nil, ""); !errors.Is(err, queries.ErrUnknown) {
		t.Errorf("a push kind is answerable as a query: %v", err)
	}
}

// --- the event log ------------------------------------------------------- //

func TestEventsAnswersAPageNewestFirst(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	seedEvents(t, db.Events(), 5, nil)
	r := registryOver(t, queries.Sources{Events: db.Events()})

	got := ask(t, r, "events", map[string]any{"limit": float64(3)})
	rows, _ := got["events"].([]store.EventRecord)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want the requested 3", len(rows))
	}
	if !rows[0].Time.After(rows[2].Time) {
		t.Errorf("rows are not newest first: %v .. %v", rows[0].Time, rows[2].Time)
	}
}

func TestAPageEchoesTheCursorToResumeFrom(t *testing.T) {
	t.Parallel()
	// (time, id) is the table's key, and a client assembling it from the
	// last row's fields would be reimplementing the one thing that must
	// not drift.
	db := openStore(t)
	seedEvents(t, db.Events(), 5, nil)
	r := registryOver(t, queries.Sources{Events: db.Events()})

	first := ask(t, r, "events", map[string]any{"limit": float64(2)})
	next, _ := first["next"].(map[string]any)
	if next == nil || next["before_id"] == "" {
		t.Fatalf("no cursor: %+v", first)
	}

	second := ask(t, r, "events", map[string]any{
		"limit": float64(2), "before_id": next["before_id"], "before_time": next["before_time"],
	})
	firstRows, _ := first["events"].([]store.EventRecord)
	secondRows, _ := second["events"].([]store.EventRecord)
	if len(secondRows) == 0 {
		t.Fatal("the second page is empty")
	}
	for _, a := range firstRows {
		for _, b := range secondRows {
			if a.ID == b.ID {
				t.Errorf("the cursor repeated row %s", a.ID)
			}
		}
	}
}

func TestACursorWithoutItsTimestampIsRefused(t *testing.T) {
	t.Parallel()
	// Time alone is not unique — burst writes share a timestamp at
	// microsecond resolution — so a cursor missing half its key would skip
	// or repeat whatever collided with it, silently.
	db := openStore(t)
	r := registryOver(t, queries.Sources{Events: db.Events()})
	_, err := r.Answer(t.Context(), "events", map[string]any{"before_id": "e1"}, "")
	if !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("err = %v, want ErrBadParams", err)
	}
}

func TestTheLimitIsClampedNotObeyed(t *testing.T) {
	t.Parallel()
	// Unbounded lets one tab pull the whole event log through a process
	// every other tab shares.
	db := openStore(t)
	seedEvents(t, db.Events(), 20, nil)
	r := registryOver(t, queries.Sources{Events: db.Events()})

	got := ask(t, r, "events", map[string]any{"limit": float64(1 << 20)})
	rows, _ := got["events"].([]store.EventRecord)
	if len(rows) > queries.MaxEventPage {
		t.Errorf("rows = %d, want the ceiling of %d", len(rows), queries.MaxEventPage)
	}
	// And a limit left off is a default, not nothing.
	if rows := ask(t, r, "events", nil)["events"].([]store.EventRecord); len(rows) == 0 {
		t.Error("a query with no limit returned nothing")
	}
}

func TestTheStoresOwnFiltersArePassedThrough(t *testing.T) {
	t.Parallel()
	// A listing this surface filtered itself would page differently from
	// one the store filtered, and the difference shows up as rows that
	// vanish when a reader scrolls.
	db := openStore(t)
	seedEvents(t, db.Events(), 6, func(i int, rec *store.EventRecord) {
		if i%2 == 0 {
			rec.Type = "task_completed"
			rec.Actor = "CTO"
		}
	})
	r := registryOver(t, queries.Sources{Events: db.Events()})

	got := ask(t, r, "events", map[string]any{"type": "task_completed"})
	rows, _ := got["events"].([]store.EventRecord)
	if len(rows) == 0 {
		t.Fatal("the type filter matched nothing")
	}
	for _, row := range rows {
		if row.Type != "task_completed" {
			t.Errorf("the type filter let %q through", row.Type)
		}
	}
	byActor := ask(t, r, "events", map[string]any{"actor": "CTO"})["events"].([]store.EventRecord)
	for _, row := range byActor {
		if row.Actor != "CTO" {
			t.Errorf("the actor filter let %q through", row.Actor)
		}
	}
}

func TestAnEmptyPageSaysHistoryIsExhausted(t *testing.T) {
	t.Parallel()
	// A page shorter than the limit does not mean the walk is over when a
	// related-agent filter is set — that filter over-fetches and
	// post-filters — so only a zero-row page ends it. Saying so beats a
	// client inferring it wrongly.
	db := openStore(t)
	r := registryOver(t, queries.Sources{Events: db.Events()})

	got := ask(t, r, "events", nil)
	if got["exhausted"] != true {
		t.Errorf("an empty page did not report itself exhausted: %+v", got)
	}
	if got["next"] != nil {
		t.Errorf("an empty page offered a cursor: %v", got["next"])
	}

	seedEvents(t, db.Events(), 3, nil)
	if got := ask(t, r, "events", nil); got["exhausted"] != false {
		t.Errorf("a page with rows reported itself exhausted")
	}
}

func TestEventAnswersOneRowWithItsPayload(t *testing.T) {
	t.Parallel()
	// The listing deliberately omits payloads; this is where a reader gets
	// one.
	db := openStore(t)
	seedEvents(t, db.Events(), 1, nil)
	r := registryOver(t, queries.Sources{Events: db.Events()})

	rows := ask(t, r, "events", nil)["events"].([]store.EventRecord)
	got, err := r.Answer(t.Context(), "event", map[string]any{"id": rows[0].ID}, "")
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	rec, _ := got.(store.EventRecord)
	if len(rec.Payload) == 0 {
		t.Error("the single-row read carried no payload")
	}
}

func TestEventAndTraceNeedTheirIdentifiers(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	r := registryOver(t, queries.Sources{Events: db.Events()})
	for _, what := range []string{"event", "trace"} {
		if _, err := r.Answer(t.Context(), what, nil, ""); !errors.Is(err, queries.ErrBadParams) {
			t.Errorf("%s: err = %v, want ErrBadParams", what, err)
		}
	}
}

func TestTraceAnswersEverythingSharingOne(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	seedEvents(t, db.Events(), 4, func(i int, rec *store.EventRecord) {
		if i >= 2 {
			rec.TraceID = "tr-2"
		}
	})
	r := registryOver(t, queries.Sources{Events: db.Events()})

	got := ask(t, r, "trace", map[string]any{"trace_id": "tr-1"})
	rows, _ := got["events"].([]store.EventRecord)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want the two sharing tr-1", len(rows))
	}
	for _, row := range rows {
		if row.TraceID != "tr-1" {
			t.Errorf("a row from %q came back", row.TraceID)
		}
	}
}
