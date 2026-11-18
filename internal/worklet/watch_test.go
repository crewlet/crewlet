package worklet_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// oneTask reads one task back through the reader.
func oneTask(t *testing.T, r *roundTrip, id string) worklet.Task {
	t.Helper()
	detail, err := r.reader.Task(t.Context(), id, worklet.DetailWants{},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return detail.Task
}

// A WATCH GESTURE ADDS ONE PERSON AND REMOVES NOBODY. A patch carries
// collections WHOLE, so `watchers: [me]` would drop everyone else; the writer
// resolves the gesture inside its own snapshot, the one consistent read of the
// set.
func TestAWatchGestureAddsOnePersonAndRemovesNobody(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	task := newTask("t-watch")
	task.Watchers = []string{"alice", "bob"}
	if _, err := r.writer.CreateTask(t.Context(), "op-create", task, nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	if _, err := r.writer.UpdateTask(t.Context(), "op-watch", "t-watch", "ENG", 0,
		worklet.TaskPatch{Watch: &worklet.WatchIntent{Handle: "carol", Watch: true}}, worklet.ChangeFields,
		nil); err != nil {
		t.Fatalf("watch: %v", err)
	}
	r.drain()

	after := oneTask(t, r, "t-watch")
	if got := after.Watchers; len(got) != 3 ||
		!slices.Contains(got, "alice") || !slices.Contains(got, "bob") ||
		!slices.Contains(got, "carol") {
		t.Fatalf("after carol watched, the watchers are %v — want alice, bob "+
			"and carol: a gesture about one person must not rewrite the set", got)
	}
	if len(after.Muted) != 0 {
		t.Fatalf("carol watching muted %v", after.Muted)
	}

	// AND UN-WATCHING MUTES THE ONE PERSON rather than clearing the set:
	// "never watched" and "un-watched" are what the two fields keep apart.
	if _, err := r.writer.UpdateTask(t.Context(), "op-unwatch", "t-watch", "ENG", 0,
		worklet.TaskPatch{Watch: &worklet.WatchIntent{Handle: "bob", Watch: false}}, worklet.ChangeFields,
		nil); err != nil {
		t.Fatalf("unwatch: %v", err)
	}
	r.drain()

	quiet := oneTask(t, r, "t-watch")
	if slices.Contains(quiet.Watchers, "bob") {
		t.Fatalf("bob is still in %v after un-watching", quiet.Watchers)
	}
	if !slices.Contains(quiet.Muted, "bob") {
		t.Fatalf("bob un-watched and the muted set is %v — a person who "+
			"un-watched must be distinguishable from one who never watched, "+
			"or the next mention silently re-adds them", quiet.Muted)
	}
	if !slices.Contains(quiet.Watchers, "alice") ||
		!slices.Contains(quiet.Watchers, "carol") {
		t.Fatalf("bob un-watching left the watchers as %v — it removed people "+
			"who said nothing", quiet.Watchers)
	}
}

// AND THE TWO SPELLINGS ARE REFUSED TOGETHER, never resolved in some order:
// one is a gesture about one person and the other is the whole set, so
// whichever won would silently discard the other.
func TestAPatchCarryingBothAWatchGestureAndASetIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.CreateTask(t.Context(), "op-create",
		newTask("t-both"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	_, err := r.writer.UpdateTask(t.Context(), "op-both", "t-both", "ENG", 0,
		worklet.TaskPatch{
			Watch:    &worklet.WatchIntent{Handle: "carol", Watch: true},
			Watchers: &[]string{"dave"},
		}, worklet.ChangeWatchers, nil)
	if err == nil {
		t.Fatal("a patch carrying both a watch gesture and a whole watcher " +
			"set was accepted, so one of them was silently discarded")
	}
}

// AND THE ROUTING CAP IS ENFORCED WHERE THE SET GROWS BY ONE: the gesture is
// the one place a watcher set grows singly, so it refuses past MaxWatchers,
// beyond which a comment on the item wakes everybody.
func TestAWatchGestureRefusesPastTheRoutingCap(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	full := newTask("t-full")
	for i := range worklet.MaxWatchers {
		full.Watchers = append(full.Watchers, "w"+itoa(i))
	}
	if _, err := r.writer.CreateTask(t.Context(), "op-create", full, nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	_, err := r.writer.UpdateTask(t.Context(), "op-over", "t-full", "ENG", 0,
		worklet.TaskPatch{Watch: &worklet.WatchIntent{Handle: "one-more", Watch: true}}, worklet.ChangeFields,
		nil)
	if err == nil {
		t.Fatalf("a %dth watcher was accepted against a cap of %d",
			worklet.MaxWatchers+1, worklet.MaxWatchers)
	}

	// AND UN-WATCHING IS NEVER REFUSED BY IT: leaving a set that is too
	// large is the one move that makes it smaller.
	if _, err := r.writer.UpdateTask(t.Context(), "op-leave", "t-full", "ENG", 0,
		worklet.TaskPatch{Watch: &worklet.WatchIntent{Handle: "w0", Watch: false}}, worklet.ChangeFields,
		nil); err != nil {
		t.Fatalf("un-watching a full item was refused: %v", err)
	}
}
