package configapi_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/api/configapi"
)

// TestTheChartCollectionsAreReadableAndNotWritableAtEveryDoor.
//
// There are TWO doors into one rule, and the rule is that a seat and a unit
// are not part of the company's settings: the HTTP route, and the route table
// that decides which patterns to mount at all. A rule stated twice is a rule
// one of them eventually stops obeying, so both read the same table.
func TestTheChartCollectionsAreReadableAndNotWritableAtEveryDoor(t *testing.T) {
	t.Parallel()
	// THE TABLE ITSELF, first: it is what both doors read.
	writable := configapi.WritableEntityKinds()
	for _, kind := range []string{configapi.EntityRoles, configapi.EntityUnits} {
		if slices.Contains(writable, kind) {
			t.Errorf("%s is writable here, and it is the org chart", kind)
		}
		// AND STILL ADDRESSABLE, which is the half that must not be
		// lost: a revision written before the split still carries both
		// inside it, and somebody repairing one has to be able to see
		// it.
		if !slices.Contains(configapi.EntityKinds(), kind) {
			t.Errorf("%s is not addressable at all, so a pre-split revision's "+
				"chart cannot be read", kind)
		}
	}
	// THE CONTROL: the collections this surface still writes are still
	// writable, or every case above would pass over a surface that had
	// simply lost the ability to write anything.
	for _, kind := range []string{
		configapi.EntityLLMProviders, configapi.EntityMCPServers,
	} {
		if !slices.Contains(writable, kind) {
			t.Errorf("%s is not writable, and nothing else writes it either", kind)
		}
	}
}
