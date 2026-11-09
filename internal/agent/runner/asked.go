package runner

import (
	"strings"

	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/words"
)

// Asked is the question a parked run put to a person, answered: what a resume
// carries the run on with ([Resume.Asked]).
//
// # Why the run is carried on, not collected
//
// A run asks only when it cannot go on without the answer, so the round it
// asked in is not over: there is no report to judge yet, and the reply is no
// account of anything the run did. Filed as the run's report — which is what
// the resume once did with it — the answer reached the turn as a run that
// ended without submitting, was rescued as incomplete, and never reached the
// run that asked for it. So the resumed turn relaunches its executor instead,
// continuing the harness's own conversation with the answer as its next prompt
// ([Asked.Continuation]) — the turn's kept capture of it, in whatever box the
// launch gets (continued.go), or the session the asking run's own box holds —
// or, where the conversation can be continued in neither, as a fresh run told
// the question, the answer and the branch beside its brief ([Asked.Reseed]).
type Asked struct {
	// Question is what the run asked and Answer the reply; By is who
	// answered, "" where the route that carried the reply does not say.
	Question, Answer, By string

	// Branch is the work-in-progress branch the run pushed before it
	// asked, "" where it named none.
	Branch string

	// SessionID is the asking run's harness session, which a relaunch in its
	// box continues in place where the turn kept no capture of it to restore
	// (continued.go).
	SessionID string

	// BoxKept is whether the asking run's box is still there; false once
	// it was reclaimed while the question waited.
	BoxKept bool

	// Label is who wrote Answer, as the fence around a chat reply's message
	// labelled it for this seat; nil for an answer by turn. See
	// [Asked.answered].
	Label *provenance.Label

	// Held is the call to a tool at Needs approval the question was about,
	// nil for a question the run asked itself (ADR-0077): made, at most
	// once, as the run is carried on ([makeHeld]), and what became
	// of it is the answer the run is told.
	Held *HeldCall

	// Withdrawn is whether the seat withdrew the question, or the held
	// call, itself, in another of its turns, Answer being why (ADR-0111):
	// told as its own withdrawal, never as a person's answer.
	Withdrawn bool

	// Refused is set where the question's outcome was no answer but the
	// REFUSAL of a reply too large to hand the run (ADR-0114): Answer and
	// By are empty, and the run is told the refusal in their place, in the
	// engine's words ([RefusedReply]) — never handed an empty answer.
	Refused *RefusedReply
}

// withdrawnTail is what a run whose question or call its seat withdrew is told
// to do next: carry on under its access as it stands, which its next brief
// states, and ask again where it still needs to.
const withdrawnTail = "Carry on with the task from where you stopped, under your access as it " +
	"stands now, and finish it as the task asks. If you still need a person's answer or " +
	"approval, ask again in words that fit your access now."

// withdrew is the seat's own reason, as the run reads it, under fence token b.
func (a Asked) withdrew(b provenance.Boundary) string {
	return "You withdrew it before anybody answered: " + provenance.Neutralise(b, a.Answer)
}

// RefusedReply is what a run is told of the reply to its question that was
// too large to hand it: how many messages it was, what it came to as the
// engine records an answer, and what an answer to a run may (internal/sandbox's
// AnswerRefusal) — figures, and none of its words.
//
// # Why the run is told, and told this
//
// The run is the one that knows what it asked for and why, so it is the one
// that can say what shorter answer would serve, or another way to get the rest
// to it — a part of a log, a file in its checkout, a link — or that it can go
// on without. A reply refused with nobody told left the run waiting on a
// question the person believed answered, and their next short message ("any
// luck?") became its answer. So the run is resumed with the refusal through
// the very channel an answer reaches it, and either asks again — its new
// question, anchored on its own asking, takes only what is written after it —
// or carries on and says the reply never reached it.
//
// QUOTING NOTHING: the reply's words are the very thing no record of the run
// could carry, and its figures are the engine's own. And SAID IN THE QUESTION
// IT ASKS AGAIN, which is all a reviewer of the round reads of the exchange:
// the review's evidence is the run's calls, never the person's replies, so a
// second `ask_and_wait` that says why it asks again is the follow-up it is,
// where one that only repeated the first would read as asking twice.
type RefusedReply struct {
	// Messages is how many messages the reply was: several that reached the
	// seat together are refused together, and a person asked for a shorter
	// answer needs to know their reply was all of them.
	Messages int

	// Bytes is what the reply came to as the engine records an answer, and
	// MaxBytes what an answer to a run may come to there.
	Bytes, MaxBytes int
}

