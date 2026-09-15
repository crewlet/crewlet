package store_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// seedPhase writes one completed phase with a price and the promoted counts.
func seedPhase(t *testing.T, log *store.EventLog, id string, at time.Time,
	role string, tokens int, usd float64) {

	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"role": role, "phase": "execute", "model": "sonnet",
		"input_tokens": tokens, "output_tokens": 0, "total_tokens": tokens,
		"cost_usd": usd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(t.Context(), store.EventRecord{
		ID: id, Type: "agent_phase_completed", Time: at,
		Category: "lifecycle", Actor: role,
		Tags: map[string]string{
			"agent_role": role, "phase": "execute", "model": "sonnet",
			"input_tokens": "0", "output_tokens": "0",
		},
		Payload: payload,
	}); err != nil {
		t.Fatalf("append %s: %v", id, err)
	}
}

// THE PRICE IS A COLUMN NOWHERE, and it is the one value here still read out
// of the payload.
//
// Only a subscription coding CLI reports one, so promoting it would be a
// migration and a column that is NULL on almost every row of the table. What
// matters is that the read carries it at all: without it the rollup's currency
// total is zero for every window, which renders as a company that has never
// spent a cent rather than as one whose backend does not quote.
func TestAPhasesPriceReachesTheRollup(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	base := time.Now().UTC().Add(-time.Hour)
	seedPhase(t, log, "p1", base, "PM", 100, 0.25)
	seedPhase(t, log, "p2", base.Add(time.Minute), "PM", 50, 0)

	got, err := log.PhaseTokens(t.Context(), store.PhaseTokenQuery{SinceDays: 1})
	if err != nil {
		t.Fatalf("phase tokens: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("records = %d, want two", len(got))
	}
	var total float64
	for _, r := range got {
		total += r.CostUSD
	}
	if total != 0.25 {
		t.Errorf("summed price = %v, want 0.25 — the payload's own cost_usd", total)
	}
}

// A WINDOW IS TWO INSTANTS, which a day count cannot name.
//
// A cost explorer's range control produces two edges, and its
// compare-to-previous asks for the window immediately before the one on
// screen. Neither is "N days back from now", and a reader that could only be
// asked the second question answers the first one with the wrong rows.
func TestAnInstantWindowSelectsExactlyItsOwnRows(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	base := time.Now().UTC().Truncate(time.Hour).Add(-4 * time.Hour)
	seedPhase(t, log, "before", base.Add(-time.Minute), "PM", 1, 0)
	seedPhase(t, log, "edge", base, "PM", 2, 0)
	seedPhase(t, log, "inside", base.Add(30*time.Minute), "PM", 4, 0)
	seedPhase(t, log, "after", base.Add(time.Hour), "PM", 8, 0)

	got, err := log.PhaseTokens(t.Context(), store.PhaseTokenQuery{
		Since: base, Until: base.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("phase tokens: %v", err)
	}
	var sum int
	for _, r := range got {
		sum += r.TotalTokens
	}
	// 2 + 4: the lower edge is inclusive and the upper exclusive, so two
	// adjacent windows share their boundary instant without either losing
	// it or counting it twice.
	if sum != 6 {
		t.Errorf("tokens in [base, base+1h) = %d, want 6 — got %d rows", sum, len(got))
	}
}

// AND THE FLOOR IS REPORTED, not silently applied.
//
// A request further back than the table's retention cannot return more rows.
// Honouring it would make a scan of the whole table look like a supported
// query; applying it silently would put a decade's heading over a month of
// data, which is a lie about the numbers beside it.
func TestAWindowBelowTheFloorIsRaisedAndSaidSo(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	q := store.PhaseTokenQuery{Since: now.AddDate(-3, 0, 0)}
	since, until := q.Window(now)
	want := now.Add(-time.Duration(store.MaxPhaseTokenDays) * 24 * time.Hour)
	if !since.Equal(want) {
		t.Errorf("since = %s, want the floor at %s", since, want)
	}
	if !until.IsZero() {
		t.Errorf("until = %s, want unbounded", until)
	}

	// A DAY COUNT STILL WORKS, and past the ceiling lands on the same floor.
	if since, _ := (store.PhaseTokenQuery{SinceDays: 9000}).Window(now); !since.Equal(want) {
		t.Errorf("since from a day count = %s, want the floor at %s", since, want)
	}
	// AND AN ABSENT ONE IS THE DEFAULT WINDOW, not the floor: a caller that
	// named nothing asked for a week, and answering with a month would make
	// every unparameterised read scan four times what it meant to.
	if since, _ := (store.PhaseTokenQuery{}).Window(now); !since.Equal(
		now.Add(-time.Duration(store.DefaultPhaseTokenDays) * 24 * time.Hour)) {
		t.Errorf("since with nothing named = %s, want the default window", since)
	}
}

// AN INVERTED WINDOW COLLAPSES rather than reading as a quiet company.
//
// until < since names no rows, and a reader that let it through would hand the
// SQL a range the store answers as empty — which is indistinguishable from a
// company that spent nothing.
func TestAnInvertedWindowCoversNothingRatherThanEverything(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	since, until := store.PhaseTokenQuery{
		Since: now.Add(-time.Hour), Until: now.Add(-2 * time.Hour),
	}.Window(now)
	if !since.Equal(until) {
		t.Errorf("window = %s..%s, want an empty one at the later edge", since, until)
	}
}
