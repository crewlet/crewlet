package engine

import "github.com/crewlet/crewlet/internal/iamdomain"

// identityMoved is what the identity applier calls after a committed batch,
// with what that batch moved.
//
// A SEAT'S STANDING is the party registry's to act on — see
// [Engine.nudgeDirectory], which this signals rather than runs: it is called
// on the apply loop's own goroutine with the next batch waiting behind it.
func (e *Engine) identityMoved(moved iamdomain.Moved) {
	if moved.Seats {
		e.nudgeDirectory()
	}
}
