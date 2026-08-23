// Package scheduletest is the contract suite every schedule.Ledger must pass.
//
// One suite, every backend — the in-memory twin and the SQL ledger over
// `scheduled_runs`. A backend the suite has not certified does not exist as
// far as the engine is concerned, and a divergence between two of them becomes
// a failing case here instead of a production surprise on whichever node runs
// the other one.
//
// The invariant under all of it is small and absolute: A SCHEDULE FIRES AT
// MOST ONCE PER IDENTITY. Everything else in the scheduler — the tick window,
// the catchup clamp, the fan-out, the fleet-singleton duty — is an efficiency
// or a policy. This is the correctness.
//
// # What the cases SEND
//
// Enumerated deliberately, because no mutation can reveal an input the suite
// never sends: a backend cannot be wrong about a question nobody asks it. Two
// separate axes, and a suite can be thorough on one while blind to the other.
//
// The VALUE axis — what goes into an identity and a row:
//
//   - every identity field varied one at a time, so no field can quietly drop
//     out of the key (a backend keying on four of the five columns passes any
//     test that only ever varies the fifth);
//   - an empty TargetHandle, which is what a skipped catchup writes, beside a
//     non-empty one for the same minute;
//   - a delimiter inside a name, split two ways across adjacent fields, which
//     is the exact aliasing a joined-string key produces;
//   - names carrying non-ASCII text, quotes, percent signs and newlines — a
//     unit name comes out of YAML and is not sanitised anywhere;
//   - a long name, past any column width a backend might have assumed;
//   - a zero FiredAt (the production case, which asks the ledger to stamp it)
//     and a supplied one (the test case, which must be kept verbatim);
//   - a zero and a negative Recent limit;
//   - a purge cutoff before, exactly at, and after a row's FiredAt.
//
// The LIFECYCLE axis — WHEN each operation is sent, which is the axis that has
// found real divergences here:
//
//   - Recent and Purge on an empty ledger, before anything is claimed;
//   - Claim after a refused Claim of the same identity;
//   - Recent after a refused Claim, which must still show the FIRST row;
//   - Purge twice in succession, the second finding nothing;
//   - Claim of an identity a Purge has just dropped, which must be granted —
//     the case that catches a sweep that drops the record and keeps the key;
//   - Recent after a Purge, which must not report what was swept;
//   - concurrent Claims of ONE identity, and concurrent Claims of DISTINCT
//     identities, both under -race.
//
// # What the suite does NOT require
//
// A backend may answer "unknown" — a non-nil error — at any moment. That is
// not a defect the suite gets to disallow: a store can be unreachable. What it
// may never do is answer a definite `false` when it does not know, because the
// scheduler reads a false as "somebody already fired this" and skips the run
// for good. The suite asserts on backends it can reach; a fault-injecting
// wrapper belongs to whoever ships a remote backend.
//
// Ordering beyond the documented one is not required either — but the
// documented one is total (FiredAt descending, then the identity tuple), and
// that IS required, because two backends sorting a tie differently hand a
// dashboard two different pages for the same data.
//
// # Known gaps, stated rather than left to be discovered
//
//   - DURABILITY is not tested, because it is not in the interface. The twin
//     loses every claim when the process exits and the SQL ledger does not,
//     and no case here can tell them apart. That difference is the entire
//     reason the SQL backend exists, and it is asserted by whoever wires it.
//   - CONCURRENCY is tested within one process. Two processes claiming the
//     same identity through two connections to one database is what the
//     at-most-once guarantee actually faces, and a single-process suite
//     approximates it with goroutines over one handle.
//   - A NUL byte inside an identity is not sent. SQLite's TEXT type is
//     documented to be able to hold one and the drivers disagree about
//     whether it truncates; a case here would be certifying the driver, not
//     the ledger. No component of a real identity can contain one — handles
//     are slugified, fire labels are formatted from a time — so the exposure
//     is a unit name typed with an escape sequence in YAML.
package scheduletest

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/schedule"
)