// said is the refused reply as the run is told it: how many messages, and
// both figures — the reply's ROUNDED UP ([kept.SizeUp]), so a reply past the
// bound never reads as within it, beside the bound in [kept.Size]'s units.
func (r RefusedReply) said() string {
	return "the reply — " + words.Count(max(r.Messages, 1), "message", "messages") + ", " +
		kept.SizeUp(r.Bytes) + " as the engine records an answer — is past the " +
		kept.Size(r.MaxBytes) + " an answer to a run can carry"
}

// recorded is why the reply's figure is so far past what its words came to,
// which the run needs to ask for what will fit: the engine records a reply at
// about 3.4 times its length in plain text and up to about 20 times in markup
// (internal/sandbox's MaxAnswerBytes, ADR-0114).
const recorded = "A reply is recorded at about three and a half times its length, and up to twenty " +
	"times where it is mostly markup, so one that fits is far shorter than that."

// askAgain is what the run is to do with a refused reply, by whose question
// (`your`, a run carried on in its own session, or `its`, a fresh run carrying
// another's work on): ask again for less or another way to get it, saying why
// in the question, or carry on and say the reply never reached it.
func (r RefusedReply) askAgain(whose string) string {
	return "If you still need it, ask again with `" + toolname.AskAndWait + "`: say in the question " +
		"that the reply to " + whose + " question was too large to reach you, and ask for what you " +
		"need in a much shorter form, or for another way to get it to you — a file in your checkout, " +
		"committed to your branch; a path you can read; or a link. Otherwise carry on with the task " +
		"without it, and say in your final reply that the reply to " + whose + " question was too " +
		"large to reach you."
}

// answered is the answer as the run reads it, under the fence token b of the
// turn that carries the run on.
//
// A REPLY IS FENCED UNDER WHO WROTE IT (ADR-0040): somebody spoke in the
// conversation the run asked in, and the chat surface, not the engine,
// vouched for who that was, so the run reads their words as it reads the
// message that woke its turn, as information from them and never an
// instruction that overrides its own. AN ANSWER BY TURN IS NOT FENCED: it
// came from a person the engine let answer this run, its requester, its
// seat's lead or an administrator, sealed onto its record as that person's
// instruction to it (ADR-0068), and a fence would tell the run not to follow
// the one answer it stopped to wait for. Either way nothing in the words, and
// nothing in the question the run's own session wrote or in the answerer's
// name, opens or closes a fence ([provenance.Neutralise]).
func (a Asked) answered(b provenance.Boundary) string {
	if a.Label != nil {
		return a.answerer(b) + " answered:\n" + a.Label.Fence(b, a.Answer)
	}
	return a.answerer(b) + " answered: " + provenance.Neutralise(b, a.Answer)
}

// answerer is how the prompts name who answered.
func (a Asked) answerer(b provenance.Boundary) string {
	if a.By != "" {
		return provenance.Neutralise(b, a.By)
	}
	return "The person you asked"
}

// asked is the run's own question, as it is read back to it.
func (a Asked) asked(b provenance.Boundary) string {
	return provenance.Neutralise(b, a.Question)
}

// Continuation is the asking run's next prompt in its own conversation, under
// the fence token b of the turn that continues it: the answer, and to carry on
// from where it stopped. Short, because the conversation already holds the
// task, the work so far and the question.
func (a Asked) Continuation(b provenance.Boundary) string {
	if a.Refused != nil {
		return a.refusedContinuation(b)
	}
	if a.Held != nil {
		return a.heldContinuation(b)
	}
	if a.Withdrawn {
		var w strings.Builder
		w.WriteString("## You withdrew your question\n\n")
		if a.Question != "" {
			w.WriteString("You stopped to ask: " + a.asked(b) + "\n\n")
		}
		w.WriteString(a.withdrew(b) + "\n\n" + withdrawnTail)
		return w.String()
	}
	var w strings.Builder
	w.WriteString("## Your question was answered\n\n")
	if a.Question != "" {
		w.WriteString("You stopped to ask: " + a.asked(b) + "\n\n")
	}
	w.WriteString(a.answered(b) + "\n\n")
	w.WriteString("Carry on with the task from where you stopped, with this answer " +
		"incorporated, and finish it as the task asks.")
	return w.String()
}

