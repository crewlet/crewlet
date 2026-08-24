package livestate_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
)

// --- token accounting --------------------------------------------------- //

func TestTurnTokensAccumulate(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	for i, tokens := range []int{10, 5} {
		s.Apply(env("agent_turn_completed", map[string]any{
			"role": "Lead", "input_tokens": tokens, "output_tokens": tokens,
			"total_tokens": tokens * 2,
		}, id(string(rune('a'+i)))))
	}
	got := overlayOf(t, s, "Lead")
	if got.InputTokens != 15 || got.OutputTokens != 15 || got.TotalTokens != 30 {
		t.Errorf("tokens = %d/%d/%d, want 15/15/30",
			got.InputTokens, got.OutputTokens, got.TotalTokens)
	}
}

func TestARedeliveredTurnIsCountedOnce(t *testing.T) {
	t.Parallel()
	// The dedupe exists so a hydrated turn is not counted a second time
	// when the same turn also arrives on the live stream.
	s := livestate.New()
	turn := env("agent_turn_completed", map[string]any{
		"role": "Lead", "input_tokens": 10, "output_tokens": 2, "total_tokens": 12,
	})
	s.Apply(turn)
	s.Apply(turn)

	if got := overlayOf(t, s, "Lead").TotalTokens; got != 12 {
		t.Errorf("total tokens = %d, want 12: a redelivery was counted twice", got)
	}
}

func TestTurnsWithNoEventIDAreEachCounted(t *testing.T) {
	t.Parallel()
	// The counterfactual: the dedupe is keyed on the event id, so an
	// envelope carrying none cannot be deduped — and must not be silently
	// collapsed with an unrelated one.
	s := livestate.New()
	for range 2 {
		s.Apply(env("agent_turn_completed", map[string]any{
			"role": "Lead", "total_tokens": 5,
		}, id("")))
	}
	if got := overlayOf(t, s, "Lead").TotalTokens; got != 10 {
		t.Errorf("total tokens = %d, want 10", got)
	}
}

// --- the activity feed -------------------------------------------------- //

func TestPersistedEventsAreReturnedNewestFirst(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	for i, kind := range []string{"task_created", "task_started", "task_completed"} {
		s.Apply(env(kind, map[string]any{"role": "Lead"}, id(string(rune('a'+i)))))
	}
	feed := s.RecentEvents(0)
	if len(feed) != 3 {
		t.Fatalf("feed = %d rows, want 3", len(feed))
	}
	if feed[0].Type != "task_completed" {
		t.Errorf("newest row = %q, want task_completed", feed[0].Type)
	}
	if got := s.RecentEvents(2); len(got) != 2 || got[0].Type != "task_completed" {
		t.Errorf("limited feed = %v", got)
	}
}

func TestTheFeedIsBoundedAndDropsTheOldest(t *testing.T) {
	t.Parallel()
	s := livestate.New(livestate.WithFeedLimit(3))
	for i := range 6 {
		s.Apply(env("task_created", map[string]any{"role": "Lead"},
			id(string(rune('a'+i))), at(time.Date(2026, 6, 14, 12, i, 0, 0, time.UTC).Format(time.RFC3339))))
	}
	feed := s.RecentEvents(0)
	if len(feed) != 3 {
		t.Fatalf("feed = %d rows, want the cap of 3", len(feed))
	}
	if feed[0].ID != "f" || feed[2].ID != "d" {
		t.Errorf("feed ids = %s..%s, want the three newest", feed[0].ID, feed[2].ID)
	}
}

func TestUncategorizedEventsAreNotBuffered(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("task_started", map[string]any{"role": "Lead"}, streamOnly))
	if got := s.RecentEvents(0); len(got) != 0 {
		t.Errorf("feed = %v, want empty", got)
	}
}

func TestAFeedRowCarriesTheFailureFlag(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("agent_phase_completed", map[string]any{"role": "Lead", "failed": true}))
	feed := s.RecentEvents(0)
	if len(feed) != 1 || !feed[0].Failed {
		t.Errorf("feed = %+v, want one failed row", feed)
	}
}

