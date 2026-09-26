package livestate_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
)

const seedNow = "2026-06-14T12:00:00Z"

func seededState(t *testing.T) *livestate.LiveState {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, seedNow)
	if err != nil {
		t.Fatalf("parse the pinned clock: %v", err)
	}
	return livestate.New(livestate.WithClock(func() time.Time { return at }))
}

func storedRow(id, ts string) livestate.FeedRow {
	return livestate.FeedRow{
		ID: id, Type: "agent_turn_completed", Timestamp: ts,
		Category: "agent", Summary: id, Topic: "crewlet.events.agent_turn_completed",
	}
}

func storedSpend(id, ts string, total int) tokens.Record {
	return tokens.Record{
		EventID: id, Timestamp: ts, AgentRole: "Lead", AgentID: "a-1",
		Phase: "execute", Model: "claude-sonnet-5", TurnID: "tn-" + id,
		InputTokens: total / 2, OutputTokens: total / 2, TotalTokens: total,
	}
}

func feedIDs(rows []livestate.FeedRow) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.ID)
	}
	return out
}

// HISTORY IS WHAT THE PROJECTION STARTS FROM, rather than nothing at all.
//
// Every screen the projection feeds used to start empty in every process: a
// restart, a deploy or a node joining a fleet showed an operator a company
// that had apparently done nothing, beside a store that said otherwise.
// A FULL SEED DOES NOT COUNT THE LIVE ROWS IT ALREADY HOLDS A SECOND TIME.
//
// The seed runs after the broadcast subscription is attached, so phases that
// complete in between are in the projection AND in the store the seed reads.
// While the dedupe was a bounded set capped at the number of records the seed
// reads, those live ids sat at the FRONT of its eviction order: a seed that
// filled the cap evicted every one of them before its own loop reached the
// store's copies of those same phases, and each was counted twice. The rollup
// then over-reported the day by exactly the overlap, which is the figure the
// company's headroom is read from.
func TestAFullSeedDoesNotRecountTheLiveRowsItAlreadyHolds(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	base := time.Now().UTC().Add(-12 * time.Hour)
	newest := base.Add(11 * time.Hour).Format(time.RFC3339Nano)

	// Ten phases land between the subscribe and the read. Five are this
	// node's own, so its store holds them too; five are peers', so they
	// reach the projection only off the stream.
	live := make([]string, 0, 10)
	for i := range 10 {
		id := fmt.Sprintf("L%d", i)
		live = append(live, id)
		s.Apply(phaseSpend(id, newest, 7))
	}

	// And the store answers a FULL window, which is what makes the cap bind.
	recs := make([]tokens.Record, 0, livestate.SpendRecordLimit)
	for i := range livestate.SpendRecordLimit - 5 {
		recs = append(recs, tokens.Record{
			EventID:   fmt.Sprintf("S%d", i),
			Timestamp: base.Add(time.Duration(i) * time.Millisecond).Format(time.RFC3339Nano),
			AgentRole: "Lead", Phase: "execute", TotalTokens: 1,
		})
	}
	for i := 0; i < 10; i += 2 {
		recs = append(recs, tokens.Record{
			EventID: live[i], Timestamp: newest,
			AgentRole: "Lead", Phase: "execute", TotalTokens: 7,
		})
	}
	s.Seed(livestate.History{Spend: recs})

	seen := map[string]int{}
	for _, r := range s.SpendRecords() {
		seen[r.EventID]++
	}
	var twice []string
	for id, n := range seen {
		if n > 1 {
			twice = append(twice, fmt.Sprintf("%s x%d", id, n))
		}
	}
	slices.Sort(twice)
	if len(twice) > 0 {
		t.Fatalf("counted more than once: %v; the rollup over-reports the day "+
			"by every one of them", twice)
	}
}

