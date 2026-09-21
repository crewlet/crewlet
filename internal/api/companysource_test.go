package api_test

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
	return liveCompanySource(t, func() *config.Company { return c })
}

// liveCompanySource is [companySource] over a company a case REPLACES while the
// app is running, which is what the production seam does on every apply: the
// pair is read on each call rather than captured, so a case can assert that a
// screen follows the live epoch rather than the one it was built with.
func liveCompanySource(t *testing.T, read func() *config.Company,
) func() (*config.Company, *org.Organization) {
	t.Helper()
	return func() (*config.Company, *org.Organization) {
		c := read()
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
