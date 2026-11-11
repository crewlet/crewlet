package worklet_test

import (
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/worklet"
)

// A WITHDRAWN QUESTION SAYS WHY AND WHAT COVERS IT: the resolve that withdraws
// it records the asker's reply in its thread and the item or link that now
// covers it, the person's Inbox and their answered list read both, and a later
// resolve that names none keeps them. A reason that is not the asker's reply,
// a replacement that is the question's own item, a link that is not one, and
// an item that does not exist are refused before anything is written.
//
// Mutation: drop the withdrawal from the applier and the person reads no
// reason; drop the reply check and anybody's words stand as the asker's.
func TestAWithdrawnQuestionSaysWhyAndWhatCoversIt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "decide", "dev")
	assign(t, r, "covering", "dev")
	task := r.task(t, "decide").Task
	dev := r.writer.As("dev", worklet.AuthorAgent, worklet.Provenance{})
	ask := worklet.Comment{ID: "c-ask", Task: "decide", Author: "dev", AuthorKind: worklet.AuthorAgent,
		Body: "Run the migration?", Ask: "ana", Decision: decisionFixture(), CreatedAt: wednesday}
	if _, err := dev.UpdateTask(t.Context(), "op-ask", "decide", "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Comment: &ask}, worklet.ChangeComment,
		worklet.Wake{Kind: worklet.ChangeComment, Before: task, After: task, Comment: &ask,
			Thread: worklet.ThreadParties{Asked: "ana"}}.Notify(nil)); err != nil {
		t.Fatalf("ask: %v", err)
	}
	r.drain()
	why := "Covered by the pull request, which runs the migration itself."
	askID := "c-ask"
	reply := func(id, author string, to *string) {
		askOn(t, r, "op-"+id, "decide", worklet.Comment{ID: id, Task: "decide", Author: author,
			AuthorKind: worklet.AuthorAgent, Body: why, ReplyTo: to, CreatedAt: wednesday.Add(time.Minute)})
	}
	reply("c-why", "dev", &askID)
	reply("c-loose", "dev", nil)
	reply("c-other", "cy", &askID)

	for name, w := range map[string]worklet.Withdrawal{
		"a remark that replies to nothing": {Reason: "c-loose"},
		"somebody else's reply":            {Reason: "c-other"},
		"the question's own item":          {Reason: "c-why", ReplacedBy: &worklet.Replacement{Item: "decide"}},
		"a link that is not one":           {Reason: "c-why", ReplacedBy: &worklet.Replacement{URL: "ftp://x"}},
		"both an item and a link": {Reason: "c-why", ReplacedBy: &worklet.Replacement{Item: "covering",
			URL: "https://example.com/pull/1"}},
	} {
		if _, err := dev.ResolveComment(t.Context(), "op-bad", "decide", "ENG", "c-ask", &w,
			nil); err == nil {
			t.Errorf("%s: the withdrawal was written", name)
		}
	}
	missing := worklet.Withdrawal{Reason: "c-why", ReplacedBy: &worklet.Replacement{Item: "nowhere"}}
	if _, err := dev.ResolveComment(t.Context(), "op-missing", "decide", "ENG", "c-ask", &missing,
		nil); !errors.Is(err, worklet.ErrNoTask) {
		t.Errorf("a replacement naming no item answered %v, want no such task", err)
	}
	r.drain()
	if c := r.remarkOn("decide", "c-ask"); c.Resolved {
		t.Fatal("a refused withdrawal resolved the question")
	}

	w := worklet.Withdrawal{Reason: "c-why", ReplacedBy: &worklet.Replacement{Item: "covering"}}
	if _, err := dev.ResolveComment(t.Context(), "op-withdraw", "decide", "ENG", "c-ask", &w,
		nil); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	r.drain()
	check := func(where string, got *worklet.Withdrawn) {
		t.Helper()
		switch {
		case got == nil:
			t.Errorf("%s: the withdrawn question carries no withdrawal", where)
		case got.Reason != why || got.ReasonComment != "c-why":
			t.Errorf("%s: the reason reads %q from %q, want dev's reply", where, got.Reason,
				got.ReasonComment)
		case got.ReplacedBy == nil || got.ReplacedBy.Item == nil || got.ReplacedBy.Item.ID != "covering":
			t.Errorf("%s: replaced by %+v, want the covering item", where, got.ReplacedBy)
		}
	}
	notices := r.inbox(worklet.InboxQuery{Handle: "ana"}).Notices
	if len(notices) == 0 || notices[0].Ask == nil || !notices[0].Ask.Resolved {
		t.Fatalf("ana's inbox is %+v, want her asked notice reading withdrawn", notices)
	}
	check("the inbox", notices[0].Ask.Withdrawn)
	answered := r.answered("ana")
	if len(answered.Asks) != 1 || !answered.Asks[0].Answer.Resolved {
		t.Fatalf("ana's answered list is %+v, want the withdrawn question", answered.Asks)
	}
	check("the answered list", answered.Asks[0].Answer.Withdrawn)

	// RESOLVED AGAIN WITH NO WITHDRAWAL, it keeps why it was withdrawn.
	if _, err := dev.ResolveComment(t.Context(), "op-again", "decide", "ENG", "c-ask", nil,
		nil); err != nil {
		t.Fatalf("resolve again: %v", err)
	}
	r.drain()
	check("after a second resolve", r.answered("ana").Asks[0].Answer.Withdrawn)
}
