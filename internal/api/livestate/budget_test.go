package livestate_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/tokens"
)

func meterReport(meterID string, seq int, agents ...map[string]any) map[string]any {
	rows := make([]any, 0, len(agents))
	for _, a := range agents {
		rows = append(rows, a)
	}
	return map[string]any{
		"meter_id": meterID, "seq": seq,
		"org_used_tokens": 500, "org_max_tokens": 1000,
		"agents": rows,
	}
}

func seatMeter(role string, used, max int) map[string]any {
	return map[string]any{
		"role": role, "agent_id": "a-1",
		"used_tokens": used, "max_tokens": max,
	}
}

func TestAMeterReportLandsOnTheSeatAndTheOrg(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("budget_reported", meterReport("m-1", 1, seatMeter("Lead", 100, 400)), streamOnly))

	org := s.Budget()
	if org.MeterID != "m-1" || org.Org.Used != 500 || org.Org.Max != 1000 {
		t.Errorf("org meter = %+v", org)
	}
	seat := overlayOf(t, s, "Lead").Budget
	if seat == nil || seat.Used != 100 || seat.Max != 400 {
		t.Errorf("seat meter = %+v", seat)
	}
}

func TestTheMeterNeverEntersTheActivityFeed(t *testing.T) {
	t.Parallel()
	// Stream-only: a report is a snapshot of a counter that moves every
	// round, so a persisted copy replayed from history would show figures
	// the counter left behind as the current ones.
	s := livestate.New()
	change := s.Apply(env("budget_reported", meterReport("m-1", 1)))
	if change.Events {
		t.Error("a meter report was recorded in the feed")
	}
	if got := s.RecentEvents(0); len(got) != 0 {
		t.Errorf("feed = %v, want empty", got)
	}
}

func TestAnOlderReportCannotWalkTheMeterBackwards(t *testing.T) {
	t.Parallel()
	// Broker ordering holds only within a topic and the API reads a
	// broadcast subscription across all of them.
	s := livestate.New()
	s.Apply(env("budget_reported", meterReport("m-1", 5, seatMeter("Lead", 300, 400)), streamOnly))
	s.Apply(env("budget_reported", meterReport("m-1", 2, seatMeter("Lead", 100, 400)), streamOnly))

	if got := overlayOf(t, s, "Lead").Budget; got.Used != 300 {
		t.Errorf("used = %d, want 300: an older report walked the meter back", got.Used)
	}
	if got := s.Budget().Seq; got != 5 {
		t.Errorf("seq = %d, want 5", got)
	}
}

func TestARepeatedSeqIsDropped(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("budget_reported", meterReport("m-1", 3, seatMeter("Lead", 300, 400)), streamOnly))
	change := s.Apply(env("budget_reported", meterReport("m-1", 3, seatMeter("Lead", 999, 400)), streamOnly))

	if change.Moved() {
		t.Error("a repeated seq moved the projection")
	}
	if got := overlayOf(t, s, "Lead").Budget.Used; got != 300 {
		t.Errorf("used = %d, want the held 300", got)
	}
}

func TestANewMeterReplacesRatherThanMerges(t *testing.T) {
	t.Parallel()
	// Each report is a complete snapshot of the shared counter, and a reset
	// legitimately lowers it. Merging, or taking a maximum, would pin a
	// high-water mark that no later report could clear.
	s := livestate.New()
	s.Apply(env("budget_reported", meterReport("m-1", 9, seatMeter("Lead", 900, 1000)), streamOnly))
	s.Apply(env("budget_reported", meterReport("m-2", 1, seatMeter("Lead", 10, 1000)), streamOnly))

	if got := s.Budget().MeterID; got != "m-2" {
		t.Errorf("meter id = %q, want the newer report's", got)
	}
	if got := overlayOf(t, s, "Lead").Budget.Used; got != 10 {
		t.Errorf("used = %d, want 10: the newer report was merged with an older one", got)
	}
}

