package worklet

import (
	"fmt"
	"testing"
)

// EVERY KIND A FIELD MOVES HAS A NOUN TO BE TOLD BY, AND NO OTHER: the change
// line names a write's other moves by [movedNoun] ([changeLead]), and the
// prompt reads back only the kinds it can name ([metaMoved]), so a kind given
// a field without a noun would vanish from every line that lists a write's
// moves, and a noun for a kind that moves no field would name a move nothing
// records.
//
// Mutation: drop one noun — its kind fails; give `fields` one — it fails.
func TestEveryKindAFieldMovesHasANoun(t *testing.T) {
	t.Parallel()
	for _, kind := range ChangeKinds {
		if moves, named := kind.MovedFields() != nil, movedNoun(kind) != ""; moves != named {
			t.Errorf("%s moves a field: %v, and has a noun: %v (%q); want both or neither",
				kind, moves, named, movedNoun(kind))
		}
	}
}

// A TRIMMED CARD KEEPS THE FIELDS A ROUTE READS: the router decides a hand-off
// and the finished edge on a record's deltas ([Notify.moved]), so a card cut to
// [MaxDeltas] by field name alone, past forty fields named before them, would
// make a hand-off written with them read as none.
//
// Mutation: trim by field name alone — `assignee` and `status` are both cut.
func TestATrimmedCardKeepsTheFieldsARouteReads(t *testing.T) {
	t.Parallel()
	all := map[string]Delta{
		"assignee": {From: "ana", To: "bo"},
		"status":   {From: "todo", To: "done"},
	}
	for i := range 40 {
		all[fmt.Sprintf("a%02d", i)] = Delta{From: "x", To: "y"}
	}
	shown, omitted := cardFields(all)
	if len(shown) != MaxDeltas || omitted != len(all)-MaxDeltas {
		t.Fatalf("a card of %d moves shows %d and leaves %d off, want %d and %d",
			len(all), len(shown), omitted, MaxDeltas, len(all)-MaxDeltas)
	}
	for _, field := range routedFields {
		if _, held := shown[field]; !held {
			t.Errorf("a trimmed card drops %s, which a route reads", field)
		}
	}
}

// THE PARENT'S ASSIGNEE IS NAMED ONLY ACROSS THE FINISHED EDGE, by the write's
// own restatement ([Notify.restated]), whatever kind its record ends with: a
// status move within the open groups, or an edit that moved no status, names
// nobody for the parent, so the router, which reads the field wherever it is
// filled, tells the parent of the one transition it is told of. Each record
// here is an `assignee` one, as a hand-off that also closes the task would be
// filed by a writer that ranked the assignee first.
//
// Mutation: drop the edge from the gate — the open move and the edit name pa;
// gate it on kind `status` as well — nothing names pa.
func TestTheParentIsNamedOnlyAcrossTheFinishedEdge(t *testing.T) {
	t.Parallel()
	todo := Task{ID: "t", Key: "ENG-1", Project: "ENG",
		Status: StatusTodo, StatusGroup: GroupNotStarted}
	doing := todo
	doing.Status, doing.StatusGroup = StatusInProgress, GroupActive
	finished := todo
	finished.Status, finished.StatusGroup = StatusDone, GroupDone
	for _, c := range []struct {
		name          string
		before, after Task
		want          string
	}{
		{"into done", doing, finished, "pa"},
		{"out of done", finished, doing, "pa"},
		{"within the open groups", todo, doing, ""},
		{"no status move", doing, doing, ""},
	} {
		n := (&Notify{Kind: ChangeAssignee}).restated(c.before, c.after, nil, nil, "pa")
		if got := n.Snapshot.ParentAssignee; got != c.want {
			t.Errorf("%s names %q for the parent, want %q", c.name, got, c.want)
		}
	}
}

// A WATCH WHOSE CARD LEFT MOVES OFF IS NO WATCH ALONE: a card trimmed past
// [MaxDeltas] says by how many ([Notify.FieldsOmitted]), and what it left off
// is more than who watches, so the owners are asked as for any other change.
//
// Mutation: let watchOnly read only the deltas it shows — a trimmed card that
// shows the watchers alone reads as a watch.
func TestAWatchWhoseCardLeftMovesOffIsNoWatchAlone(t *testing.T) {
	t.Parallel()
	n := &Notify{Kind: ChangeWatchers, FieldsOmitted: 3,
		Fields: map[string]Delta{"watchers": {Added: []string{"dev"}}}}
	if n.watchOnly() {
		t.Error("a watch whose card left three moves off reads as a watch alone")
	}
}
