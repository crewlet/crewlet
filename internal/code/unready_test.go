package code_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/code"
)

// A WALK LETS ANOTHER NODE FILL ONLY PAST ONE BRINGING NO COPY UP (ADR-0059).
// Every reason a git node gives for not answering a read is held to whether it
// passes the walk's one fill on: a node behind its configuration (which judges
// no seat until its apply succeeds) and one that failed on its own pass it, so
// the next node fills the copy the read needs; a node syncing — filling,
// repairing, or failing on what every node's fill would meet — at its share,
// or seconds from its chart keeps it, since a second fill beside it would
// double the download. A reason this build does not know passes nothing. Every
// reason of the closed set is classified here, so one added later is decided
// the day it is declared.
//
// Mutation: let `syncing` pass the fill — red.
func TestAReasonPassesTheFillOnlyWhereNoCopyIsComing(t *testing.T) {
	t.Parallel()
	want := map[code.UnreadyReason]bool{
		code.UnreadySyncing:      false,
		code.UnreadyShare:        false,
		code.UnreadyNoChart:      false,
		code.UnreadyBehindConfig: true,
		code.UnreadyNotHeld:      true,
		code.UnreadyNodeFailed:   true,
		// A holder building its index holds its copy: the second holder,
		// which holds one too, answers — never a fill beside them.
		code.UnreadyIndexBuilding: false,
	}
	for _, r := range code.UnreadyReasons() {
		passes, classified := want[r]
		if !classified {
			t.Errorf("%s is a reason this test does not classify: decide whether it passes the fill", r)
			continue
		}
		if got := r.PassesFill(); got != passes {
			t.Errorf("%s passes the fill: %v, want %v", r, got, passes)
		}
	}
	for _, unknown := range []code.UnreadyReason{"", "drowsy"} {
		if unknown.PassesFill() {
			t.Errorf("%q, no reason this build knows, passes the fill: an older asker would start a second "+
				"download for a reason it cannot read", unknown)
		}
	}
}
