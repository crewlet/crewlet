package runner

import (
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// Refused is a run whose job ended on a request its provider refused as built
// (sandbox.Result.RequestRefused) in a session the job CONTINUED: what a
// resume carries the round on from ([Resume.Refused]).
//
// A continued session resends what it holds, so one holding what the provider
// no longer takes (a tool name outside its pattern, a message shape) is
// refused before the model reads a word, which says nothing of the round's
// work. So the session is retired (internal/sandbox's sessionSpent) and the
// executor relaunched for the same round in a fresh session, ONCE: the
// relaunch is recorded as a follow-up, and a follow-up refused again, or a job
// refused in a session it started, is collected and ends the turn with the
// provider's message ([Resume.RequestRefused]) rather than relaunching forever.
type Refused struct {
	// Said is the provider's message, redacted.
	Said string

	// BoxKept is whether the refused job's box is still there.
	BoxKept bool
}

// Reseed is what the fresh run carrying the round on is told beside its brief.
func (r Refused) Reseed() string {
	out := "## A run before you on this task could not start its work\n\n" +
		"It continued an earlier session of this conversation, and the model's provider refused " +
		"every request that session made, so it did nothing. That session is retired and you are " +
		"in a new one: what earlier turns did reaches you through this brief, not a transcript.\n\n"
	if r.BoxKept {
		out += "If a working tree is in front of you, read `git status`, `git diff` and `git log` " +
			"before anything else.\n\n"
	}
	return out + "Do the task as it asks."
}

// A refused round continues no session: the session is what was refused. Its
// words are the engine's own, so no fence token touches them, and no later
// brief retells them ([carried.lead]): the refused run did no work to lack.
func (r *Refused) session() string                         { return "" }
func (r *Refused) boxKept() bool                           { return r.BoxKept }
func (r *Refused) continuation(provenance.Boundary) string { return r.Reseed() }
func (r *Refused) reseed(provenance.Boundary) string       { return r.Reseed() }
func (r *Refused) tellsHeld() (id, tool string)            { return "", "" }
func (r *Refused) tellsCalled() []types.CallReturned       { return nil }
func (r *Refused) lead() *sandbox.Lead                     { return nil }
func (r *Refused) keeps() bool                             { return false }
func (r *Refused) branch() string                          { return "" }
func (r *Refused) refusal() error                          { return nil }
func (r *Refused) what() string {
	return "carrying on the round whose continued session its provider refused"
}

// isRefused reports whether a carried round is one a refused session cut off.
func isRefused(c carried) bool {
	_, refused := c.(*Refused)
	return refused
}
