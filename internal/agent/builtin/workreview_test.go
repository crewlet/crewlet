package builtin_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/worklet"
)

// AN AGENT'S REVIEW NAMES ITS REVIEWER (ADR-0124): moving an item it keeps into
// review names `reviewer`. A colleague seat is handed the item, as an
// assignment; a person needs the agent's open ask to them on the item, which
// is how the review reaches their Inbox, and the item stays with the agent.
// Handing the item to a colleague seat names its reviewer by itself. The agent
// itself, a person handed the item with no question, a reviewer beside a
// different assignee, and a reviewer of an item left out of review are refused
// before anything is written. A person's own move is theirs.
//
// Mutation: return nil from holdReview and every self-submitted review is held
// by nobody; drop the open-ask check and a person is named reviewer of a review
// that never reaches them.
func TestAnAgentsReviewNamesItsReviewer(t *testing.T) {
	t.Parallel()
	roster := func() []colleague.Seat {
		return []colleague.Seat{
			{Handle: "eng", Name: "Engineer", Kind: "agent"},
			{Handle: "qa", Name: "Quality", Kind: "agent"},
			{Handle: "pm", Name: "Pat Manager", Kind: "agent"},
			{Handle: "ana", Name: "Ana Lind", Kind: "human"},
		}
	}
	person := func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
		return builtin.Actor{Handle: "ana", Kind: worklet.AuthorHuman}, nil
	}
	asked := worklet.Comment{ID: "c-1", Author: "eng", AuthorKind: worklet.AuthorAgent,
		Body: "Approve the release notes?", Ask: "ana"}
	for _, c := range []struct {
		name     string
		args     map[string]any
		actor    func(context.Context, *turnctx.Turn) (builtin.Actor, error)
		comments []worklet.Comment
		// assignee is what the written patch hands the item to, "" for none.
		assignee string
		refused  string
	}{
		{name: "no reviewer", args: map[string]any{"status": "in_review"}, refused: "`reviewer`"},
		{name: "a colleague", args: map[string]any{"status": "in_review", "reviewer": "qa"},
			assignee: "qa"},
		{name: "a person not asked", args: map[string]any{"status": "in_review", "reviewer": "ana"},
			refused: "`ask`"},
		{name: "a person asked", args: map[string]any{"status": "in_review", "reviewer": "ana"},
			comments: []worklet.Comment{asked}},
		{name: "itself", args: map[string]any{"status": "in_review", "reviewer": "eng"},
			refused: "your own work"},
		{name: "a reviewer out of review", args: map[string]any{"status": "in_progress",
			"reviewer": "qa"}, refused: "`in_review`"},
		{name: "handed to a colleague", args: map[string]any{"status": "in_review", "assignee": "qa"},
			assignee: "qa"},
		{name: "handed to a person not asked", args: map[string]any{"status": "in_review",
			"assignee": "ana"}, refused: "`reviewer`"},
		{name: "a reviewer beside another assignee", args: map[string]any{"status": "in_review",
			"reviewer": "qa", "assignee": "pm"}, refused: "Name one of them"},
		{name: "a person's own move", args: map[string]any{"status": "in_review"}, actor: person},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			item := trk.tasks["ENG-1"]
			item.Task.Assignee, item.Comments = "eng", c.comments
			trk.tasks["ENG-1"] = item
			reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as, Seats: roster,
				Actor: c.actor})
			args := map[string]any{"item": "ENG-1"}
			for k, v := range c.args {
				args[k] = v
			}
			got := callWork(t, reg, builtin.UpdateWorkItemTool, args)
			if c.refused != "" {
				if !got.Failed || len(trk.patched) != 0 || !strings.Contains(got.Output, c.refused) {
					t.Fatalf("answered %q and wrote %d, want a refusal naming %s", got.Output,
						len(trk.patched), c.refused)
				}
				return
			}
			if got.Failed || len(trk.patched) != 1 {
				t.Fatalf("answered %q and wrote %d, want the move written", got.Output, len(trk.patched))
			}
			p := trk.patched[0]
			switch {
			case c.assignee == "" && p.Assignee != nil:
				t.Errorf("the move handed the item to %q, want it kept", *p.Assignee)
			case c.assignee != "" && (p.Assignee == nil || *p.Assignee != c.assignee):
				t.Errorf("the move handed the item to %v, want %s", p.Assignee, c.assignee)
			case c.assignee != "" && trk.kinds[0] != worklet.ChangeAssignee:
				t.Errorf("the hand-off was written as %q, want an assignment", trk.kinds[0])
			}
		})
	}
}
