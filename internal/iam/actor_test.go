package iam

import (
	"context"
	"errors"
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

// holdersSpy is a directory that answers from a map and counts what it was
// asked, so a case can show which names never reached it.
type holdersSpy struct {
	records map[string]string
	unseat  map[string]bool
	err     error
	asked   []string
}

func (h *holdersSpy) HolderRecord(_ context.Context, login string) (string, error) {
	h.asked = append(h.asked, login)
	switch {
	case h.err != nil:
		return "", h.err
	case h.unseat[login]:
		return "", ErrHolderUnseated
	}
	owner, held := h.records[login]
	if !held {
		return "", ErrNoHolder
	}
	return owner, nil
}

// A NAME ADDRESSES ONE RECORD, WHOEVER NAMES IT, AND A LOGIN IS NEVER A SEAT.
//
// [RecordOwner] made the caller's own record one name for the read and the
// write; somebody else's was read and written under whatever was typed. An
// administrator's `jane.doe`, for a person the directory binds to the seat
// `jane`, read an empty inbox and wrote pins under the login that no screen of
// hers reads — and `set_priorities` sent it through the colleague resolver,
// which set the priorities of whichever seat it resembled. A login is looked
// up, and a name that is not one is handed back for the chart, UNSETTLED.
func TestANameAddressesTheRecordItsHolderKeeps(t *testing.T) {
	t.Parallel()
	jane := Principal{Kind: KindPerson, Login: "jane.doe", Seat: "jane"}
	admin := Principal{Kind: KindMachine, Login: "token:admin"}
	directory := func() *holdersSpy {
		return &holdersSpy{
			records: map[string]string{
				"jane.doe": "jane", "bo.smith": "bo.smith",
				"token:ci": "ops", "token:admin": "token:admin",
			},
			unseat: map[string]bool{"leaver.person": true},
		}
	}
	for _, c := range []struct {
		name    string
		caller  Principal
		typed   string
		owner   string
		settled bool
		err     error
		lookup  bool
	}{
		{"nothing typed is your own", jane, "", "jane", true, nil, false},
		{"your own login is your own record", jane, "jane.doe", "jane", true, nil, false},
		{"your own seat is your own record", jane, "jane", "jane", true, nil, false},
		{"a bound person's login is their seat's record", admin, "jane.doe",
			"jane", true, nil, true},
		{"an unbound person's login is their own", admin, "bo.smith",
			"bo.smith", true, nil, true},
		{"a bound token's login is its seat's record", admin, "token:ci",
			"ops", true, nil, true},
		{"a login nobody holds names no record", admin, "ghost.person",
			"", false, ErrNoHolder, true},
		{"a holder whose seat is gone names none either", admin, "leaver.person",
			"", false, ErrHolderUnseated, true},
		{"a seat's handle is the chart's to resolve", admin, "dev",
			"dev", false, nil, false},
		{"so are words", admin, "Jane Doe", "Jane Doe", false, nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			holders := directory()
			owner, settled, err := OwnerOf(context.Background(), c.caller, c.typed, holders)
			switch {
			case c.err != nil && !errors.Is(err, c.err):
				t.Fatalf("OwnerOf(%q) = %v, want %v", c.typed, err, c.err)
			case c.err == nil && err != nil:
				t.Fatalf("OwnerOf(%q) = %v", c.typed, err)
			}
			if owner != c.owner || settled != c.settled {
				t.Errorf("OwnerOf(%q) = %q settled=%v, want %q settled=%v",
					c.typed, owner, settled, c.owner, c.settled)
			}
			if asked := len(holders.asked) > 0; asked != c.lookup {
				t.Errorf("OwnerOf(%q) asked the directory = %v, want %v — "+
					"your own names and a seat's handle are not its to answer",
					c.typed, asked, c.lookup)
			}
		})
	}
}

// A DIRECTORY THAT CANNOT SAY IS NEVER READ AS NOBODY, and a surface wired
// without one reads no login literally.
//
// Both are the UNKNOWN arm: "nobody holds it" answered off a directory that
// could not be read is a guess about whose record to write, and a missing
// directory answering the login itself is the defect this function replaced.
func TestADirectoryThatCannotSayIsNeverReadAsNobody(t *testing.T) {
	t.Parallel()
	admin := Principal{Kind: KindMachine, Login: "token:admin"}
	for name, holders := range map[string]Holders{
		"a directory that could not be read": &holdersSpy{err: errors.New("store blip")},
		"no directory at all":                nil,
	} {
		owner, settled, err := OwnerOf(context.Background(), admin, "jane.doe", holders)
		if err == nil || errors.Is(err, ErrNoHolder) || errors.Is(err, ErrHolderUnseated) {
			t.Errorf("%s: OwnerOf = %q, %v, %v — want the unknown arm", name,
				owner, settled, err)
		}
		if owner != "" || settled {
			t.Errorf("%s: OwnerOf answered %q settled=%v beside an error", name,
				owner, settled)
		}
	}
}

// A LOGIN'S SHAPE IS NEVER A SEAT'S, in either grammar.
func TestALoginShapedNameIsNeverASeat(t *testing.T) {
	t.Parallel()
	for name, login := range map[string]bool{
		"jane.doe": true, "token:ops": true, "ci:release": true,
		"jane": false, "Jane Doe": false, "jane@example.com": false, "": false,
		"Jane.Doe": false,
	} {
		if got := NamesLogin(name); got != login {
			t.Errorf("NamesLogin(%q) = %v, want %v", name, got, login)
		}
	}
}
