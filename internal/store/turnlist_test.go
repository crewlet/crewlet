package store_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// seedTurn writes one turn's worth of events: two phases and a completion.
func seedTurn(t *testing.T, log *store.EventLog, id string, at time.Time,
	role string, mutate func(*store.EventRecord, int)) {

	t.Helper()
	for i := range 2 {
		rec := store.EventRecord{
			ID:       fmt.Sprintf("%s-p%d", id, i),
			Type:     "agent_phase_completed",
			Time:     at.Add(time.Duration(i) * time.Second),
			Category: "lifecycle", Actor: role,
			// THE TAGS, not the actor: `agent_role` is a PROMOTED
			// column written from the tag map, which is where a real
			// phase record carries it.
			Tags: map[string]string{
				"turn_id": id, "trigger": "chat", "agent_role": role,
			},
		}
		if mutate != nil {
			mutate(&rec, i)
		}
		if err := log.Append(t.Context(), rec); err != nil {
			t.Fatalf("append %s: %v", rec.ID, err)
		}
	}
	payload, err := json.Marshal(map[string]any{
		"turn_id": id, "duration_ms": 4200, "plan_summary": "did the thing",
		"task_id": "ENG-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(t.Context(), store.EventRecord{
		ID: id + "-done", Type: "turn_completed", Time: at.Add(3 * time.Second),
		Category: "lifecycle", Actor: role,
		Tags:    map[string]string{"turn_id": id},
		Payload: payload,
	}); err != nil {
		t.Fatalf("append the completion: %v", err)
	}
}

