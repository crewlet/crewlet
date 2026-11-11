package worklet

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// A HISTORY CARRIER THIS BUILD CANNOT READ IS A STRANGER'S. An agent's row
// keeps whom its write was made for as JSON, and the pickup works for whom it
// names. A value that does not decode, or decodes to nothing, names nobody
// this build can place, so its pickup is held to a stranger's ceiling — no
// app — rather than to the writing agent's every app. A row with no carrier
// reads as its writer, as before.
//
// Mutation: read an undecodable carrier as the writer, or as nil, and a row
// goes red.
func TestAHistoryCarrierThisBuildCannotReadIsAStrangers(t *testing.T) {
	t.Parallel()
	stranger := &types.WorkingFor{Kind: types.WorkingForStranger}
	for _, raw := range []string{"null", "{", "[]"} {
		got := whomFor(AuthorAgent, "b", "", sql.NullString{String: raw, Valid: true}, "c")
		if !reflect.DeepEqual(got, stranger) {
			t.Errorf("an agent's row whose carrier is %q reads as %+v, want a stranger's", raw, got)
		}
	}
	got := whomFor(AuthorAgent, "b", "", sql.NullString{}, "c")
	if want := (&types.WorkingFor{Kind: types.WorkingForSeat, Seat: "b"}); !reflect.DeepEqual(got, want) {
		t.Errorf("an agent's row with no carrier reads as %+v, want its writer %+v", got, want)
	}
}

// ONLY AN AGENT'S ROW IS READ FOR WHOM IT WAS WRITTEN FOR. A person, a
// credential and the engine write for nobody but themselves, so a carrier on
// one of their rows — or on a row of a kind this build does not know — is never
// believed: the row reads as its writer. And a seat carrier naming the reading
// seat is the seat's own work.
//
// Mutation: believe a non-agent row's carrier, and a row goes red.
func TestOnlyAnAgentsRowIsReadForWhomItWasWrittenFor(t *testing.T) {
	t.Parallel()
	carried := sql.NullString{String: `{"kind":"seat","seat":"r"}`, Valid: true}
	cases := []struct {
		name string
		kind AuthorKind
		want *types.WorkingFor
	}{
		{"a person's row", AuthorHuman, &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "w"}},
		{"a credential's row", AuthorOperator, &types.WorkingFor{Kind: types.WorkingForCredential, Login: "w"}},
		{"a row of a kind this build does not know", AuthorKind("robot"),
			&types.WorkingFor{Kind: types.WorkingForSeat, Seat: "w"}},
		{"an agent's row", AuthorAgent, &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "r"}},
	}
	for _, tc := range cases {
		if got := whomFor(tc.kind, "w", "", carried, "c"); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s carrying seat r reads as %+v, want %+v", tc.name, got, tc.want)
		}
	}
	if got := whomFor(AuthorAgent, "b", "", sql.NullString{String: `{"kind":"seat","seat":" c "}`,
		Valid: true}, "c"); got != nil {
		t.Errorf("an agent's row written for the reading seat reads as %+v, want nil: the seat's own work", got)
	}
}

// THE SEAT'S OWN CREATION COUNTS ONLY AS AN AGENT'S ROW THAT CARRIES WHOM ITS
// TURN WORKED FOR, which is the rule whomFor believes a carrier by. Seeded rows
// rather than written ones, because no writer of this build lets a person's
// row carry anybody: the arm must still keep out the seat's own row of another
// kind, a newer build's or a damaged one.
//
// Mutation: admit the seat's own carried creation whatever its kind, and the
// person's row is read.
func TestASeatsOwnRowIsAdmittedOnlyAsAnAgentsRowForSomebody(t *testing.T) {
	t.Parallel()
	db, estate := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	w, err := estate.Writer(t.Context())
	if err != nil {
		t.Fatalf("take the writer: %v", err)
	}
	defer w.Close()
	at := store.EncodeTime(time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC))
	var got map[string]*types.WorkingFor
	if err := w.Tx(t.Context(), func(tx *sql.Tx) error {
		for i, row := range []struct{ subject, kind string }{
			{"t-person", string(AuthorHuman)},
			{"t-agent", string(AuthorAgent)},
		} {
			if _, err := tx.ExecContext(t.Context(), `
				INSERT INTO tracker_history
					(id, subject_kind, subject_id, project_key, kind, actor, actor_kind,
					 fields_json, log_seq, log_stream, created_at, effective_at, document,
					 working_for)
				VALUES (?, 'task', ?, 'ENG', 'created', 'c', ?, '{}', ?, ?, ?, ?, '{}', ?)`,
				"h-"+row.subject, row.subject, row.kind, int64(i+1), Domain{}.Stream().Name, at, at,
				`{"kind":"seat","seat":"r"}`); err != nil {
				return err
			}
		}
		got, err = writersOf(t.Context(), tx, arms(2, readyCreatedArm),
			[]any{"t-person", "c", "t-agent", "c"}, "c")
		return err
	}); err != nil {
		t.Fatalf("seed and read the creations: %v", err)
	}
	want := map[string]*types.WorkingFor{"t-agent": {Kind: types.WorkingForSeat, Seat: "r"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the seat's own creations read as %+v, want only the agent's row carried for r (%+v)",
			got, want)
	}
}
