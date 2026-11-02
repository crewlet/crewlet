package runner

import (
	"context"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/provenance"
)

// A RELAUNCH THAT CONTINUES ITS TURN'S SESSION, and what its run is told.
//
// Every relaunch of a turn — a reviewer's send-back, a round a person's answer,
// a loss or a replaced sign-in carries on — continues the turn's newest kept
// session where its launcher says it can ([SessionProber]), in whatever box it
// gets (internal/sandbox, restore.go). So its brief is the session's NEXT
// MESSAGE, built from what the session lacks:
//
//   - what carries it on: the review and the engine's correction with the task
//     restated and the rules its earlier rounds' calls are read by
//     ([prompts.BuildContinuedRound]), the answer to its question, or what cut
//     it off;
//   - that it may end mid-call, where its harness was cut off when it was saved;
//   - every call its run made through the seat's tools after the point it was
//     saved at, with the call's outcome;
//   - every note a person sent the turn that the session has not read — read
//     after it was saved, by the run that went on in it or by a review, which
//     is never the session's — and then the ones this brief is the first to read.
//
// AND IN TWO FORMS, because which box the launch gets is the launch's to learn:
// one for the box the session was last worked in, and one for any other, which
// adds what a fresh checkout changes ([prompts.BoxReplaced]). The fresh brief,
// for a run whose box cannot take the session up after all, rides beside them.
//
// A RELAUNCH THAT CONTINUES THE SESSION REBUILDS ITS GUARD from the skills the
// session loaded — the launch state's and every load in the turn's calls —
// because the continued model has those bodies in front of it; one that starts
// fresh keeps a fresh guard. Within a turn only: the next turn's first launch
// continues nothing yet.

// sessionBasis asks the turn's launcher which kept session this relaunch
// continues ([SessionProber]), brief being the fresh brief whose size the budget
// half of the answer weighs and carries whether the relaunch carries its round
// on. A launcher that cannot say continues none.
func (r *Runner) sessionBasis(ctx context.Context, brief string, carries bool) SessionBasis {
	prober, ok := r.cfg.Launcher.(SessionProber)
	if !ok {
		return SessionBasis{Why: "the turn's launcher continues no kept session"}
	}
	return prober.ProbeSession(ctx, (len(brief)+bytesPerToken-1)/bytesPerToken, carries)
}

// keptSession is how a relaunch continues basis, nil where basis names no kept
// session: lead — what carries the session on — then what came after the
// session was saved, then every note it has not read and the ones fresh holds,
// in the box it was last worked in, and in another with what that changes. The
// engine's own words and the seat's, every foreign word in them fenced or
// defused under the relaunching segment's token; branch and the basis's home
// are the engine's record of the session, defused too.
func (r *Runner) keptSession(basis SessionBasis, lead string, bridged []BridgedCall,
	fresh []steer.Note, branch string) *SessionContinuation {

	if !basis.Continues() {
		return nil
	}
	b := r.cfg.Turn.Context.FenceBoundary()
	parts := []string{strings.TrimSpace(lead)}
	if basis.Interrupted {
		parts = append(parts, prompts.InterruptedSession)
	}
	parts = append(parts, prompts.CalledAfter(r.calledAfter(bridged, basis.BridgeSeq, b)))
	parts = append(parts, r.unreadSince(basis.NoteCursor)...)
	parts = append(parts, renderedNotes(fresh)...)
	parts = slices.DeleteFunc(parts, func(p string) bool { return strings.TrimSpace(p) == "" })
	brief := strings.Join(parts, "\n\n")
	replaced := brief + "\n\n" + prompts.BoxReplaced(provenance.Neutralise(b, branch),
		provenance.Neutralise(b, basis.Home))
	return &SessionContinuation{Basis: basis, Brief: brief, Replaced: replaced}
}

// calledAfter is the calls of bridged the bridge admitted after seq — what the
// run working in a session did through the seat's tools after the session was
// saved — as the prior-work ledger renders them, the meta-tools it skips left
// out, and defused under b: its arguments are a model's, and a failed call's
// error is whatever the tool said. "" for none. A call with no place (zero) is
// after nothing.
func (r *Runner) calledAfter(bridged []BridgedCall, seq int, b provenance.Boundary) string {
	var after []ledger.Call
	for _, call := range inAdmissionOrder(bridged) {
		if call.Seq > seq && !slices.Contains(r.cfg.SkipNames, call.Name) {
			after = append(after, call.Call)
		}
	}
	if len(after) == 0 {
		return ""
	}
	return provenance.Neutralise(b, ledger.FormatCalls(after, ledger.FormatOptions{Skip: r.cfg.SkipNames}))
}

// unreadSince is every note the turn has read that a session saved with cursor
// as its last read has not, each rendered as the model reads it: a read the
// record placed after cursor, and every read of this segment, which the record
// has placed after any session its launch continues was saved.
func (r *Runner) unreadSince(cursor int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, read := range r.steered {
		if read.Order == 0 || read.Order > cursor {
			out = append(out, prompts.SteerMessage(read.By, read.Text))
		}
	}
	return out
}

// sendBackLead is what carries a send-back's session on: the review's notes and
// the engine's correction, with the task restated and the rules the earlier
// rounds' calls are read by ([prompts.BuildContinuedRound]).
func (r *Runner) sendBackLead(notes string) string {
	return prompts.BuildContinuedRound(prompts.ContinuedRound{Task: r.cfg.Task, Review: notes}).Text
}

// turnCalls is every call the turn's closed rounds made.
func turnCalls(history []ledger.Iteration) []ledger.Call {
	var out []ledger.Call
	for _, iteration := range history {
		out = append(out, iteration.Calls...)
	}
	return out
}
