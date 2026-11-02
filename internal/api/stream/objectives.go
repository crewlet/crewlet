package stream

import (
	"time"

	"github.com/crewlet/crewlet/internal/objectives"
)

// Objectives is the node's service objective meter, as the socket's pushes
// record into it: one CHANGE VISIBILITY event for every committed change this
// node pushes to the sockets watching it — a seat's inbox movement
// ([Service.InboxChanged]), an artifact's movement ([Service.ArtifactsMoved]),
// a datastore's changed documents ([Service.ArtifactDataMoved]) and a chat
// record ([ChatHub.fan]) — timed from the broker's own instant for the record
// to the push.
//
// AT THE PUSH, not at each socket's write: the push is the last thing the
// engine does for every viewer at once, and what a socket's writer waits on
// after it is the viewer's own network, which the objective's bound is not
// the engine's to promise (as the ready report keeps the browser's share
// apart). And only a push some socket was handed is an event: a change nobody
// was watching was shown to nobody, late or not.
//
// FROM THE BROKER'S INSTANT, not this node's commit, so a batch a peer applied
// and the relay carried here is timed as its viewer waited for it — the lag of
// whichever applier announced it included — and so every node's figure for one
// record starts from the same instant ([objectives.Meter.RecordSince] absorbs
// the clocks' skew).
//
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
