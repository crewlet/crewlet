package builtin_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/learning"
)

// attributingRecall is a [builtin.AttributingRecaller]: each binding records
// whom it was told the recall serves.
type attributingRecall struct {
	*fakeRecall
	bound []learning.Attribution
}

func (a *attributingRecall) For(who learning.Attribution) builtin.Recaller {
	a.bound = append(a.bound, who)
	return a.fakeRecall
}

// A RECALL NAMES THE TURN THAT ASKED FOR IT. The recaller is built once per
// company revision, before any turn exists, so it cannot know which turn a
// recall serves; the tool can, and binds it before every recall — the turn's
// run and its unit of work, under the recall worker — so the auxiliary call a
// recall makes is recorded on the turn that made it rather than on none.
func TestTheRecallToolsBindTheTurnTheyServe(t *testing.T) {
	t.Parallel()
	want := learning.Attribution{Worker: learning.RecallWorker, TurnID: "run-1", WorkKey: "wk-1"}
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{builtin.QueryEpisodesTool, map[string]any{"query": "search indexing"}},
		{builtin.RefreshMemoryTool, map[string]any{"context_hint": "fixing the indexing bug"}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			t.Parallel()
			recall := &attributingRecall{fakeRecall: &fakeRecall{}}
			tool := registered(t, builtin.Deps{
				Episodes: &countingEpisodes{}, Diary: &countingDiary{}, Recall: recall,
			}, tc.tool)
			if res := callFor(t, tool, turnFor(t, "agent-ceo"), tc.args); res.Failed {
				t.Fatalf("%s failed: %q", tc.tool, res.Output)
			}
			if len(recall.bound) != 1 || recall.bound[0] != want {
				t.Errorf("bound %+v, want the recall bound once to %+v", recall.bound, want)
			}
		})
	}
}
