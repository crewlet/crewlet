package worklet_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// typedProbe writes, as the agent seat ana in turn, one change to id.
type typedProbe func(t *testing.T, r *roundTrip, turn, id string)

func probeUpdate(patch worklet.TaskPatch, kind worklet.ChangeKind) typedProbe {
	return func(t *testing.T, r *roundTrip, turn, id string) {
		t.Helper()
		ana := r.writer.As("ana", worklet.AuthorAgent, worklet.Provenance{TurnID: turn})
		if _, err := ana.UpdateTask(t.Context(), "op-"+turn, id, "ENG", worklet.NoIfMatch, patch,
			kind, nil); err != nil {
			t.Fatalf("%s writes %s: %v", turn, id, err)
		}
		r.drain()
	}
}

func probeComment(ask string) typedProbe {
	return func(t *testing.T, r *roundTrip, turn, id string) {
		t.Helper()
		c := worklet.Comment{ID: "c-" + turn, Task: id, Author: "ana", AuthorKind: worklet.AuthorAgent,
			Body: "still waits on the vendor", Ask: ask, CreatedAt: wednesday}
		probeUpdate(worklet.TaskPatch{Comment: &c}, worklet.ChangeComment)(t, r, turn, id)
	}
}

func typedAnswer(t *testing.T, r *roundTrip, turn, id string) bool {
	t.Helper()
	typed, err := r.reader.TypedAnswer(t.Context(), worklet.TypedAnswerQuery{Turn: turn, Item: id,
		Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("TypedAnswer(%s, %s): %v", turn, id, err)
	}
	return typed
}

// A TURN'S TYPED ANSWER IS A CHANGE ON THE ITEM OR AN ASK, NEVER WORDS ALONE
// (ADR-0122): read off the turn's own history rows on the item, a field, its
// status, its assignee or a question put with `ask` answers it, and a comment
// that asks nothing, a change of who watches, a change to another item and
// another turn's change do not.
//
// Mutation: drop the comment arm from typedWrite, and the ask reads as words.
func TestATurnsTypedAnswerIsAChangeOnTheItemOrAnAsk(t *testing.T) {
	t.Parallel()
	urgent, started, holder := worklet.PriorityUrgent, worklet.StatusInProgress, "pm"
	until, reason := wednesday.Add(72*time.Hour), "the vendor's window opens then"
	for _, row := range []struct {
		name  string
		write typedProbe
		typed bool
	}{
		{"a comment that asks nothing", probeComment(""), false},
		{"a comment that asks somebody", probeComment("pm"), true},
		{"a field change", probeUpdate(worklet.TaskPatch{Priority: &urgent}, worklet.ChangeFields), true},
		{"a status change", probeUpdate(worklet.TaskPatch{Status: &started}, worklet.ChangeStatus), true},
		{"a reassignment", probeUpdate(worklet.TaskPatch{Assignee: &holder}, worklet.ChangeAssignee), true},
		{"a wait until a date", probeUpdate(worklet.TaskPatch{StartAt: &until, WaitReason: &reason},
			worklet.ChangeFields), true},
		{"a watcher added", probeUpdate(worklet.TaskPatch{
			Watch: &worklet.WatchIntent{Handle: "designer", Watch: true}}, worklet.ChangeWatchers), false},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			r := newRoundTrip(t)
			fileFor(t, r, "w-1", "ana", worklet.StatusTodo, worklet.PriorityNormal)
			fileFor(t, r, "w-2", "ana", worklet.StatusTodo, worklet.PriorityNormal)
			row.write(t, r, "turn-1", "w-1")
			if got := typedAnswer(t, r, "turn-1", "w-1"); got != row.typed {
				t.Errorf("%s answered the item typed: %v, want %v", row.name, got, row.typed)
			}
			if typedAnswer(t, r, "turn-1", "w-2") {
				t.Errorf("%s on w-1 answered w-2", row.name)
			}
			if typedAnswer(t, r, "turn-2", "w-1") {
				t.Errorf("%s by turn-1 answered for turn-2", row.name)
			}
		})
	}
}

// AN ITEM GOES QUIET ON THE SEAT'S LAST WORD UNLESS THAT WORD IS TYPED
// (ADR-0122): with nothing from the seat it is quiet since the change that last
// woke it; a comment that asks nothing moves the instant on; a typed answer
// holds it, so it carries none; words after a typed answer make it quiet
// again; and a change of who watches is no word.
//
// Mutation: read the seat's last write without its typed flag, and a typed
// answer leaves the item quiet, so the pickup would hand it back a day later.
func TestAnItemGoesQuietOnTheSeatsLastWordUnlessItIsTyped(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	fileFor(t, r, "w-1", "ana", worklet.StatusTodo, worklet.PriorityNormal)
	ready := func() worklet.ReadyItem {
		t.Helper()
		items := r.readyWork("ana").Items
		if len(items) != 1 {
			t.Fatalf("ana's ready work is %v, want w-1 alone", readyIDs(r.readyWork("ana")))
		}
		return items[0]
	}
	filed := ready()
	if filed.Quiet.IsZero() || !filed.Quiet.Equal(filed.MovedAt) {
		t.Fatalf("an item the seat has said nothing on is quiet since %s, want %s, the change that woke it",
			filed.Quiet, filed.MovedAt)
	}

	probeComment("")(t, r, "turn-1", "w-1")
	said := ready()
	if said.Quiet.IsZero() || said.Quiet.Before(filed.Quiet) {
		t.Fatalf("after the seat's comment the item is quiet since %s, want the comment's instant, "+
			"not before %s", said.Quiet, filed.Quiet)
	}

	urgent := worklet.PriorityUrgent
	probeUpdate(worklet.TaskPatch{Priority: &urgent}, worklet.ChangeFields)(t, r, "turn-2", "w-1")
	if typed := ready(); !typed.Quiet.IsZero() {
		t.Errorf("after the seat's typed answer the item is quiet since %s, want it held", typed.Quiet)
	}

	probeUpdate(worklet.TaskPatch{Watch: &worklet.WatchIntent{Handle: "designer", Watch: true}},
		worklet.ChangeWatchers)(t, r, "turn-3", "w-1")
	if watched := ready(); !watched.Quiet.IsZero() {
		t.Errorf("a watcher the seat added counted as its word: quiet since %s", watched.Quiet)
	}

	probeComment("")(t, r, "turn-4", "w-1")
	if again := ready(); again.Quiet.IsZero() || again.Quiet.Before(said.Quiet) {
		t.Errorf("words after a typed answer leave the item quiet since %s, want it quiet again "+
			"from the words", again.Quiet)
	}
}
