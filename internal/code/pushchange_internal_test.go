package code

import (
	"database/sql"
	"reflect"
	"testing"
)

// A BATCH NAMES THE CHANGE ITS EARLIER PUSH OPENED: two pushes of one batch to
// one new branch, each asking for a change. The first opens it in its own
// entry, its target the repository's default branch STAMPED there, so the
// apply reads it from the record alone; the second NAMES that change — its
// own move revises it — and its entry carries none, the change's number the
// apply's to give, so the decision answers none for it yet. Its rows are
// counted with the record's: the open's change, author and revision, the
// second's revision.
//
// Mutation: leave the target to the apply, and the first entry names none.
func TestABatchNamesTheChangeItsEarlierPushOpened(t *testing.T) {
	t.Parallel()
	e := decideEstate(t)
	first := proposalOf(cmd("refs/heads/topic", ZeroOID, oid("X")))
	first.Change = &ChangeOption{Title: "Topic"}
	second := proposalOf(cmd("refs/heads/topic", oid("X"), oid("Y")))
	second.Change = &ChangeOption{Title: "Again", Target: "refs/heads/feature"}
	var (
		entries []PushEntry
		answers []decided
		rows    int
	)
	if err := e.db.Read(t.Context(), func(tx *sql.Tx) error {
		var err error
		entries, answers, rows, _, err = decideBatch(t.Context(), tx, decideRepo, []Proposal{first, second},
			DefaultLimits(), DefaultDeletedRetention, SeatRules{}, decideAt, &verdicts{})
		return err
	}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("the batch decided %d entries, want both pushes'", len(entries))
	}
	if want := (&ChangeOption{Target: "refs/heads/main", Title: "Topic"}); !reflect.DeepEqual(entries[0].Change,
		want) {
		t.Fatalf("the first entry carries %+v, want %+v: the change it opens, its target stamped", entries[0].Change,
			want)
	}
	if entries[1].Change != nil {
		t.Fatalf("the second entry carries %+v: a push to a branch heading a live change opens none",
			entries[1].Change)
	}
	opened := PushChangeID(first.PushID)
	if want := (&ChangeAnswer{ChangeID: opened, Opened: true}); !reflect.DeepEqual(answers[0].change, want) {
		t.Fatalf("the first push's change answered %+v, want %+v", answers[0].change, want)
	}
	if want := (&ChangeAnswer{ChangeID: opened}); !reflect.DeepEqual(answers[1].change, want) {
		t.Fatalf("the second push's change answered %+v, want it to name %+v", answers[1].change, want)
	}
	if rows != 4 {
		t.Fatalf("the record writes %d rows of changes, want 4: the change, its author and two revisions", rows)
	}
}
