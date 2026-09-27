package subagent_test

import (
	"context"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// A WORKER'S RESULT CARRIES WHICH MODEL SERVED EACH OF ITS ROUNDS. A worker's
// chain can fall back from one member to the next mid-task like any phase's,
// and the result's one Model names only the first: the per-model breakdown its
// phase record feeds would bill every round to it.
func TestAWorkersResultCarriesItsModelSplit(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	p := &provider{name: "configured", reply: func(_ context.Context, n int, _ llm.Request) (*llm.Completion, error) {
		if n == 1 {
			c := callTool("read_file", nil, 40, 4)
			c.Model = "first"
			return c, nil
		}
		c := answer("found it", 10, 1)
		c.Model = "second"
		return c, nil
	}}
	res := one(t, baseConfig(t, w, p), request("read_file"))

	want := []toolloop.ModelTokens{
		{Model: "first", InputTokens: 40, OutputTokens: 4},
		{Model: "second", InputTokens: 10, OutputTokens: 1},
	}
	if !slices.Equal(res.Models, want) {
		t.Errorf("models = %+v, want each round under the model that served it %+v", res.Models, want)
	}
	if res.Model != "first" || res.Tokens() != 55 {
		t.Errorf("model %q, %d tokens: the premise is a two-model task of 55", res.Model, res.Tokens())
	}
}
