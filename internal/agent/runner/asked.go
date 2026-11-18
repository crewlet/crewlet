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
// The answer is never filed as the run's report: a run asks only when it
// cannot go on, so its round is not over and the reply accounts for nothing
// it did. The resumed turn relaunches the executor instead, continuing the
// harness's conversation with the answer as its next prompt
// ([Asked.Continuation]) from the turn's kept capture (continued.go) or the
// asking box's session, or else a fresh run told the question, answer and
// branch beside its brief ([Asked.Reseed]).
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
	// nil for a question the run asked itself (ADR-0077): made at most once
	// as the run is carried on ([makeHeld]); its outcome is the answer.
	Held *HeldCall

	// Withdrawn is whether the seat withdrew the question, or the held
	// call, itself, in another of its turns, Answer being why (ADR-0111):
	// told as its own withdrawal, never as a person's answer.
	Withdrawn bool

	// Refused is set where a reply was refused as too large to hand the run
	// (ADR-0114): Answer and By are empty, and the run is told the refusal
	// in the engine's words ([RefusedReply]), never an empty answer.
	Refused *RefusedReply
}

// withdrawnTail is what a run whose question or call its seat withdrew is told
// to do next: carry on under its access as it stands, which its next brief
// states, and ask again where it still needs to.
const withdrawnTail = "Carry on with the task from where you stopped, under your access as it " +
	"stands now, and finish it as the task asks. If you still need a person's answer or " +
	"approval, ask again in words that fit your access now."

// happened is what every fresh run carrying an asking run's work on is told of
// what that run did through the seat's tools, as [Lost.Reseed] and
// [Rotated.Reseed] tell it: it happened, so check before redoing it. Said in
// every branch of [Asked.Reseed], before [Asked.freshMachine].
//
// The list of the round's calls (internal/agent/prompts' CalledBefore) cannot
// say this alone: it omits work outside the seat's tools (commits, pushes,
// shell) and calls the record kept only in a tally or past the read cap.
const happened = "What it did through your tools before it stopped did happen: check before doing any of it again."

// withdrew is the seat's own reason, as the run reads it, under fence token b.
func (a Asked) withdrew(b provenance.Boundary) string {
	return "You withdrew it before anybody answered: " + provenance.Neutralise(b, a.Answer)
}

// RefusedReply is what a run is told of a reply too large to hand it: its
// message count, its recorded size and the bound (internal/sandbox's
// AnswerRefusal) — figures, never its words, which no record could carry.
//
// The run is resumed with the refusal through the channel an answer takes,
// because only it knows what shorter answer or other route (part of a log, a
// file, a link) would serve; left untold, it waits on a question the person
// thinks answered, and their next short message becomes its answer. It asks
// again (the new question takes only what is written after it) or carries on,
// saying the reply never reached it.
// The run says why in the question it asks again, since a reviewer reads the
// run's calls, never the replies, and a bare repeat reads as asking twice.
type RefusedReply struct {
	// Messages is how many messages the reply was: several that reached the
	// seat together are refused together, which the person needs to know.
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
// A chat reply is fenced under who wrote it (ADR-0040): the chat surface, not
// the engine, vouched for them, so it is information, never an instruction.
// An answer by turn is not fenced: it came from someone the engine let answer
// (requester, seat's lead or administrator), sealed as their instruction
// (ADR-0068), and a fence would tell the run not to follow it. Either way no
// words, question or name open or close a fence ([provenance.Neutralise]).
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
// It must not promise a working tree: a fresh run most often means the box
// was reclaimed while the question waited, so it is pointed at the branch,
// git being the durable state, not at files that may be gone.
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
		w.WriteString(happened + "\n\n")
		w.WriteString(a.freshMachine("under your access as it stands now"))
		return w.String()
	}
	var w strings.Builder
	w.WriteString("## A run before you on this task asked a question, and it was answered\n\n")
	if a.Question != "" {
		w.WriteString("It stopped to ask: " + a.asked(b) + "\n\n")
	}
	w.WriteString(a.answered(b) + "\n\n")
	w.WriteString(happened + "\n\n")
	w.WriteString(a.freshMachine("with this answer incorporated"))
	return w.String()
}

// refusedContinuation is [Asked.Continuation] for a refused reply: the asking
// run's own session is told the person replied and the reply did not reach it,
// with its figures, and to ask again or carry on. Checked before Held: a
// refused reply is a chat reply, which never answers a held call
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
	w.WriteString(happened + "\n\n")
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
