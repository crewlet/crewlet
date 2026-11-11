package worklet_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// typedProbe writes, as the agent seat ana in turn, one change to id.
type typedProbe func(t *testing.T, r *roundTrip, turn, id string)

func probeUpdate(patch worklet.TaskPatch, kind worklet.ChangeKind) typedProbe {
	return func(t *testing.T, r *roundTrip, turn, id string) {
		t.Helper()
		provenance := aTurnNobodyWoke()
		provenance.TurnID = turn
		ana := r.writer.As("ana", worklet.AuthorAgent, provenance)
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
			Body: "still waits on the vendor", Ask: ask, CreatedAt: r.at}
		probeUpdate(worklet.TaskPatch{Comment: &c}, worklet.ChangeComment)(t, r, turn, id)
	}
}

// probeBy writes patch on id as another party, outside any turn, under op.
func probeBy(t *testing.T, r *roundTrip, op, who string, kind worklet.AuthorKind, id string,
	patch worklet.TaskPatch, change worklet.ChangeKind) {
	t.Helper()
	provenance := worklet.Provenance{}
	if kind == worklet.AuthorAgent {
		provenance = aTurnNobodyWoke()
	}
	if _, err := r.writer.As(who, kind, provenance).UpdateTask(t.Context(), op,
		id, "ENG", worklet.NoIfMatch, patch, change, nil); err != nil {
		t.Fatalf("%s writes %s: %v", who, id, err)
	}
	r.drain()
}

// pickupAnswer reads how seat left id since since.
func pickupAnswer(t *testing.T, r *roundTrip, seat, id string, since time.Time) types.PickupAnswer {
	t.Helper()
	answer, err := r.reader.PickupAnswer(t.Context(), worklet.PickupAnswerQuery{Seat: seat, Item: id,
		Since: since, Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("PickupAnswer(%s, %s): %v", seat, id, err)
	}
	return answer
}

// A SEAT'S TYPED ANSWER IS A CHANGE ON THE ITEM OR AN ASK, NEVER WORDS ALONE
// (ADR-0122): read off the seat's own history rows on the item since its turn
// began, a field, its status, its assignee, a wait, its reason restated or a
// question put with `ask` answers it, whichever turn or none wrote it (a merge
// queue's close is the seat's with no turn); a comment that asks nothing, a
// change of who watches, a change to another item, another seat's change and
// the seat's own change from before the turn began do not.
//
// Mutation: drop the comment arm from typedWrite, and the ask reads as words;
// read the turn's id rather than the seat's rows, and a close made outside the
// turn answers nothing.
func TestASeatsTypedAnswerIsAChangeOnTheItemOrAnAsk(t *testing.T) {
	t.Parallel()
	urgent, started, holder, held := worklet.PriorityUrgent, worklet.StatusInProgress, "pm", "ana"
	until, reason, other := wednesday.Add(72*time.Hour), "the vendor's window opens then",
		"the release is cut then"
	for _, row := range []struct {
		name  string
		turn  string
		write typedProbe
		typed bool
	}{
		{"a comment that asks nothing", "turn-1", probeComment(""), false},
		{"a comment that asks somebody", "turn-1", probeComment("pm"), true},
		{"a field change", "turn-1", probeUpdate(worklet.TaskPatch{Priority: &urgent}, worklet.ChangeFields), true},
		{"a status change", "turn-1", probeUpdate(worklet.TaskPatch{Status: &started}, worklet.ChangeStatus), true},
		{"a status change outside any turn", "", probeUpdate(worklet.TaskPatch{Status: &started},
			worklet.ChangeStatus), true},
		{"a reassignment", "turn-1", probeUpdate(worklet.TaskPatch{Assignee: &holder}, worklet.ChangeAssignee), true},
		{"a wait until a date", "turn-1", probeUpdate(worklet.TaskPatch{StartAt: &until, WaitReason: &reason},
			worklet.ChangeFields), true},
		{"a watcher added", "turn-1", probeUpdate(worklet.TaskPatch{
			Watch: &worklet.WatchIntent{Handle: "designer", Watch: true}}, worklet.ChangeWatchers), false},
		// A WATCH WRITTEN BESIDE A CHANGE IS THAT CHANGE (#953), whatever
		// kind its row names, and a restatement beside a watch moved nothing
		// but who watches.
		{"a watcher added beside a priority", "turn-1", probeUpdate(worklet.TaskPatch{
			Watch:    &worklet.WatchIntent{Handle: "designer", Watch: true},
			Priority: &urgent}, worklet.ChangeWatchers), true},
		{"an assignment restated beside a watcher", "turn-1", probeUpdate(worklet.TaskPatch{
			Assignee: &held, Watch: &worklet.WatchIntent{Handle: "designer", Watch: true}},
			worklet.ChangeAssignee), false},
		// A REASON RESTATED ON THE SAME DATE moves the wait, and says so.
		{"a reason restated on the same date", "turn-1", func(t *testing.T, r *roundTrip, turn, id string) {
			probeBy(t, r, "op-pm", "pm", worklet.AuthorHuman, id, worklet.TaskPatch{StartAt: &until, WaitReason: &reason},
				worklet.ChangeFields)
			r.at = r.at.Add(time.Minute)
			probeUpdate(worklet.TaskPatch{StartAt: &until, WaitReason: &other}, worklet.ChangeFields)(t, r, turn, id)
		}, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			r := newRoundTrip(t)
			fileFor(t, r, "w-1", "ana", worklet.StatusTodo, worklet.PriorityNormal)
			fileFor(t, r, "w-2", "ana", worklet.StatusTodo, worklet.PriorityNormal)
			began := r.at.Add(time.Second)
			r.at = began
			row.write(t, r, row.turn, "w-1")
			want := types.PickupUnanswered
			if row.typed {
				want = types.PickupTyped
			}
			if got := pickupAnswer(t, r, "ana", "w-1", began); got != want {
				t.Errorf("%s answered the item %q, want %q", row.name, got, want)
			}
			if got := pickupAnswer(t, r, "ana", "w-2", began); got != types.PickupUnanswered {
				t.Errorf("%s on w-1 answered w-2: %q", row.name, got)
			}
			if got := pickupAnswer(t, r, "ana", "w-1", r.at.Add(time.Second)); got == types.PickupTyped {
				t.Errorf("%s made before the turn began answered it", row.name)
			}
		})
	}
	// ANOTHER SEAT'S CHANGE IS NOT THE SEAT'S ANSWER.
	r := newRoundTrip(t)
	fileFor(t, r, "w-1", "ana", worklet.StatusTodo, worklet.PriorityNormal)
	probeBy(t, r, "op-pm", "pm", worklet.AuthorHuman, "w-1", worklet.TaskPatch{Priority: &urgent}, worklet.ChangeFields)
	if got := pickupAnswer(t, r, "ana", "w-1", wednesday); got != types.PickupUnanswered {
		t.Errorf("a colleague's priority answered the seat's pickup: %q", got)
	}
}