func TestFailureByEventTypeNeedsNoPayloadFlag(t *testing.T) {
	t.Parallel()
	// Some events ARE a failure by their very type, independent of any
	// payload flag, and three layers have to agree about which.
	for _, kind := range []string{"task_failed", "llm_unavailable", "budget_exhausted", "turn.guard_breach"} {
		s := livestate.New()
		s.Apply(env(kind, map[string]any{"role": "Lead"}))
		feed := s.RecentEvents(0)
		if len(feed) != 1 || !feed[0].Failed {
			t.Errorf("%s: feed = %+v, want a failed row", kind, feed)
		}
	}
}

func TestAnOrdinaryEventIsNotMarkedFailed(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("task_completed", map[string]any{"role": "Lead"}))
	if feed := s.RecentEvents(0); len(feed) != 1 || feed[0].Failed {
		t.Errorf("feed = %+v, want an unfailed row", feed)
	}
}

// --- merging onto static rows ------------------------------------------- //

func TestTheOverlayIsMergedOntoStaticRows(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("task_started", map[string]any{"role": "Lead", "task_id": "t-1"}))

	rows := s.MergeAgents([]map[string]any{
		{"role": "Lead", "handle": "lead", "unit": "Eng"},
		{"role": "Quiet", "handle": "quiet"},
	})
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0]["handle"] != "lead" || rows[0]["unit"] != "Eng" {
		t.Errorf("the static half was lost: %v", rows[0])
	}
	if rows[0]["state"] != "working" {
		t.Errorf("state = %v, want working", rows[0]["state"])
	}
	// A role with no live entry is returned as-is, which the dashboard
	// renders offline — and must not gain a half-filled overlay.
	if _, ok := rows[1]["state"]; ok {
		t.Errorf("a role with no live entry gained a state: %v", rows[1])
	}
}

func TestMergingDoesNotMutateTheCallersRows(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("task_started", map[string]any{"role": "Lead", "task_id": "t-1"}))

	static := map[string]any{"role": "Lead", "handle": "lead"}
	s.MergeAgents([]map[string]any{static})
	if _, ok := static["state"]; ok {
		t.Error("MergeAgents wrote into the caller's own row")
	}
}

// --- sandbox projection -------------------------------------------------- //

// fixtureNow is just after the timestamps these fixtures use.
//
// Every sandbox test pins it. The entries are swept on read against the wall
// clock, and a fixture dated in the past — which every fixture with a literal
// date eventually is — would be swept before the assertion ran, so the test
// would fail for a reason that has nothing to do with what it is checking.
var fixtureNow = time.Date(2026, 6, 14, 12, 30, 0, 0, time.UTC)

func sandboxState(t *testing.T) *livestate.LiveState {
	t.Helper()
	return livestate.New(livestate.WithClock(func() time.Time { return fixtureNow }))
}

func sandboxPayload(turnID string) map[string]any {
	return map[string]any{
		"turn_id": turnID, "role": "Coder", "agent_handle": "coder",
		"agent_id": "a-9", "coding_agent": "claude", "sandbox_id": "sb-1",
		"task": "fix the build",
	}
}

func TestASandboxRunIsTrackedThenDropped(t *testing.T) {
	t.Parallel()
	s := sandboxState(t)
	s.Apply(env("sandbox_run_started", sandboxPayload("tn-1")))

	runs := s.ActiveSandboxes()
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	if runs[0].Status != "running" || runs[0].Task != "fix the build" {
		t.Errorf("entry = %+v", runs[0])
	}

	s.Apply(env("sandbox_run_completed", map[string]any{"turn_id": "tn-1"}))
	if runs := s.ActiveSandboxes(); len(runs) != 0 {
		t.Errorf("runs = %+v, want none after completion", runs)
	}
}

func TestAClarificationFlipsARunToAwaitingInput(t *testing.T) {
	t.Parallel()
	s := sandboxState(t)
	s.Apply(env("sandbox_run_started", sandboxPayload("tn-1")))
	s.Apply(env("sandbox_clarification_requested", map[string]any{
		"turn_id": "tn-1", "question": "which branch?", "audience": "author",
	}))

	runs := s.ActiveSandboxes()
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	if runs[0].Status != "awaiting_input" {
		t.Errorf("status = %q, want awaiting_input", runs[0].Status)
	}
	if runs[0].Question != "which branch?" || runs[0].Audience != "author" {
		t.Errorf("entry = %+v", runs[0])
	}
	// The started event's own fields survive the flip.
	if runs[0].Task != "fix the build" {
		t.Errorf("task = %q, want the one the run started with", runs[0].Task)
	}
}

