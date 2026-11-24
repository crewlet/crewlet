package executorkeep

import (
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/redact"
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
// ended establishes, and the executor log's writer refuses an engine word past
// its bound ([Ending.Validate]) and cuts model-written prose to its own
// ([Ending.Fit]), so an ending never makes its record larger than the log's
// declared bound.

// Outcome is how a turn ended, in the engine's four words.
type Outcome string

const (
	// OutcomeCompleted is a turn its reviewer closed.
	OutcomeCompleted Outcome = "completed"

	// OutcomeFailed is a turn a guard, a budget or a refusal ended.
	OutcomeFailed Outcome = "failed"

	// OutcomeStopped is a turn a person or a duty stopped.
	OutcomeStopped Outcome = "stopped"

	// OutcomeLost is a turn NO FRAME OF IT SAW END: the engine ended its run
	// rather than the turn's own loop — its job lost with its box or host and
	// not carried on, a resume that broke or kept failing, a claim stranded, its
	// seat removed — or its end was never recorded, and the retention duty
	// ended it a day after its last record. Nothing says the last of what it
	// did reached its session.
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
	// THE END IS WHERE IT IS SAID: the run's record is deleted with the turn,
	// and the row would go on naming an older capture as the last word. So an
	// end naming one says the turn went on ([Ending.WentOn]); one that does
	// not (an older build's) has the apply take the object off the row, by the
	// record's version (internal/executordomain's applyEnd). DECIDED ON THE
	// ROW'S OWN PLACE, in log order, since the log can have taken a capture the
	// end calls not kept: nothing no newer than the row's place is released
	// (internal/executordomain's Session.Unkept).
	Unkept *Unkept `json:"unkept,omitempty"`

	// WentOn says the turn WENT ON PAST the session its row names: the run's
	// record does not hold its LAST WORD (the final capture of its last launch
	// that ran a job) kept, so the row's session holds what the turn did up to it
	// and nothing after. Written only where true, so an older build has no
	// reader for it (internal/executordomain's record version 4).
	//
	// SAID AT THE END, for Unkept's reason, and HELD TO THE ROW'S OWN PLACE: the
	// log can hold a capture the record calls refused. So the end NAMES ITS LAST
	// WORD ([Ending.LastWord]), and the turn went on only where it names none or
	// the row's place is older (internal/executordomain's Session.WentOn). The
	// row's session is kept, told as a prefix: no unkept attempt beside it takes
	// the object off, and the next turn is told it is not the last word.
	WentOn bool `json:"went_on,omitempty"`

	// LastWord is the place of the turn's LAST WORD (the final capture of its
	// last launch that ran a job) where the end says the turn went on
	// ([Ending.WentOn]) and that capture is the newest: refused, never answered,
	// or begun and never finished. Nil elsewhere: beside an end that does not
	// say it went on, where the last word was never taken (a box already gone,
	// a relaunch that kept nothing), or when it is not the newest capture.
	//
	// The log alone knows whether it took that capture's record all the same,
	// so a reader holds the row's place to it: at or past this place the row
	// holds the last word, whatever went_on says; older, or none, was gone past.
	// Named only beside went_on, so it stamps no end at a version went_on does
	// not (record version 4); placed, never worded: two ordinals.
	LastWord *CapturePlace `json:"last_word,omitempty"`

	// SpentSessions are the sessions a launch of the turn could not go on in,
	// each with how ([SpentSession]): out of its model's context, or refused by
	// its provider. The end is where the fleet is told, for Unkept's reason, and
	// the log's apply retires each from every ended row of the conversation,
	// keeping that loss on the row, so the next turn says it exactly.
	SpentSessions []SpentSession `json:"spent_sessions,omitempty"`

	// NotContinued is what the turn's runs did not continue, and why
	// ([NotContinued]): at most [NotContinuedMaxEntries], its first (what the
	// conversation lost going into the turn) and its newest (what its final
	// session lacks). The reader's surfaces say it in its [NotContinued.Sentence].
	NotContinued []NotContinued `json:"not_continued,omitempty"`

	// Summary names the newest compaction summary a box read of the turn kept
	// ([Summary]): its object, never its words. Nil where none was kept. The
	// log's apply names it on the turn's row, which releases it with the row.
	Summary *Summary `json:"summary,omitempty"`
}

// SpentSession is a session a launch of a turn could not go on in, and how
// ([Loss]): [LossContextExhausted] or [LossRequestRefused] as this build says
// it, a newer build's loss a value held to its bound ([Loss.Within]).
type SpentSession struct {
	Session string `json:"session"`
	Loss    Loss   `json:"loss"`
}

// SpentWhy is why no launch continues session, which a launch of turn spent by
// loss: the one sentence a launch logs and a reader is told.
func SpentWhy(session, turn string, loss Loss) string {
	return fmt.Sprintf("session %s could not go on in a launch of turn %s (%s), and another launch of it "+
		"would meet the same end, so it is retired: no launch continues it, and the conversation carries on "+
		"from how its earlier turns ended", session, turn, loss.Words())
}

// Unkept is a capture of a turn's session that no row names: its place among
// the turn's captures, and why it was not kept.
type Unkept struct {
	// LaunchSeq and CaptureSeq are its place, the ordinals every capture of
	// the turn carries.
	LaunchSeq  int64 `json:"launch_seq"`
	CaptureSeq int64 `json:"capture_seq"`

	// Loss is why, in the one vocabulary ([Loss]): what the turn's next run
	// says it did not continue.
	Loss Loss `json:"loss,omitempty"`

	// Reason is why, in the engine's words, which quote what refused it — an
	// error a box or a store wrote — and so are fenced.
	Reason string `json:"reason" untrusted:"fence"`
}

// CapturePlace is a capture's place among its turn's captures: the ordinals
// every capture of the turn carries, its launch's and its own among that
// launch's, each from 1 — the engine's own numbers, which hold no words.
type CapturePlace struct {
	LaunchSeq  int64 `json:"launch_seq"`
	CaptureSeq int64 `json:"capture_seq"`
}

// Validate refuses an ending the engine could not mean: an unknown outcome,
// an engine word past [WordMaxBytes] or not UTF-8, an unkept capture or a last
// word placed before the first capture, a spent session outside its bounds.
// Prose is not refused here; [Ending.Fit] cuts it. The error names the field
// and what is wrong with it; the executor log's writer answers it as its own
// refusal of an invalid write.
func (e Ending) Validate() error {
	if !e.Outcome.Valid() {
		return fmt.Errorf("the ending's outcome %q is not one of %s", e.Outcome, words.Or(Outcomes))
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
		switch {
		case !utf8.ValidString(word.value):
			return fmt.Errorf("the ending's %s is not valid UTF-8", word.field)
		case len(word.value) > word.max:
			return fmt.Errorf("the ending's %s is %d bytes and the maximum is %d",
				word.field, len(word.value), word.max)
		}
	}
	prose := []struct{ field, value string }{
		{"completed_work", e.CompletedWork}, {"blocked_on", e.BlockedOn},
		{"stop_reason", e.StopReason}, {"failure", e.Failure},
	}
	if u := e.Unkept; u != nil {
		if u.LaunchSeq <= 0 || u.CaptureSeq <= 0 {
			return fmt.Errorf("the ending's unkept capture is placed at (%d, %d), and every capture "+
				"is placed from (1, 1)", u.LaunchSeq, u.CaptureSeq)
		}
		if !u.Loss.Within() {
			return fmt.Errorf("the ending's unkept capture's loss %q is not a word of 1 to %d bytes of UTF-8",
				u.Loss, WordMaxBytes)
		}
		prose = append(prose, struct{ field, value string }{"unkept reason", u.Reason})
	}
	if len(e.NotContinued) > NotContinuedMaxEntries {
		return fmt.Errorf("the ending names %d sessions not continued, and it holds at most %d",
			len(e.NotContinued), NotContinuedMaxEntries)
	}
	for _, n := range e.NotContinued {
		if err := n.Validate(); err != nil {
			return fmt.Errorf("the ending's session not continued: %w", err)
		}
	}
	if w := e.LastWord; w != nil && (w.LaunchSeq <= 0 || w.CaptureSeq <= 0) {
		return fmt.Errorf("the ending's last word is placed at (%d, %d), and every capture is placed "+
			"from (1, 1)", w.LaunchSeq, w.CaptureSeq)
	}
	if s := e.Summary; s != nil {
		if err := s.Validate(); err != nil {
			return fmt.Errorf("the ending's summary: %w", err)
		}
	}
	for _, p := range prose {
		if !utf8.ValidString(p.value) {
			return fmt.Errorf("the ending's %s is not valid UTF-8", p.field)
		}
	}
	if !e.spentWithin() {
		return fmt.Errorf("the ending names %d spent sessions, and it holds at most %d, each a valid "+
			"UTF-8 id of 1 to %d bytes spent by a loss of 1 to %d", len(e.SpentSessions), SpentMaxSessions,
			SessionIDMaxBytes, WordMaxBytes)
	}
	return nil
}

// spentWithin reports whether the ending's spent sessions are inside their
// bounds, each naming its loss. Refused rather than cut: a shortened id names
// another session.
func (e Ending) spentWithin() bool {
	if len(e.SpentSessions) > SpentMaxSessions {
		return false
	}
	for _, spent := range e.SpentSessions {
		if spent.Session == "" || len(spent.Session) > SessionIDMaxBytes || !utf8.ValidString(spent.Session) ||
			!spent.Loss.Within() {
			return false
		}
	}
	return true
}

// Fit is the ending with every prose field cut to its bound as a record keeps
// it ([Excerpt]): redacted, and then cut on a rune boundary, marked where it was
// cut inside the bound the record is held to.
//
// AND WHERE IT IS OWED, before the log records it: a turn's end rides its run's
// row until the run's ending records it here (internal/sandbox's OweEnd), and
// that row is held to a byte budget that counts an owed end with every field
// at its bound, so the sandbox fits it, and refuses one still outside
// [Ending.Within], before the row carries it.
func (e Ending) Fit() Ending {
	e.CompletedWork = Excerpt(e.CompletedWork, CompletedWorkMaxBytes)
	e.BlockedOn = Excerpt(e.BlockedOn, BlockedOnMaxBytes)
	e.StopReason = Excerpt(e.StopReason, StopReasonMaxBytes)
	e.Failure = Excerpt(e.Failure, FailureMaxBytes)
	if e.Unkept != nil {
		unkept := *e.Unkept
		unkept.Reason = Excerpt(unkept.Reason, UnkeptReasonMaxBytes)
		e.Unkept = &unkept
	}
	if e.NotContinued != nil {
		fitted := make([]NotContinued, len(e.NotContinued))
		for i, n := range e.NotContinued {
			fitted[i] = n.Fit()
		}
		e.NotContinued = fitted
	}
	return e
}

// Excerpt is text as a record of the executor log keeps somebody's words (an
// ending's prose, what a turn was asked), at most bound bytes: redacted
// ([redact.Secrets]) and then cut on a rune boundary, marked where it was cut
// inside the bound ([textcut.Within]), and never inside a marker the
// redaction wrote, which a cut moves back from to where it begins
// ([redact.MarkersAround]).
//
// REDACTED BEFORE IT IS CUT: a credential the bound runs across, cut first,
// leaves a head no rule recognises. Only the bound and one marker past it of
// the redaction are kept ([redact.Prefix]), where every marker a cut can fall
// in is whole. A reader still redacts a row's words (internal/executordomain's
// Session.Redacted), reaching rules added since the record was written.
func Excerpt(text string, bound int) string {
	shown, whole := redact.Prefix(text, bound+redact.LongestMarker())
	if whole <= bound {
		return shown
	}
	kept := textcut.Bytes(shown, bound-len(textcut.Opening("", bound)))
	if begin, _, inside := redact.MarkersAround(shown, len(kept)); inside {
		kept = kept[:begin]
	}
	return textcut.Opening(kept, bound)
}

// Within reports whether every field of the ending is inside its bound: what
// the executor log's applier holds a record to before it writes one through,
// so a writer that skipped [Ending.Fit] writes nothing rather than an
// unbounded row. An unkept capture naming no loss is an end stamped before
// losses were (internal/executordomain's record version 7).
func (e Ending) Within() bool {
	return e.Outcome.Valid() &&
		len(e.Decision) <= WordMaxBytes && len(e.DeliveredTo) <= WordMaxBytes &&
		len(e.Answer) <= WordMaxBytes && len(e.StoppedBy) <= ActorMaxBytes &&
		len(e.CompletedWork) <= CompletedWorkMaxBytes &&
		len(e.BlockedOn) <= BlockedOnMaxBytes &&
		len(e.StopReason) <= StopReasonMaxBytes && len(e.Failure) <= FailureMaxBytes &&
		(e.Unkept == nil || (e.Unkept.LaunchSeq > 0 && e.Unkept.CaptureSeq > 0 &&
			(e.Unkept.Loss == "" || e.Unkept.Loss.Within()) &&
			len(e.Unkept.Reason) <= UnkeptReasonMaxBytes)) &&
		e.notContinuedWithin() &&
		(e.LastWord == nil || (e.LastWord.LaunchSeq > 0 && e.LastWord.CaptureSeq > 0)) &&
		e.spentWithin()
}

// notContinuedWithin reports whether the ending's verdicts are inside their
// bounds ([NotContinued.Within]).
func (e Ending) notContinuedWithin() bool {
	if len(e.NotContinued) > NotContinuedMaxEntries {
		return false
	}
	for _, n := range e.NotContinued {
		if !n.Within() {
			return false
		}
	}
	return true
}

// HoldNotContinued is held with n beside it, as an end keeps a turn's
// verdicts: the first kept, n the newest, never past [NotContinuedMaxEntries].
func HoldNotContinued(held []NotContinued, n NotContinued) []NotContinued {
	held = slices.Clone(held)
	if len(held) < NotContinuedMaxEntries {
		return append(held, n)
	}
	held[len(held)-1] = n
	return held
}