// A PICKUP IS MOOT ONCE ITS ITEM LEAVES THE SEAT (ADR-0122): with nothing typed
// from the seat, an item somebody else put in the trash, finished or handed to
// another seat or to nobody owes it nothing, and the read says which; an item
// still the seat's is unanswered; and the seat's own typed answer reads typed
// whatever it did to the item.
//
// Mutation: answer unanswered whenever nothing typed is on the item, and a turn
// whose item was trashed under it can never end.
func TestAPickupIsMootOnceItsItemLeavesTheSeat(t *testing.T) {
	t.Parallel()
	done, nobody, other := worklet.StatusDone, "", "bob"
	for _, row := range []struct {
		name  string
		leave func(t *testing.T, r *roundTrip)
		want  types.PickupAnswer
	}{
		{"nothing", func(*testing.T, *roundTrip) {}, types.PickupUnanswered},
		{"put in the trash", func(t *testing.T, r *roundTrip) {
			pm := r.writer.As("pm", worklet.AuthorHuman, worklet.Provenance{})
			if _, err := pm.RemoveTask(t.Context(), "op-trash", "w-1", "ENG", false, nil); err != nil {
				t.Fatalf("RemoveTask: %v", err)
			}
			r.drain()
		}, types.PickupRemoved},
		{"finished", func(t *testing.T, r *roundTrip) {
			probeBy(t, r, "op-pm", "pm", worklet.AuthorHuman, "w-1", worklet.TaskPatch{Status: &done}, worklet.ChangeStatus)
		}, types.PickupFinished},
		{"handed to another seat", func(t *testing.T, r *roundTrip) {
			probeBy(t, r, "op-pm", "pm", worklet.AuthorHuman, "w-1", worklet.TaskPatch{Assignee: &other},
				worklet.ChangeAssignee)
		}, types.PickupReassigned},
		{"unassigned", func(t *testing.T, r *roundTrip) {
			probeBy(t, r, "op-pm", "pm", worklet.AuthorHuman, "w-1", worklet.TaskPatch{Assignee: &nobody},
				worklet.ChangeAssignee)
		}, types.PickupReassigned},
		{"finished by the seat itself", func(t *testing.T, r *roundTrip) {
			probeUpdate(worklet.TaskPatch{Status: &done}, worklet.ChangeStatus)(t, r, "turn-2", "w-1")
		}, types.PickupTyped},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			r := newRoundTrip(t)
			fileFor(t, r, "w-1", "ana", worklet.StatusTodo, worklet.PriorityNormal)
			began := r.at.Add(time.Second)
			r.at = began
			probeComment("")(t, r, "turn-1", "w-1")
			row.leave(t, r)
			if got := pickupAnswer(t, r, "ana", "w-1", began); got != row.want {
				t.Errorf("an item %s answered %q, want %q", row.name, got, row.want)
			}
			if got := pickupAnswer(t, r, "ana", "w-404", began); got != types.PickupRemoved {
				t.Errorf("an item no row holds answered %q, want it removed", got)
			}
		})
	}
}