func TestSeedingFillsTheFeedAndTheSpendWindow(t *testing.T) {
	t.Parallel()
	s := seededState(t)
	change := s.Seed(livestate.History{
		Events: []livestate.FeedRow{
			storedRow("e2", "2026-06-14T11:30:00Z"),
			storedRow("e1", "2026-06-14T11:00:00Z"),
		},
		Spend: []tokens.Record{
			storedSpend("p2", "2026-06-14T11:30:00Z", 20),
			storedSpend("p1", "2026-06-14T11:00:00Z", 10),
		},
	})

	if !change.Events || !change.Tokens {
		t.Errorf("change = %+v, want both the feed and the rollup reported moved", change)
	}
	if got := feedIDs(s.RecentEvents(0)); len(got) != 2 || got[0] != "e2" {
		t.Errorf("feed = %v, want the stored rows newest first", got)
	}
	rollup := tokens.Aggregate(s.SpendRecords(), tokens.Options{})
	if rollup.Totals.TotalTokens != 30 || rollup.Totals.Calls != 2 {
		t.Errorf("rollup = %d tokens over %d calls, want 30 over 2",
			rollup.Totals.TotalTokens, rollup.Totals.Calls)
	}
	if len(rollup.ByAgent) != 1 || rollup.ByAgent[0].Role != "Lead" {
		t.Errorf("by_agent = %+v, want the seat the stored phases named", rollup.ByAgent)
	}
}

// THE LIVE STREAM AND THE STORE OVERLAP, and the overlap is counted once.
//
// The caller subscribes BEFORE it reads the store, so an event published in
// between cannot be lost, and arrives both ways.
func TestSeedingDoesNotCountWhatTheStreamAlreadyApplied(t *testing.T) {
	t.Parallel()
	s := seededState(t)
	live := phaseSpend("p1", "2026-06-14T11:45:00Z", 40)
	s.Apply(live)
	s.Apply(env("agent_turn_completed", map[string]any{"role": "Lead"},
		id("e1"), at("2026-06-14T11:45:00Z")))

	s.Seed(livestate.History{
		Events: []livestate.FeedRow{
			storedRow("e1", "2026-06-14T11:45:00Z"),
			storedRow("e0", "2026-06-14T11:00:00Z"),
		},
		Spend: []tokens.Record{
			storedSpend("p1", "2026-06-14T11:45:00Z", 40),
			storedSpend("p0", "2026-06-14T11:00:00Z", 10),
		},
	})

	seen := 0
	for _, id := range feedIDs(s.RecentEvents(0)) {
		if id == "e1" {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("the row that arrived live and was also stored appears %d times in %v",
			seen, feedIDs(s.RecentEvents(0)))
	}
	rollup := tokens.Aggregate(s.SpendRecords(), tokens.Options{})
	if rollup.Totals.TotalTokens != 50 || rollup.Totals.Calls != 2 {
		t.Errorf("rollup = %d tokens over %d calls, want 50 over 2: a record was counted twice",
			rollup.Totals.TotalTokens, rollup.Totals.Calls)
	}
}

// AND IN THE OTHER ORDER, which the store makes the likelier one. The node that
// publishes an event writes its row inline, before the broker has delivered
// anything to anybody, so the seed can list an event the stream has not handed
// over yet. When the envelope lands it is the same event: listed once, counted
// once, and still pushed, because the envelope is the only frame that carries
// the payload a client keeps for a completed phase.
func TestAnEventTheSeedAlreadyListedIsListedOnceWhenItStreams(t *testing.T) {
	t.Parallel()
	s := seededState(t)
	stored := storedRow("p1", "2026-06-14T11:45:00Z")
	stored.Type = "agent_phase_completed"
	s.Seed(livestate.History{
		Events: []livestate.FeedRow{stored, storedRow("e0", "2026-06-14T11:00:00Z")},
		Spend:  []tokens.Record{storedSpend("p1", "2026-06-14T11:45:00Z", 40)},
	})

	change := s.Apply(phaseSpend("p1", "2026-06-14T11:45:00Z", 40))

	if got, want := feedIDs(s.RecentEvents(0)), []string{"p1", "e0"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("feed = %v, want %v: the streamed event was listed a second time", got, want)
	}
	if !change.Events {
		t.Error("the streamed event was not pushed, and its envelope is the only frame carrying its payload")
	}
	rollup := tokens.Aggregate(s.SpendRecords(), tokens.Options{})
	if rollup.Totals.TotalTokens != 40 || rollup.Totals.Calls != 1 {
		t.Errorf("rollup = %d tokens over %d calls, want 40 over 1: the seeded phase was counted again",
			rollup.Totals.TotalTokens, rollup.Totals.Calls)
	}
}

// HISTORY LANDS BEHIND THE LIVE ROWS IT PREDATES. The feed is read
// newest-first, and a seed that appended would put an hour-old row above the
// one a reader just watched arrive.
func TestSeededRowsAreOrderedAgainstLiveOnes(t *testing.T) {
	t.Parallel()
	s := seededState(t)
	s.Apply(env("agent_turn_completed", map[string]any{"role": "Lead"},
		id("live"), at("2026-06-14T11:59:00Z")))
	s.Seed(livestate.History{Events: []livestate.FeedRow{
		storedRow("old", "2026-06-14T09:00:00Z"),
		storedRow("older", "2026-06-14T08:00:00Z"),
	}})

	want := []string{"live", "old", "older"}
	if got := feedIDs(s.RecentEvents(0)); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("feed = %v, want %v", got, want)
	}
}

