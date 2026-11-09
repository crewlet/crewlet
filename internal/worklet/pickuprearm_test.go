package worklet_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE PICKUP RE-ARMS ONLY ON A CHANGE THAT WOKE THE SEAT: one table drives the
// wake (did the parser start a turn for the item's holder) and the ready read
// (did the item's key move for it), each row with its declared outcome, and
// the two answer alike row by row.
func TestANoAskCommentOrWatcherChangeDoesNotReArmThePickup(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	ctx := t.Context()
	fileFor(t, r, "w-1", "ana", worklet.StatusTodo, worklet.PriorityNormal)
	parties := registry(t, "ana", "qa", "pm", "designer")
	task := func() worklet.Task {
		t.Helper()
		detail, err := r.reader.Task(ctx, "w-1", worklet.DetailWants{}, statelog.Freshness{Level: statelog.ReadSession})
		if err != nil {
			t.Fatalf("read w-1: %v", err)
		}
		return detail.Task
	}
	no, yes := false, true
	comment := func(op, author string, kind worklet.AuthorKind, asks *bool, mentions ...string) func() {
		return func() {
			t.Helper()
			before := task()
			c := worklet.Comment{ID: op, Task: "w-1", Author: author, AuthorKind: kind, Body: "noted",
				Mentions: mentions, CreatedAt: wednesday}
			wake := worklet.Wake{Kind: worklet.ChangeComment, Before: before, After: before, Comment: &c,
				Mentions: mentions, Asks: asks}
			if kind == worklet.AuthorAgent {
				wake.MentionedAgents = mentions
			}
			if _, err := r.writer.As(author, kind, worklet.Provenance{}).UpdateTask(ctx, op, "w-1", "ENG",
				worklet.NoIfMatch, worklet.TaskPatch{Comment: &c}, worklet.ChangeComment, wake.Notify(nil)); err != nil {
				t.Fatalf("comment %s: %v", op, err)
			}
			r.drain()
		}
	}
	watch := func() {
		t.Helper()
		before := task()
		after := before
		after.Watchers = append(slices.Clone(before.Watchers), "designer")
		wake := worklet.Wake{Kind: worklet.ChangeWatchers, Before: before, After: after}.Notify(nil)
		if _, err := r.writer.As("qa", worklet.AuthorAgent, worklet.Provenance{}).UpdateTask(ctx, "op-watch", "w-1",
			"ENG", worklet.NoIfMatch, worklet.TaskPatch{Watch: &worklet.WatchIntent{Handle: "designer", Watch: true}},
			worklet.ChangeWatchers, wake); err != nil {
			t.Fatalf("watch: %v", err)
		}
		r.drain()
	}
	start := func() {
		t.Helper()
		before := task()
		after := before
		after.Status, after.StatusGroup = worklet.StatusInProgress, worklet.StatusInProgress.Group()
		wake := worklet.Wake{Kind: worklet.ChangeStatus, Before: before, After: after}.Notify(nil)
		status := worklet.StatusInProgress
		if _, err := r.writer.As("qa", worklet.AuthorAgent, worklet.Provenance{}).UpdateTask(ctx, "op-start", "w-1",
			"ENG", worklet.NoIfMatch, worklet.TaskPatch{Status: &status}, worklet.ChangeStatus, wake); err != nil {
			t.Fatalf("start: %v", err)
		}
		r.drain()
	}
	for _, row := range []struct {
		name   string
		change func()
		wakes  bool
	}{
		{"another agent's comment that asks nothing and names nobody", comment("c-1", "qa", worklet.AuthorAgent, &no), false},
		{"another agent adds a watcher", watch, false},
		{"another agent's comment that asks the holder", comment("c-2", "qa", worklet.AuthorAgent, &yes, "ana"), true},
		{"the holder's own comment", comment("c-3", "ana", worklet.AuthorAgent, &no), false},
		{"a person's comment that names nobody", comment("c-4", "pm", worklet.AuthorHuman, nil), true},
		{"another agent moves the status", start, true},
	} {
		before := r.readyWork("ana").Items[0].Moved
		row.change()
		woke := r.wokeByLast(parties, "ana")
		moved := r.readyWork("ana").Items[0].Moved != before
		if woke != row.wakes {
			t.Errorf("%s woke the holder %v, want %v", row.name, woke, row.wakes)
		}
		if moved != woke {
			t.Errorf("%s moved the pickup's key %v while it woke the holder %v: the two must agree", row.name, moved, woke)
		}
	}
}

// wokeByLast reports whether the newest record on the log woke handle, through
// the change feed's translator and the parser; a record the feed does not relay
// woke nobody.
func (r *roundTrip) wokeByLast(parties *notify.Registry, handle string) bool {
	r.t.Helper()
	last, err := r.log.End(r.t.Context())
	if err != nil {
		r.t.Fatalf("read the log's end: %v", err)
	}
	_, payload, _, ok, err := r.log.At(r.t.Context(), last)
	if err != nil || !ok {
		r.t.Fatalf("read record %d: %v", last, err)
	}
	delivery, wakes, err := worklet.NewTranslator().Translate(r.t.Context(), changefeed.Record{Payload: r.open(last, payload)})
	if err != nil {
		r.t.Fatalf("translate record %d: %v", last, err)
	}
	if !wakes {
		return false
	}
	routed, err := worklet.NewParser(worklet.ParserOptions{}).Parse(r.t.Context(), types.RawWebhook{Body: delivery.Body}, parties)
	if err != nil {
		r.t.Fatalf("parse record %d: %v", last, err)
	}
	return slices.ContainsFunc(routed, func(x notify.Routed) bool { return x.To.Handle == handle && !x.Held })
}
