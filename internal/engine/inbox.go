package engine

import "github.com/crewlet/crewlet/internal/inbox"

// SetOnInboxMoved registers what this node does when a committed batch of a
// [registration.Inbox] domain's applier moved somebody's inbox: beside the
// engine, the API pushing an `inbox_changed` frame to the sockets watching that
// seat, the dashboard being a person's delivery surface. Every data node applies
// every record and every socket-serving node is one, so each tells its own
// sockets what its own appliers moved while its copy of that domain serves; a
// copy out of service has that domain's movements relayed from a sound peer
// (liverelay.go) and its own withheld.
//
// A setter because the API is built after the engine; safe to call while the
// engine runs and to leave unset. fn runs on an apply loop's or relay
// follower's goroutine with the next batch waiting, so it must not block.
func (e *Engine) SetOnInboxMoved(fn func([]inbox.Movement)) {
	e.onInbox.Store(&fn)
}

// SetOnInboxGap registers what this node does when the movements of one source
// it delivers have a GAP (its sockets switched between its own applier and a
// relayed peer's, or between peers): beside the engine, the API telling every
// watched inbox to re-read itself, since the rows are the truth and the gap is
// not replayed. fn is handed the source domain, must not block, and may be left
// unset ([Engine.SetOnInboxMoved]).
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
