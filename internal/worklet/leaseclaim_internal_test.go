package worklet

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
)

// A WALK'S CLAIM NAMES ITSELF WITHIN A LEASE'S RECORD: a move's and a merge's
// claim on a task, whose id is a UUID (IsTaskID), and the owner a walk takes
// it under, a node's id and a UUID, each within coord.MaxLeaseNameBytes.
//
// Mutation: lower coord.MaxLeaseNameBytes to 100, and the owner is refused.
func TestAWalksClaimNamesItselfWithinALeasesRecord(t *testing.T) {
	t.Parallel()
	task := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff").String()
	if !IsTaskID(task) {
		t.Fatalf("premise: %s is no task id", task)
	}
	w := &Writer{nodeID: strings.Repeat("n", coord.MaxNodeIDBytes)}
	for what, name := range map[string]string{"a move's claim": moveClaim(task),
		"a merge's claim": mergeClaim(task), "a walk's owner": w.claimOwner()} {
		if err := coord.HoldLeaseName(what, name); err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}
}
