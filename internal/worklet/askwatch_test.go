package worklet_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A WATCH A QUESTION ADDED ENDS WITH THE QUESTION: the person asked follows the
// item while a question here is theirs to answer, and stops once the last one is
// answered or resolved, or the item finishes; a watch they chose stays.
func TestAWatchAQuestionAddedEndsWithTheQuestion(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	watching := func(r *roundTrip, id, handle string) (bool, bool) {
		t.Helper()
		detail, err := r.reader.Task(t.Context(), id, worklet.DetailWants{},
			statelog.Freshness{Level: statelog.ReadSession})
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return slices.Contains(detail.Task.Watchers, handle), slices.Contains(detail.Task.AskWatch, handle)
	}
	n := 0
	comment := func(r *roundTrip, id, author, ask string, answers *string) {
		t.Helper()
		n++
		// AN AGENT ASKS, as the product's questions to a person are, and ana answers.
		kind := worklet.AuthorAgent
		if author == "ana" {
			kind = worklet.AuthorHuman
		}
		c := worklet.Comment{ID: "c-" + itoa(n), Task: id, Author: author, AuthorKind: kind,
			Body: "which way?", Ask: ask, Answers: answers, CreatedAt: at.Add(time.Duration(n) * time.Minute)}
		if _, err := r.writer.As(author, kind, aTurnNobodyWoke()).UpdateTask(t.Context(),
			"op-"+c.ID, id, "ENG", worklet.NoIfMatch, worklet.TaskPatch{Comment: &c}, worklet.ChangeComment,
			nil); err != nil {
			t.Fatalf("comment %s: %v", c.ID, err)
		}
		r.drain()
	}
	ref := func(s string) *string { return &s }

	t.Run("the last answer ends it", func(t *testing.T) {
		r := newRoundTrip(t)
		filedTask(t, r, "w")
		comment(r, "w", "pm", "ana", nil)
		first := "c-" + itoa(n)
		comment(r, "w", "qa", "ana", nil)
		second := "c-" + itoa(n)
		if w, asked := watching(r, "w", "ana"); !w || !asked {
			t.Fatalf("asked twice, ana watches %v (by the question %v), want both", w, asked)
		}
		comment(r, "w", "ana", "", ref(first))
		if w, _ := watching(r, "w", "ana"); !w {
			t.Error("one question answered of two, ana stopped watching")
		}
		comment(r, "w", "ana", "", ref(second))
		if w, asked := watching(r, "w", "ana"); w || asked {
			t.Errorf("both answered, ana still watches %v (by the question %v)", w, asked)
		}
	})

	// A QUESTION ITS ASKER ASKED AGAIN IS OVERTAKEN, and owes nothing: the
	// answer to the newer one ends the watch.
	t.Run("an overtaken question keeps no watch", func(t *testing.T) {
		r := newRoundTrip(t)
		filedTask(t, r, "w")
		comment(r, "w", "pm", "ana", nil)
		comment(r, "w", "pm", "ana", nil)
		comment(r, "w", "ana", "", ref("c-"+itoa(n)))
		if w, asked := watching(r, "w", "ana"); w || asked {
			t.Errorf("the newer question answered, ana still watches %v (by the question %v)", w, asked)
		}
	})

	t.Run("a watch she chose stays", func(t *testing.T) {
		r := newRoundTrip(t)
		filedTask(t, r, "w")
		comment(r, "w", "pm", "ana", nil)
		asked := "c-" + itoa(n)
		if _, err := r.writer.As("ana", worklet.AuthorHuman, worklet.Provenance{}).UpdateTask(t.Context(),
			"op-watch", "w", "ENG", worklet.NoIfMatch,
			worklet.TaskPatch{Watch: &worklet.WatchIntent{Handle: "ana", Watch: true}}, worklet.ChangeWatchers,
			nil); err != nil {
			t.Fatalf("watch: %v", err)
		}
		r.drain()
		comment(r, "w", "ana", "", ref(asked))
		if w, byAsk := watching(r, "w", "ana"); !w || byAsk {
			t.Errorf("ana chose to watch, then answered: watches %v (by the question %v), want hers to stay", w, byAsk)
		}
	})

	t.Run("a question its asker resolves ends it", func(t *testing.T) {
		r := newRoundTrip(t)
		filedTask(t, r, "w")
		comment(r, "w", "pm", "ana", nil)
		asked := "c-" + itoa(n)
		pm := r.writer.As("pm", worklet.AuthorAgent, aTurnNobodyWoke())
		if _, err := pm.ResolveComment(t.Context(), "op-resolve", "w", "ENG", asked, nil); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		r.drain()
		if w, byAsk := watching(r, "w", "ana"); w || byAsk {
			t.Errorf("pm resolved the question, ana still watches %v (by the question %v)", w, byAsk)
		}
	})

	t.Run("the item finishing ends it", func(t *testing.T) {
		r := newRoundTrip(t)
		filedTask(t, r, "w")
		comment(r, "w", "pm", "ana", nil)
		done := worklet.StatusDone
		if _, err := r.writer.UpdateTask(t.Context(), "op-done", "w", "ENG", worklet.NoIfMatch,
			worklet.TaskPatch{Status: &done}, worklet.ChangeStatus, nil); err != nil {
			t.Fatalf("done: %v", err)
		}
		r.drain()
		if w, asked := watching(r, "w", "ana"); w || asked {
			t.Errorf("the item is done, ana still watches %v (by the question %v)", w, asked)
		}
	})
}
