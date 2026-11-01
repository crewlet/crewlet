package toolloop_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
)

// EVERY HARNESS RUN NAMES THE ROUNDS IT MADE, observed or not, so a reader groups
// a phase's rounds under the run that made them by the round number the
// rounds, the calls and the narration already join on — never by placing
// them in time, which an unobserved run's round has no instant for.
//
// Two runs of a harness whose record shows each request: the first makes a call
// and then ends in prose, which the loop corrects in a second run. And the
// same phase on a harness whose record shows none, where each run is one round.
//
// Mutation: leave the run's first round where the previous run's ended (drop
// the mark [toolloop.Invocation.FirstRound] counts from), and the second run
// claims the first's rounds.
func TestEveryRunNamesTheRoundsItMade(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		unobserved bool
		want       [][2]int
	}{
		{"observed requests", false, [][2]int{{1, 2}, {3, 3}}},
		{"unobserved requests", true, [][2]int{{1, 1}, {2, 2}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			surf := newSurface("look", "submit_work")
			prov := &llmtest.Script{Unobserved: tc.unobserved, Runs: []llmtest.Run{
				{Responses: []llmtest.Response{
					{Text: "looking", Calls: []llmtest.Call{{Name: "look"}}},
					{Text: "here is my report: all done"},
				}},
				{Responses: []llmtest.Response{
					{Text: "submitting", Calls: []llmtest.Call{{Name: "submit_work"}}},
				}},
			}}
			res, err := run(t, toolloop.Config{Provider: prov, Surface: surf,
				TerminateAfter: []string{"submit_work"}})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(res.Invocations) != len(tc.want) {
				t.Fatalf("%d runs recorded (%+v), want %d", len(res.Invocations), res.Invocations, len(tc.want))
			}
			last := 0
			for i, inv := range res.Invocations {
				got := [2]int{inv.FirstRound, inv.LastRound}
				if got != tc.want[i] {
					t.Errorf("run %d made rounds %v, want %v", i+1, got, tc.want[i])
				}
				last = inv.LastRound
			}
			if n := len(res.Rounds); last != res.Rounds[n-1].Round {
				t.Errorf("the last run ends at round %d and the record's last round is %d", last,
					res.Rounds[n-1].Round)
			}
		})
	}
}
