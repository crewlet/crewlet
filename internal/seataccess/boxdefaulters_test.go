package seataccess_test

import (
	"maps"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// A VENDOR GAINS BOX DEFAULTS ONLY BY A REVIEWED CHANGE TO THIS LIST. What a
// [seataccess.BoxDefaulter] supplies reaches the box of every seat with any of
// its lanes on, beneath every layer a person writes, so which vendors supply
// one is held here, each with why: a vendor that starts to fails this test
// until it is named. Scaleway and Google Cloud do in this build. Every vendor
// is registered here (scoperequired_test.go), so the list is checked against
// all of them.
func TestAVendorGainsBoxDefaultsOnlyByAReviewedChange(t *testing.T) {
	t.Parallel()
	reviewed := map[integration.Kind]string{
		integration.KindScaleway: "the scw CLI reads the Organization and the seat's default project from " +
			"SCW_DEFAULT_ORGANIZATION_ID and SCW_DEFAULT_PROJECT_ID, which the company's connection and the " +
			"seat's projects decide, and SCW_DISABLE_CHECK_VERSION keeps its update notice out of the " +
			"agent's output; none is a secret, and a line the person writes wins",
		integration.KindGoogleCloud: "gcloud reads the agent's own token from a file the poll keeps " +
			"current, and its first project as the default; neither signs a harness in",
	}
	var supplying []integration.Kind
	registered := seataccess.Registered()
	for _, v := range registered {
		if _, ok := v.(seataccess.BoxDefaulter); ok {
			supplying = append(supplying, v.Kind())
		}
	}
	slices.Sort(supplying)
	if want := slices.Sorted(maps.Keys(reviewed)); !slices.Equal(supplying, want) {
		t.Errorf("the vendors supplying box defaults are %v and the reviewed list is %v: "+
			"name each with why its defaults belong in every on seat's box", supplying, want)
	}
	if len(registered) != len(seataccess.Kinds()) {
		t.Fatalf("%d vendors are registered here of the %d kinds, so the list is checked "+
			"against only some of them", len(registered), len(seataccess.Kinds()))
	}
}
