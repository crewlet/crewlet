package livestate_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
)

func meterReport(meterID string, seq int, agents ...map[string]any) map[string]any {
	rows := make([]any, 0, len(agents))
	for _, a := range agents {
		rows = append(rows, a)
	}
	return map[string]any{
		"meter_id": meterID, "seq": seq,
		"org_used_tokens": 500, "org_max_tokens": 1000, "org_refused_at": "",
		"agents": rows,
	}
}

func seatMeter(role string, used, max int) map[string]any {
	return map[string]any{
		"role": role, "agent_id": "a-1",
		"used_tokens": used, "max_tokens": max, "refused_at": "",
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
	// Stream-only for the reason the in-flight call is, and one stronger:
	// these figures describe ONE engine run, so a persisted copy replayed
	// from history would show a dead process's counters as the current
	// ones.
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
	// The counters are a process-lifetime meter, so a restart legitimately
	// zeroes them. Merging, or taking a maximum, would pin a phantom
	// high-water mark that no later report could clear.
	s := livestate.New()
	s.Apply(env("budget_reported", meterReport("m-1", 9, seatMeter("Lead", 900, 1000)), streamOnly))
	s.Apply(env("budget_reported", meterReport("m-2", 1, seatMeter("Lead", 10, 1000)), streamOnly))

	if got := s.Budget().MeterID; got != "m-2" {
		t.Errorf("meter id = %q, want the new run's", got)
	}
	if got := overlayOf(t, s, "Lead").Budget.Used; got != 10 {
		t.Errorf("used = %d, want 10: the new run's figures were merged with a dead one's", got)
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
	s.Apply(env("task_started", map[string]any{"role": "Lead", "task_id": "t-1"}))

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
	// timestamp-ordered, and it is not reliably: the API subscribes to the
	// stream before it hydrates, so a live event can land ahead of the
	// older records hydration then appends behind it. One recent record at
	// the head is enough to make a head-popping loop exit immediately and
	// never prune again — the window would silently stop being a window.
	s := livestate.New()
	s.Apply(phaseSpend("live", "2026-06-14T12:00:00Z", 10))
	s.Apply(phaseSpend("hydrated-old", "2026-06-12T00:00:00Z", 20))
	s.Apply(phaseSpend("trigger", "2026-06-14T12:30:00Z", 5))

	for _, record := range s.SpendRecords() {
		if record.EventID == "hydrated-old" {
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
	// Sequence numbers are per meter, so a new run's first report starts
	// low. Comparing it against the held run's would refuse it, and the
	// dashboard would show a dead process's counters until the new run
	// happened to pass the old one's sequence.
	s := livestate.New()
	s.Apply(env("budget_reported", meterReport("m-1", 99, seatMeter("Lead", 900, 1000)), streamOnly))
	s.Apply(env("budget_reported", meterReport("m-2", 1, seatMeter("Lead", 5, 1000)), streamOnly))

	if got := s.Budget().Seq; got != 1 {
		t.Errorf("seq = %d, want the new run's 1", got)
	}
	if got := overlayOf(t, s, "Lead").Budget.Used; got != 5 {
		t.Errorf("used = %d, want the new run's 5", got)
	}
}

func TestANewMeterDropsASeatItDoesNotMention(t *testing.T) {
	t.Parallel()
	// The case that says a new run's report replaces rather than merges: a
	// seat metered under the old run and absent from the new one must lose
	// its bar, not keep a dead process's figure.
	s := livestate.New()
	s.Apply(env("budget_reported", meterReport("m-1", 4,
		seatMeter("Lead", 900, 1000), seatMeter("Dev", 700, 1000)), streamOnly))
	s.Apply(env("budget_reported", meterReport("m-2", 1, seatMeter("Lead", 5, 1000)), streamOnly))

	if got := overlayOf(t, s, "Dev").Budget; got != nil {
		t.Errorf("Dev budget = %+v, want none: it is a dead run's figure", got)
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
