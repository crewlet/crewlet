package engine

import "github.com/crewlet/crewlet/internal/tracker"

// SetOnInboxMoved registers what this node does when a committed tracker batch
// moved somebody's inbox — which, beside the engine, is the API pushing an
// `inbox_changed` frame to the sockets watching that seat.
//
// # How a person learns they have work
//
// An agent is woken by the change feed. A person has no turn to wake, and a
// Crewlet-only person has no Slack or Jira either: the dashboard is their
// delivery surface. The tracker's applier says, after every committed batch,
// whose inbox it moved (internal/tracker/inboxmove.go), and this is where the
// engine hands that on. EVERY NODE APPLIES EVERY RECORD, so every node that
// serves sockets hears every movement from its own applier and tells its own
// sockets — nothing is forwarded between nodes, and a node that serves none
// simply has nobody registered here.
//
// A SETTER for [Engine.SetOnCompanyPublished]'s reason: the API half is built
// after the engine. Safe to call while the engine is running, and safe to leave
// unset.
//
// fn IS CALLED ON THE APPLY LOOP'S OWN GOROUTINE, with the next batch waiting
// behind it, so it must not block: the stream's fan-out it is built for never
// does.
func (e *Engine) SetOnInboxMoved(fn func([]tracker.InboxMovement)) {
	e.onInbox.Store(&fn)
}

// inboxMoved is what the tracker applier calls after a committed batch that
// wrote somebody a notice.
func (e *Engine) inboxMoved(moved []tracker.InboxMovement) {
	if fn := e.onInbox.Load(); fn != nil && *fn != nil {
		(*fn)(moved)
	}
}