// budget bounds how long the suite waits on a backend before calling it stuck.
//
// A contract call that never returns has to fail as a NAMED CASE, not as a
// package timeout: the Go test binary's ten-minute panic dumps every goroutine
// in the process and is attributed to whichever package the deadline lands in.
// The heaviest case here issues a few hundred round trips, so 30 s clears a
// contended file-backed store by two orders of magnitude while still reporting
// well inside the default package timeout.
const budget = 30 * time.Second

// Run executes the contract suite against ledgers built by newLedger.
//
// newLedger is called once per case and must hand back an EMPTY ledger. Cases
// assert on counts and on whole listings, so a leftover row from a previous
// case reads as a claim that should not exist — a failure that looks like a
// broken key rather than like a dirty fixture. The harness checks and says so.
func Run(t *testing.T, newLedger func(t *testing.T) schedule.Ledger) {
	t.Helper()
	groups := []struct {
		name  string
		cases []testCase
	}{
		{"claim", claimCases},
		{"read", readCases},
		{"purge", purgeCases},
		{"values", valueCases},
		{"concurrency", concurrencyCases},
	}
	for _, g := range groups {
		t.Run(g.name, func(t *testing.T) {
			t.Parallel()
			for _, c := range g.cases {
				t.Run(c.name, func(t *testing.T) {
					t.Parallel()
					c.fn(newHarness(t, newLedger))
				})
			}
		})
	}
}

type testCase struct {
	name string
	fn   func(h *harness)
}

// harness is a ledger plus the assertions the cases are written in. Every
// helper that cannot fail in a correct backend fails the test itself, so a
// case body reads as the invariant rather than as error plumbing.
type harness struct {
	t   *testing.T
	ctx context.Context
	l   schedule.Ledger
}

func newHarness(t *testing.T, newLedger func(t *testing.T) schedule.Ledger) *harness {
	t.Helper()
	l := newLedger(t)
	if l == nil {
		t.Fatal("newLedger returned a nil ledger")
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	t.Cleanup(cancel)
	h := &harness{t: t, ctx: ctx, l: l}
	// The FIRST thing every case does is read an empty ledger, which is
	// both the fixture check and one of the lifecycle points the suite
	// promises to cover.
	if rows := h.recent(50); len(rows) != 0 {
		t.Fatalf("newLedger must return an empty ledger, got %d row(s): %v", len(rows), keysOf(rows))
	}
	return h
}

// --- fixtures -------------------------------------------------------------

// aKey is the identity every case starts from. A role-scoped smoke test at
// 09:00 on a fixed Monday — the same shape the Python suite used, so a reader
// comparing the two is comparing behaviour and not fixtures.
func aKey() schedule.FireKey {
	return schedule.FireKey{
		Scope:        types.ScheduleScopeRole,
		ScopeID:      "qa",
		ScheduleName: "smoke",
		FireLabel:    "20260608T0900",
		TargetHandle: "qa",
	}
}

var scheduledAt = time.Date(2026, time.June, 8, 9, 0, 0, 0, time.UTC)

// aRun is a fired run for the given key.
func aRun(key schedule.FireKey) schedule.Run {
	return schedule.Run{
		FireKey:     key,
		ScheduledAt: scheduledAt,
		Outcome:     schedule.OutcomeFired,
		TraceID:     "0af7651916cd43dd8448eb211c80319c",
	}
}

// --- assertions -----------------------------------------------------------

func (h *harness) claimed(run schedule.Run) {
	h.t.Helper()
	ok, err := h.l.Claim(h.ctx, run)
	if err != nil {
		h.t.Fatalf("Claim(%v): unexpected error: %v", run.FireKey, err)
	}
	if !ok {
		h.t.Fatalf("Claim(%v): refused, expected this call to write the row", run.FireKey)
	}
}

func (h *harness) refused(run schedule.Run) {
	h.t.Helper()
	ok, err := h.l.Claim(h.ctx, run)
	if err != nil {
		// The tri-state matters in exactly this direction: a refusal is
		// (false, nil) and an unknown is (_, err). A backend that returned
		// an error here would be saying "I could not tell", and the
		// scheduler would keep retrying rather than treating the fire as
		// spent.
		h.t.Fatalf("Claim(%v): a genuine refusal must be (false, nil), got error: %v", run.FireKey, err)
	}
	if ok {
		h.t.Fatalf("Claim(%v): granted, expected the identity to be taken already", run.FireKey)
	}
}

func (h *harness) recent(limit int) []schedule.Run {
	h.t.Helper()
	rows, err := h.l.Recent(h.ctx, limit)
	if err != nil {
		h.t.Fatalf("Recent(%d): unexpected error: %v", limit, err)
	}
	return rows
}

func (h *harness) purge(before time.Time) int {
	h.t.Helper()
	n, err := h.l.Purge(h.ctx, before)
	if err != nil {
		h.t.Fatalf("Purge(%v): unexpected error: %v", before, err)
	}
	return n
}

func (h *harness) requireKeys(what string, rows []schedule.Run, want ...schedule.FireKey) {
	h.t.Helper()
	got := keysOf(rows)
	wantS := make([]string, 0, len(want))
	for _, k := range want {
		wantS = append(wantS, keyString(k))
	}
	if !slices.Equal(got, wantS) {
		h.t.Fatalf("%s = %v, want %v", what, got, wantS)
	}
}

func keysOf(rows []schedule.Run) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, keyString(r.FireKey))
	}
	return out
}

