package gitlet

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// A ROLLUP JUDGES A CHECK AS THE GATE DOES (gatejudge.go's check
// precondition): requeued errors, cancelled sets, skipped required checks,
// unknown states, the failing order and an empty set.
//
// Mutation: count an errored attempt of the home's requeue as failing at any
// attempt: the first set reads failing, red; let a skipped required check
// count as not passed: 0 of 1, red; sort the failing checks by name alone:
// the optional one leads, red; answer a rollup of nothing as passed: red.
func TestARollupJudgesACheckAsTheGateDoes(t *testing.T) {
	t.Parallel()
	at := time.Date(2031, 4, 2, 3, 14, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		rows []rollupRow
		want *CheckRollup
	}{
		{"nothing", nil, nil},
		{"a runner lost below the requeue's bound",
			[]rollupRow{{name: "unit", attempt: 1, state: CheckStateErrored, reason: CheckReasonRunnerLost,
				required: true, finishedAt: at}},
			&CheckRollup{State: RollupPending, Counts: CheckCounts{Errored: 1}, Required: 1, LastEnded: at}},
		{"a runner lost at the requeue's bound",
			[]rollupRow{{name: "unit", attempt: AutoRequeueAttempts, state: CheckStateErrored,
				reason: CheckReasonRunnerLost, required: true, finishedAt: at}},
			&CheckRollup{State: RollupFailing, Counts: CheckCounts{Errored: 1}, Required: 1, LastEnded: at,
				Failing: []FailingCheck{{Name: "unit", State: CheckStateErrored, Required: true}}}},
		{"a definition that does not run",
			[]rollupRow{{name: "unit", attempt: 1, state: CheckStateErrored, reason: CheckReasonDefsInvalid}},
			&CheckRollup{State: RollupFailing, Counts: CheckCounts{Errored: 1},
				Failing: []FailingCheck{{Name: "unit", State: CheckStateErrored}}}},
		{"cancelled alone",
			[]rollupRow{{name: "unit", attempt: 1, state: CheckStateCancelled, reason: CheckReasonSuperseded,
				required: true}, {name: "lint", attempt: 1, state: CheckStatePassed}},
			&CheckRollup{State: RollupCancelled, Counts: CheckCounts{Passed: 1, Cancelled: 1}, Required: 1}},
		{"a skipped requirement",
			[]rollupRow{{name: "unit", attempt: 1, state: CheckStateSkipped, reason: CheckReasonPaths,
				required: true}},
			&CheckRollup{State: RollupPassed, Counts: CheckCounts{Skipped: 1}, Required: 1, RequiredPassed: 1}},
		{"a later build's state",
			[]rollupRow{{name: "unit", attempt: 1, state: "paused"}, {name: "lint", attempt: 1,
				state: CheckStateCancelled}},
			&CheckRollup{State: RollupPending, Counts: CheckCounts{Cancelled: 1, Other: 1}}},
		{"running past a queued one",
			[]rollupRow{{name: "a", attempt: 1, state: CheckStateQueued}, {name: "b", attempt: 1,
				state: CheckStateRunning, startedAt: at.Add(time.Minute)}, {name: "c", attempt: 1,
				state: CheckStateRunning, startedAt: at}},
			&CheckRollup{State: RollupRunning, Counts: CheckCounts{Queued: 1, Running: 2}, RunningSince: at}},
	} {
		if got := rollUp(tc.rows, false); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s rolls up to %+v, want %+v", tc.name, got, tc.want)
		}
	}

	// THE FAILING CHECKS, required first and then by name, cut at the
	// bound and the rest counted.
	var rows []rollupRow
	for _, name := range []string{"k", "j", "i", "h", "g", "f", "e", "d", "c", "b", "a"} {
		rows = append(rows, rollupRow{name: name, attempt: 1, state: CheckStateFailed, required: name == "k"})
	}
	got := rollUp(rows, true)
	if got.State != RollupFailing || len(got.Failing) != MaxRollupFailing ||
		got.FailingMore != len(rows)-MaxRollupFailing || got.Failing[0].Name != "k" || got.Failing[1].Name != "a" ||
		!got.More {
		t.Errorf("eleven failing checks roll up to %+v, want k (required) first, then a, eight named, three "+
			"more, and the read's more kept", got)
	}
}

// A ROLLUP COUNTS EVERY CHECK STATE: each known state has its wire-named
// count, an unknown one counts as other, and every rollup state is Valid.
//
// Mutation: drop the skipped count: skipped has no field, red.
func TestARollupCountsEveryCheckState(t *testing.T) {
	t.Parallel()
	for _, s := range CheckStates() {
		var c CheckCounts
		c.count(s)
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		if want := `{"` + string(s) + `":1}`; string(raw) != want {
			t.Errorf("an attempt %s counts as %s, want %s", s, raw, want)
		}
	}
	var c CheckCounts
	c.count("paused")
	if c != (CheckCounts{Other: 1}) {
		t.Errorf("a state this build does not know counts as %+v, want other", c)
	}
	for _, s := range CheckRollupStates() {
		if !s.Valid() {
			t.Errorf("%q is a rollup state Valid does not know", s)
		}
	}
	if CheckRollupState("green").Valid() {
		t.Error("Valid knows a rollup state no build declares")
	}
}