// AN ITEM GOES QUIET ON THE SEAT'S LAST WORD, WHATEVER IT WAS (ADR-0122): with
// nothing from the seat it is quiet since the change that last woke it; a
// comment moves the instant to the comment's; an answer that leaves the item
// ready (a priority, an edit of its content) is a last word like any other, so
// the item comes back a day on it rather than never; a change of who watches is
// no word; and a status the seat moved is its word too, its pickup handed again
// for the new status. Every instant is the harness's stated authored clock,
// moved write by write.
//
// Mutation: hold the item where the seat's last write is typed, and a priority
// set on a todo item leaves it in the seat's ready work, handed never again;
// read a change of who watches by its kind, and the priority set beside a
// watch reads as no word.
func TestAnItemGoesQuietOnTheSeatsLastWord(t *testing.T) {
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
	if filed := ready(); !filed.Quiet.Equal(wednesday) || !filed.Quiet.Equal(filed.MovedAt) {
		t.Fatalf("an item the seat has said nothing on is quiet since %s, want %s, the change "+
			"that woke it", filed.Quiet, wednesday)
	}

	said := wednesday.Add(time.Hour)
	r.at = said
	probeComment("")(t, r, "turn-1", "w-1")
	if got := ready().Quiet; !got.Equal(said) {
		t.Fatalf("after the seat's comment the item is quiet since %s, want the comment's %s", got, said)
	}

	edited := wednesday.Add(2 * time.Hour)
	r.at = edited
	urgent := worklet.PriorityUrgent
	probeUpdate(worklet.TaskPatch{Priority: &urgent}, worklet.ChangeFields)(t, r, "turn-2", "w-1")
	if got := ready().Quiet; !got.Equal(edited) {
		t.Errorf("after a priority that left the item ready it is quiet since %s, want the "+
			"priority's %s", got, edited)
	}

	r.at = wednesday.Add(3 * time.Hour)
	probeUpdate(worklet.TaskPatch{Watch: &worklet.WatchIntent{Handle: "designer", Watch: true}},
		worklet.ChangeWatchers)(t, r, "turn-3", "w-1")
	if got := ready().Quiet; !got.Equal(edited) {
		t.Errorf("a watcher the seat added counted as its word: quiet since %s", got)
	}

	beside := wednesday.Add(4 * time.Hour)
	r.at = beside
	high := worklet.PriorityHigh
	probeUpdate(worklet.TaskPatch{Priority: &high,
		Watch: &worklet.WatchIntent{Handle: "qa", Watch: true}}, worklet.ChangeWatchers)(t, r, "turn-4", "w-1")
	if got := ready().Quiet; !got.Equal(beside) {
		t.Errorf("a priority the seat set beside a watch left the item quiet since %s, want %s", got, beside)
	}

	moved := wednesday.Add(5 * time.Hour)
	r.at = moved
	started := worklet.StatusInProgress
	probeUpdate(worklet.TaskPatch{Status: &started}, worklet.ChangeStatus)(t, r, "turn-5", "w-1")
	if got := ready(); got.Status != started || !got.Quiet.Equal(moved) {
		t.Errorf("after the seat started the item it is %s, quiet since %s, want it started and "+
			"quiet since %s", got.Status, got.Quiet, moved)
	}
}
