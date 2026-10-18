package engine

import (
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

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
// where the engine hands that on. EVERY DATA NODE APPLIES EVERY RECORD of the
// identity log, whatever its company, and every node that serves sockets is a
// data node (ingress requires `data`), so each hears every move from its own
// applier and closes its own sockets — nothing is forwarded between nodes, and
// a node that serves none simply has nobody registered here.
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
	if len(moved.Removed) > 0 {
		e.nudgeAssistantErasure()
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

// estateReplaced tells the replicated estate's listeners that every one of its
// rows may have moved with no committed batch to say so — an adoption that
// replaced the replicated file with a donor's, or an estate reopened on a file
// a failed join installed.
//
// THE IDENTITY ESTATE'S, EVERYTHING, because nothing narrower is honest: the
// rows are a peer's now, and every revocation that peer applied while this
// node was too far behind to follow arrived without an apply. An open socket
// still serving a session one of them ended would go on serving it until it
// closed on its own — so the move names everyone ([iamdomain.Moved.Everyone]),
// which closes every socket for its handshake to decide, and the party
// registry is rebuilt at once rather than at its next periodic re-read.
//
// AND THE ARTIFACTS ESTATE'S LISTENERS, for the same reason: every share,
// trash and purge the donor applied arrived here without an apply, so an
// authorization verdict or a room decided from the old rows is told that
// everything moved ([artifactsdomain.MoveReset]), and the datastore's for the
// same reason ([artifactdata.Moved.Reset]).
//
// AND WHAT A FORWARD WALK DERIVED FROM THE ROWS, which cannot see a row it has
// already passed being replaced ([Engine.estateReplaced]).
func (s *stateLog) estateReplaced() {
	if s.identityMoved != nil {
		s.identityMoved(iamdomain.Moved{Seats: true, Everyone: true})
	}
	if s.artifactsMoved != nil {
		s.artifactsMoved([]artifactsdomain.Moved{artifactsdomain.Reset()})
	}
	if s.artifactDataMoved != nil {
		s.artifactDataMoved([]artifactdata.Moved{artifactdata.ResetMove()})
	}
	if s.replaced != nil {
		s.replaced()
	}
}

// estateReplaced is what this node does once its replicated estate was replaced
// under it: the chat index, whose walk only moves forward over the
// transcript's own versions, drops what it holds and walks the rows it now has
// ([search.ChatIndexer.RequestRebuild]) — a document indexed from the copy the
// adoption replaced would otherwise answer searches for as long as the
// deployment lived. The knowledge base's lexical index needs nothing: its walk
// laps by id and repairs a drifted row where it finds one.
//
// A NODE THAT HAS MET NO COMPANY, or one whose chat is not the engine's own,
// holds no chat index and has nothing to rebuild.
func (e *Engine) estateReplaced() {
	if n := e.native.Load(); n != nil && n.chatIndex != nil {
		n.chatIndex.RequestRebuild()
	}
}
