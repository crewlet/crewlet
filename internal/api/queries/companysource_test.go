package queries_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// companySource is the company seam over an AUTHORED document.
//
// A running node composes its org from the chart's own rows; a test holds a
// file, so the org a fixture answers with is the one that document describes.
// BOTH HALVES COME FROM ONE CALL, which is the contract the seam states: they
// move on different rhythms in production, so a fixture that read them
// separately would not be exercising the shape the engine has.
func companySource(t *testing.T, c *config.Company) func() (*config.Company, *org.Organization) {
	t.Helper()
	return func() (*config.Company, *org.Organization) {
		if c == nil {
			return nil, nil
		}
		o, err := c.Organization()
		if err != nil {
			// LOUD, because a fixture whose document will not build is
			// a fixture with no seats — and every case about a seat
			// would then pass over an empty company for a reason
			// nothing in its output says.
			t.Fatalf("this fixture's company does not build an org: %v", err)
		}
		return c, o
	}
}

// eachAuthoredSeat walks every seat a DOCUMENT declares, at any depth.
//
// A test fixture that hangs a per-seat credential is authoring a file, so the
// walk it wants is the file's — and `config`'s own is unexported for exactly
// the reason this comment exists: outside that package, "walk the document"
// and "walk the company" are different questions, and only one of them is
// answered by `roles:` and `units:`.
func eachAuthoredSeat(c *config.Company, visit func(*config.Role)) {
	for i := range c.Roles {
		visit(&c.Roles[i])
	}
	var walk func([]config.Unit)
	walk = func(units []config.Unit) {
		for i := range units {
			for j := range units[i].Roles {
				visit(&units[i].Roles[j])
			}
			walk(units[i].Children)
		}
	}
	walk(c.Units)
}
