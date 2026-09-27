package learning_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/store"
)

// A DECLINE IS REPORTED AS ONE, AND AN ANSWER THAT DECIDED NOTHING IS NOT.
//
// `{}` is the model looking at the evidence and deciding there is no
// procedure — the ordinary answer, reported at debug as a decline. An answer
// that does not decode, or a draft missing a part, decided nothing: it is
// reported at WARN with the answer itself, and a decline line beside it would
// count a worker that has stopped working as a model with nothing to draft.
// Each case finds its own lines by the turn or the seat only it used, which is
// what lets it share the package's one log sink with every other case.
func TestOnlyADeclinedDraftIsReportedAsADecline(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		// answer is the model's, given the seat it is about.
		answer   func(seat string) string
		declined bool
		// warned is the line the unusable answer is reported on instead.
		warned string
	}{
		"a decline": {
			answer: func(string) string { return "{}" }, declined: true,
		},
		"prose": {
			answer: func(seat string) string { return "no procedure here for " + seat },
			warned: "skill_draft_undecodable",
		},
		"a draft missing a part": {
			answer: func(seat string) string {
				return `{"name":"` + seat + `","description":"d"}`
			},
			warned: "skill_draft_incomplete",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// THE SEAT'S OWN NAME IS THE CASE'S, and the answer carries it:
			// both are what finds this case's lines among every other
			// case's.
			seat := "draft-" + strings.ReplaceAll(name, " ", "-")
			answer := tc.answer(seat)

			// THE SINGLE-TURN SYNTHESIZER.
			turn := toolTurn("fetch", "build", "tag")
			turn.Event.AgentHandle, turn.Event.TurnID = seat, "turn-"+seat
			s := synthesizer(t, newStore(t), answer, learning.SynthesizerOptions{
				MinToolCalls: 1,
			})
			if out, err := s.Reflect(t.Context(), turn); err != nil || len(out) != 0 {
				t.Fatalf("Reflect = %v, %v — want nothing drafted and no failure", out, err)
			}
			declined := 0
			for _, rec := range logs.records(t, "skill_synthesis_declined") {
				if rec["turn_id"] == "turn-"+seat {
					declined++
				}
			}
			if declined != boolCount(tc.declined) {
				t.Errorf("the synthesizer reported %d declines for %q, want %d",
					declined, answer, boolCount(tc.declined))
			}

			// THE CLUSTER PASS, over runs that converge.
			db := newStore(t)
			appendRuns(t, db, seat, 4, "fetch", "build", "tag")
			c, _ := clusterer(t, db, answer, learning.SynthesizerOptions{
				MinToolCalls: 3, ClusterMinSize: 3,
			})
			if out, err := c.ClusterPass(t.Context(), clusterSeat, seat,
				clusterAgentID); err != nil || len(out) != 0 {
				t.Fatalf("ClusterPass = %v, %v — want nothing drafted and no "+
					"failure", out, err)
			}
			declined = 0
			for _, rec := range logs.records(t, "skill_clustering_declined") {
				if rec["agent_handle"] == seat {
					declined++
				}
			}
			if declined != boolCount(tc.declined) {
				t.Errorf("the cluster pass reported %d declines for %q, want %d",
					declined, answer, boolCount(tc.declined))
			}

			// AND AN ANSWER THAT DECIDED NOTHING IS REPORTED WHERE IT
			// SAYS, once per worker that read it.
			if tc.warned == "" {
				return
			}
			warned := 0
			for _, rec := range logs.records(t, tc.warned) {
				if response, _ := rec["response"].(string); strings.Contains(response, seat) {
					warned++
				}
			}
			if warned != 2 {
				t.Errorf("%s was logged %d times for this case's answer, want "+
					"once by each worker", tc.warned, warned)
			}
		})
	}
}

// boolCount is how many lines a case expects of a line it expects or does not.
func boolCount(expected bool) int {
	if expected {
		return 1
	}
	return 0
}

// appendRuns writes n settled turns of one seat that ran the same tools.
func appendRuns(t *testing.T, db *store.DB, handle string, n int, tools ...string) {
	t.Helper()
	eps := learning.NewEpisodes(db)
	base := time.Now().UTC().Add(-time.Duration(n) * time.Hour)
	for i := range n {
		if _, err := eps.Append(t.Context(), learning.Episode{
			ID: episodeID(), Handle: handle, Role: "Dev",
			TurnID:      handle + "-run-" + itoa(i),
			StartedAt:   base.Add(time.Duration(i) * time.Hour),
			EndedAt:     base.Add(time.Duration(i)*time.Hour + time.Minute),
			TaskSummary: "ship release " + itoa(i), PlanSummary: "cut, tag, announce",
			ToolSequence: tools, ReviewOutcome: "done", Kind: learning.KindRaw,
		}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
}
