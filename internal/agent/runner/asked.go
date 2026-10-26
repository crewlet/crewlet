package runner

import (
	"strings"
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
// in the asking run's own box, continuing the CLI's own conversation with the
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

	// SessionID is the asking run's CLI session, which a relaunch in its
	// box continues.
	SessionID string

	// BoxKept is whether the asking run's box is still there; false once
	// it was reclaimed while the question waited.
	BoxKept bool
}

// answerer is how the prompts name who answered.
func (a Asked) answerer() string {
	if a.By != "" {
		return a.By
	}
	return "The person you asked"
}

// Continuation is the asking run's next prompt in its own conversation: the
// answer, and to carry on from where it stopped. Short, because the
// conversation already holds the task, the work so far and the question.
func (a Asked) Continuation() string {
	var b strings.Builder
	b.WriteString("## Your question was answered\n\n")
	if a.Question != "" {
		b.WriteString("You stopped to ask: " + a.Question + "\n\n")
	}
	b.WriteString(a.answerer() + " answered: " + a.Answer + "\n\n")
	b.WriteString("Carry on with the task from where you stopped, with this answer " +
		"incorporated, and finish it as the task asks.")
	return b.String()
}

// Reseed is what a FRESH run carrying the asking run's work on is told beside
// its brief: that a run before it asked, what, and the answer — and where its
// work in progress is, since this run does not remember any of it.
//
// IT MUST NOT PROMISE A WORKING TREE. A fresh run is one that could not carry
// the asking run's conversation on, and most often that is because the box was
// reclaimed while the question waited: the run starts by looking for a
// checkout, and a text that implied one would send it looking for files that
// are gone. Git is the durable state, so the branch is what it is pointed at.
func (a Asked) Reseed() string {
	var b strings.Builder
	b.WriteString("## A run before you on this task asked a question, and it was answered\n\n")
	if a.Question != "" {
		b.WriteString("It stopped to ask: " + a.Question + "\n\n")
	}
	b.WriteString(a.answerer() + " answered: " + a.Answer + "\n\n")
	switch {
	case !a.BoxKept && a.Branch != "":
		b.WriteString("Its box has since been reclaimed, so its working tree and its memory " +
			"of the task are gone: you are on a FRESH machine. Check out the existing branch `" +
			a.Branch + "` it pushed, re-read the code there, and finish the task with this " +
			"answer incorporated.")
	case !a.BoxKept:
		b.WriteString("Its box has since been reclaimed, so its working tree and its memory " +
			"of the task are gone: you are on a FRESH machine. Check out the work-in-progress " +
			"branch it pushed earlier, re-read the code there, and finish the task with this " +
			"answer incorporated.")
	case a.Branch != "":
		b.WriteString("Its work in progress is on the git branch `" + a.Branch + "`. If the " +
			"working tree in front of you is not that branch's, check it out first and re-read " +
			"the code there; then finish the task with this answer incorporated.")
	default:
		b.WriteString("Finish the task with this answer incorporated, starting from whatever " +
			"work in progress it left in the repository.")
	}
	return b.String()
}
