package org_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/org"
)

// THE ORG BUILDER SLUGS A NAME AS THE ENGINE DOES: a seat added on the canvas
// is offered the handle its name slugs to, so that it reads as the handle the
// engine derives from that name ([org.Slugify]); that holds only while the run
// the builder replaces with a hyphen is the engine's ([org.NonSlugPattern]).
//
// Mutation: change either side's pattern and this fails.
func TestTheOrgBuilderSlugsANameAsTheEngineDoes(t *testing.T) {
	t.Parallel()
	got, err := clientsource.Scalar(clientsource.Tree(t), "NON_SLUG_PATTERN")
	if err != nil {
		t.Fatal(err)
	}
	if got != org.NonSlugPattern {
		t.Errorf("the org builder slugs a name by %q and the engine by %q: change "+
			"NON_SLUG_PATTERN in contract/identity.ts", got, org.NonSlugPattern)
	}
}
