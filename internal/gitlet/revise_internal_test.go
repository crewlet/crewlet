package gitlet

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/iam"
)

// A REVISE'S STAMPED BYTES COVER ITS WIDEST ASK: what the writer may add for a
// matched owner is at least the widest ask it can stamp for it, a person's
// longest handle asked for globs at MaxOwnedPathsBytes and the most it can omit.
//
// Mutation: price each owner at its skip alone (under the ask, red).
func TestARevisesStampedBytesCoverItsWidestAsk(t *testing.T) {
	t.Parallel()
	widest := OwnerAsk{Reviewer: Reviewer{Handle: strings.Repeat("h", iam.MaxLogin), Kind: PartyPerson},
		Paths:        []string{strings.Repeat("p", MaxOwnedPathsBytes-len(`[""]`))},
		PathsOmitted: MaxPathOwners * MaxPathOwnerPaths}
	asked, err := json.Marshal(widest)
	if err != nil {
		t.Fatalf("encode the widest ask: %v", err)
	}
	paths, _ := json.Marshal(widest.Paths)
	if len(paths) != MaxOwnedPathsBytes {
		t.Fatalf("the widest ask's globs are %d bytes as JSON, want %d", len(paths), MaxOwnedPathsBytes)
	}
	lists := len(`,"asked":[],"skipped_owners":[]`)
	for _, owners := range []int{1, MaxPathOwners} {
		r := Revise{RequestedOwners: make([]MatchedOwner, owners)}
		for i := range r.RequestedOwners {
			r.RequestedOwners[i].Owner = Owner{Kind: OwnerUnit, Ref: "Platform"}
		}
		if got, want := stampedReviseBytes(r), lists+owners*(len(asked)+1); got < want {
			t.Errorf("%d owners are priced at %d bytes, under the %d their widest asks take", owners, got, want)
		}
	}
}
