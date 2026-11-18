package engine

import (
	"sync"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/queue"
)

// seatInboxes is the attachment of each seat's inbox this node is taking or
// holds ([queue.Attachment]), by the seat's id, which every hold this engine
// takes on the inbox names: the sandbox coordinator's, a person's pause, a
// park, the no-model pause.
//
// RECORDED AS THE SEAT IS TAKEN ([Engine.prepareSeat], before the attach) AND
// DROPPED AS IT IS RELEASED ([Engine.releaseSeat], after the detach), so a hold
// asked after the detach names the ended attachment, which the queue refuses,
// never the one a retaken seat attaches; a seat this node does not hold names
// the zero attachment. WHO HAS TO AGREE ON IT: this node alone. The package
// doc's "Every hold is taken for one attachment of the inbox" says why.
type seatInboxes struct {
	mu    sync.Mutex
	seats map[uuid.UUID]queue.Attachment
}

// attachInbox records the attachment seat's inbox is taken under.
func (e *Engine) attachInbox(seat uuid.UUID, inbox queue.Attachment) {
	e.inboxes.mu.Lock()
	defer e.inboxes.mu.Unlock()
	if e.inboxes.seats == nil {
		e.inboxes.seats = map[uuid.UUID]queue.Attachment{}
	}
	e.inboxes.seats[seat] = inbox
}

// detachInbox forgets seat's attachment, for a seat this node has released.
func (e *Engine) detachInbox(seat uuid.UUID) {
	e.inboxes.mu.Lock()
	defer e.inboxes.mu.Unlock()
	delete(e.inboxes.seats, seat)
}

// inboxOf is the attachment of seat's inbox this node holds, and the zero
// attachment, which every hold refuses, for a seat it does not.
func (e *Engine) inboxOf(seat uuid.UUID) queue.Attachment {
	e.inboxes.mu.Lock()
	defer e.inboxes.mu.Unlock()
	return e.inboxes.seats[seat]
}
