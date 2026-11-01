package iamdomain_test

import (
	"slices"
	"testing"

	"github.com/google/uuid"
)

// A REMOVAL IS READ BACK BY ITS TOMBSTONE, and only a removal: what another
// domain's erasure is reconciled against names exactly the persons whose
// removal this node applied, in the order it was asked, and never a person
// who is still here or an id nobody ever held. Mutation: answer every id
// asked about and the person still here is named.
func TestARemovedPersonIsReadBackByTheirTombstone(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	gone := bindNew(t, rig, "noor.aziz", "ops-lead")
	here := bindNew(t, rig, "sarah.chen", "sarah-chen")
	if err := rig.draining(func() error {
		_, err := rig.writer.Remove(t.Context(), gone, "op-remove", "left")
		return err
	}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	rig.drain()
	nobody := uuid.NewString()
	got, err := rig.reader(t).Removed(t.Context(), []string{here, gone, nobody})
	if err != nil {
		t.Fatalf("Removed: %v", err)
	}
	if !slices.Equal(got, []string{gone}) {
		t.Errorf("the tombstones name %v, want only %s", got, gone)
	}
	if got, err := rig.reader(t).Removed(t.Context(), nil); err != nil || len(got) != 0 {
		t.Errorf("asking about nobody answers %v, %v", got, err)
	}
}
