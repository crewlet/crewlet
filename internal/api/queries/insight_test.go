package queries_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
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

func (stubSearcher) Backend() string                             { return "stub" }
func (stubSearcher) CanSearch(*org.Role, *org.Organization) bool { return false }
func (stubSearcher) Building(context.Context) bool               { return false }
func (stubSearcher) Search(context.Context, knowledge.Query) knowledge.Answer {
	return knowledge.Answer{}
}

// served wires a searcher this node serves.
func served(s knowledge.Searcher) func() (knowledge.Searcher, knowledge.Refusal) {
	return func() (knowledge.Searcher, knowledge.Refusal) { return s, knowledge.Refusal{} }
}

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
	// OLDEST FIRST: a turn is read forwards, the executor and then the
	// reviewer round by round, which is the opposite of a feed.
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
	// A SHORT TURN IS NOT A CUT ONE. The flag has to be present and false,
	// or a client cannot tell "read to the end" from a build that predates
	// the field — and would have to guess, which is what it was doing.
	if got["truncated"] != false {
		t.Errorf("truncated = %#v on a three-event turn, want an explicit false",
			got["truncated"])
	}
}

// A TURN READ SHORT SAYS SO — and here that matters more than it does on a
// trace, because of WHICH rows go missing.
//
// EventLog.Turn orders oldest first and stops at store.MaxTurnEvents, so the
// events a long turn loses are its ENDING: `agent_turn_completed` and
// `turn_completed`, which are the two records the Turn screen reads its
// outcome, its wall clock and its plan summary off. With no flag, a turn cut
// at the cap is indistinguishable from a turn that never finished: the screen
// printed "no turn record" directly above the rows it did get, and fell back
// to an event span captioned as the turn's own measurement.
func TestATurnReadToItsCapSaysItWasCut(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	log := db.Events()
	base := time.Now().UTC().Add(-time.Hour)

	payload, err := json.Marshal(map[string]any{"turn_id": "long", "phase": "execute"})
	if err != nil {
		t.Fatal(err)
	}
	// Well past the cap, so the read stops at it rather than at the end of
	// the turn — which is the only way to reach the flag's true branch — and
	// far enough past that the recovered ending cannot overlap the opening.
	const extra = 60
	for i := range store.MaxTurnEvents + extra {
		if err := log.Append(t.Context(), store.EventRecord{
			ID:   fmt.Sprintf("e-%04d", i),
			Type: "agent_phase_completed", Time: base.Add(time.Duration(i) * time.Second),
			Category: "lifecycle", Actor: "PM", Payload: payload,
		}); err != nil {
			t.Fatal(err)
		}
	}

	got := asMap(t, answer(t, queries.Sources{Events: log}, "turn",
		map[string]any{"turn_id": "long"}))
	if got["truncated"] != true {
		t.Errorf("truncated = %#v on a turn read to its cap; a reader has no "+
			"way to tell the missing ending from a turn that never ended",
			got["truncated"])
	}
	// THE OPENING PLUS THE ENDING. The head read is the cap; the closing rows
	// are recovered beside it, so the answer is longer than the cap by the
	// rows a reader could not otherwise have.
	events := rows(t, got["events"])
	if n := len(events); n <= store.MaxTurnEvents {
		t.Fatalf("%d events, want the cap %d plus the recovered ending",
			n, store.MaxTurnEvents)
	}
	if n := len(events); n > store.MaxTurnEvents+queries.TurnClosingEvents {
		t.Errorf("%d events, past the cap plus %d closing rows",
			n, queries.TurnClosingEvents)
	}
	// The LAST row of the turn is in the answer — which is the whole point,
	// because on a real turn it is `turn_completed`.
	last := fmt.Sprintf("e-%04d", store.MaxTurnEvents+extra-1)
	if events[len(events)-1]["id"] != last {
		t.Errorf("the answer ends at %v, want the turn's own last row %q — "+
			"the record the header reads its outcome off is exactly what a "+
			"head-only read drops", events[len(events)-1]["id"], last)
	}
	// ONE COPY OF EACH. The two reads come from opposite ends of one index,
	// so a turn that only just reached the cap has them overlapping.
	seen := map[any]bool{}
	for _, e := range events {
		if seen[e["id"]] {
			t.Fatalf("event %v is in the answer twice; a phase card renders "+
				"twice and the token totals double", e["id"])
		}
		seen[e["id"]] = true
	}
	// AND THE SEQUENCE HOLDS ACROSS THE GAP. A turn is read forwards, so the
	// recovered ending goes after the opening rather than beside it.
	for i := 1; i < len(events); i++ {
		if fmt.Sprint(events[i-1]["timestamp"]) > fmt.Sprint(events[i]["timestamp"]) {
			t.Fatalf("row %d is older than the one before it; the merge lost "+
				"the order a turn is read in", i)
		}
	}
}

