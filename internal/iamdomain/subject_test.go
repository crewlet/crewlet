package iamdomain_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A SUBJECT ROUND-TRIPS THROUGH THE LOG'S GRAMMAR, including the dots a node
// id carries (`node-a.example`).
func TestASubjectRoundTripsThroughTheWireGrammar(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]iamdomain.Subject{
		"the directory":     iamdomain.DirectorySubject(),
		"a person":          iamdomain.PersonSubject("018f-0001"),
		"a session lineage": iamdomain.SessionSubject("018f-0003"),
		"a sweep":           iamdomain.SweepSubject(7),
		"an eviction":       iamdomain.EvictionSubject("node-a.example"),
		"a reanchor":        iamdomain.GenerationSubject(3),
		"the invalidation":  iamdomain.InvalidationSubject(),
		"the barrier":       iamdomain.BarrierSubject(),
		"a trimmed record": iamdomain.TrimmedSubject(statelog.Position{
			Stream: topics.IamLogStream, Generation: 2, Seq: 41}),
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
				t.Errorf("%q parses back as %+v, want %+v — an id may carry "+
					"a dot, and splitting on every one loses everything after "+
					"the first", wire, got, want)
			}
			if err := want.Validate(); err != nil {
				t.Errorf("a subject this package builds was refused: %v", err)
			}
		})
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
	// AND A KIND THIS BUILD DOES NOT KNOW IS NOT REFUSED: this build must
	// hold a newer peer's record it cannot act on
	// ([iamdomain.Subject.Validate]).
	newer := iamdomain.Subject{Kind: "somethingnewer", ID: "x"}
	if err := newer.Validate(); err != nil {
		t.Errorf("a kind a newer peer wrote was refused at the subject layer: "+
			"%v — this build could then only DROP that record", err)
	}
}

// THE THREE SINGLETON KINDS ARE THE THREE THE GRAMMAR SAYS THEY ARE, both
// ways: a kind that stopped needing an id would publish to the log's own
// prefix, and a singleton given one would let two writes that must contend
// (one login for two people, two invalidations) miss each other.
func TestTheSingletonKindsAreTheOnesWithNoId(t *testing.T) {
	t.Parallel()
	singletons := map[iamdomain.ObjectKind]bool{
		iamdomain.KindDirectory:    true,
		iamdomain.KindInvalidation: true,
		iamdomain.KindBarrier:      true,
	}
	for _, kind := range iamdomain.ObjectKinds {
		want := !singletons[kind]
		if got := kind.Identified(); got != want {
			t.Errorf("%s reports Identified() = %v, want %v", kind, got, want)
		}
		// An idless subject sits ONE token below the prefix, which a
		// wildcard written as `prefix.*.>` would silently exclude.
		if !want {
			s := iamdomain.Subject{Kind: kind}
			if s.Wire() != topics.IamLogPrefix+"."+string(kind) {
				t.Errorf("%s builds the subject %q", kind, s.Wire())
			}
		}
	}
}

// EVERY KIND BUT THE BARRIER AND THE TRIMMED RECORD ARBITRATES, and the
// stream's declaration is DERIVED from that: a list written twice can disagree,
// which wedges that subject the first time a gate drops a record.
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
	if len(spec.ArbitratedKinds) != len(iamdomain.ObjectKinds)-2 {
		t.Errorf("the stream arbitrates %d of %d kinds — the barrier shares one "+
			"subject across the whole domain, so an expectation there would "+
			"serialise every linearizable read behind every other one, and a "+
			"trimmed record removes anchors, so arbitrated it would write one of "+
			"its own (ADR-0080)",
			len(spec.ArbitratedKinds), len(iamdomain.ObjectKinds))
	}
}

// EXACTLY FIVE KINDS MAY STATE THE WHOLE ESTATE AS THEIR SCOPE, both ways: a
// kind that gained it would freeze every login on the first record a node could
// not decode, and one that lost it would be a gate or a reanchor blocking only
// some reads. The five are never deferred ([iamdomain.ObjectKind.RootScoped]).
func TestExactlyFiveKindsMayClaimTheWholeEstate(t *testing.T) {
	t.Parallel()
	want := []iamdomain.ObjectKind{
		iamdomain.KindInvalidation, iamdomain.KindEviction,
		iamdomain.KindGeneration, iamdomain.KindBarrier, iamdomain.KindTrimmed,
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
