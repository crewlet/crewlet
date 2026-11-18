package engine

import (
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// SetOnIdentityMoved registers what this node does when the identity estate
// moved somebody's credentials: the API deciding again the dashboard sockets
// held open on them, since a socket is authenticated once, at its handshake,
// and a revocation must reach an open tab some other way. The identity applier
// says after every committed batch whose credentials it moved
// ([iamdomain.Moved]). EVERY DATA NODE APPLIES EVERY identity RECORD, whatever
// its company, and every socket server is a data node (ingress requires `data`),
// so each closes its own sockets from its own applier; nothing is forwarded.
//
// A setter for [Engine.SetOnCompanyPublished]'s reason; safe to call while the
// engine runs, and to leave unset. fn runs ON THE APPLY LOOP'S GOROUTINE with
// the next batch behind it, so it must not block.
func (e *Engine) SetOnIdentityMoved(fn func(iamdomain.Moved)) {
	e.onIdentity.Store(&fn)
}

// identityMoved is what the identity applier calls after a committed batch,
// with what it moved. A SEAT'S STANDING goes to the party registry
// ([Engine.nudgeDirectory], signalled rather than run); WHOSE CREDENTIALS to
// what [Engine.SetOnIdentityMoved] registered, which a batch that moved none (a
// sign-in, the bulk of this log's traffic) never reaches.
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

// estateReplaced tells the replicated estate's listeners that every row may
// have moved with no committed batch to say so: an adoption installed a donor's
// file, or the estate reopened on a file a failed join installed. Every
// revocation, share, trash and purge the donor applied arrived without an
// apply, so nothing narrower is honest: the identity move names everyone
// ([iamdomain.Moved.Everyone]), closing every socket for its handshake to
// decide, and rebuilds the party registry now rather than at its next re-read;
// the artifacts estate's and the datastore's listeners are told everything
// moved ([artifactsdomain.MoveReset], [artifactdata.Moved.Reset]); and so is
// what a forward walk derived from the rows, which cannot see a row it already
// passed being replaced ([Engine.estateReplaced]).
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
// under it: the chat index drops what it holds and rewalks, the index of
// changes' words rewalks over what it keeps ([search.ChatIndexer.RequestRebuild],
// [search.ChangeIndexer.RequestRebuild]), or a document indexed from the
// replaced copy answers searches indefinitely. The knowledge base's lexical
// index laps by id and repairs drift itself.
//
// THE NUDGE, NOT THE GUARANTEE: the adopter already named the new copy
// ([statelog.EstateInstance]), which each index compares on every sweep, so a
// process stopped first is still rebuilt; this call makes searches stop
// trusting the index now. A node with no company, or whose chat is not the
// engine's own and which hosts no code, holds neither index.
func (e *Engine) estateReplaced() {
	n := e.native.Load()
	if n == nil {
		return
	}
	if n.chatIndex != nil {
		n.chatIndex.RequestRebuild()
	}
	if n.changeIndex != nil {
		n.changeIndex.RequestRebuild()
	}
}
