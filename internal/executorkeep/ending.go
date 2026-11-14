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
	// THE END IS WHERE IT IS SAID, because the run's record that knew it is
	// deleted with the turn, and the turn's row would otherwise go on naming
	// an older capture of the turn — one that does not hold what came after
	// it — as the turn's last word, for whatever continues the conversation's
	// session to take for it, with nothing to say so. So an end that names one
	// says too that the turn went on past the session its row names
	// ([Ending.WentOn]) — whatever that row names: an older capture the run's
	// record kept, one the log took that the record holds refused, or none —
	// and the row keeps that session, told as what the turn did up to it rather
	// than all it did. Only an end that does not say so — one an older build
	// wrote — has the executor log's apply take that older object off the row,
	// so the row names no session of the turn; the record's version decides
	// which (internal/executordomain's applyEnd).
	//
	// DECIDED ON THE ROW'S OWN PLACE, in log order, and read the same way: an
	// end is decided off the run's record, which can name a capture as not
	// kept whose record the log took before the end all the same — and that
	// capture is then the row's, and kept. So the apply releases nothing for
	// one no newer than the row's place, and a reader tells it only where it
	// is newer (internal/executordomain's Session.Unkept).
	Unkept *Unkept `json:"unkept,omitempty"`

	// WentOn says the turn WENT ON PAST the session its row names: the turn
	// took a capture, and the run's record does not hold its LAST WORD — the
	// final capture of its last launch that ran a job — kept. What it kept was
	// taken while its job still worked, or is an earlier launch's, or a newer
	// capture beside it was not kept ([Ending.Unkept], which never rides an
	// end without it), or it kept none: so the session the row names, where it
	// names one, holds what the turn did up to it and nothing after. Written
	// only where true, and so a field a build that predates it has no reader
	// for (internal/executordomain's record version 4).
	//
	// THE END IS WHERE IT IS SAID, for Unkept's reason: only the run's record
	// knows which capture its last launch left, and that record is deleted
	// with the turn. Decided off that record, it is HELD TO THE ROW'S OWN PLACE
	// as Unkept is, because the record and the log can part: a capture the
	// record holds refused can be on the log all the same — its taker had handed
	// its record on before a wait's settle read the turn's row, and the log took
	// it after — and where that capture is the turn's last word, the row names
	// it and holds all the turn did. So the end NAMES ITS LAST WORD wherever it
	// knows it ([Ending.LastWord]), and a reader holds the row's place to it:
	// the turn went on past the session the row names only where the end names
	// no last word, or the row's place is older than the one it names
	// (internal/executordomain's Session.WentOn), and a row at that place is
	// read as the turn's last word, as any whose final capture landed is.
	//
	// AN END THAT CARRIES IT KEEPS THE ROW'S SESSION: a prefix of what the turn
	// did, told as one, holds more than the conversation's previous head or a
	// fresh session would. So no unkept attempt beside it takes the row's
	// object off, the end absorbs what the turn's read handed out as any turn
	// whose row keeps a session does, and the row says the session is not the
	// turn's last word: the conversation's next turn is told so in words, and
	// every reader of the row is shown it here (internal/executordomain's
	// Session.WentOn).
	WentOn bool `json:"went_on,omitempty"`

	// LastWord is the place of the turn's LAST WORD — the final capture of its
	// last launch that ran a job — where the end says the turn went on
	// ([Ending.WentOn]) and that capture is the turn's newest: refused, never
	// answered, or begun and never finished, on the run's record. Nil
	// anywhere else: beside an end that does not say the turn went on, whose
	// last word was kept; and where the turn's last word was never taken — a
	// stop or a loss whose box was already gone, a relaunch that kept nothing —
	// or is not its newest capture.
	//
	// WHAT A READER HOLDS THE ROW'S PLACE TO: the end names the last word its
	// record could not keep, and the log, which alone knows whether it took that
	// capture's record all the same, settles whether the turn went on past the
	// row's session. A row at this place or past it holds the turn's last word,
	// and its turn went on past no session of it, whatever went_on says; a row
	// older than it, or naming none, is one the turn went on past. Named only
	// beside went_on, so it stamps no end at a version went_on does not
	// (internal/executordomain's record version 4); placed, and never worded:
	// the two ordinals every capture of the turn carries.
	LastWord *CapturePlace `json:"last_word,omitempty"`

	// Exhausted names the sessions a launch of the turn could not go on in: it
	// ran out of its model's context, or its provider refused the request the
	// session makes. The end is where the fleet is told, for Unkept's reason, and
	// the log's apply retires each from every ended row of the conversation.
	Exhausted []string `json:"exhausted,omitempty"`
}

