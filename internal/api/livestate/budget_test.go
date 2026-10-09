package livestate_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/tokens"
)

func meterReport(meterID string, seq int, seats ...map[string]any) map[string]any {
	rows := make([]any, 0, len(seats))
	for _, a := range seats {
		rows = append(rows, a)
	}
	return map[string]any{
		"meter_id": meterID, "seq": seq, "timezone": "UTC",
		"org":   map[string]any{"windows": []any{dayWindow(500, 1000, "")}},
		"seats": rows,
	}
}

// dayWindow is 14 June's window as a frame states it, refusing since refused
// when that is set.
func dayWindow(used, limit int, refused string) map[string]any {
	w := map[string]any{
		"period": "day", "window": "2026-06-14",
		"starts_at": "2026-06-14T00:00:00Z", "resets_at": "2026-06-15T00:00:00Z",
		"used": used, "limit": limit, "state": "ok",
	}
	if refused != "" {
		w["refused_at"] = refused
		w["state"] = "refusing"
	}
	return w
}

func seatMeter(role string, used, max int) map[string]any {
	return map[string]any{
		"role": role, "agent_id": "a-1", "handle": strings.ToLower(role),
		"windows": []any{dayWindow(used, max, "")},
	}
}

// usedOf is a meter's day, the one window these reports carry.
func usedOf(t *testing.T, m *livestate.BudgetMeter) int {
	t.Helper()
	if m == nil || len(m.Windows) != 1 {
		t.Fatalf("meter = %+v, want its one day window", m)
	}
	return m.Windows[0].Used
}

func TestAMeterReportLandsOnTheSeatAndTheOrg(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("budget_meters", meterReport("m-1", 1, seatMeter("Lead", 100, 400)), streamOnly))

	org := s.Budget()
	if org.MeterID != "m-1" || org.Timezone != "UTC" || len(org.Org.Windows) != 1 ||
		org.Org.Windows[0].Used != 500 || org.Org.Windows[0].Limit == nil || *org.Org.Windows[0].Limit != 1000 {
		t.Errorf("org meter = %+v", org)
	}
	seat := overlayOf(t, s, "Lead").Budget
	if got := usedOf(t, seat); got != 100 || *seat.Windows[0].Limit != 400 {
		t.Errorf("seat meter = %+v", seat)
	}
}

// THE REFUSAL REACHES THE PUSH. The projection used to fold a report by picking
// named figures out of it, and the refusal stamp was not one of them: every
// frame carried it and every push dropped it, so the dashboard's "refusing
// charges" rows and badges were unreachable while the gate turned turns away.
func TestAMeterReportCarriesEachWindowsRefusalAndState(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	report := meterReport("m-1", 1, map[string]any{
		"role": "Lead", "agent_id": "a-1", "handle": "lead",
		"windows": []any{dayWindow(400, 400, "2026-06-14T10:00:05.25Z")},
	})
	report["org"] = map[string]any{"windows": []any{dayWindow(990, 1000, "2026-06-14T12:00:00Z")}}
	s.Apply(env("budget_meters", report, streamOnly))

	org := s.Budget().Org.Windows
	if len(org) != 1 || org[0].RefusedAt != "2026-06-14T12:00:00Z" || org[0].State != "refusing" {
		t.Errorf("org windows = %+v, want the day refusing since 12:00Z", org)
	}
	seat := overlayOf(t, s, "Lead").Budget
	if seat == nil || len(seat.Windows) != 1 || seat.Windows[0].RefusedAt != "2026-06-14T10:00:05.25Z" ||
		seat.Windows[0].State != "refusing" || seat.Windows[0].ResetsAt != "2026-06-15T00:00:00Z" {
		t.Errorf("seat meter = %+v, want the day refusing since 10:00:05.25Z, resetting at midnight", seat)
	}
	pushed, err := json.Marshal(s.Budget())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(pushed), `"refused_at":"2026-06-14T12:00:00Z"`) {
		t.Errorf("the budget push lost the refusal: %s", pushed)
	}
}