// keyString renders an identity for a failure message only. It joins on a
// delimiter precisely because nothing load-bearing may — see the aliasing case.
func keyString(k schedule.FireKey) string {
	return fmt.Sprintf("%s/%s/%s/%s/%s", k.Scope, k.ScopeID, k.ScheduleName, k.FireLabel, k.TargetHandle)
}

// --- claim ----------------------------------------------------------------

var claimCases = []testCase{
	{"the_first_claimer_wins_and_the_second_is_refused", func(h *harness) {
		h.claimed(aRun(aKey()))
		h.refused(aRun(aKey()))
	}},

	{"a_refused_claim_leaves_the_first_row_intact", func(h *harness) {
		// The other half of a refusal, and the half a boolean cannot show.
		// A read-modify-write backend that validates after writing returns
		// the right answer and still overwrites the row — so the second
		// caller's trace id would replace the one the first fire's turn is
		// actually running under, and the dashboard link would go nowhere.
		first := aRun(aKey())
		h.claimed(first)

		second := aRun(aKey())
		second.Outcome = schedule.OutcomeSkippedCatchup
		second.TraceID = "ffffffffffffffffffffffffffffffff"
		second.ScheduledAt = scheduledAt.Add(time.Hour)
		h.refused(second)

		rows := h.recent(10)
		if len(rows) != 1 {
			h.t.Fatalf("Recent = %d rows, want 1 — a refusal must not insert", len(rows))
		}
		if rows[0].TraceID != first.TraceID || rows[0].Outcome != first.Outcome {
			h.t.Fatalf("the row moved to %+v, want the first claim's %+v", rows[0], first)
		}
		if !rows[0].ScheduledAt.Equal(first.ScheduledAt) {
			h.t.Fatalf("scheduled_at moved to %v, want %v", rows[0].ScheduledAt, first.ScheduledAt)
		}
	}},

	{"every_field_of_the_identity_is_part_of_it", func(h *harness) {
		// One field varied at a time. A backend keying on four of the five
		// columns passes any case that only ever varies the fifth, so each
		// field gets its own claim and each must be granted.
		base := aKey()
		h.claimed(aRun(base))

		for _, tc := range []struct {
			field string
			key   schedule.FireKey
		}{
			{"scope", func() schedule.FireKey { k := base; k.Scope = types.ScheduleScopeUnit; return k }()},
			{"scope id", func() schedule.FireKey { k := base; k.ScopeID = "other"; return k }()},
			{"schedule name", func() schedule.FireKey { k := base; k.ScheduleName = "other"; return k }()},
			{"fire label", func() schedule.FireKey { k := base; k.FireLabel = "20260608T1000"; return k }()},
			{"target handle", func() schedule.FireKey { k := base; k.TargetHandle = "other"; return k }()},
		} {
			ok, err := h.l.Claim(h.ctx, aRun(tc.key))
			if err != nil {
				h.t.Fatalf("Claim varying %s: %v", tc.field, err)
			}
			if !ok {
				h.t.Fatalf("Claim varying %s was refused — that field is not part of the identity, "+
					"so two distinct fires collapse onto one and one of them never runs", tc.field)
			}
		}
		if got := len(h.recent(50)); got != 6 {
			h.t.Fatalf("Recent = %d rows, want 6", got)
		}
	}},

	{"a_different_runner_is_a_different_fire", func(h *harness) {
		// The `each` fan-out mints one identity per member. If the runner
		// were not part of the key, the first member claimed would suppress
		// every sibling and a standup would reach exactly one person.
		lead := aKey()
		lead.Scope, lead.ScopeID, lead.ScheduleName = types.ScheduleScopeUnit, "Quality", "standup"
		lead.TargetHandle = "qa-lead"
		dev := lead
		dev.TargetHandle = "qa-dev"

		h.claimed(aRun(lead))
		h.claimed(aRun(dev))
		h.refused(aRun(lead))
	}},

	{"two_instants_sharing_a_fire_label_claim_once", func(h *harness) {
		// DST fall-back. The evaluator reports both UTC instants of a
		// repeated local minute, and the LABEL is what collapses them: two
		// runs with different ScheduledAt and one FireLabel are one fire.
		//
		// This is the ledger's half of the split; the evaluator's half is
		// TestARepeatedLocalHourYieldsTwoInstants.
		first := aRun(aKey())
		first.ScheduledAt = time.Date(2026, time.October, 25, 0, 30, 0, 0, time.UTC)
		second := aRun(aKey())
		second.ScheduledAt = time.Date(2026, time.October, 25, 1, 30, 0, 0, time.UTC)

		h.claimed(first)
		h.refused(second)
		if got := len(h.recent(10)); got != 1 {
			h.t.Fatalf("Recent = %d rows, want 1 — one local minute is one fire", got)
		}
	}},

	{"a_skip_row_and_a_fire_row_for_one_minute_do_not_collide", func(h *harness) {
		// A skipped catchup resolved no runner, so it writes an empty
		// target handle. A fire for the same minute carries a real one, and
		// the two are distinct identities — otherwise recording the skip
		// would silently consume the claim a later corrected tick needs.
		skip := aRun(aKey())
		skip.TargetHandle = ""
		skip.Outcome = schedule.OutcomeSkippedCatchup

		h.claimed(skip)
		h.claimed(aRun(aKey()))
		if got := len(h.recent(10)); got != 2 {
			h.t.Fatalf("Recent = %d rows, want 2", got)
		}
	}},

	{"a_delimiter_in_a_name_cannot_alias_two_identities", func(h *harness) {
		// The identity is the column TUPLE, never a joined string. Split the
		// same characters two ways across adjacent fields: a backend joining
		// on ':' produces one key for both, and one of the two schedules
		// then never fires again.
		a := aKey()
		a.Scope, a.ScopeID, a.ScheduleName = types.ScheduleScopeUnit, "q3:launch", "review"
		b := a
		b.ScopeID, b.ScheduleName = "q3", "launch:review"

		h.claimed(aRun(a))
		h.claimed(aRun(b))
		h.refused(aRun(a))
		h.refused(aRun(b))
	}},

	{"an_empty_scope_id_is_still_an_identity", func(h *harness) {
		// Not reachable from a validated org — but a ledger that treated an
		// empty component as "no identity" would either reject the row or
		// collapse it onto every other empty one, and both are silent.
		blank := aKey()
		blank.ScopeID = ""
		h.claimed(aRun(blank))
		h.refused(aRun(blank))

		other := blank
		other.ScheduleName = "other"
		h.claimed(aRun(other))
	}},

	{"a_claim_after_a_purge_that_dropped_it_is_granted", func(h *harness) {
		// The lifecycle point that catches a sweep dropping the record and
		// keeping the key: the store has no evidence of the fire and must
		// not go on refusing it.
		h.claimed(aRun(aKey()))
		if n := h.purge(time.Now().Add(time.Hour)); n != 1 {
			h.t.Fatalf("Purge dropped %d rows, want 1", n)
		}
		h.claimed(aRun(aKey()))
	}},
}