func TestAClarificationWithNoPriorStartSynthesizesAnEntry(t *testing.T) {
	t.Parallel()
	// The API can come up mid-run, so the start may simply have been
	// missed. Dropping the signal would hide a run that is blocked on a
	// human.
	s := sandboxState(t)
	s.Apply(env("sandbox_clarification_requested", map[string]any{
		"turn_id": "tn-1", "role": "Coder", "question": "which branch?",
	}))
	runs := s.ActiveSandboxes()
	if len(runs) != 1 || runs[0].Status != "awaiting_input" {
		t.Fatalf("runs = %+v, want one awaiting-input entry", runs)
	}
	if runs[0].Role != "Coder" {
		t.Errorf("role = %q", runs[0].Role)
	}
}

func TestActiveSandboxesAreOldestFirst(t *testing.T) {
	t.Parallel()
	// Oldest-first so the longest-running job — the one most likely to
	// need attention — sorts to the top of the panel.
	s := sandboxState(t)
	s.Apply(env("sandbox_run_started", sandboxPayload("tn-late"),
		at("2026-06-14T12:05:00+00:00")))
	s.Apply(env("sandbox_run_started", sandboxPayload("tn-early"),
		at("2026-06-14T12:01:00+00:00")))

	runs := s.ActiveSandboxes()
	got := []string{runs[0].TurnID, runs[1].TurnID}
	if !slices.Equal(got, []string{"tn-early", "tn-late"}) {
		t.Errorf("order = %v, want oldest first", got)
	}
}

func TestASandboxEventWithNoTurnIDIsIgnored(t *testing.T) {
	t.Parallel()
	s := sandboxState(t)
	s.Apply(env("sandbox_run_started", map[string]any{"role": "Coder"}))
	if runs := s.ActiveSandboxes(); len(runs) != 0 {
		t.Errorf("runs = %+v, want none: the entry is keyed by turn id", runs)
	}
}

func TestARunWhoseCompletionNeverArrivedIsEventuallyDropped(t *testing.T) {
	t.Parallel()
	// The set is cleared by a completion, and an event stream that can
	// miss a start can miss a completion too. A ghost entry is a false
	// report of work in flight that no operator can clear.
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	s := livestate.New(livestate.WithClock(func() time.Time { return now }))
	s.Apply(env("sandbox_run_started", sandboxPayload("tn-1"),
		at("2026-06-14T12:00:00Z")))

	if runs := s.ActiveSandboxes(); len(runs) != 0 {
		t.Errorf("runs = %+v, want the day-old entry swept", runs)
	}
}

func TestALongRunningJobIsNotSweptFromUnderAnOperator(t *testing.T) {
	t.Parallel()
	// The counterfactual. A detached coding run can legitimately take
	// hours, and a sweep that took them out would be the same false report
	// in the other direction.
	now := time.Date(2026, 6, 14, 20, 0, 0, 0, time.UTC)
	s := livestate.New(livestate.WithClock(func() time.Time { return now }))
	s.Apply(env("sandbox_run_started", sandboxPayload("tn-1"),
		at("2026-06-14T12:00:00Z")))

	if runs := s.ActiveSandboxes(); len(runs) != 1 {
		t.Errorf("runs = %+v, want an eight-hour job kept", runs)
	}
}

func TestAnEntryWithNoTimestampIsKeptRatherThanGuessedAt(t *testing.T) {
	t.Parallel()
	// It cannot be aged out on time, and dropping it on that basis would
	// be arbitrary.
	now := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	s := livestate.New(livestate.WithClock(func() time.Time { return now }))
	s.Apply(env("sandbox_run_started", sandboxPayload("tn-1"), at("")))

	if runs := s.ActiveSandboxes(); len(runs) != 1 {
		t.Errorf("runs = %+v, want the undateable entry kept", runs)
	}
}