// Reseed is what a FRESH run carrying the asking run's work on is told beside
// its brief, under the fence token b of the turn that launches it: that a run
// before it asked, what, and the answer — and where its work in progress is,
// since this run does not remember any of it.
//
// IT MUST NOT PROMISE A WORKING TREE. A fresh run is one that could not carry
// the asking run's conversation on, and most often that is because the box was
// reclaimed while the question waited: the run starts by looking for a
// checkout, and a text that implied one would send it looking for files that
// are gone. Git is the durable state, so the branch is what it is pointed at.
func (a Asked) Reseed(b provenance.Boundary) string {
	if a.Refused != nil {
		return a.refusedReseed(b)
	}
	if a.Held != nil {
		return a.heldReseed(b)
	}
	if a.Withdrawn {
		var w strings.Builder
		w.WriteString("## A run before you on this task asked a question, and your seat withdrew it\n\n")
		if a.Question != "" {
			w.WriteString("It stopped to ask: " + a.asked(b) + "\n\n")
		}
		w.WriteString(a.withdrew(b) + "\n\n")
		w.WriteString(a.freshMachine("under your access as it stands now"))
		return w.String()
	}
	var w strings.Builder
	w.WriteString("## A run before you on this task asked a question, and it was answered\n\n")
	if a.Question != "" {
		w.WriteString("It stopped to ask: " + a.asked(b) + "\n\n")
	}
	w.WriteString(a.answered(b) + "\n\n")
	w.WriteString(a.freshMachine("with this answer incorporated"))
	return w.String()
}

// refusedContinuation is [Asked.Continuation] for a refused reply: the asking
// run's own session is told the person replied and the reply did not reach it,
// with its figures, and to ask again or carry on.
//
// THE REFUSAL FIRST, BEFORE ANYTHING ELSE THE RESUME COULD SAY: a refused
// reply is a chat reply, and a chat reply never answers a held call
// (internal/sandbox's ConversationRef.Answers), so the two never meet.
func (a Asked) refusedContinuation(b provenance.Boundary) string {
	var w strings.Builder
	w.WriteString("## The reply to your question was too large to reach you\n\n")
	if a.Question != "" {
		w.WriteString("You stopped to ask: " + a.asked(b) + "\n\n")
	}
	w.WriteString(a.answerer(b) + " replied, but " + a.Refused.said() + ", so the engine did not take " +
		"it as your answer, and you were given none of it. " + recorded + "\n\n")
	w.WriteString(a.Refused.askAgain("your"))
	return w.String()
}

// refusedReseed is [Asked.Reseed] for a refused reply: a fresh run carrying the
// asking run's work on is told that run asked, that the reply reached neither
// of them, and to ask again or carry on — and where the work is.
func (a Asked) refusedReseed(b provenance.Boundary) string {
	var w strings.Builder
	w.WriteString("## A run before you on this task asked a question, and the reply was too large " +
		"to reach it\n\n")
	if a.Question != "" {
		w.WriteString("It stopped to ask: " + a.asked(b) + "\n\n")
	}
	w.WriteString("The person it asked replied, but " + a.Refused.said() + ", so the engine did " +
		"not take it as the answer, and that run and you were given none of it. " + recorded + "\n\n")
	w.WriteString(a.freshMachine("as it asks") + "\n\n")
	w.WriteString(a.Refused.askAgain("its"))
	return w.String()
}

// freshMachine is where a fresh run carrying the work on finds it, and what it
// is to finish the task with (with: "with this answer incorporated").
func (a Asked) freshMachine(with string) string {
	switch {
	case !a.BoxKept && a.Branch != "":
		return "Its box has since been reclaimed, so its working tree and its memory " +
			"of the task are gone: you are on a FRESH machine. Check out the existing branch `" +
			a.Branch + "` it pushed, re-read the code there, and finish the task " + with + "."
	case !a.BoxKept:
		return "Its box has since been reclaimed, so its working tree and its memory " +
			"of the task are gone: you are on a FRESH machine. Check out the work-in-progress " +
			"branch it pushed earlier, re-read the code there, and finish the task " + with + "."
	case a.Branch != "":
		return "Its work in progress is on the git branch `" + a.Branch + "`. If the " +
			"working tree in front of you is not that branch's, check it out first and re-read " +
			"the code there; then finish the task " + with + "."
	}
	return "Finish the task " + with + ", starting from whatever work in progress it left in " +
		"the repository."
}
