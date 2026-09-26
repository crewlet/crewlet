package livestate_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
)

// --- what a seat has spent ---------------------------------------------- //

func TestATurnCompletionCarriesNoSecondSpendTotal(t *testing.T) {
	t.Parallel()
	// What a seat SPENT is the spend rollup's per-agent row, folded from
	// the phase records by internal/tokens. The overlay used to keep its
	// own sum of turn totals as well: a second aggregation of the same
	// spend, over whatever turns this process happened to have seen, that
	// no store read could ever reproduce. The turn still moves the seat.
	s := livestate.New()
	s.Apply(env("agent_phase_started", map[string]any{"role": "Lead", "turn_id": "tn-1", "phase": "execute"}))
	change := s.Apply(env("agent_turn_completed", map[string]any{
		"role": "Lead", "turn_id": "tn-1", "input_tokens": 10, "output_tokens": 2, "total_tokens": 12,
	}, id("turn"), at("2026-06-14T12:00:05Z")))

	if _, moved := change.Agents["Lead"]; !moved {
		t.Error("a turn ending did not move its seat")
	}
	rows := s.MergeAgents([]map[string]any{{"role": "Lead"}})
	for _, key := range []string{"input_tokens", "output_tokens", "total_tokens"} {
		if v, present := rows[0][key]; present {
			t.Errorf("the merged seat row carries %s = %v, a total no rollup agrees with", key, v)
		}
	}
	if got := s.SpendRecords(); len(got) != 0 {
		t.Errorf("a turn completion became %d spend records; spend is folded from phases", len(got))
	}
}

// --- the activity feed -------------------------------------------------- //

func TestPersistedEventsAreReturnedNewestFirst(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	for i, kind := range []string{
		"agent_phase_started", "agent_phase_completed", "agent_turn_completed",
	} {
		s.Apply(env(kind, map[string]any{"role": "Lead"}, id(string(rune('a'+i)))))
	}
	feed := s.RecentEvents(0)
	if len(feed) != 3 {
		t.Fatalf("feed = %d rows, want 3", len(feed))
	}
	if feed[0].Type != "agent_turn_completed" {
		t.Errorf("newest row = %q, want agent_turn_completed", feed[0].Type)
	}
	if got := s.RecentEvents(2); len(got) != 2 || got[0].Type != "agent_turn_completed" {
		t.Errorf("limited feed = %v", got)
	}
}