// --- read -----------------------------------------------------------------

var readCases = []testCase{
	{"a_row_round_trips_every_field", func(h *harness) {
		want := aRun(aKey())
		want.FiredAt = time.Date(2026, time.June, 8, 9, 0, 12, 0, time.UTC)
		h.claimed(want)

		rows := h.recent(10)
		if len(rows) != 1 {
			h.t.Fatalf("Recent = %d rows, want 1", len(rows))
		}
		got := rows[0]
		if got.FireKey != want.FireKey {
			h.t.Fatalf("identity came back as %v, want %v", keyString(got.FireKey), keyString(want.FireKey))
		}
		if !got.ScheduledAt.Equal(want.ScheduledAt) {
			h.t.Fatalf("scheduled_at = %v, want %v", got.ScheduledAt, want.ScheduledAt)
		}
		if !got.FiredAt.Equal(want.FiredAt) {
			h.t.Fatalf("fired_at = %v, want %v", got.FiredAt, want.FiredAt)
		}
		if got.Outcome != want.Outcome {
			h.t.Fatalf("outcome = %q, want %q", got.Outcome, want.Outcome)
		}
		if got.TraceID != want.TraceID {
			// Not decoration. The trace is the only link from a ledger row
			// to the turn the fire caused, and the dashboard's "view calls"
			// follows exactly it.
			h.t.Fatalf("trace_id = %q, want %q", got.TraceID, want.TraceID)
		}
	}},

	{"a_supplied_fired_at_is_kept_and_a_zero_one_is_stamped", func(h *harness) {
		stamped := aRun(aKey())
		before := time.Now().Add(-time.Second)
		h.claimed(stamped) // FiredAt zero — the production case

		supplied := aRun(aKey())
		supplied.ScheduleName = "supplied"
		supplied.FiredAt = time.Date(2020, time.January, 2, 3, 4, 5, 0, time.UTC)
		h.claimed(supplied)

		for _, row := range h.recent(10) {
			if row.ScheduleName == "supplied" {
				if !row.FiredAt.Equal(supplied.FiredAt) {
					h.t.Fatalf("a supplied fired_at came back as %v, want %v", row.FiredAt, supplied.FiredAt)
				}
				continue
			}
			if row.FiredAt.Before(before) || row.FiredAt.After(time.Now().Add(time.Second)) {
				h.t.Fatalf("a zero fired_at was stamped %v, want roughly now — a ledger that "+
					"left it zero would put the row in year 1, below every retention cutoff "+
					"and at the wrong end of every listing", row.FiredAt)
			}
		}
	}},

	{"recent_is_newest_first", func(h *harness) {
		for i, name := range []string{"oldest", "middle", "newest"} {
			run := aRun(aKey())
			run.ScheduleName = name
			run.FiredAt = scheduledAt.Add(time.Duration(i) * time.Minute)
			h.claimed(run)
		}
		var names []string
		for _, row := range h.recent(10) {
			names = append(names, row.ScheduleName)
		}
		if want := []string{"newest", "middle", "oldest"}; !slices.Equal(names, want) {
			h.t.Fatalf("Recent = %v, want %v", names, want)
		}
	}},

	{"recent_breaks_a_tie_on_the_identity", func(h *harness) {
		// Two rows can share a fired_at, and without a total order the two
		// backends hand a dashboard different pages for the same data. The
		// tiebreak is the identity tuple ascending.
		tied := scheduledAt
		for _, name := range []string{"c", "a", "b"} {
			run := aRun(aKey())
			run.ScheduleName = name
			run.FiredAt = tied
			h.claimed(run)
		}
		var names []string
		for _, row := range h.recent(10) {
			names = append(names, row.ScheduleName)
		}
		if want := []string{"a", "b", "c"}; !slices.Equal(names, want) {
			h.t.Fatalf("Recent = %v, want %v — ties order by the identity tuple", names, want)
		}
	}},

	{"recent_honours_the_limit_and_takes_the_newest", func(h *harness) {
		for i := range 5 {
			run := aRun(aKey())
			run.ScheduleName = fmt.Sprintf("s%d", i)
			run.FiredAt = scheduledAt.Add(time.Duration(i) * time.Minute)
			h.claimed(run)
		}
		rows := h.recent(2)
		if len(rows) != 2 {
			h.t.Fatalf("Recent(2) = %d rows, want 2", len(rows))
		}
		// A backend that limited before ordering returns two arbitrary rows
		// and still passes a length check.
		if rows[0].ScheduleName != "s4" || rows[1].ScheduleName != "s3" {
			h.t.Fatalf("Recent(2) = %v, want the two newest (s4, s3)", []string{rows[0].ScheduleName, rows[1].ScheduleName})
		}
	}},

	{"recent_returns_nothing_for_a_non_positive_limit", func(h *harness) {
		h.claimed(aRun(aKey()))
		for _, limit := range []int{0, -1} {
			if rows := h.recent(limit); len(rows) != 0 {
				h.t.Fatalf("Recent(%d) = %d rows, want none — an unbounded read is not what a "+
					"caller whose page size arrived as zero asked for", limit, len(rows))
			}
		}
	}},

	{"recent_over_reads_an_empty_ledger", func(h *harness) {
		// The lifecycle point the harness already checks once, asserted here
		// as its own case so the guard cannot be lost by a change to the
		// fixture check.
		if rows := h.recent(50); len(rows) != 0 {
			h.t.Fatalf("Recent on an empty ledger = %d rows, want none", len(rows))
		}
	}},
}