func TestASeatThatLostItsMeterLosesItsBar(t *testing.T) {
	t.Parallel()
	// Only metered seats are reported. A cap edited down to zero or a
	// decommissioned role must lose its bar rather than keep the last
	// figure it had.
	s := livestate.New()
	s.Apply(env("budget_reported", meterReport("m-1", 1,
		seatMeter("Lead", 100, 400), seatMeter("Dev", 50, 400)), streamOnly))
	s.Apply(env("budget_reported", meterReport("m-1", 2, seatMeter("Lead", 120, 400)), streamOnly))

	if got := overlayOf(t, s, "Dev").Budget; got != nil {
		t.Errorf("Dev budget = %+v, want none", got)
	}
	if got := overlayOf(t, s, "Lead").Budget; got == nil || got.Used != 120 {
		t.Errorf("Lead budget = %+v", got)
	}
}

func TestNoMeterReportsAsNoMeter(t *testing.T) {
	t.Parallel()
	// nil covers two situations that look the same from here and read the
	// same on screen: the seat has no per-agent budget, or no engine is
	// reporting at all. Either way a bar drawn without one would be a
	// claim nobody measured.
	s := livestate.New()
	s.Apply(env("agent_phase_started", map[string]any{"role": "Lead", "phase": "execute"}))

	if got := overlayOf(t, s, "Lead").Budget; got != nil {
		t.Errorf("budget = %+v, want none", got)
	}
	if got := s.Budget(); got.MeterID != "" {
		t.Errorf("org budget = %+v, want empty", got)
	}
}

// --- the live spend window ---------------------------------------------- //

func phaseSpend(eventID, ts string, total int) *livestate.Envelope {
	return env("agent_phase_completed", map[string]any{
		"role": "Lead", "agent_id": "a-1", "phase": "plan",
		"model": "claude-sonnet-5", "turn_id": "tn-1",
		"input_tokens": total / 2, "output_tokens": total / 2, "total_tokens": total,
	}, id(eventID), at(ts))
}

func TestRecordsInsideTheWindowAreKept(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(phaseSpend("p1", "2026-06-14T12:00:00Z", 10))
	s.Apply(phaseSpend("p2", "2026-06-14T13:00:00Z", 20))

	records := s.SpendRecords()
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	if records[0].Model != "claude-sonnet-5" || records[0].AgentRole != "Lead" {
		t.Errorf("record = %+v", records[0])
	}
}

func TestARedeliveredPhaseIsNotCountedTwice(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	spend := phaseSpend("p1", "2026-06-14T12:00:00Z", 10)
	if !s.Apply(spend).Tokens {
		t.Fatal("the first delivery did not count")
	}
	if s.Apply(spend).Tokens {
		t.Error("a redelivered phase counted again")
	}
	if got := len(s.SpendRecords()); got != 1 {
		t.Errorf("records = %d, want 1", got)
	}
}

func TestRecordsOlderThanTheWindowAreDropped(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(phaseSpend("old", "2026-06-13T00:00:00Z", 10))
	s.Apply(phaseSpend("new", "2026-06-14T12:00:00Z", 20))

	records := s.SpendRecords()
	if len(records) != 1 || records[0].EventID != "new" {
		t.Errorf("records = %+v, want only the recent one", records)
	}
}

func TestPruningSurvivesAnOutOfOrderHead(t *testing.T) {
	t.Parallel()
	// Popping from the front is only correct while the slice is
	// timestamp-ordered, and the live path does not keep it so: events on
	// different topics arrive in no order between them, and a fleet's
	// clocks disagree. One recent record at the head is enough to make a
	// head-popping loop exit immediately and never prune again, and the
	// window would silently stop being a window.
	s := livestate.New()
	s.Apply(phaseSpend("live", "2026-06-14T12:00:00Z", 10))
	s.Apply(phaseSpend("late-old", "2026-06-12T00:00:00Z", 20))
	s.Apply(phaseSpend("trigger", "2026-06-14T12:30:00Z", 5))

	for _, record := range s.SpendRecords() {
		if record.EventID == "late-old" {
			t.Error("a record behind a recent head was never pruned")
		}
	}
}

