package runner

import "github.com/crewlet/crewlet/internal/provenance"

// Rotated is a run whose job ended refused by its vendor on a SIGN-IN THAT WAS
// REPLACED WHILE IT RAN: what a resume carries the round on from
// ([Resume.Rotated]).
//
// # Why the round is carried on, not collected
//
// A process's environment cannot change, and the coding CLIs re-read no file
// for a token, so a job that was running when its entry's credential was
// rotated keeps the old one until it ends. Where the vendor revoked the old
// one, the job ends refused, though the entry already signs in with the new
// credential. Judged as the round's outcome, that refusal would fail the turn
// for a reason the round no longer has. So the resumed turn relaunches its
// executor for the same round, on the entry that ran it, which now hands the
// new credential to every launch: in the job's box, continuing its own CLI
// session where the launch can, so the work in its working tree and its
// conversation carry on.
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
	// SessionID is the refused job's CLI session, which the relaunch
	// continues where it runs in that job's box.
	SessionID string

	// BoxKept is whether that job's box is still there.
	BoxKept bool
}

// RotatedContinuation is what the refused job's own session is told when it is
// continued in its box.
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
func (r *Rotated) continuation(provenance.Boundary) string { return RotatedContinuation }
func (r *Rotated) reseed(provenance.Boundary) string       { return r.Reseed() }
func (r *Rotated) refusal() error                          { return nil }
func (r *Rotated) what() string {
	return "carrying on the round a replaced sign-in cut off"
}

// isRotated reports whether a carried round is one a replaced sign-in cut off.
func isRotated(c carried) bool {
	_, rotated := c.(*Rotated)
	return rotated
}
