package queries_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
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
			Type:     "agent_phase_started",
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

// askRaw returns the answer as it came back, for the surfaces that answer with
// a typed value rather than a map.
func askRaw(t *testing.T, r *queries.Registry, what string, params map[string]any) any {
	t.Helper()
	got, err := r.Answer(t.Context(), what, params, "")
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	return got
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
	// EXCEPT THE ONES WHOSE SOURCE IS THE CALLER. `viewer` answers who
	// presented this credential, which is a fact about the request rather
	// than about anything this process was wired with — so a node with no
	// sources at all still answers it, and answers "no seat", which is what
	// lets a screen say what to bind instead of looking broken.
	if got := r.Names(); !slices.Equal(got, []string{"viewer"}) {
		t.Errorf("names = %v, want only the caller's own questions", got)
	}
	if _, err := r.Answer(t.Context(), "events", nil, ""); !errors.Is(err, queries.ErrUnknown) {
		t.Errorf("err = %v, want ErrUnknown", err)
	}
}

func TestEachSourceRegistersItsOwnQuestions(t *testing.T) {
	t.Parallel()
	state := livestate.New()
	db := openStore(t)

	// ALWAYS REGISTERED, whatever this process has: a credential is
	// presented to a node with no sources at all, and "this token resolves
	// to no seat" is the answer the screen needs in order to say what to
	// bind. Named here so adding another is a deliberate edit to this list
	// rather than a silent shift in every count below.
	//
	// THE EXACT SET, never a count with the names in a comment beside it.
	// That was the shape here and it had already drifted: `turns` was
	// registered, the comment still said five, and the number it compared
	// against was the old one — so the registration this case exists to
	// notice went unnoticed, and the failure it eventually produced named a
	// number rather than the question that had appeared.
	for _, c := range []struct {
		what    string
		sources queries.Sources
		names   []string
	}{
		// `viewer` answers who presented this credential, which is a fact
		// about the REQUEST rather than about anything this process was
		// wired with.
		{"a node with no sources at all", queries.Sources{}, []string{"viewer"}},
		{"the live projection alone", queries.Sources{State: state},
			[]string{"agent", "tokens", "viewer"}},
		// `turn` is what made "everything that happened in this unit of
		// work" askable at all (see migration 0014); `turns` is the list
		// of them, which the dashboard used to fake by paging the raw
		// feed; `phases` is the company-wide phase record WITH its
		// payloads, which the event listing deliberately cannot serve;
		// `token_series` is the spend with a time axis, which the
		// breakdown has no dimension for; and `event_series` is the log's
		// own, which a page of rows has no dimension for either.
		{"the event log alone", queries.Sources{Events: db.Events()},
			[]string{"event", "event_series", "events", "phases", "token_series",
				"trace", "turn", "turns", "viewer"}},
		{"both, plus health", queries.Sources{
			State: state, Events: db.Events(),
			Health: func(context.Context) any { return map[string]any{"status": "ok"} },
		}, []string{"agent", "event", "event_series", "events", "phases", "stream",
			"token_series", "tokens", "trace", "turn", "turns", "viewer"}},
	} {
		if got := registryOver(t, c.sources).Names(); !slices.Equal(got, c.names) {
			t.Errorf("%s answers\n  %v\nwant\n  %v", c.what, got, c.names)
		}
	}
}

// --- the projection questions -------------------------------------------- //

