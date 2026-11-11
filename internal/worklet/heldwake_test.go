package worklet_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/worklet"
)

// AN AGENT THE CHANGE TELLS AND DOES NOT WAKE IS ITS HELD WAKE, through the
// change feed's translator and the parser: an agent's comment that asks nothing
// holds the agent it names, under the reason it was told, and wakes it never.
func TestAnAgentToldAndNotWokenIsAHeldWake(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.createTask("release checklist")
	comment := worklet.Comment{ID: "c-1", Task: created.ID, Author: "pm", AuthorKind: worklet.AuthorAgent,
		Body: "Nothing for @qa, it still reads right.", Mentions: []string{"qa"}, CreatedAt: wednesday}
	no := false
	wake := worklet.Wake{Kind: worklet.ChangeComment, Before: created, After: created,
		Comment: &comment, Mentions: comment.Mentions, Asks: &no, MentionedAgents: []string{"qa"}}.Notify(nil)
	pm := r.writer.As("pm", worklet.AuthorAgent, aTurnNobodyWoke())
	if _, err := pm.UpdateTask(t.Context(),
		"op-c-1", created.ID, "ENG", worklet.NoIfMatch, worklet.TaskPatch{Comment: &comment},
		worklet.ChangeComment, wake); err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()
	routed, _ := r.routedOfLast(registry(t, "pm", "qa"))
	var held, woken int
	for _, c := range routed {
		if c.To.Handle != "qa" {
			continue
		}
		if !c.Held {
			woken++
			continue
		}
		held++
		if want := (types.TriggerOrigin{Kind: types.OriginWorkItem, ID: created.Key,
			Reason: string(worklet.ReasonMention)}); c.Origin != want {
			t.Errorf("the held wake's origin is %+v, want %+v", c.Origin, want)
		}
		if c.WakeID.String() == "" || c.WakeAt.IsZero() {
			t.Error("the held wake carries no derived id or instant, so a redelivery would count it twice")
		}
	}
	if held != 1 || woken != 0 {
		t.Fatalf("qa was held %d times and woken %d, want held once and never woken", held, woken)
	}
}
