package iamdomain_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
)

// THE DECLARATION HALF OF THE FRAMEWORK'S OWN SUITE, run against the fifth
// domain as soon as there is a declaration to run it against.
//
// [statelogtest.Run]'s four halves include the APPLY cases, and this domain
// has no applier yet — the engine's boot check refuses a register entry with a
// nil applier, so a registration cannot land before its applier, and a
// candidate with a nil one would fail for the wrong reason. [Declaration] is a
// pure function over what the domain SAYS about itself, and every contract it
// checks is one this change is entirely made of: the table classification the
// scrub list and the identity claim are derived from, the stream's replay
// protocol, the arbitrated kinds, the operation ledger's pairing with seat
// admission, and the plain-identifier rule the framework interpolates these
// names under.
//
// It is the half that can run now, so it runs now rather than waiting for a
// later change to certify a declaration this one shipped.
func TestTheIdentityDomainsDeclarationIsWellFormed(t *testing.T) {
	t.Parallel()
	kinds := make([]string, 0, len(iamdomain.ObjectKinds))
	for _, k := range iamdomain.ObjectKinds {
		kinds = append(kinds, string(k))
	}
	errs := statelogtest.Declaration(statelogtest.Candidate{
		Domain: iamdomain.Domain{},
		Kinds:  kinds,
	})
	for _, err := range errs {
		t.Error(err)
	}
}
