package runner

import (
	"strings"

	"github.com/crewlet/crewlet/internal/provenance"
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
// run that asked for it. So the resumed turn relaunches its executor instead:
// in the asking run's own box, continuing the harness's own conversation with the
// answer as its next prompt ([Asked.Continuation]), or — where that box is
// gone — as a fresh run told the question, the answer and the branch beside
// its brief ([Asked.Reseed]).
type Asked struct {
	// Question is what the run asked and Answer the reply; By is who
	// answered, "" where the route that carried the reply does not say.
	Question, Answer, By string

	// Branch is the work-in-progress branch the run pushed before it
	// asked, "" where it named none.
	Branch string

	// SessionID is the asking run's harness session, which a relaunch in its
	// box continues.
	SessionID string

	// BoxKept is whether the asking run's box is still there; false once
	// it was reclaimed while the question waited.
	BoxKept bool

	// Label is who wrote Answer, as the fence around a chat reply's message
	// labelled it for this seat; nil for an answer by turn. See
	// [Asked.answered].
	Label *provenance.Label
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
	var w strings.Builder
	w.WriteString("## A run before you on this task asked a question, and it was answered\n\n")
	if a.Question != "" {
		w.WriteString("It stopped to ask: " + a.asked(b) + "\n\n")
	}
	w.WriteString(a.answered(b) + "\n\n")
	switch {
	case !a.BoxKept && a.Branch != "":
		w.WriteString("Its box has since been reclaimed, so its working tree and its memory " +
			"of the task are gone: you are on a FRESH machine. Check out the existing branch `" +
			a.Branch + "` it pushed, re-read the code there, and finish the task with this " +
			"answer incorporated.")
	case !a.BoxKept:
		w.WriteString("Its box has since been reclaimed, so its working tree and its memory " +
			"of the task are gone: you are on a FRESH machine. Check out the work-in-progress " +
			"branch it pushed earlier, re-read the code there, and finish the task with this " +
			"answer incorporated.")
	case a.Branch != "":
		w.WriteString("Its work in progress is on the git branch `" + a.Branch + "`. If the " +
			"working tree in front of you is not that branch's, check it out first and re-read " +
			"the code there; then finish the task with this answer incorporated.")
	default:
		w.WriteString("Finish the task with this answer incorporated, starting from whatever " +
			"work in progress it left in the repository.")
	}
	return w.String()
}
