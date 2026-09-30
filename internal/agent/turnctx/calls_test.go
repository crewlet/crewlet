package turnctx_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
)

// A CALL'S COUNT IS THE DIFFERENT CALLS TO ITS TOOL BEFORE IT, so a call made
// again after a different one is a new operation and a call repeated with
// nothing different between is the same one.
func TestACallCountsTheDifferentCallsBeforeIt(t *testing.T) {
	t.Parallel()
	a := map[string]any{"item": "ENG-1", "status": "in_progress"}
	b := map[string]any{"item": "ENG-1", "status": "done"}
	other := map[string]any{"item": "ENG-1", "body": "on it"}

	counts := func(log *turnctx.CallLog, calls ...struct {
		tool string
		args map[string]any
	}) []int {
		var out []int
		for _, c := range calls {
			out = append(out, log.Ordinal(c.tool, c.args))
			log.Record(c.tool, c.args)
		}
		return out
	}
	type call = struct {
		tool string
		args map[string]any
	}
	cases := []struct {
		name  string
		calls []call
		want  []int
	}{
		{"a repeat with nothing between is one operation",
			[]call{{"update", a}, {"update", a}}, []int{0, 0}},
		{"a call made again after a different one is a new operation",
			[]call{{"update", a}, {"update", b}, {"update", a}}, []int{0, 1, 1}},
		{"and so is the different one made again after it",
			[]call{{"update", a}, {"update", b}, {"update", a}, {"update", b}}, []int{0, 1, 1, 2}},
		{"a call to another tool counts for nothing",
			[]call{{"update", a}, {"comment", other}, {"update", a}}, []int{0, 0, 0}},
		{"the arguments are compared canonically",
			[]call{{"update", a}, {"update", map[string]any{"status": "in_progress", "item": "ENG-1"}}},
			[]int{0, 0}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := counts(turnctx.NewCallLog(), c.calls...)
			if len(got) != len(c.want) {
				t.Fatalf("counts = %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("counts = %v, want %v", got, c.want)
				}
			}
		})
	}
}

// A RE-RUN REPRODUCES EVERY COUNT, because a count is a function of the calls
// the run made and nothing else — which is what lets a re-run's writes
// collapse onto the first run's call for call.
func TestARerunReproducesEveryCount(t *testing.T) {
	t.Parallel()
	calls := []map[string]any{
		{"status": "in_progress"}, {"status": "done"}, {"status": "in_progress"},
		{"status": "in_progress"}, {"status": "blocked"},
	}
	run := func() []int {
		log := turnctx.NewCallLog()
		var out []int
		for _, args := range calls {
			out = append(out, log.Ordinal("update", args))
			log.Record("update", args)
		}
		return out
	}
	first, rerun := run(), run()
	for i := range first {
		if first[i] != rerun[i] {
			t.Fatalf("the first run counted %v and the re-run %v", first, rerun)
		}
	}
}

// A RESUMED RUN SEEDED WITH WHAT IT CALLED COUNTS AS IF IT HAD NEVER PARKED.
func TestASeededLogCountsTheCallsBeforeTheSeed(t *testing.T) {
	t.Parallel()
	a, b := map[string]any{"status": "in_progress"}, map[string]any{"status": "done"}
	log := turnctx.NewCallLog()
	log.Seed(turnctx.SeededCall{Tool: "update", Args: a},
		turnctx.SeededCall{Tool: "update", Args: b})
	if got := log.Ordinal("update", a); got != 1 {
		t.Fatalf("a call made again after the seeded different one counts %d, "+
			"want 1 — the resumed run would take the first copy's id", got)
	}
}

// A WORKER COUNTS FROM THE RUN AND ITS OWN CALLS, NEVER A SIBLING'S — and its
// calls reach the run once it is absorbed.
func TestAForkCountsItsOwnCallsAndIsAbsorbed(t *testing.T) {
	t.Parallel()
	a, b := map[string]any{"status": "in_progress"}, map[string]any{"status": "done"}
	run := turnctx.NewCallLog()
	run.Record("update", a)

	first, second := run.Fork(), run.Fork()
	if got := first.Ordinal("update", b); got != 1 {
		t.Fatalf("a fork counts %d different calls before it, want the run's 1", got)
	}
	first.Record("update", b)
	if got := second.Ordinal("update", a); got != 0 {
		t.Fatalf("a sibling's call reached another fork (count %d, want 0) — a "+
			"count that depends on which sibling finished first is one a re-run "+
			"does not reproduce", got)
	}
	if got := run.Ordinal("update", a); got != 0 {
		t.Fatalf("a fork's call reached the run before it was absorbed (count %d)", got)
	}

	run.Absorb(first)
	run.Absorb(second)
	if got := run.Ordinal("update", a); got != 1 {
		t.Fatalf("after the wave the run counts %d different calls before a "+
			"repeat of its first, want the worker's 1 — the repeat would take "+
			"the first copy's id with the worker's value in place", got)
	}
}

// A NIL LOG HOLDS NOTHING AND COUNTS ZERO, which is every call as its run's
// first of its kind — the id every call derived before the log existed.
func TestANilLogCountsNothing(t *testing.T) {
	t.Parallel()
	var log *turnctx.CallLog
	log.Record("update", map[string]any{"status": "done"})
	log.Absorb(log.Fork())
	if got := log.Ordinal("update", map[string]any{"status": "todo"}); got != 0 {
		t.Fatalf("a nil log counted %d", got)
	}
	var turn *turnctx.Turn
	if turn.CallLog() != nil || turn.WithCalls(turnctx.NewCallLog()) != nil {
		t.Fatal("a nil turn has a call log")
	}
}

// A DERIVED TURN CARRIES ITS OWN LOG AND LEAVES THE ORIGINAL'S ALONE.
func TestWithCallsDerivesATurn(t *testing.T) {
	t.Parallel()
	parent := &turnctx.Turn{RunID: "run-1", WorkKey: "work-1", Calls: turnctx.NewCallLog()}
	fork := parent.Calls.Fork()
	child := parent.WithCalls(fork)
	if child == parent || child.CallLog() != fork || parent.CallLog() == fork {
		t.Fatal("WithCalls changed the parent's log rather than deriving a turn")
	}
	if child.RunID != parent.RunID || child.WorkKey != parent.WorkKey {
		t.Fatal("the derived turn lost the run it belongs to")
	}
}
