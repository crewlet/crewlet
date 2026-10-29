package worklet_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

func (r *roundTrip) answered(handle string) worklet.AnsweredAnswer {
	r.t.Helper()
	answer, err := r.reader.AnsweredAsks(r.t.Context(), worklet.DecisionsQuery{
		Handle: handle, Level: statelog.ReadStale,
	}, wednesday, time.UTC)
	if err != nil {
		r.t.Fatalf("AnsweredAsks: %v", err)
	}
	return answer
}

// answerOn writes an answer to one ask, as whoever answers it.
func answerOn(t *testing.T, r *roundTrip, opID, task, id, by, ask, choice, body string,
	at time.Time) {

	t.Helper()
	kind := worklet.AuthorHuman
	askID := ask
	askOn(t, r, opID, task, worklet.Comment{ID: id, Task: task, Author: by,
		AuthorKind: kind, Body: body, Answers: &askID, Choice: choice, CreatedAt: at})
}

var shipOrHold = &decision.Decision{
	Question: "Ship or hold?",
	Options: []decision.Option{
		{ID: "ship", Label: "Ship it"},
		{ID: "hold", Label: "Hold it"},
	},
	Recommended: "ship",
	Role:        decision.RoleApprover,
}

// WHAT A PERSON DECIDED LATELY is every ask put to them and closed, by them or
// by somebody else, newest closing first: never an ask still open, never an
// ask put to somebody else, and each with who closed it, when, and how.
func TestAnsweredAsksListWhatWasPutToThePersonAndClosed(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "a", "bo")
	asked := wednesday.Add(-5 * time.Hour)
	for _, c := range []worklet.Comment{
		{ID: "c-choice", Body: "ship?", Ask: "jane", Decision: shipOrHold},
		{ID: "c-words", Body: "which name?", Ask: "jane"},
		{ID: "c-open", Body: "still waiting?", Ask: "jane"},
		{ID: "c-ana", Body: "yours?", Ask: "ana"},
	} {
		c.Task, c.Author, c.AuthorKind, c.CreatedAt = "a", "bo", worklet.AuthorAgent, asked
		askOn(t, r, "op-"+c.ID, "a", c)
	}
	answerOn(t, r, "op-ans-1", "a", "c-ans-1", "jane", "c-choice", "ship", "Ship it",
		wednesday.Add(-2*time.Hour))
	// SOMEBODY ELSE ANSWERS FOR JANE, in words.
	answerOn(t, r, "op-ans-2", "a", "c-ans-2", "dana", "c-words", "", "Call it Ledger",
		wednesday.Add(-time.Hour))
	answerOn(t, r, "op-ans-3", "a", "c-ans-3", "ana", "c-ana", "", "Mine", wednesday)

	got := r.answered("jane")
	if len(got.Asks) != 2 {
		t.Fatalf("jane's answered asks are %+v, want the two put to her and closed", got.Asks)
	}
	words, choice := got.Asks[0], got.Asks[1]
	if words.Ask.Comment != "c-words" || choice.Ask.Comment != "c-choice" {
		t.Fatalf("the order is %s, %s; want the newest closing first", words.Ask.Comment,
			choice.Ask.Comment)
	}
	if words.Answer.By != "dana" || words.Answer.Body != "Call it Ledger" ||
		words.Answer.Choice != "" || words.Answer.Comment != "c-ans-2" ||
		!words.At.Equal(wednesday.Add(-time.Hour)) {
		t.Errorf("the answer in words is %+v at %v, want dana's words an hour ago",
			words.Answer, words.At)
	}
	if choice.Answer.By != "jane" || choice.Answer.Choice != "ship" ||
		choice.Ask.Decision == nil || choice.Ask.Decision.Recommended != "ship" {
		t.Errorf("the chosen answer is %+v on %+v, want jane choosing ship on the "+
			"ask's own decision", choice.Answer, choice.Ask.Decision)
	}
	for _, a := range got.Asks {
		if a.Ask.Open || a.Ask.Answer != "" || a.Ask.Key == "" || a.Ask.AskedBy != "bo" {
			t.Errorf("answered ask %s reads open=%v answer_with=%q key=%q asked_by=%q; "+
				"want a closed ask on its task with no call to answer it again",
				a.Ask.Comment, a.Ask.Open, a.Ask.Answer, a.Ask.Key, a.Ask.AskedBy)
		}
	}
	if want := wednesday.Add(-worklet.AnsweredWindow); !got.Since.Equal(want) {
		t.Errorf("since = %v, want %v", got.Since, want)
	}
}

// THE WINDOW IS A WORKING WEEK: an ask closed before it is on its task's
// thread, not in this list.
func TestAnsweredAsksKeepToTheWindow(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "a", "bo")
	old := wednesday.Add(-worklet.AnsweredWindow - time.Hour)
	for _, id := range []string{"c-old", "c-new"} {
		askOn(t, r, "op-"+id, "a", worklet.Comment{ID: id, Task: "a", Author: "bo",
			AuthorKind: worklet.AuthorAgent, Body: "?", Ask: "jane", CreatedAt: old})
	}
	answerOn(t, r, "op-ans-old", "a", "c-ans-old", "jane", "c-old", "", "yes", old)
	answerOn(t, r, "op-ans-new", "a", "c-ans-new", "jane", "c-new", "", "no",
		wednesday.Add(-worklet.AnsweredWindow+time.Hour))

	got := r.answered("jane")
	if len(got.Asks) != 1 || got.Asks[0].Ask.Comment != "c-new" {
		t.Errorf("the answered asks are %+v, want only the one closed inside the window",
			got.Asks)
	}
}