func TestAnUnparseableTimestampDoesNotPruneTheWindow(t *testing.T) {
	t.Parallel()
	// The cutoff cannot be computed from a timestamp that is not one, and
	// pruning against a zero cutoff would empty the window.
	s := livestate.New()
	s.Apply(phaseSpend("p1", "2026-06-14T12:00:00Z", 10))
	s.Apply(phaseSpend("p2", "not-a-timestamp", 20))

	if got := len(s.SpendRecords()); got != 2 {
		t.Errorf("records = %d, want both kept", got)
	}
}

func TestSpendRecordsDoNotAliasTheProjection(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(phaseSpend("p1", "2026-06-14T12:00:00Z", 10))
	held := s.SpendRecords()
	s.Apply(phaseSpend("p2", "2026-06-14T12:05:00Z", 20))
	if len(held) != 1 {
		t.Errorf("a snapshot taken earlier grew to %d records", len(held))
	}
}

func TestARestartedEnginesFirstReportIsNotRefusedAsOld(t *testing.T) {
	t.Parallel()
	// Sequence numbers are per meter, so a restarted node's first report
	// starts low. Comparing it against another meter's would refuse it,
	// and the dashboard would hold a stale frame until the new meter
	// happened to pass the old one's sequence.
	s := livestate.New()
	s.Apply(env("budget_reported", meterReport("m-1", 99, seatMeter("Lead", 900, 1000)), streamOnly))
	s.Apply(env("budget_reported", meterReport("m-2", 1, seatMeter("Lead", 5, 1000)), streamOnly))

	if got := s.Budget().Seq; got != 1 {
		t.Errorf("seq = %d, want the new meter's 1", got)
	}
	if got := overlayOf(t, s, "Lead").Budget.Used; got != 5 {
		t.Errorf("used = %d, want the new meter's 5", got)
	}
}

func TestANewMeterDropsASeatItDoesNotMention(t *testing.T) {
	t.Parallel()
	// The case that says another meter's report replaces rather than
	// merges: a seat the newer report does not meter (a node already on a
	// revision that dropped its cap) must lose its bar, not keep a figure
	// nothing reports any more.
	s := livestate.New()
	s.Apply(env("budget_reported", meterReport("m-1", 4,
		seatMeter("Lead", 900, 1000), seatMeter("Dev", 700, 1000)), streamOnly))
	s.Apply(env("budget_reported", meterReport("m-2", 1, seatMeter("Lead", 5, 1000)), streamOnly))

	if got := overlayOf(t, s, "Dev").Budget; got != nil {
		t.Errorf("Dev budget = %+v, want none: nothing meters it any more", got)
	}
}

func TestASpendRecordWithNoUsableTimestampIsKept(t *testing.T) {
	t.Parallel()
	// The same rule the sandbox sweep follows: a record that cannot be
	// aged out on time must not be dropped on that basis. The count cap is
	// what bounds those.
	s := livestate.New()
	s.Apply(phaseSpend("undateable", "", 10))
	s.Apply(phaseSpend("old", "2026-06-12T00:00:00Z", 5))
	// Applied LAST because the sweep runs against the incoming event's own
	// timestamp: an out-of-order old arrival computes an old cutoff and
	// prunes nothing, and the next in-window event is what clears it.
	s.Apply(phaseSpend("recent", "2026-06-14T12:00:00Z", 20))

	var ids []string
	for _, record := range s.SpendRecords() {
		ids = append(ids, record.EventID)
	}
	if len(ids) != 2 {
		t.Fatalf("records = %v, want the undateable one and the recent one", ids)
	}
	for _, want := range []string{"undateable", "recent"} {
		found := false
		for _, got := range ids {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("records = %v, missing %q", ids, want)
		}
	}
}

