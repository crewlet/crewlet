package engine

import "github.com/crewlet/crewlet/internal/inbox"

// SetOnInboxMoved registers what this node does when a committed batch of a
// domain's applier moved somebody's inbox — which, beside the engine, is the
// API pushing an `inbox_changed` frame to the sockets watching that seat.
//
// # How a person learns they have work
//
// An agent is woken by a change feed. A person has no turn to wake, and a
// Crewlet-only person has no Slack or Jira either: the dashboard is their
// delivery surface. Each domain whose changes are a person's news — the
// tracker's, marked [registration.Inbox] — has its applier say, after every
// committed batch, whose inbox it moved (internal/inbox's movement.go), each
// movement naming that domain as its source, and this is where the engine
// hands that on. EVERY DATA NODE APPLIES EVERY RECORD, and every node that
// serves sockets is a data node (ingress requires `data`), so each hears every
// movement from its own appliers and tells its own sockets — WHILE ITS COPY OF
// THAT DOMAIN SERVES. A copy out of service keeps its node in rotation, and its
// applier is the one thing on that node nobody vouches for, so for as long as
// it is out that domain's movements come from a sound peer's applier instead,
// relayed (liverelay.go), and its own are withheld — that domain's alone:
// every other domain's copy still serves, and its applier is still heard. A
// node that serves no sockets simply has nobody registered here.
//
// A SETTER for [Engine.SetOnCompanyPublished]'s reason: the API half is built
// after the engine. Safe to call while the engine is running, and safe to leave
// unset.
//
// fn IS CALLED ON AN APPLY LOOP'S OWN GOROUTINE, or a relay follower's, with
// the next batch waiting behind it, so it must not block: the stream's fan-out
// it is built for never does.
func (e *Engine) SetOnInboxMoved(fn func([]inbox.Movement)) {
	e.onInbox.Store(&fn)
}

// SetOnInboxGap registers what this node does when the movements of one
// source it delivers have a GAP — its sockets switched from its own applier to
// a relayed peer's, back, or from one peer to another — which, beside the
// engine, is the API telling every watched inbox to read itself again. What
// moved in the gap is not replayed: the rows are the truth, and a screen
// re-reads them. fn is handed the source, the domain whose movements switched.
//
// A SETTER, safe to leave unset, and fn must not block, for
// [Engine.SetOnInboxMoved]'s reasons.
func (e *Engine) SetOnInboxGap(fn func(source string)) {
	e.onInboxGap.Store(&fn)
}

// inboxMoved is what a domain's applier calls after a committed batch that
// moved somebody's inbox: announced to this node's sockets and kept for its
// relay followers, one source of each domain's movements at a time
// ([Engine.announce]).
func (e *Engine) inboxMoved(moved []inbox.Movement) {
	e.announce(liveBatch{Inbox: moved})
}

// inboxListening reports whether anything on this node registered for its
// inbox movements — the API, on a node that serves one.
func (e *Engine) inboxListening() bool {
	fn := e.onInbox.Load()
	return fn != nil && *fn != nil
}

// deliverInbox hands one batch's movements to whatever this node registered.
func (e *Engine) deliverInbox(moved []inbox.Movement) {
	fn := e.onInbox.Load()
	if fn == nil || *fn == nil {
		return
	}
	(*fn)(moved)
}

// inboxGap tells whatever this node registered that its movements from source
// have a gap.
func (e *Engine) inboxGap(source string) {
	fn := e.onInboxGap.Load()
	if fn == nil || *fn == nil {
		return
	}
	(*fn)(source)
}
