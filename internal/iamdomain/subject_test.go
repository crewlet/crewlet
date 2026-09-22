package iamdomain_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// NO TWO CLAIMS SHARE A SUBJECT, and it is the whole of what keeps two people
// from holding one identity.
//
// There is no unique index anywhere in this estate and there cannot be one, so
// the subject a claim arbitrates on is the uniqueness check. A shared subject
// fails in two directions and one of them is silent: two distinct claims
// folded onto one token never contend at all, and the duplicate identity
// nobody refused is discovered by somebody signing in as somebody else.
func TestEveryClaimArbitratesOnASubjectOfItsOwn(t *testing.T) {
	t.Parallel()
	// The same string as an address, a login, a seat and a lineage — which
	// is the shape that catches a grammar folding two namespaces together,
	// since anything that DID would map them all to one token.
	const same = "jane-doe"
	wire := map[string]string{
		"an address":     iamdomain.EmailSubject(same).Wire(),
		"a login":        iamdomain.LoginSubject(same).Wire(),
		"a seat binding": iamdomain.SeatSubject(same).Wire(),
		"a session":      iamdomain.SessionSubject(same).Wire(),
		"a person":       iamdomain.PersonSubject(same).Wire(),
	}
	seen := map[string]string{}
	for what, subject := range wire {
		if other, clash := seen[subject]; clash {
			t.Errorf("%s and %s both arbitrate on %q — two claims on one "+
				"subject means the second never contends, and nothing else in "+
				"this estate could ever refuse it", what, other, subject)
		}
		seen[subject] = what
	}
}

// A SUBJECT ROUND-TRIPS THROUGH THE LOG'S GRAMMAR, including a login's dots.
//
// internal/iam REQUIRES the dot in a person's login, because it is what tells
// a login apart from a seat handle in an audit row — so `jane.doe` is not an
// awkward edge case here, it is what every login claim looks like.
func TestASubjectRoundTripsThroughTheWireGrammar(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]iamdomain.Subject{
		"a person":              iamdomain.PersonSubject("018f-0001"),
		"an address":            iamdomain.EmailSubject("9f8e7d6c5b4a3928"),
		"a login, with its dot": iamdomain.LoginSubject("jane.doe"),
		"a machine handle":      iamdomain.LoginSubject("ci:release"),
		"a seat binding":        iamdomain.SeatSubject("018f-0002"),
		"a session lineage":     iamdomain.SessionSubject("018f-0003"),
		"a sweep":               iamdomain.SweepSubject(7),
		"an eviction":           iamdomain.EvictionSubject("node-a.example"),
		"a reanchor":            iamdomain.GenerationSubject(3),
		"the bootstrap":         iamdomain.BootstrapSubject(),
		"the barrier":           iamdomain.BarrierSubject(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			wire := want.Wire()
			if wire == "" {
				t.Fatalf("%+v is not publishable at all", want)
			}
			got, ok := iamdomain.ParseSubject(wire)
			if !ok {
				t.Fatalf("%q does not parse back as a subject on this log", wire)
			}
			if got != want {
				t.Errorf("%q parses back as %+v, want %+v — a login carries a "+
					"dot in every case, and splitting on every one loses "+
					"everything after the first", wire, got, want)
			}
			if err := want.Validate(); err != nil {
				t.Errorf("a subject this package builds was refused: %v", err)
			}
		})
	}
}

// A LOGIN IS FOLDED ONCE, ON THE WAY INTO THE SUBJECT.
//
// `Jane.Doe` and `jane.doe` are one address. Two subjects would make them two
// people, which is exactly the duplicate nothing in this estate can refuse
// after the fact — and a folding done in SQL instead would be a second answer
// to what one login is, in a dialect where the Go answer beside it disagrees.
func TestALoginIsFoldedIntoOneSubject(t *testing.T) {
	t.Parallel()
	a := iamdomain.LoginSubject("Jane.Doe").Wire()
	b := iamdomain.LoginSubject("  jane.doe  ").Wire()
	if a != b {
		t.Errorf("%q and %q are two subjects — two administrators claiming one "+
			"login would never contend, and both claims would land", a, b)
	}
	if !iam.ValidLogin(strings.TrimPrefix(
		iamdomain.LoginSubject("Jane.Doe").ID, "")) {
		t.Error("the folded login is not one internal/iam would accept, so the " +
			"subject and the vocabulary disagree about what a login is")
	}
}

// A SUBJECT WITH NO KIND IS NOT PUBLISHABLE, and a kind that needs an id and
// has none is refused where the record is written.
func TestValidateRefusesASubjectThatAddressesNothing(t *testing.T) {
	t.Parallel()
	for name, s := range map[string]iamdomain.Subject{
		"no kind":                {ID: "x"},
		"a person with no id":    {Kind: iamdomain.KindPerson},
		"a kind with a dot":      {Kind: "per.son", ID: "x"},
		"a kind with a wildcard": {Kind: "per>son", ID: "x"},
		"an id with a wildcard":  {Kind: iamdomain.KindPerson, ID: "a>b"},
		"an id with whitespace":  {Kind: iamdomain.KindPerson, ID: "a b"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := s.Validate(); err == nil {
				t.Errorf("%+v was accepted", s)
			}
		})
	}
	// AND A KIND THIS BUILD DOES NOT KNOW IS NOT REFUSED. It is refused
	// where a record is WRITTEN and accepted where one is READ, which is
	// the asymmetry the whole two-pass decode exists for: this build must
	// be able to hold a newer peer's record under its own subject without
	// being able to act on it.
	newer := iamdomain.Subject{Kind: "somethingnewer", ID: "x"}
	if err := newer.Validate(); err != nil {
		t.Errorf("a kind a newer peer wrote was refused at the subject layer: "+
			"%v — this build could then only DROP that record", err)
	}
}