// A TURN THAT FITS IS NOT MERGED WITH ITSELF.
//
// The closing read comes from the other end of the same index, so on a turn
// at or just under the cap it returns rows the head read already has.
// Concatenating would render those phase cards twice and double their tokens.
func TestATurnAtTheCapIsNotDoubled(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	log := db.Events()
	base := time.Now().UTC().Add(-time.Hour)

	payload, err := json.Marshal(map[string]any{"turn_id": "exact", "phase": "execute"})
	if err != nil {
		t.Fatal(err)
	}
	// EXACTLY the cap: the head read fills, so the answer reports truncated
	// and asks for a closing read whose every row the head already holds.
	for i := range store.MaxTurnEvents {
		if err := log.Append(t.Context(), store.EventRecord{
			ID:   fmt.Sprintf("x-%04d", i),
			Type: "agent_phase_completed", Time: base.Add(time.Duration(i) * time.Second),
			Category: "lifecycle", Actor: "PM", Payload: payload,
		}); err != nil {
			t.Fatal(err)
		}
	}

	got := asMap(t, answer(t, queries.Sources{Events: log}, "turn",
		map[string]any{"turn_id": "exact"}))
	if n := len(rows(t, got["events"])); n != store.MaxTurnEvents {
		t.Errorf("%d events for a turn of exactly %d; the closing read was "+
			"concatenated rather than merged", n, store.MaxTurnEvents)
	}
	// AND IT IS NOT REPORTED CUT. `len(rows) == cap` is the inference this
	// replaces, and this is the boundary it gets wrong: every row of the turn
	// is on the page, under a banner saying part of it is missing.
	if got["truncated"] != false {
		t.Errorf("truncated = %#v for a turn of exactly the cap; the page "+
			"holds all of it", got["truncated"])
	}
}

