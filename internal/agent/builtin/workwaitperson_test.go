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

// A WAIT ON A PERSON IS AN ASK (ADR-0122): an agent's `wait_until` whose reason
// names a person, by `@handle` or by the name the chart gives them, is refused
// naming `ask`, unless the agent holds an open ask to that person on the item.
// A reason naming nobody, or only an agent, a word that merely contains a
// handle, a handle or a name used as the common word it also is, a person the
// directory withholds, a person's own wait, and a wait on a person the agent
// asked who has not answered are written.
//
// Mutation: return nil from checkWaitOnPerson, and an agent's date stands in
// for the person it waits on, who never hears of it; match a bare handle, and
// "the release will be cut Friday" is refused as a wait on will.
func TestAWaitOnAPersonIsAnAsk(t *testing.T) {
	t.Parallel()
	roster := func() []colleague.Seat {
		return []colleague.Seat{
			{Handle: "pm", Name: "Pat Manager", Kind: "agent"},
			{Handle: "ana", Name: "Ana Lind", Kind: "human"},
			{Handle: "will", Name: "Will", Kind: "human"},
			{Handle: "max", Name: "Max Ito", Kind: "human", Withheld: true},
		}
	}
	person := func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
		return builtin.Actor{Handle: "jane", Kind: worklet.AuthorHuman}, nil
	}
	asked := worklet.Comment{ID: "c-1", Author: "eng", AuthorKind: worklet.AuthorAgent,
		Body: "Can you post the results?", Ask: "ana"}
	answer := "c-1"
	answered := worklet.Comment{ID: "c-2", Author: "ana", AuthorKind: worklet.AuthorHuman,
		Body: "Posted.", Answers: &answer}
	for _, c := range []struct {
		name     string
		reason   string
		actor    func(context.Context, *turnctx.Turn) (builtin.Actor, error)
		comments []worklet.Comment
		// waitsOn is the person a refusal names, empty where the wait is written.
		waitsOn string
	}{
		{"a person by name", "Waiting for Ana Lind to post the results", nil, nil, "ana"},
		{"a person by handle", "waits on @ana's sign-off", nil, nil, "ana"},
		{"a bare handle", "ana has not confirmed", nil, nil, ""},
		{"a handle that is a common word", "the release will be cut Friday", nil, nil, ""},
		{"a one-word name as written", "until Will confirms the window", nil, nil, "will"},
		{"a withheld person", "until @max signs off", nil, nil, ""},
		{"a person asked who has answered", "Waiting for Ana Lind again", nil,
			[]worklet.Comment{asked, answered}, "ana"},
		{"a person asked who has not answered", "Waiting for Ana Lind to post the results", nil,
			[]worklet.Comment{asked}, ""},
		{"an agent", "Waiting for pm's draft", nil, nil, ""},
		{"nobody", "the vendor's window opens then", nil, nil, ""},
		{"a word holding a handle", "the banana shipment lands then", nil, nil, ""},
		{"a person's own wait", "Waiting for Ana Lind to post the results", person, nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			item := trk.tasks["ENG-1"]
			item.Comments = c.comments
			trk.tasks["ENG-1"] = item
			reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as, Seats: roster, Actor: c.actor})
			got := callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
				"item": "ENG-1", "wait_until": "2031-04-20", "wait_reason": c.reason})
			refused := c.waitsOn != ""
			switch {
			case refused && (!got.Failed || len(trk.patched) != 0):
				t.Fatalf("answered %q and wrote %d, want the wait refused", got.Output, len(trk.patched))
			case refused && (!strings.Contains(got.Output, "`ask`") ||
				!strings.Contains(got.Output, `"`+c.waitsOn+`"`)):
				t.Errorf("the refusal %q does not name %s and `ask`", got.Output, c.waitsOn)
			case !refused && (got.Failed || len(trk.patched) != 1):
				t.Fatalf("answered %q and wrote %d, want the wait written", got.Output, len(trk.patched))
			}
		})
	}
}
