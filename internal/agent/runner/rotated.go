package runner

import (
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// Rotated is a run whose job its vendor refused on a sign-in REPLACED WHILE IT
// RAN: a resume carries its round on ([Resume.Rotated]). A process's
// environment cannot change and harnesses re-read no token file, so the job
// kept the revoked credential; judging that refusal the round's outcome would
// fail the turn for a reason it no longer has. The relaunch runs on the same
// entry, now handing out the new credential, continuing the job's harness
// session where it can (continued.go).
//
// ONCE: the relaunch is recorded as a follow-up for this reason, and one
// refused again is an ordinary refusal (internal/engine's signInReplaced), so a
// failing credential fails the turn rather than looping.
// Unless the round was already submitted, as for [Lost]: then it is collected.
type Rotated struct {
	// SessionID is the refused job's harness session, continued in place where
	// the relaunch runs in that job's box with no kept capture to restore
	// (continued.go).
	SessionID string

	// BoxKept is whether that job's box is still there.
	BoxKept bool
}

// RotatedContinuation is what the refused job's own session is told when it is
// continued, in its box or restored into another.
const RotatedContinuation = "Your previous segment ended because the model's sign-in was " +
	"replaced while it ran; continue from where you are."

// Reseed is what a fresh run carrying the round on is told beside its brief,
// where the refused job's session could not be continued.
func (r Rotated) Reseed() string {
	out := "## A run before you on this task was cut off\n\n" +
		"It was working on this task when the model's sign-in was replaced, and the vendor " +
		"refused the credential it had started with, so it stopped before it finished.\n\n"
	if r.BoxKept {
		out += "If its working tree is in front of you, read `git status`, `git diff` and " +
			"`git log` before anything else, and carry on from there. Otherwise look for a " +
			"work-in-progress branch it may have pushed.\n\n"
	} else {
		out += "Look for a work-in-progress branch it may have pushed, and carry on from that " +
			"if there is one.\n\n"
	}
	return out + "What it did through your tools before it stopped did happen: check before " +
		"doing any of it again. Then finish the task as it asks."
}

// A rotated round's words are the engine's own, which no fence token touches.
func (r *Rotated) session() string                         { return r.SessionID }
func (r *Rotated) boxKept() bool                           { return r.BoxKept }
func (r *Rotated) continuation(provenance.Boundary) string { return RotatedContinuation }
func (r *Rotated) reseed(provenance.Boundary) string       { return r.Reseed() }
func (r *Rotated) tellsHeld() (id, tool string)            { return "", "" }
func (r *Rotated) tellsCalled() []types.CallReturned       { return nil }
func (r *Rotated) lead() *sandbox.Lead                     { return nil }
func (r *Rotated) keeps() bool                             { return true }
func (r *Rotated) branch() string                          { return "" }
func (r *Rotated) refusal() error                          { return nil }
func (r *Rotated) what() string {
	return "carrying on the round a replaced sign-in cut off"
}

// isRotated reports whether a carried round is one a replaced sign-in cut off.
func isRotated(c carried) bool {
	_, rotated := c.(*Rotated)
	return rotated
}