// THERE WAS NO LIST OF TURNS ANYWHERE, and a turn is the unit of work this
// engine does.
//
// The dashboard faked one by paging the raw event feed sixty-one times and
// folding the rows in the browser — slow, capped at whatever the caller gave
// up on, and wrong at the page boundary, where a turn straddling two pages
// appeared twice. Every aggregate here is a promoted COLUMN (migration 0015),
// so one row per turn is a group-by over narrow values rather than a fold over
// documents.
func TestTurnsFoldOneRowPerTurn(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	base := time.Now().UTC().Add(-time.Hour)
	seedTurn(t, log, "t-1", base, "PM", nil)
	seedTurn(t, log, "t-2", base.Add(time.Minute), "Dev", nil)

	got, err := log.Turns(t.Context(), store.TurnQuery{})
	if err != nil {
		t.Fatalf("Turns: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("%d turns from six events, want 2: %+v", len(got), got)
	}
	// NEWEST FIRST, ordered on the turn's START rather than on any one
	// event: a turn that ran for an hour began before one that started
	// after it finished.
	if got[0].TurnID != "t-2" {
		t.Errorf("the first turn is %s, want the newer one", got[0].TurnID)
	}
	one := got[1]
	switch {
	case one.Phases != 2:
		t.Errorf("t-1 has %d phases, want 2", one.Phases)
	case !one.Complete:
		t.Error("t-1 has a completion record and reports itself incomplete")
	case one.DurationMS != 4200:
		t.Errorf("t-1's duration is %d, want the turn's own measurement",
			one.DurationMS)
	case one.Summary != "did the thing":
		t.Errorf("t-1's summary is %q", one.Summary)
	case one.TaskID != "ENG-1":
		t.Errorf("t-1's task is %q", one.TaskID)
	case one.Trigger != "chat":
		t.Errorf("t-1's trigger is %q, want the phase records' own", one.Trigger)
	case one.AgentRole != "PM":
		t.Errorf("t-1's seat is %q", one.AgentRole)
	}
	// THE SPAN IS THE EVENTS', which is a different fact from the
	// duration: the span covers the reflection pass that publishes after
	// the turn ends, and the duration is what the turn itself measured.
	if !one.EndedAt.After(one.StartedAt) {
		t.Errorf("t-1's span is %s..%s", one.StartedAt, one.EndedAt)
	}
}

// A TURN WITH NO COMPLETION IS INCOMPLETE, not a turn with a duration of zero.
// It is either still running or died mid-flight, and a zero duration beside a
// `complete: true` would say it finished instantly.
func TestATurnWithNoCompletionSaysSo(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	if err := log.Append(t.Context(), store.EventRecord{
		ID: "live-p0", Type: "agent_phase_completed",
		Time: time.Now().UTC().Add(-time.Minute), Category: "lifecycle",
		Actor: "PM",
		Tags:  map[string]string{"turn_id": "live", "agent_role": "PM"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := log.Turns(t.Context(), store.TurnQuery{})
	if err != nil {
		t.Fatalf("Turns: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("%d turns, want the running one", len(got))
	}
	if got[0].Complete {
		t.Error("a turn with no completion record reports itself complete")
	}
	if got[0].DurationMS != 0 {
		t.Errorf("duration = %d on a turn that has not ended", got[0].DurationMS)
	}
}

// THE AGGREGATES ARE OVER COLUMNS, and the tokens are the turn's whole spend.
func TestATurnSumsItsTokensAndNamesEveryModel(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	base := time.Now().UTC().Add(-time.Hour)
	seedTurn(t, log, "t-1", base, "PM", func(rec *store.EventRecord, i int) {
		payload, err := json.Marshal(map[string]any{
			"turn_id":       "t-1",
			"model":         []string{"claude-opus-5", "claude-haiku-4-5"}[i],
			"input_tokens":  100 * (i + 1),
			"output_tokens": 10 * (i + 1),
			"total_tokens":  110 * (i + 1),
			"iteration":     i + 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		rec.Payload = payload
	})

	got, err := log.Turns(t.Context(), store.TurnQuery{})
	if err != nil {
		t.Fatalf("Turns: %v", err)
	}
	one := got[0]
	switch {
	case one.TotalTokens != 330:
		t.Errorf("total tokens = %d, want 110 + 220", one.TotalTokens)
	case one.InputTokens != 300:
		t.Errorf("input tokens = %d", one.InputTokens)
	case one.Rounds != 2:
		t.Errorf("rounds = %d, want the highest iteration any phase reached",
			one.Rounds)
	}
	// EVERY MODEL AND NOTHING ELSE, because a turn routinely uses two — a
	// cheap one for the extension judge and the seat's own for the work —
	// and naming one makes a cost reader attribute the whole turn to it.
	//
	// THE WHOLE LIST, not `Contains` over it: `model` is NOT NULL with an
	// empty default and only a phase record carries one, so the turn's own
	// `turn_completed` row folded into the join as a nameless element and
	// every `Contains` assertion passed straight over it.
	want := []string{"claude-haiku-4-5", "claude-opus-5"}
	named := splitModels(one.Models)
	slices.Sort(named)
	if !slices.Equal(named, want) {
		t.Errorf("models = %q, which splits to %q, want exactly %q",
			one.Models, named, want)
	}
}

// ONE SEAT, ONE MODEL, OR THE FAILURES — the three narrowings, each a question
// an operator actually asks of a list of turns.
func TestTheTurnListNarrowsBySeatModelAndFailure(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	base := time.Now().UTC().Add(-time.Hour)
	seedTurn(t, log, "t-pm", base, "PM", func(rec *store.EventRecord, _ int) {
		payload, _ := json.Marshal(map[string]any{
			"turn_id": "t-pm", "model": "claude-opus-5",
		})
		rec.Payload = payload
	})
	seedTurn(t, log, "t-dev", base.Add(time.Minute), "Dev", func(rec *store.EventRecord, i int) {
		payload, _ := json.Marshal(map[string]any{
			"turn_id": "t-dev", "model": "claude-haiku-4-5",
		})
		rec.Payload = payload
		if i == 0 {
			rec.Tags["failed"] = "true"
		}
	})

	ids := func(q store.TurnQuery) []string {
		t.Helper()
		got, err := log.Turns(t.Context(), q)
		if err != nil {
			t.Fatalf("Turns(%+v): %v", q, err)
		}
		out := []string{}
		for _, turn := range got {
			out = append(out, turn.TurnID)
		}
		slices.Sort(out)
		return out
	}

	if got := ids(store.TurnQuery{AgentRole: "PM"}); !slices.Equal(got, []string{"t-pm"}) {
		t.Errorf("PM's turns = %v", got)
	}
	if got := ids(store.TurnQuery{Model: "claude-haiku-4-5"}); !slices.Equal(got,
		[]string{"t-dev"}) {

		t.Errorf("the haiku turns = %v — a turn is selected when ANY of its "+
			"phases used the model", got)
	}
	yes, no := true, false
	if got := ids(store.TurnQuery{Failed: &yes}); !slices.Equal(got, []string{"t-dev"}) {
		t.Errorf("the failed turns = %v", got)
	}
	if got := ids(store.TurnQuery{Failed: &no}); !slices.Equal(got, []string{"t-pm"}) {
		t.Errorf("the clean turns = %v — nil is both, and false is not nil", got)
	}
}

// THE CURSOR IS ON THE TURN'S START, which is what the listing is ordered by:
// a cursor on any one event would page a turn twice, which is exactly the
// defect the browser-side fold had at its page boundary.
func TestTheTurnCursorPagesWithoutRepeating(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	base := time.Now().UTC().Add(-time.Hour)
	for i := range 5 {
		seedTurn(t, log, fmt.Sprintf("t-%d", i),
			base.Add(time.Duration(i)*time.Minute), "PM", nil)
	}

	first, err := log.Turns(t.Context(), store.TurnQuery{Limit: 2})
	if err != nil {
		t.Fatalf("Turns: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("the first page has %d turns", len(first))
	}
	second, err := log.Turns(t.Context(), store.TurnQuery{
		Limit: 2, Before: first[len(first)-1].StartedAt,
	})
	if err != nil {
		t.Fatalf("Turns: %v", err)
	}
	if len(second) != 2 {
		t.Fatalf("the second page has %d turns", len(second))
	}
	for _, a := range first {
		for _, b := range second {
			if a.TurnID == b.TurnID {
				t.Fatalf("%s is on both pages", a.TurnID)
			}
		}
	}
}

func splitModels(s string) []string {
	if s == "" {
		return nil
	}
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		out = append(out, strings.TrimSpace(part))
	}
	return out
}

// AN EMPTY IDENTIFIER IS NOT A FILTER, and binding one was how a seat read
// answered every non-agent event in the window.
//
// The clause was `(agent_id = ? OR agent_role = ?)` with both bound
// unconditionally, so a caller holding only a role — which is every caller
// that got its handle from a URL the roster could not resolve — matched every
// row whose `agent_id` is empty. The guard only caught the case where BOTH
// were empty.
func TestASeatFilterWithOneIdentifierDoesNotMatchEverything(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	base := time.Now().UTC().Add(-time.Hour)
	seedTurn(t, log, "t-pm", base, "PM", nil)
	// A TURN WITH NO SEAT AT ALL, which is what a system-authored one
	// looks like — and is exactly what an empty identifier used to match.
	if err := log.Append(t.Context(), store.EventRecord{
		ID: "sys-p0", Type: "agent_phase_completed", Time: base.Add(time.Minute),
		Category: "lifecycle", Tags: map[string]string{"turn_id": "t-sys"},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := log.Turns(t.Context(), store.TurnQuery{AgentRole: "PM"})
	if err != nil {
		t.Fatalf("Turns: %v", err)
	}
	if len(got) != 1 || got[0].TurnID != "t-pm" {
		t.Fatalf("PM's turns = %+v, want only theirs — an empty agent_id "+
			"matched the seatless turn", got)
	}

	// AND THE SAME TRAP ONE FUNCTION OVER. `AgentPhases` bound both
	// identifiers the same way, so a handle that resolved to no role was
	// answered every seatless phase in the window.
	phases, err := log.AgentPhases(t.Context(), "", "PM", nil)
	if err != nil {
		t.Fatalf("AgentPhases: %v", err)
	}
	for _, rec := range phases {
		if rec.ID == "sys-p0" {
			t.Fatal("PM's phases include a phase with no seat at all")
		}
	}
	if len(phases) == 0 {
		t.Fatal("PM's phases are empty, so the case above proves nothing")
	}
}
