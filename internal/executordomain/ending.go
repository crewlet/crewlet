package executordomain

import (
	"slices"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/words"
)

// THE ENDING: the engine's account of how a turn ended, which a harness's
// transcript cannot know — the reviewer's verdict, whether a reply reached
// anybody, who stopped the turn and why, what guard or loss ended it. The next
// turn on the conversation is told it beside the session it continues, so a
// turn that resumes a transcript in which it was about to reply knows whether
// the reply went out.
//
// EVERY FIELD IS OPTIONAL AND BOUNDED: the engine fills what the way a turn
// ended establishes, and the writer refuses an engine word past its bound and
// cuts model-written prose to its own ([Ending.Fit]), so an ending never makes
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

	// DeliveryUnknown says the engine cannot tell whether the reply went:
	// the turn's record of its calls is incomplete — calls were elided from
	// it to keep it bounded — and the calls it kept show no delivery. Never
	// set beside Delivered. A reader tells the next turn it is not known,
	// rather than that the reply did not go.
	DeliveryUnknown bool `json:"delivery_unknown,omitempty"`

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

	// Unkept is the turn's NEWEST capture where it is not one the turn's row
	// names: the object store or the log refused it, or its record was never
	// answered. Nil where the newest capture landed, or the turn took none.
	//
	// THE END IS WHERE IT IS SAID, because the run's record that knew it is
	// deleted with the turn, and the turn's row would otherwise go on naming
	// an older capture of the turn — one that does not hold what the turn's
	// later launch did — as the turn's last word, for whatever continues the
	// conversation's session to take for it. The apply takes that older
	// object off the row ([applyEnd]), so the row names no session of the
	// turn, and the end says why.
	Unkept *Unkept `json:"unkept,omitempty"`
}

// Unkept is a capture of a turn's session that no row names: its place among
// the turn's captures, and why it was not kept.
type Unkept struct {
	// LaunchSeq and CaptureSeq are its place, the ordinals every capture of
	// the turn carries ([Capture.LaunchSeq]).
	LaunchSeq  int64 `json:"launch_seq"`
	CaptureSeq int64 `json:"capture_seq"`

	// Reason is why, in the engine's words, which quote what refused it — an
	// error a box or a store wrote — and so are fenced.
	Reason string `json:"reason" untrusted:"fence"`
}

// supersedes reports whether u is a later capture than the one at (launch,
// capture): the row's newest is then older than the turn's last attempt.
func (u Unkept) supersedes(launch, capture int64) bool {
	return newer(u.LaunchSeq, u.CaptureSeq, launch, capture)
}

// validate refuses an ending the engine could not mean: an unknown outcome,
// an engine word past [WordMaxBytes] or not UTF-8. Prose is not refused here;
// [Ending.Fit] cuts it.
func (e Ending) validate() error {
	if !e.Outcome.Valid() {
		return invalid("the ending's outcome %q is not one of %s", e.Outcome, words.Or(Outcomes))
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
	prose := []struct{ field, value string }{
		{"completed_work", e.CompletedWork}, {"blocked_on", e.BlockedOn},
		{"stop_reason", e.StopReason}, {"failure", e.Failure},
	}
	if u := e.Unkept; u != nil {
		if u.LaunchSeq <= 0 || u.CaptureSeq <= 0 {
			return invalid("the ending's unkept capture is placed at (%d, %d), and every capture "+
				"is placed from (1, 1)", u.LaunchSeq, u.CaptureSeq)
		}
		prose = append(prose, struct{ field, value string }{"unkept reason", u.Reason})
	}
	for _, p := range prose {
		if !utf8.ValidString(p.value) {
			return invalid("the ending's %s is not valid UTF-8", p.field)
		}
	}
	return nil
}

// Fit is the ending with every prose field cut to its bound on a rune
// boundary, marked where it was cut ([textcut.Within], so the marker is inside
// the bound the record is held to).
//
// EXPORTED FOR THE OTHER RECORD AN ENDING RIDES before this log's: a turn's
// end is owed on its run's row until the run's ending records it here
// (internal/sandbox's OweEnd), and that row is held to a byte budget that
// counts an owed end at this log's bound — so the ending is fitted where it
// is owed, not first where it is recorded.
func (e Ending) Fit() Ending {
	e.CompletedWork = textcut.Within(e.CompletedWork, CompletedWorkMaxBytes)
	e.BlockedOn = textcut.Within(e.BlockedOn, BlockedOnMaxBytes)
	e.StopReason = textcut.Within(e.StopReason, StopReasonMaxBytes)
	e.Failure = textcut.Within(e.Failure, FailureMaxBytes)
	if e.Unkept != nil {
		unkept := *e.Unkept
		unkept.Reason = textcut.Within(unkept.Reason, UnkeptReasonMaxBytes)
		e.Unkept = &unkept
	}
	return e
}

// within reports whether every field of the ending is inside its bound: what
// the applier holds a record to before it writes one through, so a writer
// that skipped [Ending.Fit] writes nothing rather than an unbounded row.
func (e Ending) within() bool {
	return e.Outcome.Valid() &&
		len(e.Decision) <= WordMaxBytes && len(e.DeliveredTo) <= WordMaxBytes &&
		len(e.Answer) <= WordMaxBytes && len(e.StoppedBy) <= ActorMaxBytes &&
		len(e.CompletedWork) <= CompletedWorkMaxBytes &&
		len(e.BlockedOn) <= BlockedOnMaxBytes &&
		len(e.StopReason) <= StopReasonMaxBytes && len(e.Failure) <= FailureMaxBytes &&
		(e.Unkept == nil || (e.Unkept.LaunchSeq > 0 && e.Unkept.CaptureSeq > 0 &&
			len(e.Unkept.Reason) <= UnkeptReasonMaxBytes))
}
