package setupapi_test

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
