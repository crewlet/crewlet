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
	fileFor(t, r, "w-1", "ana", worklet.StatusTodo, worklet.PriorityNormal)
	parties := registry(t, "ana", "qa", "pm", "designer")
	// change writes one change to w-1 as author, its notification built as the
	// write paths build it from the item before and after.
	change := func(op, author string, kind worklet.AuthorKind, patch worklet.TaskPatch,
		changeKind worklet.ChangeKind, after func(*worklet.Task), wake func(*worklet.Wake)) func() {
		return func() {
			t.Helper()
			detail, err := r.reader.Task(t.Context(), "w-1", worklet.DetailWants{},
				statelog.Freshness{Level: statelog.ReadSession})
			if err != nil {
				t.Fatalf("read w-1: %v", err)
			}
			w := worklet.Wake{Kind: changeKind, Before: detail.Task, After: detail.Task}
			if after != nil {
				after(&w.After)
			}
			if wake != nil {
				wake(&w)
			}
			if _, err := r.writer.As(author, kind, worklet.Provenance{}).UpdateTask(t.Context(), op, "w-1", "ENG",
				worklet.NoIfMatch, patch, changeKind, w.Notify(nil)); err != nil {
				t.Fatalf("%s: %v", op, err)
			}
			r.drain()
		}
	}
	no, yes := false, true
	comment := func(op, author string, kind worklet.AuthorKind, asks *bool, mentions ...string) func() {
		c := worklet.Comment{ID: op, Task: "w-1", Author: author, AuthorKind: kind, Body: "noted",
			Mentions: mentions, CreatedAt: wednesday}
		return change(op, author, kind, worklet.TaskPatch{Comment: &c}, worklet.ChangeComment, nil,
			func(w *worklet.Wake) {
				w.Comment, w.Mentions, w.Asks = &c, mentions, asks
				if kind == worklet.AuthorAgent {
					w.MentionedAgents = mentions
				}
			})
	}
	watch := change("op-watch", "qa", worklet.AuthorAgent,
		worklet.TaskPatch{Watch: &worklet.WatchIntent{Handle: "designer", Watch: true}}, worklet.ChangeWatchers,
		func(t *worklet.Task) { t.Watchers = append(slices.Clone(t.Watchers), "designer") }, nil)
	started := worklet.StatusInProgress
	start := change("op-start", "qa", worklet.AuthorAgent, worklet.TaskPatch{Status: &started},
		worklet.ChangeStatus, func(t *worklet.Task) { t.Status, t.StatusGroup = started, started.Group() }, nil)
	urgent := worklet.PriorityUrgent
	prioritise := change("op-priority", "pm", worklet.AuthorHuman, worklet.TaskPatch{Priority: &urgent},
		worklet.ChangeFields, func(t *worklet.Task) { t.Priority = urgent }, nil)
	holder := "ana"
	reassign := change("op-reassign", "qa", worklet.AuthorAgent, worklet.TaskPatch{Assignee: &holder},
		worklet.ChangeAssignee, nil, nil)
	reassignByPerson := change("op-reassign-person", "pm", worklet.AuthorHuman,
		worklet.TaskPatch{Assignee: &holder}, worklet.ChangeAssignee, nil, nil)
	for _, row := range []struct {
		name   string
		change func()
		wakes  *bool
	}{
		{"another agent's comment that asks nothing and names nobody", comment("c-1", "qa", worklet.AuthorAgent, &no), &no},
		{"another agent's comment that names the holder and asks nothing", comment("c-2", "qa", worklet.AuthorAgent, &no, "ana"), &no},
		{"another agent adds a watcher", watch, &no},
		// A REASSIGNMENT TO THE HOLDER CHANGES NOTHING, whoever makes it.
		{"another agent assigns the item to its holder again", reassign, &no},
		{"a person assigns the item to its holder again", reassignByPerson, &no},
		{"another agent's comment that asks the holder", comment("c-3", "qa", worklet.AuthorAgent, &yes, "ana"), &yes},
		{"the holder's own comment", comment("c-4", "ana", worklet.AuthorAgent, &no), &no},
		{"a person's comment that names nobody", comment("c-5", "pm", worklet.AuthorHuman, nil), &yes},
		{"a person raises the priority", prioritise, &yes},
		{"another agent moves the status", start, &yes},
	} {
		before := r.readyWork("ana").Items[0].Moved
		row.change()
		woke := r.wokeByLast(parties, "ana")
		moved := r.readyWork("ana").Items[0].Moved != before
		if row.wakes != nil && woke != *row.wakes {
			t.Errorf("%s woke the holder %v, want %v", row.name, woke, *row.wakes)
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
