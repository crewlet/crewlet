package runner

import (
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// Lost is a run whose job was LOST with the host it ran on before it finished:
// what a resume carries the round on from ([Resume.Lost]).
//
// # Why the round is carried on, not collected
//
// The job was cut off mid-step by something outside it — the restart that
// replaced its engine killed it, its box went with its host, its node left the
// fleet — with nothing written to say how far it got. What it left is not an
// account of the round, and judged as one the round "did not succeed" for a
// reason the run never had. So the resumed turn relaunches its executor for the
// same round, as it does for a run a person's answer carries on ([Asked]): in
// the lost job's box where the launch may continue in it, its working tree as
// the job left it, and in a fresh one otherwise. In the lost job's own session,
// where its launcher says it can be continued (continued.go): the capture
// packed once the loss was found, where its engine's restart left its box
// behind — never the box's own copy as it stands, which the harness was cut off
// in — told it was cut off, what it called after the capture and that it may
// end mid-call ([Lost.Continuation]). A job whose box went with its host left
// no capture, and an earlier job's holds none of what this one was told, so its
// round goes on in a session of its own, as it does after a second loss in the
// round: told what happened and where to look for what the job left
// ([Lost.Reseed]).
//
// UNLESS THE ROUND WAS ALREADY SUBMITTED: a job whose `submit_work` is on the
// round's calls had finished its work and was lost on its way out, and the
// round is collected from those calls like any other ([Runner.Resume]).
type Lost struct {
	// Why is what happened to the job, as a clause that follows "when":
	// "the engine it ran under restarted, which killed it".
	Why string

	// BoxKept is whether the lost job's box is still where it ran — its
	// engine's restart killed the job and left the box — so a relaunch that
	// continues in it finds the job's working tree as the job left it.
	BoxKept bool

	// Losses is how many of the round's jobs have been lost, this one
	// included, and Limit how many a round is carried on after: past it the
	// round is not relaunched again ([ErrRoundLost]).
	Losses, Limit int
}

// ErrRoundLost is a round whose jobs were lost with their hosts more often than
// a round is carried on after ([Lost.Limit]): a run that keeps taking its own
// host down, which every relaunch would take down again.
var ErrRoundLost = errors.New("the executor's round was lost with its host too often to start again")

// Reseed is what the run carrying a lost job's round on is told beside its
// brief: that a run before it on this round was cut off, why, and where to look
// for what it left.
//
// IT PROMISES NOTHING THE LAUNCH DECIDES. Whether this run is in the lost
// job's box is the launch's to decide once it knows which box it has, and a
// kept box is continued in only on the node whose host it is on, by the same
// harness — so the text says where to look either way rather than describing a
// working tree that may not be in front of it. And it says that what the lost
// job did through the seat's tools did happen: those calls are not undone by
// the loss, and a run that repeats them repeats their effects.
func (l Lost) Reseed() string {
	var b strings.Builder
	b.WriteString("## A run before you on this task was cut off\n\n")
	b.WriteString("It was working on this task when " + l.why() + ", so it stopped mid-step, " +
		"before it finished, and did not get to report.\n\n")
	if l.BoxKept {
		b.WriteString("Its box was kept. If this is that box, its working tree is in front of you " +
			"as it left it, changes it had not committed included: read `git status`, `git diff` " +
			"and `git log` before anything else, and carry on from there. If what is in front of " +
			"you is a fresh checkout instead, look for a work-in-progress branch it may have " +
			"pushed and carry on from that.\n\n")
	} else {
		b.WriteString("Its box went with it, so you are on a FRESH machine and anything it had not " +
			"pushed is gone. Look for a work-in-progress branch it may have pushed, and carry on " +
			"from that if there is one.\n\n")
	}
	b.WriteString("What it did through your tools before it stopped — a comment posted, a work " +
		"item updated, a file published — did happen: check before doing any of it again. Then " +
		"finish the task as it asks.")
	return b.String()
}

// why is the job's loss as a clause, in the most general terms where the
// coordinator gave none.
func (l Lost) why() string {
	if strings.TrimSpace(l.Why) == "" {
		return "it was lost with the host it ran on"
	}
	return l.Why
}

// Continuation is what the session a lost job worked in is told when the round
// is carried on in it, restored from the turn's kept capture of it: that it was
// cut off, why, and that what it did through the seat's tools before did happen.
// What came after the capture, and that the session may end mid-call, the
// continuation adds beside it (continued.go).
func (l Lost) Continuation() string {
	return "## Your previous segment was cut off\n\n" +
		"You were working on this task when " + l.why() + ", so you stopped mid-step, before you " +
		"finished, and did not get to report. What you did through your tools before you were cut " +
		"off — a comment posted, a work item updated, a file published — did happen: check before " +
		"doing any of it again. Then finish the task as it asks."
}

// carried is a run whose round a resume carries on rather than collects: one
// that asked a person and was answered ([Asked]), one lost with its host
// ([Lost]), or one refused on a sign-in replaced while it ran ([Rotated]).
// [Runner.continueAgentRun] relaunches each the same way.
type carried interface {
	// session is the run's own harness session, which the relaunch continues
	// where it runs in that run's box and takes up no kept capture of it;
	// "" continues none there.
	session() string
	// continuation is the prompt the continued session is given, and
	// reseed what a fresh run carrying the round on is told beside its
	// brief, each under the fence token of the turn that relaunches it.
	continuation(provenance.Boundary) string
	reseed(provenance.Boundary) string
	// keeps says the round may be carried on in the turn's kept session at
	// all (continued.go); branch is the work-in-progress branch the run
	// before pushed, where the engine knows it, for a box that replaced its.
	keeps() bool
	branch() string
	// refusal is why the round may not be carried on, nil where it may.
	refusal() error
	// what names the carrying on, for an error that says what failed.
	what() string
}

func (a *Asked) session() string                           { return a.SessionID }
func (a *Asked) continuation(b provenance.Boundary) string { return a.Continuation(b) }
func (a *Asked) reseed(b provenance.Boundary) string       { return a.Reseed(b) }
func (a *Asked) keeps() bool                               { return true }
func (a *Asked) branch() string                            { return a.Branch }
func (a *Asked) refusal() error                            { return nil }

// what names the carrying on, an answer's or a refused reply's.
func (a *Asked) what() string {
	if a.Refused != nil {
		return "carrying on the run told the reply to its question was too large to reach it"
	}
	return "carrying on the run with its answer"
}

// A lost job's session is never continued IN PLACE: what its box holds is
// what its harness was cut off in, and the kept capture of it — whose archive
// leaves out what its harness never finished, and whose continuation says what
// came after it — is what a relaunch continues (continued.go). Not a second
// time in one round, though: a round lost again is a run that may take its host
// down from the state it holds, so its relaunch starts a session of its own
// rather than replay that state toward the round's limit. What a fresh run is
// told of it is the engine's own words, which no fence token touches.
func (l *Lost) session() string                         { return "" }
func (l *Lost) continuation(provenance.Boundary) string { return l.Continuation() }
func (l *Lost) reseed(provenance.Boundary) string       { return l.Reseed() }
func (l *Lost) keeps() bool                             { return sandbox.KeptAfterLoss(l.Losses) }
func (l *Lost) branch() string                          { return "" }
func (l *Lost) what() string                            { return "carrying on the round its host lost" }

func (l *Lost) refusal() error {
	if l.Losses <= l.Limit {
		return nil
	}
	return fmt.Errorf("%w: its run was lost %d times in this round, the last when %s, and a "+
		"round is carried on after at most %d", ErrRoundLost, l.Losses, l.why(), l.Limit)
}
