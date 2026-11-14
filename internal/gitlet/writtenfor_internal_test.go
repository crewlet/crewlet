package gitlet

import (
	"database/sql"
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
)

// A STORED CARRIER THIS BUILD CANNOT READ IS A STRANGER'S (ADR-0109): what a
// working_for column holds is read back by the engine's later records
// (storedCarrier) — NULL as nobody, a carrier as it was kept, and a value
// that does not decode, or decodes to nothing (JSON's null, a fragment, a
// list), as a stranger's, who holds no app: the closed end, never nobody,
// which would wake the turn for the acting seat with every app. A kind this
// build does not know is no value it cannot read: it decodes, and is read as
// the row holds it, for the one rule to judge wherever a holder meets it
// (KindUnknownToThisBuild) and for a node of the build that knows it to
// compose — never rewritten to a stranger's here, which would decide for that
// build what only it can.
//
// Mutations: an unreadable value read as nobody — null, the fragment and the
// list read nil, red; a kind this build does not know rewritten to a
// stranger's — the unit's carrier reads a stranger's, red.
func TestAStoredCarrierThisBuildCannotReadIsAStrangers(t *testing.T) {
	t.Parallel()
	stranger := &types.WorkingFor{Kind: types.WorkingForStranger}
	newer := &types.WorkingFor{Kind: "unit", Seat: "platform"}
	if !KindUnknownToThisBuild(newer) {
		t.Fatalf("the case's newer carrier %+v names a kind this build knows, and the case is about one it does "+
			"not", newer)
	}
	for _, tc := range []struct {
		name   string
		column sql.NullString
		want   *types.WorkingFor
	}{
		{"NULL", sql.NullString{}, nil},
		{"a seat's carrier", sql.NullString{String: `{"kind":"seat","seat":"founder"}`, Valid: true},
			&types.WorkingFor{Kind: types.WorkingForSeat, Seat: "founder"}},
		{"JSON's null", sql.NullString{String: "null", Valid: true}, stranger},
		{"a fragment", sql.NullString{String: "{", Valid: true}, stranger},
		{"a list", sql.NullString{String: "[]", Valid: true}, stranger},
		{"a kind this build does not know", sql.NullString{String: `{"kind":"unit","seat":"platform"}`,
			Valid: true}, newer},
	} {
		if got := storedCarrier(tc.column); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s stored reads %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// A ROUTING BESIDE A PERSON'S ACT CARRIES NOBODY (ADR-0109): routingFor stamps
// whom a change was made for beside an agent's actor alone, and only where the
// routing wakes a seat — a person writes for nobody but themselves, and a
// routing that tells only persons wakes no turn to carry it to. Handed
// founder's seat as the carrier, a routing that tells reader, an agent,
// carries it beside dev's act and nothing beside founder's own act or beside
// no actor at all; and one that tells founder alone carries nothing beside
// dev's act either. THE ENGINE WRITES FOR NOBODY BUT ITSELF, save its one
// exception: handed founder as whom an entry's enqueue was made for — what the
// merge queue's ejection and a close it could not make hand it, and nothing
// else does — a routing beside no actor that tells reader carries founder,
// one that tells founder alone carries nothing, and one beside dev's act
// carries nothing of it either.
//
// Mutations: stamp a person actor's routing — founder's carries the carrier,
// red; stamp every routing whatever it wakes — the one telling founder alone
// carries it, red; never stamp — dev's carries nothing, red; stamp an
// actor's carrier beside no actor — the routing beside no actor carries the
// carrier dev's act was handed, red; never stamp an enqueue's beside no actor
// — the queue's record carries nobody, red; stamp an enqueue's beside an
// actor — dev's act carries what it never asked, red.
func TestARoutingBesideAPersonsActCarriesNobody(t *testing.T) {
	t.Parallel()
	founder := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "founder"}
	row := changeRow{id: "0192f6c1-0000-7000-8000-0000000000c1", repoID: "repo-a", number: 7,
		title: "Retry the upload", owner: "eng-lead", ownerKind: PartyAgent}
	for _, tc := range []struct {
		name     string
		actor    string
		kind     PartyKind
		mentions []string
		// workingFor is the carrier handed beside the actor, and
		// enqueuedFor the one handed as whom an entry's enqueue was made
		// for.
		workingFor, enqueuedFor *types.WorkingFor
		want                    *types.WorkingFor
	}{
		{"beside dev's act, telling reader", "dev", PartyAgent, []string{"reader"}, founder, nil, founder},
		{"beside founder's own act, telling reader", "founder", PartyPerson, []string{"reader"}, founder, nil,
			nil},
		{"beside no actor, telling reader", "", "", []string{"reader"}, founder, nil, nil},
		{"beside dev's act, telling founder alone", "dev", PartyAgent, []string{"founder"}, founder, nil, nil},
		{"an enqueue's, beside no actor, telling reader", "", "", []string{"reader"}, nil, founder, founder},
		{"an enqueue's, beside no actor, telling founder alone", "", "", []string{"founder"}, nil, founder,
			nil},
		{"an enqueue's, beside dev's act, telling reader", "dev", PartyAgent, []string{"reader"}, nil, founder,
			nil},
	} {
		r, err := routingFor(t.Context(), nil, concerning{repoName: "web", change: row, actor: tc.actor,
			actorKind: tc.kind, workingFor: tc.workingFor, enqueuedFor: tc.enqueuedFor, roster: seatChart{},
			mentions: tc.mentions})
		switch {
		case err != nil:
			t.Fatalf("%s: %v", tc.name, err)
		case r == nil:
			t.Fatalf("%s: the routing tells nobody, and the case is about whom it carries", tc.name)
		case !reflect.DeepEqual(r.WorkingFor, tc.want):
			t.Errorf("the routing %s carries %+v, want %+v", tc.name, r.WorkingFor, tc.want)
		}
	}
}