func TestASandboxEventDoesNotCreateASeat(t *testing.T) {
	t.Parallel()
	// The sandbox lifecycle maintains its own set and stops there. Letting
	// it fall through would mint a seat entry for the run's role — an
	// offline row for a seat the roster may not even contain, appearing
	// the moment a coding run started.
	s := sandboxState(t)
	change := s.Apply(env("sandbox_run_started", sandboxPayload("tn-1")))

	if !change.Sandboxes {
		t.Error("the sandbox set did not move")
	}
	if len(change.Agents) != 0 {
		t.Errorf("a sandbox event moved seats: %v", change.Agents)
	}
	if got := s.AgentOverlay("Coder"); got != nil {
		t.Errorf("a sandbox event created a seat entry: %+v", got)
	}
}

func TestAnEventWithNoAgentIDKeepsTheKnownRuntimeID(t *testing.T) {
	t.Parallel()
	// Only some events carry the running instance's id. An unconditional
	// write would blank it on the next one that does not, and the seat
	// page would lose the link to the instance mid-turn.
	s := livestate.New()
	s.Apply(env("agent_spawned", map[string]any{"role": "Lead", "agent_id": "a-1"}))
	s.Apply(env("task_started", map[string]any{"role": "Lead", "task_id": "t-1"},
		at("2026-06-14T12:01:00Z")))

	if got := s.RuntimeIDFor("Lead"); got != "a-1" {
		t.Errorf("runtime id = %q, want it kept across an event that carries none", got)
	}
}

func TestAProgressRoundWithNoAgentIDKeepsTheKnownOne(t *testing.T) {
	t.Parallel()
	// The same rule on the progress path, which records it separately.
	s := livestate.New()
	s.Apply(env("agent_spawned", map[string]any{"role": "Lead", "agent_id": "a-1"}))
	s.Apply(env("agent_turn_progress",
		map[string]any{"role": "Lead", "turn_id": "tn-1", "phase": "plan", "round_num": 0},
		streamOnly, at("2026-06-14T12:01:00Z")))

	if got := s.RuntimeIDFor("Lead"); got != "a-1" {
		t.Errorf("runtime id = %q, want it kept", got)
	}
}

func TestNumbersSurviveTheWireTheyActuallyArriveOn(t *testing.T) {
	t.Parallel()
	// Every payload that crossed a broker was JSON, and JSON has one
	// number type — so an integer field arrives as a float64 rather than
	// as the int a Go caller would have put there. A reader that handled
	// only int would report every token count as zero on exactly the path
	// production uses, and never in a test that built its payload by hand.
	raw := []byte(`{
		"id": "e1", "type": "agent_turn_completed", "timestamp": "2026-06-14T12:00:00Z",
		"category": "system",
		"payload": {"role": "Lead", "input_tokens": 12, "output_tokens": 3, "total_tokens": 15}
	}`)
	var e livestate.Envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	s := livestate.New()
	s.Apply(e)

	got := overlayOf(t, s, "Lead")
	if got.InputTokens != 12 || got.TotalTokens != 15 {
		t.Errorf("tokens = %d/%d, want 12/15 off the wire", got.InputTokens, got.TotalTokens)
	}
}

func TestAMistypedNumberReadsAsZeroRatherThanPanicking(t *testing.T) {
	t.Parallel()
	// The payload comes off a wire this process does not control. A string
	// where a count belongs is bad data, not a reason to take the
	// projection down.
	s := livestate.New()
	s.Apply(env("agent_turn_completed", map[string]any{
		"role": "Lead", "total_tokens": "lots",
	}))
	if got := overlayOf(t, s, "Lead").TotalTokens; got != 0 {
		t.Errorf("total tokens = %d, want 0", got)
	}
}

func TestAnAlternateFieldNameIsUsedOnlyWhenTheFirstIsEmpty(t *testing.T) {
	t.Parallel()
	// Several payloads name the same thing two ways. The fallback only
	// helps if an EMPTY first value falls through to it.
	s := livestate.New()
	s.Apply(env("task_started", map[string]any{
		"role": "", "agent_role": "Lead", "task_id": "t-1",
	}))
	if s.AgentOverlay("Lead") == nil {
		t.Fatal("an empty role did not fall through to agent_role")
	}

	s2 := livestate.New()
	s2.Apply(env("task_started", map[string]any{
		"role": "Primary", "agent_role": "Fallback", "task_id": "t-1",
	}))
	if s2.AgentOverlay("Primary") == nil {
		t.Error("the first name lost to its fallback")
	}
	if s2.AgentOverlay("Fallback") != nil {
		t.Error("both names created a seat")
	}
}