// A REPORT'S WINDOWS REPLACE THE HELD ONES, never merge with them: a ceiling
// removed from the week leaves a report with the day alone, and the week's bar
// must go with it rather than keep the figure it had.
func TestAReportsWindowsReplaceTheHeldOnes(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	week := dayWindow(40, 900, "")
	week["period"], week["window"] = "week", "2026-W24"
	first := meterReport("m-1", 1)
	first["org"] = map[string]any{"windows": []any{dayWindow(10, 100, ""), week}}
	s.Apply(env("budget_meters", first, streamOnly))
	s.Apply(env("budget_meters", meterReport("m-1", 2), streamOnly))

	if got := s.Budget().Org.Windows; len(got) != 1 || got[0].Period != "day" || got[0].Used != 500 {
		t.Errorf("org windows = %+v, want the second report's day alone", got)
	}
}

func TestTheMeterNeverEntersTheActivityFeed(t *testing.T) {
	t.Parallel()
	// Stream-only: a report is a snapshot of a counter that moves every
	// round, so a persisted copy replayed from history would show figures
	// the counter left behind as the current ones.
	s := livestate.New()
	change := s.Apply(env("budget_meters", meterReport("m-1", 1)))
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
	s.Apply(env("budget_meters", meterReport("m-1", 5, seatMeter("Lead", 300, 400)), streamOnly))
	s.Apply(env("budget_meters", meterReport("m-1", 2, seatMeter("Lead", 100, 400)), streamOnly))

	if got := usedOf(t, overlayOf(t, s, "Lead").Budget); got != 300 {
		t.Errorf("used = %d, want 300: an older report walked the meter back", got)
	}
	if got := s.Budget().Seq; got != 5 {
		t.Errorf("seq = %d, want 5", got)
	}
}

func TestARepeatedSeqIsDropped(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("budget_meters", meterReport("m-1", 3, seatMeter("Lead", 300, 400)), streamOnly))
	change := s.Apply(env("budget_meters", meterReport("m-1", 3, seatMeter("Lead", 999, 400)), streamOnly))

	if change.Moved() {
		t.Error("a repeated seq moved the projection")
	}
	if got := usedOf(t, overlayOf(t, s, "Lead").Budget); got != 300 {
		t.Errorf("used = %d, want the held 300", got)
	}
}

func TestANewMeterReplacesRatherThanMerges(t *testing.T) {
	t.Parallel()
	// Each report is a complete snapshot of the shared counter, and a window
	// turning over legitimately lowers it. Merging, or taking a maximum, would pin a
	// high-water mark that no later report could clear.
	s := livestate.New()
	s.Apply(env("budget_meters", meterReport("m-1", 9, seatMeter("Lead", 900, 1000)), streamOnly))
	s.Apply(env("budget_meters", meterReport("m-2", 1, seatMeter("Lead", 10, 1000)), streamOnly))

	if got := s.Budget().MeterID; got != "m-2" {
		t.Errorf("meter id = %q, want the newer report's", got)
	}
	if got := usedOf(t, overlayOf(t, s, "Lead").Budget); got != 10 {
		t.Errorf("used = %d, want 10: the newer report was merged with an older one", got)
	}
}

