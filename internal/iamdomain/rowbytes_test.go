package iamdomain_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE PARTY REGISTRY A NODE HOLDING NO DATA READS IS REFUSED PAST ITS BYTES,
// never cut: its one caller rebuilds the registry whole, and a slice of it
// would hand the missing seats to the chart. Within the bound it is the whole
// listing.
//
// Mutations, each red: drop the Take check over the bindings (a slice is
// answered); answer a cut registry without an error.
func TestThePartyRegistryIsRefusedPastItsBytesNeverCut(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)
	bindNewWith(t, rig, "sarah.chen", "sarah-chen", []iam.Grant{iam.GrantWorklet})
	bindNewWith(t, rig, "priya.shah", "platform-lead", []iam.Grant{iam.GrantAdmin})
	whole, err := reader.SeatHolders(t.Context())
	if err != nil || len(whole) != 2 {
		t.Fatalf("the registry is %+v, %v; want both bindings", whole, err)
	}
	if held, err := reader.SeatHoldersHeld(t.Context()); err != nil || len(held) != len(whole) {
		t.Errorf("the held registry within its bound is %d bindings, %v; want %d", len(held), err, len(whole))
	}
	one := statelog.WithAnswerBytes(t.Context(), 1)
	if held, err := reader.SeatHoldersHeld(one); err == nil {
		t.Errorf("a registry past one binding's bytes was answered as %+v, not refused", held)
	}
}