// ExhaustedWhy is why no launch continues session, which a launch of turn could
// not go on in: the one sentence a launch logs and a reader is told. It names
// both causes, since the log keeps which sessions were retired and not why.
func ExhaustedWhy(session, turn string) string {
	return fmt.Sprintf("session %s could not go on in a launch of turn %s (it ran out of its model's context, "+
		"or its provider refused the request it makes), and another launch of it would meet the same end, "+
		"so it is retired: no launch continues it, and the conversation carries on from how its earlier "+
		"turns ended", session, turn)
}

// Unkept is a capture of a turn's session that no row names: its place among
// the turn's captures, and why it was not kept.
type Unkept struct {
	// LaunchSeq and CaptureSeq are its place, the ordinals every capture of
	// the turn carries.
	LaunchSeq  int64 `json:"launch_seq"`
	CaptureSeq int64 `json:"capture_seq"`

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
// word placed before the first capture, an exhausted session outside its
// bounds. Prose is not refused here; [Ending.Fit] cuts it. The error
// names the field and what is wrong with it; the executor log's writer answers
// it as its own refusal of an invalid write.
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
		prose = append(prose, struct{ field, value string }{"unkept reason", u.Reason})
	}
	if w := e.LastWord; w != nil && (w.LaunchSeq <= 0 || w.CaptureSeq <= 0) {
		return fmt.Errorf("the ending's last word is placed at (%d, %d), and every capture is placed "+
			"from (1, 1)", w.LaunchSeq, w.CaptureSeq)
	}
	for _, p := range prose {
		if !utf8.ValidString(p.value) {
			return fmt.Errorf("the ending's %s is not valid UTF-8", p.field)
		}
	}
	if !e.exhaustedWithin() {
		return fmt.Errorf("the ending names %d exhausted sessions, and it holds at most %d, each a valid "+
			"UTF-8 id of 1 to %d bytes", len(e.Exhausted), ExhaustedMaxSessions, SessionIDMaxBytes)
	}
	return nil
}

// exhaustedWithin reports whether the ending's exhausted sessions are inside
// their bounds. Refused rather than cut: a shortened id names another session.
func (e Ending) exhaustedWithin() bool {
	if len(e.Exhausted) > ExhaustedMaxSessions {
		return false
	}
	for _, session := range e.Exhausted {
		if session == "" || len(session) > SessionIDMaxBytes || !utf8.ValidString(session) {
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
	return e
}

// Excerpt is text as a record of the executor log keeps somebody's words — an
// ending's prose, what a turn was asked — at most bound bytes: redacted
// ([redact.Secrets]) and then cut on a rune boundary, marked where it was cut
// inside the bound ([textcut.Within]), and never inside a marker the
// redaction wrote, which a cut moves back from to where it begins
// ([redact.MarkersAround]).
//
// REDACTED BEFORE IT IS CUT, never only after: a credential the bound runs
// across, cut first, leaves a head no rule recognises — a GitHub token cut to
// its first twenty characters is no token the redaction knows — and a
// reader's redaction of the row could no longer take it out, so the head was
// handed to every reader, a seat's model among them. Redacted whole, every
// credential the redaction knows is a marker before the bound is applied. A
// reader still passes a row's words through the redaction
// (internal/executordomain's Session.Redacted), which a rule added since the
// record was written reaches.
func Excerpt(text string, bound int) string {
	shown := redact.Secrets(text)
	if len(shown) <= bound {
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
// unbounded row.
func (e Ending) Within() bool {
	return e.Outcome.Valid() &&
		len(e.Decision) <= WordMaxBytes && len(e.DeliveredTo) <= WordMaxBytes &&
		len(e.Answer) <= WordMaxBytes && len(e.StoppedBy) <= ActorMaxBytes &&
		len(e.CompletedWork) <= CompletedWorkMaxBytes &&
		len(e.BlockedOn) <= BlockedOnMaxBytes &&
		len(e.StopReason) <= StopReasonMaxBytes && len(e.Failure) <= FailureMaxBytes &&
		(e.Unkept == nil || (e.Unkept.LaunchSeq > 0 && e.Unkept.CaptureSeq > 0 &&
			len(e.Unkept.Reason) <= UnkeptReasonMaxBytes)) &&
		(e.LastWord == nil || (e.LastWord.LaunchSeq > 0 && e.LastWord.CaptureSeq > 0)) &&
		e.exhaustedWithin()
}