func TestTheFeedIsBoundedAndDropsTheOldest(t *testing.T) {
	t.Parallel()
	s := livestate.New(livestate.WithFeedLimit(3))
	for i := range 6 {
		s.Apply(env("agent_phase_started", map[string]any{"role": "Lead"},
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
	s.Apply(env("agent_phase_started", map[string]any{"role": "Lead"}, streamOnly))
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
	for _, kind := range []string{
		"sandbox_run_failed", "llm_unavailable", "budget_exhausted", "turn.guard_breach",
	} {
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
	s.Apply(env("agent_turn_completed", map[string]any{"role": "Lead"}))
	if feed := s.RecentEvents(0); len(feed) != 1 || feed[0].Failed {
		t.Errorf("feed = %+v, want an unfailed row", feed)
	}
}

// --- merging onto static rows ------------------------------------------- //

func TestTheOverlayIsMergedOntoStaticRows(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("agent_phase_started", map[string]any{
		"role": "Lead", "phase": "execute", "turn_id": "t-1",
	}))

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
	if rows[0]["activity"] != livestate.ActivityWorking {
		t.Errorf("activity = %v, want working", rows[0]["activity"])
	}
	// A ROLE WITH NO LIVE ENTRY STILL HAS A STATE: nothing the events said
	// is needed to know it is waiting for work. A row without one was drawn
	// as offline, which for a seat a peer was serving was simply false.
	if rows[1]["activity"] != livestate.ActivityIdle {
		t.Errorf("a role with no live entry has activity %v, want idle", rows[1]["activity"])
	}
}

func TestMergingDoesNotMutateTheCallersRows(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("agent_phase_started", map[string]any{
		"role": "Lead", "phase": "execute", "turn_id": "t-1",
	}))

	static := map[string]any{"role": "Lead", "handle": "lead"}
	s.MergeAgents([]map[string]any{static})
	if _, ok := static["activity"]; ok {
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

func TestAClarificationFlipsARunToAwaitingAPerson(t *testing.T) {
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
	// THE RUN RECORD'S OWN WORD, which is the one the dashboard asks
	// "is a person needed?" in. This projection used to write
	// `awaiting_input`, which the record cannot, so the answer was never
	// yes for a live run.
	if runs[0].Status != livestate.SandboxAwaiting {
		t.Errorf("status = %q, want %q", runs[0].Status, livestate.SandboxAwaiting)
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
	if len(runs) != 1 || runs[0].Status != livestate.SandboxAwaiting {
		t.Fatalf("runs = %+v, want one entry awaiting a person", runs)
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

func TestAParkedRunOlderThanADayIsStillShown(t *testing.T) {
	t.Parallel()
	// A run parked on a question can rightly wait days for a person, and
	// that is the state this panel most needs to show. It used to be aged
	// out at twelve hours, so the runs that most needed somebody were the
	// ones least likely to be on screen.
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	s := livestate.New(livestate.WithClock(func() time.Time { return now }))
	s.Apply(env("sandbox_run_started", sandboxPayload("tn-1"),
		at("2026-06-14T12:00:00Z")))
	s.Apply(env("sandbox_clarification_requested", map[string]any{
		"turn_id": "tn-1", "role": "Coder", "question": "which branch?",
	}, at("2026-06-14T12:10:00Z")))

	runs := s.ActiveSandboxes()
	if len(runs) != 1 || runs[0].Status != livestate.SandboxAwaiting {
		t.Fatalf("runs = %+v, want the three-day-old parked run still shown", runs)
	}
	if runs[0].PausedAt != "2026-06-14T12:10:00Z" {
		t.Errorf("paused at %q, want the instant it stopped to ask", runs[0].PausedAt)
	}
}

func TestAFailedRunLeavesTheSandboxesSet(t *testing.T) {
	t.Parallel()
	// A run lost to an unreachable box or a stranded claim announces it with
	// sandbox_run_failed, which this projection read nothing of: the run
	// stayed on the panel as running until an age-out took it.
	s := sandboxState(t)
	s.Apply(env("sandbox_run_started", sandboxPayload("tn-1")))
	change := s.Apply(env("sandbox_run_failed", map[string]any{
		"turn_id": "tn-1", "role": "Coder", "reason": "collect_unreachable",
	}, id("e-failed")))

	if !change.Sandboxes {
		t.Error("a lost run did not move the sandbox set")
	}
	if runs := s.ActiveSandboxes(); len(runs) != 0 {
		t.Errorf("runs = %+v, want the lost run gone", runs)
	}
}

func TestAStartedRunNamesItsItemAndItsOwner(t *testing.T) {
	t.Parallel()
	s := sandboxState(t)
	s.Apply(env("sandbox_run_started", with(sandboxPayload("tn-1"), map[string]any{
		"node":      "core-1",
		"work_item": map[string]any{"backend": "native", "id": "t-9", "key": "ENG-9", "project": "p-1"},
	})))
	runs := s.ActiveSandboxes()
	if len(runs) != 1 {
		t.Fatalf("runs = %+v, want one", runs)
	}
	if runs[0].Owner != "core-1" {
		t.Errorf("owner = %q, want the node that announced the start", runs[0].Owner)
	}
	if runs[0].WorkItem == nil || runs[0].WorkItem.Key != "ENG-9" || runs[0].WorkItem.ID != "t-9" {
		t.Errorf("work item = %+v, want ENG-9", runs[0].WorkItem)
	}
}

// record is one durable run as the reconcile is handed it.
func record(turnID string, status livestate.SandboxStatus, launch string, written time.Time) livestate.SandboxRecord {
	return livestate.SandboxRecord{
		Entry: livestate.SandboxEntry{
			TurnID: turnID, Role: "Coder", AgentHandle: "coder", Status: status,
			StartedAt: "2026-06-14T11:00:00Z", Owner: "core-2", Task: "from the record",
		},
		LaunchID: launch, WrittenAt: written,
	}
}

func TestReconcileRemovesARunTheStoreNoLongerHolds(t *testing.T) {
	t.Parallel()
	// The event that would have cleared it never arrived: a completion is
	// as lossy as a start. The durable record no longer holds the run, and
	// the record is the truth about which runs exist.
	s := sandboxState(t)
	s.Apply(env("sandbox_run_started", sandboxPayload("tn-ghost")))

	change := s.ReconcileSandboxes(nil, fixtureNow.Add(time.Second))
	if !change.Sandboxes {
		t.Error("dropping a ghost did not move the sandbox set")
	}
	if runs := s.ActiveSandboxes(); len(runs) != 0 {
		t.Errorf("runs = %+v, want the run the record no longer holds gone", runs)
	}
}

func TestReconcileKeepsARunStartedWhileTheRecordWasRead(t *testing.T) {
	t.Parallel()
	// The listing began before the run's record was written, so it cannot
	// hold it — and the start event this process learned of after the read
	// began is the newer truth.
	s := sandboxState(t)
	s.Apply(env("sandbox_run_started", sandboxPayload("tn-new")))

	s.ReconcileSandboxes(nil, fixtureNow.Add(-time.Second))
	if runs := s.ActiveSandboxes(); len(runs) != 1 {
		t.Errorf("runs = %+v, want the run started during the read kept", runs)
	}
}

func TestReconcileSeedsWhatTheEventsNeverSaid(t *testing.T) {
	t.Parallel()
	// A process that came up while runs were in flight — parked on a
	// question for days, or still coding — saw none of their starts.
	s := sandboxState(t)
	change := s.ReconcileSandboxes([]livestate.SandboxRecord{
		record("tn-parked", livestate.SandboxAwaiting, "l-1", fixtureNow.Add(-72*time.Hour)),
		record("tn-running", livestate.SandboxRunning, "l-2", fixtureNow.Add(-time.Minute)),
		// A run whose turn has taken its result back is over.
		record("tn-resumed", livestate.SandboxStatus("resumed"), "l-3", fixtureNow),
	}, fixtureNow)

	if !change.Sandboxes {
		t.Error("seeding from the record did not move the sandbox set")
	}
	runs := s.ActiveSandboxes()
	var got []string
	for _, r := range runs {
		got = append(got, r.TurnID+"="+string(r.Status))
	}
	slices.Sort(got)
	want := []string{"tn-parked=awaiting_clarification", "tn-running=running"}
	if !slices.Equal(got, want) {
		t.Errorf("runs = %v, want %v", got, want)
	}
	if again := s.ReconcileSandboxes([]livestate.SandboxRecord{
		record("tn-parked", livestate.SandboxAwaiting, "l-1", fixtureNow.Add(-72*time.Hour)),
		record("tn-running", livestate.SandboxRunning, "l-2", fixtureNow.Add(-time.Minute)),
	}, fixtureNow.Add(time.Second)); again.Sandboxes {
		t.Error("a reconcile that found nothing new reported a move, which pushes the panel for nothing")
	}
}

func TestReconcileDoesNotResurrectARunWhoseCompletionLanded(t *testing.T) {
	t.Parallel()
	// The record lags its run's end by the collection the end starts: the
	// completion is announced while the record still says running. Put
	// back, the finished job would be drawn as running until the next read.
	s := sandboxState(t)
	s.Apply(env("sandbox_run_started", sandboxPayload("tn-1")))
	s.Apply(env("sandbox_run_completed", map[string]any{"turn_id": "tn-1", "launch_id": "l-1"}))

	s.ReconcileSandboxes([]livestate.SandboxRecord{
		record("tn-1", livestate.SandboxRunning, "l-1", fixtureNow),
	}, fixtureNow.Add(time.Second))
	if runs := s.ActiveSandboxes(); len(runs) != 0 {
		t.Errorf("runs = %+v, want the completed job kept off the panel", runs)
	}

	// A RELAUNCH in the same turn is another job, and it is shown.
	s.ReconcileSandboxes([]livestate.SandboxRecord{
		record("tn-1", livestate.SandboxRunning, "l-2", fixtureNow.Add(time.Minute)),
	}, fixtureNow.Add(2*time.Second))
	if runs := s.ActiveSandboxes(); len(runs) != 1 {
		t.Errorf("runs = %+v, want the relaunched job shown", runs)
	}
}

func TestReconcileDoesNotResurrectAFailedRun(t *testing.T) {
	t.Parallel()
	// A failure names no job, so the record's own last write decides: one
	// not written since the failure is the failed run's.
	s := sandboxState(t)
	s.Apply(env("sandbox_run_started", sandboxPayload("tn-1")))
	s.Apply(env("sandbox_run_failed", map[string]any{"turn_id": "tn-1"},
		at("2026-06-14T12:10:00Z")))

	stale := time.Date(2026, 6, 14, 12, 9, 0, 0, time.UTC)
	s.ReconcileSandboxes([]livestate.SandboxRecord{
		record("tn-1", livestate.SandboxRunning, "l-1", stale),
	}, fixtureNow)
	if runs := s.ActiveSandboxes(); len(runs) != 0 {
		t.Errorf("runs = %+v, want the failed run kept off the panel", runs)
	}
}

func TestReconcileTakesTheRecordsWordOverAnOlderEvent(t *testing.T) {
	t.Parallel()
	// The event said running; the record, read later, says the run stopped
	// to ask a question whose announcement was lost.
	s := sandboxState(t)
	s.Apply(env("sandbox_run_started", sandboxPayload("tn-1")))
	parked := record("tn-1", livestate.SandboxAwaiting, "l-1", fixtureNow)
	parked.Entry.Question = "which branch?"
	parked.Entry.PausedAt = "2026-06-14T12:20:00Z"

	s.ReconcileSandboxes([]livestate.SandboxRecord{parked}, fixtureNow.Add(time.Second))
	runs := s.ActiveSandboxes()
	if len(runs) != 1 || runs[0].Status != livestate.SandboxAwaiting ||
		runs[0].Question != "which branch?" || runs[0].Owner != "core-2" {
		t.Fatalf("runs = %+v, want the record's parked run", runs)
	}
	// What only the announcement carried survives: its one-line task and
	// the instant the run was announced.
	if runs[0].Task != "fix the build" || runs[0].StartedAt != defaultTS {
		t.Errorf("task %q started %q, want the announcement's", runs[0].Task, runs[0].StartedAt)
	}
}

func TestASandboxEventDoesNotCreateASeatTheCompanyLacks(t *testing.T) {
	t.Parallel()
	// A run moves its seat's state, so a run starting pushes that seat — but
	// only a seat the company HAS. Minting an entry for any role a run names
	// would put a row on screen for a seat the roster may not contain, the
	// moment a coding run of a removed seat started.
	s := sandboxState(t)
	change := s.Apply(env("sandbox_run_started", sandboxPayload("tn-1")))

	if !change.Sandboxes {
		t.Error("the sandbox set did not move")
	}
	if len(change.Agents) != 0 {
		t.Errorf("a sandbox event moved seats the company does not have: %v", change.Agents)
	}
	if got := s.AgentOverlay("Coder"); got != nil {
		t.Errorf("a sandbox event created a seat entry: %+v", got)
	}

	// The company's own seat moves with its run.
	s.SetPlacement(map[string]bool{"Coder": true})
	change = s.Apply(env("sandbox_run_completed", sandboxPayload("tn-1"), id("e2")))
	if _, moved := change.Agents["Coder"]; !moved {
		t.Errorf("a run ending did not push its seat: %v", change.Agents)
	}
	if got := s.AgentOverlay("Coder"); got == nil || got.Activity != livestate.ActivityIdle {
		t.Errorf("overlay = %+v, want the seat idle once its run ended", got)
	}
}

func TestAnEventWithNoAgentIDKeepsTheKnownRuntimeID(t *testing.T) {
	t.Parallel()
	// Only some events carry the running instance's id. An unconditional
	// write would blank it on the next one that does not, and the seat
	// page would lose the link to the instance mid-turn.
	s := livestate.New()
	s.Apply(env("agent_spawned", map[string]any{"role": "Lead", "agent_id": "a-1"}))
	s.Apply(env("agent_phase_started", map[string]any{"role": "Lead", "task_id": "t-1"},
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
		"id": "e1", "type": "agent_phase_completed", "timestamp": "2026-06-14T12:00:00Z",
		"category": "system",
		"payload": {"role": "Lead", "turn_id": "tn-1", "phase": "execute",
			"input_tokens": 12, "output_tokens": 3, "total_tokens": 15}
	}`)
	var e livestate.Envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	s := livestate.New()
	s.Apply(&e)

	records := s.SpendRecords()
	if len(records) != 1 || records[0].InputTokens != 12 || records[0].TotalTokens != 15 {
		t.Errorf("records = %+v, want one at 12/15 off the wire", records)
	}
}

func TestAMistypedNumberReadsAsZeroRatherThanPanicking(t *testing.T) {
	t.Parallel()
	// The payload comes off a wire this process does not control. A string
	// where a count belongs is bad data, not a reason to take the
	// projection down.
	s := livestate.New()
	s.Apply(env("agent_phase_completed", map[string]any{
		"role": "Lead", "turn_id": "tn-1", "phase": "execute", "total_tokens": "lots",
	}))
	if got := s.SpendRecords(); len(got) != 1 || got[0].TotalTokens != 0 {
		t.Errorf("records = %+v, want one at 0 tokens", got)
	}
}

func TestAnAlternateFieldNameIsUsedOnlyWhenTheFirstIsEmpty(t *testing.T) {
	t.Parallel()
	// Several payloads name the same thing two ways. The fallback only
	// helps if an EMPTY first value falls through to it.
	s := livestate.New()
	s.Apply(env("agent_phase_started", map[string]any{
		"role": "", "agent_role": "Lead", "task_id": "t-1",
	}))
	if s.AgentOverlay("Lead") == nil {
		t.Fatal("an empty role did not fall through to agent_role")
	}

	s2 := livestate.New()
	s2.Apply(env("agent_phase_started", map[string]any{
		"role": "Primary", "agent_role": "Fallback", "task_id": "t-1",
	}))
	if s2.AgentOverlay("Primary") == nil {
		t.Error("the first name lost to its fallback")
	}
	if s2.AgentOverlay("Fallback") != nil {
		t.Error("both names created a seat")
	}
}

// THE PUSHED FRAME AND THE FEED ROW AGREE ABOUT THE SAME EVENT.
//
// `failed` is derived once, in Apply, and stamped onto the envelope the client
// is handed as well as onto the feed row the snapshot carries. It used to be
// derived only for the feed row: the live `event` push had no such field, so a
// turn that failed while somebody was watching rendered exactly like one that
// succeeded — and then grew its failure mark on the next reload, when the same
// row came back through the snapshot.
func TestAFailureIsStampedOnTheFrameAndOnTheFeedRow(t *testing.T) {
	t.Parallel()
	s := livestate.New()

	e := &livestate.Envelope{
		ID: "e1", Type: "agent_phase_completed", Timestamp: defaultTS,
		Category: "system", Actor: "Lead",
		Payload: map[string]any{"role": "Lead", "phase": "plan", "failed": true},
	}
	s.Apply(e)

	if !e.Failed {
		t.Error("the frame the client is handed carries no failure mark")
	}
	feed := s.RecentEvents(0)
	if len(feed) != 1 || !feed[0].Failed {
		t.Fatalf("the feed row disagrees with the frame: %+v", feed)
	}

	// And an ordinary event is marked on neither, so the mark still means
	// something.
	ok := &livestate.Envelope{
		ID: "e2", Type: "agent_phase_completed", Timestamp: defaultTS,
		Category: "system", Actor: "Lead",
		Payload: map[string]any{"role": "Lead", "phase": "plan"},
	}
	s.Apply(ok)
	if ok.Failed {
		t.Error("an ordinary event is marked failed")
	}
}