// THE THREE SINGLETON KINDS ARE THE THREE THE GRAMMAR SAYS THEY ARE.
//
// Both directions: a kind that stopped needing an id would silently start
// publishing to the log's own prefix, and a singleton that acquired one would
// stop being a singleton — which for the bootstrap means two live ways into an
// engine that has no other way in, and for the invalidation means two
// operators ending the company's sessions without ever contending, each
// reading the same generation and writing the same new one.
func TestTheSingletonKindsAreTheOnesWithNoId(t *testing.T) {
	t.Parallel()
	singletons := map[iamdomain.ObjectKind]bool{
		iamdomain.KindBootstrap:    true,
		iamdomain.KindInvalidation: true,
		iamdomain.KindBarrier:      true,
	}
	for _, kind := range iamdomain.ObjectKinds {
		want := !singletons[kind]
		if got := kind.Identified(); got != want {
			t.Errorf("%s reports Identified() = %v, want %v", kind, got, want)
		}
		// And the grammar agrees: an idless subject sits ONE token below
		// the prefix rather than two, which is the shape a wildcard
		// written as `prefix.*.>` would silently exclude.
		if !want {
			s := iamdomain.Subject{Kind: kind}
			if s.Wire() != topics.IamLogPrefix+"."+string(kind) {
				t.Errorf("%s builds the subject %q", kind, s.Wire())
			}
		}
	}
}

// EVERY KIND BUT THE BARRIER ARBITRATES, and the declaration the stream is
// created with is DERIVED from that rather than typed again.
//
// A list written twice is a kind that arbitrates in one place and not the
// other, which wedges that subject the first time a gate drops a record.
func TestTheArbitratedKindsAreDerivedFromTheEnum(t *testing.T) {
	t.Parallel()
	spec := iamdomain.Domain{}.Stream()
	for _, kind := range iamdomain.ObjectKinds {
		named := false
		for _, k := range spec.ArbitratedKinds {
			if k == string(kind) {
				named = true
			}
		}
		if named != kind.Arbitrated() {
			t.Errorf("%s reports Arbitrated() = %v and the stream spec %s it",
				kind, kind.Arbitrated(), map[bool]string{
					true: "names", false: "does not name"}[named])
		}
	}
	if len(spec.ArbitratedKinds) != len(iamdomain.ObjectKinds)-1 {
		t.Errorf("the stream arbitrates %d of %d kinds — the barrier shares one "+
			"subject across the whole domain, so an expectation there would "+
			"serialise every linearizable read behind every other one",
			len(spec.ArbitratedKinds), len(iamdomain.ObjectKinds))
	}
}

// EXACTLY FOUR KINDS MAY STATE THE WHOLE ESTATE AS THEIR SCOPE.
//
// Both directions, because each failure is silent in its own way: a kind that
// gained the permission would freeze every login in the company on the first
// record a node could not decode, and one that lost it would be a gate or a
// reanchor blocking only some reads while licensing the rest.
//
// EVERY ONE OF THE FOUR IS A KIND THAT IS NEVER DEFERRED, which is what makes
// the permission affordable: three install a gate and the fourth declares the
// framework's own barrier scope, so none of them can be the record a node
// cannot decode.
func TestExactlyFourKindsMayClaimTheWholeEstate(t *testing.T) {
	t.Parallel()
	want := []iamdomain.ObjectKind{
		iamdomain.KindInvalidation, iamdomain.KindEviction,
		iamdomain.KindGeneration, iamdomain.KindBarrier,
	}
	for _, kind := range iamdomain.ObjectKinds {
		got := kind.RootScoped()
		if expect := slices.Contains(want, kind); got != expect {
			t.Errorf("%s reports RootScoped() = %v, want %v", kind, got, expect)
		}
	}
}

// AN UNKNOWN KIND AND AN UNKNOWN OP ARE VALUES, NOT PANICS.
func TestAnUnknownKindOrOpOffTheWireIsAValue(t *testing.T) {
	t.Parallel()
	if iamdomain.ObjectKind("somethingnewer").Valid() {
		t.Error("a kind this build has never heard of reported itself valid")
	}
	if iamdomain.OpKind("somethingnewer").Valid() {
		t.Error("an op this build has never heard of reported itself valid")
	}
	for _, k := range iamdomain.ObjectKinds {
		if !k.Valid() {
			t.Errorf("%s is declared and reports itself invalid", k)
		}
	}
	for _, o := range iamdomain.OpKinds {
		if !o.Valid() {
			t.Errorf("%s is declared and reports itself invalid", o)
		}
	}
}