// --- purge ----------------------------------------------------------------

var purgeCases = []testCase{
	{"purge_drops_only_what_is_strictly_older_than_the_cutoff", func(h *harness) {
		// Three rows straddling the cutoff, including one exactly ON it.
		// "Before" is strict: a row at the boundary is inside the window a
		// tick could still evaluate, and deleting it lets that fire run
		// twice.
		cutoff := scheduledAt
		for name, firedAt := range map[string]time.Time{
			"older": cutoff.Add(-time.Minute),
			"exact": cutoff,
			"newer": cutoff.Add(time.Minute),
		} {
			run := aRun(aKey())
			run.ScheduleName = name
			run.FiredAt = firedAt
			h.claimed(run)
		}
		if n := h.purge(cutoff); n != 1 {
			h.t.Fatalf("Purge dropped %d rows, want 1 (only the strictly older one)", n)
		}
		var names []string
		for _, row := range h.recent(10) {
			names = append(names, row.ScheduleName)
		}
		slices.Sort(names)
		if want := []string{"exact", "newer"}; !slices.Equal(names, want) {
			h.t.Fatalf("survivors = %v, want %v", names, want)
		}
	}},

	{"purge_frees_the_claim_of_what_it_dropped_and_not_of_what_it_kept", func(h *harness) {
		dropped := aRun(aKey())
		dropped.ScheduleName = "dropped"
		dropped.FiredAt = scheduledAt.Add(-time.Hour)
		kept := aRun(aKey())
		kept.ScheduleName = "kept"
		kept.FiredAt = scheduledAt.Add(time.Hour)
		h.claimed(dropped)
		h.claimed(kept)

		if n := h.purge(scheduledAt); n != 1 {
			h.t.Fatalf("Purge dropped %d rows, want 1", n)
		}
		// A sweep that drops the record but keeps the key goes on refusing
		// a fire it has no evidence of — silently, for ever.
		h.claimed(dropped)
		// And one that over-collects frees a claim still inside the window.
		h.refused(kept)
	}},

	{"purge_on_an_empty_ledger_reports_nothing", func(h *harness) {
		if n := h.purge(time.Now().Add(time.Hour)); n != 0 {
			h.t.Fatalf("Purge on an empty ledger dropped %d rows, want 0", n)
		}
	}},

	{"purge_is_idempotent", func(h *harness) {
		h.claimed(aRun(aKey()))
		cutoff := time.Now().Add(time.Hour)
		if n := h.purge(cutoff); n != 1 {
			h.t.Fatalf("first Purge dropped %d rows, want 1", n)
		}
		if n := h.purge(cutoff); n != 0 {
			h.t.Fatalf("second Purge dropped %d rows, want 0 — a count that keeps rising is a "+
				"sweep reporting rows it did not delete", n)
		}
		if rows := h.recent(10); len(rows) != 0 {
			h.t.Fatalf("Recent after Purge = %d rows, want none", len(rows))
		}
	}},

	{"purge_of_nothing_leaves_every_row_and_its_claim", func(h *harness) {
		h.claimed(aRun(aKey()))
		if n := h.purge(scheduledAt.Add(-24 * time.Hour)); n != 0 {
			h.t.Fatalf("Purge below every row dropped %d, want 0", n)
		}
		h.refused(aRun(aKey()))
		if rows := h.recent(10); len(rows) != 1 {
			h.t.Fatalf("Recent = %d rows, want 1", len(rows))
		}
	}},
}