// THE SEED IS BOUNDED BY THE PROJECTION'S OWN LIMITS, never by what the store
// happened to return: the ring keeps its newest rows and the window drops what
// has aged out of it, measured from NOW rather than from an arriving event.
func TestSeedingRespectsTheRingAndTheWindow(t *testing.T) {
	t.Parallel()
	s := livestate.New(livestate.WithFeedLimit(3),
		livestate.WithClock(func() time.Time {
			return time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
		}))
	var rows []livestate.FeedRow
	for i := range 5 {
		rows = append(rows, storedRow(fmt.Sprintf("e%d", i),
			time.Date(2026, 6, 14, 10, i, 0, 0, time.UTC).Format(time.RFC3339Nano)))
	}
	s.Seed(livestate.History{
		Events: rows,
		Spend: []tokens.Record{
			storedSpend("inside", "2026-06-14T11:00:00Z", 10),
			// Older than LiveSpendWindow, measured from the pinned clock.
			storedSpend("aged-out", "2026-06-12T11:00:00Z", 999),
		},
	})

	want := []string{"e4", "e3", "e2"}
	if got := feedIDs(s.RecentEvents(0)); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("feed = %v, want the newest %v", got, want)
	}
	records := s.SpendRecords()
	if len(records) != 1 || records[0].EventID != "inside" {
		t.Errorf("records = %+v, want only the one inside the window", records)
	}
}

// AN EMPTY STORE MOVES NOTHING, so a caller cannot report a seed that had
// nothing to seed as a change.
func TestSeedingNothingMovesNothing(t *testing.T) {
	t.Parallel()
	s := seededState(t)
	if change := s.Seed(livestate.History{}); change.Moved() {
		t.Errorf("change = %+v, want nothing moved", change)
	}
}

func TestSeedReportsWhichNodesItCovered(t *testing.T) {
	t.Parallel()
	// The seed reads every live node's store. Which ones answered is what
	// says whether the history a restarted node shows is the fleet's or is
	// missing a node, and a projection that kept it to itself would make a
	// short feed indistinguishable from a quiet company.
	s := seededState(t)
	if _, seeded := s.SeededFrom(); seeded {
		t.Fatal("a projection nothing seeded reported a seed")
	}
	coverage := eventfan.Coverage{Nodes: []eventfan.NodeCoverage{
		{ID: "core-1", Answered: true},
		{ID: "core-2", Error: "no answer within the 2s fleet read budget"},
	}}
	s.Seed(livestate.History{Coverage: coverage})

	got, seeded := s.SeededFrom()
	if !seeded {
		t.Fatal("a seed that ran did not say so")
	}
	if got.Complete || !slices.Equal(got.Missing(), []string{"core-2"}) {
		t.Errorf("seeded from %+v, want core-2 named as missing", got)
	}
	got.Nodes[0].ID = "mutated"
	if again, _ := s.SeededFrom(); again.Nodes[0].ID != "core-1" {
		t.Error("the coverage handed out aliases the projection's own")
	}
}

