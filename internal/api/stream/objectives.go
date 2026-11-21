package stream

import (
	"time"

	"github.com/crewlet/crewlet/internal/objectives"
)

// Objectives is the node's service objective meter, as the socket's pushes
// record into it: one CHANGE VISIBILITY event for every committed change this
// node pushes to the sockets watching it ([Service.InboxChanged],
// [Service.ArtifactsMoved], [Service.ArtifactDataMoved], [ChatHub.fan]),
// timed from the broker's own instant for the record to the push.
//
// AT THE PUSH, not each socket's write (the viewer's network is not the
// engine's to promise); only a push some socket was handed is an event.
// FROM THE BROKER'S INSTANT, not this node's commit, so a peer-applied batch
// is timed as its viewer waited and every node's figure for a record starts
// at one instant ([objectives.Meter.RecordSince] absorbs clock skew).
// Satisfied by [*objectives.Meter]; nil records nothing.
type Objectives interface {
	RecordSince(n objectives.Name, stamp time.Time, answered bool)
}

// visible records one change pushed to the sockets watching it: sent is how
// many it was handed to ([fanOut]), stored the broker's instant for its
// record.
func visible(judge Objectives, sent int, stored time.Time) {
	if judge == nil || sent == 0 {
		return
	}
	judge.RecordSince(objectives.ChangeVisibility, stored, true)
}