// --- values ---------------------------------------------------------------

var valueCases = []testCase{
	{"awkward_names_round_trip_and_stay_distinct", func(h *harness) {
		// A unit name comes out of YAML and is sanitised nowhere. Each of
		// these is claimed, read back verbatim, and re-claimed to prove the
		// identity survived whatever encoding the backend chose.
		names := []string{
			"Qualité & Sécurité",              // non-ASCII
			`the "nightly" run`,               // double quotes
			"o'brien's team",                  // an apostrophe, which is SQL's own quote
			"100% coverage",                   // a percent sign, LIKE's wildcard
			"line\nbreak",                     // an embedded newline
			"tab\there",                       // an embedded tab
			strings.Repeat("long-", 60) + "x", // 301 characters
			"emoji 🚀 unit",
		}
		for _, name := range names {
			key := aKey()
			key.Scope, key.ScopeID = types.ScheduleScopeUnit, name
			h.claimed(aRun(key))
		}
		rows := h.recent(100)
		if len(rows) != len(names) {
			h.t.Fatalf("Recent = %d rows, want %d — two awkward names collapsed onto one identity",
				len(rows), len(names))
		}
		seen := map[string]struct{}{}
		for _, row := range rows {
			seen[row.ScopeID] = struct{}{}
		}
		for _, name := range names {
			if _, ok := seen[name]; !ok {
				h.t.Fatalf("%q did not come back verbatim; got %v", name, seen)
			}
			key := aKey()
			key.Scope, key.ScopeID = types.ScheduleScopeUnit, name
			h.refused(aRun(key))
		}
	}},

	{"an_unset_outcome_and_trace_round_trip_as_unset", func(h *harness) {
		// The zero Run. Not something the scheduler writes, but a ledger
		// that substituted a default here would make "nobody said" and
		// "fired" the same row, and the dashboard would draw a dispatch
		// that never happened.
		run := schedule.Run{FireKey: aKey(), ScheduledAt: scheduledAt}
		h.claimed(run)
		rows := h.recent(10)
		if len(rows) != 1 {
			h.t.Fatalf("Recent = %d rows, want 1", len(rows))
		}
		if rows[0].Outcome != "" {
			h.t.Fatalf("outcome = %q, want it to come back unset", rows[0].Outcome)
		}
		if rows[0].TraceID != "" {
			h.t.Fatalf("trace_id = %q, want it to come back unset", rows[0].TraceID)
		}
	}},

	{"a_zero_scheduled_at_round_trips_as_zero", func(h *harness) {
		// The other zero, and the one with a storage trap: an instant
		// encoded as an integer offset puts the zero time at a large
		// negative number, and a backend that clamped it would report a
		// fire scheduled at the epoch.
		run := aRun(aKey())
		run.ScheduledAt = time.Time{}
		h.claimed(run)
		rows := h.recent(10)
		if len(rows) != 1 {
			h.t.Fatalf("Recent = %d rows, want 1", len(rows))
		}
		if !rows[0].ScheduledAt.IsZero() {
			h.t.Fatalf("scheduled_at = %v, want the zero time back", rows[0].ScheduledAt)
		}
	}},
}

