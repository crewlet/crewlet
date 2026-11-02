package assistant_test

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// A WRITE THE CATALOGUE SAYS CHANGES WHO SEES SOMETHING IS PROPOSED ONLY WITH
// ITS AUDIENCE: the Surface hands the tool's own ChangesVisibility to the
// draft, so a share whose preview came back naming nobody is refused before
// anything is recorded, and the model reads the refusal.
//
// Mutation: draft with ChangesVisibility false (or derive it from anything
// but the tool) and the proposal is recorded with no audience on its card.
func TestAVisibilityWriteIsNeverProposedWithoutItsAudience(t *testing.T) {
	t.Parallel()
	share := assistanttest.Tool(t, "share_artifact")
	if !share.ChangesVisibility {
		t.Fatal("share_artifact no longer declares that it changes visibility")
	}
	real := share.Preview
	share.Preview = func(ctx context.Context, read proposal.Read, now time.Time, args map[string]any) (proposal.Preview, error) {
		p, err := real(ctx, read, now, args)
		p.Audience = nil
		return p, err
	}
	id := evals.ID("artifact/deck").String()
	f := assistanttest.NewRig(t, evals.RigOptions{
		Tools: []catalog.Tool{share},
		Acts:  []string{share.Act},
		Handler: assistanttest.Routes(map[string]exec.Response{
			"GET /artifacts/" + id: evals.JSON(map[string]any{"id": id,
				"title": "Q3 deck", "head_number": 5}),
		}),
		Rounds: []evals.Round{
			evals.Call("c1", "share_artifact",
				map[string]any{"artifact": id, "to": "ada", "role": "viewer"}),
			evals.Say("I could not share it.")}})
	f.Person = evals.Person("grace", "grace", iam.GrantAdmin)
	accepted, err := f.Ask("share", "Share the deck with Ada", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.Follow(accepted.TurnID, 0)
	if got := f.Store.Proposals(); len(got) != 0 {
		t.Fatalf("a share with no audience was proposed: %+v", got)
	}
	var result llm.Message
	for _, m := range f.Model.Seen() {
		if m.Role == llm.RoleTool {
			result = m
		}
	}
	if !result.Failed {
		t.Errorf("the model read the refused share as %q, not as a failure", result.Content)
	}
}
