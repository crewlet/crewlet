package configapi_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/configapi"
)

// TestTheChartCollectionsAreReadableAndNotWritableAtEveryDoor.
//
// There are THREE doors into one rule, and the rule is that a seat and a unit
// are not part of the company's settings: the HTTP route, the programmatic
// [configapi.Service.ApplyEntity] the engine's own setup flow uses, and the
// route table that decides which patterns to mount at all. A rule stated
// three times is a rule two of them eventually stop obeying, so every one of
// them reads the same table.
func TestTheChartCollectionsAreReadableAndNotWritableAtEveryDoor(t *testing.T) {
	t.Parallel()
	// THE TABLE ITSELF, first: it is what the other two doors read.
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

// AND THE PROGRAMMATIC DOOR REFUSES BY NAME.
//
// [configapi.Service.ApplyEntity] is the same write one layer down, reached by
// the engine's own setup flow rather than by a request. It used to fall
// through to "no such entity" — which, answered to a founder whose company
// plainly has a CEO, reads as the engine having lost their org chart.
func TestApplyingAChartEntityIsRefusedByName(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)

	_, err := s.service().ApplyEntity(t.Context(), configapi.ApplyEntityRequest{
		Kind: configapi.EntityRoles, ID: "ceo",
		Body: []byte(`{"name":"CEO","handle":"ceo"}`), Summary: "edit it here",
	})
	if !errors.Is(err, configapi.ErrEntityReadOnly) {
		t.Fatalf("err = %v, want ErrEntityReadOnly", err)
	}
	// IT NAMES WHAT IS WRITABLE, because the caller has to be able to tell
	// a collection that moved from one they misspelled.
	for _, kind := range configapi.WritableEntityKinds() {
		if !strings.Contains(err.Error(), kind) {
			t.Errorf("the refusal does not name %s: %v", kind, err)
		}
	}
	// AND THE CONTROL, one collection along: a writable kind is not
	// refused for being read-only.
	_, err = s.service().ApplyEntity(t.Context(), configapi.ApplyEntityRequest{
		Kind: configapi.EntityMCPServers, ID: "nothing-called-this",
		Body: []byte(`{"name":"nothing-called-this"}`), Summary: "s",
	})
	if errors.Is(err, configapi.ErrEntityReadOnly) {
		t.Fatalf("a writable collection was refused as read-only: %v", err)
	}
}