func TestASeatThatLostItsMeterLosesItsBar(t *testing.T) {
	t.Parallel()
	// Only metered seats are reported. A cap removed or a
	// decommissioned role must lose its bar rather than keep the last
	// figure it had.
	s := livestate.New()
	s.Apply(env("budget_meters", meterReport("m-1", 1,
		seatMeter("Lead", 100, 400), seatMeter("Dev", 50, 400)), streamOnly))
	s.Apply(env("budget_meters", meterReport("m-1", 2, seatMeter("Lead", 120, 400)), streamOnly))

	if got := overlayOf(t, s, "Dev").Budget; got != nil {
		t.Errorf("Dev budget = %+v, want none", got)
	}
	if got := overlayOf(t, s, "Lead").Budget; got == nil || usedOf(t, got) != 120 {
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
	if got := s.Budget(); got != nil {
		t.Errorf("org budget = %+v, want none: nobody has read the counter", got)
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

// spendIDs lists the records a read of the window holds, in its order.
func spendIDs(s *livestate.LiveState) []string {
	var ids []string
	for _, record := range s.SpendRecords() {
		ids = append(ids, record.EventID)
	}
	return ids
}

func TestRecordsInsideTheWindowAreKept(t *testing.T) {
	t.Parallel()
	s := stoppedAt(t, "2026-06-14T14:00:00Z")
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
	s := stoppedAt(t, "2026-06-14T14:00:00Z")
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

// A RECORD STAMPED BEFORE THE WINDOW IS NOT COUNTED, whenever it arrives: first,
// or late behind records inside the window — a cross-topic straggler, or a
// node whose clock runs behind. Nor is it indexed, so a redelivery of it is
// refused the same way rather than held by an id no record answers for.
func TestARecordOlderThanTheWindowIsNotCounted(t *testing.T) {
	t.Parallel()
	s := stoppedAt(t, "2026-06-14T14:00:00Z")
	if s.Apply(phaseSpend("old", "2026-06-13T00:00:00Z", 10)).Tokens {
		t.Error("a record from before the window moved the rollup")
	}
	s.Apply(phaseSpend("new", "2026-06-14T12:00:00Z", 20))
	if s.Apply(phaseSpend("late-old", "2026-06-12T00:00:00Z", 20)).Tokens {
		t.Error("a late record from before the window moved the rollup")
	}
	if s.Apply(phaseSpend("old", "2026-06-13T00:00:00Z", 10)).Tokens {
		t.Error("a redelivered record from before the window moved the rollup")
	}

	if ids := spendIDs(s); !slices.Equal(ids, []string{"new"}) {
		t.Errorf("records = %v, want only the one inside the window", ids)
	}
}

// THE WINDOW AGES ON THE CLOCK, with nothing arriving to age it.
//
// It used to be cut only when a spend record arrived, at that record's own
// stamp minus a day — so a company that went quiet kept showing its last busy
// day under a heading that said it was this one, for as long as it stayed
// quiet. A read leaves out what has aged on the projection's clock, and the
// expiry a caller pushing the rollup runs on its tick reports it leaving —
// ONCE, and never swallowed by a read in between, or the tick that should
// re-push the rollup finds nothing to report.
func TestTheWindowAgesOnTheClockWithNothingArriving(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 14, 14, 0, 0, 0, time.UTC)
	s := livestate.New(livestate.WithClock(func() time.Time { return now }))
	s.Apply(phaseSpend("p1", "2026-06-14T12:00:00Z", 10))
	s.Apply(phaseSpend("p2", "2026-06-14T13:00:00Z", 20))

	now = now.Add(livestate.LiveSpendWindow - 90*time.Minute) // p1 has aged, p2 has not
	window := s.Spend()
	if ids := spendIDs(s); !slices.Equal(ids, []string{"p2"}) {
		t.Errorf("records = %v, want p1 aged out with nothing arriving", ids)
	}
	if !window.Until.Equal(now) || !window.Since.Equal(now.Add(-livestate.LiveSpendWindow)) {
		t.Errorf("window = %s .. %s, want the day ending at the clock's %s",
			window.Since, window.Until, now)
	}
	if !s.ExpireSpend() {
		t.Error("the expiry found nothing to report after two reads: a read dropped p1 " +
			"itself, so the tick that re-pushes the rollup would push nothing")
	}
	if s.ExpireSpend() {
		t.Error("a second expiry with nothing left to age reported the rollup moving")
	}

	now = now.Add(time.Hour) // p2 has aged too
	if ids := spendIDs(s); len(ids) != 0 {
		t.Errorf("records = %v, want an empty window a day after the last record", ids)
	}
	if !s.ExpireSpend() {
		t.Error("the expiry dropped p2 and reported that the rollup did not move")
	}
}

// A RECORD STAMPED AHEAD AGES NOTHING. One node's fast clock used to move the
// whole window's cutoff with the record it stamped, dropping every correctly
// stamped record a day behind it; the cutoff is the projection's clock now,
// and a record from the future is held and aged from its arrival, behind
// every record stamped before that arrival.
func TestARecordStampedAheadAgesNothing(t *testing.T) {
	t.Parallel()
	s := stoppedAt(t, "2026-06-14T14:00:00Z")
	s.Apply(phaseSpend("p1", "2026-06-14T01:00:00Z", 10))
	s.Apply(phaseSpend("ahead", "2026-06-16T12:00:00Z", 20))
	s.Apply(phaseSpend("p2", "2026-06-14T13:00:00Z", 5))

	if ids := spendIDs(s); !slices.Equal(ids, []string{"p1", "p2", "ahead"}) {
		t.Errorf("records = %v, want every one held, in the order they age out", ids)
	}
}

// A RECORD STAMPED AHEAD LEAVES A DAY AFTER IT ARRIVED.
//
// Aged by its own stamp, a record from a node whose clock is badly wrong — a
// garbled year — was held until the clock passed that stamp plus a day: in the
// rollup labelled "the last 24 hours" for the life of the process, under a
// window that excluded it. It is aged from its arrival on the projection's
// clock, it leaves when a record that arrived beside it would, and the expiry
// reports it leaving so the rollup is pushed again. It keeps the stamp it was
// published with, which is how an operator finds the node with the wrong
// clock.
//
// Mutation: age a record by its stamp alone ([ageingStamp] returning
// newStamp), and the record is still held a day after it arrived.
func TestARecordStampedAheadLeavesADayAfterItArrived(t *testing.T) {
	t.Parallel()
	const far = "2099-01-01T00:00:00Z"
	arrived := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	now := arrived
	s := livestate.New(livestate.WithClock(func() time.Time { return now }))
	if !s.Apply(phaseSpend("far", far, 1000)).Tokens {
		t.Fatal("a record stamped ahead was not counted when it arrived")
	}
	window := s.Spend()
	if len(window.Records) != 1 || window.Records[0].Timestamp != far {
		t.Fatalf("the window holds %+v, want the record with the stamp it was published with",
			window.Records)
	}

	now = arrived.Add(livestate.LiveSpendWindow - time.Hour)
	if s.ExpireSpend() {
		t.Error("the record aged out before a day had passed since it arrived")
	}
	if ids := spendIDs(s); !slices.Equal(ids, []string{"far"}) {
		t.Errorf("records = %v, want the record held until a day after it arrived", ids)
	}

	now = arrived.Add(livestate.LiveSpendWindow + time.Hour)
	if !s.ExpireSpend() {
		t.Error("the expiry found nothing to drop a day after the record arrived: " +
			"it is aged by a stamp the clock will not reach for decades")
	}
	if ids := spendIDs(s); len(ids) != 0 {
		t.Errorf("records = %v, want the window empty a day after its only record arrived", ids)
	}
}

// RECORDS ARRIVING OUT OF ORDER AGE OUT IN STAMP ORDER. A broadcast
// subscription reads across topics with no order between them and a fleet's
// clocks disagree, so the window is fed out of order — and it is held in the
// order its records age out regardless, so what leaves is always the oldest
// stamped, never whatever happened to arrive first.
func TestRecordsArrivingOutOfOrderAgeOutInStampOrder(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	s := livestate.New(livestate.WithClock(func() time.Time { return now }))
	for _, r := range []struct{ id, ts string }{
		{"b", "2026-06-14T10:00:00Z"},
		{"d", "2026-06-14T11:30:00Z"},
		{"a", "2026-06-14T09:00:00Z"},
		{"c", "2026-06-14T11:00:00Z"},
		{"c2", "2026-06-14T11:00:00Z"}, // shares c's instant: behind it, as it arrived
		{"e", "2026-06-14T11:59:00Z"},
	} {
		s.Apply(phaseSpend(r.id, r.ts, 1))
	}
	if ids := spendIDs(s); !slices.Equal(ids, []string{"a", "b", "c", "c2", "d", "e"}) {
		t.Fatalf("records = %v, want them held oldest stamp first", ids)
	}

	now = time.Date(2026, 6, 15, 10, 30, 0, 0, time.UTC) // a and b are a day old
	if ids := spendIDs(s); !slices.Equal(ids, []string{"c", "c2", "d", "e"}) {
		t.Errorf("records = %v, want exactly the two stamped before the window gone", ids)
	}
}

// THE ROLLUP NAMES A SEAT BY THE ID ITS NEWEST RECORD CARRIES.
//
// tokens.Aggregate keeps the LAST runtime id it is handed for a seat and for a
// turn, so the order the window hands its records over in is what decides which
// id a row links by — and that order is the one the records age out in,
// whatever order they arrived in. Held in arrival order, a record that lost a
// cross-topic race to a newer one named the seat by the older id.
//
// Mutation: hold the window in arrival order (append in holdSpend), and both
// rows name the late record's older id.
func TestTheRollupNamesASeatByItsNewestRecordsID(t *testing.T) {
	t.Parallel()
	s := stoppedAt(t, "2026-06-14T14:00:00Z")
	spent := func(eventID, agentID, ts string) *livestate.Envelope {
		return env("agent_phase_completed", map[string]any{
			"role": "Lead", "agent_id": agentID, "turn_id": "tn-1", "phase": "execute",
			"total_tokens": 10,
		}, id(eventID), at(ts))
	}
	s.Apply(spent("newer", "a-current", "2026-06-14T13:00:00Z"))
	s.Apply(spent("older", "a-before", "2026-06-14T12:00:00Z")) // arriving late

	window := s.Spend()
	rollup := tokens.Aggregate(window.Records, tokens.Options{Since: window.Since, Until: window.Until})
	if len(rollup.ByAgent) != 1 || rollup.ByAgent[0].AgentID != "a-current" {
		t.Errorf("by_agent = %+v, want the one seat named by the newest record's id", rollup.ByAgent)
	}
	if len(rollup.ByTurn) != 1 || rollup.ByTurn[0].AgentID != "a-current" {
		t.Errorf("by_turn = %+v, want the one turn named by the newest record's id", rollup.ByTurn)
	}
}

func TestSpendRecordsDoNotAliasTheProjection(t *testing.T) {
	t.Parallel()
	s := stoppedAt(t, "2026-06-14T14:00:00Z")
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
	s.Apply(env("budget_meters", meterReport("m-1", 99, seatMeter("Lead", 900, 1000)), streamOnly))
	s.Apply(env("budget_meters", meterReport("m-2", 1, seatMeter("Lead", 5, 1000)), streamOnly))

	if got := s.Budget().Seq; got != 1 {
		t.Errorf("seq = %d, want the new meter's 1", got)
	}
	if got := usedOf(t, overlayOf(t, s, "Lead").Budget); got != 5 {
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
	s.Apply(env("budget_meters", meterReport("m-1", 4,
		seatMeter("Lead", 900, 1000), seatMeter("Dev", 700, 1000)), streamOnly))
	s.Apply(env("budget_meters", meterReport("m-2", 1, seatMeter("Lead", 5, 1000)), streamOnly))

	if got := overlayOf(t, s, "Dev").Budget; got != nil {
		t.Errorf("Dev budget = %+v, want none: nothing meters it any more", got)
	}
}

// A RECORD WITH NO USABLE TIMESTAMP IS KEPT — absent or unparseable — however
// far the clock moves. The same rule the sandbox sweep follows: a record that
// cannot be aged out on time must not be dropped on that basis, nor taken for
// one stamped at the zero instant and so a day old. The count cap is what
// bounds those.
func TestASpendRecordWithNoUsableTimestampIsKept(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 14, 14, 0, 0, 0, time.UTC)
	s := livestate.New(livestate.WithClock(func() time.Time { return now }))
	s.Apply(phaseSpend("recent", "2026-06-14T12:00:00Z", 20))
	if !s.Apply(phaseSpend("absent", "", 10)).Tokens {
		t.Error("a record with no timestamp was not counted")
	}
	if !s.Apply(phaseSpend("garbled", "not-a-timestamp", 10)).Tokens {
		t.Error("a record with an unparseable timestamp was not counted")
	}
	if ids := spendIDs(s); !slices.Equal(ids, []string{"absent", "garbled", "recent"}) {
		t.Errorf("records = %v, want both undateable ones beside the recent one", ids)
	}

	now = now.Add(2 * livestate.LiveSpendWindow)
	if ids := spendIDs(s); !slices.Equal(ids, []string{"absent", "garbled"}) {
		t.Errorf("records = %v, want the undateable ones kept after the dated one aged", ids)
	}
}

func TestSpendRecordsAreCappedByCount(t *testing.T) {
	t.Parallel()
	// The real bound is the window; this only binds for an org emitting
	// more than the cap in a day. Truncation drops the OLDEST records, so
	// an org past the cap sees a rollup covering slightly less than a day
	// rather than a wrong total.
	s := stoppedAt(t, "2026-06-14T13:00:00Z")
	beyondCap := livestate.SpendRecordLimit + 100
	for i := range beyondCap {
		// All inside the window, so only the count cap can bind.
		ts := time.Date(2026, 6, 14, 12, 0, 0, i*1000, time.UTC).Format(time.RFC3339Nano)
		s.Apply(phaseSpend(fmt.Sprintf("p%05d", i), ts, 1))
	}
	records := s.SpendRecords()
	if len(records) > livestate.SpendRecordLimit {
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
	// refused rather than seeded in, so a seed cannot widen the window the
	// rollup claims to cover — and it says it moved nothing. Asked of the
	// seed's own report rather than of a read, which leaves aged records
	// out whether or not the seed took them.
	if old := s.Seed(livestate.History{Spend: []tokens.Record{
		rec("old", now.Add(-livestate.LiveSpendWindow-3*time.Hour), 99),
	}}); old.Tokens {
		t.Error("a seed of a record from outside the live window reported the rollup moved")
	}
}

func TestADelayedReportFromAnotherNodeCannotWalkTheMeterBackwards(t *testing.T) {
	t.Parallel()
	// Every node reports the same shared counter under its own meter id,
	// and their sequence numbers are unrelated. A frame node B read ten
	// seconds before node A's last one, delivered late, must not replace
	// A's: it is the same counter, read earlier.
	s := livestate.New()
	s.Apply(env("budget_meters", meterReport("node-a", 6, seatMeter("Lead", 300, 400)),
		streamOnly, at("2026-06-14T12:00:30Z")))
	change := s.Apply(env("budget_meters", meterReport("node-b", 40, seatMeter("Lead", 250, 400)),
		streamOnly, at("2026-06-14T12:00:20Z")))

	if change.Moved() {
		t.Error("an older read from another node moved the projection")
	}
	if got := usedOf(t, overlayOf(t, s, "Lead").Budget); got != 300 {
		t.Errorf("used = %d, want the newer read's 300", got)
	}
	if got := s.Budget().MeterID; got != "node-a" {
		t.Errorf("meter id = %q, want node-a's report held", got)
	}

	// And a LATER read from that node is taken, whatever its seq.
	s.Apply(env("budget_meters", meterReport("node-b", 41, seatMeter("Lead", 320, 400)),
		streamOnly, at("2026-06-14T12:00:45Z")))
	if got := usedOf(t, overlayOf(t, s, "Lead").Budget); got != 320 {
		t.Errorf("used = %d, want node-b's newer read of 320", got)
	}
}

func TestAnUndatedReportDoesNotEraseTheReorderGuard(t *testing.T) {
	t.Parallel()
	// A report with no usable timestamp is applied, as every guard here
	// lets one through, but the instant the next report is compared
	// against must survive it.
	s := livestate.New()
	s.Apply(env("budget_meters", meterReport("node-a", 1, seatMeter("Lead", 300, 400)),
		streamOnly, at("2026-06-14T12:00:30Z")))
	s.Apply(env("budget_meters", meterReport("node-b", 1, seatMeter("Lead", 310, 400)),
		streamOnly, at("")))
	s.Apply(env("budget_meters", meterReport("node-c", 1, seatMeter("Lead", 100, 400)),
		streamOnly, at("2026-06-14T12:00:10Z")))

	if got := usedOf(t, overlayOf(t, s, "Lead").Budget); got != 310 {
		t.Errorf("used = %d, want 310: a report older than the last dated one was applied", got)
	}
}

func TestAMeterReportClaimsNothingAboutWhetherASeatIsRunning(t *testing.T) {
	t.Parallel()
	// A report names every capped seat whether or not anything is running
	// it. The overlay it creates used to start at "offline" and overwrite
	// the roster's own state, so the first report after a boot flipped
	// every capped seat this node serves from idle to offline, on every
	// open dashboard, until the seat took a turn. A report whose windows
	// are not refusing says nothing about the seat's state at all.
	s := livestate.New()
	s.SetPlacement(map[string]bool{"Lead": true})
	s.Apply(env("budget_meters", meterReport("m-1", 1, seatMeter("Lead", 100, 400)), streamOnly))

	rows := s.MergeAgents([]map[string]any{{"role": "Lead", "handle": "lead"}})
	if got := rows[0]["activity"]; got != livestate.ActivityIdle {
		t.Errorf("merged activity = %v, want idle: a meter is not a stop", got)
	}
	if rows[0]["budget"] == nil {
		t.Error("the merged row lost the meter the report carried")
	}
	pushed := s.OverlayRows([]string{"Lead"})
	if len(pushed) != 1 {
		t.Fatalf("pushed rows = %v, want the one seat the report moved", pushed)
	}
	if got := pushed[0]["activity"]; got != livestate.ActivityIdle {
		t.Errorf("the agents push carried activity %v, want idle", got)
	}
}

// TestTheOrgMeterIsUnknownBeforeAnyReportAndAListAfterOne holds the two states
// a reader must never confuse. Before a node's first `budget_meters` frame
// nobody has read the counter, and the meter is `null`; its zero value went
// out as an empty meter instead, which the Spend and Home tiles read as "no
// budget" — so for the seconds after every engine start a capped company
// offered its operator "Set one". A report that caps nothing is the other
// fact, and it states its list as `[]` rather than `null`, which the client
// reads as the list the wire promises. A seat meter with no windows marshals
// the same way.
func TestTheOrgMeterIsUnknownBeforeAnyReportAndAListAfterOne(t *testing.T) {
	s := livestate.New()
	raw, err := json.Marshal(s.Budget())
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "null" {
		t.Fatalf("the unreported org meter must be null, not a reading, got %s", raw)
	}
	s.Apply(env("budget_meters", map[string]any{
		"meter_id": "m-1", "seq": 1, "timezone": "UTC", "org": map[string]any{"windows": nil},
	}, streamOnly))
	raw, err = json.Marshal(s.Budget())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"org":{"windows":[]}`) {
		t.Fatalf("a report capping nothing must state an empty list, got %s", raw)
	}
	seat, err := json.Marshal(&livestate.BudgetMeter{})
	if err != nil {
		t.Fatal(err)
	}
	if string(seat) != `{"windows":[]}` {
		t.Fatalf("a meter with no windows must state an empty list, got %s", seat)
	}
}
