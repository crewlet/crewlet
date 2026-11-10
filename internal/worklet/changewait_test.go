package worklet_test

import (
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// AN ITEM WAITS ON A PULL REQUEST UNTIL ITS HOST SAYS IT MERGED OR CLOSED: the
// edge has no mirror and is never one-sided, the item reads blocked, and the
// engine's settle clears it without moving the item's status, telling its
// holder to update it, once however many times it is written.
func TestAnItemWaitingOnAPullRequestIsClearedByItsSettle(t *testing.T) {
	t.Parallel()
	const pr, other = "https://github.com/acme/web/pull/12", "https://github.com/acme/web/pull/13"
	r := newRoundTrip(t)
	fileFor(t, r, "w", "app", worklet.StatusTodo, worklet.PriorityLow)
	fileFor(t, r, "v", "app", worklet.StatusTodo, worklet.PriorityLow)
	for _, id := range []string{"w", "v"} {
		if _, err := r.writer.Depend(t.Context(), "op-wait-"+id, worklet.DependencyChange{
			Task: id, Project: "ENG", WaitingOnAdd: []string{map[string]string{"w": pr, "v": other}[id]},
		}, fixedLeads{project: "eng-lead"}); err != nil {
			t.Fatalf("wait %s on its pull request: %v", id, err)
		}
	}
	r.drain()
	if got := r.task(t, "w"); !got.Blocked {
		t.Fatalf("an item waiting on an open pull request reads unblocked")
	}
	if flagged := r.flagged(t, "one_sided"); len(flagged) != 0 {
		t.Errorf("a pull request edge is flagged one-sided on %v; it has no mirror to wait for", flagged)
	}
	open, err := r.reader.OpenChanges(t.Context(), "", 10, statelog.ReadStale)
	if err != nil || len(open) != 2 || open[0] != pr || open[1] != other {
		t.Fatalf("open changes = %v (%v), want both pull requests", open, err)
	}
	if _, err := r.writer.Depend(t.Context(), "op-block", worklet.DependencyChange{
		Task: "w", Project: "ENG", BlockingAdd: []string{pr},
	}, nil); !errors.Is(err, worklet.ErrInvalid) {
		t.Errorf("a pull request made to wait on an item: %v, want it refused", err)
	}

	settle := func(id, state string) {
		t.Helper()
		waiting, err := r.reader.WaitingOnChange(t.Context(), map[string]string{"w": pr, "v": other}[id],
			statelog.ReadStale)
		if err != nil || len(waiting) != 1 || waiting[0].ID != id {
			t.Fatalf("the tasks waiting on %s's pull request are %v (%v)", id, waiting, err)
		}
		at := time.Date(2026, 3, 6, 9, 0, 0, 0, time.UTC)
		s := worklet.ChangeSettlement{Ref: waiting[0].Relations[0].Other, State: state, At: at,
			Source: worklet.SettledByWebhook}
		for range 2 {
			if _, err := r.writer.SettleChange(t.Context(), "op-settle-"+id, waiting[0], s); err != nil {
				t.Fatalf("settle %s: %v", id, err)
			}
			r.drain()
		}
	}
	settle("w", worklet.ChangeMerged)
	settle("v", worklet.ChangeClosed)
	for id, state := range map[string]string{"w": worklet.ChangeMerged, "v": worklet.ChangeClosed} {
		got := r.task(t, id)
		if got.Blocked || got.Task.Status != worklet.StatusTodo {
			t.Errorf("%s after its pull request %s: blocked %v, status %s; want unblocked and unmoved",
				id, state, got.Blocked, got.Task.Status)
		}
		ref := got.Task.Relations[0].Other
		if s := got.Task.Changes[ref]; s.State != state {
			t.Errorf("%s records its pull request as %+v, want %s", id, s, state)
		}
	}
	if n := r.count(`SELECT COUNT(*) FROM tracker_history WHERE kind = 'change_settled'`); n != 2 {
		t.Errorf("%d settle records, want one per item however many times it was written", n)
	}
	var told int
	for _, n := range r.wakes(t) {
		if n.Kind != worklet.ChangeLinkSettled {
			continue
		}
		if heard, ok := worklet.CandidateFor(n, "app"); ok && heard.Addressed && heard.Wakes {
			told++
		}
	}
	if told != 2 {
		t.Errorf("the holder was woken and asked %d times, want once per settled item", told)
	}
	if open, _ := r.reader.OpenChanges(t.Context(), "", 10, statelog.ReadStale); len(open) != 0 {
		t.Errorf("settled pull requests still read open: %v", open)
	}
	if _, err := r.writer.SettleChange(t.Context(), "op-stray", r.task(t, "w").Task, worklet.ChangeSettlement{
		Ref: other, State: worklet.ChangeMerged, At: time.Now()}); !errors.Is(err, worklet.ErrInvalid) {
		t.Errorf("a settle of a change the task does not wait on: %v, want it refused", err)
	}
}