func TestSeedSetsEachSeatsLastTurnAndAParkedOne(t *testing.T) {
	t.Parallel()
	// "Idle · last turn 24m ago" has to survive a restart, and so does a
	// turn parked on a coding run: the run is a durable record and a box,
	// not a goroutine, and the turn is still the seat's.
	s := seededState(t)
	base := time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC)
	item := &types.WorkItem{Backend: "native", ID: "t-1", Key: "ENG-1"}
	change := s.Seed(livestate.History{Turns: []store.Turn{
		{TurnID: "old", AgentRole: "Lead", StartedAt: base, EndedAt: base.Add(time.Minute), Complete: true},
		{TurnID: "newer", AgentRole: "Lead", StartedAt: base.Add(time.Hour),
			EndedAt: base.Add(time.Hour + time.Minute), Complete: true, Failed: true},
		// Running or died: nobody can say which from the store.
		{TurnID: "unfinished", AgentRole: "Lead", StartedAt: base.Add(2 * time.Hour),
			EndedAt: base.Add(2 * time.Hour)},
		{TurnID: "waiting", AgentRole: "Coder", StartedAt: base, EndedAt: base.Add(time.Minute),
			Parked: true, WorkItem: item},
	}})
	if _, ok := change.Agents["Lead"]; !ok {
		t.Error("seeding a last turn did not report the seat as moved")
	}

	lead := overlayOf(t, s, "Lead")
	if lead.LastTurn == nil || lead.LastTurn.TurnID != "newer" ||
		lead.LastTurn.Outcome != livestate.OutcomeFailed ||
		lead.LastTurn.EndedAt != base.Add(time.Hour+time.Minute).Format(time.RFC3339Nano) {
		t.Errorf("last turn = %+v, want the newest ENDED turn, failed", lead.LastTurn)
	}
	if lead.Turn != nil {
		t.Errorf("turn = %+v, want none claimed for a turn nobody can say is running", lead.Turn)
	}
	coder := overlayOf(t, s, "Coder")
	if coder.Turn == nil || coder.Turn.TurnID != "waiting" || coder.Turn.Stage != livestate.StageParked ||
		coder.Turn.WorkItem == nil || coder.Turn.WorkItem.Key != "ENG-1" {
		t.Errorf("turn = %+v, want the parked turn on its item", coder.Turn)
	}
	// A PARKED TURN IS NOT WORK OF ITS OWN: what its run is doing is the
	// run record's to say, and the record is read separately. The seed
	// claims nothing for the seat.
	if coder.Activity != livestate.ActivityIdle {
		t.Errorf("activity = %q, want idle until the run record says what the run is doing",
			coder.Activity)
	}
	if lead.Activity != livestate.ActivityIdle {
		t.Errorf("activity = %q, want idle with only ended turns seeded", lead.Activity)
	}
}

func TestSeedDoesNotOverwriteWhatTheStreamAlreadySaid(t *testing.T) {
	t.Parallel()
	// The caller subscribes first and reads second, so the stream can land
	// a newer turn before the seed does.
	s := seededState(t)
	s.Apply(env("agent_turn_completed", map[string]any{"role": "Lead", "turn_id": "live"},
		at("2026-06-14T11:59:00Z")))
	base := time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC)
	s.Seed(livestate.History{Turns: []store.Turn{
		{TurnID: "stored", AgentRole: "Lead", StartedAt: base, EndedAt: base, Complete: true},
	}})
	if last := overlayOf(t, s, "Lead").LastTurn; last == nil || last.TurnID != "live" {
		t.Errorf("last turn = %+v, want the stream's newer one kept", last)
	}
}
