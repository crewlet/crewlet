package livestate_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
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
// between cannot be lost — and arrives both ways.
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
