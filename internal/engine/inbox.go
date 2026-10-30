package engine

import "github.com/crewlet/crewlet/internal/worklet"

// SetOnInboxMoved registers what this node does when a committed tracker batch
// moved somebody's inbox — which, beside the engine, is the API pushing an
// `inbox_changed` frame to the sockets watching that seat.
//
// # How a person learns they have work
//
// An agent is woken by the change feed. A person has no turn to wake, and a
// Crewlet-only person has no Slack or Jira either: the dashboard is their
// delivery surface. The tracker's applier says, after every committed batch,
// whose inbox it moved (internal/worklet/inboxmove.go), and this is where the
// engine hands that on. EVERY DATA NODE APPLIES EVERY RECORD, and every node
// that serves sockets is a data node (ingress requires `data`), so each hears
// every movement from its own applier and tells its own sockets — WHILE ITS
// COPY SERVES. A copy out of service keeps its node in rotation, and its
// applier is the one thing on that node nobody vouches for, so for as long as
// it is out the movements come from a sound peer's applier instead, relayed
// (liverelay.go), and its own are withheld. A node that serves no sockets
// simply has nobody registered here.
//
// A SETTER for [Engine.SetOnCompanyPublished]'s reason: the API half is built
// after the engine. Safe to call while the engine is running, and safe to leave
// unset.
//
// fn IS CALLED ON THE APPLY LOOP'S OWN GOROUTINE, or the relay follower's, with
// the next batch waiting behind it, so it must not block: the stream's fan-out
// it is built for never does.
func (e *Engine) SetOnInboxMoved(fn func([]worklet.InboxMovement)) {
	e.onInbox.Store(&fn)
}

// SetOnInboxGap registers what this node does when the movements it delivers
// have a GAP — its sockets switched from its own applier to a relayed peer's,
// back, or from one peer to another — which, beside the engine, is the API
// telling every watched inbox to read itself again. What moved in the gap is
// not replayed: the rows are the truth, and a screen re-reads them.
//
// A SETTER, safe to leave unset, and fn must not block, for
// [Engine.SetOnInboxMoved]'s reasons.
func (e *Engine) SetOnInboxGap(fn func()) {
	e.onInboxGap.Store(&fn)
}

// inboxMoved is what the tracker applier calls after a committed batch that
// wrote somebody a notice: announced to this node's sockets and kept for its
// relay followers, one source at a time ([Engine.announce]).
func (e *Engine) inboxMoved(moved []worklet.InboxMovement) {
	e.announce(liveBatch{Inbox: moved})
}

// inboxListening reports whether anything on this node registered for its
// inbox movements — the API, on a node that serves one.
func (e *Engine) inboxListening() bool {
	fn := e.onInbox.Load()
	return fn != nil && *fn != nil
}

// deliverInbox hands one batch's movements to whatever this node registered.
func (e *Engine) deliverInbox(moved []worklet.InboxMovement) {
	fn := e.onInbox.Load()
	if fn == nil || *fn == nil {
		return
	}
	(*fn)(moved)
}

// inboxGap tells whatever this node registered that its movements have a gap.
func (e *Engine) inboxGap() {
	fn := e.onInboxGap.Load()
	if fn == nil || *fn == nil {
		return
	}
	(*fn)()
}
