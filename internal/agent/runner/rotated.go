package runner

import (
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// Rotated is a run whose job ended refused by its vendor on a SIGN-IN THAT WAS
// REPLACED WHILE IT RAN: what a resume carries the round on from
// ([Resume.Rotated]).
//
// # Why the round is carried on, not collected
//
// A process's environment cannot change, and the harnesses re-read no file
// for a token, so a job that was running when its entry's credential was
// rotated keeps the old one until it ends. Where the vendor revoked the old
// one, the job ends refused, though the entry already signs in with the new
// credential. Judged as the round's outcome, that refusal would fail the turn
// for a reason the round no longer has. So the resumed turn relaunches its
// executor for the same round, on the entry that ran it, which now hands the
// new credential to every launch, continuing the job's harness session where
// the launch can — the turn's kept capture of it, in whatever box the launch
// gets (continued.go), or the copy the job's own box holds — so its
// conversation carries on, and its working tree too where the box is the job's.
//
// ONCE. The relaunched job is recorded as a follow-up for this reason, and a
// follow-up refused again is an ordinary refusal (the engine decides; see
// internal/engine's signInReplaced), so a credential that keeps failing fails
// the turn rather than relaunching it for ever.
//
// UNLESS THE ROUND WAS ALREADY SUBMITTED, as for [Lost]: a job whose
// `submit_work` is on the round's calls had finished its work, and the round
// is collected from those calls.
type Rotated struct {
	// SessionID is the refused job's harness session, which the relaunch
	// continues in place where it runs in that job's box and the turn kept no
	// capture of it to restore (continued.go).
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
func (r *Rotated) tellsHeld() string                       { return "" }
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
