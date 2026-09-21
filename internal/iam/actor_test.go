package iam

import (
	"testing"

	"github.com/google/uuid"
)

// TestActorForIsTotalOverFourCases protects the one property a row-writing
// call needs: every principal, however malformed, comes back as exactly one of
// the four author kinds three stores already hold, under a name that is never
// empty. There is nowhere for this function to return an error to.
func TestActorForIsTotalOverFourCases(t *testing.T) {
	cases := []struct {
		name string
		in   Principal
		want Actor
	}{{
		name: "a seat acting inside its turn is the agent",
		in:   Principal{Kind: KindSeat, Seat: "backend-lead"},
		want: Actor{Name: "backend-lead", Kind: ActorAgent},
	}, {
		// The degradation rule: the DECLARED kind stands, only the
		// name falls back. Calling this the engine's write would put
		// somebody else's name on somebody's decision.
		name: "a seat with no handle is still an agent, under nobody's name",
		in:   Principal{Kind: KindSeat},
		want: Actor{Name: AnonymousActor, Kind: ActorAgent},
	}, {
		name: "a person bound to a seat acts as themselves",
		in:   Principal{Kind: KindPerson, Login: "jane.doe", Seat: "founder"},
		want: Actor{Name: "founder", Kind: ActorHuman},
	}, {
		// THE SEATLESS LOGIN, which carries a dot: an operator who is
		// not in the org chart acts as the credential, under its own
		// login, and the dot is what stops that name being read as a
		// seat handle.
		name: "a person with no seat acts as the credential",
		in:   Principal{Kind: KindPerson, Login: "jane.doe"},
		want: Actor{Name: "jane.doe", Kind: ActorOperator},
	}, {
		// THE MACHINE HANDLE, which carries a colon, for the same
		// reason and into the same column.
		name: "a machine is an operator under its own handle",
		in:   Principal{Kind: KindMachine, Login: "ci:release"},
		want: Actor{Name: "ci:release", Kind: ActorOperator},
	}, {
		name: "a machine with no handle is still an operator",
		in:   Principal{Kind: KindMachine},
		want: Actor{Name: AnonymousActor, Kind: ActorOperator},
	}, {
		name: "the engine names itself with the node id",
		in:   Principal{Kind: KindEngine, Login: "node-7"},
		want: Actor{Name: "node-7", Kind: ActorSystem},
	}, {
		name: "the engine with no node id is still the system",
		in:   Principal{Kind: KindEngine},
		want: Actor{Name: AnonymousActor, Kind: ActorSystem},
	}, {
		// A kind this build cannot read claims neither a seat nor the
		// engine, which is the least it can claim and stay total.
		name: "the zero kind is an operator nobody can name",
		in:   Principal{},
		want: Actor{Name: AnonymousActor, Kind: ActorOperator},
	}, {
		name: "a newer peer's kind is an operator nobody can name",
		in:   Principal{Kind: Kind("delegate-swarm"), Seat: "backend-lead", Login: "ci:release"},
		want: Actor{Name: AnonymousActor, Kind: ActorOperator},
	}}

	produced := map[ActorKind]bool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ActorFor(tc.in)
			if got != tc.want {
				t.Fatalf("ActorFor(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
		produced[ActorFor(tc.in).Kind] = true
	}

	// THE CONTROL, and the reason this test is not a list of literals: a
	// function that answered `operator` to everything would satisfy every
	// case above that expects one. All four have to come out of it.
	for _, kind := range ActorKinds {
		if !produced[kind] {
			t.Errorf("no case produced %q — ActorFor is not total over the four", kind)
		}
	}
}

// TestActorForNeverEscapesTheFourKinds is the universal half: over every
// declared kind and a spread of junk ones, with and without each name field,
// the answer is always one of the four and never nameless.
func TestActorForNeverEscapesTheFourKinds(t *testing.T) {
	kinds := append([]Kind{}, Kinds...)
	kinds = append(kinds, "", "Person", "seat ", "operator", "engine\n", "🙂")

	seats := []string{"", "backend-lead"}
	logins := []string{"", "jane.doe", "ci:release", "not a name at all"}

	checked := 0
	for _, kind := range kinds {
		for _, seat := range seats {
			for _, login := range logins {
				p := Principal{
					ID: uuid.New(), Kind: kind, Seat: seat, Login: login,
					Stage: StageActive,
				}
				got := ActorFor(p)
				if !got.Kind.Valid() {
					t.Fatalf("ActorFor(%+v).Kind = %q, which is not one of %v", p, got.Kind, ActorKinds)
				}
				if got.Name == "" {
					t.Fatalf("ActorFor(%+v) produced an empty author name", p)
				}
				checked++
			}
		}
	}
	if checked != len(kinds)*len(seats)*len(logins) {
		t.Fatalf("checked %d principals, want %d", checked, len(kinds)*len(seats)*len(logins))
	}
}
