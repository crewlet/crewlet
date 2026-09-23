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

// A CALLER'S OWN RECORD IS KEPT UNDER THE NAME THEY WRITE UNDER.
//
// The tools wrote a caller's inbox, pins, priorities and personal views under
// [ActorFor]'s name and the query surface read them back under the principal's
// SEAT — one value for a bound person, and nothing at all for an unbound one,
// whose record was written under a login nothing ever read. The three shapes
// of caller are the first three cases, and the universal half holds the two
// functions to one name for every principal that has one.
func TestTheOwnRecordIsKeptUnderTheNameAWriteIsMadeUnder(t *testing.T) {
	for _, c := range []struct {
		name string
		in   Principal
		want string
	}{
		{"a bound person's is their seat",
			Principal{Kind: KindPerson, Login: "jane.doe", Seat: "founder"}, "founder"},
		{"an unbound person's is their login",
			Principal{Kind: KindPerson, Login: "jane.doe"}, "jane.doe"},
		{"an unbound token's is its login, colon and all",
			Principal{Kind: KindMachine, Login: "token:ops"}, "token:ops"},
		{"a machine carrying a seat is still the credential",
			Principal{Kind: KindMachine, Login: "token:ops", Seat: "founder"}, "token:ops"},
		{"a seat's is its handle",
			Principal{Kind: KindSeat, Seat: "backend-lead"}, "backend-lead"},
		// NOBODY HAS NO RECORD, rather than sharing one called
		// "anonymous" with everybody else who is nobody.
		{"a person with no name has none", Principal{Kind: KindPerson}, ""},
		{"a kind this build cannot read has none",
			Principal{Kind: Kind("delegate-swarm"), Login: "jane.doe"}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := RecordOwner(c.in); got != c.want {
				t.Fatalf("RecordOwner(%+v) = %q, want %q", c.in, got, c.want)
			}
		})
	}

	for _, kind := range Kinds {
		for _, seat := range []string{"", "founder"} {
			for _, login := range []string{"", "jane.doe", "token:ops"} {
				p := Principal{Kind: kind, Seat: seat, Login: login}
				owner := RecordOwner(p)
				if owner == "" {
					continue
				}
				if got := ActorFor(p).Name; got != owner {
					t.Errorf("%+v writes as %q and keeps its record under %q",
						p, got, owner)
				}
			}
		}
	}
}

// EITHER OF A PERSON'S NAMES IS THEM, AND NOTHING ELSE IS.
//
// A bound person is known by their seat and by their login, and a surface that
// compared against one of them read the other as a colleague's name. Nobody
// names nothing, so a principal with no record is never "self" — least of all
// under the empty name every unnamed argument arrives as.
func TestEitherOfAPersonsNamesIsThem(t *testing.T) {
	bound := Principal{Kind: KindPerson, Login: "jane.doe", Seat: "jane"}
	for _, c := range []struct {
		p    Principal
		name string
		self bool
	}{
		{bound, "jane", true},
		{bound, "jane.doe", true},
		{bound, "bo", false},
		{bound, "", false},
		{Principal{Kind: KindMachine, Login: "token:ops"}, "token:ops", true},
		{Principal{Kind: KindMachine, Login: "token:ops", Seat: "jane"}, "jane", false},
		{Principal{Kind: KindPerson}, "", false},
		{Principal{Kind: Kind("delegate-swarm"), Login: "jane.doe"}, "jane.doe", false},
	} {
		if got := NamesSelf(c.p, c.name); got != c.self {
			t.Errorf("NamesSelf(%+v, %q) = %v, want %v", c.p, c.name, got, c.self)
		}
	}
}
