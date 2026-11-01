package executordomain

import (
	"slices"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/textcut"
)

// THE ENDING: the engine's account of how a turn ended, which a coding CLI's
// transcript cannot know — the reviewer's verdict, whether a reply reached
// anybody, who stopped the turn and why, what guard or loss ended it. The next
// turn on the conversation is told it beside the session it continues, so a
// turn that resumes a transcript in which it was about to reply knows whether
// the reply went out.
//
// EVERY FIELD IS OPTIONAL AND BOUNDED: the engine fills what the way a turn
// ended establishes, and the writer refuses an engine word past its bound and
// cuts model-written prose to its own ([Ending.fit]), so an ending never makes
// its record larger than the log's declared bound.

// Outcome is how a turn ended, in the engine's four words.
type Outcome string

const (
	// OutcomeCompleted is a turn its reviewer closed.
	OutcomeCompleted Outcome = "completed"

	// OutcomeFailed is a turn a guard, a budget or a refusal ended.
	OutcomeFailed Outcome = "failed"

	// OutcomeStopped is a turn a person or a duty stopped.
	OutcomeStopped Outcome = "stopped"

	// OutcomeLost is a turn whose executor was lost with its box or its
	// host and not carried on.
	OutcomeLost Outcome = "lost"
)

// Outcomes are the four.
var Outcomes = []Outcome{OutcomeCompleted, OutcomeFailed, OutcomeStopped, OutcomeLost}

// Valid reports whether an outcome off the wire is one this build knows.
func (o Outcome) Valid() bool { return slices.Contains(Outcomes, o) }

// Ending is how one turn ended.
type Ending struct {
	// Outcome is the engine's word for it. Required.
	Outcome Outcome `json:"outcome"`

	// Decision is the reviewer's final decision word, empty where no
	// review ran.
	Decision string `json:"decision,omitempty"`

	// Delivered reports that the turn's reply reached the party waiting on
	// it, and DeliveredTo is the surface it was delivered on (a chat
	// backend, a tracker, `a2a` for a colleague's channel). Answer is the
	// engine's own outcome of an agent-to-agent ask the turn answered,
	// where it answered one.
	Delivered   bool   `json:"delivered,omitempty"`
	DeliveredTo string `json:"delivered_to,omitempty"`
	Answer      string `json:"answer,omitempty"`

	// CompletedWork is the reviewer's account of what landed: model-written
	// prose, somebody else's words to the next turn that reads it.
	CompletedWork string `json:"completed_work,omitempty" untrusted:"fence"`

	// BlockedOn is the executor's own evidence of what stopped a blocked
	// round: model-written prose, fenced like the reviewer's.
	BlockedOn string `json:"blocked_on,omitempty" untrusted:"fence"`

	// StoppedBy is who stopped the turn (a person's login, a seat's handle,
	// a machine's name), and StopReason the reason they gave, in their own
	// words.
	StoppedBy  string `json:"stopped_by,omitempty" untrusted:"name"`
	StopReason string `json:"stop_reason,omitempty" untrusted:"fence"`

	// Failure is the engine's own account of the guard, the budget or the
	// loss that ended the turn. Its words are the engine's, and fenced all
	// the same: a guard's account quotes the tool or the error it tripped
	// on, which came from a box.
	Failure string `json:"failure,omitempty" untrusted:"fence"`
}

// validate refuses an ending the engine could not mean: an unknown outcome,
// an engine word past [WordMaxBytes] or not UTF-8. Prose is not refused here;
// [Ending.fit] cuts it.
func (e Ending) validate() error {
	if !e.Outcome.Valid() {
		return invalid("the ending's outcome %q is not one of %v", e.Outcome, Outcomes)
	}
	for _, word := range []struct {
		field, value string
		max          int
	}{
		{"decision", e.Decision, WordMaxBytes},
		{"delivered_to", e.DeliveredTo, WordMaxBytes},
		{"answer", e.Answer, WordMaxBytes},
		{"stopped_by", e.StoppedBy, ActorMaxBytes},
	} {
		if err := checkText("the ending's "+word.field, word.value, word.max); err != nil {
			return err
		}
	}
	for _, prose := range []struct{ field, value string }{
		{"completed_work", e.CompletedWork}, {"blocked_on", e.BlockedOn},
		{"stop_reason", e.StopReason}, {"failure", e.Failure},
	} {
		if !utf8.ValidString(prose.value) {
			return invalid("the ending's %s is not valid UTF-8", prose.field)
		}
	}
	return nil
}

// fit is the ending with every prose field cut to its bound on a rune
// boundary, marked where it was cut ([textcut.Within], so the marker is inside
// the bound the record is held to).
func (e Ending) fit() Ending {
	e.CompletedWork = textcut.Within(e.CompletedWork, CompletedWorkMaxBytes)
	e.BlockedOn = textcut.Within(e.BlockedOn, BlockedOnMaxBytes)
	e.StopReason = textcut.Within(e.StopReason, StopReasonMaxBytes)
	e.Failure = textcut.Within(e.Failure, FailureMaxBytes)
	return e
}

// within reports whether every field of the ending is inside its bound: what
// the applier holds a record to before it writes one through, so a writer
// that skipped [Ending.fit] writes nothing rather than an unbounded row.
func (e Ending) within() bool {
	return e.Outcome.Valid() &&
		len(e.Decision) <= WordMaxBytes && len(e.DeliveredTo) <= WordMaxBytes &&
		len(e.Answer) <= WordMaxBytes && len(e.StoppedBy) <= ActorMaxBytes &&
		len(e.CompletedWork) <= CompletedWorkMaxBytes &&
		len(e.BlockedOn) <= BlockedOnMaxBytes &&
		len(e.StopReason) <= StopReasonMaxBytes && len(e.Failure) <= FailureMaxBytes
}
