package coord_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/executorkeep"
)

// A SESSION CLAIM HOLDS EVERY SESSION ID A BOX MAY NAME: the claim's bound on
// each part is the one a box's session id is held to where it is read, so a
// session the sandbox binds is never refused by its claim's record.
//
// Mutation: lower MaxSessionClaimPartBytes to 64, and this is red.
func TestASessionClaimHoldsEverySessionIDABoxMayName(t *testing.T) {
	t.Parallel()
	if coord.MaxSessionClaimPartBytes != executorkeep.SessionIDMaxBytes {
		t.Errorf("a session claim holds %d bytes of a session id, a box may name one of %d",
			coord.MaxSessionClaimPartBytes, executorkeep.SessionIDMaxBytes)
	}
}