func TestAgentAnswersOneSeatsLiveState(t *testing.T) {
	t.Parallel()
	state := livestate.New()
	state.Apply(&livestate.Envelope{
		ID: "e1", Type: "agent_phase_started", Timestamp: "2026-06-14T12:00:00Z",
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
	state.Apply(&livestate.Envelope{
		ID: "p1", Type: "agent_phase_completed", Timestamp: "2026-06-14T12:00:00Z",
		Category: "agent", Payload: map[string]any{
			"role": "Lead", "phase": "plan", "total_tokens": 12,
		},
	})
	r := registryOver(t, queries.Sources{State: state})

	// THE ROLLUP, not the records. store.js reads `.totals` off this and
	// the spend view reads `.since`/`.until`; a list of raw records fails the
	// first check and is discarded, which left the whole Spend room blank
	// with the numbers sitting in memory the entire time.
	got := askRaw(t, r, "tokens", nil).(tokens.Rollup)
	if got.Totals.TotalTokens != 12 || got.Totals.Calls != 1 {
		t.Errorf("totals = %+v", got.Totals)
	}
	if len(got.ByPhase) != 1 || got.ByPhase[0].Phase != "plan" {
		t.Errorf("by_phase = %+v", got.ByPhase)
	}
	// The window is reported, not assumed: a reader comparing this against
	// a different window on the same screen has to be able to tell them
	// apart, and a number with the wrong label is worse than no label.
	if width := windowWidth(t, got); width != livestate.LiveSpendWindow {
		t.Errorf("window = %s .. %s (%s), want the live window %s",
			got.Since, got.Until, width, livestate.LiveSpendWindow)
	}
	// The high-water mark the client folds live events onto. Without it an
	// event that is both in this baseline and redelivered on the stream is
	// counted twice.
	if got.AggregatedThrough != "2026-06-14T12:00:00Z" {
		t.Errorf("aggregated_through = %q", got.AggregatedThrough)
	}
}

func TestTokensOverAnotherWindowReadsTheStore(t *testing.T) {
	t.Parallel()
	// The live projection can only answer for its own window. Any other one
	// is a scan — folded by the SAME aggregator, so the number a reader
	// sees when they change the window is comparable with the one they
	// were looking at.
	db := openStore(t)
	log := db.Events()
	write := func(id, role, phase string, total int) {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{
			"role": role, "phase": phase, "model": "m-1", "turn_id": "tn-1",
			"input_tokens": total / 2, "output_tokens": total / 2,
			"total_tokens": total,
		})
		if err := log.Append(t.Context(), store.EventRecord{
			ID: id, Type: "agent_phase_completed", Time: time.Now().UTC().Add(-time.Hour),
			Category: "system", Actor: role, Summary: "phase",
			Tags: map[string]string{"agent_role": role}, Payload: payload,
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	write("s1", "Lead", "plan", 10)
	write("s2", "Lead", "execute", 20)
	write("s3", "Coder", "plan", 6)

	r := registryOver(t, queries.Sources{State: livestate.New(), Events: log})
	got := askRaw(t, r, "tokens", map[string]any{"since_days": 3}).(tokens.Rollup)

	if got.Totals.TotalTokens != 36 || got.Totals.Calls != 3 {
		t.Errorf("totals = %+v", got.Totals)
	}
	if width := windowWidth(t, got); width != 3*24*time.Hour {
		t.Errorf("window = %s .. %s (%s), want the three days asked for",
			got.Since, got.Until, width)
	}
	// Biggest first, and every dimension present.
	if len(got.ByPhase) != 2 || got.ByPhase[0].Phase != "execute" {
		t.Errorf("by_phase = %+v, want execute first", got.ByPhase)
	}
	if len(got.ByAgent) != 2 || got.ByAgent[0].Role != "Lead" {
		t.Errorf("by_agent = %+v", got.ByAgent)
	}
	if len(got.ByTurn) != 1 || got.ByTurn[0].TurnID != "tn-1" {
		t.Errorf("by_turn = %+v", got.ByTurn)
	}
}

func TestOneRoleCanBeAskedForAlone(t *testing.T) {
	t.Parallel()
	// A per-seat window is a store read even when it IS the live window:
	// the projection holds the whole org, and filtering it here would be a
	// second implementation of the store's own filter.
	db := openStore(t)
	log := db.Events()
	for _, seat := range []struct{ id, role string }{{"a", "Lead"}, {"b", "Coder"}} {
		payload, _ := json.Marshal(map[string]any{
			"role": seat.role, "phase": "plan", "total_tokens": 5,
		})
		if err := log.Append(t.Context(), store.EventRecord{
			ID: seat.id, Type: "agent_phase_completed", Time: time.Now().UTC(),
			Category: "system", Tags: map[string]string{"agent_role": seat.role},
			Payload: payload,
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	r := registryOver(t, queries.Sources{State: livestate.New(), Events: log})
	got := askRaw(t, r, "tokens", map[string]any{"agent_role": "Lead"}).(tokens.Rollup)

	if got.Totals.Calls != 1 || got.AgentRole != "Lead" {
		t.Errorf("rollup = %+v", got)
	}
}

func TestANodeWithNoEventStoreLabelsTheWindowItWasAsked(t *testing.T) {
	t.Parallel()
	// An empty rollup labelled with the window ASKED for, not the live one
	// relabelled: a week's heading over an hour's numbers is a lie about
	// what a reader is looking at.
	r := registryOver(t, queries.Sources{State: livestate.New()})
	got := askRaw(t, r, "tokens", map[string]any{"since_days": 14}).(tokens.Rollup)
	if width := windowWidth(t, got); width != 14*24*time.Hour || got.Totals.Calls != 0 {
		t.Errorf("rollup = %+v (window %s)", got, width)
	}
	// Never nil: the client does `d.by_phase.length`, so a null throws in
	// the browser rather than rendering an empty table.
	if got.ByPhase == nil || got.ByAgent == nil || got.ByTurn == nil {
		t.Error("an empty rollup carries nil slices, which marshal to null")
	}
}

func TestStreamAnswersHealthUnderItsOwnName(t *testing.T) {
	t.Parallel()
	// Deliberately not called health: a query must never share a name with
	// a push kind, or a reader of the protocol has to know which direction
	// a frame was travelling to know what it means.
	r := registryOver(t, queries.Sources{
		Health: func(context.Context) any { return map[string]any{"status": "ok"} },
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
			rec.Type = "agent_turn_completed"
			rec.Actor = "CTO"
		}
	})
	r := registryOver(t, queries.Sources{Events: db.Events()})

	got := ask(t, r, "events", map[string]any{"type": "agent_turn_completed"})
	rows, _ := got["events"].([]store.EventRecord)
	if len(rows) == 0 {
		t.Fatal("the type filter matched nothing")
	}
	for _, row := range rows {
		if row.Type != "agent_turn_completed" {
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
	// A SHORT TRACE IS NOT A CUT ONE, and the flag has to say so explicitly:
	// a client cannot tell an absent field from a false one.
	if got["truncated"] != false {
		t.Errorf("truncated = %#v on a two-event trace", got["truncated"])
	}
}

// A TRACE OF EXACTLY THE CAP IS NOT A TRUNCATED ONE.
//
// `len(rows) == cap` is the inference this replaces, and this is the boundary
// it gets wrong: every row of the trace is on the page, under a caution badge
// saying only the oldest of them are.
func TestATraceOfExactlyTheCapIsNotReportedCut(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	log := db.Events()
	base := time.Now().UTC().Add(-time.Hour)
	for i := range store.MaxTraceEvents {
		if err := log.Append(t.Context(), store.EventRecord{
			ID: fmt.Sprintf("t-%04d", i), Type: "task_assigned",
			Time:     base.Add(time.Duration(i) * time.Second),
			Category: "task", Actor: "PM", TraceID: "tr-exact",
		}); err != nil {
			t.Fatal(err)
		}
	}
	r := registryOver(t, queries.Sources{Events: log})

	got := ask(t, r, "trace", map[string]any{"trace_id": "tr-exact"})
	if rows, _ := got["events"].([]store.EventRecord); len(rows) != store.MaxTraceEvents {
		t.Fatalf("rows = %d, want the cap %d", len(rows), store.MaxTraceEvents)
	}
	if got["truncated"] != false {
		t.Errorf("truncated = %#v for a trace of exactly the cap; every row "+
			"of it is on the page", got["truncated"])
	}

	// THE CONTROL: one more event and it genuinely is cut.
	if err := log.Append(t.Context(), store.EventRecord{
		ID: "t-9999", Type: "task_assigned",
		Time:     base.Add(time.Duration(store.MaxTraceEvents) * time.Second),
		Category: "task", Actor: "PM", TraceID: "tr-exact",
	}); err != nil {
		t.Fatal(err)
	}
	if got := ask(t, r, "trace", map[string]any{"trace_id": "tr-exact"}); got["truncated"] != true {
		t.Errorf("truncated = %#v for a trace one past the cap", got["truncated"])
	}
}

func TestBudgetsPairTheCapWithTheDurableCounter(t *testing.T) {
	t.Parallel()
	// THE CAP AND THE COUNTER GO TOGETHER, and neither is useful alone: a
	// ceiling with no usage says nothing about how close a company is, and
	// usage with no ceiling says nothing about whether it will be refused.
	cfg := parsed(t, `
name: Acme
providers:
  llm:
    p: {type: anthropic, model: m, api_keys: ["${K}"]}
roles:
  - name: CEO
    handle: ceo
    llm: p
    token_budget: 500
  - name: Founder
    kind: human
    contact: {slack_user_id: U0F}
token_budget: 10000
`)
	organization, err := cfg.Organization()
	if err != nil {
		t.Fatalf("organization: %v", err)
	}
	ceo := organization.AgentSeatByHandle("ceo")
	id, _ := organization.AgentIDFor(ceo)
	budgets := coordmemory.NewFleet()
	if _, err := budgets.Charge(t.Context(), coord.AgentScope(id.String()), 120, 10000, 500); err != nil {
		t.Fatalf("charge: %v", err)
	}

	r := registryOver(t, queries.Sources{
		State:   livestate.New(),
		Company: func() *config.Company { return cfg },
		Budget:  budgets,
	})
	got := ask(t, r, "budgets", nil)

	if got["durable"] != true {
		t.Error("durable = false with a readable counter, so every figure " +
			"below it reads as unmeasured")
	}
	orgRow, _ := got["org"].(map[string]any)
	if orgRow["max_tokens"] != 10000 || orgRow["durable_used"] != 120 {
		t.Errorf("org = %+v", orgRow)
	}
	if orgRow["durable_updated_at"] == "" {
		t.Error("no charge timestamp, so an operator cannot tell a live " +
			"counter from a stale one")
	}

	seats, _ := got["seats"].([]any)
	if len(seats) != 1 {
		t.Fatalf("seats = %d, want the one AGENT seat — a human spends "+
			"nothing and a permanent zero row is noise", len(seats))
	}
	seat, _ := seats[0].(map[string]any)
	if seat["role"] != "CEO" || seat["max_tokens"] != 500 || seat["durable_used"] != 120 {
		t.Errorf("seat = %+v", seat)
	}
}

func TestBudgetsWithNoStoreSayNobodyLooked(t *testing.T) {
	t.Parallel()
	// `durable: false` means UNREADABLE, never zero. A company drawn at 0%
	// of its budget when the truth is that nobody looked is the lie this
	// field exists to prevent.
	cfg := parsed(t, `
name: Acme
providers:
  llm:
    p: {type: anthropic, model: m, api_keys: ["${K}"]}
roles:
  - name: CEO
    handle: ceo
    llm: p
token_budget: 10000
`)
	r := registryOver(t, queries.Sources{
		State: livestate.New(), Company: func() *config.Company { return cfg },
	})
	got := ask(t, r, "budgets", nil)
	if got["durable"] != false {
		t.Error("a node with no counter claimed a durable reading")
	}
	// The CAPS are still stated: they are config, and a node without a
	// store still knows them.
	orgRow, _ := got["org"].(map[string]any)
	if orgRow["max_tokens"] != 10000 {
		t.Errorf("the cap was dropped with the counter: %+v", orgRow)
	}
}

func TestBudgetsCarryTheRefusalTheCounterRecorded(t *testing.T) {
	t.Parallel()
	// "Exhausted" is a refusal, never durable_used >= max_tokens: a refused
	// charge increments nothing, so a seat charged in rounds stalls short
	// of its cap and never reads as full. The stamp is what says the gate
	// is turning turns away, so the answer carries it for the scope that
	// refused and leaves it empty for the one that did not.
	cfg := parsed(t, `
name: Acme
providers:
  llm:
    p: {type: anthropic, model: m, api_keys: ["${K}"]}
roles:
  - name: CEO
    handle: ceo
    llm: p
    token_budget: 100
token_budget: 10000
`)
	organization, err := cfg.Organization()
	if err != nil {
		t.Fatalf("organization: %v", err)
	}
	id, _ := organization.AgentIDFor(organization.AgentSeatByHandle("ceo"))
	scope := coord.AgentScope(id.String())
	budgets := coordmemory.NewFleet()
	if _, err := budgets.Charge(t.Context(), scope, 90, 10000, 100); err != nil {
		t.Fatalf("charge: %v", err)
	}
	refusal, err := budgets.Charge(t.Context(), scope, 20, 10000, 100)
	if err != nil || refusal.RefusedScope != "agent" {
		t.Fatalf("setup: refusal = (%+v, %v), want the seat to refuse", refusal, err)
	}

	r := registryOver(t, queries.Sources{
		State:   livestate.New(),
		Company: func() *config.Company { return cfg },
		Budget:  budgets,
	})
	got := ask(t, r, "budgets", nil)

	seats, _ := got["seats"].([]any)
	if len(seats) != 1 {
		t.Fatalf("seats = %d", len(seats))
	}
	seat, _ := seats[0].(map[string]any)
	stamp, _ := seat["refused_at"].(string)
	if _, err := time.Parse(time.RFC3339Nano, stamp); err != nil {
		t.Errorf("seat refused_at = %q, want the refusal's instant: %v", stamp, err)
	}
	if seat["durable_used"] != 90 {
		t.Errorf("seat durable_used = %v, want the 90 that fit", seat["durable_used"])
	}
	orgRow, _ := got["org"].(map[string]any)
	if orgRow["refused_at"] != "" {
		t.Errorf("org refused_at = %v, want empty: the company refused nothing", orgRow["refused_at"])
	}
	// The retired per-process figure is gone rather than null.
	for _, row := range []map[string]any{seat, orgRow} {
		if _, present := row["live_used"]; present {
			t.Errorf("row still carries live_used: %+v", row)
		}
	}
}

// parsed builds a company from a YAML fixture.
func parsed(t *testing.T, doc string) *config.Company {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return cfg
}

// A SEAT'S ANSWER CARRIES ITS FINISHED CALLS, not only the one in flight.
//
// The live overlay and the history are two different sources and always were:
// the projection holds the call happening now, the event store holds the ones
// that completed. The answer carried only the first, so a seat page rendered
// the round in progress and nothing before it — while the spend chart beside
// it, which reads the same store, reported every phase the turn had already
// finished. Two panels on one screen disagreeing about whether a seat had done
// anything.
func TestAnAgentAnswerCarriesItsFinishedCalls(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	log := db.Events()
	seedPhases(t, log, "Lead", "agent-lead")

	r := registryOver(t, queries.Sources{State: livestate.New(), Events: log})
	got := ask(t, r, "agent", map[string]any{"role": "Lead"})

	history, _ := got["llm_history"].([]store.EventRecord)
	if len(history) != 2 {
		t.Fatalf("llm_history = %v, want both finished phases", got["llm_history"])
	}
	// EVENT RECORDS, payload nested — the same shape `event`, `trace` and
	// `turn` answer with. They used to be the payload flattened with an id
	// and a timestamp merged in, so one client carried two readers for one
	// kind of thing and a field added to the envelope reached three screens
	// and not the fourth.
	if len(history[0].Payload) == 0 {
		t.Errorf("history row carries no payload: %+v", history[0])
	}
	var body map[string]any
	if err := json.Unmarshal(history[0].Payload, &body); err != nil {
		t.Fatalf("history payload: %v", err)
	}
	for _, key := range []string{"turn_id", "phase", "model", "total_tokens", "response"} {
		if _, present := body[key]; !present {
			t.Errorf("history payload has no %q: %v", key, body)
		}
	}
	if history[0].Time.IsZero() {
		t.Errorf("history row has no timestamp: %+v", history[0])
	}
	// Newest first, which is the order the list renders in.
	if body["phase"] != "execute" {
		t.Errorf("first row is %v, want the newest", body["phase"])
	}
	// A page shorter than the cap is the end of the record, and says so by
	// offering no cursor — a client that got one would page forever.
	if got["next"] != "" {
		t.Errorf("next = %v, want no cursor on a short page", got["next"])
	}
}

// A SEAT ADDRESSED BY HANDLE FINDS THE SAME HISTORY. The dashboard holds the
// handle; the store rows are keyed by the derived agent id and the role.
func TestAgentHistoryResolvesFromTheHandle(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	log := db.Events()
	seedPhases(t, log, "Lead", "agent-lead")

	r := registryOver(t, queries.Sources{State: livestate.New(), Events: log})
	got := ask(t, r, "agent", map[string]any{"role": "Lead"})
	if history, _ := got["llm_history"].([]store.EventRecord); len(history) != 2 {
		t.Fatalf("asking by role found %v", got["llm_history"])
	}
}

// AN UNREADABLE OR ABSENT LOG COSTS THE HISTORY AND NOTHING ELSE. The live
// half is the answer's point and comes from memory; refusing the seat page
// because the event log is missing would turn a degraded panel into no screen.
func TestAnAgentAnswerSurvivesWithNoEventLog(t *testing.T) {
	t.Parallel()
	r := registryOver(t, queries.Sources{State: livestate.New()})
	got := ask(t, r, "agent", map[string]any{"role": "Lead"})

	if got["role"] != "Lead" {
		t.Fatalf("answer = %v", got)
	}
	history, ok := got["llm_history"].([]store.EventRecord)
	if !ok || len(history) != 0 {
		t.Errorf("llm_history = %v, want an empty list rather than a missing key",
			got["llm_history"])
	}
}

// seedPhases writes two finished phases for one seat, plus one for another so
// the filter has something to exclude.
func seedPhases(t *testing.T, log *store.EventLog, role, agentID string) {
	t.Helper()
	base := time.Now().UTC().Add(-time.Hour)
	rows := []struct {
		id, phase, role, agent string
		at                     time.Time
	}{
		{"p1", "plan", role, agentID, base},
		{"p2", "execute", role, agentID, base.Add(time.Second)},
		{"p3", "plan", "Someone Else", "agent-other", base.Add(2 * time.Second)},
	}
	for _, row := range rows {
		payload := `{"turn_id":"t-1","phase":"` + row.phase + `","iteration":1,` +
			`"model":"claude-sonnet-5","total_tokens":10,"response":"ok"}`
		if err := log.Append(t.Context(), store.EventRecord{
			ID:      row.id,
			Type:    "agent_phase_completed",
			Source:  "engine",
			Time:    row.at,
			Actor:   row.role,
			Tags:    map[string]string{"agent_role": row.role, "agent_id": row.agent},
			Payload: json.RawMessage(payload),
		}); err != nil {
			t.Fatalf("append %s: %v", row.id, err)
		}
	}
}

// A DEAD LINK IS NOT A BROKEN NODE.
//
// `EventLog.ByID` answers `store.ErrNotFound`, which is not `queries.ErrNotFound`
// — so an id the log simply does not hold used to reach the API classifier as a
// plain failure and come back as `query_failed`. That is the one classification
// an operator acts on by going to look for a fault, and there is none: every
// event id on the dashboard is a link somebody can follow after the 30-day
// window has closed over it.
func TestAMissingEventIsNotFoundRatherThanAFailure(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	seedEvents(t, db.Events(), 1, nil)
	r := registryOver(t, queries.Sources{Events: db.Events()})

	_, err := r.Answer(t.Context(), "event", map[string]any{"id": "ev-nobody-published"}, "")
	if !errors.Is(err, queries.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	// The id is IN the message, because the surface that renders this shows it
	// to a person who pasted or followed it.
	if !strings.Contains(err.Error(), "ev-nobody-published") {
		t.Errorf("error %q does not name the id that was not found", err)
	}
}

// AN EMPTY LIST ANSWERS `[]`, NEVER `null`.
//
// The two differ in exactly one place — the JSON — and it is the place every
// one of these answers ends up: a client reading `.events.length` off `null`
// crashes for "nothing matched" and works for everything else, which is how
// the Trace screen came to report "not found" for an empty trace. Asserted on
// the marshalled bytes rather than on the Go value, because a non-nil empty
// slice is the only thing that fixes it and `len(rows) == 0` cannot tell the
// two apart.
func TestAnEmptyTraceAnswersAnArrayNotNull(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	seedEvents(t, db.Events(), 1, nil)
	r := registryOver(t, queries.Sources{Events: db.Events()})

	for _, tc := range []struct {
		what  string
		param map[string]any
	}{
		{"trace", map[string]any{"trace_id": "tr-nothing-shares-this"}},
		{"turn", map[string]any{"turn_id": "turn-nothing-shares-this"}},
	} {
		got := ask(t, r, tc.what, tc.param)
		raw, err := json.Marshal(got["events"])
		if err != nil {
			t.Fatalf("%s: marshal: %v", tc.what, err)
		}
		if string(raw) != "[]" {
			t.Errorf("%s: events marshalled as %s, want []", tc.what, raw)
		}
	}
}

// AN EMPTY HISTORY IS NOT A PANIC.
//
// `phaseHistory` echoes the last row's key as the paging cursor, and it used to
// reach for that row behind a nil check — which is not the same question. The
// event log answers a nil slice for a seat it cannot name and an allocated
// empty one for a seat that simply has not run yet, and `records[len-1]` is out
// of range on both: the second walked straight past the guard and took the
// whole seat screen down with a 500, on the one seat state every new company
// starts in.
func TestASeatWithNoFinishedPhasesAnswersRatherThanPanics(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	// Seeded, so the log is readable and non-empty — the empty result has to
	// come from this seat having no phases, not from an empty table.
	seedEvents(t, db.Events(), 3, nil)
	r := registryOver(t, queries.Sources{State: livestate.New(), Events: db.Events()})

	got := ask(t, r, "agent", map[string]any{"role": "NobodyHasThisRole"})
	rows, ok := got["llm_history"].([]store.EventRecord)
	if !ok {
		t.Fatalf("llm_history = %#v, want a slice", got["llm_history"])
	}
	if len(rows) != 0 {
		t.Errorf("llm_history = %d rows, want none for a role nothing published under", len(rows))
	}
	// And the cursor is withheld rather than pointing at a row that is not
	// there, which is what makes a client stop paging.
	if got["next"] != "" {
		t.Errorf("next = %#v on an empty page, want the empty string", got["next"])
	}
}

// windowWidth is how long a rollup says it covers.
func windowWidth(t *testing.T, got tokens.Rollup) time.Duration {
	t.Helper()
	since, err := time.Parse(time.RFC3339, got.Since)
	if err != nil {
		t.Fatalf("since = %q: %v", got.Since, err)
	}
	until, err := time.Parse(time.RFC3339, got.Until)
	if err != nil {
		t.Fatalf("until = %q: %v", got.Until, err)
	}
	return until.Sub(since)
}

func TestTokensTakeTheSameTwoInstantsTheSeriesDoes(t *testing.T) {
	t.Parallel()
	// THE WHOLE POINT OF THE WINDOW BEING INSTANTS. A time-range control
	// produces two edges, and a reader who names one that ended yesterday
	// gets a chart over it and figures above the chart over this afternoon
	// — two facts on one screen that cannot be compared — unless the
	// breakdown takes the same pair.
	db := openStore(t)
	log := db.Events()
	at := time.Now().UTC().Add(-5 * 24 * time.Hour)
	write := func(id string, when time.Time, total int) {
		payload, _ := json.Marshal(map[string]any{
			"role": "Lead", "phase": "plan", "total_tokens": total, "turn_id": "tn-1",
		})
		if err := log.Append(t.Context(), store.EventRecord{
			ID: id, Type: "agent_phase_completed", Time: when,
			Category: "system", Payload: payload,
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	write("old", at.Add(-48*time.Hour), 100)
	write("in", at.Add(time.Hour), 7)
	write("new", time.Now().UTC(), 500)

	r := registryOver(t, queries.Sources{State: livestate.New(), Events: log})
	got := askRaw(t, r, "tokens", map[string]any{
		"since": at.Format(time.RFC3339),
		"until": at.Add(24 * time.Hour).Format(time.RFC3339),
	}).(tokens.Rollup)

	if got.Totals.TotalTokens != 7 {
		t.Errorf("totals = %+v, want only the record inside the named window", got.Totals)
	}
	if width := windowWidth(t, got); width != 24*time.Hour {
		t.Errorf("window = %s .. %s (%s), want the day that was named",
			got.Since, got.Until, width)
	}
}

func TestATokensWindowThatEndsWhereItBeginsIsRefused(t *testing.T) {
	t.Parallel()
	// Half-open, so an empty window names no rows at all. The same refusal
	// the events and series questions give, in the same words, because a
	// reader scrubbing a range hits all three.
	r := registryOver(t, queries.Sources{State: livestate.New()})
	at := time.Now().UTC().Format(time.RFC3339)
	_, err := r.Answer(t.Context(), "tokens", map[string]any{"since": at, "until": at}, "")
	if !errors.Is(err, queries.ErrBadParams) {
		t.Fatalf("err = %v, want ErrBadParams", err)
	}
}
