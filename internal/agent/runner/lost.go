package runner

import (
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// Lost is a run whose job was LOST with the host it ran on before it finished:
// what a resume carries the round on from ([Resume.Lost]).
//
// The round is carried on, not collected: a job cut off mid-step left no
// account of the round, and judged as one it "did not succeed" for a reason it
// never had. The executor relaunches for the same round as for [Asked]: in the
// kept box where it may continue, else a fresh one; in the job's session where
// its launcher can continue one, restored from the capture packed once the
// loss was found, never the box's copy it was cut off in ([Lost.Continuation],
// continued.go); else, with no capture or after a second loss, in a session of
// its own ([Lost.Reseed]). A round whose `submit_work` is already on its calls
// is collected instead ([Runner.Resume]).
type Lost struct {
	// Why is what happened to the job, as a clause that follows "when":
	// "the engine it ran under restarted, which killed it".
	Why string

	// BoxKept is whether the lost job's box is still where it ran — its
	// engine's restart killed the job and left the box — so a relaunch that
	// continues in it finds the job's working tree as the job left it.
	BoxKept bool

	// Losses counts the round's lost jobs, this one included; past Limit the
	// round is not relaunched ([ErrRoundLost]).
	Losses, Limit int
}

// ErrRoundLost is a round lost with its hosts more than [Lost.Limit] times: a
// run that keeps taking its host down. Its refusal also wraps
// [turn.ErrPermanent], ending the turn, since a retry carries the same losses.
var ErrRoundLost = errors.New("the executor's round was lost with its host too often to start again")

// Reseed is what the run carrying a lost job's round on is told beside its
// brief: that a run before it was cut off, why, and where to look for what it
// left.
//
// It promises nothing the launch decides: whether this run is in the kept box
// is known only at launch, so the text says where to look either way. And the
// lost job's tool calls did happen; repeating them repeats their effects.
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

// why is the loss as a clause, a general one where the coordinator gave none.
func (l Lost) why() string {
	if strings.TrimSpace(l.Why) == "" {
		return "it was lost with the host it ran on"
	}
	return l.Why
}

// Continuation is what the lost job's session, restored from the turn's kept
// capture, is told: that it was cut off, why, and that its tool calls did
// happen. What came after the capture is added beside it (continued.go).
func (l Lost) Continuation() string {
	return "## Your previous segment was cut off\n\n" +
		"You were working on this task when " + l.why() + ", so you stopped mid-step, before you " +
		"finished, and did not get to report. What you did through your tools before you were cut " +
		"off — a comment posted, a work item updated, a file published — did happen: check before " +
		"doing any of it again. Then finish the task as it asks."
}

// carried is a run whose round a resume carries on rather than collects: one
// that asked a person and was answered ([Asked]), one lost with its host
// ([Lost]), one refused on a sign-in replaced while it ran ([Rotated]), or one
// whose continued session its provider refused ([Refused]).
// [Runner.continueAgentRun] relaunches each the same way.
type carried interface {
	// session is the harness session a relaunch in the run's own box,
	// taking up no kept capture, continues in place ("" for none); boxKept
	// says that box still exists, and where it does not, no launch continues
	// in it whatever session says.
	session() string
	boxKept() bool
	// continuation prompts the continued session, reseed a fresh run, each
	// under the fence token of the relaunching turn.
	continuation(provenance.Boundary) string
	reseed(provenance.Boundary) string
	// tellsHeld is the held call ([Asked.Held]) the continuation's lead tells
	// the fate of, by id and tool ("", "" for none); that call is left out of
	// the brief's list of the round's calls ([Runner.calledAfter]).
	tellsHeld() (id, tool string)
	// tellsCalled is every call the continuation's lead tells its run parked
	// on ([Asked.Called]), each left out of the lists as the held call is.
	tellsCalled() []types.CallReturned
	// lead is kept beside the run's record so a later brief to a session that
	// lacks it can tell it again (leads.go); nil where the engine's own words
	// carry the round on (a loss, a replaced sign-in, which every later brief's
	// cut-off section repeats) or the run did no work ([Refused]).
	lead() *sandbox.Lead
	// keeps says the round may continue the turn's kept session at all
	// (continued.go); branch is the work-in-progress branch the previous run
	// pushed, where known, for a replacement box.
	keeps() bool
	branch() string
	// refusal is why the round may not be carried on, nil where it may.
	refusal() error
	// what names the carrying on, for an error that says what failed.
	what() string
}

func (a *Asked) session() string                           { return a.SessionID }
func (a *Asked) boxKept() bool                             { return a.BoxKept }
func (a *Asked) continuation(b provenance.Boundary) string { return a.Continuation(b) }
func (a *Asked) reseed(b provenance.Boundary) string       { return a.Reseed(b) }
func (a *Asked) keeps() bool                               { return true }
func (a *Asked) branch() string                            { return a.Branch }
func (a *Asked) refusal() error                            { return nil }

// tellsHeld is the held call the question was about, whose fate the
// continuation tells ([Asked.Continuation]), by the id the approved call
// carries ([BridgedCall.HeldID]) and its tool. "" for no held call, and for a
// refused reply, whose continuation tells only the refusal (a chat reply never
// answers a held call).
func (a *Asked) tellsHeld() (id, tool string) {
	if a.Held == nil || a.Refused != nil {
		return "", ""
	}
	return a.Held.HeldID, a.Held.Tool
}

// tellsCalled is every call the run parked on, which the continuation tells
// whatever answered it ([Asked.calledToo]), a refused reply's included.
func (a *Asked) tellsCalled() []types.CallReturned { return a.Called }

// what names the carrying on, an answer's or a refused reply's.
func (a *Asked) what() string {
	if a.Refused != nil {
		return "carrying on the run told the reply to its question was too large to reach it"
	}
	return "carrying on the run with its answer"
}

// A lost job's session is never continued IN PLACE: its box holds what the
// harness was cut off in, so a relaunch continues the kept capture instead
// (continued.go), and after a second loss not even that
// ([sandbox.KeptAfterLoss]). A fresh run is told only the engine's own words,
// which need no fence token.
func (l *Lost) session() string                         { return "" }
func (l *Lost) boxKept() bool                           { return l.BoxKept }
func (l *Lost) continuation(provenance.Boundary) string { return l.Continuation() }
func (l *Lost) reseed(provenance.Boundary) string       { return l.Reseed() }
func (l *Lost) tellsHeld() (id, tool string)            { return "", "" }
func (l *Lost) tellsCalled() []types.CallReturned       { return nil }
func (l *Lost) lead() *sandbox.Lead                     { return nil }
func (l *Lost) keeps() bool                             { return sandbox.KeptAfterLoss(l.Losses) }
func (l *Lost) branch() string                          { return "" }
func (l *Lost) what() string                            { return "carrying on the round its host lost" }

func (l *Lost) refusal() error {
	if l.Losses <= l.Limit {
		return nil
	}
	return fmt.Errorf("%w, and %w: its run was lost %d times in this round, the last when %s, and a "+
		"round is carried on after at most %d", ErrRoundLost, turn.ErrPermanent, l.Losses, l.why(), l.Limit)
}
