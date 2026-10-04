package engine

import "github.com/crewlet/crewlet/internal/iamdomain"

// SetOnIdentityMoved registers what this node does when the identity estate
// moved somebody's credentials — which, beside the engine, is the API deciding
// again the dashboard sockets held open on them.
//
// # How a revocation reaches a tab that is already open
//
// A socket is authenticated once, at its handshake, and then held for as long
// as the tab is: a session ended, a person suspended, a grant narrowed or a
// machine token revoked has to reach it some other way, or a revocation this
// engine arbitrates across a fleet in under a second would stop at the one
// surface a browser keeps open. The identity applier says, after every
// committed batch, whose credentials it moved ([iamdomain.Moved]), and this is
// where the engine hands that on. EVERY NODE APPLIES EVERY RECORD of the
// identity log, whatever its roles, so every node that serves sockets hears
// every move from its own applier and closes its own sockets — nothing is
// forwarded between nodes, and a node that serves none simply has nobody
// registered here.
//
// A SETTER for [Engine.SetOnCompanyPublished]'s reason: the API half is built
// after the engine. Safe to call while the engine is running, and safe to leave
// unset.
//
// fn IS CALLED ON THE APPLY LOOP'S OWN GOROUTINE, with the next batch waiting
// behind it, so it must not block: the socket registry it is built for only
// signals.
func (e *Engine) SetOnIdentityMoved(fn func(iamdomain.Moved)) {
	e.onIdentity.Store(&fn)
}

// identityMoved is what the identity applier calls after a committed batch,
// with what that batch moved.
//
// TWO LISTENERS, each handed its half. A SEAT'S STANDING is the party
// registry's — see [Engine.nudgeDirectory], which this signals rather than
// runs. WHOSE CREDENTIALS is whatever [Engine.SetOnIdentityMoved] registered,
// and a batch that moved none of them — a sign-in, the bulk of this log's
// traffic — reaches it not at all.
func (e *Engine) identityMoved(moved iamdomain.Moved) {
	if moved.Seats {
		e.nudgeDirectory()
	}
	if !moved.Credentials() {
		return
	}
	fn := e.onIdentity.Load()
	if fn == nil || *fn == nil {
		return
	}
	(*fn)(moved)
}

// estateReplaced tells the identity estate's listeners that every one of its
// rows may have moved with no committed batch to say so — an adoption that
// replaced the replicated file with a donor's, or an estate reopened on a file
// a failed join installed.
//
// EVERYTHING, because nothing narrower is honest: the rows are a peer's now,
// and every revocation that peer applied while this node was too far behind to
// follow arrived without an apply. An open socket still serving a session one
// of them ended would go on serving it until it closed on its own — so the
// move names everyone ([iamdomain.Moved.Everyone]), which closes every socket
// for its handshake to decide, and the party registry is rebuilt at once
// rather than at its next periodic re-read.
func (s *stateLog) estateReplaced() {
	if s.identityMoved != nil {
		s.identityMoved(iamdomain.Moved{Seats: true, Everyone: true})
	}
}