func TestSpendRecordsAreCappedByCount(t *testing.T) {
	t.Parallel()
	// The real bound is the window; this only binds for an org emitting
	// more than the cap in a day. Truncation drops the OLDEST records, so
	// an org past the cap sees a rollup covering slightly less than a day
	// rather than a wrong total.
	s := livestate.New()
	const beyondCap = 8_100
	for i := range beyondCap {
		// All inside the window, so only the count cap can bind.
		ts := time.Date(2026, 6, 14, 12, 0, 0, i*1000, time.UTC).Format(time.RFC3339Nano)
		s.Apply(phaseSpend(fmt.Sprintf("p%05d", i), ts, 1))
	}
	records := s.SpendRecords()
	if len(records) > 8_000 {
		t.Errorf("records = %d, want the cap to bind", len(records))
	}
	if len(records) == 0 {
		t.Fatal("the cap emptied the window")
	}
	// The OLDEST went, not the newest: a rollup missing today's spend
	// would be a wrong total rather than a shorter window.
	if records[len(records)-1].EventID != fmt.Sprintf("p%05d", beyondCap-1) {
		t.Errorf("newest kept = %q, want the last one applied",
			records[len(records)-1].EventID)
	}
	if records[0].EventID == "p00000" {
		t.Error("the oldest record survived a truncation past the cap")
	}
}

// A RESTART IS NOT A DAY OF ZEROES.
//
// The projection is fed by one ephemeral subscription, so it starts empty and
// fills only as new phases complete — while the event store beside it holds
// the whole window. Every screen reading this rollup then said the company had
// spent nothing, in a window it labelled a full day, next to a chart drawn
// from the store showing the real spend.
func TestTheSeedFillsTheWindowFromWhatTheStoreAlreadyHolds(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	now := time.Now().UTC()
	rec := func(id string, at time.Time, total int) tokens.Record {
		return tokens.Record{
			EventID: id, Timestamp: at.Format(time.RFC3339Nano),
			AgentRole: "Lead", Phase: "plan", TotalTokens: total,
		}
	}
	change := s.Seed(livestate.History{Spend: []tokens.Record{
		rec("h1", now.Add(-2*time.Hour), 10),
		rec("h2", now.Add(-time.Hour), 20),
	}})
	if !change.Tokens || len(s.SpendRecords()) != 2 {
		t.Fatalf("seeded tokens=%v, holding %d; want both records", change.Tokens, len(s.SpendRecords()))
	}

	// DEDUPED AGAINST WHAT IS ALREADY HERE, in both directions, which is
	// what makes the ORDER of the seed and the subscription a non-question:
	// the caller subscribes before it seeds, so a live event lands ahead of
	// the older records this appends behind it.
	if again := s.Seed(livestate.History{Spend: []tokens.Record{rec("h1", now.Add(-2*time.Hour), 10)}}); again.Tokens {
		t.Error("a second seed counted a record it had already counted; a rollup " +
			"that grows on every reload is worse than one that is slightly short")
	}
	s.Apply(phaseSpend("h2", now.Add(-time.Hour).Format(time.RFC3339Nano), 20))
	if held := len(s.SpendRecords()); held != 2 {
		t.Errorf("holding %d records after the stream redelivered a seeded "+
			"one; want the same 2", held)
	}

	// AND A RECORD WITH NO ID IS DROPPED: the dedupe has nothing to hold it
	// by, so a second seed would add it again.
	if landed := s.Seed(livestate.History{Spend: []tokens.Record{rec("", now, 5)}}); landed.Tokens {
		t.Error("an id-less record landed; it cannot be deduped, so it is summed twice")
	}

	// THE WINDOW STILL BINDS. A record older than the live window is
	// pruned rather than seeded in, so a seed cannot widen the window the
	// rollup claims to cover.
	s.Seed(livestate.History{Spend: []tokens.Record{rec("old", now.Add(-livestate.LiveSpendWindow-3*time.Hour), 99)}})
	for _, r := range s.SpendRecords() {
		if r.EventID == "old" {
			t.Error("a record from outside the live window survived the seed")
		}
	}
}

