package gitlet

import (
	"database/sql"
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
)

// A STORED CARRIER THIS BUILD CANNOT READ IS A STRANGER'S (ADR-0109):
// storedCarrier reads NULL as nobody, a carrier as kept, and a value that does
// not decode or decodes to nothing (JSON's null, a fragment, a list) as a
// stranger's, who holds no app — never nobody, which would wake the turn for
// the acting seat with every app. A kind this build does not know decodes and
// is read as held, for KindUnknownToThisBuild to judge and a build that knows
// it to compose; rewriting it to a stranger's would decide for that build.
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
// the carrier beside an agent's actor alone, and only where the routing wakes
// a seat — a person writes for nobody but themselves, and a routing telling
// only persons wakes no turn. The engine writes for nobody, save an entry's
// enqueue carrier, which only the queue's ejection and a close it could not
// make hand it, stamped beside no actor where the routing wakes a seat, never
// beside one.
//
// Mutations, each red: stamp a person actor's routing (founder's act); stamp
// every routing (telling founder alone); never stamp (dev's act); stamp an
// actor's carrier beside no actor; never stamp an enqueue's beside no actor;
// stamp an enqueue's beside an actor (dev's act).
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
		// workingFor is handed beside the actor, enqueuedFor as an entry's
		// enqueue carrier.
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