// A TURN THE RECOVERY MAKES WHOLE IS NOT REPORTED CUT EITHER.
//
// The closing read is twenty rows, so a turn between the cap and the cap plus
// twenty ends up complete on the page: its opening is the head and its ending
// closes the gap outright. Inferring the flag from the row count would put a
// "middle not shown" banner over a turn with nothing missing — which is the
// band a long turn most often lands in, so it is the common case rather than
// an edge.
func TestATurnTheRecoveryMakesWholeIsNotReportedCut(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	log := db.Events()
	base := time.Now().UTC().Add(-time.Hour)

	payload, err := json.Marshal(map[string]any{"turn_id": "whole", "phase": "execute"})
	if err != nil {
		t.Fatal(err)
	}
	// Past the cap, but inside the reach of the closing read.
	total := store.MaxTurnEvents + queries.TurnClosingEvents/2
	for i := range total {
		if err := log.Append(t.Context(), store.EventRecord{
			ID:   fmt.Sprintf("w-%04d", i),
			Type: "agent_phase_completed", Time: base.Add(time.Duration(i) * time.Second),
			Category: "lifecycle", Actor: "PM", Payload: payload,
		}); err != nil {
			t.Fatal(err)
		}
	}

	got := asMap(t, answer(t, queries.Sources{Events: log}, "turn",
		map[string]any{"turn_id": "whole"}))
	if n := len(rows(t, got["events"])); n != total {
		t.Errorf("%d of the turn's %d events reached the answer", n, total)
	}
	if got["truncated"] != false {
		t.Errorf("truncated = %#v for a turn the recovery made whole",
			got["truncated"])
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
	// A PAGE WITH A RECORD PAST IT offers a cursor; the end of the record
	// must not, or a client pages forever.
	next, _ := got["next"].(map[string]any)
	if next["before_id"] == nil || next["before_id"] == "" {
		t.Errorf("a page that left a phase behind offers no cursor: %v", got["next"])
	}
	if got["exhausted"] != false {
		t.Errorf("a page that left a phase behind claims to be exhausted: %v", got)
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

	// A PAGE THAT EXACTLY HOLDS THE RECORD is its end too: the store read no
	// row past it, and a cursor here would lead a pager to an empty page.
	whole := asMap(t, answer(t, queries.Sources{Events: log}, "phases", map[string]any{"limit": 3}))
	if len(rows(t, whole["phases"])) != 3 || whole["exhausted"] != true {
		t.Errorf("a page holding the whole record: %d phases, exhausted=%v, next=%v",
			len(rows(t, whole["phases"])), whole["exhausted"], whole["next"])
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
		Knowledge: served(stubSearcher{}),
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
	if !strings.Contains(gated["note"].(string), "knowledge.scope") {
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

// indexSearcher is a searchable backend with an index of its own: whether it
// is still on its first build, what it ranks and what that ranking missed,
// whether the search failed, and what it was asked.
type indexSearcher struct {
	building bool
	hits     []knowledge.Hit
	partial  *knowledge.Partial
	failed   bool
	asked    *[]knowledge.Query
}

func (indexSearcher) Backend() string                             { return "native" }
func (indexSearcher) CanSearch(*org.Role, *org.Organization) bool { return true }
func (s indexSearcher) Building(context.Context) bool             { return s.building }
func (s indexSearcher) Search(_ context.Context, q knowledge.Query) knowledge.Answer {
	*s.asked = append(*s.asked, q)
	if s.failed {
		return knowledge.Answer{Failed: true}
	}
	return knowledge.Answer{Hits: s.hits, Partial: s.partial}
}

// A NODE STILL INDEXING SAYS SO, and searches nothing.
//
// "The company has written nothing down" and "this node has not finished
// reading what it wrote" are opposite facts, and a screen showing the answer of
// an index on its first build would state the first for the second. Built, the
// same backend answers the turn-start block's own number of pages, each with
// what opens it.
func TestKnowledgeSaysWhenThisNodeIsStillIndexing(t *testing.T) {
	t.Parallel()
	company := func() *config.Company { return &config.Company{Name: "Acme"} }
	var asked []knowledge.Query
	hits := []knowledge.Hit{{Title: "Deploy runbook", Container: "ENG", PageID: "p-1"}}

	building := asMap(t, answer(t, queries.Sources{
		Knowledge: served(indexSearcher{building: true, hits: hits, asked: &asked}),
		Company:   company,
	}, "knowledge", map[string]any{"q": "deploy"}))
	if building["available"] != false ||
		building["reason"] != string(queries.KnowledgeBuilding) {
		t.Errorf("a building index answered %v, want reason %q", building,
			queries.KnowledgeBuilding)
	}
	if len(asked) != 0 {
		t.Errorf("a building index was searched anyway: %+v", asked)
	}

	built := asMap(t, answer(t, queries.Sources{
		Knowledge: served(indexSearcher{hits: hits, asked: &asked}),
		Company:   company,
	}, "knowledge", map[string]any{"q": "deploy"}))
	if built["available"] != true || built["reason"] != string(queries.KnowledgeRan) {
		t.Errorf("a built index answered %v, want the search to have run", built)
	}
	if len(asked) != 1 || asked[0].Limit != queries.KnowledgeHitLimit {
		t.Fatalf("the search was asked %+v, want one ask for %d pages", asked,
			queries.KnowledgeHitLimit)
	}
	rows, _ := built["hits"].([]any)
	var row map[string]any
	if len(rows) == 1 {
		row, _ = rows[0].(map[string]any)
	}
	if row["id"] != "p-1" || row["container"] != "ENG" {
		t.Errorf("the hits are %#v, want the page with its id and container", built["hits"])
	}
}

// A KNOWLEDGE BASE THIS NODE IS NOT SERVING IS ITS OWN STATE, not "no
// backend": the company configured one, and an operator told otherwise goes and
// re-checks a setting that is already right. The note is the refuser's own
// sentence, because the refuser is the one that knows what to fix.
func TestKnowledgeNamesAKnowledgeBaseThisNodeIsNotServing(t *testing.T) {
	t.Parallel()
	const detail = "the company's knowledge base is Confluence, and " +
		"`integrations.confluence.token` resolves to no credential"
	got := asMap(t, answer(t, queries.Sources{
		Knowledge: func() (knowledge.Searcher, knowledge.Refusal) {
			return nil, knowledge.Refusal{State: knowledge.NotServed, Detail: detail}
		},
		Company: func() *config.Company { return &config.Company{Name: "Acme"} },
	}, "knowledge", map[string]any{"q": "deploy"}))
	if got["available"] != false || got["reason"] != string(queries.KnowledgeNotServed) {
		t.Errorf("a knowledge base this node is not serving answered %v, want "+
			"reason %q", got, queries.KnowledgeNotServed)
	}
	if got["note"] != detail {
		t.Errorf("note = %q, want the refuser's own sentence %q", got["note"], detail)
	}
	if !queries.KnowledgeNotServed.Valid() {
		t.Errorf("%q is emitted and the enum does not admit it", queries.KnowledgeNotServed)
	}
}

// A QUERY PAST THE BOUND A SEAT'S SEARCH HOLDS IS REFUSED, naming the field the
// caller sent and the limit — and never cut, because a cut query is a
// different search whose hits nobody can tell from hits for what was typed.
func TestKnowledgeRefusesAQueryPastTheBoundNamingIt(t *testing.T) {
	t.Parallel()
	var asked []knowledge.Query
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{
		Knowledge: served(indexSearcher{asked: &asked}),
		Company:   func() *config.Company { return &config.Company{Name: "Acme"} },
	})
	long := strings.Repeat("x", knowledge.MaxQueryBytes+1)
	for _, field := range []string{"q", "text"} {
		_, err := r.Answer(t.Context(), "knowledge", map[string]any{field: long}, "")
		if !errors.Is(err, queries.ErrBadParams) {
			t.Fatalf("a %d-byte `%s` answered %v, want bad params", len(long), field, err)
		}
		for _, want := range []string{"`" + field + "`", fmt.Sprint(knowledge.MaxQueryBytes)} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal %q does not name %s", err, want)
			}
		}
	}
	if len(asked) != 0 {
		t.Fatalf("a refused query was searched anyway: %d asks", len(asked))
	}
	// AT THE BOUND IS NOT PAST IT, and what is searched is what was sent.
	at := strings.Repeat("x", knowledge.MaxQueryBytes)
	if _, err := r.Answer(t.Context(), "knowledge", map[string]any{"q": at}, ""); err != nil {
		t.Fatalf("a query exactly at the bound answered %v", err)
	}
	if len(asked) != 1 || asked[0].Text != at {
		t.Errorf("the search was asked %d times, want once with the query whole", len(asked))
	}
}

// A PARTIAL RANKING SAYS WHAT IT MISSED, AND A FAILED SEARCH SAYS IT FAILED.
// Either one shown as a bare list reads as everything that matched, which is
// the one conclusion this screen must not invite. Both are searches that RAN
// and degraded, so the reason stays the zero one and `note` says the rest.
func TestKnowledgeMarksAPartialAnswerAndAFailedOne(t *testing.T) {
	t.Parallel()
	company := func() *config.Company { return &config.Company{Name: "Acme"} }
	hits := []knowledge.Hit{{Title: "Deploy runbook", Container: "ENG", PageID: "p-1"}}
	var asked []knowledge.Query
	ask := func(s indexSearcher) map[string]any {
		s.asked = &asked
		return asMap(t, answer(t, queries.Sources{Knowledge: served(s), Company: company},
			"knowledge", map[string]any{"q": "deploy"}))
	}

	whole := ask(indexSearcher{hits: hits})
	if p, carried := whole["partial"]; !carried || p != nil {
		t.Errorf("a whole answer's partial = %v (present %v), want an explicit null", p, carried)
	}
	if whole["note"] != "" {
		t.Errorf("a whole answer carries the note %q", whole["note"])
	}

	partial := ask(indexSearcher{hits: hits, partial: &knowledge.Partial{
		BucketsAnswered: 42, BucketsMissing: 22, AbsentNodes: []string{"node-b"},
	}})
	if partial["available"] != true || partial["reason"] != string(queries.KnowledgeRan) {
		t.Errorf("a partial answer reports the search as not having run: %v", partial)
	}
	if rows, _ := partial["hits"].([]any); len(rows) != 1 {
		t.Errorf("a partial answer dropped its hits: %v", partial["hits"])
	}
	missing, _ := partial["partial"].(map[string]any)
	absent, _ := missing["absent_nodes"].([]any)
	if missing["buckets_answered"] != float64(42) || missing["buckets_missing"] != float64(22) ||
		len(absent) != 1 || absent[0] != "node-b" || missing["semantic_skipped"] != false {

		t.Errorf("partial = %v, want what the ranking missed", partial["partial"])
	}
	note, _ := partial["note"].(string)
	for _, want := range []string{"22 of 64", "node-b", "may still exist"} {
		if !strings.Contains(note, want) {
			t.Errorf("the partial note %q does not say %q", note, want)
		}
	}

	failed := ask(indexSearcher{failed: true})
	if failed["available"] != true || failed["reason"] != string(queries.KnowledgeRan) {
		t.Errorf("a failed search reports itself as never having started: %v", failed)
	}
	if note, _ := failed["note"].(string); !strings.Contains(note, "failed") {
		t.Errorf("a failed search's note %q does not say it failed", note)
	}
	if failed["partial"] != nil {
		t.Errorf("a failed search carries partial %v", failed["partial"])
	}
}

// THE SCREEN HIDES WHAT A SEAT IS NOT SHOWN. It answers "what would an agent
// find", and an agent's search excludes the auto-drafted pages nobody has
// reviewed — so this one does too, or it shows an operator pages no agent sees.
//
// Held on the exclusion the query ASKS FOR ([knowledge.Query.Excluded]), which
// is what every backend applies, rather than on the field the caller set: the
// field left nil takes the same default, and an empty one turns it off.
func TestKnowledgeHidesTheDraftsASeatsSearchHides(t *testing.T) {
	t.Parallel()
	var asked []knowledge.Query
	answer(t, queries.Sources{
		Knowledge: served(indexSearcher{asked: &asked}),
		Company:   func() *config.Company { return &config.Company{Name: "Acme"} },
	}, "knowledge", map[string]any{"q": "deploy"})
	if len(asked) != 1 || !slices.Contains(asked[0].Excluded(), knowledge.AutoDraftedParent) {
		t.Errorf("the search was asked %+v, want the auto-drafts excluded", asked)
	}
}

// EVERY TRACE THIS TURN TOUCHED, and the capped read is exactly why it is
// ASKED rather than derived.
//
// A turn RESUMED on another node after a restart spans more than one trace,
// which is precisely the turn somebody opens this page to understand. The
// client derived the set from the rows it was handed — so a trace whose events
// all fell in the middle a capped read dropped simply vanished, and one button
// labelled "trace" led to half the story with nothing saying a second half
// existed.
func TestATurnNamesEveryTraceItTouchedEvenOnesTheCapDropped(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	log := db.Events()
	base := time.Now().UTC().Add(-time.Hour)

	payload, err := json.Marshal(map[string]any{"turn_id": "resumed", "phase": "execute"})
	if err != nil {
		t.Fatal(err)
	}
	// THE SECOND TRACE IS IN THE MIDDLE, which is the only placement a
	// client-side derivation cannot see: the head read stops at the cap and
	// the recovered ending starts after it, so a trace confined to the rows
	// between them is invisible to anybody counting the rows they got.
	const extra = 60
	total := store.MaxTurnEvents + extra
	middle := store.MaxTurnEvents + extra/2
	for i := range total {
		trace := "trace-first"
		switch {
		case i == middle:
			trace = "trace-resumed"
		case i > middle:
			trace = "trace-third"
		}
		if err := log.Append(t.Context(), store.EventRecord{
			ID:   fmt.Sprintf("e-%04d", i),
			Type: "agent_phase_completed", Time: base.Add(time.Duration(i) * time.Second),
			Category: "lifecycle", Actor: "PM", TraceID: trace, Payload: payload,
		}); err != nil {
			t.Fatal(err)
		}
	}

	got := asMap(t, answer(t, queries.Sources{Events: log}, "turn",
		map[string]any{"turn_id": "resumed"}))
	traces := stringList(t, got["trace_ids"])
	// IN FIRST-APPEARANCE ORDER, which is the order a reader follows them
	// in: the trace the turn started under comes first.
	want := []string{"trace-first", "trace-resumed", "trace-third"}
	if !slices.Equal(traces, want) {
		t.Fatalf("trace_ids = %v, want %v — the middle one is the trace a "+
			"client-side derivation loses to the cap", traces, want)
	}
}

// A TURN WITH NO TRACE AT ALL ANSWERS AN EMPTY LIST, never null: a client
// rendering `trace_ids.length` should not have to guard the field as well, and
// an event written before tracing existed carries no trace id.
func TestATurnWithNoTracesAnswersAnEmptyList(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	log := db.Events()
	payload, err := json.Marshal(map[string]any{"turn_id": "untraced"})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(t.Context(), store.EventRecord{
		ID: "e-1", Type: "turn_completed", Time: time.Now().UTC().Add(-time.Minute),
		Category: "lifecycle", Actor: "PM", Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}
	got := asMap(t, answer(t, queries.Sources{Events: log}, "turn",
		map[string]any{"turn_id": "untraced"}))
	if got["trace_ids"] == nil {
		// AN EMPTY LIST, never null: a client rendering `.length` on the
		// field should not have to guard the field as well.
		t.Fatal("trace_ids is null on a turn whose events carry no trace")
	}
	if traces := stringList(t, got["trace_ids"]); len(traces) != 0 {
		t.Errorf("trace_ids = %v on a turn whose events carry none", traces)
	}
}

// stringList reads a JSON list of strings off an answer, since the helper above
// round-trips through the wire — which is the shape a client actually sees.
//
// Not `strings`, which is the standard package this file also uses.
func stringList(t *testing.T, v any) []string {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("%#v is not a list", v)
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("%#v is not a string", item)
		}
		out = append(out, s)
	}
	return out
}

// THE WINDOW IS A FILTER AND NOT THE CURSOR, and `turn_id` was declared,
// documented against migration 0014, and unaskable.
//
// `store.ListQuery` carried `TurnID` and the reader filtered on it, and no
// surface ever passed one — so "every event of this turn" was answerable by
// the store and reachable from nowhere. `since`/`until` did not exist at all,
// which left a reader scrubbing a time range paging backwards through history
// until they found it.
func TestTheEventListTakesATurnAndAWindow(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	log := db.Events()
	// RELATIVE TO NOW, because the log's own history window bounds every
	// read: a fixed date far enough in the past is outside it, and the
	// window filter under test would then be checked against a read that
	// was already empty.
	base := time.Now().UTC().Truncate(time.Second)

	for i, e := range []struct {
		id    string
		turn  string
		delta time.Duration
	}{
		{"e-old", "turn-a", -3 * time.Hour},
		{"e-mid", "turn-a", -2 * time.Hour},
		{"e-new", "turn-b", -1 * time.Hour},
	} {
		// THE TAG, not a field: `turn_id` is a promoted COLUMN the
		// writer derives from the event's own tags and payload, so a
		// record does not carry one directly.
		if err := log.Append(t.Context(), store.EventRecord{
			ID: e.id, Type: "agent_phase_completed", Time: base.Add(e.delta),
			Category: "lifecycle", Actor: "PM",
			Tags: map[string]string{"turn_id": e.turn},
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	src := queries.Sources{Events: log}

	ids := func(params map[string]any) []string {
		t.Helper()
		got := asMap(t, answer(t, src, "events", params))
		out := []string{}
		for _, row := range rows(t, got["events"]) {
			out = append(out, fmt.Sprint(row["id"]))
		}
		return out
	}

	// ONE TURN, which is the filter that existed and could not be asked.
	if got := ids(map[string]any{"turn_id": "turn-a"}); !slices.Equal(got,
		[]string{"e-mid", "e-old"}) {

		t.Errorf("turn_id=turn-a gave %v, want the turn's two events newest first", got)
	}

	// A HALF-OPEN WINDOW: since is inclusive, until is not. The boundary is
	// what makes two adjacent windows cover a range without double-counting
	// the row on the seam.
	since := base.Add(-2 * time.Hour).Format(time.RFC3339)
	until := base.Add(-1 * time.Hour).Format(time.RFC3339)
	if got := ids(map[string]any{"since": since, "until": until}); !slices.Equal(got,
		[]string{"e-mid"}) {

		t.Errorf("[%s, %s) gave %v, want only the row on the lower bound",
			since, until, got)
	}

	// EACH SIDE IS OPTIONAL, because an instant nobody named is unbounded
	// rather than midnight in 1970.
	if got := ids(map[string]any{"since": since}); !slices.Equal(got,
		[]string{"e-new", "e-mid"}) {

		t.Errorf("since alone gave %v, want everything from the bound on", got)
	}
}

// A WINDOW THAT NAMES NO ROWS IS REFUSED rather than answered empty. The
// interval is half-open, so `until <= since` is not a narrow window — it is an
// empty one, and a reader who typed their bounds the wrong way round is told
// rather than shown a company that did nothing.
func TestAnInvertedWindowIsRefusedRatherThanAnsweredEmpty(t *testing.T) {
	t.Parallel()
	src := queries.Sources{Events: openStore(t).Events()}
	_, err := askNative(t, src, "events", map[string]any{
		"since": "2026-04-16T12:00:00Z",
		"until": "2026-04-16T11:00:00Z",
	})
	if !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("an inverted window answered %v, want bad params", err)
	}
	// AND AN UNPARSEABLE BOUND NAMES THE PARAMETER, because a silently
	// dropped one is a read that answers a different question than the one
	// asked.
	if _, err := askNative(t, src, "events", map[string]any{
		"since": "last tuesday",
	}); !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("an unparseable since answered %v, want bad params", err)
	}
}

// `failed` IS THREE-VALUED, and the third value is the default.
//
// nil is every turn, true is the ones that carried a failure, false is the
// ones that did not. Folding the absent case into `false` would make an
// unparameterised list hide every failing turn — which is the one an operator
// opens this screen for.
func TestTheTurnListsFailedFilterIsThreeValued(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	log := db.Events()
	base := time.Now().UTC().Add(-time.Hour)

	seed := func(id string, failed bool) {
		t.Helper()
		tags := map[string]string{"turn_id": id, "agent_role": "PM"}
		if failed {
			tags["failed"] = "true"
		}
		if err := log.Append(t.Context(), store.EventRecord{
			ID: id + "-p0", Type: "agent_phase_completed", Time: base,
			Category: "lifecycle", Actor: "PM", Tags: tags,
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed("t-ok", false)
	seed("t-bad", true)

	src := queries.Sources{Events: log}
	ids := func(params map[string]any) []string {
		t.Helper()
		got := asMap(t, answer(t, src, "turns", params))
		out := []string{}
		for _, row := range rows(t, got["turns"]) {
			out = append(out, fmt.Sprint(row["turn_id"]))
		}
		slices.Sort(out)
		return out
	}

	if got := ids(nil); !slices.Equal(got, []string{"t-bad", "t-ok"}) {
		t.Errorf("the default gave %v, want every turn — an absent filter is "+
			"not `false`", got)
	}
	if got := ids(map[string]any{"failed": "true"}); !slices.Equal(got, []string{"t-bad"}) {
		t.Errorf("failed=true gave %v", got)
	}
	if got := ids(map[string]any{"failed": "false"}); !slices.Equal(got, []string{"t-ok"}) {
		t.Errorf("failed=false gave %v", got)
	}
	// A VALUE THAT IS NEITHER is refused rather than read as one of them.
	if _, err := askNative(t, src, "turns", map[string]any{"failed": "maybe"}); !errors.Is(
		err, queries.ErrBadParams) {

		t.Errorf("failed=maybe answered %v, want bad params", err)
	}
}

// THE TURN LIST PAGES ON THE CURSOR IT HANDS BACK, and offers one only when
// the window holds more.
//
// Echoed rather than left for a client to assemble — a client building it
// from the last row's fields would be reimplementing the one thing that must
// not drift — and a PAIR, because t-3 and t-2 begin at the same instant. The
// page is ONE turn so the first boundary falls BETWEEN those two: a cursor on
// the start alone would ask for turns that began before base+1s, answer t-1
// on the second page, and never list t-2 at all. Offered only when the store
// read a turn past the page, so a window that exactly fills a page ends there
// instead of leading a pager to an empty one, and `truncated` says which a
// page is before anything is drawn from it.
func TestTheTurnListPagesOnTheCursorItHandsBack(t *testing.T) {
	t.Parallel()
	log := openStore(t).Events()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for _, turn := range []struct {
		id string
		at time.Time
	}{
		{"t-1", base},
		{"t-2", base.Add(time.Second)},
		{"t-3", base.Add(time.Second)},
	} {
		if err := log.Append(t.Context(), store.EventRecord{
			ID: turn.id + "-p0", Type: "agent_phase_completed", Time: turn.at,
			Category: "lifecycle", Actor: "PM",
			Tags: map[string]string{"turn_id": turn.id, "agent_role": "PM"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	src := queries.Sources{Events: log}
	idsOf := func(page map[string]any) []string {
		var out []string
		for _, row := range rows(t, page["turns"]) {
			out = append(out, row["turn_id"].(string))
		}
		return out
	}

	// ORDER IS (start DESC, turn_id DESC), so of the two that share base+1s
	// t-3 comes first and the first page's cursor sits between them.
	first := asMap(t, answer(t, src, "turns", map[string]any{"limit": 1}))
	if got := idsOf(first); !slices.Equal(got, []string{"t-3"}) {
		t.Fatalf("first page = %v, want the newest", got)
	}
	if first["truncated"] != true {
		t.Errorf("a page that left turns behind says truncated=%v", first["truncated"])
	}
	next, _ := first["next"].(map[string]any)
	if next["before_id"] != "t-3" || next["before_time"] == nil {
		t.Fatalf("next = %#v, want the last row's start and turn id", first["next"])
	}

	// THE TURN THAT SHARES THE BOUNDARY'S INSTANT is the next page, not
	// the turn a second older.
	second := asMap(t, answer(t, src, "turns", map[string]any{
		"limit": 1, "before_time": next["before_time"], "before_id": next["before_id"],
	}))
	if got := idsOf(second); !slices.Equal(got, []string{"t-2"}) {
		t.Fatalf("second page = %v, want t-2, which began at the same instant "+
			"as the cursor", got)
	}
	next, _ = second["next"].(map[string]any)
	if second["truncated"] != true || next["before_id"] != "t-2" {
		t.Fatalf("second page: truncated=%v next=%#v, want a cursor at t-2",
			second["truncated"], second["next"])
	}

	rest := asMap(t, answer(t, src, "turns", map[string]any{
		"limit": 1, "before_time": next["before_time"], "before_id": next["before_id"],
	}))
	if got := idsOf(rest); !slices.Equal(got, []string{"t-1"}) {
		t.Fatalf("third page = %v, want the one turn left", got)
	}
	// AND NOTHING TO RESUME FROM AT THE END, so a client walking the list
	// stops rather than re-asking for the same page for ever.
	if rest["next"] != nil || rest["truncated"] != false {
		t.Errorf("the last page offers next=%#v, truncated=%v", rest["next"], rest["truncated"])
	}

	// A WINDOW THAT EXACTLY FILLS THE PAGE is not a cut one.
	whole := asMap(t, answer(t, src, "turns", map[string]any{"limit": 3}))
	if len(idsOf(whole)) != 3 || whole["next"] != nil || whole["truncated"] != false {
		t.Errorf("a page holding the whole window: %d turns, next=%#v, truncated=%v",
			len(idsOf(whole)), whole["next"], whole["truncated"])
	}

	// HALF A CURSOR IS REFUSED rather than read as the first page, which a
	// pager would follow for ever.
	if _, err := askNative(t, src, "turns", map[string]any{
		"before_time": next["before_time"],
	}); !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("a cursor with no turn id answered %v, want bad params", err)
	}
}

// THE TURN SCREEN SAYS WHICH ATTEMPT IT IS SHOWING.
//
// A turn id names one RUN (ADR-0017), so a trigger that failed without
// reaching outside the engine and was redelivered is several turns — and this
// screen is the destination of every deep link in the product. Landing on one
// of them with nothing saying the other exists is how a reader concludes the
// company did the work twice, or that it failed when the next attempt
// succeeded.
func TestATurnNamesEveryAttemptAtItsTrigger(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	log := db.Events()
	base := time.Now().UTC().Add(-time.Minute)

	write := func(id, kind, run, key string, at time.Time) {
		t.Helper()
		payload, err := json.Marshal(map[string]any{
			"turn_id": run, "work_key": key, "phase": "execute",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := log.Append(t.Context(), store.EventRecord{
			ID: id, Type: kind, Time: at, Category: "lifecycle", Actor: "CEO",
			Tags: map[string]string{
				"turn_id": run, "work_key": key, "agent_role": "CEO",
			},
			Payload: payload,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Two attempts at one trigger, and an unrelated turn that must not join.
	write("a", "agent_phase_completed", "run-1", "wk-1", base)
	write("b", "agent_phase_completed", "run-2", "wk-1", base.Add(2*time.Minute))
	write("c", "agent_phase_completed", "run-9", "wk-2", base.Add(3*time.Minute))

	got := asMap(t, answer(t, queries.Sources{Events: log}, "turn",
		map[string]any{"turn_id": "run-2"}))

	if got["work_key"] != "wk-1" {
		t.Errorf("work_key = %v, want the trigger both attempts share — a key read "+
			"off a field the read path never fills answers this for no turn at all",
			got["work_key"])
	}
	attempts := rows(t, got["attempts"])
	if len(attempts) != 2 {
		t.Fatalf("%d attempts, want both runs of wk-1 and not the other trigger's: %v",
			len(attempts), attempts)
	}
	// OLDEST FIRST, because "attempt 2 of 2" counts from the one that ran
	// first however the listing was ordered.
	if attempts[0]["turn_id"] != "run-1" || attempts[1]["turn_id"] != "run-2" {
		t.Errorf("attempts = %v, want run-1 then run-2", attempts)
	}
	if got["attempts_truncated"] != false {
		t.Errorf("attempts_truncated = %v over every run of the trigger", got["attempts_truncated"])
	}
}

// A LIST OF ATTEMPTS THAT IS NOT EVERY RUN SAYS SO, AND THE ROUTE IT NAMES
// REACHES THE RUNS IT LEFT OUT.
//
// The list is counted from its first element — "attempt 2 of 3" — and it is
// the NEWEST runs reversed, so one cut at the page would renumber every attempt
// in it with nothing on the screen to say the first one shown was not the
// first one run. The run left out is the OLDEST, placed here past the turns
// list's default week and inside the thirty days the attempts read covers:
// the route the answer documents — `turns` with `work_key=` and
// `days=` store.MaxTurnDays, paged by `next` — must reach it, and the same
// route without `days` must not, which is why `days` is part of it.
func TestACutListOfAttemptsSaysItIsCut(t *testing.T) {
	t.Parallel()
	log := openStore(t).Events()
	now := time.Now().UTC()
	base := now.Add(-time.Hour)
	const runs = store.MaxTurnPage + 1
	for i := range runs {
		run := fmt.Sprintf("run-%03d", i)
		at := base.Add(time.Duration(i) * time.Second)
		if i == 0 {
			at = now.Add(-10 * 24 * time.Hour)
		}
		if err := log.Append(t.Context(), store.EventRecord{
			ID: run + "-p0", Type: "agent_phase_completed",
			Time: at, Category: "lifecycle", Actor: "CEO",
			Tags: map[string]string{"turn_id": run, "work_key": "wk-1", "agent_role": "CEO"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	newest := fmt.Sprintf("run-%03d", runs-1)
	got := asMap(t, answer(t, queries.Sources{Events: log}, "turn",
		map[string]any{"turn_id": newest}))
	attempts := rows(t, got["attempts"])
	if len(attempts) != store.MaxTurnPage {
		t.Fatalf("%d attempts, want the page of %d", len(attempts), store.MaxTurnPage)
	}
	if got["attempts_truncated"] != true {
		t.Errorf("attempts_truncated = %v with %d runs held and %d listed",
			got["attempts_truncated"], runs, len(attempts))
	}
	// THE NEWEST RUNS, so the one being read is on the list and the one
	// left out is the first.
	if attempts[len(attempts)-1]["turn_id"] != newest || attempts[0]["turn_id"] == "run-000" {
		t.Errorf("attempts run %v … %v, want the newest %d ending at %s",
			attempts[0]["turn_id"], attempts[len(attempts)-1]["turn_id"], store.MaxTurnPage, newest)
	}

	// THE ROUTE TO THE REST, followed to its end.
	walk := func(params map[string]any) map[string]bool {
		t.Helper()
		seen := map[string]bool{}
		for pages := 0; ; pages++ {
			if pages > runs {
				t.Fatal("the turns cursor never reached the end of the runs")
			}
			page := asMap(t, answer(t, queries.Sources{Events: log}, "turns", params))
			for _, row := range rows(t, page["turns"]) {
				seen[row["turn_id"].(string)] = true
			}
			next, _ := page["next"].(map[string]any)
			if next == nil {
				return seen
			}
			params = maps.Clone(params)
			params["before_time"], params["before_id"] = next["before_time"], next["before_id"]
		}
	}
	every := walk(map[string]any{"work_key": "wk-1", "days": store.MaxTurnDays})
	if len(every) != runs || !every["run-000"] {
		t.Errorf("turns with work_key and days=%d reached %d of %d runs (run-000: %t)",
			store.MaxTurnDays, len(every), runs, every["run-000"])
	}
	if week := walk(map[string]any{"work_key": "wk-1"}); week["run-000"] {
		t.Errorf("turns without days reached the ten-day-old run — the default " +
			"window is no longer a week, and the documented route should say so")
	}
}

// AND A TURN WITH NO WORK KEY SAYS SO rather than claiming every other
// unkeyed turn as an attempt at itself.
func TestATurnWithNoWorkKeyClaimsNoAttempts(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	log := db.Events()
	payload, err := json.Marshal(map[string]any{"turn_id": "run-1", "phase": "execute"})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(t.Context(), store.EventRecord{
		ID: "a", Type: "agent_phase_completed", Time: time.Now().UTC().Add(-time.Minute),
		Category: "lifecycle", Actor: "CEO",
		Tags:    map[string]string{"turn_id": "run-1", "agent_role": "CEO"},
		Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}
	got := asMap(t, answer(t, queries.Sources{Events: log}, "turn",
		map[string]any{"turn_id": "run-1"}))
	if got["work_key"] != "" {
		t.Errorf("work_key = %v, want empty", got["work_key"])
	}
	if n := len(rows(t, got["attempts"])); n != 0 {
		t.Errorf("%d attempts for a turn with no trigger key, want none", n)
	}
}

// AND A TURN OLDER THAN THE TURN LIST'S DEFAULT WINDOW STILL NAMES ITS OWN
// ATTEMPTS.
//
// The two halves of this answer are read through different windows: the rows
// come from store.EventLog.Turn, which floors at store.EventHistory — thirty
// days — while the attempts come from the turns LISTING, whose query takes
// DefaultTurnDays when asked for nothing, which is a week. Left implicit, a
// turn between eight and thirty days old found its work key on its own rows
// and then reported no attempt at all, not even the one being read: the
// screen said "attempt ? of 0" about a turn it was displaying.
func TestAnOldTurnStillNamesItsAttempts(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	log := db.Events()
	// PAST DefaultTurnDays AND INSIDE EventHistory, which is the only band
	// where the two windows disagree.
	base := time.Now().UTC().Add(-10 * 24 * time.Hour)

	write := func(id, run, key string, at time.Time) {
		t.Helper()
		payload, err := json.Marshal(map[string]any{
			"turn_id": run, "work_key": key, "phase": "execute",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := log.Append(t.Context(), store.EventRecord{
			ID: id, Type: "agent_phase_completed", Time: at,
			Category: "lifecycle", Actor: "CEO",
			Tags: map[string]string{
				"turn_id": run, "work_key": key, "agent_role": "CEO",
			},
			Payload: payload,
		}); err != nil {
			t.Fatal(err)
		}
	}
	write("a", "run-1", "wk-1", base)
	write("b", "run-2", "wk-1", base.Add(2*time.Minute))

	got := asMap(t, answer(t, queries.Sources{Events: log}, "turn",
		map[string]any{"turn_id": "run-2"}))

	if got["work_key"] != "wk-1" {
		t.Fatalf("work_key = %v, want wk-1", got["work_key"])
	}
	attempts := rows(t, got["attempts"])
	if len(attempts) != 2 {
		t.Fatalf("%d attempts for a turn ten days old, want both runs — the "+
			"detail read reaches thirty days back and the attempts read must "+
			"reach as far: %v", len(attempts), attempts)
	}
}

// AND A TURN FROM BEFORE THE SPLIT ANSWERS OFF THE BACKFILLED COLUMN.
//
// schema/0029 moved the work key into a column of its own and backfilled it
// from turn_id, which is where it lived; it did not rewrite the stored tags,
// because those record what the writer extracted from an event that carried no
// such field. A key read out of the tags therefore answers nothing for every
// turn in the history the backfill exists to preserve, while /events?work_key=
// — which filters on the column — returns those same rows.
//
// Append's Spend is the carrier for every promoted column, so a record setting
// the key there and not in its tags writes exactly a post-backfill row.
func TestAPreSplitTurnNamesItsAttemptsFromTheBackfilledColumn(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	log := db.Events()
	base := time.Now().UTC().Add(-time.Minute)

	write := func(id, run, key string, at time.Time) {
		t.Helper()
		if err := log.Append(t.Context(), store.EventRecord{
			ID: id, Type: "agent_phase_completed", Time: at,
			Category: "lifecycle", Actor: "CEO",
			// NO work_key TAG, exactly as history carries it.
			Tags: map[string]string{"turn_id": run, "agent_role": "CEO"},
			Spend: &store.Spend{
				TurnID: run, WorkKey: key, Phase: "execute",
			},
			Payload: []byte(`{"turn_id":"` + run + `","phase":"execute"}`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	write("a", "run-1", "wk-old", base)
	write("b", "run-2", "wk-old", base.Add(2*time.Minute))

	got := asMap(t, answer(t, queries.Sources{Events: log}, "turn",
		map[string]any{"turn_id": "run-2"}))

	if got["work_key"] != "wk-old" {
		t.Errorf("work_key = %v, want the backfilled column — a tag read "+
			"answers this for no turn written before schema/0029",
			got["work_key"])
	}
	if n := len(rows(t, got["attempts"])); n != 2 {
		t.Errorf("%d attempts, want both runs of wk-old", n)
	}
}