func TestADelayedReportFromAnotherNodeCannotWalkTheMeterBackwards(t *testing.T) {
	t.Parallel()
	// Every node reports the same shared counter under its own meter id,
	// and their sequence numbers are unrelated. A frame node B read ten
	// seconds before node A's last one, delivered late, must not replace
	// A's: it is the same counter, read earlier.
	s := livestate.New()
	s.Apply(env("budget_reported", meterReport("node-a", 6, seatMeter("Lead", 300, 400)),
		streamOnly, at("2026-06-14T12:00:30Z")))
	change := s.Apply(env("budget_reported", meterReport("node-b", 40, seatMeter("Lead", 250, 400)),
		streamOnly, at("2026-06-14T12:00:20Z")))

	if change.Moved() {
		t.Error("an older read from another node moved the projection")
	}
	if got := overlayOf(t, s, "Lead").Budget.Used; got != 300 {
		t.Errorf("used = %d, want the newer read's 300", got)
	}
	if got := s.Budget().MeterID; got != "node-a" {
		t.Errorf("meter id = %q, want node-a's report held", got)
	}

	// And a LATER read from that node is taken, whatever its seq.
	s.Apply(env("budget_reported", meterReport("node-b", 41, seatMeter("Lead", 320, 400)),
		streamOnly, at("2026-06-14T12:00:45Z")))
	if got := overlayOf(t, s, "Lead").Budget.Used; got != 320 {
		t.Errorf("used = %d, want node-b's newer read of 320", got)
	}
}

func TestAnUndatedReportDoesNotEraseTheReorderGuard(t *testing.T) {
	t.Parallel()
	// A report with no usable timestamp is applied, as every guard here
	// lets one through, but the instant the next report is compared
	// against must survive it.
	s := livestate.New()
	s.Apply(env("budget_reported", meterReport("node-a", 1, seatMeter("Lead", 300, 400)),
		streamOnly, at("2026-06-14T12:00:30Z")))
	s.Apply(env("budget_reported", meterReport("node-b", 1, seatMeter("Lead", 310, 400)),
		streamOnly, at("")))
	s.Apply(env("budget_reported", meterReport("node-c", 1, seatMeter("Lead", 100, 400)),
		streamOnly, at("2026-06-14T12:00:10Z")))

	if got := overlayOf(t, s, "Lead").Budget.Used; got != 310 {
		t.Errorf("used = %d, want 310: a report older than the last dated one was applied", got)
	}
}

func TestAMeterReportClaimsNothingAboutWhetherASeatIsRunning(t *testing.T) {
	t.Parallel()
	// A report names every capped seat whether or not anything is running
	// it. The overlay it creates used to start at "offline" and overwrite
	// the roster's own state, so the first report after a boot flipped
	// every capped seat this node serves from idle to offline, on every
	// open dashboard, until the seat took a turn.
	s := livestate.New()
	s.Apply(env("budget_reported", meterReport("m-1", 1, seatMeter("Lead", 100, 400)), streamOnly))

	rows := s.MergeAgents([]map[string]any{{"role": "Lead", "handle": "lead", "state": "idle"}})
	if got := rows[0]["state"]; got != "idle" {
		t.Errorf("merged state = %v, want the roster's idle kept", got)
	}
	if rows[0]["budget"] == nil {
		t.Error("the merged row lost the meter the report carried")
	}
	pushed := s.OverlayRows([]string{"Lead"})
	if len(pushed) != 1 {
		t.Fatalf("pushed rows = %v, want the one seat the report moved", pushed)
	}
	if got, present := pushed[0]["state"]; present {
		t.Errorf("the agents push carried state %v, which a client merges over the one it holds", got)
	}
	if got := overlayOf(t, s, "Lead").State; got != "" {
		t.Errorf("overlay state = %q, want none claimed", got)
	}

	// A spawn is the first thing that says the seat runs.
	s.Apply(env("agent_spawned", map[string]any{"role": "Lead", "agent_id": "a-1"}))
	if got := overlayOf(t, s, "Lead").State; got != "idle" {
		t.Errorf("state after a spawn = %q, want idle", got)
	}
}
