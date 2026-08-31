package queries_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/store"
)

// stubSearcher is a knowledge backend that is wired but has nothing to search,
// which is a real state: an operator who configured the integration and no
// spaces.
type stubSearcher struct{}

func (stubSearcher) Backend() string                                         { return "stub" }
func (stubSearcher) CanSearch(*org.Role, *org.Organization) bool             { return false }
func (stubSearcher) Search(context.Context, knowledge.Query) []knowledge.Hit { return nil }

// A TURN IS ITS OWN QUESTION, and it is not a slice of the trace.
//
// One trace can span several turns — a webhook that wakes two seats — and a
// turn resumed on another node after a restart can span several traces. Until
// `turn_id` was promoted to an indexed column (migration 0014) neither could be
// asked, so "show me everything that happened in this unit of work" had no
// answer at all: a long self-iterating turn pushes its own earlier phases out
// of the seat's window and out of the feed, which is exactly the turn worth
// reading.
func TestTurnAnswersEveryEventOfOneUnitOfWork(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	log := db.Events()
	base := time.Now().UTC().Add(-time.Minute)

	write := func(id, kind, turn string, at time.Time) {
		t.Helper()
		payload, err := json.Marshal(map[string]any{"turn_id": turn, "phase": "plan"})
		if err != nil {
			t.Fatal(err)
		}
		if err := log.Append(t.Context(), store.EventRecord{
			ID: id, Type: kind, Time: at, Category: "lifecycle", Actor: "PM",
			Tags: map[string]string{"turn_id": turn, "agent_role": "PM"},
			// The tag is what promotes the column; the payload is what a
			// reader renders.
			Payload: payload,
		}); err != nil {
			t.Fatal(err)
		}
	}
	write("a", "agent_phase_completed", "t-1", base)
	write("b", "provider_fallback", "t-1", base.Add(time.Second))
	write("c", "agent_turn_completed", "t-1", base.Add(2*time.Second))
	write("d", "agent_phase_completed", "t-2", base.Add(3*time.Second))

	// Read through JSON, which is what a client actually sees.
	got := asMap(t, answer(t, queries.Sources{Events: log}, "turn", map[string]any{"turn_id": "t-1"}))

	events := rows(t, got["events"])
	if len(events) != 3 {
		t.Fatalf("%d events for t-1, want 3 (the other turn's must not be here): %v",
			len(events), got["events"])
	}
	// OLDEST FIRST: a turn is read forwards — plan, then execute, then review
	// — which is the opposite of a feed.
	if events[0]["id"] != "a" || events[2]["id"] != "c" {
		t.Errorf("turn is not oldest-first: %v %v %v",
			events[0]["id"], events[1]["id"], events[2]["id"])
	}
	// The payload rides along, unlike a listing: a turn is a handful of events
	// and the caller renders all of them, so making it re-fetch each one would
	// be an N+1 over a set the query already had in hand.
	if events[0]["payload"] == nil {
		t.Errorf("turn events carry no payload: %v", events[0])
	}
	if got["turn_id"] != "t-1" {
		t.Errorf("answer does not name its turn: %v", got)
	}
}

// A TURN NOBODY RECORDED IS AN EMPTY LIST, not a null.
//
// A nil slice marshals as `null` and every consumer reads `.length` off it —
// the exact shape mismatch that made the Trace screen report "not found" for
// every trace it was given.
func TestAnUnknownTurnIsAnEmptyList(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	got := asMap(t, answer(t, queries.Sources{Events: db.Events()}, "turn",
		map[string]any{"turn_id": "nobody"}))
	events, ok := got["events"].([]any)
	if !ok {
		// `null` is the failure this guards: every consumer reads `.length`
		// off it, which is the shape mismatch that made the Trace screen
		// report "not found" for every trace it was given.
		t.Fatalf("events = %#v, want an empty list rather than null", got["events"])
	}
	if len(events) != 0 {
		t.Errorf("%d events for an unknown turn", len(events))
	}
}

