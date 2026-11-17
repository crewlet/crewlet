package worklet_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// hold files an open item held by holder at priority; the writer mints its key.
func hold(t *testing.T, r *roundTrip, id, holder string, priority worklet.Priority) {
	t.Helper()
	task := newTask(id)
	task.Assignee, task.Priority = holder, priority
	if _, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
		t.Fatalf("CreateTask %s: %v", id, err)
	}
	r.drain()
}

func (r *roundTrip) openWork(t *testing.T, agents, people []string) worklet.OpenWork {
	t.Helper()
	now := time.Now().UTC()
	got, err := r.reader.WorkPatterns(t.Context(), worklet.PatternsQuery{
		Since: now.Add(-7 * 24 * time.Hour), Until: now.Add(time.Microsecond),
		Level: statelog.ReadStale, QuietSince: now, Agents: agents, People: people})
	if err != nil {
		t.Fatalf("WorkPatterns: %v", err)
	}
	return got.Open
}

// OPEN WORK IS READ BY HOLDER AND BY WHAT WAITS ON WHAT, across the company.
//
// dev holds four items, one urgent and one high; qa one; a finished item
// counts nothing. root is waited on by a and b, a by c, and b and c by d (once):
// four. A question to the owner on gate holds e and f, which agents hold with
// no question of their own, and g, which waits on its own question.
//
// Mutation: count a waiter twice, count finished work, or count an agent item
// that waits on its own question as ready, and a check goes red.
func TestOpenWorkIsReadByHolderAndByWhatWaitsOnWhat(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, it := range []struct {
		id, holder string
		p          worklet.Priority
	}{
		{"root", "dev", worklet.PriorityUrgent}, {"a", "dev", worklet.PriorityHigh},
		{"b", "dev", worklet.PriorityNormal}, {"c", "qa", worklet.PriorityNormal},
		{"d", "dev", worklet.PriorityLow}, {"gone", "dev", worklet.PriorityUrgent},
		{"gate", "owner", worklet.PriorityNormal}, {"e", "ops", worklet.PriorityNormal},
		{"f", "ops", worklet.PriorityNormal}, {"g", "ops", worklet.PriorityNormal},
	} {
		hold(t, r, it.id, it.holder, it.p)
	}
	key := map[string]string{}
	for i, id := range []string{"root", "a", "b", "c", "d", "gone", "gate", "e", "f", "g"} {
		key[id] = fmt.Sprintf("ENG-%d", i+1)
	}
	for _, w := range [][2]string{{"a", "root"}, {"b", "root"}, {"c", "a"}, {"d", "b"}, {"d", "c"},
		{"gone", "root"}, {"e", "gate"}, {"f", "gate"}, {"g", "gate"}} {
		waitsOn(t, r, w[0], w[1])
	}
	done := worklet.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-gone-done", "gone", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Status: &done}, worklet.ChangeStatus, nil); err != nil {
		t.Fatalf("finish gone: %v", err)
	}
	r.drain()
	askOn(t, r, "op-ask-gate", "gate", worklet.Comment{ID: "c-gate", Task: "gate", Author: "ops",
		AuthorKind: worklet.AuthorAgent, Body: "Ship it?", Ask: "owner", CreatedAt: wednesday})
	askOn(t, r, "op-ask-g", "g", worklet.Comment{ID: "c-g", Task: "g", Author: "ops",
		AuthorKind: worklet.AuthorAgent, Body: "Which one?", Ask: "owner", CreatedAt: wednesday})

	got := r.openWork(t, []string{"dev", "qa", "ops"}, []string{"owner"})
	if len(got.Holders) == 0 || got.Holders[0].Handle != "dev" || got.Holders[0].Open != 4 ||
		got.Holders[0].Pressing != 2 || !slices.Equal(got.Holders[0].Keys, []string{key["root"], key["a"], key["b"], key["d"]}) {
		t.Errorf("first holder = %+v, want dev with 4 open, 2 pressing, the urgent first", got.Holders)
	}
	byKey := map[string]worklet.WaitedOn{}
	for _, b := range got.Blockers {
		byKey[b.Key] = b
	}
	root := byKey[key["root"]]
	if root.Blocks != 4 || root.Holder != "dev" || root.Ready != 4 || len(root.Asked) != 0 {
		t.Errorf("root = %+v, want 4 waiting (a, b, c, d once; the finished one none), all ready, held by dev", root)
	}
	if got.Blockers[0].Key != key["root"] {
		t.Errorf("first blocker = %s, want the most waited on, root", got.Blockers[0].Key)
	}
	gate := byKey[key["gate"]]
	if gate.Blocks != 3 || gate.Ready != 2 || !slices.Equal(gate.ReadyKeys, []string{key["e"], key["f"]}) ||
		!slices.Equal(gate.Asked, []string{"owner"}) || !slices.Equal(gate.Asks, []string{"c-gate"}) ||
		!gate.HeldByPerson(func(h string) bool { return h == "owner" }) {
		t.Errorf("gate = %+v, want 3 waiting, 2 of them ready (e, f; g waits on its own question), "+
			"held by the question c-gate to owner", gate)
	}
}
