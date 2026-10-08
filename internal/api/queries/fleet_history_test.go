package queries_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/store"
)

// twoNodes is a fleet of two stores on one broker, read from the first: every
// history answer here is assembled from both, as a real fleet's is.
func twoNodes(t *testing.T) (fleet *eventfan.Fleet, a, b *store.EventLog) {
	t.Helper()
	broker := memory.NewBroker()
	start := func() queue.EventQueue {
		q := broker.Client()
		if err := q.Start(t.Context()); err != nil {
			t.Fatalf("start a queue client: %v", err)
		}
		t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
		return q
	}
	qa, qb := start(), start()
	a, b = openStore(t).Events(), openStore(t).Events()
	stop, err := eventfan.Serve(t.Context(), qb, "node-b", b)
	if err != nil {
		t.Fatalf("serve node-b: %v", err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })
	return &eventfan.Fleet{
		Self: "node-a", Local: a, Queue: qa,
		Roster: func(context.Context) ([]string, error) {
			return []string{"node-a", "node-b"}, nil
		},
		Budget: 5 * time.Second,
	}, a, b
}

// EVERY HISTORY READ GOES THROUGH THE FLEET.
//
// The decision ADR-0021 records: turn-level detail is on the node that
// published it and nowhere else, so every question that reads it is asked of
// every node and says which answered. Here the rows are split across two
// stores and each question is asked once, through the registry a client
// reaches — and each answer must hold BOTH nodes' rows and name both nodes.
//
// Half of the enforcement is this test and the other half is the compiler:
// [queries.FleetEvents] answers a coverage from every method, so a bare
// *store.EventLog — one node's store, read as though it were the company's —
// does not fit in [queries.Sources] at all.
//
// Mutation: answer any of these from the asker's store alone and it fails.
func TestEventReadsGoThroughTheFleet(t *testing.T) {
	t.Parallel()
	fleet, a, b := twoNodes(t)
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	put := func(log *store.EventLog, id, kind string, at time.Time) {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"turn_id": "t-1"})
		if err := log.Append(t.Context(), store.EventRecord{
			ID: id, Type: kind, Time: at, Category: "task", TraceID: "tr-1",
			Tags:    map[string]string{"turn_id": "t-1", "agent_role": "Lead"},
			Payload: payload,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// ONE TURN, PARKED ON node-a AND FINISHED ON node-b after a move.
	put(a, "on-a", "agent_phase_completed", at)
	put(b, "on-b", "agent_phase_completed", at.Add(time.Minute))
	put(b, "done-b", "turn_completed", at.Add(2*time.Minute))
	r := registryOver(t, queries.Sources{Events: fleet})

	both := func(what string, got any, ids ...string) {
		t.Helper()
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Coverage eventfan.Coverage `json:"coverage"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		if !body.Coverage.Complete || len(body.Coverage.Nodes) != 2 {
			t.Errorf("%s: coverage %+v, want both nodes, complete", what, body.Coverage)
		}
		for _, id := range ids {
			if !json.Valid(raw) || !containsID(raw, id) {
				t.Errorf("%s holds no %s — that row is on the other node", what, id)
			}
		}
	}
	both("events", askRaw(t, r, "events", nil), "on-a", "on-b")
	both("trace", askRaw(t, r, "trace", map[string]any{"trace_id": "tr-1"}), "on-a", "on-b")
	both("turn", askRaw(t, r, "turn", map[string]any{"turn_id": "t-1"}), "on-a", "done-b")
	both("phases", askRaw(t, r, "phases", nil), "on-a", "on-b")
	both("event", askRaw(t, r, "event", map[string]any{"id": "done-b"}), "done-b")
	series := askRaw(t, r, "event_series", map[string]any{"bucket": "day"}).(queries.SeriesAnswer)
	if series.Total != 3 {
		t.Errorf("event_series totals %d, want all 3 rows across both nodes", series.Total)
	}
	both("event_series", series)

	turns := askRaw(t, r, "turns", nil)
	both("turns", turns)
	rows, _ := turns.(map[string]any)["turns"].([]store.Turn)
	if len(rows) != 1 || rows[0].Phases != 2 || !rows[0].Complete {
		t.Errorf("turns = %+v, want ONE row holding both halves, complete", rows)
	}
}

// THE INTEGRATIONS' COUNTS ARE THE FLEET'S, deliveries and outcomes alike. A
// delivery is stored on the node the load balancer handed it to, and the drop
// it became on the node that routed it — here node-a and node-b — so a row
// read from one store said a delivery arrived and nothing became of it.
//
// Mutation: count the outcomes from the asker's store alone, and gitlab's drop
// is missing.
func TestIntegrationCountsAreTheFleets(t *testing.T) {
	t.Parallel()
	fleet, a, b := twoNodes(t)
	at := time.Now().UTC().Add(-time.Hour)
	if err := a.Append(t.Context(), store.EventRecord{ID: "delivered", Type: "webhook:push",
		Source: "gitlab", Category: "webhook", Tags: map[string]string{"route": "gitlab"}, Summary: "push", Time: at}); err != nil {
		t.Fatal(err)
	}
	if err := b.Append(t.Context(), store.EventRecord{ID: "dropped", Type: "notification_skipped",
		Source: "engine", Category: "notification", Summary: "skipped", Time: at.Add(time.Second),
		Tags: map[string]string{"notification_source": "gitlab"}}); err != nil {
		t.Fatal(err)
	}
	cfg := company(t)
	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg }, Events: fleet,
	}, "integrations", nil))
	gitlab := surfacesOf(t, body)["gitlab"]
	if gitlab["inbound"] != float64(1) || gitlab["skipped"] != float64(1) {
		t.Errorf("gitlab inbound %v, skipped %v — want node-a's delivery and node-b's drop",
			gitlab["inbound"], gitlab["skipped"])
	}
	raw, err := json.Marshal(body["coverage"])
	if err != nil {
		t.Fatal(err)
	}
	var coverage eventfan.Coverage
	if err := json.Unmarshal(raw, &coverage); err != nil {
		t.Fatal(err)
	}
	if !coverage.Complete || len(coverage.Nodes) != 2 {
		t.Errorf("coverage %+v, want both nodes, complete", coverage)
	}
}

// A PAGE OF EXACTLY THE PAGE'S SIZE CAN HOLD EVERY DELIVERY, and its window is
// then the whole history.
//
// The window turns on whether the fleet may hold deliveries past the page,
// never on the page's length: two nodes holding half a page each fill neither
// node's own page, so the merged page — exactly [queries.MaxEventPage] long —
// is every delivery the fleet has. The window is the month, and a drop older
// than the oldest delivery is counted in it.
//
// Mutation: read a page of exactly the page's size as capped, and the window
// starts a microsecond past the oldest delivery — which goes uncounted, with
// the drop before it.
func TestAFullPageFromTwoNodesIsTheWholeHistory(t *testing.T) {
	t.Parallel()
	fleet, a, b := twoNodes(t)
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	fleet.Clock = func() time.Time { return at }
	oldest := at
	for i := range queries.MaxEventPage {
		log := a
		if i%2 == 1 {
			log = b
		}
		oldest = at.Add(-time.Duration(i+1) * time.Minute)
		if err := log.Append(t.Context(), store.EventRecord{ID: fmt.Sprintf("w%03d", i),
			Type: "webhook:push", Source: "gitlab", Category: "webhook", Tags: map[string]string{"route": "gitlab"}, Summary: "push",
			Time: oldest}); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Append(t.Context(), store.EventRecord{ID: "dropped-early", Type: "notification_skipped",
		Source: "engine", Category: "notification", Summary: "skipped", Time: oldest.Add(-time.Hour),
		Tags: map[string]string{"notification_source": "gitlab"}}); err != nil {
		t.Fatal(err)
	}
	cfg := company(t)
	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg }, Events: fleet,
	}, "integrations", nil))
	if got, want := body["traffic_since"], at.Add(-store.EventHistory).Format(time.RFC3339Nano); got != want {
		t.Errorf("traffic_since = %v, want the history floor, %s — the page held every delivery", got, want)
	}
	gitlab := surfacesOf(t, body)["gitlab"]
	if gitlab["inbound"] != float64(queries.MaxEventPage) || gitlab["skipped"] != float64(1) {
		t.Errorf("gitlab inbound %v, skipped %v — want all %d deliveries and the drop before "+
			"the oldest of them", gitlab["inbound"], gitlab["skipped"], queries.MaxEventPage)
	}
}

func containsID(raw []byte, id string) bool {
	return slices.Contains(idsIn(raw), id)
}

// idsIn collects every `"id"` value anywhere in a JSON document.
func idsIn(raw []byte) []string {
	var doc any
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if id, ok := x["id"].(string); ok {
				out = append(out, id)
			}
			for _, child := range x {
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(doc)
	return out
}

// THE ORDER IS A CLOSED SET, and a ranking takes no cursor.
func TestTurnsRefusesAnUnknownSortAndACursorOnARanking(t *testing.T) {
	t.Parallel()
	r := registryOver(t, queries.Sources{Events: fleetOf(openStore(t).Events())})
	for _, params := range []map[string]any{
		{"sort": "-cost"},
		// A CURSOR AS A PAGE HANDS IT OUT, so the refusal is the ranking's.
		{"sort": "-tokens", "before": base64.RawURLEncoding.EncodeToString(
			[]byte("2026-09-01T00:00:00Z t-1"))},
	} {
		if _, err := r.Answer(t.Context(), "turns", params, ""); !errors.Is(err, queries.ErrBadParams) {
			t.Errorf("turns %v answered %v, want %v", params, err, queries.ErrBadParams)
		}
	}
	if _, err := r.Answer(t.Context(), "turns", map[string]any{"sort": "-tokens"}, ""); err != nil {
		t.Errorf("a ranked page was refused: %v", err)
	}
}

// THE TURNS LIST TAKES A WINDOW OF TWO INSTANTS, which is what a bar picked on
// its axis IS — and refuses a window named twice or backwards rather than
// silently answering one of them.
func TestTurnsAnswersAWindowInThePast(t *testing.T) {
	t.Parallel()
	log := openStore(t).Events()
	bar := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Hour)
	for id, at := range map[string]time.Time{
		"in-the-bar": bar.Add(10 * time.Minute),
		"today":      time.Now().UTC().Add(-time.Minute),
	} {
		if err := log.Append(t.Context(), store.EventRecord{
			ID: id + "-p0", Type: "agent_phase_completed", Time: at, Category: "lifecycle",
			Tags: map[string]string{"turn_id": id, "agent_role": "PM"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	r := registryOver(t, queries.Sources{Events: fleetOf(log)})
	got, err := r.Answer(t.Context(), "turns", map[string]any{
		"since": bar.Format(time.RFC3339), "until": bar.Add(time.Hour).Format(time.RFC3339),
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	turns, _ := got.(map[string]any)["turns"].([]store.Turn)
	if len(turns) != 1 || turns[0].TurnID != "in-the-bar" {
		t.Fatalf("the bar three days ago listed %+v, want its own turn", turns)
	}
	for _, params := range []map[string]any{
		{"days": 7, "since": bar.Format(time.RFC3339)},
		{"since": bar.Format(time.RFC3339), "until": bar.Format(time.RFC3339)},
		{"until": "yesterday"},
	} {
		if _, err := r.Answer(t.Context(), "turns", params, ""); !errors.Is(err, queries.ErrBadParams) {
			t.Errorf("turns %v answered %v, want %v", params, err, queries.ErrBadParams)
		}
	}
}