// PHASES CARRY THEIR PAYLOADS, which is the whole reason the question exists.
//
// `events?type=agent_phase_completed` is not a substitute: the event listing
// deliberately never selects the payload — a page of ordinary events with every
// payload attached is the query that makes an activity screen slow — and a
// phase record without one has no prompts, no response, no tool calls and no
// decision, which is everything a reader came for.
func TestPhasesCarryPayloadsAndPage(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	log := db.Events()
	base := time.Now().UTC().Add(-time.Hour)
	for i, role := range []string{"PM", "PM", "Engineer"} {
		payload, err := json.Marshal(map[string]any{
			"turn_id": "t", "phase": "plan", "response": "ok", "role": role,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := log.Append(t.Context(), store.EventRecord{
			ID: string(rune('a' + i)), Type: "agent_phase_completed",
			Time: base.Add(time.Duration(i) * time.Second), Category: "lifecycle",
			Actor: role, Tags: map[string]string{"agent_role": role},
			Payload: payload,
		}); err != nil {
			t.Fatal(err)
		}
	}

	got := asMap(t, answer(t, queries.Sources{Events: log}, "phases", map[string]any{"limit": 2}))
	phases := rows(t, got["phases"])
	if len(phases) != 2 {
		t.Fatalf("%d phases, want the requested 2", len(phases))
	}
	if phases[0]["payload"] == nil {
		t.Fatalf("a phase record with no payload has nothing a reader came for: %v", phases[0])
	}
	// A FULL page offers a cursor; a short one is the end of the record and
	// must not, or a client pages forever.
	next, _ := got["next"].(map[string]any)
	if next["before_id"] == nil || next["before_id"] == "" {
		t.Errorf("a full page offers no cursor: %v", got["next"])
	}
	if got["exhausted"] != false {
		t.Errorf("a full page claims to be exhausted: %v", got)
	}

	last := asMap(t, answer(t, queries.Sources{Events: log}, "phases", map[string]any{
		"limit":       2,
		"before_time": next["before_time"],
		"before_id":   next["before_id"],
	}))
	rest := rows(t, last["phases"])
	if len(rest) != 1 {
		t.Fatalf("the second page holds %d, want the remaining 1", len(rest))
	}
	if last["exhausted"] != true {
		t.Errorf("a short page does not report the end of the record: %v", last)
	}

	// The role filter narrows server-side, so a busy company's other seats are
	// never fetched and thrown away.
	mine := asMap(t, answer(t, queries.Sources{Events: log}, "phases",
		map[string]any{"role": "Engineer"}))
	if got := rows(t, mine["phases"]); len(got) != 1 {
		t.Errorf("role filter returned %d rows, want 1", len(got))
	}
}

// rows reads a list of records out of a JSON-decoded answer.
func rows(t *testing.T, value any) []map[string]any {
	t.Helper()
	list, ok := value.([]any)
	if !ok {
		t.Fatalf("%#v is not a list", value)
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		row, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("%#v is not a record", item)
		}
		out = append(out, row)
	}
	return out
}

// A CURSOR WITHOUT ITS TIMESTAMP IS REFUSED rather than silently ignored.
//
// The pair IS the key. A client sending half of it and getting the newest page
// back would page the same rows forever and read that as the end of history —
// which is what the Activity screen did, for every cursored branch it ever
// requested.
func TestAHalfCursorIsRefused(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	r := registryOver(t, queries.Sources{Events: db.Events()})
	if _, err := r.Answer(t.Context(), "phases", map[string]any{"before_id": "x"}, ""); err == nil {
		t.Fatal("a before_id with no before_time was accepted")
	}
}

// KNOWLEDGE SEARCH SAYS WHY IT FOUND NOTHING.
//
// Search is BEST EFFORT by contract — every failure path is an empty result, so
// a turn never dies because a wiki was slow — which means an empty answer here
// is not proof that nothing matches. The screen has to be able to tell the two
// apart, so the answer carries `available` and a `note`.
func TestKnowledgeSaysWhenThereIsNoBackend(t *testing.T) {
	t.Parallel()

	// A COMPANY WITH NO SEARCHER still answers, and says why. Gated on the
	// company rather than on the searcher, for the same reason `budgets` is:
	// exactly one backend serves a company, chosen by which integration is
	// configured, so "none is" is a fact the company establishes on its own —
	// and it is a far more useful answer than an unknown query.
	none := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return &config.Company{Name: "Acme"} },
	}, "knowledge", map[string]any{"q": "anything"}))
	if none["available"] != false {
		t.Errorf("no backend, yet the search reports itself available: %v", none)
	}
	if none["note"] == "" {
		t.Errorf("an unavailable search gives no reason: %v", none)
	}
	if hits, ok := none["hits"].([]any); !ok || len(hits) != 0 {
		t.Errorf("hits = %#v, want an empty list", none["hits"])
	}

	// A searcher that is wired but cannot search — an operator who configured
	// the integration and no spaces — is a DIFFERENT state, and says so
	// rather than answering an empty search as though it had run.
	gated := asMap(t, answer(t, queries.Sources{
		Knowledge: stubSearcher{},
		Company:   func() *config.Company { return &config.Company{Name: "Acme"} },
	}, "knowledge", map[string]any{"q": "anything"}))
	if gated["available"] != false || gated["note"] == "" {
		t.Errorf("a gated search does not explain itself: %v", gated)
	}
	// And it must not BORROW the no-backend wording. Asserting only that a
	// note exists cannot catch the two states sharing one string, which is
	// exactly the bug: it sends an operator whose integration is fine to go
	// and re-check that integration.
	if gated["note"] == none["note"] {
		t.Errorf("a wired-but-unscoped backend reports itself as no backend at all: %q", gated["note"])
	}
	if !strings.Contains(gated["note"].(string), "confluence_spaces") {
		t.Errorf("the gated note does not name the field to fix: %q", gated["note"])
	}

	// And with no company at all, the answer names THAT rather than blaming
	// the backend.
	unconfigured := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return nil },
	}, "knowledge", map[string]any{"q": "anything"}))
	if unconfigured["available"] != false {
		t.Errorf("no company, yet the search reports itself available: %v", unconfigured)
	}

	// The three states must be told apart by a VALUE, not by the prose. A
	// screen picking which remedy to offer branches on this, so two states
	// sharing a reason offers one of them the wrong fix — and every reason
	// this package emits has to be one the enum admits.
	for name, got := range map[string]map[string]any{
		"no company": unconfigured, "no backend": none, "no scope": gated,
	} {
		reason, _ := got["reason"].(string)
		if !queries.KnowledgeReason(reason).Valid() {
			t.Errorf("%s reports a reason the enum does not admit: %q", name, reason)
		}
		if queries.KnowledgeReason(reason) == queries.KnowledgeRan {
			t.Errorf("%s reports the search as having run: %v", name, got)
		}
	}
	if none["reason"] == gated["reason"] {
		t.Errorf("no backend and an unscoped backend share one reason: %q", none["reason"])
	}
	if unconfigured["reason"] == none["reason"] {
		t.Errorf("no company and no backend share one reason: %q", none["reason"])
	}

}
