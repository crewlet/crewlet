package gitlet

import (
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
)

// A ROUTING BESIDE A PERSON'S ACT CARRIES NOBODY (ADR-0109): routingFor stamps
// whom a change was made for beside an agent's actor alone, and only where the
// routing wakes a seat — a person and the engine write for nobody but
// themselves, and a routing that tells only persons wakes no turn to carry it
// to. Handed founder's seat as the carrier, a routing that tells reader, an
// agent, carries it beside dev's act and nothing beside founder's own act or
// beside no actor at all; and one that tells founder alone carries nothing
// beside dev's act either.
//
// Mutations: stamp a person actor's routing — founder's carries the carrier,
// red; stamp every routing whatever it wakes — the one telling founder alone
// carries it, red; never stamp — dev's carries nothing, red.
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
		want     *types.WorkingFor
	}{
		{"beside dev's act, telling reader", "dev", PartyAgent, []string{"reader"}, founder},
		{"beside founder's own act, telling reader", "founder", PartyPerson, []string{"reader"}, nil},
		{"beside no actor, telling reader", "", "", []string{"reader"}, nil},
		{"beside dev's act, telling founder alone", "dev", PartyAgent, []string{"founder"}, nil},
	} {
		r, err := routingFor(t.Context(), nil, concerning{repoName: "web", change: row, actor: tc.actor,
			actorKind: tc.kind, workingFor: founder, roster: seatChart{}, mentions: tc.mentions})
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
