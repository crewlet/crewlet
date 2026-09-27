package runner

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/subagent"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/events/types"
)

// A WORKER'S RECORD CARRIES ITS MODEL SPLIT, as the turn's own phases' do: the
// spend rollups read the split off the record, and a worker record without one
// counts every round under the model that answered first.
func TestAWorkersRecordCarriesItsModelSplit(t *testing.T) {
	t.Parallel()
	pub := &collector{}
	var mu sync.Mutex
	e := emitter{pub: pub, turn: Turn{RunID: "tn-1"}, role: "Lead", tally: &Spend{}, mu: &mu}
	e.nestedAt(1).subagentCompleted(context.Background(), subagent.Result{
		ID: "research", Status: subagent.StatusOK, Model: "first",
		InputTokens: 50, OutputTokens: 5,
		Models: []toolloop.ModelTokens{
			{Model: "first", InputTokens: 40, OutputTokens: 4},
			{Model: "second", InputTokens: 10, OutputTokens: 1},
		},
	})
	want := []types.ModelSpend{
		{Model: "first", InputTokens: 40, OutputTokens: 4},
		{Model: "second", InputTokens: 10, OutputTokens: 1},
	}
	for _, ev := range pub.events {
		if done, ok := ev.Data.(*types.AgentPhaseCompleted); ok && done.Phase == types.PhaseSubagent {
			if !reflect.DeepEqual(done.Models, want) {
				t.Errorf("models = %+v, want %+v", done.Models, want)
			}
			return
		}
	}
	t.Fatal("no worker record was published")
}
