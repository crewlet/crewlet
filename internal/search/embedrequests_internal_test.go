package search

import (
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/embeddings"
)

// A HALF THE TICK HAS NO ROOM FOR STAYS A SUSPECT.
//
// A split half is part of an isolation: its sources are suspected of holding
// the input the provider refuses. Sent at the room the tick had left, the part
// that did not fit used to be dropped from the split, and the next tick sent it
// with ordinary neighbours — where, if it held the refused input, it was
// refused beside them and the isolation began again from a whole request. It
// stays at the front of the split instead, so the tick's end suspends it with
// the rest of the isolation.
func TestAHalfTheTickHasNoRoomForStaysASuspect(t *testing.T) {
	t.Parallel()
	var half []pending
	for i := range 8 {
		half = append(half, pending{doc: Document{ID: fmt.Sprintf("s%d", i)},
			text: fmt.Sprintf("source %d", i)})
	}
	later := []pending{{doc: Document{ID: "later"}, text: "a later half"}}
	q := &corpusQueue{split: [][]pending{half, later}}
	limits := embeddings.Limits{InputBytes: 8192, BatchInputs: 128, BatchBytes: 300_000}

	group, err := q.next(limits, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(group) != 3 || group[0].doc.ID != "s0" {
		t.Fatalf("sent %d source(s) from %q, want the first three of the half", len(group), group[0].doc.ID)
	}
	if len(q.split) != 2 || len(q.split[0]) != 5 || q.split[0][0].doc.ID != "s3" ||
		q.split[1][0].doc.ID != "later" {
		t.Fatalf("the split after a short send is %v, want the half's other five "+
			"first and the later half behind them", q.split)
	}
}

// AN ISOLATION RESUMES AS IT WAS SUSPENDED, less what has moved since.
//
// The groups are rebuilt from what this tick's selection returned, in the order
// they were suspended and ahead of everything else, and a member whose text
// changed — another digest — is a new input and goes back among its neighbours.
func TestAnIsolationResumesAsItWasSuspended(t *testing.T) {
	t.Parallel()
	r := NewRefusals()
	at := func(id, text string) pending {
		return pending{doc: Document{ID: id}, text: text, sha: digestText(text)}
	}
	r.suspend(SourceTask, [][]pending{
		{at("a", "alpha"), at("b", "bravo")},
		{at("c", "charlie")},
		{at("gone", "removed since")},
	})
	q := &corpusQueue{source: SourceTask, waiting: []pending{
		at("x", "an ordinary source"), at("c", "charlie"),
		at("b", "bravo rewritten"), at("a", "alpha"),
	}}
	q.resume(r.resume(SourceTask))

	if len(q.split) != 2 || len(q.split[0]) != 1 || q.split[0][0].doc.ID != "a" ||
		len(q.split[1]) != 1 || q.split[1][0].doc.ID != "c" {
		t.Fatalf("resumed %v, want [a] then [c] — b was rewritten and the gone "+
			"source is not selected", q.split)
	}
	if len(q.waiting) != 2 || q.waiting[0].doc.ID != "x" || q.waiting[1].doc.ID != "b" {
		t.Fatalf("left waiting %v, want x and the rewritten b in their order", q.waiting)
	}

	// AND A TICK THAT ENDS WITH NOTHING SPLIT KEEPS NOTHING.
	r.suspend(SourceTask, nil)
	if got := r.resume(SourceTask); len(got) != 0 {
		t.Fatalf("a finished isolation left %v to resume", got)
	}
}
