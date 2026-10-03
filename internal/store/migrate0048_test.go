package store_test

import (
	"database/sql"
	"slices"
	"testing"
)

// A HISTORY ROW NAMES ITS AUTHOR ONCE, and 0048 is what takes the second copy
// away.
//
// 0026 added `tracker_history.actor_seat` for the operator-id binding, which
// this build does not have: a person bound to a seat is recorded as the seat
// itself, kind `human`, beside the credential in `operator_id` — so the column
// would be a second author nothing writes and a reader could still take as the
// one that counts. What is asserted is the shape a fresh estate ends at, and
// that a row written in that shape reads back exactly as written: the three
// columns that carry the author survive the drop, and the drop took nothing
// else with it.
//
// Mutation: delete 0048's statement, and the column is reported.
func TestAHistoryRowCarriesNoSeatBesideItsActor(t *testing.T) {
	t.Parallel()

	db := openReplicated(t)
	columns := columnsOf(t, db, "tracker_history")
	if slices.Contains(columns, "actor_seat") {
		t.Errorf("tracker_history still carries actor_seat — the author is "+
			"`actor`, and a seat beside it is a second answer: %v", columns)
	}
	for _, kept := range []string{"actor", "actor_kind", "operator_id"} {
		if !slices.Contains(columns, kept) {
			t.Errorf("tracker_history lost %s with the drop: %v", kept, columns)
		}
	}

	type author struct{ actor, kind, operator string }
	want := author{"maya", "human", "session:0192f00d-0000-7000-8000-0000000000b0"}
	ctx := t.Context()
	if err := db.Replicated().Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO tracker_history
			(id, subject_kind, subject_id, kind, actor, actor_kind, operator_id,
			 log_seq, log_stream, created_at, document)
			VALUES ('h-1', 'task', 'task-1', 'updated', ?, ?, ?, 1, 'CREWLET_TRACKER_LOG', 1, x'7b7d')`,
			want.actor, want.kind, want.operator)
		return err
	}); err != nil {
		t.Fatalf("write a history row in the shape the estate ends at: %v", err)
	}
	var got author
	if err := db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT actor, actor_kind, operator_id
			FROM tracker_history WHERE id = 'h-1'`).Scan(&got.actor, &got.kind, &got.operator)
	}); err != nil {
		t.Fatalf("read the history row back: %v", err)
	}
	if got != want {
		t.Errorf("the history row reads back as %+v, want %+v", got, want)
	}
}