// --- concurrency ----------------------------------------------------------

var concurrencyCases = []testCase{
	{"exactly_one_of_many_concurrent_claimers_wins", func(h *harness) {
		// THE invariant. Every node's tick enumerates every schedule and
		// all but one lose this race on every due fire; a backend that let
		// two through sends one company two standups.
		const claimers = 16
		var (
			wg   sync.WaitGroup
			mu   sync.Mutex
			won  int
			errs []error
		)
		start := make(chan struct{})
		for range claimers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				ok, err := h.l.Claim(h.ctx, aRun(aKey()))
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err != nil:
					// Unknown is permitted at any moment — a contended
					// store can honestly fail to say who won. It is only
					// counted, never treated as a win.
					errs = append(errs, err)
				case ok:
					won++
				}
			}()
		}
		close(start)
		h.await(&wg, "concurrent claims of one identity")

		if won != 1 {
			h.t.Fatalf("%d of %d claimers won, want exactly 1 (%d reported unknown: %v)",
				won, claimers, len(errs), errs)
		}
		if rows := h.recent(50); len(rows) != 1 {
			h.t.Fatalf("Recent = %d rows, want 1", len(rows))
		}
	}},

	{"concurrent_claims_of_distinct_identities_all_win", func(h *harness) {
		// The other direction: a backend serialising on one global key
		// would pass the case above and quietly refuse a whole fan-out.
		const runners = 12
		var (
			wg     sync.WaitGroup
			mu     sync.Mutex
			won    int
			failed []error
		)
		start := make(chan struct{})
		for i := range runners {
			wg.Add(1)
			go func() {
				defer wg.Done()
				key := aKey()
				key.TargetHandle = fmt.Sprintf("member-%02d", i)
				<-start
				ok, err := h.l.Claim(h.ctx, aRun(key))
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err != nil:
					failed = append(failed, err)
				case ok:
					won++
				}
			}()
		}
		close(start)
		h.await(&wg, "concurrent claims of distinct identities")

		if won+len(failed) != runners {
			h.t.Fatalf("%d of %d distinct identities were REFUSED — a refusal means the identity "+
				"was already taken, and no two of these share one", runners-won-len(failed), runners)
		}
		if won != runners {
			h.t.Fatalf("%d of %d claims won, %d reported unknown: %v", won, runners, len(failed), failed)
		}
	}},

	{"claims_and_reads_interleave_without_tearing", func(h *harness) {
		// -race is the point of this one. The Python twin's correctness
		// rested on there being one event loop, and every one of those
		// implicit serialisations is a real race here.
		const rounds = 40
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			for i := range rounds {
				key := aKey()
				key.FireLabel = fmt.Sprintf("20260608T%04d", i)
				if _, err := h.l.Claim(h.ctx, aRun(key)); err != nil {
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for range rounds {
				if _, err := h.l.Recent(h.ctx, 10); err != nil {
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for range rounds {
				// A cutoff below every row, so the sweep runs its whole
				// scan and deletes nothing — the read-write interleaving is
				// the subject, not the deletion.
				if _, err := h.l.Purge(h.ctx, time.Time{}); err != nil {
					return
				}
			}
		}()
		h.await(&wg, "interleaved claims, reads and sweeps")

		if got := len(h.recent(rounds * 2)); got != rounds {
			h.t.Fatalf("Recent = %d rows, want %d — a claim was lost or duplicated under "+
				"concurrent reads", got, rounds)
		}
	}},
}

// await joins the suite's own goroutines within the budget. what names what
// they were doing, so a stuck backend fails on a line that says which call
// never came back rather than on a goroutine dump.
func (h *harness) await(wg *sync.WaitGroup, what string) {
	h.t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(budget):
		// Evidence, not a verdict: this helper cannot tell a call that
		// never returns from one merely slower than a budget the SUITE
		// chose, and "never returns" is a serious accusation to hand an
		// author on no more than a timer firing.
		h.t.Fatalf("%s: still running after %v, which is this suite's budget rather than a "+
			"promise the contract makes. Most likely a Ledger call is not returning; a store "+
			"merely slower than that looks identical from here", what, budget)
	}
}