// AN ASK RESOLVED WITHOUT AN ANSWER is closed too, and says so, with who
// resolved it and when.
func TestAnAskResolvedWithoutAnAnswerIsListedAsResolved(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "a", "bo")
	ask := worklet.Comment{ID: "c-1", Task: "a", Author: "bo", AuthorKind: worklet.AuthorAgent,
		Body: "still needed?", Ask: "jane", CreatedAt: wednesday.Add(-3 * time.Hour)}
	askOn(t, r, "op-ask", "a", ask)
	resolvedAt := wednesday.Add(-time.Hour)
	ask.Resolved, ask.ResolvedBy, ask.ResolvedAt = true, "bo", &resolvedAt
	if _, err := r.writer.UpdateTask(t.Context(), "op-resolve", "a", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Comment: &ask}, worklet.ChangeCommentResolved,
		nil); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	r.drain()

	got := r.answered("jane")
	if len(got.Asks) != 1 {
		t.Fatalf("the answered asks are %+v, want the resolved one", got.Asks)
	}
	a := got.Asks[0].Answer
	if !a.Resolved || a.By != "bo" || !a.At.Equal(resolvedAt) || a.Comment != "" {
		t.Errorf("the resolution reads %+v, want resolved by bo at %v with no answer comment",
			a, resolvedAt)
	}
}

// WHAT THE ASKER DID NEXT is its first record on the task after the answer:
// nothing before it acts, a colleague's change never, and then the change it
// made, with the deltas the history holds.
func TestAnsweredAsksSayWhatTheAskerDidNext(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "a", "bo")
	askOn(t, r, "op-ask", "a", worklet.Comment{ID: "c-1", Task: "a", Author: "bo",
		AuthorKind: worklet.AuthorAgent, Body: "ship?", Ask: "jane", Decision: shipOrHold,
		CreatedAt: wednesday.Add(-3 * time.Hour)})
	answerOn(t, r, "op-ans", "a", "c-2", "jane", "c-1", "ship", "Ship it",
		wednesday.Add(-2*time.Hour))
	if got := r.answered("jane"); len(got.Asks) != 1 || got.Asks[0].FollowedUp != nil {
		t.Fatalf("before the asker acts the answered ask is %+v, want no follow-up", got.Asks)
	}

	// A COLLEAGUE'S CHANGE IS NOT THE ASKER'S.
	askOn(t, r, "op-other", "a", worklet.Comment{ID: "c-3", Task: "a", Author: "cy",
		AuthorKind: worklet.AuthorAgent, Body: "noted", CreatedAt: wednesday.Add(-90 * time.Minute)})
	bo := r.writer.As("bo", worklet.AuthorAgent, worklet.Provenance{})
	done := worklet.StatusInProgress
	if _, err := bo.UpdateTask(t.Context(), "op-move", "a", "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Status: &done}, worklet.ChangeStatus, nil); err != nil {
		t.Fatalf("move: %v", err)
	}
	r.drain()
	if _, err := bo.UpdateTask(t.Context(), "op-move-again", "a", "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Comment: &worklet.Comment{ID: "c-4", Task: "a", Author: "bo",
			AuthorKind: worklet.AuthorAgent, Body: "shipping", CreatedAt: wednesday}},
		worklet.ChangeComment, nil); err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()

	got := r.answered("jane")
	if len(got.Asks) != 1 || got.Asks[0].FollowedUp == nil {
		t.Fatalf("after the asker acts the answered ask is %+v, want its follow-up", got.Asks)
	}
	f := got.Asks[0].FollowedUp
	if f.Kind != worklet.ChangeStatus || f.Fields["status"].To != string(worklet.StatusInProgress) {
		t.Errorf("the follow-up is %+v, want the asker's FIRST record after the answer: "+
			"its move to in progress", f)
	}
}

// THE PAGE IS [worklet.MaxAnswered], and the answer says the window held more.
func TestAnsweredAsksSayWhenTheWindowHeldMoreThanThePage(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "a", "bo")
	const asks = worklet.MaxAnswered + 2
	for i := range asks {
		id := fmt.Sprintf("c-%02d", i)
		askOn(t, r, "op-"+id, "a", worklet.Comment{ID: id, Task: "a", Author: "bo",
			AuthorKind: worklet.AuthorAgent, Body: "?", Ask: "jane",
			CreatedAt: wednesday.Add(-2 * time.Hour)})
		answerOn(t, r, "op-ans-"+id, "a", "ans-"+id, "jane", id, "", "yes",
			wednesday.Add(-time.Duration(i)*time.Minute))
	}
	got := r.answered("jane")
	if len(got.Asks) != worklet.MaxAnswered || !got.Truncated {
		t.Errorf("the page carries %d (truncated=%v), want %d and truncated",
			len(got.Asks), got.Truncated, worklet.MaxAnswered)
	}
	if got.Asks[0].Ask.Comment != "c-00" {
		t.Errorf("the page opens on %s, want the newest answer's ask", got.Asks[0].Ask.Comment)
	}
}
