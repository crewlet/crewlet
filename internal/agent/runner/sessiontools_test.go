package runner_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/tools"
)

// sessionTools registers the seat's session tools, as reads, as the builtins
// annotate them.
func sessionTools(t *testing.T, reg *tools.Registry) {
	t.Helper()
	for _, name := range []string{builtin.ListSessionsTool, builtin.ReadSessionTool} {
		if err := reg.RegisterWith(stubTool{name: name, out: "a past run's transcript"},
			tools.OriginBuiltin, builtin.AnnotationsFor(name)); err != nil {
			t.Fatalf("Register %s: %v", name, err)
		}
	}
}

// THE REVIEWER IS OFFERED NO SESSION TOOL. It judges the turn's engine-side
// record — the run's bridged calls, what the run reported delivering — and
// never the executor's memory (#519), so a seat whose registry holds the
// session tools offers its reviewer its submission alone, and a reviewer that
// calls one anyway is refused by its harness, which was never offered it.
//
// Mutation: activate the seat's registry on the review's surface, and every
// review request offers the session tools.
func TestTheReviewerIsOfferedNoSessionTool(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{review: []llmtest.Response{
		calling(callOf(t, builtin.ReadSessionTool, map[string]any{"turn_id": "turn-1"})),
		submitCall(t, runner.SubmitReviewTool, `{"decision":"done","final_artifact":"a"}`),
	}}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}}, buildOpts{register: sessionTools})
	if _, err := r.Review(context.Background(), 1, workFor("posted"), nil); err != nil {
		t.Fatalf("Review: %v", err)
	}
	offered := prov.offeredFor("review")
	if len(offered) < 2 {
		t.Fatalf("the reviewer made %d model requests, want the refused read and its answer", len(offered))
	}
	for i, got := range offered {
		if !slices.Equal(got, []string{runner.SubmitReviewTool}) {
			t.Errorf("review call %d offered %v, want only %s: a session is the executor's memory, "+
				"which the reviewer never judges by", i+1, got, runner.SubmitReviewTool)
		}
	}
	if got := prov.answerTo("review", builtin.ReadSessionTool); !strings.Contains(got, "No such tool") {
		t.Errorf("the reviewer's read was answered %q, want its harness's refusal", got)
	}
}
